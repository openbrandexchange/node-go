package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openbrandexchange/node-go/internal/blobstore"
	"github.com/openbrandexchange/node-go/internal/config"
	"github.com/openbrandexchange/node-go/internal/db"
)

func TestHealthAndReadiness(t *testing.T) {
	n := newTestNode(t)
	c := n.client()
	if r := c.do("GET", "/healthz", nil); r.Status != 200 || string(r.Body) != "ok\n" {
		t.Fatalf("healthz: %d %s", r.Status, r.Body)
	}
	ready := c.must(200, "GET", "/readyz", nil)
	if ready["status"] != "ready" || ready["schema_version"].(float64) != float64(db.SchemaVersion()) {
		t.Fatalf("readyz: %v", ready)
	}
	// Before setup the probes still answer (a fresh node is healthy).
	n.app.SetDraining()
	r := c.do("GET", "/readyz", nil)
	if r.Status != 503 || !strings.Contains(string(r.Body), "shutting down") {
		t.Fatalf("draining readyz: %d %s", r.Status, r.Body)
	}
	if r := c.do("GET", "/healthz", nil); r.Status != 200 {
		t.Fatalf("healthz while draining: %d", r.Status)
	}
}

func TestRequestIDsAndSecurityHeaders(t *testing.T) {
	n := newTestNode(t)
	c := n.client()
	r := c.do("GET", "/admin/api/setup", nil)
	id := r.Header.Get("X-Request-Id")
	if len(id) != 24 {
		t.Fatalf("request id: %q", id)
	}
	if r2 := c.do("GET", "/admin/api/setup", nil); r2.Header.Get("X-Request-Id") == id {
		t.Fatal("request ids repeat")
	}
	r = c.do("GET", "/admin/api/setup", nil, "X-Request-Id", "lb-trace-0123456789")
	if got := r.Header.Get("X-Request-Id"); got != "lb-trace-0123456789" {
		t.Fatalf("incoming id not kept: %q", got)
	}
	r = c.do("GET", "/admin/api/setup", nil, "X-Request-Id", "bad id\" with spaces")
	if got := r.Header.Get("X-Request-Id"); got == "" || strings.Contains(got, " ") {
		t.Fatalf("unsafe incoming id echoed: %q", got)
	}
	for _, p := range []string{"/admin/api/setup", "/obx/v1/changes", "/"} {
		h := c.do("GET", p, nil).Header
		for _, k := range []string{"X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options", "Content-Security-Policy"} {
			if h.Get(k) == "" {
				t.Errorf("%s: missing %s", p, k)
			}
		}
		if !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s: CSP without frame-ancestors: %s", p, h.Get("Content-Security-Policy"))
		}
		if h.Get("Strict-Transport-Security") != "" {
			t.Errorf("%s: HSTS over plain http", p)
		}
		if p != "/.well-known/obx" && h.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: CORS header outside discovery", p)
		}
	}
	// Behind TLS (public address https) browsers are pinned to https.
	n.app.Cfg.PublicURL = "https://images.kestrel.example"
	if h := c.do("GET", "/admin/api/setup", nil).Header.Get("Strict-Transport-Security"); !strings.HasPrefix(h, "max-age=") {
		t.Fatalf("HSTS: %q", h)
	}
}

