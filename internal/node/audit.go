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
// which outcome. It never stores request bodies, secrets or passwords.
type auditEntry struct {
	ActorType  string // user | cli | system
	ActorID    string
	ActorEmail string
	Action     string // e.g. retailers.update, session.login
	TargetType string
	TargetID   string
	Status     int
	Details    map[string]any
}

// audit writes an entry; failures are logged, never returned (the audit
// log must not break the action itself).
func (a *App) audit(ctx context.Context, e auditEntry) {
	if e.ActorType == "" {
		e.ActorType = "user"
	}
	var details any
	if len(e.Details) > 0 {
		details = string(mustJSON(e.Details))
	}
	var status any
	if e.Status != 0 {
		status = e.Status
	}
	_, err := a.DB.Exec(ctx, `INSERT INTO audit_log (id, at, actor_type, actor_id, actor_email, action, target_type, target_id, status, request_id, details) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ids.New("aud"), a.nowStr(), e.ActorType, nullIfEmpty(e.ActorID), nullIfEmpty(e.ActorEmail), e.Action,
		nullIfEmpty(e.TargetType), nullIfEmpty(e.TargetID), status, nullIfEmpty(RequestID(ctx)), details)
	if err != nil {
		a.Log.ErrorContext(ctx, "audit log write failed", "action", e.Action, "error", err)
	}
}

// Audit records an action done outside HTTP (command line).
func (a *App) Audit(ctx context.Context, action, targetType, targetID string) {
	a.audit(ctx, auditEntry{ActorType: "cli", Action: action, TargetType: targetType, TargetID: targetID})
}

// notAudited are writes that change only the caller's own working state.
var notAudited = map[string]bool{
	"/admin/api/portal/selection":     true,
	"/admin/api/portal/gtin-lookup":   true,
	"/admin/api/portal/download-jobs": true,
	"/admin/api/session/login":        true, // audited explicitly with the outcome
}

var wildcardRe = regexp.MustCompile(`\{([a-z0-9_]+)\$?\}`)

// auditable reports whether an admin API request is recorded.
func auditable(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return false
	}
	_, p, _ := strings.Cut(r.Pattern, " ")
	return p != "" && !notAudited[p]
}

// auditAction turns a route pattern into a stable action name:
// "PATCH /admin/api/retailers/{id}" → "retailers.update",
// "POST /admin/api/access-requests/{id}/approve" → "access-requests.approve".
func auditAction(method, pattern string) (action, targetType string) {
	p := strings.TrimPrefix(pattern, "/admin/api/")
	segs := strings.Split(p, "/")
	var nouns []string
	verb := ""
	for i, s := range segs {
		if strings.HasPrefix(s, "{") {
			continue
		}
		if i == len(segs)-1 && i > 0 && strings.HasPrefix(segs[i-1], "{") && method == http.MethodPost {
			verb = s
			continue
		}
		nouns = append(nouns, s)
	}
	switch p {
	case "session/logout":
		return "session.logout", "user"
	case "password/forgot":
		return "password.forgot", "user"
	case "password/reset":
		return "password.reset", "user"
	case "setup":
		return "setup.complete", "node"
	case "me":
		return "me.update", "user"
	}
	if verb == "" {
		switch method {
		case http.MethodPost:
			verb = "create"
		case http.MethodPut:
			verb = "set"
		case http.MethodPatch:
			verb = "update"
		case http.MethodDelete:
			verb = "delete"
		default:
			verb = strings.ToLower(method)
		}
	}
	if len(nouns) > 0 {
		targetType = nouns[0]
		if targetType == "catalog" && len(nouns) > 1 {
			targetType = nouns[1]
		}
		targetType = strings.TrimSuffix(targetType, "s")
	}
	return strings.Join(append(nouns, verb), "."), targetType
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

// auditRequest records a finished admin API write.
func (a *App) auditRequest(r *http.Request, s *session, status int) {
	_, p, _ := strings.Cut(r.Pattern, " ")
	action, targetType := auditAction(r.Method, p)
	e := auditEntry{Action: action, TargetType: targetType, TargetID: auditTarget(r), Status: status}
	if s != nil {
		e.ActorID, e.ActorEmail = s.User.ID, s.User.Email
	} else {
		e.ActorType = "anonymous"
	}
	a.audit(r.Context(), e)
}

// handleAuditLog lists the audit log, newest first (admin only).
func (a *App) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	limit, p := limitParam(r, 50, 500)
	if p != nil {
		problem.Write(w, p)
		return
	}
	var cur struct {
		At string `json:"at"`
		ID string `json:"id"`
	}
	where, args := "", []any{}
	if c := r.URL.Query().Get("cursor"); c != "" {
		if !a.decodeCursor("audit", c, &cur) {
			failP(w, "cursor_invalid", "The cursor is not valid.")
			return
		}
		where, args = `WHERE at < ? OR (at = ? AND id < ?)`, []any{cur.At, cur.At, cur.ID}
	}
	if actor := r.URL.Query().Get("actor_id"); actor != "" {
		if where == "" {
			where = "WHERE actor_id = ?"
		} else {
			where = "WHERE (" + strings.TrimPrefix(where, "WHERE ") + ") AND actor_id = ?"
		}
		args = append(args, actor)
	}
	items, err := a.auditRows(r.Context(), where+` ORDER BY at DESC, id DESC LIMIT ?`, append(args, limit+1)...)
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
	rs, err := a.DB.Query(ctx, `SELECT id, at, actor_type, actor_id, actor_email, action, target_type, target_id, status, request_id, details FROM audit_log `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	items := []map[string]any{}
	for rs.Next() {
		var id, at, actorType, action string
		var actorID, email, tt, tid, rid, details sql.NullString
		var status sql.NullInt64
		if err := rs.Scan(&id, &at, &actorType, &actorID, &email, &action, &tt, &tid, &status, &rid, &details); err != nil {
			return nil, err
		}
		it := map[string]any{"id": id, "at": at, "actor": map[string]any{"type": actorType, "id": nullStr(actorID), "email": nullStr(email)},
			"action": action, "target": map[string]any{"type": nullStr(tt), "id": nullStr(tid)}, "status": nil, "request_id": nullStr(rid), "details": nil}
		if status.Valid {
			it["status"] = status.Int64
		}
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
