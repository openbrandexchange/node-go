// Package tlsauto gives the node its own HTTPS (OBX_TLS=auto): a Let's
// Encrypt certificate for the portal's domain, obtained and renewed
// automatically with the ACME http-01 and tls-alpn-01 challenges, and an
// http listener that answers the challenges and redirects everything else to
// https. Certificates and the ACME account key are cached in
// $OBX_DATA_DIR/acme, so restarts do not ask Let's Encrypt again.
package tlsauto

import (
	"crypto/tls"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/openbrandexchange/node-go/internal/config"
	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// CacheDir is where certificates are kept for cfg.
func CacheDir(cfg *config.Config) string { return filepath.Join(cfg.DataDir, "acme") }

// Manager returns the certificate manager for cfg. It only ever asks for a
// certificate for cfg.TLSHost(): requests for other names (scanners probing
// the server's IP address) are refused without contacting Let's Encrypt.
func Manager(cfg *config.Config) *autocert.Manager {
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(CacheDir(cfg)),
		HostPolicy: autocert.HostWhitelist(cfg.TLSHost()),
		Email:      cfg.ACMEEmail,
	}
	if cfg.ACMEDirectory != "" {
		m.Client = &acme.Client{DirectoryURL: cfg.ACMEDirectory}
	}
	return m
}

// TLSConfig is the server configuration: the manager's certificates and the
// acme-tls/1 protocol, TLS 1.2 or newer.
func TLSConfig(m *autocert.Manager) *tls.Config {
	c := m.TLSConfig()
	c.MinVersion = tls.VersionTLS12
	return c
}

// HTTPHandler serves the plain-http port: ACME http-01 challenges, the
// health checks (so `node-go healthcheck` and monitors work without a
// certificate) and a permanent redirect to https for everything else.
func HTTPHandler(m *autocert.Manager, cfg *config.Config, health http.Handler) http.Handler {
	host := cfg.TLSHost()
	if cfg.HTTPSPort != 443 {
		host += ":" + strconv.Itoa(cfg.HTTPSPort)
	}
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/readyz":
			health.ServeHTTP(w, r)
			return
		}
		code := http.StatusPermanentRedirect // keeps the method and body
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			code = http.StatusMovedPermanently
		}
		w.Header().Set("Connection", "close")
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), code)
	})
	return m.HTTPHandler(fallback)
}
