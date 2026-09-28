// Package config reads the node's settings from the environment. Every
// setting has a sensible default so that `node-go serve` works with no
// configuration at all (SQLite and blobs under ./data). Load validates every
// value and reports all problems at once, so a typo stops the node on start
// with a clear message instead of misbehaving later.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Config is the node configuration.
type Config struct {
	PublicURL           string // OBX_PUBLIC_URL, no trailing slash
	Port                int    // PORT
	DataDir             string // OBX_DATA_DIR
	DatabaseURL         string // OBX_DATABASE_URL (sqlite:/path or postgres://)
	BlobURL             string // OBX_BLOB_URL (file:///path, s3://bucket/prefix, gs://bucket/prefix)
	SecretKey           []byte // OBX_SECRET_KEY
	SecretFromEnv       bool   // the key came from OBX_SECRET_KEY (not the data directory)
	SetupCode           string // OBX_SETUP_CODE
	AllowPrivateURLs    bool   // OBX_ALLOW_PRIVATE_URLS
	SMTPURL             string // OBX_SMTP_URL smtp://user:pass@host:587 or smtps://...:465
	MailFrom            string // OBX_MAIL_FROM
	MailLogBodies       bool   // OBX_MAIL_LOG_BODIES (development only)
	Workers             int    // OBX_WORKERS
	TrustProxy          bool   // OBX_TRUST_PROXY: take the client IP from X-Forwarded-For
	LogFormat           string // OBX_LOG_FORMAT text|json
	LogLevel            string // OBX_LOG_LEVEL debug|info|warn|error
	AccessLog           bool   // OBX_ACCESS_LOG: one info line per request (default on)
	Metrics             bool   // OBX_METRICS: serve Prometheus metrics at /metrics
	MetricsToken        string // OBX_METRICS_TOKEN: bearer token required for /metrics
	CustomDomainHelp    string // OBX_CUSTOM_DOMAIN_HELP (Markdown shown in settings)
	TestRateLimit       int    // OBX_TEST_RATE_LIMIT: per-minute API limit override (conformance)
	ChangeRetentionDays int    // OBX_CHANGE_RETENTION_DAYS (>= 90)
	// Retention of operational records (days). Personal and usage data
	// (download statistics, audit log, access requests) follow the brand's
	// own retention settings in the admin.
	JobRetentionDays        int // OBX_JOB_RETENTION_DAYS: finished jobs, batch results, downloads
	WebhookLogRetentionDays int // OBX_WEBHOOK_LOG_RETENTION_DAYS: webhook delivery log
	ShutdownDelaySeconds    int // OBX_SHUTDOWN_DELAY: seconds /readyz reports 503 before the server stops
}

// Known lists every variable the node reads; others starting with OBX_ are
// reported as probable typos.
var Known = map[string]bool{
	"OBX_PUBLIC_URL": true, "OBX_DATA_DIR": true, "OBX_DATABASE_URL": true, "OBX_BLOB_URL": true,
	"OBX_SECRET_KEY": true, "OBX_SETUP_CODE": true, "OBX_ALLOW_PRIVATE_URLS": true, "OBX_SMTP_URL": true,
	"OBX_MAIL_FROM": true, "OBX_MAIL_LOG_BODIES": true, "OBX_WORKERS": true, "OBX_TRUST_PROXY": true,
	"OBX_LOG_FORMAT": true, "OBX_LOG_LEVEL": true, "OBX_ACCESS_LOG": true, "OBX_METRICS": true,
	"OBX_METRICS_TOKEN": true, "OBX_CUSTOM_DOMAIN_HELP": true, "OBX_TEST_RATE_LIMIT": true,
	"OBX_CHANGE_RETENTION_DAYS": true, "OBX_JOB_RETENTION_DAYS": true, "OBX_WEBHOOK_LOG_RETENTION_DAYS": true,
	"OBX_SHUTDOWN_DELAY": true, "OBX_S3_ACCESS_KEY_ID": true, "OBX_S3_SECRET_ACCESS_KEY": true,
	// Read by the command-line tools and tests, not by the server.
	"OBX_URL": true, "OBX_TOKEN": true, "OBX_TEST_DATABASE_URL": true, "OBX_TEST_LOG": true,
	"OBX_TEST_S3_ENDPOINT": true, "OBX_TEST_S3_BLOBS": true, "OBX_TEST_SFTP": true, "OBX_LOAD_TEST": true,
	"OBX_LOAD_PRODUCTS": true, "OBX_ALLOW_PRIVATE": true,
}

// problems collects validation errors.
type problems []string

func (p *problems) add(format string, args ...any) { *p = append(*p, fmt.Sprintf(format, args...)) }

func (c *Config) intVar(p *problems, k string, def, minv, maxv int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		p.add("%s must be a whole number, got %q", k, v)
		return def
	}
	if n < minv || n > maxv {
		p.add("%s must be between %d and %d, got %d", k, minv, maxv, n)
		return def
	}
	return n
}

