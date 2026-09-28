package node

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/ids"
	"github.com/openbrandexchange/node-go/internal/problem"
)

// sourceRow is a scheduled pull import.
type sourceRow struct {
	ID, Name, Kind, Mode, CreatedAt, UpdatedAt string
	Enabled, AllowMass                         bool
	Schedule                                   map[string]any
	Config                                     map[string]any
	CredsEnc, NextRun, LastRunID, Fingerprint  *string
}

type sourceCreds struct {
	Password        *string           `json:"password,omitempty"`
	PrivateKey      *string           `json:"private_key,omitempty"`
	AccessKeyID     *string           `json:"access_key_id,omitempty"`
	SecretAccessKey *string           `json:"secret_access_key,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
}

func (a *App) loadSource(ctx context.Context, id string) (*sourceRow, error) {
	s := &sourceRow{}
	var sched, cfg string
	var enabled, mass int
	err := a.DB.QueryRow(ctx, `SELECT id, name, kind, mode, created_at, updated_at, enabled, allow_mass_unpublish, schedule, config, credentials_enc, next_run_at, last_run_id, last_fingerprint FROM import_sources WHERE id = ? AND deleted = 0`, id).
		Scan(&s.ID, &s.Name, &s.Kind, &s.Mode, &s.CreatedAt, &s.UpdatedAt, &enabled, &mass, &sched, &cfg, &s.CredsEnc, &s.NextRun, &s.LastRunID, &s.Fingerprint)
	if db.NotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.Enabled, s.AllowMass = enabled == 1, mass == 1
	_ = json.Unmarshal([]byte(sched), &s.Schedule)
	_ = json.Unmarshal([]byte(cfg), &s.Config)
	return s, nil
}

func (a *App) sourceCreds(s *sourceRow) sourceCreds {
	var c sourceCreds
	if s.CredsEnc != nil {
		if b, err := a.Keys.Decrypt(*s.CredsEnc); err == nil {
			_ = json.Unmarshal(b, &c)
		}
	}
	return c
}

func (a *App) sourceJSON(ctx context.Context, s *sourceRow) map[string]any {
	c := a.sourceCreds(s)
	headers := []string{}
	for k := range c.Headers {
		headers = append(headers, k)
	}
	set := map[string]any{"password": c.Password != nil, "private_key": c.PrivateKey != nil, "access_key_id": c.AccessKeyID != nil,
		"secret_access_key": c.SecretAccessKey != nil, "headers": headers}
	var last any
	if j := a.latestSourceRun(ctx, s.ID); j != nil {
		last = a.importRunJSON(ctx, j)
	}
	cfg := cloneDoc(s.Config)
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfg["kind"] = s.Kind
	return map[string]any{"id": s.ID, "name": s.Name, "kind": s.Kind, "enabled": s.Enabled, "schedule": s.Schedule, "mode": s.Mode,
		"allow_mass_unpublish": s.AllowMass, "config": cfg, "credentials_set": set, "last_run": last, "next_run_at": s.NextRun,
		"created_at": s.CreatedAt, "updated_at": s.UpdatedAt}
}

func (a *App) latestSourceRun(ctx context.Context, sourceID string) *jobRow {
	j, err := scanJob(a.DB.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE kind = 'import' AND source_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, sourceID))
	if err != nil {
		return nil
	}
	return j
}

// validateSource checks a full source document (after merge).
func (a *App) validateSource(ctx context.Context, doc map[string]any) *problem.Problem {
	check := map[string]any{}
	for k, v := range doc {
		if k != "credentials" {
			check[k] = v
		}
	}
	check["id"], check["created_at"], check["updated_at"] = "src_x", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"
	if cfg, ok := check["config"].(map[string]any); ok {
		cfg = cloneDoc(cfg)
		cfg["kind"] = doc["kind"]
		check["config"] = cfg
	}
	if errs := lenientValidate("ImportSource", check); errs != nil {
		return validationProblem(errs)
	}
	cfg, _ := doc["config"].(map[string]any)
	switch doc["kind"] {
	case "https_url":
		for _, k := range []string{"url", "images_url"} {
			if u, ok := cfg[k].(string); ok && u != "" {
				if err := a.Policy.CheckURL(ctx, u, "https"); err != nil {
					return problem.New("unsafe_url", "%s", err.Error()).Field("/config/"+k, "unsafe_url", err.Error())
				}
			}
		}
	case "sftp":
		host, _ := cfg["host"].(string)
		if err := a.Policy.CheckHost(ctx, host); err != nil {
			return problem.New("unsafe_url", "%s", err.Error()).Field("/config/host", "unsafe_url", err.Error())
		}
	case "s3":
		if ep, ok := cfg["endpoint"].(string); ok && ep != "" {
			if err := a.Policy.CheckURL(ctx, ep, "https"); err != nil {
				return problem.New("unsafe_url", "%s", err.Error()).Field("/config/endpoint", "unsafe_url", err.Error())
			}
		}
	}
	if sched, ok := doc["schedule"].(map[string]any); ok {
		if tz, ok := sched["timezone"].(string); ok && tz != "" {
			if _, err := time.LoadLocation(tz); err != nil {
				return problem.New("validation_failed", "Unknown timezone %q.", tz).Field("/schedule/timezone", "validation_failed", "unknown timezone")
			}
		}
		if f, _ := sched["frequency"].(string); (f == "daily" || f == "weekly") && sched["time"] == nil {
			sched["time"] = "03:00"
		}
		if f, _ := sched["frequency"].(string); f == "weekly" && sched["weekday"] == nil {
			sched["weekday"] = 1
		}
	}
	return nil
}

// mergeCreds applies write-only credential changes (omit = keep, null = clear).
func mergeCreds(cur sourceCreds, patch map[string]any) sourceCreds {
	set := func(k string, dst **string) {
		v, ok := patch[k]
		if !ok {
			return
		}
		if s, isStr := v.(string); isStr && s != "" {
			*dst = &s
		} else {
			*dst = nil
		}
	}
	set("password", &cur.Password)
	set("private_key", &cur.PrivateKey)
	set("access_key_id", &cur.AccessKeyID)
	set("secret_access_key", &cur.SecretAccessKey)
	if v, ok := patch["headers"]; ok {
		if m, isMap := v.(map[string]any); isMap {
			cur.Headers = map[string]string{}
			for k, x := range m {
				if s, ok := x.(string); ok {
					cur.Headers[k] = s
				}
			}
		} else {
			cur.Headers = nil
		}
	}
	return cur
}

func (a *App) adminSources(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rs, err := a.DB.Query(ctx, `SELECT id FROM import_sources WHERE deleted = 0 ORDER BY created_at`)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var list []string
	for rs.Next() {
		var id string
		if rs.Scan(&id) == nil {
			list = append(list, id)
		}
	}
	rs.Close()
	items := []any{}
	for _, id := range list {
		if s, _ := a.loadSource(ctx, id); s != nil {
			items = append(items, a.sourceJSON(ctx, s))
		}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (a *App) adminSourceCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	doc, p := readJSONObject(r)
	if p != nil {
		problem.Write(w, p)
		return
	}
	doc = normalizeNumbers(doc).(map[string]any)
	if _, ok := doc["enabled"]; !ok {
		doc["enabled"] = true
	}
	if _, ok := doc["mode"]; !ok {
		doc["mode"] = "upsert"
	}
	if _, ok := doc["schedule"]; !ok {
		doc["schedule"] = map[string]any{"frequency": "daily", "time": "03:00", "timezone": "UTC"}
	}
	if p := a.validateSource(ctx, doc); p != nil {
		problem.Write(w, p)
		return
	}
	var creds sourceCreds
	if c, ok := doc["credentials"].(map[string]any); ok {
		creds = mergeCreds(creds, c)
	}
	enc, err := a.Keys.Encrypt(mustJSON(creds))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	cfg, _ := doc["config"].(map[string]any)
	delete(cfg, "kind")
	sched, _ := doc["schedule"].(map[string]any)
	id := ids.New("src")
	now := a.nowStr()
	enabled, _ := doc["enabled"].(bool)
	mass, _ := doc["allow_mass_unpublish"].(bool)
	next := nextRun(sched, a.now(), enabled)
	if _, err := a.DB.Exec(ctx, `INSERT INTO import_sources (id, name, kind, enabled, schedule, mode, allow_mass_unpublish, config, credentials_enc, created_at, updated_at, next_run_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, strings.TrimSpace(doc["name"].(string)), doc["kind"], db.Bool(enabled), string(mustJSON(sched)), doc["mode"], db.Bool(mass), string(mustJSON(cfg)), enc, now, now, next); err != nil {
		a.fail(w, r, err)
		return
	}
	s, _ := a.loadSource(ctx, id)
	writeJSON(w, 201, a.sourceJSON(ctx, s))
}

