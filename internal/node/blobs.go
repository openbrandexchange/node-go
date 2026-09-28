package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/openbrandexchange/node-go/internal/blobstore"
	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/imaging"
	"github.com/openbrandexchange/node-go/internal/problem"
)

// Limits advertised in IngestInfo (docs/ingest.md §9).
const (
	MaxUploadBytes     int64 = 100 << 20
	MaxObjectBytes     int64 = 2 << 30
	PartBytes          int64 = 8 << 20
	MaxBatchOperations       = 1000
	MaxImportBytes     int64 = 2 << 30
)

// Blob is the Blob schema.
type Blob struct {
	SHA256      string `json:"sha256"`
	Bytes       int64  `json:"bytes"`
	ContentType string `json:"content_type"`
	Width       *int   `json:"width"`
	Height      *int   `json:"height"`
	Created     bool   `json:"created"`
}

// spool copies r (at most limit bytes) to a temp file, hashing it.
func spool(r io.Reader, limit int64) (*os.File, string, int64, error) {
	f, err := os.CreateTemp("", "obx-upload-*")
	if err != nil {
		return nil, "", 0, err
	}
	os.Remove(f.Name()) // unlinked; deleted on close
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, limit+1))
	if err != nil {
		f.Close()
		return nil, "", 0, err
	}
	if n > limit {
		f.Close()
		return nil, "", n, problem.New("payload_too_large", "The file is larger than %d bytes.", limit)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, "", 0, err
	}
	return f, hex.EncodeToString(h.Sum(nil)), n, nil
}

// PutBlob stores an uploaded image. declaredSHA (may be "") is verified.
// Only accepted image types (by magic bytes) are stored.
func (a *App) PutBlob(ctx context.Context, r io.Reader, declaredSHA string, limit int64) (*Blob, error) {
	f, sha, n, err := spool(r, limit)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if declaredSHA != "" && declaredSHA != sha {
		return nil, problem.New("checksum_mismatch", "The uploaded bytes have sha256 %s, not %s.", sha, declaredSHA)
	}
	if n == 0 {
		return nil, problem.New("validation_failed", "The file is empty.")
	}
	return a.storeSpooled(ctx, f, sha, n)
}

func (a *App) storeSpooled(ctx context.Context, f *os.File, sha string, n int64) (*Blob, error) {
	if b, err := a.getBlob(ctx, sha); err == nil && b != nil {
		ok, _ := a.Store.Exists(ctx, blobstore.BlobKey(sha))
		if ok {
			b.Created = false
			return b, nil
		}
	}
	head := make([]byte, 512)
	k, _ := io.ReadFull(f, head)
	ct := imaging.Sniff(head[:k])
	if ct == "" {
		return nil, problem.New("unsupported_media_type", "Only JPEG, PNG, WebP and TIFF images are accepted.")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	b := &Blob{SHA256: sha, Bytes: n, ContentType: ct, Created: true}
	if w, h, _, err := imaging.Dimensions(f); err == nil {
		b.Width, b.Height = &w, &h
	} else if ct != "image/tiff" {
		return nil, problem.New("validation_failed", "The image cannot be read: %v", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if err := a.Store.Put(ctx, blobstore.BlobKey(sha), f, n, ct); err != nil {
		return nil, err
	}
	_, err := a.DB.Exec(ctx, `INSERT INTO blobs (sha256, bytes, content_type, width, height, created_at) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (sha256) DO NOTHING`,
		sha, n, ct, b.Width, b.Height, a.nowStr())
	if err != nil {
		return nil, err
	}
	return b, nil
}

// PutBlobBytes stores in-memory bytes (renditions, imports).
func (a *App) PutBlobBytes(ctx context.Context, data []byte) (*Blob, error) {
	s := sha256.Sum256(data)
	sha := hex.EncodeToString(s[:])
	if b, err := a.getBlob(ctx, sha); err == nil && b != nil {
		if ok, _ := a.Store.Exists(ctx, blobstore.BlobKey(sha)); ok {
			return b, nil
		}
	}
	ct := imaging.Sniff(data)
	if ct == "" {
		return nil, problem.New("unsupported_media_type", "Only JPEG, PNG, WebP and TIFF images are accepted.")
	}
	b := &Blob{SHA256: sha, Bytes: int64(len(data)), ContentType: ct, Created: true}
	if w, h, _, err := imaging.Dimensions(bytes.NewReader(data)); err == nil {
		b.Width, b.Height = &w, &h
	} else if ct != "image/tiff" {
		return nil, problem.New("validation_failed", "The image cannot be read: %v", err)
	}
	if err := a.Store.Put(ctx, blobstore.BlobKey(sha), bytes.NewReader(data), int64(len(data)), ct); err != nil {
		return nil, err
	}
	_, err := a.DB.Exec(ctx, `INSERT INTO blobs (sha256, bytes, content_type, width, height, created_at) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (sha256) DO NOTHING`,
		sha, b.Bytes, ct, b.Width, b.Height, a.nowStr())
	return b, err
}

func (a *App) getBlob(ctx context.Context, sha string) (*Blob, error) {
	b := &Blob{}
	var w, h *int
	err := a.DB.QueryRow(ctx, `SELECT sha256, bytes, content_type, width, height FROM blobs WHERE sha256 = ?`, sha).Scan(&b.SHA256, &b.Bytes, &b.ContentType, &w, &h)
	if db.NotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.Width, b.Height = w, h
	return b, nil
}

// HasBlob reports whether the node has the bytes.
func (a *App) HasBlob(ctx context.Context, sha string) bool {
	b, err := a.getBlob(ctx, sha)
	return err == nil && b != nil
}

// checkRendition enforces the rendition rules on a client-generated file.
func checkRendition(b *Blob, name string) *problem.Problem {
	if name == "original" {
		return nil
	}
	if b.ContentType != "image/jpeg" {
		return problem.New("validation_failed", "Rendition %s must be a JPEG.", name)
	}
	if b.Width == nil || b.Height == nil {
		return nil
	}
	w, h := *b.Width, *b.Height
	switch name {
	case "web":
		if max(w, h) > imaging.WebMax {
			return problem.New("validation_failed", "The web rendition may be at most %d px on its longest side.", imaging.WebMax)
		}
	case "thumb":
		if max(w, h) > imaging.ThumbMax {
			return problem.New("validation_failed", "The thumb rendition may be at most %d px on its longest side.", imaging.ThumbMax)
		}
	case "square":
		if w != h {
			return problem.New("validation_failed", "The square rendition must be square.")
		}
	}
	return nil
}

// ---- server-side renditions ---------------------------------------------------

var renderMu sync.Mutex
var rendering = map[string]bool{}

func (a *App) queueRender(sha string) {
	select {
	case a.renderQ <- sha:
	default: // the periodic sweep picks it up
	}
}

// renderLoop renders queued originals until stop.
func (a *App) renderLoop() {
	defer a.wg.Done()
	for {
		select {
		case <-a.stop:
			return
		case sha := <-a.renderQ:
			if err := a.RenderOriginal(context.Background(), sha); err != nil {
				a.Log.Warn("rendering failed", "sha256", sha, "error", err)
			}
		}
	}
}

// RenderOriginal makes web/square/thumb for an original (idempotent) and
// updates the assets waiting for them.
func (a *App) RenderOriginal(ctx context.Context, sha string) error {
	renderMu.Lock()
	if rendering[sha] {
		renderMu.Unlock()
		return nil
	}
	rendering[sha] = true
	renderMu.Unlock()
	defer func() {
		renderMu.Lock()
		delete(rendering, sha)
		renderMu.Unlock()
	}()
	if err := a.ensureRenditions(ctx, sha); err != nil {
		return err
	}
	return a.DB.WriteTx(ctx, func(tx *db.Tx) error {
		w := a.newWriter(ctx, tx)
		rows, err := loadRows(ctx, tx, KindAsset, `WHERE original_sha = ? AND (pending = 1 OR live_files IS NULL) ORDER BY product_id, CASE WHEN color_code IS NULL THEN 0 ELSE 1 END, color_code, position, external_id`, sha)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := w.recompute(r); err != nil {
				return err
			}
		}
		return nil
	})
}

// ensureRenditions renders web/square/thumb for an original when they do
// not exist yet. Writes call it before attaching an original so the asset is
// complete (and its change entry appears) in operation order.
func (a *App) ensureRenditions(ctx context.Context, sha string) error {
	if !isSHA256(sha) || !a.HasBlob(ctx, sha) {
		return nil
	}
	var n int
	_ = a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM renditions WHERE original_sha = ?`, sha).Scan(&n)
	if n >= len(DerivedRenditions) {
		return nil
	}
	var prev string
	if a.DB.QueryRow(ctx, `SELECT error FROM render_errors WHERE original_sha = ?`, sha).Scan(&prev) == nil {
		return nil // failed before; do not retry forever
	}
	if err := a.renderFiles(ctx, sha); err != nil {
		_, _ = a.DB.Exec(ctx, `INSERT INTO render_errors (original_sha, error, at) VALUES (?, ?, ?) ON CONFLICT (original_sha) DO UPDATE SET error = excluded.error, at = excluded.at`,
			sha, err.Error(), a.nowStr())
		return err
	}
	return nil
}

// prerenderDoc renders the original referenced by an asset document.
func (a *App) prerenderDoc(ctx context.Context, doc map[string]any) {
	files, _ := doc["files"].(map[string]any)
	orig, _ := files["original"].(map[string]any)
	if sha, _ := orig["sha256"].(string); sha != "" {
		if err := a.ensureRenditions(ctx, sha); err != nil {
			a.Log.Warn("rendering failed", "sha256", sha, "error", err)
		}
	}
}

func (a *App) renderFiles(ctx context.Context, sha string) error {
	obj, err := a.Store.Open(ctx, blobstore.BlobKey(sha))
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			return fmt.Errorf("original %s is missing from storage", sha)
		}
		return err
	}
	data, err := io.ReadAll(obj)
	obj.Close()
	if err != nil {
		return err
	}
	outs, err := imaging.Render(data)
	if err != nil {
		return err
	}
	for _, r := range outs {
		b, err := a.PutBlobBytes(ctx, r.Data)
		if err != nil {
			return err
		}
		if _, err := a.DB.Exec(ctx, `INSERT INTO renditions (original_sha, name, sha256, width, height, bytes) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (original_sha, name) DO NOTHING`,
			sha, r.Name, b.SHA256, r.Width, r.Height, b.Bytes); err != nil {
			return err
		}
	}
	return nil
}

// renderSweep queues originals of pending assets (after restarts).
func (a *App) renderSweep(ctx context.Context) {
	rs, err := a.DB.Query(ctx, `SELECT DISTINCT original_sha FROM assets WHERE pending = 1 AND original_sha IS NOT NULL`)
	if err != nil {
		return
	}
	var shas []string
	for rs.Next() {
		var s string
		if rs.Scan(&s) == nil {
			shas = append(shas, s)
		}
	}
	rs.Close()
	for _, s := range shas {
		a.queueRender(s)
	}
}