func boolVar(p *problems, k string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	switch v {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	p.add("%s must be true or false, got %q", k, os.Getenv(k))
	return def
}

// Load reads and validates the configuration from the environment.
func Load() (*Config, error) {
	var p problems
	c := &Config{
		DataDir:          env("OBX_DATA_DIR", "./data"),
		SetupCode:        strings.TrimSpace(os.Getenv("OBX_SETUP_CODE")),
		SMTPURL:          strings.TrimSpace(os.Getenv("OBX_SMTP_URL")),
		MailFrom:         strings.TrimSpace(os.Getenv("OBX_MAIL_FROM")),
		LogFormat:        strings.ToLower(env("OBX_LOG_FORMAT", "text")),
		LogLevel:         strings.ToLower(env("OBX_LOG_LEVEL", "info")),
		CustomDomainHelp: os.Getenv("OBX_CUSTOM_DOMAIN_HELP"),
		MetricsToken:     strings.TrimSpace(os.Getenv("OBX_METRICS_TOKEN")),
	}
	c.Port = c.intVar(&p, "PORT", 8080, 1, 65535)
	c.Workers = c.intVar(&p, "OBX_WORKERS", 2, 1, 64)
	c.TestRateLimit = c.intVar(&p, "OBX_TEST_RATE_LIMIT", 0, 0, 1000000)
	c.ChangeRetentionDays = c.intVar(&p, "OBX_CHANGE_RETENTION_DAYS", 400, 90, 36500)
	c.JobRetentionDays = c.intVar(&p, "OBX_JOB_RETENTION_DAYS", 30, 8, 36500)
	c.WebhookLogRetentionDays = c.intVar(&p, "OBX_WEBHOOK_LOG_RETENTION_DAYS", 8, 1, 365)
	c.ShutdownDelaySeconds = c.intVar(&p, "OBX_SHUTDOWN_DELAY", 0, 0, 60)
	c.AllowPrivateURLs = boolVar(&p, "OBX_ALLOW_PRIVATE_URLS", false)
	c.MailLogBodies = boolVar(&p, "OBX_MAIL_LOG_BODIES", false)
	c.TrustProxy = boolVar(&p, "OBX_TRUST_PROXY", false)
	c.AccessLog = boolVar(&p, "OBX_ACCESS_LOG", true)
	c.Metrics = boolVar(&p, "OBX_METRICS", c.MetricsToken != "")
	if c.LogFormat != "text" && c.LogFormat != "json" {
		p.add("OBX_LOG_FORMAT must be text or json, got %q", c.LogFormat)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		p.add("OBX_LOG_LEVEL must be debug, info, warn or error, got %q", c.LogLevel)
	}
	if c.MetricsToken != "" && len(c.MetricsToken) < 16 {
		p.add("OBX_METRICS_TOKEN must be at least 16 characters")
	}
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		p.add("OBX_DATA_DIR %q is not a usable path: %v", c.DataDir, err)
	}
	c.DataDir = abs
	c.PublicURL = strings.TrimRight(env("OBX_PUBLIC_URL", platformURL(c.Port)), "/")
	u, err := url.Parse(c.PublicURL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		p.add("OBX_PUBLIC_URL must be an absolute http(s) address such as https://images.yourbrand.com, got %q", c.PublicURL)
		u = &url.URL{Scheme: "http", Host: "localhost"}
	case u.Path != "" || u.RawQuery != "" || u.Fragment != "":
		p.add("OBX_PUBLIC_URL must be the bare address without a path (the portal runs at the root of its own domain), got %q", c.PublicURL)
	}
	c.DatabaseURL = env("OBX_DATABASE_URL", env("DATABASE_URL", "sqlite:"+filepath.Join(c.DataDir, "obx.db")))
	if !strings.HasPrefix(c.DatabaseURL, "sqlite:") && !strings.HasPrefix(c.DatabaseURL, "postgres://") && !strings.HasPrefix(c.DatabaseURL, "postgresql://") {
		p.add("OBX_DATABASE_URL must start with sqlite: or postgres:// (got %q)", redact(c.DatabaseURL))
	}
	c.BlobURL = env("OBX_BLOB_URL", "file://"+filepath.Join(c.DataDir, "blobs"))
	if bu, err := url.Parse(c.BlobURL); err != nil {
		p.add("OBX_BLOB_URL is not a valid address")
	} else {
		switch bu.Scheme {
		case "file", "s3", "gs", "r2":
			if bu.Scheme != "file" && bu.Host == "" {
				p.add("OBX_BLOB_URL needs a bucket name: %s://bucket/prefix", bu.Scheme)
			}
		default:
			p.add("OBX_BLOB_URL must start with file://, s3://, r2:// or gs:// (got %q)", redact(c.BlobURL))
		}
	}
	if c.SMTPURL != "" {
		su, err := url.Parse(c.SMTPURL)
		if err != nil || (su.Scheme != "smtp" && su.Scheme != "smtps") || su.Hostname() == "" {
			p.add("OBX_SMTP_URL must look like smtp://user:password@host:587 or smtps://user:password@host:465 (got %q)", redact(c.SMTPURL))
		} else if port := su.Port(); port != "" {
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				p.add("OBX_SMTP_URL has an invalid port %q", port)
			}
		}
	}
	if c.MailFrom == "" {
		c.MailFrom = "no-reply@" + u.Hostname()
	} else if a, err := mail.ParseAddress(c.MailFrom); err != nil || a.Name != "" {
		p.add("OBX_MAIL_FROM must be a plain e-mail address such as portal@yourbrand.com, got %q", c.MailFrom)
	}
	if s := strings.TrimSpace(os.Getenv("OBX_SECRET_KEY")); s != "" && len(s) < 32 {
		p.add("OBX_SECRET_KEY must be at least 32 characters (generate one with: openssl rand -base64 48)")
	}
	if len(p) > 0 {
		return nil, fmt.Errorf("the configuration has %d problem(s):\n  - %s\nSee docs/configuration.md", len(p), strings.Join(p, "\n  - "))
	}
	key, err := c.loadSecret()
	if err != nil {
		return nil, err
	}
	c.SecretKey = key
	return c, nil
}

// ApplyDefaults fills settings left at zero (a Config built in code rather
// than by Load) with the defaults Load uses.
func (c *Config) ApplyDefaults() {
	def := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	def(&c.Workers, 2)
	def(&c.ChangeRetentionDays, 400)
	def(&c.JobRetentionDays, 30)
	def(&c.WebhookLogRetentionDays, 8)
}

// Warnings returns advice about a valid but risky configuration.
func (c *Config) Warnings() []string {
	var w []string
	var unknown []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "OBX_") && !Known[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		w = append(w, fmt.Sprintf("%s is not a setting of this node (typo?); it is ignored", k))
	}
	u, _ := url.Parse(c.PublicURL)
	if u != nil && u.Scheme == "http" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" {
		w = append(w, "OBX_PUBLIC_URL uses plain http: passwords and download links travel unencrypted. Put the node behind https for real use.")
	}
	if c.AllowPrivateURLs {
		w = append(w, "OBX_ALLOW_PRIVATE_URLS is on: imports and webhooks may reach private networks. Use it for local development only.")
	}
	if c.MailLogBodies {
		w = append(w, "OBX_MAIL_LOG_BODIES is on: one-time links appear in the log. Use it for local development only.")
	}
	if c.Metrics && c.MetricsToken == "" {
		w = append(w, "OBX_METRICS is on without OBX_METRICS_TOKEN: /metrics is readable by anyone who can reach the node.")
	}
	return w
}

// redact removes a password from a URL for error messages.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	return u.String()
}

// loadSecret returns OBX_SECRET_KEY or, when unset, a key persisted in the
// data directory (created on first start). Hosted setups should always set
// OBX_SECRET_KEY because a lost key makes stored credentials unreadable.
func (c *Config) loadSecret() ([]byte, error) {
	if s := strings.TrimSpace(os.Getenv("OBX_SECRET_KEY")); s != "" {
		c.SecretFromEnv = true
		return []byte(s), nil
	}
	path := filepath.Join(c.DataDir, "secret.key")
	if b, err := os.ReadFile(path); err == nil {
		s := strings.TrimSpace(string(b))
		if len(s) >= 32 {
			return []byte(s), nil
		}
		return nil, fmt.Errorf("%s holds a key shorter than 32 characters; restore it from your backup or set OBX_SECRET_KEY", path)
	}
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("OBX_DATA_DIR %s cannot be created: %w", c.DataDir, err)
	}
	raw := make([]byte, 48)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	s := base64.RawURLEncoding.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(s+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("OBX_SECRET_KEY is not set and %s cannot be written: %w", path, err)
	}
	slog.Warn("OBX_SECRET_KEY is not set; generated one and stored it in the data directory. Back it up or set OBX_SECRET_KEY.", "path", path)
	return []byte(s), nil
}

// Secure reports whether cookies must carry the Secure flag.
func (c *Config) Secure() bool { return strings.HasPrefix(c.PublicURL, "https://") }

// Host returns the host of the public URL.
func (c *Config) Host() string {
	u, _ := url.Parse(c.PublicURL)
	return u.Host
}

// platformURL guesses the public URL on hosts that announce it.
func platformURL(port int) string {
	if u := os.Getenv("RENDER_EXTERNAL_URL"); u != "" {
		return u
	}
	if d := os.Getenv("RAILWAY_PUBLIC_DOMAIN"); d != "" {
		return "https://" + d
	}
	if a := os.Getenv("FLY_APP_NAME"); a != "" {
		return "https://" + a + ".fly.dev"
	}
	return fmt.Sprintf("http://localhost:%d", port)
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