func TestMetrics(t *testing.T) {
	n := newTestNode(t)
	c := n.client()
	if r := c.do("GET", "/metrics", nil); r.Status != 404 {
		t.Fatalf("metrics off: %d", r.Status)
	}
	n.app.Cfg.Metrics = true
	n.app.Cfg.MetricsToken = "metrics-token-0123456789"
	if r := c.do("GET", "/metrics", nil); r.Status != 401 {
		t.Fatalf("metrics without token: %d", r.Status)
	}
	c.do("GET", "/admin/api/setup", nil)
	r := c.do("GET", "/metrics", nil, "Authorization", "Bearer metrics-token-0123456789")
	body := string(r.Body)
	for _, want := range []string{
		`obx_http_requests_total{method="GET",route="/admin/api/setup",code="200"}`,
		`obx_http_request_duration_seconds_bucket{route="/admin/api/setup",le="+Inf"}`,
		`obx_build_info{version=`, `obx_mail_outbox_pending 0`, `obx_catalog_visible{type="product"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	if strings.Contains(body, "route=\"/admin/api/invites/") || strings.Contains(body, "lb-trace") {
		t.Error("metrics carry unbounded labels")
	}
}

func TestAuditLog(t *testing.T) {
	n := newTestNode(t)
	admin := n.setup()
	ret := admin.must(201, "POST", "/admin/api/retailers", map[string]any{"name": "Audit Store"})
	admin.must(200, "PATCH", "/admin/api/retailers/"+ret["id"].(string), map[string]any{"notes": "call first"})
	admin.must(201, "POST", "/admin/api/users/invites", map[string]any{"email": "x@store.example", "name": "X", "role": "retailer", "retailer_id": ret["id"]})
	bad := n.client()
	bad.session()
	if r := bad.do("POST", "/admin/api/session/login", map[string]any{"email": adminEmail, "password": "wrong password here"}); r.Status != 401 {
		t.Fatalf("bad login: %d", r.Status)
	}
	log := admin.must(200, "GET", "/admin/api/audit-log?limit=100", nil)
	var actions []string
	byAction := map[string]map[string]any{}
	for _, it := range log["items"].([]any) {
		m := it.(map[string]any)
		actions = append(actions, m["action"].(string))
		byAction[m["action"].(string)] = m
	}
	for _, want := range []string{"setup.complete", "retailers.create", "retailers.update", "users.invites.create", "session.login_failed"} {
		if byAction[want] == nil {
			t.Errorf("audit log lacks %s: %v", want, actions)
		}
	}
	upd := byAction["retailers.update"]
	if upd["target"].(map[string]any)["id"] != ret["id"] || upd["actor"].(map[string]any)["email"] != adminEmail || upd["status"].(float64) != 200 {
		t.Errorf("retailers.update entry: %v", upd)
	}
	if upd["request_id"] == nil {
		t.Error("audit entry without request id")
	}
	if f := byAction["session.login_failed"]; f["details"].(map[string]any)["reason"] != "invalid_credentials" {
		t.Errorf("failed login entry: %v", f)
	}
	// Newest first, paged with a cursor.
	p1 := admin.must(200, "GET", "/admin/api/audit-log?limit=2", nil)
	if len(p1["items"].([]any)) != 2 || p1["next_cursor"] == nil {
		t.Fatalf("page 1: %v", p1)
	}
	p2 := admin.must(200, "GET", "/admin/api/audit-log?limit=2&cursor="+p1["next_cursor"].(string), nil)
	if p2["items"].([]any)[0].(map[string]any)["id"] == p1["items"].([]any)[1].(map[string]any)["id"] {
		t.Fatal("cursor repeats entries")
	}
	// Retailers' own work is not the brand's audit log; they cannot read it.
	if r := bad.do("GET", "/admin/api/audit-log", nil); r.Status != 401 {
		t.Fatalf("anonymous audit log: %d", r.Status)
	}
	// Secrets never reach the audit log.
	raw, _ := json.Marshal(log)
	if strings.Contains(string(raw), adminPassword) || strings.Contains(string(raw), "wrong password") {
		t.Fatal("password in audit log")
	}
}

func TestPersonalDataExportAndDeletion(t *testing.T) {
	n := newTestNode(t)
	admin := n.setup()
	ret := admin.must(201, "POST", "/admin/api/retailers", map[string]any{"name": "Privacy Store", "country": "NL"})
	rid := ret["id"].(string)
	inv := admin.must(201, "POST", "/admin/api/users/invites", map[string]any{"email": "pia@privacy.example", "name": "Pia", "role": "retailer", "retailer_id": rid})
	link := inv["invite_url"].(string)
	u := n.client()
	u.session()
	m := u.must(200, "POST", "/admin/api/invites/"+link[strings.LastIndex(link, "/")+1:]+"/accept", map[string]any{"password": "pias secure password"})
	u.csrf = m["csrf_token"].(string)
	admin.must(201, "POST", "/admin/api/api-clients", map[string]any{"retailer_id": rid, "name": "Privacy PIM"})

	// The person downloads everything stored about her.
	r := u.do("GET", "/admin/api/me/personal-data", nil)
	if r.Status != 200 || !strings.Contains(r.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("my data: %d %s", r.Status, r.Body)
	}
	mine := r.JSON(t)
	if mine["account"].(map[string]any)["email"] != "pia@privacy.example" {
		t.Fatalf("my data: %v", mine)
	}
	if strings.Contains(string(r.Body), "password_hash") || strings.Contains(string(r.Body), "$argon2") || strings.Contains(string(r.Body), "$2a$") {
		t.Fatal("password hash in export")
	}
	// The brand exports the whole retailer.
	all := admin.must(200, "GET", "/admin/api/retailers/"+rid+"/personal-data", nil)
	if all["retailer"].(map[string]any)["name"] != "Privacy Store" || len(all["users"].([]any)) != 1 || len(all["api_clients"].([]any)) != 1 {
		t.Fatalf("retailer export: %v", all)
	}
	raw, _ := json.Marshal(all)
	if strings.Contains(string(raw), "obxs_") || strings.Contains(string(raw), "secret_hash") {
		t.Fatal("secret in retailer export")
	}
	if r := u.do("GET", "/admin/api/retailers/"+rid+"/personal-data", nil); r.Status != 403 {
		t.Fatalf("retailer user reads company export: %d", r.Status)
	}

	// Deleting the retailer removes the people; the audit log keeps the
	// action without the e-mail address.
	admin.must(204, "DELETE", "/admin/api/retailers/"+rid, nil)
	ctx := context.Background()
	var users, clients int
	_ = n.app.DB.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE email = ?`, "pia@privacy.example").Scan(&users)
	_ = n.app.DB.QueryRow(ctx, `SELECT COUNT(*) FROM api_clients WHERE retailer_id = ?`, rid).Scan(&clients)
	var emails int
	_ = n.app.DB.QueryRow(ctx, `SELECT COUNT(*) FROM audit_log WHERE actor_email = ?`, "pia@privacy.example").Scan(&emails)
	if users != 0 || clients != 0 || emails != 0 {
		t.Fatalf("left after deletion: users=%d clients=%d audit emails=%d", users, clients, emails)
	}
	if r := admin.do("GET", "/admin/api/retailers/"+rid+"/personal-data", nil); r.Status != 404 {
		t.Fatalf("export after deletion: %d", r.Status)
	}
}

