package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/ids"
	"github.com/openbrandexchange/node-go/internal/problem"
)

type uploadRow struct {
	ID, SHA256, ContentType, Status, ExpiresAt, Owner string
	Bytes, PartBytes                                  int64
	PartsTotal                                        int
	Received                                          []int
}

func (a *App) loadUpload(ctx context.Context, id, owner string) (*uploadRow, error) {
	u := &uploadRow{}
	var received string
	err := a.DB.QueryRow(ctx, `SELECT id, sha256, content_type, status, expires_at, owner, bytes, part_bytes, parts_total, received FROM uploads WHERE id = ?`, id).
		Scan(&u.ID, &u.SHA256, &u.ContentType, &u.Status, &u.ExpiresAt, &u.Owner, &u.Bytes, &u.PartBytes, &u.PartsTotal, &received)
	if db.NotFound(err) || (err == nil && u.Owner != owner) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(received), &u.Received)
	if u.Status == "open" && db.Parse(u.ExpiresAt).Before(a.now()) {
		u.Status = "expired"
	}
	return u, nil
}

func (u *uploadRow) json() map[string]any {
	rec := u.Received
	if rec == nil {
		rec = []int{}
	}
	return map[string]any{"id": u.ID, "sha256": u.SHA256, "bytes": u.Bytes, "content_type": u.ContentType, "part_bytes": u.PartBytes,
		"parts_total": u.PartsTotal, "received_parts": rec, "status": u.Status, "expires_at": u.ExpiresAt}
}

func partKey(id string, n int) string { return "uploads/" + id + "/" + strconv.Itoa(n) }

func (a *App) handleUploadCreate(w http.ResponseWriter, r *http.Request, t *ingestToken) {
	var body struct {
		SHA256      string `json:"sha256"`
		Bytes       int64  `json:"bytes"`
		ContentType string `json:"content_type"`
	}
	if p := readJSON(r, &body, 0); p != nil {
		problem.Write(w, p)
		return
	}
	if !isSHA256(body.SHA256) || body.Bytes < 1 {
		failP(w, "validation_failed", "sha256 (lowercase hex) and bytes are required.")
		return
	}
	switch body.ContentType {
	case "image/jpeg", "image/png", "image/webp", "image/tiff":
	default:
		failP(w, "unsupported_media_type", "Only JPEG, PNG, WebP and TIFF images are accepted.")
		return
	}
	if body.Bytes > MaxObjectBytes {
		failP(w, "payload_too_large", "Files are limited to %d bytes.", MaxObjectBytes)
		return
	}
	ctx := r.Context()
	if b, _ := a.getBlob(ctx, body.SHA256); b != nil {
		b.Created = false
		writeJSON(w, 200, b)
		return
	}
	parts := int((body.Bytes + PartBytes - 1) / PartBytes)
	u := &uploadRow{ID: ids.New("upl"), SHA256: body.SHA256, ContentType: body.ContentType, Status: "open",
		ExpiresAt: db.Fmt(a.now().Add(24 * time.Hour)), Owner: "ingest:" + t.ID, Bytes: body.Bytes, PartBytes: PartBytes, PartsTotal: parts}
	if _, err := a.DB.Exec(ctx, `INSERT INTO uploads (id, sha256, bytes, content_type, part_bytes, parts_total, received, status, expires_at, owner) VALUES (?, ?, ?, ?, ?, ?, '[]', 'open', ?, ?)`,
		u.ID, u.SHA256, u.Bytes, u.ContentType, u.PartBytes, u.PartsTotal, u.ExpiresAt, u.Owner); err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 201, u.json())
}

