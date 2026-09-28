package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/openbrandexchange/node-go/internal/ids"
	"github.com/openbrandexchange/node-go/internal/problem"
)

// auditEntry is one row of the audit log: who changed what, when, with
// which outcome. It never stores request bodies, secrets or passwords, and
// no IP addresses. The JSON shape (GET /admin/api/audit-log) is the one the
// other OBX portals use.
type auditEntry struct {
	ActorType  string // user | anonymous | cli
	ActorID    string
	ActorName  string
	ActorEmail string
	ActorRole  string
	Action     string // e.g. retailers.update, session.login
	Method     string
	Route      string // route pattern, e.g. /admin/api/retailers/{id}
	TargetType string
	TargetID   string
	Status     int
	Details    map[string]any
}

func (e *auditEntry) actor(u *userRow) {
	if u == nil {
		return
	}
	e.ActorType, e.ActorID, e.ActorName, e.ActorEmail, e.ActorRole = "user", u.ID, u.Name, u.Email, u.Role
}

// audit writes an entry; failures are logged, never returned (the audit
// log must not break the action itself).
func (a *App) audit(ctx context.Context, e auditEntry) {
	if e.ActorType == "" {
		e.ActorType = "anonymous"
	}
	if e.Status == 0 {
		e.Status = 200
	}
	var details any
	if len(e.Details) > 0 {
		details = string(mustJSON(e.Details))
	}
	_, err := a.DB.Exec(ctx, `INSERT INTO audit_log (id, at, actor_type, actor_id, actor_name, actor_email, actor_role, action, method, route, target_type, target_id, status, request_id, details) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ids.New("aud"), a.nowStr(), e.ActorType, nullIfEmpty(e.ActorID), nullIfEmpty(e.ActorName), nullIfEmpty(e.ActorEmail), nullIfEmpty(e.ActorRole),
		e.Action, e.Method, e.Route, nullIfEmpty(e.TargetType), nullIfEmpty(e.TargetID), e.Status, nullIfEmpty(RequestID(ctx)), details)
	if err != nil {
		a.Log.ErrorContext(ctx, "audit log write failed", "action", e.Action, "error", err)
	}
}

// Audit records an action done on the command line.
func (a *App) Audit(ctx context.Context, action, targetType, targetID string) {
	a.audit(ctx, auditEntry{ActorType: "cli", Action: action, Method: "CLI", Route: "node-go " + action, TargetType: targetType, TargetID: targetID})
}

// notAudited are requests that change only the caller's own working state
// (retailers' browsing and downloads are download statistics), plus login,
// which is audited explicitly with its outcome.
var notAudited = regexp.MustCompile(`^/admin/api/(portal/(selection|download-jobs|gtin-lookup)|password/forgot|session/login)`)

var wildcardRe = regexp.MustCompile(`\{([a-z0-9_]+)\$?\}`)

// auditable reports whether an admin API request is recorded: every write,
// and reads that export personal data.
func auditable(r *http.Request) bool {
	_, p, _ := strings.Cut(r.Pattern, " ")
	if p == "" || notAudited.MatchString(p) {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return strings.HasSuffix(p, "/export")
	case http.MethodOptions:
		return false
	}
	return true
}

// auditAction turns a route pattern into a stable action name:
// "PATCH /admin/api/retailers/{id}" → "retailers.update",
// "POST /admin/api/access-requests/{id}/approve" → "access-requests.approve",
// "GET /admin/api/retailers/{id}/export" → "retailers.export".
func auditAction(method, pattern string) (action, targetType string) {
	p := strings.Trim(strings.TrimPrefix(pattern, "/admin/api"), "/")
	segs := strings.Split(p, "/")
	var plain []string
	for _, s := range segs {
		if !strings.HasPrefix(s, "{") {
			plain = append(plain, s)
		}
	}
	if len(plain) > 0 {
		targetType = plain[0]
		if targetType == "catalog" && len(plain) > 1 {
			targetType = plain[1]
		}
		targetType = strings.TrimSuffix(targetType, "s")
	}
	special := map[string]string{"session/login": "session.login", "session/logout": "session.logout", "setup": "setup.complete",
		"password/reset": "password.reset", "password/forgot": "password.forgot"}
	if s, ok := special[strings.Join(plain, "/")]; ok {
		return s, "user"
	}
	n := len(segs)
	if n >= 2 && !strings.HasPrefix(segs[n-1], "{") && strings.HasPrefix(segs[n-2], "{") {
		return strings.Join(append(plain[:len(plain)-1:len(plain)-1], segs[n-1]), "."), targetType
	}
	verb := map[string]string{"POST": "create", "PUT": "update", "PATCH": "update", "DELETE": "delete", "GET": "read", "HEAD": "read"}[method]
	if verb == "" {
		verb = strings.ToLower(method)
	}
	return strings.Join(append(plain, verb), "."), targetType
}

// auditTarget is the first path wildcard value; one-time tokens are never
// recorded.
func auditTarget(r *http.Request) string {
	for _, m := range wildcardRe.FindAllStringSubmatch(r.Pattern, -1) {
		if m[1] == "token" {
			continue
		}
		if v := r.PathValue(m[1]); v != "" {
			return v
		}
	}
	return ""
}

// auditRequest records a finished admin API request.
func (a *App) auditRequest(r *http.Request, s *session, status int) {
	_, p, _ := strings.Cut(r.Pattern, " ")
	action, targetType := auditAction(r.Method, p)
	e := auditEntry{Action: action, Method: r.Method, Route: p, TargetType: targetType, TargetID: auditTarget(r), Status: status}
	if s != nil {
		e.actor(s.User)
	}
	a.audit(r.Context(), e)
}

// handleAuditLog lists the audit log, newest first (admin only). Filters:
// action, actor_id, target_id, outcome=succeeded|failed.
func (a *App) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	limit, p := limitParam(r, 50, 200)
	if p != nil {
		problem.Write(w, p)
		return
	}
	q := r.URL.Query()
	where := []string{"1 = 1"}
	var args []any
	for _, f := range []string{"action", "actor_id", "target_id"} {
		if v := q.Get(f); v != "" {
			where = append(where, f+" = ?")
			args = append(args, v)
		}
	}
	switch q.Get("outcome") {
	case "failed":
		where = append(where, "status >= 400")
	case "succeeded":
		where = append(where, "status < 400")
	}
	if c := q.Get("cursor"); c != "" {
		var cur struct {
			At string `json:"at"`
			ID string `json:"id"`
		}
		if !a.decodeCursor("audit", c, &cur) {
			failP(w, "cursor_invalid", "The cursor is not valid.")
			return
		}
		where = append(where, "(at < ? OR (at = ? AND id < ?))")
		args = append(args, cur.At, cur.At, cur.ID)
	}
	items, err := a.auditRows(r.Context(), `WHERE `+strings.Join(where, " AND ")+` ORDER BY at DESC, id DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var next any
	if len(items) > limit {
		items = items[:limit]
		last := items[limit-1]
		next = a.encodeCursor("audit", map[string]string{"at": last["at"].(string), "id": last["id"].(string)})
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}

func (a *App) auditRows(ctx context.Context, tail string, args ...any) ([]map[string]any, error) {
	rs, err := a.DB.Query(ctx, `SELECT id, at, actor_type, actor_id, actor_name, actor_email, actor_role, action, method, route, target_type, target_id, status, request_id, details FROM audit_log `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	items := []map[string]any{}
	for rs.Next() {
		var id, at, actorType, action, method, route string
		var actorID, name, email, role, tt, tid, rid, details sql.NullString
		var status int
		if err := rs.Scan(&id, &at, &actorType, &actorID, &name, &email, &role, &action, &method, &route, &tt, &tid, &status, &rid, &details); err != nil {
			return nil, err
		}
		it := map[string]any{"id": id, "at": at,
			"actor":  map[string]any{"type": actorType, "id": nullStr(actorID), "name": nullStr(name), "role": nullStr(role), "email": nullStr(email)},
			"action": action, "method": method, "route": route, "target_type": nullStr(tt), "target_id": nullStr(tid),
			"status": status, "succeeded": status < 400, "request_id": nullStr(rid), "ip": nil, "details": nil}
		if details.Valid {
			var d any
			if json.Unmarshal([]byte(details.String), &d) == nil {
				it["details"] = d
			}
		}
		items = append(items, it)
	}
	return items, rs.Err()
}

// AuditLog returns the latest entries (command line).
func (a *App) AuditLog(ctx context.Context, limit int) ([]map[string]any, error) {
	return a.auditRows(ctx, `ORDER BY at DESC, id DESC LIMIT ?`, limit)
}