func TestRetention(t *testing.T) {
	n := newTestNode(t)
	n.setup()
	ctx := context.Background()
	old := db.Fmt(time.Now().AddDate(0, 0, -400))
	recent := db.Fmt(time.Now().AddDate(0, 0, -10))
	for _, at := range []string{old, recent} {
		_, _ = n.app.DB.Exec(ctx, `INSERT INTO download_events (id, at, kind, actor_type, files) VALUES (?, ?, 'image', 'user', 1)`, "dl"+at, at)
		_, _ = n.app.DB.Exec(ctx, `INSERT INTO audit_log (id, at, actor_type, action) VALUES (?, ?, 'user', 'x')`, "au"+at, at)
		_, _ = n.app.DB.Exec(ctx, `INSERT INTO access_requests (id, status, token_hash, retailer, contact, access, created_at, decided_at) VALUES (?, 'rejected', 'h', '{}', '{}', '[]', ?, ?)`, "ar"+at, at, at)
	}
	n.app.Cfg.StatsRetentionDays = 30
	n.app.cleanup(ctx)
	count := func(table string) int {
		var c int
		_ = n.app.DB.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&c)
		return c
	}
	if got := count("download_events"); got != 1 {
		t.Errorf("download events after 30-day retention: %d", got)
	}
	if got := count("audit_log"); got < 1 || got > 2 { // setup.complete + recent
		t.Errorf("audit log after 365-day retention: %d", got)
	}
	var ar int
	_ = n.app.DB.QueryRow(ctx, `SELECT COUNT(*) FROM access_requests WHERE status = 'rejected'`).Scan(&ar)
	if ar != 1 {
		t.Errorf("rejected access requests after 180 days: %d", ar)
	}
}

