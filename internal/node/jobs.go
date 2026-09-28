package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/ids"
)

// jobRow is a stored background job.
type jobRow struct {
	ID, Kind, Status, OwnerType, CreatedAt  string
	OwnerID, Trigger, SourceID, Result      *string
	Payload                                 string
	Done                                    int
	Total                                   *int
	StartedAt, FinishedAt, ExpiresAt, Error *string
	Attempts                                int
}

type jobSpec struct {
	ID        string
	Kind      string
	OwnerType string
	OwnerID   string
	SourceID  string
	Trigger   string
	DedupeKey string
	Payload   any
	Total     int
}

const jobCols = `id, kind, status, owner_type, created_at, owner_id, trigger_kind, source_id, result, payload, progress_done, progress_total, started_at, finished_at, expires_at, error, attempts`

func scanJob(sc interface{ Scan(...any) error }) (*jobRow, error) {
	j := &jobRow{}
	var total sql.NullInt64
	err := sc.Scan(&j.ID, &j.Kind, &j.Status, &j.OwnerType, &j.CreatedAt, &j.OwnerID, &j.Trigger, &j.SourceID, &j.Result, &j.Payload, &j.Done, &total, &j.StartedAt, &j.FinishedAt, &j.ExpiresAt, &j.Error, &j.Attempts)
	if err != nil {
		return nil, err
	}
	if total.Valid {
		t := int(total.Int64)
		j.Total = &t
	}
	return j, nil
}

