package node

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/openbrandexchange/node-go/internal/problem"
)

var logWriter io.Writer = os.Stderr

// maxJSONBody is the default request body limit for JSON endpoints.
const maxJSONBody = 4 << 20

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// fail writes err as problem+json (internal errors are logged, not leaked).
func (a *App) fail(w http.ResponseWriter, r *http.Request, err error) {
	var p *problem.Problem
	if !errors.As(err, &p) {
		a.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		p = problem.New("internal_error", "An unexpected error occurred.")
	}
	problem.Write(w, p)
}

func failP(w http.ResponseWriter, code, format string, args ...any) {
	problem.Write(w, problem.New(code, format, args...))
}

// readJSON decodes a JSON body (numbers as json.Number) into v.
func readJSON(r *http.Request, v any, limit int64) *problem.Problem {
	if limit == 0 {
		limit = maxJSONBody
	}
	ct := r.Header.Get("Content-Type")
	if ct != "" {
		mt, _, _ := mime.ParseMediaType(ct)
		if mt != "application/json" && mt != "application/merge-patch+json" && !strings.HasSuffix(mt, "+json") {
			return problem.New("unsupported_media_type", "Send application/json.")
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return problem.New("invalid_request", "The request body could not be read.")
	}
	if int64(len(body)) > limit {
		return problem.New("payload_too_large", "The request body is too large.")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return problem.New("invalid_request", "A JSON body is required.")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return problem.New("invalid_request", "The body is not valid JSON: %s", err.Error())
	}
	if dec.More() {
		return problem.New("invalid_request", "The body contains more than one JSON value.")
	}
	return nil
}

// readJSONObject reads a JSON object as a generic map.
func readJSONObject(r *http.Request) (map[string]any, *problem.Problem) {
	var v any
	if p := readJSON(r, &v, 0); p != nil {
		return nil, p
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, problem.New("invalid_request", "The body must be a JSON object.")
	}
	return m, nil
}

// readOptionalJSON reads a JSON object when a body was sent.
func readOptionalJSON(r *http.Request, v any) *problem.Problem {
	if r.ContentLength == 0 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
	if err != nil {
		return problem.New("invalid_request", "The request body could not be read.")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return problem.New("invalid_request", "The body is not valid JSON.")
	}
	return nil
}

// clientIP returns the caller's IP (X-Forwarded-For when trusted).
func (a *App) clientIP(r *http.Request) string {
	if a.Cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---- opaque signed cursors -------------------------------------------------

// encodeCursor signs v into an opaque cursor bound to this node.
func (a *App) encodeCursor(kind string, v any) string {
	payload := base64.RawURLEncoding.EncodeToString(mustJSON(v))
	sig := a.Keys.Sign("cursor/"+kind+"/"+a.nodeID, payload)[:32]
	return payload + "." + sig
}

// decodeCursor verifies and decodes a cursor; false when malformed, from
// another node or tampered with.
func (a *App) decodeCursor(kind, c string, v any) bool {
	i := strings.LastIndexByte(c, '.')
	if i <= 0 || len(c) > 4096 {
		return false
	}
	payload, sig := c[:i], c[i+1:]
	want := a.Keys.Sign("cursor/"+kind+"/"+a.nodeID, payload)[:32]
	if len(sig) != len(want) || !constantEq(sig, want) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, v) == nil
}

func constantEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var d byte
	for i := 0; i < len(a); i++ {
		d |= a[i] ^ b[i]
	}
	return d == 0
}

// limitParam parses ?limit= with default and maximum.
func limitParam(r *http.Request, def, maxv int) (int, *problem.Problem) {
	s := r.URL.Query().Get("limit")
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxv {
		return 0, problem.New("invalid_request", "limit must be between 1 and %d.", maxv).Field("", "invalid_request", "limit")
	}
	return n, nil
}

// sanitizeFilename keeps a safe file name for Content-Disposition and zips.
func sanitizeFilename(s string) string {
	s = strings.ReplaceAll(s, "\\", "/")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f || r == '"' || r == ':' || r == '*' || r == '?' || r == '<' || r == '>' || r == '|':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimLeft(out, ".")
	if out == "" {
		out = "file"
	}
	return out
}

// contentDisposition builds an RFC 6266 header value.
func contentDisposition(kind, name string) string {
	name = sanitizeFilename(name)
	ascii := true
	for _, r := range name {
		if r > 0x7e {
			ascii = false
		}
	}
	if ascii {
		return kind + `; filename="` + name + `"`
	}
	fallback := strings.Map(func(r rune) rune {
		if r > 0x7e {
			return '_'
		}
		return r
	}, name)
	return kind + `; filename="` + fallback + `"; filename*=UTF-8''` + urlPathEscape(name)
}

func urlPathEscape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

// statusRecorder captures the response status for access logs.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// logRequests logs method, path (never the query string: it may hold
// signatures) and status.
func (a *App) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		lvl := slog.LevelDebug
		if rec.status >= 500 {
			lvl = slog.LevelError
		}
		a.Log.Log(r.Context(), lvl, "http", "method", r.Method, "path", r.URL.Path, "status", rec.status)
	})
}

func errorsAs(err error, p **problem.Problem) bool { return errors.As(err, p) }

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }
