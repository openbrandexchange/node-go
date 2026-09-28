package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/ids"
	"github.com/openbrandexchange/node-go/internal/problem"
	"github.com/openbrandexchange/node-go/internal/secure"
)

type accessRequestRow struct {
	ID, Status, TokenHash, CreatedAt string
	Retailer, Contact                map[string]any
	Access                           []string
	RequestedBrandIDs                []string
	Message, DecidedAt, Reason       *string
	RetailerID, APIClientID          *string
	SecretEnc                        *string
	Claimed, Invited                 bool
}

func (a *App) loadAccessRequest(ctx context.Context, q db.Querier, id string) (*accessRequestRow, error) {
	r := &accessRequestRow{}
	var retailer, contact, access string
	var brands sql.NullString
	var claimed, invited int
	err := q.QueryRow(ctx, `SELECT id, status, token_hash, created_at, retailer, contact, access, requested_brand_ids, message, decided_at, reason, retailer_id, api_client_id, client_secret_enc, claimed, invited FROM access_requests WHERE id = ?`, id).
		Scan(&r.ID, &r.Status, &r.TokenHash, &r.CreatedAt, &retailer, &contact, &access, &brands, &r.Message, &r.DecidedAt, &r.Reason, &r.RetailerID, &r.APIClientID, &r.SecretEnc, &claimed, &invited)
	if db.NotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(retailer), &r.Retailer)
	_ = json.Unmarshal([]byte(contact), &r.Contact)
	_ = json.Unmarshal([]byte(access), &r.Access)
	if brands.Valid {
		_ = json.Unmarshal([]byte(brands.String), &r.RequestedBrandIDs)
	}
	r.Claimed, r.Invited = claimed == 1, invited == 1
	return r, nil
}

func (r *accessRequestRow) wants(what string) bool {
	for _, x := range r.Access {
		if x == what {
			return true
		}
	}
	return false
}

func strField(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func optField(m map[string]any, k string) any {
	if s, ok := m[k].(string); ok && s != "" {
		return s
	}
	return nil
}

// handleAccessRequestCreate is POST /obx/v1/access-requests.
func (a *App) handleAccessRequestCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !a.limit(w, "access-request:"+a.clientIP(r), 5, time.Hour) {
		return
	}
	if !a.Settings(ctx).Portal.AccessRequestsEnabled {
		failP(w, "forbidden", "This brand does not accept access requests.")
		return
	}
	doc, p := readJSONObject(r)
	if p != nil {
		problem.Write(w, p)
		return
	}
	if errs := lenientValidate("AccessRequestInput", doc); errs != nil {
		problem.Write(w, validationProblem(errs))
		return
	}
	retailer, _ := doc["retailer"].(map[string]any)
	contact, _ := doc["contact"].(map[string]any)
	access := []string{"portal"}
	if raw, ok := doc["access"].([]any); ok && len(raw) > 0 {
		access = nil
		for _, x := range raw {
			access = append(access, x.(string))
		}
	}
	var brands any
	if raw, ok := doc["requested_brand_ids"].([]any); ok {
		var list []string
		for _, x := range raw {
			if s, ok := x.(string); ok {
				list = append(list, s)
			}
		}
		brands = string(mustJSON(list))
	}
	token := secure.Token("obxr_")
	id := ids.New("acr")
	now := a.nowStr()
	cleanRet := map[string]any{"name": strings.TrimSpace(strField(retailer, "name")), "reference": optField(retailer, "reference"),
		"website": optField(retailer, "website"), "vat_number": optField(retailer, "vat_number"), "country": optField(retailer, "country")}
	cleanContact := map[string]any{"name": strings.TrimSpace(strField(contact, "name")), "email": normEmail(strField(contact, "email")),
		"phone": optField(contact, "phone"), "language": optField(contact, "language")}
	if cleanContact["language"] == nil {
		cleanContact["language"] = supportedLang(r.Header.Get("Accept-Language"))
	}
	var msg any
	if m, ok := doc["message"].(string); ok && strings.TrimSpace(m) != "" {
		msg = strings.TrimSpace(m)
	}
	if _, err := a.DB.Exec(ctx, `INSERT INTO access_requests (id, status, token_hash, retailer, contact, access, requested_brand_ids, message, created_at) VALUES (?, 'pending', ?, ?, ?, ?, ?, ?, ?)`,
		id, secure.HashToken(token), string(mustJSON(cleanRet)), string(mustJSON(cleanContact)), string(mustJSON(access)), brands, msg, now); err != nil {
		a.fail(w, r, err)
		return
	}
	go a.notifyAccessRequest(context.Background(), id, cleanRet["name"].(string), cleanContact["name"].(string))
	writeJSON(w, 202, map[string]any{"id": id, "status": "pending", "created_at": now, "decided_at": nil, "reason": nil,
		"request_token": token, "status_url": a.Cfg.PublicURL + "/obx/v1/access-requests/" + id, "portal": nil, "credentials": nil})
}

