package node

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/ids"
	"github.com/openbrandexchange/node-go/internal/importfmt"
	"github.com/openbrandexchange/node-go/internal/problem"
	"github.com/openbrandexchange/node-go/internal/secure"
)

var langOK = func(l string) bool {
	if len(l) < 2 || len(l) > 8 {
		return false
	}
	return importfmt.NormalizeLang(l) == l
}

// publicBranding is the PublicBranding schema.
func (a *App) publicBranding(ctx context.Context) map[string]any {
	s := a.Settings(ctx)
	name := s.Branding.DisplayName
	if name == "" {
		name = s.Company.Name
	}
	brands := []map[string]string{}
	rs, err := a.DB.Query(ctx, `SELECT id, name FROM brands WHERE visible = 1 ORDER BY name`)
	if err == nil {
		for rs.Next() {
			var id, n string
			if rs.Scan(&id, &n) == nil {
				brands = append(brands, map[string]string{"id": id, "name": n})
			}
		}
		rs.Close()
	}
	return map[string]any{"name": name, "logo_url": a.logoURL(s), "accent_color": s.Branding.AccentColor, "welcome_text": s.Branding.WelcomeText,
		"languages": s.ContentLanguages(), "default_language": s.ContentLanguages()[0], "access_requests_enabled": s.Portal.AccessRequestsEnabled,
		"setup_required": !a.SetupDone(ctx), "brands": brands, "portal_name": s.Branding.PortalName}
}

func (a *App) handlePublicBranding(w http.ResponseWriter, r *http.Request) {
	if !a.limit(w, "public:"+a.clientIP(r), 300, time.Minute) {
		return
	}
	writeJSON(w, 200, a.publicBranding(r.Context()))
}

func (a *App) handlePublicLogo(w http.ResponseWriter, r *http.Request) {
	s := a.Settings(r.Context())
	if s.LogoSHA == "" {
		failP(w, "not_found", "No logo.")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if err := a.serveBlob(w, r, s.LogoSHA, s.LogoType, ""); err != nil {
		a.fail(w, r, err)
		return
	}
}

func (a *App) handleSetupGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]bool{"required": !a.SetupDone(r.Context())})
}

