package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearEnv(t *testing.T) {
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "OBX_") || k == "PORT" || k == "DATABASE_URL" || k == "RENDER_EXTERNAL_URL" || k == "RAILWAY_PUBLIC_DOMAIN" || k == "FLY_APP_NAME" {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
	t.Setenv("OBX_DATA_DIR", t.TempDir())
}

func TestDefaults(t *testing.T) {
	clearEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 8080 || c.PublicURL != "http://localhost:8080" || c.Workers != 2 || !c.AccessLog || c.Metrics {
		t.Fatalf("defaults: %+v", c)
	}
	if c.JobRetentionDays != 30 || c.WebhookLogRetentionDays != 8 || c.ShutdownDelaySeconds != 0 {
		t.Fatalf("retention defaults: %+v", c)
	}
	if !strings.HasPrefix(c.DatabaseURL, "sqlite:") || !strings.HasPrefix(c.BlobURL, "file://") || c.MailFrom != "no-reply@localhost" {
		t.Fatalf("storage defaults: %+v", c)
	}
	// The generated key is kept for the next start.
	key := string(c.SecretKey)
	c2, err := Load()
	if err != nil || string(c2.SecretKey) != key || c2.SecretFromEnv {
		t.Fatalf("persisted key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.DataDir, "secret.key")); err != nil {
		t.Fatal(err)
	}
}

// Every invalid value is reported at once, by variable name.
func TestAllProblemsReported(t *testing.T) {
	clearEnv(t)
	for k, v := range map[string]string{
		"PORT": "eighty", "OBX_WORKERS": "0", "OBX_TRUST_PROXY": "maybe", "OBX_LOG_FORMAT": "xml",
		"OBX_PUBLIC_URL": "images.maison-noor.example", "OBX_DATABASE_URL": "mysql://u:hunter2@db/obx",
		"OBX_BLOB_URL": "ftp://files", "OBX_SMTP_URL": "http://mail", "OBX_MAIL_FROM": "Portal <p@x.example>",
		"OBX_SECRET_KEY": "short", "OBX_JOB_RETENTION_DAYS": "7",
	} {
		t.Setenv(k, v)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("invalid configuration accepted")
	}
	msg := err.Error()
	for _, want := range []string{"PORT", "OBX_WORKERS", "OBX_TRUST_PROXY", "OBX_LOG_FORMAT", "OBX_PUBLIC_URL", "OBX_DATABASE_URL",
		"OBX_BLOB_URL", "OBX_SMTP_URL", "OBX_MAIL_FROM", "OBX_SECRET_KEY", "OBX_JOB_RETENTION_DAYS", "11 problem"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %s:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "hunter2") {
		t.Error("password from a URL in the error message")
	}
}

func TestPublicURLWithPathRefused(t *testing.T) {
	clearEnv(t)
	t.Setenv("OBX_PUBLIC_URL", "https://maison-noor.example/portal")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "without a path") {
		t.Fatalf("got %v", err)
	}
	t.Setenv("OBX_PUBLIC_URL", "https://images.maison-noor.example/")
	c, err := Load()
	if err != nil || c.PublicURL != "https://images.maison-noor.example" || !c.Secure() {
		t.Fatalf("trailing slash: %v %v", c, err)
	}
}

func TestWarnings(t *testing.T) {
	clearEnv(t)
	t.Setenv("OBX_PUBLC_URL", "https://typo.example")
	t.Setenv("OBX_PUBLIC_URL", "http://images.maison-noor.example")
	t.Setenv("OBX_METRICS", "true")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(c.Warnings(), "\n")
	for _, want := range []string{"OBX_PUBLC_URL is not a setting", "plain http", "/metrics is readable"} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings lack %q:\n%s", want, w)
		}
	}
}

func TestSecretKeyFromEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv("OBX_SECRET_KEY", strings.Repeat("k", 40))
	c, err := Load()
	if err != nil || !c.SecretFromEnv || string(c.SecretKey) != strings.Repeat("k", 40) {
		t.Fatalf("%v %v", c, err)
	}
	if _, err := os.Stat(filepath.Join(c.DataDir, "secret.key")); !os.IsNotExist(err) {
		t.Fatal("a key file was written although OBX_SECRET_KEY is set")
	}
}