func (a *App) notifyAccessRequest(ctx context.Context, id, retailer, contact string) {
	a.brandEvent(ctx, "access_request.created", map[string]any{"access_request_id": id, "retailer_name": retailer})
	rs, err := a.DB.Query(ctx, `SELECT email, name, language FROM users WHERE role = 'admin' AND status = 'active'`)
	if err != nil {
		return
	}
	type adm struct{ email, name, lang string }
	var list []adm
	for rs.Next() {
		var x adm
		if rs.Scan(&x.email, &x.name, &x.lang) == nil {
			list = append(list, x)
		}
	}
	rs.Close()
	for _, x := range list {
		a.sendMail(ctx, x.email, x.lang, "access_request", map[string]string{"name": x.name, "retailer": retailer, "contact": contact,
			"link": a.Cfg.PublicURL + "/admin/access-requests/" + id})
	}
}

// handleAccessRequestStatus is GET /obx/v1/access-requests/{id}.
func (a *App) handleAccessRequestStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tok := bearer(r)
	if tok == "" {
		failP(w, "unauthorized", "Send the request_token as Bearer token.")
		return
	}
	if !a.limit(w, "access-status:"+r.PathValue("id"), 20, time.Minute) {
		return
	}
	ar, err := a.loadAccessRequest(ctx, a.DB, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if ar == nil || !secure.Equal(ar.TokenHash, secure.HashToken(tok)) {
		failP(w, "not_found", "Access request not found.")
		return
	}
	out := map[string]any{"id": ar.ID, "status": ar.Status, "created_at": ar.CreatedAt, "decided_at": ar.DecidedAt, "reason": ar.Reason,
		"status_url": a.Cfg.PublicURL + "/obx/v1/access-requests/" + ar.ID, "portal": nil, "credentials": nil}
	if ar.Status == "approved" {
		if ar.wants("portal") {
			out["portal"] = map[string]any{"invited": ar.Invited, "login_url": a.Cfg.PublicURL + "/login"}
		}
		if ar.APIClientID != nil {
			c, _ := a.loadClient(ctx, a.DB, "id = ?", *ar.APIClientID)
			if c != nil {
				var secret *string
				claimed := ar.Claimed
				if !ar.Claimed && ar.SecretEnc != nil {
					res, err := a.DB.Exec(ctx, `UPDATE access_requests SET claimed = 1, client_secret_enc = NULL WHERE id = ? AND claimed = 0`, ar.ID)
					if err == nil {
						if n, _ := res.RowsAffected(); n == 1 {
							if b, err := a.Keys.Decrypt(*ar.SecretEnc); err == nil {
								s := string(b)
								secret = &s
							}
						}
					}
					claimed = true
				}
				out["credentials"] = map[string]any{"client_id": c.ClientID, "client_secret": secret, "token_endpoint": a.Cfg.PublicURL + "/obx/v1/oauth/token",
					"scopes": c.Scopes, "claimed": claimed}
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, out)
}

// ---- admin side ----------------------------------------------------------------------------

func (a *App) adminAccessRequestJSON(ar *accessRequestRow) map[string]any {
	return map[string]any{"id": ar.ID, "status": ar.Status, "created_at": ar.CreatedAt, "decided_at": ar.DecidedAt, "reason": ar.Reason,
		"retailer": ar.Retailer, "contact": ar.Contact, "access": ar.Access, "requested_brand_ids": ar.RequestedBrandIDs,
		"message": ar.Message, "retailer_id": ar.RetailerID, "source_ip_country": nil}
}

func (a *App) adminAccessRequests(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, p := limitParam(r, 50, 200)
	if p != nil {
		problem.Write(w, p)
		return
	}
	where := `WHERE 1 = 1`
	var args []any
	if s := r.URL.Query().Get("status"); s != "" {
		where += ` AND status = ?`
		args = append(args, s)
	}
	if c := r.URL.Query().Get("cursor"); c != "" {
		var cur struct {
			C string `json:"c"`
			I string `json:"i"`
		}
		if !a.decodeCursor("access-requests", c, &cur) {
			failP(w, "cursor_invalid", "The cursor is not valid.")
			return
		}
		where += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, cur.C, cur.C, cur.I)
	}
	rs, err := a.DB.Query(ctx, `SELECT id FROM access_requests `+where+` ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var idsList []string
	for rs.Next() {
		var id string
		if rs.Scan(&id) == nil {
			idsList = append(idsList, id)
		}
	}
	rs.Close()
	items := []any{}
	var last *accessRequestRow
	for i, id := range idsList {
		if i == limit {
			break
		}
		ar, err := a.loadAccessRequest(ctx, a.DB, id)
		if err == nil && ar != nil {
			items = append(items, a.adminAccessRequestJSON(ar))
			last = ar
		}
	}
	var next *string
	if len(idsList) > limit && last != nil {
		c := a.encodeCursor("access-requests", map[string]string{"c": last.CreatedAt, "i": last.ID})
		next = &c
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}

func (a *App) adminAccessRequest(w http.ResponseWriter, r *http.Request) {
	ar, err := a.loadAccessRequest(r.Context(), a.DB, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if ar == nil {
		failP(w, "not_found", "Access request not found.")
		return
	}
	writeJSON(w, 200, a.adminAccessRequestJSON(ar))
}

func (a *App) adminAccessApprove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		RetailerID       *string  `json:"retailer_id"`
		BrandIDs         []string `json:"brand_ids"`
		InvitePortalUser *bool    `json:"invite_portal_user"`
		CreateAPIClient  *bool    `json:"create_api_client"`
	}
	raw := map[string]any{}
	if p := readOptionalJSON(r, &raw); p != nil {
		problem.Write(w, p)
		return
	}
	_ = decodeInto(raw, &body)
	ar, err := a.loadAccessRequest(ctx, a.DB, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if ar == nil {
		failP(w, "not_found", "Access request not found.")
		return
	}
	if ar.Status != "pending" {
		failP(w, "conflict", "This request was already %s.", ar.Status)
		return
	}
	invite := ar.wants("portal")
	if body.InvitePortalUser != nil {
		invite = *body.InvitePortalUser
	}
	createClient := ar.wants("api")
	if body.CreateAPIClient != nil {
		createClient = *body.CreateAPIClient
	}
	var brandIDs []string
	if _, given := raw["brand_ids"]; given {
		brandIDs = body.BrandIDs
	} else if len(ar.RequestedBrandIDs) > 0 {
		brandIDs = ar.RequestedBrandIDs
	}
	var retailerID string
	if body.RetailerID != nil {
		ret, _ := a.loadRetailer(ctx, a.DB, *body.RetailerID)
		if ret == nil {
			failP(w, "validation_failed", "Unknown retailer.")
			return
		}
		retailerID = ret.ID
	} else {
		in := map[string]any{"name": ar.Retailer["name"], "reference": ar.Retailer["reference"], "website": ar.Retailer["website"],
			"vat_number": ar.Retailer["vat_number"], "country": ar.Retailer["country"]}
		if brandIDs != nil {
			in["brand_ids"] = brandIDs
		}
		ret, p := a.createRetailer(ctx, in)
		if p != nil {
			problem.Write(w, p)
			return
		}
		retailerID = ret.ID
	}
	out := map[string]any{}
	contactEmail := strField(ar.Contact, "email")
	contactName := strField(ar.Contact, "name")
	lang := strField(ar.Contact, "language")
	invited := false
	if invite && contactEmail != "" {
		res, p := a.inviteUser(ctx, contactEmail, contactName, "retailer", &retailerID, lang)
		if p == nil {
			out["invite"] = res
			invited = true
		}
	}
	var secretEnc any
	var clientRowID any
	if createClient {
		c, secret, p := a.createAPIClient(ctx, retailerID, "API "+strField(ar.Retailer, "name"), nil, "access_request")
		if p != nil {
			problem.Write(w, p)
			return
		}
		enc, err := a.Keys.Encrypt([]byte(secret))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		secretEnc, clientRowID = enc, c.ID
		out["api_client"] = a.apiClientJSON(c)
	}
	res, err := a.DB.Exec(ctx, `UPDATE access_requests SET status = 'approved', decided_at = ?, retailer_id = ?, api_client_id = ?, client_secret_enc = ?, invited = ? WHERE id = ? AND status = 'pending'`,
		a.nowStr(), retailerID, clientRowID, secretEnc, db.Bool(invited), ar.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		failP(w, "conflict", "This request was decided meanwhile.")
		return
	}
	if !invited && contactEmail != "" {
		a.sendMail(ctx, contactEmail, lang, "access_approved", map[string]string{"name": contactName, "link": a.Cfg.PublicURL + "/login"})
	}
	ar, _ = a.loadAccessRequest(ctx, a.DB, ar.ID)
	ret, _ := a.loadRetailer(ctx, a.DB, retailerID)
	out["access_request"] = a.adminAccessRequestJSON(ar)
	out["retailer"] = a.retailerJSON(ctx, ret)
	writeJSON(w, 200, out)
}

func (a *App) adminAccessReject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Reason *string `json:"reason"`
	}
	if p := readOptionalJSON(r, &body); p != nil {
		problem.Write(w, p)
		return
	}
	if body.Reason != nil {
		s := strings.TrimSpace(*body.Reason)
		if len(s) > 2000 {
			failP(w, "validation_failed", "The reason can be at most 2000 characters.")
			return
		}
		if s == "" {
			body.Reason = nil
		} else {
			body.Reason = &s
		}
	}
	ar, err := a.loadAccessRequest(ctx, a.DB, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if ar == nil {
		failP(w, "not_found", "Access request not found.")
		return
	}
	res, err := a.DB.Exec(ctx, `UPDATE access_requests SET status = 'rejected', decided_at = ?, reason = ? WHERE id = ? AND status = 'pending'`, a.nowStr(), body.Reason, ar.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		failP(w, "conflict", "This request was already %s.", ar.Status)
		return
	}
	if email := strField(ar.Contact, "email"); email != "" {
		a.sendMail(ctx, email, strField(ar.Contact, "language"), "access_rejected", map[string]string{"name": strField(ar.Contact, "name"), "reason": deref(body.Reason)})
	}
	ar, _ = a.loadAccessRequest(ctx, a.DB, ar.ID)
	writeJSON(w, 200, a.adminAccessRequestJSON(ar))
}
