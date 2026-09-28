package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Personal data export (GDPR articles 15 and 20). The export lists what the
// node stores about a retailer company and its people: accounts, API keys
// (never secrets), webhooks, access requests, download statistics and audit
// entries. Password hashes, session ids, one-time tokens and encrypted
// secrets are left out.

// jsonCols are TEXT columns that hold JSON.
var jsonCols = map[string]bool{"brand_ids": true, "scopes": true, "events": true, "access": true, "requested_brand_ids": true,
	"details": true, "contact": true, "retailer": true}

// queryMaps runs q and returns the rows as JSON-ready maps.
func (a *App) queryMaps(ctx context.Context, q string, args ...any) ([]map[string]any, error) {
	rs, err := a.DB.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := map[string]any{}
		for i, c := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			if s, ok := v.(string); ok && jsonCols[c] && (strings.HasPrefix(s, "[") || strings.HasPrefix(s, "{")) {
				var j any
				if json.Unmarshal([]byte(s), &j) == nil {
					v = j
				}
			}
			m[c] = v
		}
		out = append(out, m)
	}
	return out, rs.Err()
}

const userExportCols = `id, email, name, role, retailer_id, status, language, created_at, last_login_at, invite_expires_at`

// retailerPersonalData collects everything stored about one retailer.
func (a *App) retailerPersonalData(ctx context.Context, id string) (map[string]any, error) {
	ret, err := a.queryMaps(ctx, `SELECT id, name, reference, website, vat_number, country, notes, status, brand_ids, allow_original, created_at, updated_at, last_activity_at FROM retailers WHERE id = ?`, id)
	if err != nil || len(ret) == 0 {
		return nil, err
	}
	out := map[string]any{"retailer": ret[0]}
	parts := []struct{ key, q string }{
		{"users", `SELECT ` + userExportCols + ` FROM users WHERE retailer_id = ? ORDER BY created_at`},
		{"api_clients", `SELECT id, client_id, name, scopes, status, created_by, created_at, last_used_at FROM api_clients WHERE retailer_id = ? ORDER BY created_at`},
		{"webhooks", `SELECT id, url, events, status, created_at, last_delivery_at, last_status FROM webhooks WHERE owner_type = 'client' AND owner_id IN (SELECT id FROM api_clients WHERE retailer_id = ?) ORDER BY created_at`},
		{"access_requests", `SELECT id, status, retailer, contact, access, message, created_at, decided_at, reason FROM access_requests WHERE retailer_id = ? ORDER BY created_at`},
		{"download_events", `SELECT at, kind, actor_type, actor_id, actor_name, variant, style_number, asset_id, usage_event, files, bytes FROM download_events WHERE retailer_id = ? ORDER BY at`},
		{"audit_log", `SELECT at, actor_name, action, method, route, target_type, target_id, status FROM audit_log WHERE actor_id IN (SELECT id FROM users WHERE retailer_id = ?) ORDER BY at`},
	}
	for _, p := range parts {
		rows, err := a.queryMaps(ctx, p.q, id)
		if err != nil {
			return nil, err
		}
		out[p.key] = rows
	}
	return out, nil
}

// userPersonalData collects everything stored about one account.
func (a *App) userPersonalData(ctx context.Context, userID string) (map[string]any, error) {
	u, err := a.queryMaps(ctx, `SELECT `+userExportCols+`, selection, selection_updated_at FROM users WHERE id = ?`, userID)
	if err != nil || len(u) == 0 {
		return nil, err
	}
	out := map[string]any{"user": u[0], "retailer": nil}
	if rid, ok := u[0]["retailer_id"].(string); ok && rid != "" {
		if ret, err := a.queryMaps(ctx, `SELECT id, name FROM retailers WHERE id = ?`, rid); err == nil && len(ret) == 1 {
			out["retailer"] = ret[0]
		}
	}
	email, _ := u[0]["email"].(string)
	if sel, ok := u[0]["selection"].(string); ok && sel != "" {
		var v any
		if json.Unmarshal([]byte(sel), &v) == nil {
			u[0]["selection"] = v
		}
	}
	parts := []struct {
		key, q string
		arg    any
	}{
		{"sessions", `SELECT created_at, last_seen_at, expires_at FROM sessions WHERE user_id = ? ORDER BY created_at`, userID},
		{"downloads", `SELECT id, status, created_at, finished_at, expires_at FROM jobs WHERE kind = 'download' AND owner_type = 'user' AND owner_id = ? ORDER BY created_at`, userID},
		{"download_events", `SELECT at, kind, retailer_name, variant, style_number, asset_id, files, bytes FROM download_events WHERE actor_id = ? ORDER BY at`, userID},
		{"audit_log", `SELECT at, action, method, route, target_type, target_id, status FROM audit_log WHERE actor_id = ? OR (target_type = 'user' AND target_id = ?) ORDER BY at`, userID},
		{"access_requests", `SELECT id, status, retailer, contact, access, message, created_at, decided_at FROM access_requests WHERE contact LIKE ? ORDER BY created_at`, "%" + email + "%"},
	}
	for _, p := range parts {
		args := []any{p.arg}
		if p.key == "audit_log" {
			args = append(args, userID)
		}
		rows, err := a.queryMaps(ctx, p.q, args...)
		if err != nil {
			return nil, err
		}
		out[p.key] = rows
	}
	return out, nil
}

