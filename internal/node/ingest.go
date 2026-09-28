package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/imaging"
	"github.com/openbrandexchange/node-go/internal/problem"
	"github.com/openbrandexchange/node-go/internal/secure"
)

// ingestToken is an authenticated brand automation token.
type ingestToken struct {
	ID, Name, Prefix, Status, CreatedAt string
	Scopes                              []string
	BrandExternalIDs                    []string // nil = all
	ExpiresAt, LastUsed                 *string
}

func (t *ingestToken) has(scope string) bool {
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

func (a *App) loadIngestToken(ctx context.Context, where string, arg any) (*ingestToken, error) {
	t := &ingestToken{}
	var scopes string
	var brands *string
	err := a.DB.QueryRow(ctx, `SELECT id, name, prefix, status, created_at, scopes, brand_external_ids, expires_at, last_used_at FROM ingest_tokens WHERE `+where, arg).
		Scan(&t.ID, &t.Name, &t.Prefix, &t.Status, &t.CreatedAt, &scopes, &brands, &t.ExpiresAt, &t.LastUsed)
	if db.NotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Scopes = strings.Fields(scopes)
	if brands != nil {
		t.BrandExternalIDs = []string{}
		_ = json.Unmarshal([]byte(*brands), &t.BrandExternalIDs)
	}
	return t, nil
}

func (t *ingestToken) status(now time.Time) string {
	if t.Status != "active" {
		return t.Status
	}
	if t.ExpiresAt != nil && db.Parse(*t.ExpiresAt).Before(now) {
		return "expired"
	}
	return "active"
}

// ingestAPI authenticates an ingest call and checks scope.
func (a *App) ingestAPI(scope string, h func(http.ResponseWriter, *http.Request, *ingestToken)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if !a.SetupDone(ctx) {
			problem.Write(w, problem.New("setup_required", "This node has not been set up yet.").WithStatus(503))
			return
		}
		tok := bearer(r)
		if tok == "" {
			failP(w, "unauthorized", "Send an ingest token: Authorization: Bearer obxi_...")
			return
		}
		t, err := a.loadIngestToken(ctx, "token_hash = ?", secure.HashToken(tok))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if t == nil {
			failP(w, "invalid_token", "Unknown ingest token.")
			return
		}
		if t.status(a.now()) != "active" {
			failP(w, "access_revoked", "This ingest token was revoked or has expired.")
			return
		}
		if !a.limit(w, "ingest:"+t.ID, 6000, time.Minute) {
			return
		}
		if scope != "" && !t.has(scope) {
			failP(w, "insufficient_scope", "This token lacks the %s scope.", scope)
			return
		}
		if t.LastUsed == nil || a.now().Sub(db.Parse(*t.LastUsed)) > time.Minute {
			_, _ = a.DB.Exec(ctx, `UPDATE ingest_tokens SET last_used_at = ? WHERE id = ?`, a.nowStr(), t.ID)
		}
		h(w, r, t)
	}
}

func (a *App) handleIngestInfo(w http.ResponseWriter, r *http.Request, t *ingestToken) {
	ctx := r.Context()
	s := a.Settings(ctx)
	brands := []map[string]string{}
	rs, err := a.DB.Query(ctx, `SELECT external_id, name FROM brands ORDER BY external_id`)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	for rs.Next() {
		var e, n string
		if rs.Scan(&e, &n) == nil {
			brands = append(brands, map[string]string{"external_id": e, "name": n})
		}
	}
	rs.Close()
	writeJSON(w, 200, map[string]any{
		"token": map[string]any{"id": t.ID, "name": t.Name, "scopes": t.Scopes, "brand_external_ids": t.BrandExternalIDs, "expires_at": rfc3339Ptr(t.ExpiresAt)},
		"limits": map[string]any{"max_upload_bytes": MaxUploadBytes, "max_object_bytes": MaxObjectBytes, "part_bytes": PartBytes,
			"max_batch_operations": MaxBatchOperations, "max_import_bytes": MaxImportBytes},
		"renditions":             map[string]any{"server_generated": true, "required": []string{"original"}},
		"accepted_content_types": imaging.AcceptedTypes,
		"default_status":         s.Defaults.IngestStatus,
		"brands":                 brands,
		"languages":              s.ContentLanguages(),
	})
}

func rfc3339Ptr(s *string) *string { return s }

// kindFromPath maps "brands" → brand etc.
func kindFromPath(seg string) string {
	switch seg {
	case "brands":
		return KindBrand
	case "products":
		return KindProduct
	case "variants":
		return KindVariant
	case "assets":
		return KindAsset
	}
	return ""
}