func (a *App) handleUploadGet(w http.ResponseWriter, r *http.Request, t *ingestToken) {
	u, err := a.loadUpload(r.Context(), r.PathValue("id"), "ingest:"+t.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if u == nil {
		failP(w, "not_found", "Upload not found.")
		return
	}
	writeJSON(w, 200, u.json())
}

func (a *App) handleUploadDelete(w http.ResponseWriter, r *http.Request, t *ingestToken) {
	ctx := r.Context()
	u, err := a.loadUpload(ctx, r.PathValue("id"), "ingest:"+t.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if u == nil {
		failP(w, "not_found", "Upload not found.")
		return
	}
	_, _ = a.DB.Exec(ctx, `UPDATE uploads SET status = 'aborted' WHERE id = ?`, u.ID)
	_ = a.Store.DeletePrefix(ctx, "uploads/"+u.ID+"/")
	w.WriteHeader(204)
}

func (a *App) handleUploadPart(w http.ResponseWriter, r *http.Request, t *ingestToken) {
	ctx := r.Context()
	u, err := a.loadUpload(ctx, r.PathValue("id"), "ingest:"+t.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if u == nil || u.Status != "open" {
		failP(w, "not_found", "Upload not found or no longer open.")
		return
	}
	n, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || n < 1 || n > u.PartsTotal {
		failP(w, "validation_failed", "Part number must be 1..%d.", u.PartsTotal)
		return
	}
	want := u.PartBytes
	if n == u.PartsTotal {
		want = u.Bytes - int64(u.PartsTotal-1)*u.PartBytes
	}
	f, _, size, err := spool(r.Body, want)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer f.Close()
	if size != want {
		failP(w, "validation_failed", "Part %d must be exactly %d bytes (got %d).", n, want, size)
		return
	}
	if err := a.Store.Put(ctx, partKey(u.ID, n), f, size, "application/octet-stream"); err != nil {
		a.fail(w, r, err)
		return
	}
	err = a.DB.WriteTx(ctx, func(tx *db.Tx) error {
		var received string
		if err := tx.QueryRow(ctx, `SELECT received FROM uploads WHERE id = ?`, u.ID).Scan(&received); err != nil {
			return err
		}
		var list []int
		_ = json.Unmarshal([]byte(received), &list)
		seen := false
		for _, x := range list {
			if x == n {
				seen = true
			}
		}
		if !seen {
			list = append(list, n)
		}
		sort.Ints(list)
		u.Received = list
		_, err := tx.Exec(ctx, `UPDATE uploads SET received = ? WHERE id = ?`, string(mustJSON(list)), u.ID)
		return err
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, u.json())
}

func (a *App) handleUploadComplete(w http.ResponseWriter, r *http.Request, t *ingestToken) {
	ctx := r.Context()
	u, err := a.loadUpload(ctx, r.PathValue("id"), "ingest:"+t.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if u == nil || u.Status != "open" {
		failP(w, "not_found", "Upload not found or no longer open.")
		return
	}
	if len(u.Received) != u.PartsTotal {
		have := map[int]bool{}
		for _, x := range u.Received {
			have[x] = true
		}
		var missing []string
		for i := 1; i <= u.PartsTotal; i++ {
			if !have[i] {
				missing = append(missing, strconv.Itoa(i))
			}
		}
		failP(w, "upload_incomplete", "Parts missing: %v.", missing)
		return
	}
	f, err := os.CreateTemp("", "obx-assemble-*")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	var total int64
	for i := 1; i <= u.PartsTotal; i++ {
		obj, err := a.Store.Open(ctx, partKey(u.ID, i))
		if err != nil {
			failP(w, "upload_incomplete", "Part %d is missing; upload it again.", i)
			return
		}
		n, err := io.Copy(io.MultiWriter(f, h), obj)
		obj.Close()
		if err != nil {
			a.fail(w, r, err)
			return
		}
		total += n
	}
	sha := hex.EncodeToString(h.Sum(nil))
	if sha != u.SHA256 || total != u.Bytes {
		_, _ = a.DB.Exec(ctx, `UPDATE uploads SET status = 'aborted' WHERE id = ?`, u.ID)
		_ = a.Store.DeletePrefix(ctx, "uploads/"+u.ID+"/")
		failP(w, "checksum_mismatch", "The assembled file has sha256 %s, not %s; the upload was aborted.", sha, u.SHA256)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		a.fail(w, r, err)
		return
	}
	b, err := a.storeSpooled(ctx, f, sha, total)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	_, _ = a.DB.Exec(ctx, `UPDATE uploads SET status = 'completed' WHERE id = ?`, u.ID)
	_ = a.Store.DeletePrefix(ctx, "uploads/"+u.ID+"/")
	writeJSON(w, 200, b)
}