// flakyMailer fails the first sends, then works.
type flakyMailer struct {
	mu    sync.Mutex
	fails int
	sent  []SentMail
}

func (f *flakyMailer) Send(_ context.Context, to, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return errors.New("421 try again later")
	}
	f.sent = append(f.sent, SentMail{to, subject, body})
	return nil
}
func (f *flakyMailer) Configured() bool { return true }

func TestMailRetry(t *testing.T) {
	n := newTestNode(t)
	admin := n.setup()
	fm := &flakyMailer{fails: 2}
	n.app.Mail = fm
	ret := admin.must(201, "POST", "/admin/api/retailers", map[string]any{"name": "Mail Store"})
	inv := admin.must(201, "POST", "/admin/api/users/invites", map[string]any{"email": "mo@mail.example", "name": "Mo", "role": "retailer", "retailer_id": ret["id"]})
	if inv["email_sent"] == true {
		t.Fatalf("invite claims the e-mail was sent: %v", inv)
	}
	ctx := context.Background()
	clock := time.Now()
	n.app.Now = func() time.Time { return clock }
	n.app.retryMail(ctx) // not due yet
	if len(fm.sent) != 0 {
		t.Fatal("retried before the backoff")
	}
	clock = clock.Add(2 * time.Minute)
	n.app.retryMail(ctx) // second failure
	clock = clock.Add(10 * time.Minute)
	n.app.retryMail(ctx)
	if len(fm.sent) != 1 || fm.sent[0].To != "mo@mail.example" || !strings.Contains(fm.sent[0].Body, "/invite/") {
		t.Fatalf("after retries: %+v", fm.sent)
	}
	var status, body string
	_ = n.app.DB.QueryRow(ctx, `SELECT status, body_enc FROM mail_outbox`).Scan(&status, &body)
	if status != "sent" || body != "" {
		t.Fatalf("outbox row after sending: %s %q", status, body)
	}
}