func (a *App) handleSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !a.limit(w, "setup:"+a.clientIP(r), 10, 15*time.Minute) {
		return
	}
	if a.SetupDone(ctx) {
		failP(w, "setup_completed", "This node is already set up.")
		return
	}
	var body struct {
		SetupCode string `json:"setup_code"`
		Company   struct {
			Name            string   `json:"name"`
			FirstBrandName  string   `json:"first_brand_name"`
			Website         string   `json:"website"`
			ContactEmail    string   `json:"contact_email"`
			DefaultLanguage string   `json:"default_language"`
			Languages       []string `json:"languages"`
			AccentColor     string   `json:"accent_color"`
		} `json:"company"`
		Admin struct {
			Name     string `json:"name"`
			Email    string `json:"email"`
			Password string `json:"password"`
		} `json:"admin"`
	}
	if p := readJSON(r, &body, 0); p != nil {
		problem.Write(w, p)
		return
	}
	want, _ := getMeta(ctx, a.DB, "setup_code_hash")
	code := strings.ToUpper(strings.TrimSpace(body.SetupCode))
	if want == "" || !(secure.Equal(want, secure.HashToken(body.SetupCode)) || secure.Equal(want, secure.HashToken(code))) {
		failP(w, "forbidden", "The setup code is not correct. You find it in the node's log.")
		return
	}
	p := problem.New("validation_failed", "Please check the highlighted fields.")
	name := strings.TrimSpace(body.Company.Name)
	if name == "" || len(name) > 200 {
		p.Field("/company/name", "validation_failed", "required")
	}
	lang := importfmt.NormalizeLang(body.Company.DefaultLanguage)
	if !langOK(lang) {
		p.Field("/company/default_language", "validation_failed", "language like nl or en")
	}
	adminName := strings.TrimSpace(body.Admin.Name)
	if adminName == "" {
		p.Field("/admin/name", "validation_failed", "required")
	}
	email := normEmail(body.Admin.Email)
	if !validEmail(email) {
		p.Field("/admin/email", "validation_failed", "invalid e-mail")
	}
	if reason := secure.PasswordProblem(body.Admin.Password); reason != "" {
		p.Field("/admin/password", "validation_failed", reason)
	}
	if body.Company.AccentColor != "" && !isHexColor(body.Company.AccentColor) {
		p.Field("/company/accent_color", "validation_failed", "#rrggbb")
	}
	if body.Company.ContactEmail != "" && !validEmail(normEmail(body.Company.ContactEmail)) {
		p.Field("/company/contact_email", "validation_failed", "invalid e-mail")
	}
	if body.Company.Website != "" && !strings.HasPrefix(body.Company.Website, "http") {
		p.Field("/company/website", "validation_failed", "absolute URL")
	}
	if len(p.Errors) > 0 {
		problem.Write(w, p)
		return
	}
	hash, err := secure.HashPassword(body.Admin.Password)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	s := defaultSettings()
	s.Company.Name = name
	s.Branding.DisplayName = name
	if body.Company.Website != "" {
		s.Company.Website = strPtr(body.Company.Website)
	}
	if body.Company.ContactEmail != "" {
		s.Company.ContactEmail = strPtr(normEmail(body.Company.ContactEmail))
	}
	if body.Company.AccentColor != "" {
		s.Branding.AccentColor = strPtr(strings.ToUpper(body.Company.AccentColor))
	}
	s.Portal.DefaultLanguage = lang
	langs := []string{lang}
	for _, l := range body.Company.Languages {
		l = importfmt.NormalizeLang(l)
		if langOK(l) && l != lang {
			langs = append(langs, l)
		}
	}
	s.Portal.Languages = langs
	u := &userRow{ID: ids.New("usr"), Email: email, Name: adminName, Role: "admin", Status: "active", Language: supportedLang(lang), CreatedAt: a.nowStr()}
	brandName := strings.TrimSpace(body.Company.FirstBrandName)
	if brandName == "" {
		brandName = name
	}
	err = a.DB.WriteTx(ctx, func(tx *db.Tx) error {
		if done, _ := getMeta(ctx, tx, "setup_done"); done == "1" {
			return problem.New("setup_completed", "This node is already set up.")
		}
		if err := a.saveSettings(ctx, tx, s); err != nil {
			return err
		}
		a.settingsMu.Lock()
		a.settings = s
		a.settingsMu.Unlock()
		wr := a.newWriter(ctx, tx)
		slug := importfmt.Slug(brandName)
		if slug == "" {
			slug = "brand"
		}
		if _, p := wr.Upsert(KindBrand, slug, map[string]any{"name": brandName, "status": "published"}, ""); p != nil {
			return p
		}
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, email, name, role, status, language, password_hash, created_at) VALUES (?, ?, ?, 'admin', 'active', ?, ?, ?)`,
			u.ID, u.Email, u.Name, u.Language, hash, u.CreatedAt); err != nil {
			return err
		}
		if err := setMeta(ctx, tx, "setup_done", "1"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM meta WHERE key = 'setup_code_plain_once'`)
		return err
	})
	if err != nil {
		a.settingsMu.Lock()
		a.settings = nil
		a.settingsMu.Unlock()
		a.fail(w, r, err)
		return
	}
	a.setupDone.Store(true)
	csrf, err := a.startSession(ctx, w, r, u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.Log.Info("first-run setup completed", "company", name)
	writeJSON(w, 201, a.sessionJSON(ctx, &session{User: u, CSRF: csrf}, csrf))
}

func isHexColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for _, c := range s[1:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// handleSession returns the session and sets the CSRF cookie.
func (a *App) handleSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := a.loadSession(r)
	csrf := ""
	if s != nil {
		csrf = s.CSRF
	} else if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 20 {
		csrf = c.Value
	} else {
		csrf = secure.RandomString(24)
	}
	a.setCookie(w, csrfCookie, csrf, int(sessionTTL.Seconds()))
	writeJSON(w, 200, a.sessionJSON(ctx, s, csrf))
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if p := readJSON(r, &body, 0); p != nil {
		problem.Write(w, p)
		return
	}
	email := normEmail(body.Email)
	if !a.limit(w, "login-ip:"+a.clientIP(r), 10, 15*time.Minute) || !a.limit(w, "login-email:"+email, 10, 15*time.Minute) {
		return
	}
	u, err := a.loadUser(ctx, a.DB, "email = ?", email)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if u == nil || u.PasswordHash == nil || len(body.Password) > 200 {
		secure.DummyPasswordCheck(body.Password)
		failP(w, "invalid_credentials", "The e-mail address or password is not correct.")
		return
	}
	if !secure.CheckPassword(*u.PasswordHash, body.Password) {
		failP(w, "invalid_credentials", "The e-mail address or password is not correct.")
		return
	}
	if u.Status == "blocked" {
		failP(w, "account_blocked", "This account is blocked.")
		return
	}
	if u.Role == "retailer" {
		if u.RetailerID == nil {
			failP(w, "account_blocked", "This account is blocked.")
			return
		}
		ret, _ := a.loadRetailer(ctx, a.DB, *u.RetailerID)
		if ret == nil || ret.Status != "active" {
			failP(w, "account_blocked", "Access for your company is blocked.")
			return
		}
	}
	csrf, err := a.startSession(ctx, w, r, u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, a.sessionJSON(ctx, &session{User: u, CSRF: csrf}, csrf))
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_, _ = a.DB.Exec(r.Context(), `DELETE FROM sessions WHERE id_hash = ?`, secure.HashToken(c.Value))
	}
	a.setCookie(w, sessionCookie, "", -1)
	w.WriteHeader(204)
}