// exportFormat names the export document (the same in every OBX portal).
const exportFormat = "obx-personal-data-export/1"

func (a *App) exportEnvelope(ctx context.Context, data map[string]any) map[string]any {
	st := a.Settings(ctx)
	data["format"] = exportFormat
	data["exported_at"] = a.nowStr()
	data["controller"] = map[string]any{"name": st.Company.Name, "contact_email": st.Company.ContactEmail, "website": st.Company.Website}
	data["retention"] = st.Retention
	data["note"] = "Everything this portal stores about you. Passwords, sessions and access keys are stored only as irreversible hashes and are not included."
	return data
}

func (a *App) writePersonalData(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	w.Header().Set("Content-Disposition", contentDisposition("attachment", name+"-"+a.now().Format("2006-01-02")+".json"))
	writeJSON(w, 200, a.exportEnvelope(r.Context(), data))
}

// handleRetailerExport: GET /admin/api/retailers/{id}/export (admin).
func (a *App) handleRetailerExport(w http.ResponseWriter, r *http.Request) {
	data, err := a.retailerPersonalData(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if data == nil {
		failP(w, "not_found", "Retailer not found.")
		return
	}
	a.writePersonalData(w, r, "personal-data-"+r.PathValue("id"), data)
}

// handleUserExport: GET /admin/api/users/{id}/export (admin).
func (a *App) handleUserExport(w http.ResponseWriter, r *http.Request) {
	data, err := a.userPersonalData(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if data == nil {
		failP(w, "not_found", "Account not found.")
		return
	}
	a.writePersonalData(w, r, "personal-data-"+r.PathValue("id"), data)
}

// handleMyExport: GET /admin/api/me/export, "download my data" for anyone
// signed in.
func (a *App) handleMyExport(w http.ResponseWriter, r *http.Request) {
	s := sessionFrom(r.Context())
	data, err := a.userPersonalData(r.Context(), s.User.ID)
	if err != nil || data == nil {
		a.fail(w, r, fmt.Errorf("personal data export: %v", err))
		return
	}
	a.writePersonalData(w, r, "my-data", data)
}

// handleMeDelete: DELETE /admin/api/me. A retailer's person deletes their own
// account; brand administrators are removed by another administrator.
func (a *App) handleMeDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := sessionFrom(ctx).User
	if u.Role == "admin" {
		failP(w, "forbidden", "Administrators are removed by another administrator under Settings → Team.")
		return
	}
	a.eraseUser(ctx, u)
	a.setCookie(w, sessionCookie, "", -1)
	w.WriteHeader(204)
}

// eraseUser deletes a person: account, sessions, prepared downloads and
// pending e-mails; their name leaves the statistics and their e-mail address
// the audit log.
func (a *App) eraseUser(ctx context.Context, u *userRow) {
	a.endSessions(ctx, a.DB, u.ID, "")
	downloads := a.userDownloadIDs(ctx, `owner_id = ?`, u.ID)
	for _, q := range []string{
		`UPDATE download_events SET actor_id = NULL, actor_name = NULL WHERE actor_id = ?`,
		`UPDATE audit_log SET actor_name = NULL, actor_email = NULL WHERE actor_id = ?`,
		`DELETE FROM job_items WHERE job_id IN (SELECT id FROM jobs WHERE kind = 'download' AND owner_type = 'user' AND owner_id = ?)`,
		`DELETE FROM jobs WHERE kind = 'download' AND owner_type = 'user' AND owner_id = ?`,
	} {
		_, _ = a.DB.Exec(ctx, q, u.ID)
	}
	_, _ = a.DB.Exec(ctx, `DELETE FROM mail_outbox WHERE recipient = ?`, u.Email)
	_, _ = a.DB.Exec(ctx, `DELETE FROM users WHERE id = ?`, u.ID)
	a.deleteDownloadFiles(ctx, downloads)
}

// PersonalData exports a retailer (id) or an account (e-mail) for the CLI.
func (a *App) PersonalData(ctx context.Context, email, retailerID string) (map[string]any, error) {
	var data map[string]any
	var err error
	if retailerID != "" {
		data, err = a.retailerPersonalData(ctx, retailerID)
	} else {
		u, lerr := a.loadUser(ctx, a.DB, "email = ?", normEmail(email))
		if lerr != nil {
			return nil, lerr
		}
		if u == nil {
			return nil, fmt.Errorf("no account with e-mail %s", email)
		}
		data, err = a.userPersonalData(ctx, u.ID)
	}
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, fmt.Errorf("retailer %s not found", retailerID)
	}
	return a.exportEnvelope(ctx, data), nil
}
