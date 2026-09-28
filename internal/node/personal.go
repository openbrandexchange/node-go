package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
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
		{"audit_log", `SELECT at, actor_email, action, target_type, target_id, status FROM audit_log WHERE actor_id IN (SELECT id FROM users WHERE retailer_id = ?) ORDER BY at`},
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
	u, err := a.queryMaps(ctx, `SELECT `+userExportCols+`, selection_updated_at FROM users WHERE id = ?`, userID)
	if err != nil || len(u) == 0 {
		return nil, err
	}
	out := map[string]any{"account": u[0]}
	email, _ := u[0]["email"].(string)
	parts := []struct {
		key, q string
		arg    any
	}{
		{"sessions", `SELECT created_at, last_seen_at, expires_at FROM sessions WHERE user_id = ? ORDER BY created_at`, userID},
		{"downloads", `SELECT id, status, created_at, finished_at, expires_at FROM jobs WHERE kind = 'download' AND owner_type = 'user' AND owner_id = ? ORDER BY created_at`, userID},
		{"download_events", `SELECT at, kind, retailer_name, variant, style_number, asset_id, files, bytes FROM download_events WHERE actor_id = ? ORDER BY at`, userID},
		{"audit_log", `SELECT at, action, target_type, target_id, status FROM audit_log WHERE actor_id = ? OR (target_type = 'user' AND target_id = ?) ORDER BY at`, userID},
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

func (a *App) writePersonalData(w http.ResponseWriter, name string, data map[string]any) {
	data["exported_at"] = a.nowStr()
	data["portal"] = a.Cfg.PublicURL
	data["note"] = "Everything this portal stores about you. Passwords, sessions and access keys are stored only as irreversible hashes and are not included."
	w.Header().Set("Content-Disposition", contentDisposition("attachment", name+"-"+a.now().Format("2006-01-02")+".json"))
	writeJSON(w, 200, data)
}

// handleRetailerPersonalData: GET /admin/api/retailers/{id}/personal-data.
func (a *App) handleRetailerPersonalData(w http.ResponseWriter, r *http.Request) {
	data, err := a.retailerPersonalData(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if data == nil {
		failP(w, "not_found", "Retailer not found.")
		return
	}
	a.audit(r.Context(), auditEntry{ActorID: sessionFrom(r.Context()).User.ID, ActorEmail: sessionFrom(r.Context()).User.Email,
		Action: "retailers.personal-data.export", TargetType: "retailer", TargetID: r.PathValue("id"), Status: 200})
	a.writePersonalData(w, "personal-data-"+r.PathValue("id"), data)
}

// handleMyPersonalData: GET /admin/api/me/personal-data (any logged-in user).
func (a *App) handleMyPersonalData(w http.ResponseWriter, r *http.Request) {
	s := sessionFrom(r.Context())
	data, err := a.userPersonalData(r.Context(), s.User.ID)
	if err != nil || data == nil {
		a.fail(w, r, fmt.Errorf("personal data export: %v", err))
		return
	}
	a.writePersonalData(w, "my-personal-data", data)
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
	data["exported_at"] = a.now().Format(time.RFC3339)
	return data, nil
}
