package tlsauto

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/openbrandexchange/node-go/internal/config"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		PublicURL: "https://portal.yourbrand.example",
		DataDir:   t.TempDir(),
		TLS:       "auto",
		ACMEEmail: "it@yourbrand.example",
		HTTPPort:  80,
		HTTPSPort: 443,
	}
}

// Only the portal's own name gets a certificate; any other name is refused
// before Let's Encrypt is contacted (no network in this test).
func TestHostPolicy(t *testing.T) {
	cfg := testConfig(t)
	m := Manager(cfg)
	if err := m.HostPolicy(context.Background(), "portal.yourbrand.example"); err != nil {
		t.Fatalf("own domain refused: %v", err)
	}
	for _, h := range []string{"other.example", "203.0.113.10", "www.portal.yourbrand.example", ""} {
		if err := m.HostPolicy(context.Background(), h); err == nil {
			t.Errorf("%q accepted", h)
		}
	}
	// A TLS hello for another name fails without an ACME request.
	if _, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "other.example"}); err == nil {
		t.Fatal("certificate for another name")
	}
	if m.Email != cfg.ACMEEmail || m.Client != nil {
		t.Fatalf("manager: email %q, client %v", m.Email, m.Client)
	}
	if got, want := CacheDir(cfg), filepath.Join(cfg.DataDir, "acme"); got != want {
		t.Fatalf("cache dir %s, want %s", got, want)
	}
}

func TestStagingDirectory(t *testing.T) {
	cfg := testConfig(t)
	cfg.ACMEDirectory = "https://acme-staging-v02.api.letsencrypt.org/directory"
	m := Manager(cfg)
	if m.Client == nil || m.Client.DirectoryURL != cfg.ACMEDirectory {
		t.Fatalf("staging directory not used: %+v", m.Client)
	}
}

func TestTLSConfig(t *testing.T) {
	c := TLSConfig(Manager(testConfig(t)))
	if c.MinVersion != tls.VersionTLS12 || !slices.Contains(c.NextProtos, "acme-tls/1") || !slices.Contains(c.NextProtos, "h2") {
		t.Fatalf("tls config: min %x, protos %v", c.MinVersion, c.NextProtos)
	}
}

func TestHTTPHandler(t *testing.T) {
	cfg := testConfig(t)
	health := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	h := HTTPHandler(Manager(cfg), cfg, health)
	do := func(method, target string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, nil)
		r.Host = "portal.yourbrand.example"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("GET", "/portal/products?q=coat"); w.Code != 301 || w.Header().Get("Location") != "https://portal.yourbrand.example/portal/products?q=coat" {
		t.Fatalf("GET redirect: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := do("POST", "/api/login"); w.Code != 308 {
		t.Fatalf("POST redirect: %d", w.Code)
	}
	if w := do("GET", "/healthz"); w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("healthz: %d %q", w.Code, w.Body.String())
	}
	// An unknown challenge token is answered by the ACME handler, not redirected.
	if w := do("GET", "/.well-known/acme-challenge/unknown-token"); w.Code == 301 || w.Code == 200 {
		t.Fatalf("challenge path: %d", w.Code)
	}

	cfg.HTTPSPort = 8443
	h = HTTPHandler(Manager(cfg), cfg, health)
	if w := do("GET", "/"); !strings.HasPrefix(w.Header().Get("Location"), "https://portal.yourbrand.example:8443/") {
		t.Fatalf("redirect with port: %s", w.Header().Get("Location"))
	}
}
