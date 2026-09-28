package node

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/ids"
	"github.com/openbrandexchange/node-go/internal/problem"
	"github.com/openbrandexchange/node-go/internal/secure"
)

const inviteTTL = 7 * 24 * time.Hour

// ---- retailers ------------------------------------------------------------------------

func (a *App) retailerJSON(ctx context.Context, r *retailerRow) map[string]any {
	if r == nil {
		return nil
	}
	var users, clients int
	_ = a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE retailer_id = ?`, r.ID).Scan(&users)
	_ = a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM api_clients WHERE retailer_id = ? AND status = 'active'`, r.ID).Scan(&clients)
	return map[string]any{"id": r.ID, "name": r.Name, "reference": r.Reference, "website": r.Website, "vat_number": r.VAT,
		"country": r.Country, "notes": r.Notes, "status": r.Status, "brand_ids": r.BrandIDs, "allow_original": r.AllowOriginal,
		"user_count": users, "api_client_count": clients, "last_activity_at": r.LastActivity, "created_at": r.CreatedAt}
}

// retailerInput validates RetailerInput fields present in doc.
func (a *App) retailerInput(ctx context.Context, doc map[string]any, r *retailerRow) *problem.Problem {
	str := func(k string, max int) (*string, *problem.Problem) {
		v, ok := doc[k]
		if !ok || v == nil {
			return nil, nil
		}
		s, ok := v.(string)
		if !ok || len(s) > max {
			return nil, problem.New("validation_failed", "%s must be text of at most %d characters.", k, max).Field("/"+k, "validation_failed", "invalid")
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, nil
		}
		return &s, nil
	}
	if v, ok := doc["name"]; ok {
		s, _ := v.(string)
		s = strings.TrimSpace(s)
		if s == "" || len(s) > 200 {
			return problem.New("validation_failed", "A name is required.").Field("/name", "validation_failed", "required")
		}
		r.Name = s
	}
	for _, f := range []struct {
		k   string
		max int
		dst **string
	}{{"reference", 100, &r.Reference}, {"website", 2048, &r.Website}, {"vat_number", 32, &r.VAT}, {"country", 2, &r.Country}, {"notes", 5000, &r.Notes}} {
		if _, present := doc[f.k]; !present {
			continue
		}
		s, p := str(f.k, f.max)
		if p != nil {
			return p
		}
		if f.k == "country" && s != nil {
			u := strings.ToUpper(*s)
			if len(u) != 2 {
				return problem.New("validation_failed", "country must be a two-letter code.").Field("/country", "validation_failed", "invalid")
			}
			s = &u
		}
		*f.dst = s
	}
	if v, ok := doc["status"]; ok {
		s, _ := v.(string)
		if s != "active" && s != "blocked" {
			return problem.New("validation_failed", "status must be active or blocked.").Field("/status", "validation_failed", "invalid")
		}
		r.Status = s
	}
	if v, ok := doc["brand_ids"]; ok {
		if v == nil {
			r.BrandIDs = nil
		} else {
			raw, ok := v.([]any)
			if !ok {
				return problem.New("validation_failed", "brand_ids must be a list or null.").Field("/brand_ids", "validation_failed", "invalid")
			}
			list := []string{}
			for _, x := range raw {
				s, _ := x.(string)
				var id string
				if a.DB.QueryRow(ctx, `SELECT id FROM brands WHERE id = ?`, s).Scan(&id) != nil {
					return problem.New("validation_failed", "Unknown brand %q.", s).Field("/brand_ids", "validation_failed", "unknown brand")
				}
				list = append(list, s)
			}
			sort.Strings(list)
			r.BrandIDs = list
		}
	}
	if v, ok := doc["allow_original"]; ok {
		if v == nil {
			r.AllowOriginal = nil
		} else if b, ok := v.(bool); ok {
			r.AllowOriginal = &b
		} else {
			return problem.New("validation_failed", "allow_original must be true, false or null.").Field("/allow_original", "validation_failed", "invalid")
		}
	}
	return nil
}

func brandIDsJSON(ids []string) any {
	if ids == nil {
		return nil
	}
	return string(mustJSON(ids))
}

