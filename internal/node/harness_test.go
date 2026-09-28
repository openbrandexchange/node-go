package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/openbrandexchange/node-go/internal/config"
	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/ids"
	"github.com/openbrandexchange/node-go/internal/schemas"
)

const testSetupCode = "TEST-SETUP-CODE"

// specDir is testdata/spec (fixtures copied from the spec repository).
func specDir(t testing.TB) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "spec")
}

type testNode struct {
	t   *testing.T
	app *App
	srv *httptest.Server
	url string
}

// newTestNode starts a fresh node on a random port with SQLite and local
// blobs in a temp dir. Background workers are not started: tests run jobs
// explicitly with app.RunJobsNow.
func newTestNode(t *testing.T) *testNode {
	t.Helper()
	dir := t.TempDir()
	srv := httptest.NewUnstartedServer(nil)
	base := "http://" + srv.Listener.Addr().String()
	cfg := &config.Config{
		PublicURL: base, Port: 8080, DataDir: dir,
		DatabaseURL: "sqlite:" + filepath.Join(dir, "obx.db"), BlobURL: "file://" + filepath.Join(dir, "blobs"),
		SecretKey: []byte("test-secret-key-that-is-long-enough-1234567890"), SetupCode: testSetupCode,
		AllowPrivateURLs: true, Workers: 1, LogFormat: "text", ChangeRetentionDays: 400, MailFrom: "no-reply@example.test",
	}
	if pg := os.Getenv("OBX_TEST_DATABASE_URL"); pg != "" {
		// One schema per test node on a shared Postgres server.
		schema := "t_" + strings.ToLower(ids.New("s")[2:])
		d, err := db.Open(pg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.SQL.Exec(`CREATE SCHEMA ` + schema); err != nil {
			t.Fatal(err)
		}
		d.Close()
		sep := "?"
		if strings.Contains(pg, "?") {
			sep = "&"
		}
		cfg.DatabaseURL = pg + sep + "search_path=" + schema
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("OBX_TEST_LOG") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	logWriter = io.Discard
	app, err := New(context.Background(), cfg, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = app.Handler()
	srv.Start()
	t.Cleanup(func() {
		srv.Close()
		app.Close()
	})
	return &testNode{t: t, app: app, srv: srv, url: base}
}

// client is an HTTP client with a cookie jar and the admin CSRF token.
type client struct {
	n      *testNode
	hc     *http.Client
	csrf   string
	bearer string
}

func (n *testNode) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{n: n, hc: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type resp struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r *resp) JSON(t testing.TB) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("not a JSON object (%d): %s", r.Status, r.Body)
	}
	return m
}

func (c *client) do(method, path string, body any, headers ...string) *resp {
	c.n.t.Helper()
	var rd io.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
		ct = "application/octet-stream"
	case string:
		rd = strings.NewReader(b)
		ct = "application/json"
	case io.Reader:
		rd = b
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
		ct = "application/json"
	}
	u := path
	if !strings.HasPrefix(path, "http") {
		u = c.n.url + path
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		c.n.t.Fatal(err)
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if c.csrf != "" && method != "GET" && method != "HEAD" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := c.hc.Do(req)
	if err != nil {
		c.n.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return &resp{Status: res.StatusCode, Header: res.Header, Body: data}
}

func (c *client) must(status int, method, path string, body any, headers ...string) map[string]any {
	c.n.t.Helper()
	r := c.do(method, path, body, headers...)
	if r.Status != status {
		c.n.t.Fatalf("%s %s: want %d, got %d: %s", method, path, status, r.Status, r.Body)
	}
	if len(r.Body) == 0 {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(r.Body, &m)
	return m
}

// session fetches the CSRF token.
func (c *client) session() map[string]any {
	m := c.must(200, "GET", "/admin/api/session", nil)
	c.csrf = m["csrf_token"].(string)
	return m
}

func (c *client) login(email, password string) {
	c.session()
	m := c.must(200, "POST", "/admin/api/session/login", map[string]any{"email": email, "password": password})
	c.csrf = m["csrf_token"].(string)
}

const adminEmail = "admin@kestrel.example"
const adminPassword = "correct horse battery"

// setup completes first-run setup as in the conformance reference path.
func (n *testNode) setup() *client {
	c := n.client()
	c.session()
	m := c.must(201, "POST", "/admin/api/setup", map[string]any{
		"setup_code": testSetupCode,
		"company":    map[string]any{"name": "Kestrel Outfitters", "first_brand_name": "Kestrel", "default_language": "nl", "languages": []string{"nl", "en"}},
		"admin":      map[string]any{"name": "Ada Admin", "email": adminEmail, "password": adminPassword},
	})
	c.csrf = m["csrf_token"].(string)
	return c
}

// multipartBody builds a multipart form.
func multipartBody(t testing.TB, files map[string][]string, fields map[string]string) (io.Reader, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, field := range keys {
		for _, p := range files[field] {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			w, _ := mw.CreateFormFile(field, filepath.Base(p))
			_, _ = w.Write(data)
		}
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func (n *testNode) kestrelFiles() map[string][]string {
	dir := filepath.Join(specDir(n.t), "kestrel")
	imgs, _ := filepath.Glob(filepath.Join(dir, "images", "*.png"))
	return map[string][]string{"csv": {filepath.Join(dir, "products.csv")}, "assets_csv": {filepath.Join(dir, "assets.csv")}, "images": imgs}
}

// importKestrel imports fixtures/kestrel through the admin API.
func (n *testNode) importKestrel(c *client) map[string]any {
	body, ct := multipartBody(n.t, n.kestrelFiles(), nil)
	run := c.must(202, "POST", "/admin/api/imports", body, "Content-Type", ct)
	n.app.RunJobsNow(context.Background())
	return c.must(200, "GET", "/admin/api/imports/"+run["id"].(string), nil)
}

// apiClient creates a retailer (with optional grant) and an API client,
// and returns a protocol client with a token.
func (n *testNode) apiClient(admin *client, name string, retailer map[string]any) (*client, map[string]any) {
	retailer["name"] = name
	ret := admin.must(201, "POST", "/admin/api/retailers", retailer)
	cl := admin.must(201, "POST", "/admin/api/api-clients", map[string]any{"retailer_id": ret["id"], "name": name + " PIM"})
	id := cl["client"].(map[string]any)["client_id"].(string)
	secret := cl["client_secret"].(string)
	tok := n.token(id, secret, "")
	c := n.client()
	c.bearer = tok
	return c, map[string]any{"retailer": ret, "client_id": id, "client_secret": secret, "api_client": cl["client"]}
}

func (n *testNode) token(id, secret, scope string) string {
	form := url.Values{"grant_type": {"client_credentials"}}
	if scope != "" {
		form.Set("scope", scope)
	}
	req, _ := http.NewRequest("POST", n.url+"/obx/v1/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(id), url.QueryEscape(secret))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		n.t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	if res.StatusCode != 200 {
		n.t.Fatalf("token: %d %v", res.StatusCode, m)
	}
	return m["access_token"].(string)
}

func (n *testNode) ingestToken(admin *client, scopes ...string) *client {
	if len(scopes) == 0 {
		scopes = []string{"ingest:read", "ingest:write", "ingest:delete"}
	}
	m := admin.must(201, "POST", "/admin/api/ingest-tokens", map[string]any{"name": "Automation", "scopes": scopes})
	c := n.client()
	c.bearer = m["secret"].(string)
	return c
}

// ---- schema validation and fixture matching --------------------------------------------

// validate checks v against a schema (strict: closed objects).
func validate(t testing.TB, schema string, v any) {
	t.Helper()
	raw, _ := json.Marshal(v)
	if errs := schemas.Strict().ValidateJSON(schema, raw); errs != nil {
		t.Fatalf("%s does not validate: %v\n%s", schema, errs, truncateForLog(raw))
	}
}

func truncateForLog(b []byte) string {
	if len(b) > 3000 {
		return string(b[:3000]) + "..."
	}
	return string(b)
}

var sha256Re = regexp.MustCompile(`^[a-f0-9]{64}$`)

// match compares actual with an expected fixture (docs/conformance.md).
func match(t testing.TB, path string, expected, actual any, products map[string]any) {
	t.Helper()
	switch e := expected.(type) {
	case string:
		switch {
		case e == "{{id}}":
			if s, ok := actual.(string); !ok || s == "" {
				t.Errorf("%s: want an id, got %v", path, actual)
			}
			return
		case e == "{{datetime}}":
			if s, ok := actual.(string); !ok || len(s) < 20 {
				t.Errorf("%s: want a datetime, got %v", path, actual)
			}
			return
		case e == "{{datetime|null}}":
			if s, ok := actual.(string); actual != nil && (!ok || len(s) < 20) {
				t.Errorf("%s: want a datetime or null, got %v", path, actual)
			}
			return
		case e == "{{url}}":
			if s, ok := actual.(string); !ok || !strings.HasPrefix(s, "http") {
				t.Errorf("%s: want a URL, got %v", path, actual)
			}
			return
		case e == "{{sha256}}":
			if s, ok := actual.(string); !ok || !sha256Re.MatchString(s) {
				t.Errorf("%s: want a sha256, got %v", path, actual)
			}
			return
		case e == "{{int}}":
			if _, ok := actual.(float64); !ok {
				t.Errorf("%s: want an integer, got %v", path, actual)
			}
			return
		case strings.HasPrefix(e, "{{product:"):
			ext := strings.TrimSuffix(strings.TrimPrefix(e, "{{product:"), "}}")
			match(t, path, products[ext], actual, products)
			return
		}
		if actual != e {
			t.Errorf("%s: want %q, got %v", path, e, actual)
		}
	case map[string]any:
		a, ok := actual.(map[string]any)
		if !ok {
			t.Errorf("%s: want object, got %T", path, actual)
			return
		}
		for k, v := range e {
			av, present := a[k]
			if !present {
				t.Errorf("%s/%s: missing", path, k)
				continue
			}
			match(t, path+"/"+k, v, av, products)
		}
		for k := range a {
			if _, ok := e[k]; !ok && !strings.HasPrefix(k, "x-") {
				t.Errorf("%s/%s: unexpected key", path, k)
			}
		}
	case []any:
		a, ok := actual.([]any)
		if !ok || len(a) != len(e) {
			t.Errorf("%s: want array of %d, got %v", path, len(e), actual)
			return
		}
		for i := range e {
			match(t, fmt.Sprintf("%s/%d", path, i), e[i], a[i], products)
		}
	default:
		if fmt.Sprint(expected) != fmt.Sprint(actual) {
			t.Errorf("%s: want %v, got %v", path, expected, actual)
		}
	}
}

func readFixture(t testing.TB, rel string) any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(specDir(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// drainChanges reads /changes from cursor until has_more is false.
func drainChanges(t *testing.T, c *client, cursor string, limit int) ([]map[string]any, string, []map[string]any) {
	t.Helper()
	var all []map[string]any
	var pages []map[string]any
	for i := 0; i < 10000; i++ {
		q := fmt.Sprintf("/obx/v1/changes?limit=%d", limit)
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		page := c.must(200, "GET", q, nil)
		validate(t, "ChangesPage", page)
		pages = append(pages, page)
		for _, ch := range page["changes"].([]any) {
			all = append(all, ch.(map[string]any))
		}
		cursor = page["next_cursor"].(string)
		if !page["has_more"].(bool) {
			return all, cursor, pages
		}
	}
	t.Fatal("feed did not end")
	return nil, "", nil
}