// OBX_DOMAIN alone is enough for a public portal with built-in HTTPS.
func TestBuiltInHTTPS(t *testing.T) {
	clearEnv(t)
	t.Setenv("OBX_DOMAIN", "Portal.YourBrand.example.")
	t.Setenv("OBX_TLS", "auto")
	t.Setenv("OBX_ACME_EMAIL", "it@yourbrand.example")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://portal.yourbrand.example" || c.TLSHost() != "portal.yourbrand.example" || c.HTTPPort != 80 || c.HTTPSPort != 443 || !c.Secure() {
		t.Fatalf("built-in https: %+v", c)
	}
	if c.MailFrom != "no-reply@portal.yourbrand.example" {
		t.Fatalf("mail from: %s", c.MailFrom)
	}

	// The host of OBX_PUBLIC_URL is used when OBX_DOMAIN is not set.
	clearEnv(t)
	t.Setenv("OBX_PUBLIC_URL", "https://images.yourbrand.example/")
	t.Setenv("OBX_TLS", "AUTO")
	if c, err = Load(); err != nil || c.TLSHost() != "images.yourbrand.example" {
		t.Fatalf("public url host: %v %+v", err, c)
	}

	// Another HTTPS port ends up in the default public address.
	clearEnv(t)
	t.Setenv("OBX_DOMAIN", "portal.yourbrand.example")
	t.Setenv("OBX_TLS", "auto")
	t.Setenv("OBX_HTTPS_PORT", "8443")
	t.Setenv("OBX_HTTP_PORT", "8080")
	if c, err = Load(); err != nil || c.PublicURL != "https://portal.yourbrand.example:8443" {
		t.Fatalf("custom port: %v %+v", err, c)
	}

	// Off (the default): OBX_DOMAIN only sets the address, the node serves http on PORT.
	clearEnv(t)
	t.Setenv("OBX_DOMAIN", "portal.yourbrand.example")
	t.Setenv("OBX_TRUST_PROXY", "true")
	if c, err = Load(); err != nil || c.TLS != "off" || c.TLSHost() != "" || c.PublicURL != "https://portal.yourbrand.example" || c.Port != 8080 {
		t.Fatalf("behind a proxy: %v %+v", err, c)
	}
	for _, w := range c.Warnings() {
		if strings.Contains(w, "OBX_TRUST_PROXY") {
			t.Fatalf("unexpected warning: %s", w)
		}
	}
}

func TestBuiltInHTTPSProblems(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"OBX_TLS": "on"}, "OBX_TLS must be off or auto"},
		{map[string]string{"OBX_TLS": "auto"}, "needs an https://"},
		{map[string]string{"OBX_TLS": "auto", "OBX_PUBLIC_URL": "https://localhost"}, "public domain name"},
		{map[string]string{"OBX_TLS": "auto", "OBX_PUBLIC_URL": "https://203.0.113.10"}, "public domain name"},
		{map[string]string{"OBX_DOMAIN": "https://portal.yourbrand.example"}, "OBX_DOMAIN must be a host name"},
		{map[string]string{"OBX_DOMAIN": "portal.yourbrand.example:443"}, "OBX_DOMAIN must be a host name"},
		{map[string]string{"OBX_TLS": "auto", "OBX_DOMAIN": "a.yourbrand.example", "OBX_PUBLIC_URL": "https://b.yourbrand.example"}, "differ"},
		{map[string]string{"OBX_TLS": "auto", "OBX_DOMAIN": "a.yourbrand.example", "OBX_ACME_EMAIL": "IT <it@x.example>"}, "OBX_ACME_EMAIL"},
		{map[string]string{"OBX_TLS": "auto", "OBX_DOMAIN": "a.yourbrand.example", "OBX_ACME_DIRECTORY": "http://acme.example/dir"}, "OBX_ACME_DIRECTORY"},
		{map[string]string{"OBX_TLS": "auto", "OBX_DOMAIN": "a.yourbrand.example", "OBX_HTTP_PORT": "443"}, "must differ"},
		{map[string]string{"OBX_TLS": "auto", "OBX_PUBLIC_URL": "https://a.yourbrand.example:9443"}, "OBX_HTTPS_PORT is 443"},
		{map[string]string{"OBX_HTTPS_PORT": "0"}, "OBX_HTTPS_PORT must be between"},
	}
	for _, tc := range cases {
		clearEnv(t)
		for k, v := range tc.env {
			t.Setenv(k, v)
		}
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: want %q, got %v", tc.env, tc.want, err)
		}
	}
}

func TestTrustProxyWarningWithBuiltInHTTPS(t *testing.T) {
	clearEnv(t)
	t.Setenv("OBX_DOMAIN", "portal.yourbrand.example")
	t.Setenv("OBX_TLS", "auto")
	t.Setenv("OBX_TRUST_PROXY", "true")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range c.Warnings() {
		found = found || strings.Contains(w, "OBX_TRUST_PROXY")
	}
	if !found {
		t.Fatal("no warning for OBX_TRUST_PROXY with built-in https")
	}
}
