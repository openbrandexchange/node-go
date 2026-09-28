package node

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/problem"
	"github.com/openbrandexchange/node-go/internal/secure"
)

// AllScopes are the protocol scopes.
var AllScopes = []string{"catalog:read", "media:read", "media:original", "usage:write"}

const accessTokenTTL = 3600

type accessClaims struct {
	C string `json:"c"` // api client row id
	V int    `json:"v"` // secret version
	S string `json:"s"` // scopes
	E int64  `json:"e"` // expiry (unix)
}

func (a *App) issueAccessToken(clientRowID string, version int, scopes []string) string {
	payload := base64.RawURLEncoding.EncodeToString(mustJSON(accessClaims{C: clientRowID, V: version, S: strings.Join(scopes, " "), E: a.now().Unix() + accessTokenTTL}))
	return "obxa_" + payload + "." + a.Keys.Sign("access-token", payload)
}

type apiClientRow struct {
	ID, ClientID, SecretHash, RetailerID, Name, Status, CreatedBy, CreatedAt string
	Version                                                                  int
	Scopes                                                                   []string
	LastUsed                                                                 *string
}

func (a *App) loadClient(ctx context.Context, q db.Querier, where string, arg any) (*apiClientRow, error) {
	c := &apiClientRow{}
	var scopes string
	err := q.QueryRow(ctx, `SELECT id, client_id, secret_hash, retailer_id, name, status, created_by, created_at, secret_version, scopes, last_used_at FROM api_clients WHERE `+where, arg).
		Scan(&c.ID, &c.ClientID, &c.SecretHash, &c.RetailerID, &c.Name, &c.Status, &c.CreatedBy, &c.CreatedAt, &c.Version, &scopes, &c.LastUsed)
	if db.NotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Scopes = strings.Fields(scopes)
	return c, nil
}