func (a *App) adminSourceGet(w http.ResponseWriter, r *http.Request) {
	s, err := a.loadSource(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if s == nil {
		failP(w, "not_found", "Import source not found.")
		return
	}
	writeJSON(w, 200, a.sourceJSON(r.Context(), s))
}

func (a *App) adminSourcePatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s, err := a.loadSource(ctx, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if s == nil {
		failP(w, "not_found", "Import source not found.")
		return
	}
	patch, p := readJSONObject(r)
	if p != nil {
		problem.Write(w, p)
		return
	}
	patch = normalizeNumbers(patch).(map[string]any)
	if k, ok := patch["kind"]; ok && k != s.Kind {
		failP(w, "validation_failed", "The kind of a source cannot change; create a new source.")
		return
	}
	credPatch, hasCreds := patch["credentials"]
	delete(patch, "credentials")
	cur := map[string]any{"name": s.Name, "kind": s.Kind, "enabled": s.Enabled, "schedule": s.Schedule, "mode": s.Mode,
		"allow_mass_unpublish": s.AllowMass, "config": s.Config}
	merged := mergePatch(cloneDoc(cur), patch).(map[string]any)
	if p := a.validateSource(ctx, merged); p != nil {
		problem.Write(w, p)
		return
	}
	creds := a.sourceCreds(s)
	if hasCreds {
		if m, ok := credPatch.(map[string]any); ok {
			creds = mergeCreds(creds, m)
		} else {
			creds = sourceCreds{}
		}
	}
	enc, err := a.Keys.Encrypt(mustJSON(creds))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	cfg, _ := merged["config"].(map[string]any)
	delete(cfg, "kind")
	sched, _ := merged["schedule"].(map[string]any)
	enabled, _ := merged["enabled"].(bool)
	mass, _ := merged["allow_mass_unpublish"].(bool)
	next := nextRun(sched, a.now(), enabled)
	fingerprint := s.Fingerprint
	if string(mustJSON(cfg)) != string(mustJSON(s.Config)) {
		fingerprint = nil
	}
	if _, err := a.DB.Exec(ctx, `UPDATE import_sources SET name = ?, enabled = ?, schedule = ?, mode = ?, allow_mass_unpublish = ?, config = ?, credentials_enc = ?, updated_at = ?, next_run_at = ?, last_fingerprint = ? WHERE id = ?`,
		strings.TrimSpace(merged["name"].(string)), db.Bool(enabled), string(mustJSON(sched)), merged["mode"], db.Bool(mass), string(mustJSON(cfg)), enc, a.nowStr(), next, fingerprint, s.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	s, _ = a.loadSource(ctx, s.ID)
	writeJSON(w, 200, a.sourceJSON(ctx, s))
}

func (a *App) adminSourceDelete(w http.ResponseWriter, r *http.Request) {
	res, err := a.DB.Exec(r.Context(), `UPDATE import_sources SET deleted = 1, enabled = 0, credentials_enc = NULL, next_run_at = NULL WHERE id = ? AND deleted = 0`, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		failP(w, "not_found", "Import source not found.")
		return
	}
	w.WriteHeader(204)
}

// startSourceRun queues an import run for a source.
func (a *App) startSourceRun(ctx context.Context, s *sourceRow, trigger string, force, dryRun bool) (*jobRow, *problem.Problem) {
	var running string
	if a.DB.QueryRow(ctx, `SELECT id FROM jobs WHERE kind = 'import' AND source_id = ? AND status IN ('queued', 'running')`, s.ID).Scan(&running) == nil {
		return nil, problem.New("conflict", "This source is already importing (run %s).", running)
	}
	job, err := a.createJob(ctx, jobSpec{Kind: "import", OwnerType: "source", OwnerID: s.ID, SourceID: s.ID, Trigger: trigger,
		Payload: importPayload{SourceID: s.ID, Force: force, Options: importOptions{Mode: s.Mode, AllowMassUnpublish: s.AllowMass, DryRun: dryRun}, CanDelete: true}})
	if err != nil {
		return nil, problem.As(err)
	}
	return job, nil
}

func (a *App) adminSourceRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s, err := a.loadSource(ctx, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if s == nil {
		failP(w, "not_found", "Import source not found.")
		return
	}
	var body struct {
		DryRun bool `json:"dry_run"`
		Force  bool `json:"force"`
	}
	if p := readOptionalJSON(r, &body); p != nil {
		problem.Write(w, p)
		return
	}
	job, p := a.startSourceRun(ctx, s, "manual_run", body.Force, body.DryRun)
	if p != nil {
		problem.Write(w, p)
		return
	}
	writeJSON(w, 202, a.importRunJSON(ctx, job))
}

func (a *App) adminSourceTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s, err := a.loadSource(ctx, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if s == nil {
		failP(w, "not_found", "Import source not found.")
		return
	}
	tctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	fetcher, p := a.fetcherFor(s)
	if p != nil {
		problem.Write(w, p)
		return
	}
	files, fp, err := fetcher.list(tctx)
	out := map[string]any{"ok": err == nil, "message": nil, "host_key_fingerprint": nil, "files": []any{}}
	if hk := fetcher.hostKey(); hk != "" {
		out["host_key_fingerprint"] = hk
	}
	if err != nil {
		out["message"] = err.Error()
	} else {
		// No message on success: the UI says "Connected, N files found" in the
		// reader's language, and an English line under it only repeats that.
		list := []any{}
		for i, f := range files {
			if i == 100 {
				break
			}
			list = append(list, map[string]any{"name": f.Name, "bytes": f.Size, "modified_at": f.Modified})
		}
		out["files"] = list
		_ = fp
	}
	writeJSON(w, 200, out)
}

// ---- schedule -------------------------------------------------------------------------------

// nextRun computes the next scheduled time (nil for manual or disabled).
func nextRun(sched map[string]any, now time.Time, enabled bool) any {
	if !enabled || sched == nil {
		return nil
	}
	freq, _ := sched["frequency"].(string)
	loc := time.UTC
	if tz, ok := sched["timezone"].(string); ok && tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	local := now.In(loc)
	hh, mm := 3, 0
	if t, ok := sched["time"].(string); ok && len(t) == 5 {
		hh, mm = atoiDefault(t[:2], 3), atoiDefault(t[3:], 0)
	}
	switch freq {
	case "hourly":
		n := local.Truncate(time.Hour).Add(time.Hour)
		return db.Fmt(n)
	case "daily":
		n := time.Date(local.Year(), local.Month(), local.Day(), hh, mm, 0, 0, loc)
		if !n.After(local) {
			n = n.AddDate(0, 0, 1)
		}
		return db.Fmt(n)
	case "weekly":
		wd := 1
		switch x := sched["weekday"].(type) {
		case float64:
			wd = int(x)
		case int:
			wd = x
		case json.Number:
			wd = atoiDefault(x.String(), 1)
		}
		goWD := time.Weekday(wd % 7) // 7 = Sunday
		n := time.Date(local.Year(), local.Month(), local.Day(), hh, mm, 0, 0, loc)
		for n.Weekday() != goWD || !n.After(local) {
			n = n.AddDate(0, 0, 1)
		}
		return db.Fmt(n)
	}
	return nil
}

// scheduleDue starts runs of sources whose next_run_at passed.
func (a *App) scheduleDue(ctx context.Context) {
	rs, err := a.DB.Query(ctx, `SELECT id, next_run_at FROM import_sources WHERE deleted = 0 AND enabled = 1 AND next_run_at IS NOT NULL AND next_run_at <= ?`, a.nowStr())
	if err != nil {
		return
	}
	type due struct{ id, next string }
	var list []due
	for rs.Next() {
		var d due
		if rs.Scan(&d.id, &d.next) == nil {
			list = append(list, d)
		}
	}
	rs.Close()
	for _, d := range list {
		s, err := a.loadSource(ctx, d.id)
		if err != nil || s == nil {
			continue
		}
		next := nextRun(s.Schedule, a.now(), s.Enabled)
		res, err := a.DB.Exec(ctx, `UPDATE import_sources SET next_run_at = ? WHERE id = ? AND next_run_at = ?`, next, s.ID, d.next)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n != 1 {
			continue // claimed by someone else
		}
		if _, p := a.startSourceRun(ctx, s, "schedule", false, false); p != nil && p.ObxError != "conflict" {
			a.Log.Warn("scheduled import could not start", "source", s.ID, "error", p.Error())
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