// handleRecordGet serves GET /{kind}s/{external_id} (ingest and admin).
func (a *App) handleRecordGet(kind string) func(http.ResponseWriter, *http.Request, *ingestToken) {
	return func(w http.ResponseWriter, r *http.Request, t *ingestToken) {
		ext, _ := ValidExternalID(r.PathValue("external_id"))
		row, err := loadRow(r.Context(), a.DB, kind, ext)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if row == nil || (t != nil && !a.tokenSeesRow(r.Context(), t, row)) {
			failP(w, "not_found", "%s %q does not exist.", kind, ext)
			return
		}
		w.Header().Set("ETag", row.ETag)
		writeJSON(w, 200, a.record(r.Context(), a.DB, row))
	}
}

func (a *App) tokenSeesRow(ctx context.Context, t *ingestToken, r *row) bool {
	if t.BrandExternalIDs == nil {
		return true
	}
	bid := r.BrandID
	if r.Kind == KindBrand {
		bid = r.ID
	}
	var ext string
	_ = a.DB.QueryRow(ctx, `SELECT external_id FROM brands WHERE id = ?`, bid).Scan(&ext)
	for _, b := range t.BrandExternalIDs {
		if b == ext {
			return true
		}
	}
	return false
}

func (a *App) handleIngestPut(kind string) func(http.ResponseWriter, *http.Request, *ingestToken) {
	return func(w http.ResponseWriter, r *http.Request, t *ingestToken) {
		doc, p := readJSONObject(r)
		if p != nil {
			problem.Write(w, p)
			return
		}
		ext := r.PathValue("external_id")
		if kind == KindAsset {
			a.prerenderDoc(r.Context(), doc)
		}
		res, prob := a.WriteOp(r.Context(), t.BrandExternalIDs, func(wr *writer) (*IngestResult, *problem.Problem) {
			return wr.Upsert(kind, ext, doc, r.Header.Get("If-Match"))
		})
		a.writeResult(w, r, res, prob, a.Cfg.PublicURL+"/obx/v1/ingest/"+kind+"s")
	}
}

func (a *App) handleIngestPatch(kind string) func(http.ResponseWriter, *http.Request, *ingestToken) {
	return func(w http.ResponseWriter, r *http.Request, t *ingestToken) {
		doc, p := readJSONObject(r)
		if p != nil {
			problem.Write(w, p)
			return
		}
		ext := r.PathValue("external_id")
		if kind == KindAsset {
			a.prerenderDoc(r.Context(), doc)
		}
		res, prob := a.WriteOp(r.Context(), t.BrandExternalIDs, func(wr *writer) (*IngestResult, *problem.Problem) {
			return wr.Patch("patch", kind, ext, doc, r.Header.Get("If-Match"))
		})
		a.writeResult(w, r, res, prob, "")
	}
}

func (a *App) handleIngestDelete(kind string) func(http.ResponseWriter, *http.Request, *ingestToken) {
	return func(w http.ResponseWriter, r *http.Request, t *ingestToken) {
		ext := r.PathValue("external_id")
		res, prob := a.WriteOp(r.Context(), t.BrandExternalIDs, func(wr *writer) (*IngestResult, *problem.Problem) {
			return wr.Delete(kind, ext)
		})
		a.writeResult(w, r, res, prob, "")
	}
}

// assetOrderBody is the asset-order request.
type assetOrderBody struct {
	ColorCode        *string  `json:"color_code"`
	AssetExternalIDs []string `json:"asset_external_ids"`
}

func (a *App) doAssetOrder(w http.ResponseWriter, r *http.Request, allowed []string) {
	var body assetOrderBody
	raw := map[string]any{}
	if p := readJSON(r, &raw, 0); p != nil {
		problem.Write(w, p)
		return
	}
	if _, ok := raw["color_code"]; !ok {
		failP(w, "validation_failed", "color_code is required (null for product-wide images).")
		return
	}
	if err := decodeInto(raw, &body); err != nil || len(body.AssetExternalIDs) == 0 {
		failP(w, "validation_failed", "asset_external_ids must list the assets in order.")
		return
	}
	var out []IngestResult
	var prob *problem.Problem
	err := a.DB.WriteTx(r.Context(), func(tx *db.Tx) error {
		wr := a.newWriter(r.Context(), tx)
		wr.allowedBrands = allowed
		out, prob = wr.AssetOrder(r.PathValue("external_id"), body.ColorCode, body.AssetExternalIDs)
		if prob != nil {
			return errRollback
		}
		return nil
	})
	if prob != nil {
		problem.Write(w, prob)
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, out)
}

// putFile handles PUT .../assets/{external_id}/files/{rendition}.
func (a *App) putFile(w http.ResponseWriter, r *http.Request, allowed []string) {
	name := r.PathValue("rendition")
	switch name {
	case "original", "web", "square", "thumb":
	default:
		failP(w, "not_found", "Unknown rendition %q.", name)
		return
	}
	ext := r.PathValue("external_id")
	if row, _ := loadRow(r.Context(), a.DB, KindAsset, ext); row == nil {
		failP(w, "not_found", "Asset %q does not exist; create its metadata first.", ext)
		return
	}
	declared := strings.ToLower(strings.TrimSpace(r.Header.Get("OBX-Content-SHA256")))
	if r.ContentLength > MaxUploadBytes {
		failP(w, "payload_too_large", "Files above %d bytes use /uploads.", MaxUploadBytes)
		return
	}
	b, err := a.PutBlob(r.Context(), r.Body, declared, MaxUploadBytes)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if p := checkRendition(b, name); p != nil {
		problem.Write(w, p)
		return
	}
	if name == "original" {
		if err := a.ensureRenditions(r.Context(), b.SHA256); err != nil {
			a.Log.Warn("rendering failed", "sha256", b.SHA256, "error", err)
		}
	}
	res, prob := a.WriteOp(r.Context(), allowed, func(wr *writer) (*IngestResult, *problem.Problem) {
		return wr.PutFile(ext, name, b.SHA256)
	})
	a.writeResult(w, r, res, prob, "")
}

func (a *App) headBlob(w http.ResponseWriter, r *http.Request) {
	if a.HasBlob(r.Context(), r.PathValue("sha256")) {
		w.WriteHeader(200)
		return
	}
	w.WriteHeader(404)
}

func (a *App) putBlob(w http.ResponseWriter, r *http.Request) {
	sha := strings.ToLower(r.PathValue("sha256"))
	if !isSHA256(sha) {
		failP(w, "validation_failed", "The path must be a lowercase hex sha256.")
		return
	}
	if r.ContentLength > MaxUploadBytes {
		failP(w, "payload_too_large", "Files above %d bytes use /uploads.", MaxUploadBytes)
		return
	}
	b, err := a.PutBlob(r.Context(), r.Body, sha, MaxUploadBytes)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if b.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, b)
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < 64; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ---- batch -----------------------------------------------------------------------

type ingestOperation struct {
	Op         string         `json:"op"`
	Kind       string         `json:"kind"`
	ExternalID string         `json:"external_id"`
	Data       map[string]any `json:"data"`
	IfMatch    *string        `json:"if_match"`
}

// parseBatch reads a JSON array or NDJSON body.
func parseBatch(r *http.Request) ([]ingestOperation, *problem.Problem) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20+1))
	if err != nil {
		return nil, problem.New("invalid_request", "The body could not be read.")
	}
	if len(body) > 64<<20 {
		return nil, problem.New("payload_too_large", "Batch bodies are limited to 64 MiB; upload files as blobs first.")
	}
	trimmed := bytes.TrimSpace(body)
	var raw []json.RawMessage
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &raw); err != nil {
			return nil, problem.New("invalid_request", "The body is not a valid JSON array.")
		}
	} else {
		sc := bufio.NewScanner(bytes.NewReader(trimmed))
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			raw = append(raw, append(json.RawMessage(nil), line...))
		}
		if sc.Err() != nil {
			return nil, problem.New("invalid_request", "The NDJSON body could not be read.")
		}
	}
	if len(raw) > MaxBatchOperations {
		return nil, problem.New("limit_exceeded", "At most %d operations per batch.", MaxBatchOperations)
	}
	ops := make([]ingestOperation, 0, len(raw))
	for i, m := range raw {
		var op ingestOperation
		dec := json.NewDecoder(bytes.NewReader(m))
		dec.UseNumber()
		if err := dec.Decode(&op); err != nil {
			return nil, problem.New("invalid_request", "Operation %d is not valid JSON.", i)
		}
		ops = append(ops, op)
	}
	return ops, nil
}

type batchSummary struct {
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Deleted   int `json:"deleted"`
	Errors    int `json:"errors"`
}

func (s *batchSummary) add(result string) {
	switch result {
	case "created":
		s.Created++
	case "updated":
		s.Updated++
	case "unchanged":
		s.Unchanged++
	case "deleted":
		s.Deleted++
	default:
		s.Errors++
	}
}

// runBatch executes operations in order, each in its own transaction.
func (a *App) runBatch(ctx context.Context, ops []ingestOperation, allowed []string, canDelete bool, progress func(done int)) ([]IngestResult, batchSummary) {
	var sum batchSummary
	results := make([]IngestResult, 0, len(ops))
	for i, op := range ops {
		idx := i
		var res *IngestResult
		var prob *problem.Problem
		if op.Op == "delete" && !canDelete {
			res = &IngestResult{Op: op.Op, Kind: op.Kind, ExternalID: op.ExternalID}
			prob = problem.New("insufficient_scope", "Deleting needs the ingest:delete scope.")
		} else if op.Op == "" || op.Kind == "" || op.ExternalID == "" {
			res = &IngestResult{Op: op.Op, Kind: op.Kind, ExternalID: op.ExternalID}
			prob = problem.New("validation_failed", "Each operation needs op, kind and external_id.")
		} else {
			ifMatch := ""
			if op.IfMatch != nil {
				ifMatch = *op.IfMatch
			}
			if op.Kind == KindAsset && op.Data != nil {
				a.prerenderDoc(ctx, op.Data)
			}
			res, prob = a.WriteOp(ctx, allowed, func(w *writer) (*IngestResult, *problem.Problem) {
				return w.runOperation(op.Op, op.Kind, op.ExternalID, op.Data, ifMatch)
			})
		}
		if res == nil {
			res = &IngestResult{Op: op.Op, Kind: op.Kind, ExternalID: op.ExternalID}
		}
		if prob != nil {
			res.Result = "error"
			res.Error = prob
		}
		if res.Op == "" {
			res.Op = op.Op
		}
		if res.ExternalID == "" {
			res.ExternalID = op.ExternalID
		}
		res.Index = &idx
		results = append(results, *res)
		sum.add(res.Result)
		if progress != nil && (i%25 == 0 || i == len(ops)-1) {
			progress(i + 1)
		}
	}
	return results, sum
}

func (a *App) handleBatch(w http.ResponseWriter, r *http.Request, t *ingestToken) {
	a.batch(w, r, t.BrandExternalIDs, t.has("ingest:delete"), "ingest:"+t.ID, "ingest_token", t.ID)
}

func (a *App) batch(w http.ResponseWriter, r *http.Request, allowed []string, canDelete bool, scope, ownerType, ownerID string) {
	ctx := r.Context()
	key := r.Header.Get("Idempotency-Key")
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20+1))
	if err != nil {
		failP(w, "invalid_request", "The body could not be read.")
		return
	}
	reqHash := hashHex(body)
	if key != "" {
		if a.replayIdempotent(ctx, w, scope+":batch", key, reqHash) {
			return
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	ops, p := parseBatch(r)
	if p != nil {
		problem.Write(w, p)
		return
	}
	async := strings.Contains(r.Header.Get("Prefer"), "respond-async") || len(ops) > 200
	if async {
		job, err := a.createJob(ctx, jobSpec{Kind: "batch", OwnerType: ownerType, OwnerID: ownerID,
			Payload: map[string]any{"ops": ops, "allowed": allowed, "can_delete": canDelete}, Total: len(ops)})
		if err != nil {
			a.fail(w, r, err)
			return
		}
		out := a.ingestJobJSON(ctx, job)
		w.Header().Set("Location", out["results_url"].(string))
		a.respondIdempotent(ctx, w, scope+":batch", key, reqHash, http.StatusAccepted, out)
		return
	}
	results, sum := a.runBatch(ctx, ops, allowed, canDelete, nil)
	a.respondIdempotent(ctx, w, scope+":batch", key, reqHash, http.StatusOK, map[string]any{"results": results, "summary": sum})
}

// replayIdempotent answers from the idempotency store when key was used.
func (a *App) replayIdempotent(ctx context.Context, w http.ResponseWriter, scope, key, reqHash string) bool {
	var h, body string
	var status int
	err := a.DB.QueryRow(ctx, `SELECT request_hash, status, body FROM idempotency WHERE scope = ? AND key = ? AND created_at > ?`,
		scope, key, db.Fmt(a.now().Add(-24*time.Hour))).Scan(&h, &status, &body)
	if err != nil {
		return false
	}
	if h != reqHash {
		failP(w, "conflict", "This Idempotency-Key was used for a different request.")
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Idempotent-Replayed", "true")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
	return true
}

func (a *App) respondIdempotent(ctx context.Context, w http.ResponseWriter, scope, key, reqHash string, status int, v any) {
	if key != "" {
		_, _ = a.DB.Exec(ctx, `INSERT INTO idempotency (scope, key, request_hash, status, body, created_at) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (scope, key) DO UPDATE SET request_hash = excluded.request_hash, status = excluded.status, body = excluded.body, created_at = excluded.created_at`,
			scope, key, reqHash, status, string(mustJSON(v)), a.nowStr())
	}
	writeJSON(w, status, v)
}
