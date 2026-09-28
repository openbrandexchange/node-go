// Package config reads the node's settings from the environment. Every
// setting has a sensible default so that `node-go serve` works with no
// configuration at all (SQLite and blobs under ./data).
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
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
	SetupCode           string // OBX_SETUP_CODE
	AllowPrivateURLs    bool   // OBX_ALLOW_PRIVATE_URLS
	SMTPURL             string // OBX_SMTP_URL smtp://user:pass@host:587 or smtps://...:465
	MailFrom            string // OBX_MAIL_FROM
	MailLogBodies       bool   // OBX_MAIL_LOG_BODIES (development only)
	Workers             int    // OBX_WORKERS
	TrustProxy          bool   // OBX_TRUST_PROXY: take the client IP from X-Forwarded-For
	LogFormat           string // OBX_LOG_FORMAT text|json
	LogLevel            string // OBX_LOG_LEVEL debug|info|warn|error
	CustomDomainHelp    string // OBX_CUSTOM_DOMAIN_HELP (Markdown shown in settings)
	TestRateLimit       int    // OBX_TEST_RATE_LIMIT: per-minute API limit override (conformance)
	ChangeRetentionDays int    // OBX_CHANGE_RETENTION_DAYS (>= 90)
}

// Load reads the configuration from the environment.
func Load() (*Config, error) {
	c := &Config{
		Port:                envInt("PORT", 8080),
		DataDir:             env("OBX_DATA_DIR", "./data"),
		SetupCode:           strings.TrimSpace(os.Getenv("OBX_SETUP_CODE")),
		SMTPURL:             strings.TrimSpace(os.Getenv("OBX_SMTP_URL")),
		MailFrom:            strings.TrimSpace(os.Getenv("OBX_MAIL_FROM")),
		Workers:             envInt("OBX_WORKERS", 2),
		LogFormat:           env("OBX_LOG_FORMAT", "text"),
		LogLevel:            env("OBX_LOG_LEVEL", "info"),
		CustomDomainHelp:    os.Getenv("OBX_CUSTOM_DOMAIN_HELP"),
		TestRateLimit:       envInt("OBX_TEST_RATE_LIMIT", 0),
		ChangeRetentionDays: envInt("OBX_CHANGE_RETENTION_DAYS", 400),
	}
	c.AllowPrivateURLs = envBool("OBX_ALLOW_PRIVATE_URLS")
	c.MailLogBodies = envBool("OBX_MAIL_LOG_BODIES")
	c.TrustProxy = envBool("OBX_TRUST_PROXY")
	if c.ChangeRetentionDays < 90 {
		c.ChangeRetentionDays = 90
	}
	if c.Workers < 1 {
		c.Workers = 1
	}
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return nil, err
	}
	c.DataDir = abs
	c.PublicURL = strings.TrimRight(env("OBX_PUBLIC_URL", platformURL(c.Port)), "/")
	u, err := url.Parse(c.PublicURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("OBX_PUBLIC_URL must be an absolute http(s) URL, got %q", c.PublicURL)
	}
	c.DatabaseURL = env("OBX_DATABASE_URL", env("DATABASE_URL", "sqlite:"+filepath.Join(c.DataDir, "obx.db")))
	c.BlobURL = env("OBX_BLOB_URL", "file://"+filepath.Join(c.DataDir, "blobs"))
	if c.MailFrom == "" {
		c.MailFrom = "no-reply@" + u.Hostname()
	}
	key, err := c.loadSecret()
	if err != nil {
		return nil, err
	}
	c.SecretKey = key
	return c, nil
}

// loadSecret returns OBX_SECRET_KEY or, when unset, a key persisted in the
// data directory (created on first start). Hosted setups should always set
// OBX_SECRET_KEY because a lost key makes stored credentials unreadable.
func (c *Config) loadSecret() ([]byte, error) {
	if s := strings.TrimSpace(os.Getenv("OBX_SECRET_KEY")); s != "" {
		if len(s) < 32 {
			return nil, errors.New("OBX_SECRET_KEY must be at least 32 characters (generate one with: openssl rand -base64 48)")
		}
		return []byte(s), nil
	}
	path := filepath.Join(c.DataDir, "secret.key")
	if b, err := os.ReadFile(path); err == nil {
		s := strings.TrimSpace(string(b))
		if len(s) >= 32 {
			return []byte(s), nil
		}
	}
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return nil, err
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

func envInt(k string, def int) int {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
