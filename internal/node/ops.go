package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ---- request ids -------------------------------------------------------------------------

type requestIDKey struct{}

// validRequestID accepts ids set by a proxy or load balancer (for example
// Cloud Run's X-Cloud-Trace-Context or an upstream X-Request-Id).
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._:/=+-]{8,128}$`)

// RequestID returns the id of the request that ctx belongs to ("" outside
// a request).
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(requestIDKey{}).(string)
	return s
}

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// withRequestID takes a sane incoming X-Request-Id or creates one, echoes it
// in the response and puts it in the request context (logs carry it).
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// LogHandler wraps a slog handler so that every record logged with a request
// context carries request_id.
func LogHandler(h slog.Handler) slog.Handler { return ctxLogHandler{h} }

type ctxLogHandler struct{ slog.Handler }

func (h ctxLogHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h ctxLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ctxLogHandler{h.Handler.WithAttrs(attrs)}
}

func (h ctxLogHandler) WithGroup(name string) slog.Handler {
	return ctxLogHandler{h.Handler.WithGroup(name)}
}

// ---- access log ---------------------------------------------------------------------------

// quietPaths are probes that would drown the access log.
var quietPaths = map[string]bool{"/healthz": true, "/readyz": true, "/metrics": true}

// logRequests logs one line per request: method, route pattern, path (never
// the query string: it may hold signatures), status, bytes and duration.
// Client IPs are not logged (personal data); set OBX_LOG_LEVEL=debug to see
// probes too.
func (a *App) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		d := time.Since(start)
		route := routeLabel(r)
		a.metrics.observe(r.Method, route, rec.status, d)
		lvl := slog.LevelInfo
		switch {
		case rec.status >= 500:
			lvl = slog.LevelError
		case quietPaths[r.URL.Path]:
			lvl = slog.LevelDebug
		}
		if !a.Cfg.AccessLog && lvl < slog.LevelError {
			lvl = slog.LevelDebug
		}
		a.Log.Log(r.Context(), lvl, "http", "method", r.Method, "route", route, "path", r.URL.Path,
			"status", rec.status, "bytes", rec.bytes, "duration_ms", float64(d.Microseconds())/1000)
	})
}

// routeLabel is the matched ServeMux pattern without the method ("unmatched"
// for the UI and API catch-alls), so metrics have a bounded label set.
func routeLabel(r *http.Request) string {
	p := r.Pattern
	if i := strings.IndexByte(p, ' '); i >= 0 {
		p = p[i+1:]
	}
	switch p {
	case "":
		return "unmatched"
	case "/":
		return "ui"
	case "/obx/", "/admin/api/":
		return "unmatched"
	}
	return p
}

// ---- security headers ---------------------------------------------------------------------

// uiCSP is the policy admin-ui documents in docs/node-integration.md and runs
// its own Playwright tests under: no inline styles, nothing from other hosts.
// Images come from this node only (signed URLs point back here).
const uiCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

// apiCSP is sent with API responses: they are data, never documents.
const apiCSP = "default-src 'none'; frame-ancestors 'none'; sandbox"

// securityHeaders sets the headers every response carries (docs/security.md).
func (a *App) securityHeaders(h http.Header, isUI bool) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	if a.Cfg.Secure() {
		// Only when the public address is https: a node reached over plain
		// http (local try-out) must not pin browsers to https.
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
	if isUI {
		h.Set("Content-Security-Policy", uiCSP)
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
	} else {
		h.Set("Content-Security-Policy", apiCSP)
	}
}

// ---- health -------------------------------------------------------------------------------

// handleHealthz is the liveness probe: the process answers and the database
// connection works.
func (a *App) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := a.DB.SQL.PingContext(ctx); err != nil {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("database unavailable\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

// handleReadyz is the readiness probe: the node is not shutting down, the
// database answers a query on the current schema and the file store is
// reachable. Load balancers should send traffic only while it answers 200.
func (a *App) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	checks := map[string]string{"database": "ok", "files": "ok", "shutdown": "ok"}
	ready := true
	if a.draining.Load() {
		checks["shutdown"] = "shutting down"
		ready = false
	}
	var v int
	if err := a.DB.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		checks["database"] = "unavailable"
		ready = false
	}
	if _, err := a.Store.Exists(ctx, "readyz-probe"); err != nil {
		checks["files"] = "unavailable"
		ready = false
	}
	status := 200
	state := "ready"
	if !ready {
		status = 503
		state = "not_ready"
	}
	writeJSON(w, status, map[string]any{"status": state, "version": Version, "schema_version": v, "checks": checks})
}

// SetDraining marks the node as shutting down: /readyz answers 503 so load
// balancers stop sending new requests while running ones finish.
func (a *App) SetDraining() { a.draining.Store(true) }
