package node

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/openbrandexchange/node-go/internal/ids"
	"github.com/openbrandexchange/node-go/internal/importfmt"
	"github.com/openbrandexchange/node-go/internal/problem"
)

// importRequest is a parsed import upload.
type importRequest struct {
	jobID    string
	files    []importFile
	manifest []manifestEntry
	options  importOptions
}

// parseImportUpload streams a multipart import upload into the blob store.
func (a *App) parseImportUpload(r *http.Request) (*importRequest, *problem.Problem) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "multipart/form-data" {
		return nil, problem.New("unsupported_media_type", "Send multipart/form-data with package, csv, assets_csv, images, images_manifest or options.")
	}
	if r.ContentLength > MaxImportBytes {
		return nil, problem.New("payload_too_large", "Imports are limited to %d bytes.", MaxImportBytes)
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, problem.New("invalid_request", "The multipart body cannot be read.")
	}
	req := &importRequest{jobID: ids.New("job")}
	var used int64
	ctx := r.Context()
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			a.cleanupStaged(req)
			return nil, problem.New("invalid_request", "The multipart body cannot be read.")
		}
		field := part.FormName()
		switch field {
		case "options", "images_manifest":
			b, err := io.ReadAll(io.LimitReader(part, 64<<20))
			if err != nil {
				a.cleanupStaged(req)
				return nil, problem.New("invalid_request", "%s cannot be read.", field)
			}
			dec := json.NewDecoder(bytes.NewReader(b))
			var derr error
			if field == "options" {
				derr = dec.Decode(&req.options)
			} else {
				derr = dec.Decode(&req.manifest)
			}
			if derr != nil {
				a.cleanupStaged(req)
				return nil, problem.New("invalid_request", "%s must be JSON.", field)
			}
		case "package", "csv", "assets_csv", "images", "images[]":
			if field == "images[]" {
				field = "images"
			}
			name := part.FileName()
			isTable := field == "csv" || field == "assets_csv"
			if isTable && importfmt.IsOtherSpreadsheetName(name) {
				// Only .xlsx workbooks are read (import-format.md §5.1).
				a.cleanupStaged(req)
				return nil, problem.New("unsupported_media_type", "%s is not a CSV file or an Excel workbook (.xlsx). Save it as CSV or as \"Excel Workbook (.xlsx)\".", sanitizeFilename(name))
			}
			if name == "" {
				name = map[string]string{"package": "package.zip", "csv": "products.csv", "assets_csv": "assets.csv", "images": "images.zip"}[field]
				// A part without a file name is a workbook by its content type (§5.1).
				if pct, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type")); isTable && pct == importfmt.WorkbookContentType {
					name = strings.TrimSuffix(name, ".csv") + ".xlsx"
				}
			}
			f, err := a.stageFile(ctx, req.jobID, field, name, part, MaxImportBytes-used)
			if err != nil {
				a.cleanupStaged(req)
				var p *problem.Problem
				if errorsAs(err, &p) {
					return nil, p
				}
				return nil, problem.As(err)
			}
			used += f.Bytes
			req.files = append(req.files, *f)
		default:
			_, _ = io.Copy(io.Discard, io.LimitReader(part, 1<<20))
		}
	}
	if len(req.files) == 0 && len(req.manifest) == 0 {
		return nil, problem.New("invalid_request", "Nothing to import: send package, csv, images or images_manifest.")
	}
	switch req.options.Mode {
	case "", "upsert", "mirror":
	default:
		a.cleanupStaged(req)
		return nil, problem.New("validation_failed", "options.mode must be upsert or mirror.")
	}
	return req, nil
}

func (a *App) cleanupStaged(req *importRequest) {
	for _, f := range req.files {
		_ = a.Store.Delete(context.Background(), f.Key)
	}
}

// enqueueImport creates the import job.
func (a *App) enqueueImport(ctx context.Context, req *importRequest, trigger, ownerType, ownerID, sourceID string, allowed []string, canDelete bool) (*jobRow, error) {
	return a.createJob(ctx, jobSpec{ID: req.jobID, Kind: "import", OwnerType: ownerType, OwnerID: ownerID, SourceID: sourceID, Trigger: trigger,
		Payload: importPayload{Files: req.files, Manifest: req.manifest, Options: req.options, Allowed: allowed, CanDelete: canDelete}})
}

func (a *App) handleIngestImport(w http.ResponseWriter, r *http.Request, t *ingestToken) {
	ctx := r.Context()
	key := r.Header.Get("Idempotency-Key")
	if key != "" {
		var body string
		if a.DB.QueryRow(ctx, `SELECT body FROM idempotency WHERE scope = ? AND key = ?`, "ingest:"+t.ID+":import", key).Scan(&body) == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(body))
			return
		}
	}
	req, p := a.parseImportUpload(r)
	if p != nil {
		problem.Write(w, p)
		return
	}
	if req.options.Mode == "mirror" && !t.has("ingest:delete") {
		a.cleanupStaged(req)
		failP(w, "insufficient_scope", "A mirror import unpublishes products and needs the ingest:delete scope.")
		return
	}
	job, err := a.enqueueImport(ctx, req, "api", "ingest_token", t.ID, "", t.BrandExternalIDs, t.has("ingest:delete"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := a.ingestJobJSON(ctx, job)
	w.Header().Set("Location", a.Cfg.PublicURL+"/obx/v1/ingest/jobs/"+job.ID)
	a.respondIdempotent(ctx, w, "ingest:"+t.ID+":import", key, "", http.StatusAccepted, out)
}

func (a *App) handleIngestJobs(w http.ResponseWriter, r *http.Request, t *ingestToken) {
	ctx := r.Context()
	limit, p := limitParam(r, 25, 100)
	if p != nil {
		problem.Write(w, p)
		return
	}
	where := `WHERE kind IN ('batch', 'import') AND owner_type = 'ingest_token'`
	var args []any
	if s := r.URL.Query().Get("status"); s != "" {
		where += ` AND status = ?`
		args = append(args, s)
	}
	if c := r.URL.Query().Get("cursor"); c != "" {
		var cur struct {
			C string `json:"c"`
			I string `json:"i"`
		}
		if !a.decodeCursor("jobs", c, &cur) {
			failP(w, "cursor_invalid", "The cursor is not valid.")
			return
		}
		where += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, cur.C, cur.C, cur.I)
	}
	rs, err := a.DB.Query(ctx, `SELECT `+jobCols+` FROM jobs `+where+` ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var jobs []*jobRow
	for rs.Next() {
		j, err := scanJob(rs)
		if err == nil {
			jobs = append(jobs, j)
		}
	}
	rs.Close()
	var next *string
	if len(jobs) > limit {
		jobs = jobs[:limit]
		c := a.encodeCursor("jobs", map[string]string{"c": jobs[limit-1].CreatedAt, "i": jobs[limit-1].ID})
		next = &c
	}
	items := []any{}
	for _, j := range jobs {
		items = append(items, a.ingestJobJSON(ctx, j))
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}

func (a *App) handleIngestJob(w http.ResponseWriter, r *http.Request, t *ingestToken) {
	j, err := a.loadJob(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if j == nil || (j.Kind != "batch" && j.Kind != "import") || j.OwnerType == "retailer" || j.OwnerType == "client" {
		failP(w, "not_found", "Job not found.")
		return
	}
	writeJSON(w, 200, a.ingestJobJSON(r.Context(), j))
}

func (a *App) handleIngestJobResults(w http.ResponseWriter, r *http.Request, t *ingestToken) {
	j, err := a.loadJob(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if j == nil || (j.Kind != "batch" && j.Kind != "import") || j.OwnerType == "retailer" || j.OwnerType == "client" {
		failP(w, "not_found", "Job not found.")
		return
	}
	a.jobItems(w, r, j.ID, 100, 1000)
}

// jobItems pages through job_items.
func (a *App) jobItems(w http.ResponseWriter, r *http.Request, jobID string, def, maxv int) {
	limit, p := limitParam(r, def, maxv)
	if p != nil {
		problem.Write(w, p)
		return
	}
	after := -1
	if c := r.URL.Query().Get("cursor"); c != "" {
		var cur struct {
			I int `json:"i"`
		}
		if !a.decodeCursor("items/"+jobID, c, &cur) {
			failP(w, "cursor_invalid", "The cursor is not valid.")
			return
		}
		after = cur.I
	}
	where := `WHERE job_id = ? AND idx > ?`
	if v := strings.ToLower(r.URL.Query().Get("errors_only")); v == "true" || v == "1" {
		where += ` AND is_error = 1`
	}
	rs, err := a.DB.Query(r.Context(), `SELECT idx, item FROM job_items `+where+` ORDER BY idx LIMIT ?`, jobID, after, limit+1)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items := []json.RawMessage{}
	var idxs []int
	for rs.Next() {
		var i int
		var s string
		if rs.Scan(&i, &s) == nil {
			items = append(items, json.RawMessage(s))
			idxs = append(idxs, i)
		}
	}
	rs.Close()
	var next *string
	if len(items) > limit {
		items = items[:limit]
		c := a.encodeCursor("items/"+jobID, map[string]int{"i": idxs[limit-1]})
		next = &c
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}