func (a *App) handleForgot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !a.limit(w, "forgot:"+a.clientIP(r), 5, 15*time.Minute) {
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	if p := readJSON(r, &body, 0); p != nil {
		problem.Write(w, p)
		return
	}
	u, _ := a.loadUser(ctx, a.DB, "email = ?", normEmail(body.Email))
	if u != nil && u.Status == "active" {
		token := secure.RandomString(32)
		_, _ = a.DB.Exec(ctx, `UPDATE users SET reset_hash = ?, reset_expires_at = ? WHERE id = ?`, secure.HashToken(token), db.Fmt(a.now().Add(time.Hour)), u.ID)
		go a.sendMail(context.Background(), u.Email, u.Language, "password_reset", map[string]string{"name": u.Name, "link": a.Cfg.PublicURL + "/reset-password/" + token})
	}
	w.WriteHeader(202)
}

// ResetLink creates a one-time password reset link (CLI).
func (a *App) ResetLink(ctx context.Context, email string) (string, error) {
	u, err := a.loadUser(ctx, a.DB, "email = ?", normEmail(email))
	if err != nil {
		return "", err
	}
	if u == nil {
		return "", problem.New("not_found", "No account with e-mail %s.", email)
	}
	token := secure.RandomString(32)
	if _, err := a.DB.Exec(ctx, `UPDATE users SET reset_hash = ?, reset_expires_at = ? WHERE id = ?`, secure.HashToken(token), db.Fmt(a.now().Add(time.Hour)), u.ID); err != nil {
		return "", err
	}
	return a.Cfg.PublicURL + "/reset-password/" + token, nil
}

// SetPassword sets a password directly (CLI) and ends all sessions.
func (a *App) SetPassword(ctx context.Context, email, password string) error {
	if reason := secure.PasswordProblem(password); reason != "" {
		return problem.New("validation_failed", "%s", reason)
	}
	u, err := a.loadUser(ctx, a.DB, "email = ?", normEmail(email))
	if err != nil {
		return err
	}
	if u == nil {
		return problem.New("not_found", "No account with e-mail %s.", email)
	}
	hash, err := secure.HashPassword(password)
	if err != nil {
		return err
	}
	status := u.Status
	if status == "invited" {
		status = "active"
	}
	if _, err := a.DB.Exec(ctx, `UPDATE users SET password_hash = ?, status = ?, reset_hash = NULL, invite_hash = NULL WHERE id = ?`, hash, status, u.ID); err != nil {
		return err
	}
	a.endSessions(ctx, a.DB, u.ID, "")
	return nil
}

func (a *App) handleReset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !a.limit(w, "reset:"+a.clientIP(r), 10, 15*time.Minute) {
		return
	}
	var body struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if p := readJSON(r, &body, 0); p != nil {
		problem.Write(w, p)
		return
	}
	u, _ := a.loadUser(ctx, a.DB, "reset_hash = ?", secure.HashToken(body.Token))
	if u == nil || u.ResetExpires == nil || db.Parse(*u.ResetExpires).Before(a.now()) || u.Status == "blocked" {
		failP(w, "invite_invalid", "This link is not valid any more. Ask for a new one.")
		return
	}
	if reason := secure.PasswordProblem(body.Password); reason != "" {
		problem.Write(w, problem.New("validation_failed", "%s", reason).Field("/password", "validation_failed", reason))
		return
	}
	hash, err := secure.HashPassword(body.Password)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	res, err := a.DB.Exec(ctx, `UPDATE users SET password_hash = ?, reset_hash = NULL, reset_expires_at = NULL, status = 'active' WHERE id = ? AND reset_hash = ?`, hash, u.ID, secure.HashToken(body.Token))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		failP(w, "invite_invalid", "This link is not valid any more. Ask for a new one.")
		return
	}
	a.endSessions(ctx, a.DB, u.ID, "")
	u.Status = "active"
	csrf, err := a.startSession(ctx, w, r, u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, a.sessionJSON(ctx, &session{User: u, CSRF: csrf}, csrf))
}