func boolPtrDB(b *bool) any {
	if b == nil {
		return nil
	}
	return db.Bool(*b)
}

func (a *App) createRetailer(ctx context.Context, doc map[string]any) (*retailerRow, *problem.Problem) {
	r := &retailerRow{ID: ids.New("rtl"), Status: "active"}
	if _, ok := doc["name"]; !ok {
		return nil, problem.New("validation_failed", "A name is required.").Field("/name", "validation_failed", "required")
	}
	if p := a.retailerInput(ctx, doc, r); p != nil {
		return nil, p
	}
	now := a.nowStr()
	r.CreatedAt, r.UpdatedAt, r.GrantUpdatedAt = now, now, now
	_, err := a.DB.Exec(ctx, `INSERT INTO retailers (id, name, reference, website, vat_number, country, notes, status, brand_ids, allow_original, created_at, updated_at, grant_updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Name, r.Reference, r.Website, r.VAT, r.Country, r.Notes, r.Status, brandIDsJSON(r.BrandIDs), boolPtrDB(r.AllowOriginal), now, now, now)
	if err != nil {
		return nil, problem.As(err)
	}
	return r, nil
}

func (a *App) adminRetailers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, p := limitParam(r, 50, 200)
	if p != nil {
		problem.Write(w, p)
		return
	}
	where := `WHERE 1 = 1`
	var args []any
	if q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q"))); q != "" {
		where += ` AND (LOWER(name) LIKE ? OR LOWER(COALESCE(reference, '')) LIKE ?)`
		args = append(args, "%"+q+"%", "%"+q+"%")
	}
	if s := r.URL.Query().Get("status"); s != "" {
		where += ` AND status = ?`
		args = append(args, s)
	}
	if c := r.URL.Query().Get("cursor"); c != "" {
		var cur struct {
			N string `json:"n"`
			I string `json:"i"`
		}
		if !a.decodeCursor("retailers", c, &cur) {
			failP(w, "cursor_invalid", "The cursor is not valid.")
			return
		}
		where += ` AND (LOWER(name) > ? OR (LOWER(name) = ? AND id > ?))`
		args = append(args, cur.N, cur.N, cur.I)
	}
	rs, err := a.DB.Query(ctx, `SELECT id FROM retailers `+where+` ORDER BY LOWER(name), id LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var list []string
	for rs.Next() {
		var id string
		if rs.Scan(&id) == nil {
			list = append(list, id)
		}
	}
	rs.Close()
	items := []any{}
	var last *retailerRow
	for i, id := range list {
		if i == limit {
			break
		}
		if ret, _ := a.loadRetailer(ctx, a.DB, id); ret != nil {
			items = append(items, a.retailerJSON(ctx, ret))
			last = ret
		}
	}
	var next *string
	if len(list) > limit && last != nil {
		c := a.encodeCursor("retailers", map[string]string{"n": strings.ToLower(last.Name), "i": last.ID})
		next = &c
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}

func (a *App) adminRetailerCreate(w http.ResponseWriter, r *http.Request) {
	doc, p := readJSONObject(r)
	if p != nil {
		problem.Write(w, p)
		return
	}
	ret, p := a.createRetailer(r.Context(), doc)
	if p != nil {
		problem.Write(w, p)
		return
	}
	writeJSON(w, 201, a.retailerJSON(r.Context(), ret))
}

func (a *App) adminRetailerGet(w http.ResponseWriter, r *http.Request) {
	ret, err := a.loadRetailer(r.Context(), a.DB, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if ret == nil {
		failP(w, "not_found", "Retailer not found.")
		return
	}
	writeJSON(w, 200, a.retailerJSON(r.Context(), ret))
}

func (a *App) adminRetailerPatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	doc, p := readJSONObject(r)
	if p != nil {
		problem.Write(w, p)
		return
	}
	ret, err := a.loadRetailer(ctx, a.DB, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if ret == nil {
		failP(w, "not_found", "Retailer not found.")
		return
	}
	before := *ret
	if p := a.retailerInput(ctx, doc, ret); p != nil {
		problem.Write(w, p)
		return
	}
	grantChanged := string(mustJSON(before.BrandIDs)) != string(mustJSON(ret.BrandIDs)) ||
		string(mustJSON(before.AllowOriginal)) != string(mustJSON(ret.AllowOriginal)) ||
		(before.Status != ret.Status && ret.Status == "active")
	err = a.DB.WriteTx(ctx, func(tx *db.Tx) error {
		now := a.nowStr()
		grantAt := ret.GrantUpdatedAt
		if grantChanged || before.Status != ret.Status {
			grantAt = now
		}
		if _, err := tx.Exec(ctx, `UPDATE retailers SET name = ?, reference = ?, website = ?, vat_number = ?, country = ?, notes = ?, status = ?, brand_ids = ?, allow_original = ?, updated_at = ?, grant_updated_at = ? WHERE id = ?`,
			ret.Name, ret.Reference, ret.Website, ret.VAT, ret.Country, ret.Notes, ret.Status, brandIDsJSON(ret.BrandIDs), boolPtrDB(ret.AllowOriginal), now, grantAt, ret.ID); err != nil {
			return err
		}
		if ret.Status == "blocked" && before.Status != "blocked" {
			if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE retailer_id = ?)`, ret.ID); err != nil {
				return err
			}
		}
		if grantChanged || (before.Status != ret.Status) {
			w := a.newWriter(ctx, tx)
			return w.emitGrant(ret.ID, grantChanged)
		}
		return nil
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if grantChanged {
		go a.retailerGrantEvent(context.Background(), ret.ID)
	}
	ret, _ = a.loadRetailer(ctx, a.DB, ret.ID)
	writeJSON(w, 200, a.retailerJSON(ctx, ret))
}

func (a *App) adminRetailerDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ret, err := a.loadRetailer(ctx, a.DB, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if ret == nil {
		failP(w, "not_found", "Retailer not found.")
		return
	}
	downloads := a.userDownloadIDs(ctx, `owner_id IN (SELECT id FROM users WHERE retailer_id = ?)`, ret.ID)
	err = a.DB.WriteTx(ctx, func(tx *db.Tx) error {
		// Everything personal goes: accounts, keys, webhooks, access
		// requests, prepared downloads, pending e-mails. Download statistics
		// keep the company name but lose the people; audit entries keep the
		// action but lose the e-mail address.
		stmts := []string{
			`DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE retailer_id = ?)`,
			`DELETE FROM mail_outbox WHERE recipient IN (SELECT email FROM users WHERE retailer_id = ?)`,
			`UPDATE audit_log SET actor_email = NULL WHERE actor_id IN (SELECT id FROM users WHERE retailer_id = ?)`,
			`DELETE FROM job_items WHERE job_id IN (SELECT id FROM jobs WHERE kind = 'download' AND owner_type = 'user' AND owner_id IN (SELECT id FROM users WHERE retailer_id = ?))`,
			`DELETE FROM jobs WHERE kind = 'download' AND owner_type = 'user' AND owner_id IN (SELECT id FROM users WHERE retailer_id = ?)`,
			`DELETE FROM access_requests WHERE retailer_id = ?`,
			`DELETE FROM webhook_deliveries WHERE webhook_id IN (SELECT id FROM webhooks WHERE owner_type = 'client' AND owner_id IN (SELECT id FROM api_clients WHERE retailer_id = ?))`,
			`DELETE FROM webhooks WHERE owner_type = 'client' AND owner_id IN (SELECT id FROM api_clients WHERE retailer_id = ?)`,
			`DELETE FROM api_clients WHERE retailer_id = ?`,
			`DELETE FROM users WHERE retailer_id = ?`,
			`UPDATE download_events SET actor_id = NULL, actor_name = NULL WHERE retailer_id = ?`,
			`DELETE FROM retailers WHERE id = ?`,
		}
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s, ret.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.deleteDownloadFiles(ctx, downloads)
	w.WriteHeader(204)
}

// userDownloadIDs lists prepared download zips of users matching where.
func (a *App) userDownloadIDs(ctx context.Context, where string, args ...any) []string {
	var out []string
	rs, err := a.DB.Query(ctx, `SELECT id FROM jobs WHERE kind = 'download' AND owner_type = 'user' AND `+where, args...)
	if err != nil {
		return nil
	}
	defer rs.Close()
	for rs.Next() {
		var id string
		if rs.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

func (a *App) deleteDownloadFiles(ctx context.Context, jobIDs []string) {
	for _, id := range jobIDs {
		_ = a.Store.Delete(ctx, "downloads/"+id+".zip")
	}
}

// ---- users and invites -------------------------------------------------------------------

func (a *App) inviteUser(ctx context.Context, email, name, role string, retailerID *string, lang string) (map[string]any, *problem.Problem) {
	email = normEmail(email)
	if !validEmail(email) {
		return nil, problem.New("validation_failed", "Enter a valid e-mail address.").Field("/email", "validation_failed", "invalid e-mail")
	}
	if u, _ := a.loadUser(ctx, a.DB, "email = ?", email); u != nil {
		return nil, problem.New("conflict", "%s already has an account.", email)
	}
	if role != "admin" && role != "retailer" {
		return nil, problem.New("validation_failed", "role must be admin or retailer.").Field("/role", "validation_failed", "invalid")
	}
	if role == "retailer" {
		if retailerID == nil {
			return nil, problem.New("validation_failed", "Choose the retailer this person works for.").Field("/retailer_id", "validation_failed", "required")
		}
		if ret, _ := a.loadRetailer(ctx, a.DB, *retailerID); ret == nil {
			return nil, problem.New("validation_failed", "Unknown retailer.").Field("/retailer_id", "validation_failed", "unknown retailer")
		}
	} else {
		retailerID = nil
	}
	if lang == "" {
		lang = a.Settings(ctx).Portal.DefaultLanguage
	}
	lang = supportedLang(lang)
	if strings.TrimSpace(name) == "" {
		name = strings.SplitN(email, "@", 2)[0]
	}
	u := &userRow{ID: ids.New("usr"), Email: email, Name: strings.TrimSpace(name), Role: role, Status: "invited", Language: lang, CreatedAt: a.nowStr(), RetailerID: retailerID}
	token := secure.RandomString(32)
	exp := db.Fmt(a.now().Add(inviteTTL))
	h := secure.HashToken(token)
	u.InviteHash, u.InviteExpires = &h, &exp
	if _, err := a.DB.Exec(ctx, `INSERT INTO users (id, email, name, role, retailer_id, status, language, invite_hash, invite_expires_at, created_at) VALUES (?, ?, ?, ?, ?, 'invited', ?, ?, ?, ?)`,
		u.ID, u.Email, u.Name, u.Role, u.RetailerID, u.Language, h, exp, u.CreatedAt); err != nil {
		if db.IsUniqueViolation(err) {
			return nil, problem.New("conflict", "%s already has an account.", email)
		}
		return nil, problem.As(err)
	}
	return a.sendInvite(ctx, u, token), nil
}

func (a *App) sendInvite(ctx context.Context, u *userRow, token string) map[string]any {
	link := a.Cfg.PublicURL + "/invite/" + token
	vars := map[string]string{"name": u.Name, "url": link}
	if u.InviteExpires != nil {
		vars["expires"] = mailDate(db.Parse(*u.InviteExpires))
	}
	sent := a.sendMail(ctx, u.Email, u.Language, mailInvite, vars)
	return map[string]any{"user": userJSON(u), "invite_url": link, "expires_at": u.InviteExpires, "email_sent": sent}
}

func validEmail(e string) bool {
	at := strings.LastIndexByte(e, '@')
	return at > 0 && at < len(e)-3 && strings.Contains(e[at:], ".") && len(e) <= 254 && !strings.ContainsAny(e, " \t\r\n<>,;\"")
}

func (a *App) adminUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, p := limitParam(r, 50, 200)
	if p != nil {
		problem.Write(w, p)
		return
	}
	where := `WHERE 1 = 1`
	var args []any
	if v := r.URL.Query().Get("role"); v != "" {
		where += ` AND role = ?`
		args = append(args, v)
	}
	if v := r.URL.Query().Get("retailer_id"); v != "" {
		where += ` AND retailer_id = ?`
		args = append(args, v)
	}
	if c := r.URL.Query().Get("cursor"); c != "" {
		var cur struct {
			E string `json:"e"`
		}
		if !a.decodeCursor("users", c, &cur) {
			failP(w, "cursor_invalid", "The cursor is not valid.")
			return
		}
		where += ` AND email > ?`
		args = append(args, cur.E)
	}
	rs, err := a.DB.Query(ctx, `SELECT id FROM users `+where+` ORDER BY email LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var list []string
	for rs.Next() {
		var id string
		if rs.Scan(&id) == nil {
			list = append(list, id)
		}
	}
	rs.Close()
	items := []any{}
	var last *userRow
	for i, id := range list {
		if i == limit {
			break
		}
		if u, _ := a.loadUser(ctx, a.DB, "id = ?", id); u != nil {
			items = append(items, userJSON(u))
			last = u
		}
	}
	var next *string
	if len(list) > limit && last != nil {
		c := a.encodeCursor("users", map[string]string{"e": last.Email})
		next = &c
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}

func (a *App) adminUserInvite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email      string  `json:"email"`
		Name       string  `json:"name"`
		Role       string  `json:"role"`
		RetailerID *string `json:"retailer_id"`
		Language   string  `json:"language"`
	}
	if p := readJSON(r, &body, 0); p != nil {
		problem.Write(w, p)
		return
	}
	res, p := a.inviteUser(r.Context(), body.Email, body.Name, body.Role, body.RetailerID, body.Language)
	if p != nil {
		problem.Write(w, p)
		return
	}
	writeJSON(w, 201, res)
}

// lastAdminGuard refuses changes that leave no active admin, or target yourself.
func (a *App) lastAdminGuard(ctx context.Context, u *userRow, self *userRow) *problem.Problem {
	if u.ID == self.ID {
		return problem.New("conflict", "You cannot block or delete your own account.")
	}
	if u.Role == "admin" && u.Status == "active" {
		var n int
		_ = a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin' AND status = 'active' AND id <> ?`, u.ID).Scan(&n)
		if n == 0 {
			return problem.New("conflict", "This is the last active administrator.")
		}
	}
	return nil
}

func (a *App) adminUserPatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Name   *string `json:"name"`
		Status *string `json:"status"`
	}
	if p := readJSON(r, &body, 0); p != nil {
		problem.Write(w, p)
		return
	}
	u, err := a.loadUser(ctx, a.DB, "id = ?", r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if u == nil {
		failP(w, "not_found", "Account not found.")
		return
	}
	if body.Name != nil {
		n := strings.TrimSpace(*body.Name)
		if n == "" || len(n) > 200 {
			failP(w, "validation_failed", "A name is required.")
			return
		}
		u.Name = n
	}
	if body.Status != nil {
		switch *body.Status {
		case "blocked":
			if p := a.lastAdminGuard(ctx, u, sessionFrom(ctx).User); p != nil {
				problem.Write(w, p)
				return
			}
			u.Status = "blocked"
			a.endSessions(ctx, a.DB, u.ID, "")
		case "active":
			if u.Status == "blocked" {
				if u.PasswordHash != nil {
					u.Status = "active"
				} else {
					u.Status = "invited"
				}
			}
		default:
			failP(w, "validation_failed", "status must be active or blocked.")
			return
		}
	}
	if _, err := a.DB.Exec(ctx, `UPDATE users SET name = ?, status = ? WHERE id = ?`, u.Name, u.Status, u.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, userJSON(u))
}

func (a *App) adminUserDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, err := a.loadUser(ctx, a.DB, "id = ?", r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if u == nil {
		failP(w, "not_found", "Account not found.")
		return
	}
	if p := a.lastAdminGuard(ctx, u, sessionFrom(ctx).User); p != nil {
		problem.Write(w, p)
		return
	}
	a.endSessions(ctx, a.DB, u.ID, "")
	downloads := a.userDownloadIDs(ctx, `owner_id = ?`, u.ID)
	for _, q := range []string{
		`UPDATE download_events SET actor_id = NULL, actor_name = NULL WHERE actor_id = ?`,
		`UPDATE audit_log SET actor_email = NULL WHERE actor_id = ?`,
		`DELETE FROM job_items WHERE job_id IN (SELECT id FROM jobs WHERE kind = 'download' AND owner_type = 'user' AND owner_id = ?)`,
		`DELETE FROM jobs WHERE kind = 'download' AND owner_type = 'user' AND owner_id = ?`,
	} {
		_, _ = a.DB.Exec(ctx, q, u.ID)
	}
	_, _ = a.DB.Exec(ctx, `DELETE FROM mail_outbox WHERE recipient = ?`, u.Email)
	_, _ = a.DB.Exec(ctx, `DELETE FROM users WHERE id = ?`, u.ID)
	a.deleteDownloadFiles(ctx, downloads)
	w.WriteHeader(204)
}

func (a *App) adminUserResendInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, err := a.loadUser(ctx, a.DB, "id = ?", r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if u == nil {
		failP(w, "not_found", "Account not found.")
		return
	}
	if u.Status != "invited" {
		failP(w, "conflict", "This account is already active.")
		return
	}
	token := secure.RandomString(32)
	exp := db.Fmt(a.now().Add(inviteTTL))
	h := secure.HashToken(token)
	if _, err := a.DB.Exec(ctx, `UPDATE users SET invite_hash = ?, invite_expires_at = ? WHERE id = ?`, h, exp, u.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	u.InviteHash, u.InviteExpires = &h, &exp
	writeJSON(w, 200, a.sendInvite(ctx, u, token))
}

// ---- API clients -------------------------------------------------------------------------

func (a *App) apiClientJSON(c *apiClientRow) map[string]any {
	return map[string]any{"id": c.ID, "client_id": c.ClientID, "name": c.Name, "retailer_id": c.RetailerID, "scopes": c.Scopes,
		"status": c.Status, "created_at": c.CreatedAt, "last_used_at": c.LastUsed}
}

func (a *App) createAPIClient(ctx context.Context, retailerID, name string, scopes []string, createdBy string) (*apiClientRow, string, *problem.Problem) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return nil, "", problem.New("validation_failed", "A name is required.").Field("/name", "validation_failed", "required")
	}
	if ret, _ := a.loadRetailer(ctx, a.DB, retailerID); ret == nil {
		return nil, "", problem.New("validation_failed", "Unknown retailer.").Field("/retailer_id", "validation_failed", "unknown retailer")
	}
	if scopes == nil {
		scopes = append([]string(nil), AllScopes...)
	}
	valid := map[string]bool{}
	for _, s := range AllScopes {
		valid[s] = true
	}
	seen := map[string]bool{}
	var clean []string
	for _, s := range scopes {
		if !valid[s] {
			return nil, "", problem.New("validation_failed", "Unknown scope %q.", s).Field("/scopes", "validation_failed", "unknown scope")
		}
		if !seen[s] {
			seen[s] = true
			clean = append(clean, s)
		}
	}
	sort.Strings(clean)
	c := &apiClientRow{ID: ids.New("cli"), ClientID: "obxc_" + secure.RandomString(16), RetailerID: retailerID, Name: name,
		Status: "active", CreatedBy: createdBy, CreatedAt: a.nowStr(), Version: 1, Scopes: clean}
	secret := secure.Token("obxs_")
	c.SecretHash = secure.HashToken(secret)
	if _, err := a.DB.Exec(ctx, `INSERT INTO api_clients (id, client_id, secret_hash, secret_version, retailer_id, name, scopes, status, created_by, created_at) VALUES (?, ?, ?, 1, ?, ?, ?, 'active', ?, ?)`,
		c.ID, c.ClientID, c.SecretHash, c.RetailerID, c.Name, strings.Join(c.Scopes, " "), createdBy, c.CreatedAt); err != nil {
		return nil, "", problem.As(err)
	}
	return c, secret, nil
}

func (a *App) clientWithSecret(c *apiClientRow, secret string) map[string]any {
	return map[string]any{"client": a.apiClientJSON(c), "client_secret": secret,
		"token_endpoint": a.Cfg.PublicURL + "/obx/v1/oauth/token", "discovery_url": a.Cfg.PublicURL + "/.well-known/obx"}
}

func (a *App) listClients(w http.ResponseWriter, r *http.Request, retailerID string) {
	q := `SELECT id FROM api_clients`
	var args []any
	if retailerID != "" {
		q += ` WHERE retailer_id = ?`
		args = append(args, retailerID)
	}
	rs, err := a.DB.Query(r.Context(), q+` ORDER BY created_at`, args...)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var list []string
	for rs.Next() {
		var id string
		if rs.Scan(&id) == nil {
			list = append(list, id)
		}
	}
	rs.Close()
	items := []any{}
	for _, id := range list {
		if c, _ := a.loadClient(r.Context(), a.DB, "id = ?", id); c != nil {
			items = append(items, a.apiClientJSON(c))
		}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (a *App) adminAPIClients(w http.ResponseWriter, r *http.Request) {
	a.listClients(w, r, r.URL.Query().Get("retailer_id"))
}

func (a *App) adminAPIClientCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RetailerID string   `json:"retailer_id"`
		Name       string   `json:"name"`
		Scopes     []string `json:"scopes"`
	}
	if p := readJSON(r, &body, 0); p != nil {
		problem.Write(w, p)
		return
	}
	c, secret, p := a.createAPIClient(r.Context(), body.RetailerID, body.Name, body.Scopes, "admin")
	if p != nil {
		problem.Write(w, p)
		return
	}
	writeJSON(w, 201, a.clientWithSecret(c, secret))
}

func (a *App) rotateClient(w http.ResponseWriter, r *http.Request, retailerID string) {
	ctx := r.Context()
	c, err := a.loadClient(ctx, a.DB, "id = ?", r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if c == nil || (retailerID != "" && c.RetailerID != retailerID) || c.Status != "active" {
		failP(w, "not_found", "API key not found.")
		return
	}
	secret := secure.Token("obxs_")
	if _, err := a.DB.Exec(ctx, `UPDATE api_clients SET secret_hash = ?, secret_version = secret_version + 1 WHERE id = ?`, secure.HashToken(secret), c.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	c.Version++
	writeJSON(w, 200, a.clientWithSecret(c, secret))
}

func (a *App) revokeClient(w http.ResponseWriter, r *http.Request, retailerID string) {
	ctx := r.Context()
	c, err := a.loadClient(ctx, a.DB, "id = ?", r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if c == nil || (retailerID != "" && c.RetailerID != retailerID) {
		failP(w, "not_found", "API key not found.")
		return
	}
	_, _ = a.DB.Exec(ctx, `UPDATE api_clients SET status = 'revoked' WHERE id = ?`, c.ID)
	_, _ = a.DB.Exec(ctx, `UPDATE webhooks SET status = 'disabled' WHERE owner_type = 'client' AND owner_id = ?`, c.ID)
	w.WriteHeader(204)
}

func (a *App) adminAPIClientRotate(w http.ResponseWriter, r *http.Request) { a.rotateClient(w, r, "") }
func (a *App) adminAPIClientRevoke(w http.ResponseWriter, r *http.Request) { a.revokeClient(w, r, "") }

// ---- ingest tokens --------------------------------------------------------------------------

func (a *App) ingestTokenJSON(t *ingestToken) map[string]any {
	return map[string]any{"id": t.ID, "name": t.Name, "prefix": t.Prefix, "scopes": t.Scopes, "brand_external_ids": t.BrandExternalIDs,
		"status": t.status(a.now()), "expires_at": t.ExpiresAt, "created_at": t.CreatedAt, "last_used_at": t.LastUsed}
}

func (a *App) adminIngestTokens(w http.ResponseWriter, r *http.Request) {
	rs, err := a.DB.Query(r.Context(), `SELECT id FROM ingest_tokens ORDER BY created_at`)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var list []string
	for rs.Next() {
		var id string
		if rs.Scan(&id) == nil {
			list = append(list, id)
		}
	}
	rs.Close()
	items := []any{}
	for _, id := range list {
		if t, _ := a.loadIngestToken(r.Context(), "id = ?", id); t != nil {
			items = append(items, a.ingestTokenJSON(t))
		}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// CreateIngestToken creates a token (admin API and CLI).
func (a *App) CreateIngestToken(ctx context.Context, name string, scopes, brands []string, expiresAt *string) (*ingestToken, string, *problem.Problem) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return nil, "", problem.New("validation_failed", "A name is required.").Field("/name", "validation_failed", "required")
	}
	valid := map[string]bool{"ingest:read": true, "ingest:write": true, "ingest:delete": true}
	if len(scopes) == 0 {
		return nil, "", problem.New("validation_failed", "Choose at least one scope.").Field("/scopes", "validation_failed", "required")
	}
	for _, s := range scopes {
		if !valid[s] {
			return nil, "", problem.New("validation_failed", "Unknown scope %q.", s).Field("/scopes", "validation_failed", "unknown scope")
		}
	}
	var exp any
	if expiresAt != nil {
		t, err := time.Parse(time.RFC3339Nano, *expiresAt)
		if err != nil {
			return nil, "", problem.New("validation_failed", "expires_at must be an RFC 3339 timestamp.").Field("/expires_at", "validation_failed", "invalid")
		}
		exp = db.Fmt(t)
	}
	var brandsJSON any
	if brands != nil {
		brandsJSON = string(mustJSON(brands))
	}
	secret := secure.Token("obxi_")
	t := &ingestToken{ID: ids.New("itk"), Name: name, Prefix: secret[:12], Status: "active", CreatedAt: a.nowStr(), Scopes: scopes, BrandExternalIDs: brands}
	if exp != nil {
		s := exp.(string)
		t.ExpiresAt = &s
	}
	if _, err := a.DB.Exec(ctx, `INSERT INTO ingest_tokens (id, name, prefix, token_hash, scopes, brand_external_ids, status, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?, 'active', ?, ?)`,
		t.ID, t.Name, t.Prefix, secure.HashToken(secret), strings.Join(scopes, " "), brandsJSON, exp, t.CreatedAt); err != nil {
		return nil, "", problem.As(err)
	}
	return t, secret, nil
}

func (a *App) adminIngestTokenCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name             string   `json:"name"`
		Scopes           []string `json:"scopes"`
		BrandExternalIDs []string `json:"brand_external_ids"`
		ExpiresAt        *string  `json:"expires_at"`
	}
	if p := readJSON(r, &body, 0); p != nil {
		problem.Write(w, p)
		return
	}
	t, secret, p := a.CreateIngestToken(r.Context(), body.Name, body.Scopes, body.BrandExternalIDs, body.ExpiresAt)
	if p != nil {
		problem.Write(w, p)
		return
	}
	writeJSON(w, 201, map[string]any{"token": a.ingestTokenJSON(t), "secret": secret, "ingest_url": a.Cfg.PublicURL + "/obx/v1/ingest"})
}

func (a *App) adminIngestTokenRotate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := a.loadIngestToken(ctx, "id = ?", r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if t == nil || t.Status != "active" {
		failP(w, "not_found", "Token not found.")
		return
	}
	secret := secure.Token("obxi_")
	t.Prefix = secret[:12]
	if _, err := a.DB.Exec(ctx, `UPDATE ingest_tokens SET token_hash = ?, prefix = ? WHERE id = ?`, secure.HashToken(secret), t.Prefix, t.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"token": a.ingestTokenJSON(t), "secret": secret, "ingest_url": a.Cfg.PublicURL + "/obx/v1/ingest"})
}

func (a *App) adminIngestTokenRevoke(w http.ResponseWriter, r *http.Request) {
	res, err := a.DB.Exec(r.Context(), `UPDATE ingest_tokens SET status = 'revoked' WHERE id = ?`, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		failP(w, "not_found", "Token not found.")
		return
	}
	w.WriteHeader(204)
}

var _ = json.Marshal