// effectiveScopes intersects the client's scopes with the retailer's grant.
func (a *App) effectiveScopes(ctx context.Context, c *apiClientRow, r *retailerRow) []string {
	g := a.grantOf(ctx, r)
	allowed := map[string]bool{}
	for _, s := range g.Scopes {
		allowed[s] = true
	}
	var out []string
	for _, s := range c.Scopes {
		if allowed[s] {
			out = append(out, s)
		}
	}
	return out
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="obx"`)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

// handleToken is the OAuth2 client-credentials token endpoint.
func (a *App) handleToken(w http.ResponseWriter, r *http.Request) {
	if !a.limit(w, "token:"+a.clientIP(r), 30, time.Minute) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, 400, "invalid_request", "The body must be application/x-www-form-urlencoded.")
		return
	}
	if r.PostForm.Get("grant_type") != "client_credentials" {
		oauthError(w, 400, "unsupported_grant_type", "Only client_credentials is supported.")
		return
	}
	id, secret, ok := r.BasicAuth()
	if ok {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id == "" || secret == "" {
		oauthError(w, 401, "invalid_client", "Client authentication failed.")
		return
	}
	ctx := r.Context()
	c, err := a.loadClient(ctx, a.DB, "client_id = ?", id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if c == nil || !secure.Equal(c.SecretHash, secure.HashToken(secret)) || c.Status != "active" {
		oauthError(w, 401, "invalid_client", "Client authentication failed.")
		return
	}
	ret, err := a.loadRetailer(ctx, a.DB, c.RetailerID)
	if err != nil || ret == nil || ret.Status != "active" {
		oauthError(w, 401, "invalid_client", "Client authentication failed.")
		return
	}
	eff := a.effectiveScopes(ctx, c, ret)
	scopes := eff
	if s := strings.TrimSpace(r.PostForm.Get("scope")); s != "" {
		have := map[string]bool{}
		for _, x := range eff {
			have[x] = true
		}
		scopes = nil
		for _, x := range strings.Fields(s) {
			if !have[x] {
				oauthError(w, 400, "invalid_scope", "Scope "+x+" is not available to this client.")
				return
			}
			scopes = append(scopes, x)
		}
	}
	sort.Strings(scopes)
	_, _ = a.DB.Exec(ctx, `UPDATE api_clients SET last_used_at = ? WHERE id = ?`, a.nowStr(), c.ID)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{
		"access_token": a.issueAccessToken(c.ID, c.Version, scopes),
		"token_type":   "Bearer",
		"expires_in":   accessTokenTTL,
		"scope":        strings.Join(scopes, " "),
	})
}

// bearer extracts a Bearer token.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// viewerForClient builds the viewer of an API client (nil + problem when
// revoked).
func (a *App) viewerForClient(ctx context.Context, c *apiClientRow, tokenScopes []string) (*Viewer, *problem.Problem) {
	if c == nil || c.Status != "active" {
		return nil, problem.New("access_revoked", "This API key was revoked.")
	}
	ret, err := a.loadRetailer(ctx, a.DB, c.RetailerID)
	if err != nil {
		return nil, problem.As(err)
	}
	if ret == nil || ret.Status != "active" {
		return nil, problem.New("access_revoked", "Access for this retailer was revoked.")
	}
	eff := map[string]bool{}
	for _, s := range a.effectiveScopes(ctx, c, ret) {
		eff[s] = true
	}
	scopes := map[string]bool{}
	for _, s := range tokenScopes {
		if eff[s] {
			scopes[s] = true
		}
	}
	return &Viewer{Kind: "client", RetailerID: ret.ID, RetailerName: ret.Name, BrandIDs: ret.BrandIDs,
		OriginalAllowed: scopes["media:original"] && a.originalAllowed(ctx, ret), Scopes: scopes,
		ClientRowID: c.ID, ClientName: c.Name}, nil
}

// protocolAuth authenticates a protocol call and checks scope.
func (a *App) protocolAuth(w http.ResponseWriter, r *http.Request, scope string) (*Viewer, bool) {
	tok := bearer(r)
	if tok == "" {
		failP(w, "unauthorized", "Send an access token: Authorization: Bearer <token>.")
		return nil, false
	}
	v, p := a.viewerFromAccessToken(r.Context(), tok)
	if p != nil {
		problem.Write(w, p)
		return nil, false
	}
	limit := 600
	if strings.Contains(r.URL.Path, "/renditions/") {
		limit = 3000
	}
	if !a.limit(w, "api:"+v.ClientRowID, limit, time.Minute) {
		return nil, false
	}
	if scope != "" && !v.Scopes[scope] {
		failP(w, "insufficient_scope", "This call needs the %s scope.", scope)
		return nil, false
	}
	return v, true
}

func (a *App) viewerFromAccessToken(ctx context.Context, tok string) (*Viewer, *problem.Problem) {
	if !strings.HasPrefix(tok, "obxa_") {
		return nil, problem.New("invalid_token", "The access token is invalid or expired.")
	}
	body := tok[5:]
	i := strings.LastIndexByte(body, '.')
	if i <= 0 || !a.Keys.Verify("access-token", body[:i], body[i+1:]) {
		return nil, problem.New("invalid_token", "The access token is invalid or expired.")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body[:i])
	var cl accessClaims
	if err != nil || json.Unmarshal(raw, &cl) != nil || cl.E < a.now().Unix() {
		return nil, problem.New("invalid_token", "The access token is invalid or expired.")
	}
	c, err := a.loadClient(ctx, a.DB, "id = ?", cl.C)
	if err != nil {
		return nil, problem.As(err)
	}
	if c == nil || c.Status != "active" || c.Version != cl.V {
		return nil, problem.New("access_revoked", "This API key was revoked or its secret was rotated.")
	}
	return a.viewerForClient(ctx, c, strings.Fields(cl.S))
}

// viewerFromTag resolves the viewer encoded in a signed URL.
func (a *App) viewerFromTag(ctx context.Context, tag string) (*Viewer, *problem.Problem) {
	switch {
	case tag == "a":
		return &Viewer{Kind: "admin", OriginalAllowed: true}, nil
	case strings.HasPrefix(tag, "c."):
		c, err := a.loadClient(ctx, a.DB, "id = ?", tag[2:])
		if err != nil {
			return nil, problem.As(err)
		}
		if c == nil {
			return nil, problem.New("access_revoked", "This API key was revoked.")
		}
		return a.viewerForClient(ctx, c, c.Scopes)
	case strings.HasPrefix(tag, "u."):
		u, err := a.loadUser(ctx, a.DB, "id = ?", tag[2:])
		if err != nil {
			return nil, problem.As(err)
		}
		if u == nil || u.Status != "active" {
			return nil, problem.New("access_revoked", "This account is blocked.")
		}
		return a.viewerForUser(ctx, u)
	}
	return nil, problem.New("unauthorized", "Invalid link.")
}

// viewerForUser builds the portal viewer of a logged-in user.
func (a *App) viewerForUser(ctx context.Context, u *userRow) (*Viewer, *problem.Problem) {
	if u.Role == "admin" {
		return &Viewer{Kind: "admin", OriginalAllowed: true, UserID: u.ID, UserName: u.Name, Language: u.Language}, nil
	}
	if u.RetailerID == nil {
		return nil, problem.New("forbidden", "This account has no retailer.")
	}
	ret, err := a.loadRetailer(ctx, a.DB, *u.RetailerID)
	if err != nil {
		return nil, problem.As(err)
	}
	if ret == nil || ret.Status != "active" {
		return nil, problem.New("account_blocked", "Access for your company was blocked.")
	}
	return &Viewer{Kind: "user", RetailerID: ret.ID, RetailerName: ret.Name, BrandIDs: ret.BrandIDs,
		OriginalAllowed: a.originalAllowed(ctx, ret), UserID: u.ID, UserName: u.Name, Language: u.Language}, nil
}

func nullStr(s sql.NullString) *string {
	if s.Valid {
		return &s.String
	}
	return nil
}

func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