func (a *App) inviteByToken(ctx context.Context, token string) *userRow {
	if len(token) < 20 || len(token) > 200 {
		return nil
	}
	u, _ := a.loadUser(ctx, a.DB, "invite_hash = ?", secure.HashToken(token))
	if u == nil || u.Status != "invited" || u.InviteExpires == nil || db.Parse(*u.InviteExpires).Before(a.now()) {
		return nil
	}
	return u
}

func (a *App) handleInviteGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !a.limit(w, "invite:"+a.clientIP(r), 30, 15*time.Minute) {
		return
	}
	u := a.inviteByToken(ctx, r.PathValue("token"))
	if u == nil {
		failP(w, "invite_invalid", "This invitation is not valid any more. Ask for a new one.")
		return
	}
	var retName *string
	if u.RetailerID != nil {
		if ret, _ := a.loadRetailer(ctx, a.DB, *u.RetailerID); ret != nil {
			retName = &ret.Name
		}
	}
	writeJSON(w, 200, map[string]any{"email": u.Email, "name": u.Name, "role": u.Role, "retailer_name": retName, "expires_at": u.InviteExpires, "language": u.Language})
}

func (a *App) handleInviteAccept(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !a.limit(w, "invite:"+a.clientIP(r), 30, 15*time.Minute) {
		return
	}
	var body struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		Language string `json:"language"`
	}
	if p := readJSON(r, &body, 0); p != nil {
		problem.Write(w, p)
		return
	}
	token := r.PathValue("token")
	u := a.inviteByToken(ctx, token)
	if u == nil {
		failP(w, "invite_invalid", "This invitation is not valid any more. Ask for a new one.")
		return
	}
	if reason := secure.PasswordProblem(body.Password); reason != "" {
		problem.Write(w, problem.New("validation_failed", "%s", reason).Field("/password", "validation_failed", reason))
		return
	}
	hash, err := secure.HashPassword(body.Password)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if n := strings.TrimSpace(body.Name); n != "" && len(n) <= 200 {
		u.Name = n
	}
	if body.Language != "" {
		u.Language = supportedLang(body.Language)
	}
	res, err := a.DB.Exec(ctx, `UPDATE users SET password_hash = ?, status = 'active', invite_hash = NULL, invite_expires_at = NULL, name = ?, language = ? WHERE id = ? AND invite_hash = ?`,
		hash, u.Name, u.Language, u.ID, secure.HashToken(token))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		failP(w, "invite_invalid", "This invitation is not valid any more. Ask for a new one.")
		return
	}
	u.Status = "active"
	u.InviteExpires = nil
	csrf, err := a.startSession(ctx, w, r, u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, a.sessionJSON(ctx, &session{User: u, CSRF: csrf}, csrf))
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := sessionFrom(ctx)
	var body struct {
		Name            *string `json:"name"`
		Language        *string `json:"language"`
		CurrentPassword *string `json:"current_password"`
		NewPassword     *string `json:"new_password"`
	}
	if p := readJSON(r, &body, 0); p != nil {
		problem.Write(w, p)
		return
	}
	u := s.User
	if body.Name != nil {
		n := strings.TrimSpace(*body.Name)
		if n == "" || len(n) > 200 {
			failP(w, "validation_failed", "A name is required.")
			return
		}
		u.Name = n
	}
	if body.Language != nil {
		u.Language = supportedLang(*body.Language)
	}
	if body.NewPassword != nil {
		if body.CurrentPassword == nil || u.PasswordHash == nil || !secure.CheckPassword(*u.PasswordHash, *body.CurrentPassword) {
			failP(w, "invalid_credentials", "Your current password is not correct.")
			return
		}
		if reason := secure.PasswordProblem(*body.NewPassword); reason != "" {
			problem.Write(w, problem.New("validation_failed", "%s", reason).Field("/new_password", "validation_failed", reason))
			return
		}
		hash, err := secure.HashPassword(*body.NewPassword)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if _, err := a.DB.Exec(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, u.ID); err != nil {
			a.fail(w, r, err)
			return
		}
		a.endSessions(ctx, a.DB, u.ID, s.IDHash)
	}
	if _, err := a.DB.Exec(ctx, `UPDATE users SET name = ?, language = ? WHERE id = ?`, u.Name, u.Language, u.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, a.sessionJSON(ctx, s, s.CSRF))
}