func (a *App) loadJob(ctx context.Context, id string) (*jobRow, error) {
	j, err := scanJob(a.DB.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
	if db.NotFound(err) {
		return nil, nil
	}
	return j, err
}

func (a *App) createJob(ctx context.Context, s jobSpec) (*jobRow, error) {
	id := s.ID
	if id == "" {
		id = ids.New("job")
	}
	var total any
	if s.Total > 0 {
		total = s.Total
	}
	payload := string(mustJSON(s.Payload))
	if _, err := a.DB.Exec(ctx, `INSERT INTO jobs (id, kind, status, owner_type, owner_id, trigger_kind, source_id, dedupe_key, payload, progress_total, created_at) VALUES (?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, s.Kind, s.OwnerType, nullIfEmpty(s.OwnerID), nullIfEmpty(s.Trigger), nullIfEmpty(s.SourceID), nullIfEmpty(s.DedupeKey), payload, total, a.nowStr()); err != nil {
		return nil, err
	}
	a.kick()
	return a.loadJob(ctx, id)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// jobContext is handed to job handlers.
type jobContext struct {
	a   *App
	job *jobRow
	ctx context.Context
}

func (jc *jobContext) progress(done int, total *int) {
	_, _ = jc.a.DB.Exec(jc.ctx, `UPDATE jobs SET progress_done = ?, progress_total = COALESCE(?, progress_total), lease_until = ? WHERE id = ?`,
		done, total, db.Fmt(jc.a.now().Add(10*time.Minute)), jc.job.ID)
}

// addItems stores result items (batch results, import row errors).
func (jc *jobContext) addItems(start int, items []any, isError func(any) bool) error {
	for i, it := range items {
		if _, err := jc.a.DB.Exec(jc.ctx, `INSERT INTO job_items (job_id, idx, item, is_error) VALUES (?, ?, ?, ?) ON CONFLICT (job_id, idx) DO UPDATE SET item = excluded.item, is_error = excluded.is_error`,
			jc.job.ID, start+i, string(mustJSON(it)), db.Bool(isError(it))); err != nil {
			return err
		}
	}
	return nil
}

type jobHandler func(jc *jobContext) (result any, err error)

func (a *App) jobHandlers() map[string]jobHandler {
	return map[string]jobHandler{
		"batch":    a.runBatchJob,
		"import":   a.runImportJob,
		"export":   a.runExportJob,
		"download": a.runDownloadJob,
	}
}

// workerLoop claims and runs jobs until stop.
func (a *App) workerLoop() {
	defer a.wg.Done()
	handlers := a.jobHandlers()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		for {
			j := a.claimJob(context.Background())
			if j == nil {
				break
			}
			a.runJob(handlers, j)
			select {
			case <-a.stop:
				return
			default:
			}
		}
		select {
		case <-a.stop:
			return
		case <-a.wake:
		case <-tick.C:
		}
	}
}

func (a *App) claimJob(ctx context.Context) *jobRow {
	now := a.nowStr()
	rs, err := a.DB.Query(ctx, `SELECT id FROM jobs WHERE status = 'queued' OR (status = 'running' AND lease_until < ?) ORDER BY created_at LIMIT 5`, now)
	if err != nil {
		return nil
	}
	var cand []string
	for rs.Next() {
		var id string
		if rs.Scan(&id) == nil {
			cand = append(cand, id)
		}
	}
	rs.Close()
	for _, id := range cand {
		res, err := a.DB.Exec(ctx, `UPDATE jobs SET status = 'running', lease_until = ?, attempts = attempts + 1, started_at = COALESCE(started_at, ?) WHERE id = ? AND (status = 'queued' OR (status = 'running' AND lease_until < ?))`,
			db.Fmt(a.now().Add(10*time.Minute)), now, id, now)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n == 1 {
			j, err := a.loadJob(ctx, id)
			if err == nil && j != nil {
				return j
			}
		}
	}
	return nil
}

// RunJobsNow runs queued jobs synchronously (CLI and tests).
func (a *App) RunJobsNow(ctx context.Context) {
	handlers := a.jobHandlers()
	for {
		j := a.claimJob(ctx)
		if j == nil {
			return
		}
		a.runJob(handlers, j)
	}
}

func (a *App) runJob(handlers map[string]jobHandler, j *jobRow) {
	ctx := context.Background()
	h := handlers[j.Kind]
	var result any
	var err error
	if h == nil {
		err = fmt.Errorf("unknown job kind %s", j.Kind)
	} else if j.Attempts > 3 {
		err = fmt.Errorf("job failed repeatedly (interrupted %d times)", j.Attempts-1)
	} else {
		func() {
			defer func() {
				if r := recover(); r != nil {
					a.Log.Error("job panicked", "job", j.ID, "panic", r, "stack", string(debug.Stack()))
					err = fmt.Errorf("internal error")
				}
			}()
			result, err = h(&jobContext{a: a, job: j, ctx: ctx})
		}()
	}
	status := "succeeded"
	var errText any
	if err != nil {
		status = "failed"
		errText = err.Error()
		a.Log.Warn("job failed", "job", j.ID, "kind", j.Kind, "error", err)
	}
	var res any
	if result != nil {
		res = string(mustJSON(result))
	}
	_, _ = a.DB.Exec(ctx, `UPDATE jobs SET status = ?, result = COALESCE(?, result), error = ?, finished_at = ?, lease_until = NULL WHERE id = ?`,
		status, res, errText, a.nowStr(), j.ID)
	a.afterJob(ctx, j.ID)
}

// afterJob fires notifications for finished jobs.
func (a *App) afterJob(ctx context.Context, id string) {
	j, err := a.loadJob(ctx, id)
	if err != nil || j == nil {
		return
	}
	switch j.Kind {
	case "import":
		run := a.importRunJSON(ctx, j)
		a.brandEvent(ctx, "import_run.completed", map[string]any{"import_run_id": j.ID, "status": j.Status, "counts": run["counts"]})
		if j.SourceID != nil {
			_, _ = a.DB.Exec(ctx, `UPDATE import_sources SET last_run_id = ? WHERE id = ?`, j.ID, *j.SourceID)
		}
	case "batch":
		a.brandEvent(ctx, "ingest_job.completed", map[string]any{"job_id": j.ID, "status": j.Status})
	case "export":
		if j.OwnerID != nil {
			a.clientEvent(ctx, *j.OwnerID, "export.completed", map[string]any{"export_id": j.ID, "status": j.Status})
		}
	case "download":
		a.notifyDownloadReady(ctx, j)
	}
}

func (a *App) runBatchJob(jc *jobContext) (any, error) {
	var p struct {
		Ops       []ingestOperation `json:"ops"`
		Allowed   []string          `json:"allowed"`
		CanDelete bool              `json:"can_delete"`
	}
	dec := json.NewDecoder(stringsReader(jc.job.Payload))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	total := len(p.Ops)
	results, sum := jc.a.runBatch(jc.ctx, p.Ops, p.Allowed, p.CanDelete, func(done int) { jc.progress(done, &total) })
	items := make([]any, len(results))
	for i := range results {
		items[i] = results[i]
	}
	if err := jc.addItems(0, items, func(x any) bool { return x.(IngestResult).Result == "error" }); err != nil {
		return nil, err
	}
	jc.progress(total, &total)
	return map[string]any{"summary": sum}, nil
}

// ingestJobJSON renders the IngestJob schema.
func (a *App) ingestJobJSON(ctx context.Context, j *jobRow) map[string]any {
	kind := j.Kind
	if kind != "batch" {
		kind = "import"
	}
	out := map[string]any{
		"id": j.ID, "kind": kind, "status": j.Status, "created_at": j.CreatedAt, "finished_at": j.FinishedAt,
		"progress":    map[string]any{"done": j.Done, "total": j.Total},
		"summary":     nil,
		"import_run":  nil,
		"results_url": a.Cfg.PublicURL + "/obx/v1/ingest/jobs/" + j.ID + "/results",
	}
	if j.Result != nil {
		var res struct {
			Summary *batchSummary `json:"summary"`
		}
		_ = json.Unmarshal([]byte(*j.Result), &res)
		if res.Summary != nil {
			out["summary"] = res.Summary
		}
	}
	if j.Kind == "import" {
		run := a.importRunJSON(ctx, j)
		out["import_run"] = run
		if c, ok := run["counts"].(map[string]any); ok && j.Status == "succeeded" {
			out["summary"] = map[string]any{"created": c["created"], "updated": c["updated"], "unchanged": c["unchanged"], "deleted": 0, "errors": c["errors"]}
		}
	}
	return out
}