func TestSchemaNewerThanBinaryIsRefused(t *testing.T) {
	dir := t.TempDir()
	url := "sqlite:" + filepath.Join(dir, "obx.db")
	d, err := db.Open(url)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _ = d.SQL.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, 'x')`, db.SchemaVersion()+1)
	d.Close()
	cfg := &config.Config{PublicURL: "http://localhost:8080", DataDir: dir, DatabaseURL: url, BlobURL: "file://" + filepath.Join(dir, "blobs"),
		SecretKey: []byte("test-secret-key-that-is-long-enough-1234567890"), Workers: 1}
	_, err = New(context.Background(), cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var tooNew *db.ErrSchemaTooNew
	if !errors.As(err, &tooNew) || !strings.Contains(err.Error(), "restore the backup") {
		t.Fatalf("want a schema-too-new error, got %v", err)
	}
}

func TestBackupAndRestore(t *testing.T) {
	k := newKestrelNode(t)
	ctx := context.Background()
	before := k.admin.must(200, "GET", "/admin/api/catalog/products?limit=100", nil)
	backup := filepath.Join(t.TempDir(), "backup")
	m, err := k.app.Backup(ctx, backup)
	if err != nil {
		t.Fatal(err)
	}
	if m.Database != "obx.db" || m.Files != "blobs" || m.FileCount == 0 {
		t.Fatalf("manifest: %+v", m)
	}
	if _, err := k.app.Backup(ctx, backup); err == nil {
		t.Fatal("backup into a used directory")
	}
	// Restore into a brand-new data directory and start a node on it.
	dir := t.TempDir()
	cfg := &config.Config{PublicURL: k.url, DataDir: dir, DatabaseURL: "sqlite:" + filepath.Join(dir, "obx.db"),
		BlobURL: "file://" + filepath.Join(dir, "blobs"), SecretKey: k.app.Cfg.SecretKey, SecretFromEnv: true, Workers: 1}
	if _, err := Restore(cfg, backup, false); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(cfg, backup, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("restore over existing data without --force: %v", err)
	}
	wrong := *cfg
	wrong.SecretKey = []byte("another-secret-key-that-is-long-enough-000000")
	if _, err := Restore(&wrong, backup, true); err != nil {
		t.Fatalf("forced restore with another key: %v", err)
	}
	wrong.SecretKey = []byte("another-secret-key-that-is-long-enough-000000")
	if _, err := Restore(&wrong, backup, false); err == nil || !strings.Contains(err.Error(), "OBX_SECRET_KEY differs") {
		t.Fatalf("restore with another key: %v", err)
	}
	app, err := New(ctx, cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if app.NodeID() != k.app.NodeID() || !app.SetupDone(ctx) {
		t.Fatal("restored node is not the same node")
	}
	var products int
	_ = app.DB.QueryRow(ctx, `SELECT COUNT(*) FROM products`).Scan(&products)
	if products != len(before["items"].([]any)) {
		t.Fatalf("restored %d products, had %d", products, len(before["items"].([]any)))
	}
	// Every original referenced by an asset is readable in the restored store.
	rs, _ := app.DB.Query(ctx, `SELECT DISTINCT original_sha FROM assets WHERE original_sha IS NOT NULL`)
	var shas []string
	for rs.Next() {
		var s string
		_ = rs.Scan(&s)
		shas = append(shas, s)
	}
	rs.Close()
	if len(shas) == 0 {
		t.Fatal("no originals")
	}
	for _, s := range shas {
		if ok, err := app.Store.Exists(ctx, blobstore.BlobKey(s)); !ok || err != nil {
			t.Fatalf("original %s missing after restore (%v)", s, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "obx.db")); err != nil {
		t.Fatal(err)
	}
}

func TestColourlessImages(t *testing.T) {
	k := newKestrelNode(t)
	png, err := os.ReadFile(filepath.Join(specDir(t), "kestrel", "images", "KO-1001_NVY_1_front.png"))
	if err != nil {
		t.Fatal(err)
	}
	img := writeTemp(t, "KO-1001_ALL_7_detail.png", append(png, 0)) // different bytes, same picture
	run := k.runImport(k.admin, map[string][]string{"images": {img}}, nil)
	if run["status"] != "succeeded" || run["counts"].(map[string]any)["errors"].(float64) != 0 {
		errs := k.admin.must(200, "GET", "/admin/api/imports/"+run["id"].(string)+"/errors", nil)
		t.Fatalf("import: %v %v", run, errs)
	}
	in := k.ingestToken(k.admin)
	rec := in.must(200, "GET", "/obx/v1/ingest/assets/KO-1001_ALL_7", nil)
	doc := rec["data"].(map[string]any)
	if doc["color_code"] != nil || doc["role"] != "detail" || doc["position"].(float64) != 7 {
		t.Fatalf("colourless asset: %v", doc)
	}
	// A colour code that is neither in the catalog nor ALL stays an error.
	bad := writeTemp(t, "KO-1001_XYZ_8.png", append(png, 1))
	run = k.runImport(k.admin, map[string][]string{"images": {bad}}, nil)
	if run["counts"].(map[string]any)["errors"].(float64) != 1 {
		t.Fatalf("unknown colour: %v", run)
	}
}
