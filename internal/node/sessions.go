package node

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/problem"
	"github.com/openbrandexchange/node-go/internal/secure"
)

const (
	sessionCookie = "obx_session"
	csrfCookie    = "obx_csrf"
	sessionTTL    = 30 * 24 * time.Hour
)

type userRow struct {
	ID, Email, Name, Role, Status, Language, CreatedAt  string
	RetailerID, PasswordHash, InviteHash, InviteExpires *string
	ResetHash, ResetExpires, LastLogin                  *string
	Selection, SelectionUpdated                         *string
}

func (a *App) loadUser(ctx context.Context, q db.Querier, where string, args ...any) (*userRow, error) {
	u := &userRow{}
	err := q.QueryRow(ctx, `SELECT id, email, name, role, status, language, created_at, retailer_id, password_hash, invite_hash, invite_expires_at, reset_hash, reset_expires_at, last_login_at, selection, selection_updated_at FROM users WHERE `+where, args...).
		Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.Status, &u.Language, &u.CreatedAt, &u.RetailerID, &u.PasswordHash, &u.InviteHash, &u.InviteExpires, &u.ResetHash, &u.ResetExpires, &u.LastLogin, &u.Selection, &u.SelectionUpdated)
	if db.NotFound(err) {
		return nil, nil
	}
	return u, err
}

// userJSON is the admin User schema.
func userJSON(u *userRow) map[string]any {
	return map[string]any{
		"id": u.ID, "email": u.Email, "name": u.Name, "role": u.Role, "retailer_id": u.RetailerID,
		"status": u.Status, "language": u.Language, "last_login_at": u.LastLogin,
		"invite_expires_at": u.InviteExpires, "created_at": u.CreatedAt,
	}
}

// session is the authenticated admin/portal session.
type session struct {
	IDHash string
	CSRF   string
	User   *userRow
}

type ctxKey int

const sessionKey ctxKey = 1

func (a *App) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: a.Cfg.Secure(),
		SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}

// loadSession returns the session of the request (nil when not logged in).
func (a *App) loadSession(r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	ctx := r.Context()
	h := secure.HashToken(c.Value)
	var s session
	var userID, expires, lastSeen string
	err = a.DB.QueryRow(ctx, `SELECT id_hash, csrf, user_id, expires_at, last_seen_at FROM sessions WHERE id_hash = ?`, h).Scan(&s.IDHash, &s.CSRF, &userID, &expires, &lastSeen)
	if err != nil {
		return nil
	}
	if db.Parse(expires).Before(a.now()) {
		_, _ = a.DB.Exec(ctx, `DELETE FROM sessions WHERE id_hash = ?`, h)
		return nil
	}
	u, err := a.loadUser(ctx, a.DB, "id = ?", userID)
	if err != nil || u == nil || u.Status != "active" {
		return nil
	}
	if u.Role == "retailer" && u.RetailerID != nil {
		var st string
		if a.DB.QueryRow(ctx, `SELECT status FROM retailers WHERE id = ?`, *u.RetailerID).Scan(&st) != nil || st != "active" {
			return nil
		}
	}
	s.User = u
	// Sliding expiry, written at most every 10 minutes.
	if a.now().Sub(db.Parse(lastSeen)) > 10*time.Minute {
		_, _ = a.DB.Exec(ctx, `UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id_hash = ?`, a.nowStr(), db.Fmt(a.now().Add(sessionTTL)), h)
	}
	return &s
}

// startSession creates a session for u (rotating any previous one) and
// returns the new CSRF token.
func (a *App) startSession(ctx context.Context, w http.ResponseWriter, r *http.Request, u *userRow) (string, error) {
	if old, err := r.Cookie(sessionCookie); err == nil {
		_, _ = a.DB.Exec(ctx, `DELETE FROM sessions WHERE id_hash = ?`, secure.HashToken(old.Value))
	}
	id := secure.RandomString(32)
	csrf := secure.RandomString(24)
	now := a.nowStr()
	if _, err := a.DB.Exec(ctx, `INSERT INTO sessions (id_hash, user_id, csrf, created_at, expires_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?)`,
		secure.HashToken(id), u.ID, csrf, now, db.Fmt(a.now().Add(sessionTTL)), now); err != nil {
		return "", err
	}
	_, _ = a.DB.Exec(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, now, u.ID)
	a.setCookie(w, sessionCookie, id, int(sessionTTL.Seconds()))
	a.setCookie(w, csrfCookie, csrf, int(sessionTTL.Seconds()))
	return csrf, nil
}

// endSessions removes all sessions of a user (optionally keeping one).
func (a *App) endSessions(ctx context.Context, q db.Querier, userID, keepHash string) {
	_, _ = q.Exec(ctx, `DELETE FROM sessions WHERE user_id = ? AND id_hash <> ?`, userID, keepHash)
}

// checkCSRF enforces the double-submit CSRF token on state-changing
// requests: header must equal the obx_csrf cookie and, with a session, the
// session's token.
func (a *App) checkCSRF(r *http.Request, s *session) *problem.Problem {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	h := r.Header.Get("X-CSRF-Token")
	if h == "" {
		return problem.New("csrf_failed", "Missing X-CSRF-Token header; reload the page.")
	}
	if s != nil {
		if !secure.Equal(h, s.CSRF) {
			return problem.New("csrf_failed", "The security token is wrong; reload the page.")
		}
		return nil
	}
	c, err := r.Cookie(csrfCookie)
	if err != nil || !secure.Equal(h, c.Value) {
		return problem.New("csrf_failed", "The security token is wrong; reload the page.")
	}
	return nil
}

func sessionFrom(ctx context.Context) *session {
	s, _ := ctx.Value(sessionKey).(*session)
	return s
}

// adminAPI wraps admin handlers: setup gate, session, role and CSRF.
// role is "admin", "any" (logged in) or "public".
func (a *App) adminAPI(role string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ctx := r.Context()
		if role != "setup" && !a.SetupDone(ctx) {
			failP(w, "setup_required", "Complete the first-run setup first.")
			return
		}
		s := a.loadSession(r)
		if bearer(r) != "" && s == nil && role != "public" && role != "setup" {
			failP(w, "unauthorized", "The admin API accepts only session cookies.")
			return
		}
		if p := a.checkCSRF(r, s); p != nil {
			problem.Write(w, p)
			return
		}
		switch role {
		case "admin", "any":
			if s == nil {
				failP(w, "unauthorized", "Please log in.")
				return
			}
			if role == "admin" && s.User.Role != "admin" {
				failP(w, "forbidden", "Only brand administrators can do this.")
				return
			}
		}
		if s != nil {
			ctx = context.WithValue(ctx, sessionKey, s)
		}
		r = r.WithContext(ctx)
		if auditable(r) {
			rec := &statusRecorder{ResponseWriter: w, status: 200}
			h(rec, r)
			a.auditRequest(r, s, rec.status)
			return
		}
		h(w, r)
	}
}

// sessionJSON is the admin Session schema.
func (a *App) sessionJSON(ctx context.Context, s *session, csrf string) map[string]any {
	out := map[string]any{"authenticated": s != nil, "csrf_token": csrf, "setup_required": !a.SetupDone(ctx), "user": nil, "retailer": nil}
	if s != nil {
		out["user"] = userJSON(s.User)
		if s.User.RetailerID != nil {
			if ret, _ := a.loadRetailer(ctx, a.DB, *s.User.RetailerID); ret != nil {
				out["retailer"] = map[string]any{"id": ret.ID, "name": ret.Name}
			}
		}
	}
	return out
}

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }
