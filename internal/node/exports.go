package node

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/openbrandexchange/node-go/internal/blobstore"
	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/problem"
)

const (
	exportPartMax = int64(2 << 30)
	exportTTL     = 48 * time.Hour
)

type exportRequest struct {
	Filter struct {
		BrandIDs     []string `json:"brand_ids"`
		ProductIDs   []string `json:"product_ids"`
		GTINs        []string `json:"gtins"`
		UpdatedSince *string  `json:"updated_since"`
	} `json:"filter"`
	Renditions  []string `json:"renditions"`
	IncludeData *bool    `json:"include_data"`
}

type exportPart struct {
	Number int    `json:"number"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Files  int    `json:"files"`
}

type exportResult struct {
	Parts  []exportPart `json:"parts"`
	Cursor string       `json:"cursor"`
}

func (a *App) handleExportCreate(w http.ResponseWriter, r *http.Request) {
	v, ok := a.protocolAuth(w, r, "catalog:read")
	if !ok {
		return
	}
	ctx := r.Context()
	raw := map[string]any{}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxJSONBody))
	key := r.Header.Get("Idempotency-Key")
	scope := "export:" + v.ClientRowID
	if key != "" && a.replayIdempotent(ctx, w, scope, key, hashHex(body)) {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		failP(w, "invalid_request", "The body must be an ExportRequest JSON object.")
		return
	}
	if errs := schemaErrors("ExportRequest", raw); errs != nil {
		problem.Write(w, errs)
		return
	}
	var req exportRequest
	_ = decodeInto(raw, &req)
	for _, rn := range req.Renditions {
		if rn == "original" && !v.OriginalAllowed {
			failP(w, "original_not_allowed", "Original files are not available to you.")
			return
		}
		if rn != "original" && !v.Scopes["media:read"] {
			failP(w, "insufficient_scope", "Exporting files needs the media:read scope.")
			return
		}
	}
	var running string
	if a.DB.QueryRow(ctx, `SELECT id FROM jobs WHERE kind = 'export' AND owner_id = ? AND status IN ('queued', 'running')`, v.ClientRowID).Scan(&running) == nil {
		failP(w, "conflict", "Export %s is still running; wait for it to finish.", running)
		return
	}
	job, err := a.createJob(ctx, jobSpec{Kind: "export", OwnerType: "client", OwnerID: v.ClientRowID, Payload: req})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := a.exportJSON(job, v)
	w.Header().Set("Location", a.Cfg.PublicURL+"/obx/v1/exports/"+job.ID)
	a.respondIdempotent(ctx, w, scope, key, hashHex(body), http.StatusAccepted, out)
}

func schemaErrors(name string, doc any) *problem.Problem {
	if errs := lenientValidate(name, doc); errs != nil {
		return validationProblem(errs)
	}
	return nil
}

func (a *App) exportJSON(j *jobRow, v *Viewer) map[string]any {
	status := j.Status
	if j.ExpiresAt != nil && db.Parse(*j.ExpiresAt).Before(a.now()) {
		status = "expired"
	}
	out := map[string]any{"id": j.ID, "status": status, "created_at": j.CreatedAt, "finished_at": j.FinishedAt,
		"expires_at": j.ExpiresAt, "progress": map[string]any{"done": j.Done, "total": j.Total}, "manifest_url": nil, "parts": []any{}, "error": nil}
	if status == "failed" && j.Error != nil {
		out["error"] = problem.New("internal_error", "%s", *j.Error)
	}
	if status == "succeeded" && j.Result != nil {
		var res exportResult
		_ = json.Unmarshal([]byte(*j.Result), &res)
		mu, _ := a.signedURL("/obx/v1/exports/"+j.ID+"/manifest", v.tag())
		out["manifest_url"] = mu
		parts := []any{}
		for _, p := range res.Parts {
			u, _ := a.signedURL("/obx/v1/exports/"+j.ID+"/parts/"+strconv.Itoa(p.Number), v.tag())
			parts = append(parts, map[string]any{"number": p.Number, "url": u, "bytes": p.Bytes, "sha256": p.SHA256, "files": p.Files})
		}
		out["parts"] = parts
	}
	return out
}

// exportViewer authenticates export reads by bearer token or signed URL.
func (a *App) exportViewer(w http.ResponseWriter, r *http.Request, scope string) (*Viewer, *jobRow, bool) {
	var v *Viewer
	if bearer(r) == "" && r.URL.Query().Get("sig") != "" {
		tag, ok := a.verifySigned(r.URL.Path, r.URL.Query())
		if !ok {
			failP(w, "unauthorized", "This link is invalid or expired.")
			return nil, nil, false
		}
		vv, p := a.viewerFromTag(r.Context(), tag)
		if p != nil {
			problem.Write(w, p)
			return nil, nil, false
		}
		v = vv
	} else {
		vv, ok := a.protocolAuth(w, r, scope)
		if !ok {
			return nil, nil, false
		}
		v = vv
	}
	j, err := a.loadJob(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return nil, nil, false
	}
	if j == nil || j.Kind != "export" || j.OwnerID == nil || *j.OwnerID != v.ClientRowID {
		failP(w, "not_found", "Export not found.")
		return nil, nil, false
	}
	return v, j, true
}

func (a *App) handleExportGet(w http.ResponseWriter, r *http.Request) {
	v, j, ok := a.exportViewer(w, r, "catalog:read")
	if !ok {
		return
	}
	writeJSON(w, 200, a.exportJSON(j, v))
}

func (a *App) exportReady(w http.ResponseWriter, j *jobRow) bool {
	if j.ExpiresAt != nil && db.Parse(*j.ExpiresAt).Before(a.now()) {
		failP(w, "export_expired", "This export expired; start a new one.")
		return false
	}
	if j.Status != "succeeded" {
		failP(w, "not_found", "The export is not finished yet.")
		return false
	}
	return true
}

func (a *App) handleExportManifest(w http.ResponseWriter, r *http.Request) {
	_, j, ok := a.exportViewer(w, r, "catalog:read")
	if !ok || !a.exportReady(w, j) {
		return
	}
	obj, err := a.Store.Open(r.Context(), "exports/"+j.ID+"/manifest.ndjson")
	if err != nil {
		failP(w, "export_expired", "The export files were deleted.")
		return
	}
	defer obj.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "private")
	http.ServeContent(w, r, "", time.Time{}, obj)
}

func (a *App) handleExportPart(w http.ResponseWriter, r *http.Request) {
	v, j, ok := a.exportViewer(w, r, "media:read")
	if !ok || !a.exportReady(w, j) {
		return
	}
	n, err := strconv.Atoi(r.PathValue("number"))
	var res exportResult
	if j.Result != nil {
		_ = json.Unmarshal([]byte(*j.Result), &res)
	}
	if err != nil || n < 1 || n > len(res.Parts) {
		failP(w, "not_found", "Part not found.")
		return
	}
	part := res.Parts[n-1]
	obj, err := a.Store.Open(r.Context(), fmt.Sprintf("exports/%s/part-%d.zip", j.ID, n))
	if err != nil {
		failP(w, "export_expired", "The export files were deleted.")
		return
	}
	defer obj.Close()
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("OBX-Content-SHA256", part.SHA256)
	h.Set("ETag", `"`+part.SHA256+`"`)
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "private")
	h.Set("Content-Disposition", contentDisposition("attachment", fmt.Sprintf("obx-export-%s-part-%d.zip", j.ID, n)))
	http.ServeContent(w, r, "", time.Time{}, obj)
	if r.Method == http.MethodGet && !isContinuationRange(r) {
		a.logDownload(r.Context(), downloadEvent{Kind: "api_export", Viewer: v, Files: part.Files, Bytes: part.Bytes})
	}
}

// zipFile is one file to put in a zip.
type zipFile struct {
	Path   string
	SHA256 string
	Bytes  int64
	Meta   map[string]any
	// Asset and Style describe an image of a portal download, for its
	// assets.csv (import-format.md §2.3); nil for other files.
	Asset *Asset
	Style string
}

// zipBuilder writes zip parts of at most maxBytes to temp files.
type zipBuilder struct {
	a       *App
	ctx     context.Context
	max     int64
	cur     *os.File
	zw      *zip.Writer
	size    int64
	files   int
	parts   []*os.File
	counts  []int
	paths   map[string]bool
	onEntry func(part int, f zipFile)
}

func (b *zipBuilder) open() error {
	f, err := os.CreateTemp("", "obx-zip-*")
	if err != nil {
		return err
	}
	os.Remove(f.Name())
	b.cur, b.zw, b.size, b.files = f, zip.NewWriter(f), 0, 0
	b.paths = map[string]bool{}
	return nil
}

func (b *zipBuilder) closePart() error {
	if b.zw == nil {
		return nil
	}
	if err := b.zw.Close(); err != nil {
		return err
	}
	b.parts = append(b.parts, b.cur)
	b.counts = append(b.counts, b.files)
	b.zw = nil
	return nil
}

// uniquePath avoids duplicate entry names within one part.
func (b *zipBuilder) uniquePath(p string) string {
	if !b.paths[p] {
		b.paths[p] = true
		return p
	}
	base, ext := p, ""
	if i := strings.LastIndexByte(p, '.'); i > strings.LastIndexByte(p, '/') {
		base, ext = p[:i], p[i:]
	}
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s-%d%s", base, i, ext)
		if !b.paths[c] {
			b.paths[c] = true
			return c
		}
	}
}

func (b *zipBuilder) add(f zipFile) error {
	if b.zw == nil || (b.files > 0 && b.size+f.Bytes > b.max) {
		if err := b.closePart(); err != nil {
			return err
		}
		if err := b.open(); err != nil {
			return err
		}
	}
	f.Path = b.uniquePath(f.Path)
	obj, err := b.a.Store.Open(b.ctx, blobstore.BlobKey(f.SHA256))
	if err != nil {
		return fmt.Errorf("file %s missing: %w", f.SHA256, err)
	}
	defer obj.Close()
	hdr := &zip.FileHeader{Name: f.Path, Method: zip.Store, Modified: b.a.now()}
	w, err := b.zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	n, err := io.Copy(w, obj)
	if err != nil {
		return err
	}
	b.size += n + int64(len(f.Path)) + 100
	b.files++
	if b.onEntry != nil {
		b.onEntry(len(b.parts)+1, f)
	}
	return nil
}

func (b *zipBuilder) addBytes(name string, data []byte) error {
	if b.zw == nil {
		if err := b.open(); err != nil {
			return err
		}
	}
	w, err := b.zw.CreateHeader(&zip.FileHeader{Name: b.uniquePath(name), Method: zip.Deflate, Modified: b.a.now()})
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	b.size += int64(len(data))
	return err
}

func (b *zipBuilder) finish() error {
	if b.zw == nil && len(b.parts) == 0 {
		if err := b.open(); err != nil {
			return err
		}
	}
	return b.closePart()
}

func (b *zipBuilder) cleanup() {
	for _, f := range b.parts {
		f.Close()
	}
	if b.cur != nil {
		b.cur.Close()
	}
}

// exportProductIDs resolves the export filter to visible product ids.
func (a *App) exportProductIDs(ctx context.Context, v *Viewer, req *exportRequest) ([]string, error) {
	where := `WHERE visible = 1`
	var args []any
	bf, bargs := v.brandFilterSQL("brand_id")
	where += bf
	args = append(args, bargs...)
	if req.Filter.BrandIDs != nil {
		if len(req.Filter.BrandIDs) == 0 {
			return nil, nil
		}
		where += ` AND brand_id IN ` + db.In(len(req.Filter.BrandIDs))
		for _, b := range req.Filter.BrandIDs {
			args = append(args, b)
		}
	}
	if req.Filter.UpdatedSince != nil {
		t, err := time.Parse(time.RFC3339Nano, *req.Filter.UpdatedSince)
		if err == nil {
			where += ` AND touched_at >= ?`
			args = append(args, db.Fmt(t))
		}
	}
	rs, err := a.DB.Query(ctx, `SELECT id FROM products `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rs.Next() {
		var id string
		if rs.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rs.Close()
	keep := func(set map[string]bool) {
		var out []string
		for _, id := range ids {
			if set[id] {
				out = append(out, id)
			}
		}
		ids = out
	}
	if req.Filter.ProductIDs != nil {
		set := map[string]bool{}
		for _, p := range req.Filter.ProductIDs {
			set[p] = true
		}
		keep(set)
	}
	if req.Filter.GTINs != nil {
		set := map[string]bool{}
		for _, g := range req.Filter.GTINs {
			if n, ok := normalizeGTIN(g); ok {
				var pid string
				if a.DB.QueryRow(ctx, `SELECT product_id FROM variants WHERE gtin = ? AND visible = 1`, n).Scan(&pid) == nil {
					set[pid] = true
				}
			}
		}
		keep(set)
	}
	return ids, nil
}

func (a *App) runExportJob(jc *jobContext) (any, error) {
	var req exportRequest
	if err := json.Unmarshal([]byte(jc.job.Payload), &req); err != nil {
		return nil, err
	}
	ctx := jc.ctx
	c, err := a.loadClient(ctx, a.DB, "id = ?", deref(jc.job.OwnerID))
	if err != nil {
		return nil, err
	}
	v, prob := a.viewerForClient(ctx, c, c0Scopes(c))
	if prob != nil {
		return nil, errors.New(problemDetail(prob))
	}
	hw, err := a.maxSeq(ctx)
	if err != nil {
		return nil, err
	}
	ids, err := a.exportProductIDs(ctx, v, &req)
	if err != nil {
		return nil, err
	}
	includeData := req.IncludeData == nil || *req.IncludeData
	total := len(ids)
	jc.progress(0, &total)
	var manifest bytes.Buffer
	enc := json.NewEncoder(&manifest)
	enc.SetEscapeHTML(false)
	var lines []any
	var fileLines []any
	counts := map[string]int{"products": 0, "variants": 0, "assets": 0, "files": 0}
	b := &zipBuilder{a: a, ctx: ctx, max: exportPartMax}
	defer b.cleanup()
	b.onEntry = func(part int, f zipFile) {
		fileLines = append(fileLines, map[string]any{"type": "file", "asset_id": f.Meta["asset_id"], "rendition": f.Meta["rendition"],
			"part": part, "path": f.Path, "bytes": f.Bytes, "sha256": f.SHA256})
		counts["files"]++
	}
	brands := map[string]bool{}
	for i := 0; i < len(ids); i += 50 {
		chunk := ids[i:min(i+50, len(ids))]
		products, err := a.productDetails(ctx, v, chunk)
		if err != nil {
			return nil, err
		}
		for _, p := range products {
			if !brands[p.BrandID] {
				brands[p.BrandID] = true
				if br, _ := loadRowByID(ctx, a.DB, KindBrand, p.BrandID); br != nil && includeData {
					lines = append(lines, map[string]any{"type": "brand", "data": renderBrand(br.Rep, br.UpdatedAt)})
				}
			}
			vs, as := *p.Variants, *p.Assets
			counts["products"]++
			counts["variants"] += len(vs)
			counts["assets"] += len(as)
			if includeData {
				pp := *p
				pp.Variants, pp.Assets = nil, nil
				lines = append(lines, map[string]any{"type": "product", "data": pp})
				for _, x := range vs {
					lines = append(lines, map[string]any{"type": "variant", "data": x})
				}
				for _, x := range as {
					lines = append(lines, map[string]any{"type": "asset", "data": x})
				}
			}
			for _, x := range as {
				for _, rn := range req.Renditions {
					rend := x.Renditions[rn]
					if rend == nil {
						continue
					}
					name := x.Filename
					if rn != "original" {
						name = renditionFilename(x.Filename, rn)
					}
					path := safeZipSegment(p.StyleNumber) + "/" + rn + "/" + sanitizeFilename(name)
					if err := b.add(zipFile{Path: path, SHA256: rend.SHA256, Bytes: rend.Bytes, Meta: map[string]any{"asset_id": x.ID, "rendition": rn}}); err != nil {
						return nil, err
					}
				}
			}
		}
		done := min(i+50, len(ids))
		jc.progress(done, &total)
	}
	if err := b.finish(); err != nil {
		return nil, err
	}
	cursor := a.encodeCursor("changes", feedCursor{Phase: "l", Seq: hw})
	_ = enc.Encode(map[string]any{"type": "header", "export_id": jc.job.ID, "created_at": jc.job.CreatedAt, "obx_version": OBXVersion,
		"changes_cursor": cursor, "counts": counts})
	for _, l := range lines {
		_ = enc.Encode(l)
	}
	for _, l := range fileLines {
		_ = enc.Encode(l)
	}
	if err := a.Store.Put(ctx, "exports/"+jc.job.ID+"/manifest.ndjson", bytes.NewReader(manifest.Bytes()), int64(manifest.Len()), "application/x-ndjson"); err != nil {
		return nil, err
	}
	res := exportResult{Cursor: cursor}
	for i, f := range b.parts {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return nil, err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		if err := a.Store.Put(ctx, fmt.Sprintf("exports/%s/part-%d.zip", jc.job.ID, i+1), f, n, "application/zip"); err != nil {
			return nil, err
		}
		res.Parts = append(res.Parts, exportPart{Number: i + 1, Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil)), Files: b.counts[i]})
	}
	_, _ = a.DB.Exec(ctx, `UPDATE jobs SET expires_at = ? WHERE id = ?`, db.Fmt(a.now().Add(exportTTL)), jc.job.ID)
	return res, nil
}

func c0Scopes(c *apiClientRow) []string {
	if c == nil {
		return nil
	}
	return c.Scopes
}

// safeZipSegment makes a style number usable as a zip folder name.
func safeZipSegment(s string) string {
	s = strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(s)
	s = sanitizeFilename(s)
	return s
}
