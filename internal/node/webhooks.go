package node

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/ids"
	"github.com/openbrandexchange/node-go/internal/problem"
	"github.com/openbrandexchange/node-go/internal/safehttp"
	"github.com/openbrandexchange/node-go/internal/secure"
)

var (
	clientEvents = map[string]bool{"changes.available": true, "grant.updated": true, "export.completed": true}
	brandEvents  = map[string]bool{"access_request.created": true, "import_run.completed": true, "ingest_job.completed": true}
)

type webhookRow struct {
	ID, OwnerType, URL, SecretEnc, Status, CreatedAt string
	OwnerID                                          *string
	Events                                           []string
	LastDelivery                                     *string
	LastStatus                                       *int
	FailingSince                                     *string
	LastNotified                                     int64
}

const webhookCols = `id, owner_type, owner_id, url, events, secret_enc, status, created_at, last_delivery_at, last_status, failing_since, last_notified_seq`

func scanWebhook(sc interface{ Scan(...any) error }) (*webhookRow, error) {
	h := &webhookRow{}
	var events string
	var ls sql.NullInt64
	err := sc.Scan(&h.ID, &h.OwnerType, &h.OwnerID, &h.URL, &events, &h.SecretEnc, &h.Status, &h.CreatedAt, &h.LastDelivery, &ls, &h.FailingSince, &h.LastNotified)
	if err != nil {
		return nil, err
	}
	h.Events = strings.Fields(events)
	if ls.Valid {
		v := int(ls.Int64)
		h.LastStatus = &v
	}
	return h, nil
}

func (h *webhookRow) json(secret *string) map[string]any {
	return map[string]any{"id": h.ID, "url": h.URL, "events": h.Events, "secret": secret, "status": h.Status,
		"created_at": h.CreatedAt, "last_delivery_at": h.LastDelivery, "last_status": h.LastStatus}
}

func (a *App) listWebhooks(ctx context.Context, ownerType string, ownerID *string) ([]*webhookRow, error) {
	q := `SELECT ` + webhookCols + ` FROM webhooks WHERE owner_type = ?`
	args := []any{ownerType}
	if ownerID != nil {
		q += ` AND owner_id = ?`
		args = append(args, *ownerID)
	}
	rs, err := a.DB.Query(ctx, q+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []*webhookRow
	for rs.Next() {
		h, err := scanWebhook(rs)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

func (a *App) loadWebhook(ctx context.Context, id, ownerType string, ownerID *string) (*webhookRow, error) {
	h, err := scanWebhook(a.DB.QueryRow(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE id = ?`, id))
	if db.NotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if h.OwnerType != ownerType || (ownerID != nil && (h.OwnerID == nil || *h.OwnerID != *ownerID)) {
		return nil, nil
	}
	return h, nil
}

// checkWebhookURL enforces https and the SSRF rules.
func (a *App) checkWebhookURL(ctx context.Context, u string) *problem.Problem {
	if len(u) > 2048 {
		return problem.New("validation_failed", "The URL is too long.")
	}
	if err := a.Policy.CheckURL(ctx, u, "https"); err != nil {
		return problem.New("unsafe_url", "%s", err.Error()).Field("/url", "unsafe_url", err.Error())
	}
	return nil
}

// createWebhook validates and stores a subscription; the secret is
// returned once.
func (a *App) createWebhook(ctx context.Context, ownerType string, ownerID *string, doc map[string]any, allowed map[string]bool, max int) (map[string]any, *problem.Problem) {
	var body struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}
	if err := decodeInto(doc, &body); err != nil || body.URL == "" || len(body.Events) == 0 {
		return nil, problem.New("validation_failed", "url and events are required.").Field("/url", "validation_failed", "required")
	}
	seen := map[string]bool{}
	for _, e := range body.Events {
		if !allowed[e] || seen[e] {
			return nil, problem.New("validation_failed", "Unknown or repeated event %q.", e).Field("/events", "validation_failed", "unknown event")
		}
		seen[e] = true
	}
	if p := a.checkWebhookURL(ctx, body.URL); p != nil {
		return nil, p
	}
	existing, err := a.listWebhooks(ctx, ownerType, ownerID)
	if err != nil {
		return nil, problem.As(err)
	}
	if max > 0 && len(existing) >= max {
		return nil, problem.New("conflict", "At most %d webhook subscriptions.", max)
	}
	secret := secure.Token("whsec_")
	enc, err := a.Keys.Encrypt([]byte(secret))
	if err != nil {
		return nil, problem.As(err)
	}
	seq, _ := a.maxSeq(ctx)
	h := &webhookRow{ID: ids.New("whk"), OwnerType: ownerType, OwnerID: ownerID, URL: body.URL, Events: body.Events, SecretEnc: enc, Status: "active", CreatedAt: a.nowStr(), LastNotified: seq}
	if _, err := a.DB.Exec(ctx, `INSERT INTO webhooks (id, owner_type, owner_id, url, events, secret_enc, status, created_at, last_notified_seq) VALUES (?, ?, ?, ?, ?, ?, 'active', ?, ?)`,
		h.ID, ownerType, ownerID, h.URL, strings.Join(h.Events, " "), enc, h.CreatedAt, seq); err != nil {
		return nil, problem.As(err)
	}
	return h.json(&secret), nil
}

// ---- protocol endpoints ---------------------------------------------------------------

func (a *App) handleWebhookList(w http.ResponseWriter, r *http.Request) {
	v, ok := a.protocolAuth(w, r, "catalog:read")
	if !ok {
		return
	}
	hooks, err := a.listWebhooks(r.Context(), "client", &v.ClientRowID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items := []any{}
	for _, h := range hooks {
		items = append(items, h.json(nil))
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (a *App) handleWebhookCreate(w http.ResponseWriter, r *http.Request) {
	v, ok := a.protocolAuth(w, r, "catalog:read")
	if !ok {
		return
	}
	doc, p := readJSONObject(r)
	if p != nil {
		problem.Write(w, p)
		return
	}
	out, p := a.createWebhook(r.Context(), "client", &v.ClientRowID, doc, clientEvents, 5)
	if p != nil {
		problem.Write(w, p)
		return
	}
	writeJSON(w, 201, out)
}

func (a *App) handleWebhookDelete(w http.ResponseWriter, r *http.Request) {
	v, ok := a.protocolAuth(w, r, "catalog:read")
	if !ok {
		return
	}
	h, err := a.loadWebhook(r.Context(), r.PathValue("id"), "client", &v.ClientRowID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if h == nil {
		failP(w, "not_found", "Webhook not found.")
		return
	}
	a.deleteWebhook(r.Context(), h.ID)
	w.WriteHeader(204)
}

func (a *App) deleteWebhook(ctx context.Context, id string) {
	_, _ = a.DB.Exec(ctx, `DELETE FROM webhooks WHERE id = ?`, id)
	_, _ = a.DB.Exec(ctx, `DELETE FROM webhook_deliveries WHERE webhook_id = ?`, id)
}

func (a *App) handleWebhookTest(w http.ResponseWriter, r *http.Request) {
	v, ok := a.protocolAuth(w, r, "catalog:read")
	if !ok {
		return
	}
	h, err := a.loadWebhook(r.Context(), r.PathValue("id"), "client", &v.ClientRowID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if h == nil {
		failP(w, "not_found", "Webhook not found.")
		return
	}
	d := a.deliverNow(r.Context(), h, "webhook.test", map[string]any{})
	writeJSON(w, 200, map[string]any{"delivered": d["success"], "status": d["status_code"]})
}

// ---- events and delivery ------------------------------------------------------------------

func (a *App) enqueueEvent(ctx context.Context, h *webhookRow, typ string, data any) {
	evt := map[string]any{"id": ids.New("evt"), "type": typ, "created_at": a.nowStr(), "data": data}
	_, err := a.DB.Exec(ctx, `INSERT INTO webhook_deliveries (id, webhook_id, event_id, event_type, payload, status, next_attempt_at, created_at) VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)`,
		ids.New("dlv"), h.ID, evt["id"], typ, string(mustJSON(evt)), a.nowStr(), a.nowStr())
	if err != nil {
		a.Log.Warn("webhook event not queued", "error", err)
	}
}

func (a *App) eventTo(ctx context.Context, ownerType string, ownerID *string, typ string, data any) {
	hooks, err := a.listWebhooks(ctx, ownerType, ownerID)
	if err != nil {
		return
	}
	for _, h := range hooks {
		if h.Status != "active" {
			continue
		}
		for _, e := range h.Events {
			if e == typ {
				a.enqueueEvent(ctx, h, typ, data)
			}
		}
	}
}

// brandEvent sends an event to the brand's own webhooks.
func (a *App) brandEvent(ctx context.Context, typ string, data any) {
	a.eventTo(ctx, "brand", nil, typ, data)
}

// clientEvent sends an event to one API client's webhooks.
func (a *App) clientEvent(ctx context.Context, clientRowID, typ string, data any) {
	a.eventTo(ctx, "client", &clientRowID, typ, data)
}

// retailerGrantEvent notifies all clients of a retailer that its grant changed.
func (a *App) retailerGrantEvent(ctx context.Context, retailerID string) {
	ret, err := a.loadRetailer(ctx, a.DB, retailerID)
	if err != nil || ret == nil {
		return
	}
	g := a.grantOf(ctx, ret)
	rs, err := a.DB.Query(ctx, `SELECT id FROM api_clients WHERE retailer_id = ? AND status = 'active'`, retailerID)
	if err != nil {
		return
	}
	var cids []string
	for rs.Next() {
		var id string
		if rs.Scan(&id) == nil {
			cids = append(cids, id)
		}
	}
	rs.Close()
	for _, c := range cids {
		a.clientEvent(ctx, c, "grant.updated", map[string]any{"grant": g})
	}
}

// notifyChanges enqueues changes.available for subscriptions that have
// relevant new entries.
func (a *App) notifyChanges(ctx context.Context) {
	rs, err := a.DB.Query(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE owner_type = 'client' AND status = 'active'`)
	if err != nil {
		return
	}
	var hooks []*webhookRow
	for rs.Next() {
		if h, err := scanWebhook(rs); err == nil {
			hooks = append(hooks, h)
		}
	}
	rs.Close()
	for _, h := range hooks {
		wants := false
		for _, e := range h.Events {
			if e == "changes.available" {
				wants = true
			}
		}
		if !wants || h.OwnerID == nil {
			continue
		}
		c, err := a.loadClient(ctx, a.DB, "id = ?", *h.OwnerID)
		if err != nil || c == nil {
			continue
		}
		v, p := a.viewerForClient(ctx, c, c.Scopes)
		if p != nil {
			continue
		}
		bf, bargs := v.brandFilterSQL("brand_id")
		var latest sql.NullInt64
		args := append([]any{h.LastNotified, v.RetailerID}, bargs...)
		q := `SELECT MAX(seq) FROM changes WHERE seq > ? AND (retailer_id = ? OR (brand_id IS NOT NULL` + bf + `))`
		if err := a.DB.QueryRow(ctx, q, args...).Scan(&latest); err != nil || !latest.Valid {
			continue
		}
		var pending int
		_ = a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM webhook_deliveries WHERE webhook_id = ? AND event_type = 'changes.available' AND status = 'pending' AND attempt = 0`, h.ID).Scan(&pending)
		if pending == 0 {
			a.enqueueEvent(ctx, h, "changes.available", map[string]any{"latest_seq": latest.Int64})
		}
		_, _ = a.DB.Exec(ctx, `UPDATE webhooks SET last_notified_seq = ? WHERE id = ?`, latest.Int64, h.ID)
	}
}

// deliverNow sends one event immediately and records the delivery.
func (a *App) deliverNow(ctx context.Context, h *webhookRow, typ string, data any) map[string]any {
	evt := map[string]any{"id": ids.New("evt"), "type": typ, "created_at": a.nowStr(), "data": data}
	payload := string(mustJSON(evt))
	id := ids.New("dlv")
	code, dur, derr := a.post(ctx, h, evt["id"].(string), payload)
	status := "delivered"
	var errText any
	if derr != nil {
		status = "failed"
		errText = derr.Error()
	}
	var codeV any
	if code > 0 {
		codeV = code
	}
	_, _ = a.DB.Exec(ctx, `INSERT INTO webhook_deliveries (id, webhook_id, event_id, event_type, payload, attempt, status, next_attempt_at, status_code, duration_ms, error, created_at, at) VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)`,
		id, h.ID, evt["id"], typ, payload, status, a.nowStr(), codeV, dur, errText, a.nowStr(), a.nowStr())
	a.recordHookStatus(ctx, h.ID, code, derr == nil)
	return deliveryJSON(id, evt["id"].(string), typ, a.nowStr(), codeV, dur, 1, derr == nil, errText)
}

func deliveryJSON(id, eventID, typ, at string, code any, dur int64, attempt int, ok bool, errText any) map[string]any {
	return map[string]any{"id": id, "event_id": eventID, "event_type": typ, "at": at, "status_code": code, "duration_ms": dur,
		"attempt": attempt, "success": ok, "error": errText}
}

// post delivers a signed payload (no redirects, SSRF-safe).
func (a *App) post(ctx context.Context, h *webhookRow, eventID, payload string) (int, int64, error) {
	secret, err := a.Keys.Decrypt(h.SecretEnc)
	if err != nil {
		return 0, 0, errors.New("webhook secret unreadable (was OBX_SECRET_KEY changed?)")
	}
	ts := strconv.FormatInt(a.now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader([]byte(payload)))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", Software+"/"+Version)
	req.Header.Set("OBX-Webhook-Id", eventID)
	req.Header.Set("OBX-Webhook-Timestamp", ts)
	req.Header.Set("OBX-Webhook-Signature", secure.WebhookSignature(string(secret), ts, []byte(payload)))
	client := a.Policy.Client(10*time.Second, 0, "https")
	start := time.Now()
	resp, err := client.Do(req)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		if errors.Is(err, safehttp.ErrUnsafe) {
			return 0, dur, fmt.Errorf("refused: the URL resolves to a private address")
		}
		return 0, dur, fmt.Errorf("request failed: %s", scrubErr(err))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, dur, fmt.Errorf("receiver answered HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, dur, nil
}

// scrubErr removes URLs (which may carry tokens) from error text.
func scrubErr(err error) string {
	s := err.Error()
	if i := strings.Index(s, "\""); i >= 0 {
		if j := strings.LastIndex(s, "\""); j > i {
			s = s[:i] + "<url>" + s[j+1:]
		}
	}
	return s
}

func (a *App) recordHookStatus(ctx context.Context, id string, code int, ok bool) {
	var codeV any
	if code > 0 {
		codeV = code
	}
	if ok {
		_, _ = a.DB.Exec(ctx, `UPDATE webhooks SET last_delivery_at = ?, last_status = ?, failing_since = NULL WHERE id = ?`, a.nowStr(), codeV, id)
		return
	}
	_, _ = a.DB.Exec(ctx, `UPDATE webhooks SET last_delivery_at = ?, last_status = ?, failing_since = COALESCE(failing_since, ?) WHERE id = ?`, a.nowStr(), codeV, a.nowStr(), id)
	_, _ = a.DB.Exec(ctx, `UPDATE webhooks SET status = 'disabled' WHERE id = ? AND failing_since IS NOT NULL AND failing_since < ?`, id, db.Fmt(a.now().Add(-72*time.Hour)))
}

// deliverDue sends pending deliveries whose time has come.
func (a *App) deliverDue(ctx context.Context) {
	rs, err := a.DB.Query(ctx, `SELECT d.id, d.webhook_id, d.event_id, d.event_type, d.payload, d.attempt, d.created_at FROM webhook_deliveries d WHERE d.status = 'pending' AND d.next_attempt_at <= ? ORDER BY d.next_attempt_at LIMIT 50`, a.nowStr())
	if err != nil {
		return
	}
	type due struct {
		id, hook, evt, typ, payload, created string
		attempt                              int
	}
	var list []due
	for rs.Next() {
		var d due
		if rs.Scan(&d.id, &d.hook, &d.evt, &d.typ, &d.payload, &d.attempt, &d.created) == nil {
			list = append(list, d)
		}
	}
	rs.Close()
	for _, d := range list {
		h, err := scanWebhook(a.DB.QueryRow(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE id = ?`, d.hook))
		if err != nil || h.Status != "active" {
			_, _ = a.DB.Exec(ctx, `UPDATE webhook_deliveries SET status = 'failed', error = 'subscription disabled or deleted' WHERE id = ?`, d.id)
			continue
		}
		code, dur, derr := a.post(ctx, h, d.evt, d.payload)
		attempt := d.attempt + 1
		var codeV any
		if code > 0 {
			codeV = code
		}
		if derr == nil {
			_, _ = a.DB.Exec(ctx, `UPDATE webhook_deliveries SET status = 'delivered', attempt = ?, status_code = ?, duration_ms = ?, error = NULL, at = ? WHERE id = ?`, attempt, codeV, dur, a.nowStr(), d.id)
		} else {
			backoff := time.Duration(1<<min(attempt, 12)) * 15 * time.Second
			if backoff > 6*time.Hour {
				backoff = 6 * time.Hour
			}
			next := a.now().Add(backoff)
			status := "pending"
			if next.After(db.Parse(d.created).Add(24 * time.Hour)) {
				status = "failed"
			}
			_, _ = a.DB.Exec(ctx, `UPDATE webhook_deliveries SET status = ?, attempt = ?, status_code = ?, duration_ms = ?, error = ?, at = ?, next_attempt_at = ? WHERE id = ?`,
				status, attempt, codeV, dur, derr.Error(), a.nowStr(), db.Fmt(next), d.id)
		}
		a.recordHookStatus(ctx, h.ID, code, derr == nil)
	}
}

// webhookLoop runs the notifier and the delivery queue.
func (a *App) webhookLoop() {
	defer a.wg.Done()
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	ctx := context.Background()
	for {
		select {
		case <-a.stop:
			return
		case <-a.notify:
			time.Sleep(500 * time.Millisecond)
			a.notifyChanges(ctx)
			a.deliverDue(ctx)
		case <-tick.C:
			a.deliverDue(ctx)
		}
	}
}

// ---- admin (brand) webhooks ------------------------------------------------------------

func (a *App) adminWebhooksList(w http.ResponseWriter, r *http.Request) {
	hooks, err := a.listWebhooks(r.Context(), "brand", nil)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items := []any{}
	for _, h := range hooks {
		items = append(items, h.json(nil))
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (a *App) adminWebhookCreate(w http.ResponseWriter, r *http.Request) {
	doc, p := readJSONObject(r)
	if p != nil {
		problem.Write(w, p)
		return
	}
	out, p := a.createWebhook(r.Context(), "brand", nil, doc, brandEvents, 20)
	if p != nil {
		problem.Write(w, p)
		return
	}
	writeJSON(w, 201, out)
}

func (a *App) adminWebhookPatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	h, err := a.loadWebhook(ctx, r.PathValue("id"), "brand", nil)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if h == nil {
		failP(w, "not_found", "Webhook not found.")
		return
	}
	var body struct {
		Events []string `json:"events"`
		Status string   `json:"status"`
	}
	if p := readJSON(r, &body, 0); p != nil {
		problem.Write(w, p)
		return
	}
	if body.Events != nil {
		for _, e := range body.Events {
			if !brandEvents[e] {
				failP(w, "validation_failed", "Unknown event %q.", e)
				return
			}
		}
		if len(body.Events) == 0 {
			failP(w, "validation_failed", "Choose at least one event.")
			return
		}
		h.Events = body.Events
	}
	switch body.Status {
	case "":
	case "active", "disabled":
		h.Status = body.Status
	default:
		failP(w, "validation_failed", "status must be active or disabled.")
		return
	}
	_, err = a.DB.Exec(ctx, `UPDATE webhooks SET events = ?, status = ?, failing_since = CASE WHEN ? = 'active' THEN NULL ELSE failing_since END WHERE id = ?`,
		strings.Join(h.Events, " "), h.Status, h.Status, h.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, h.json(nil))
}

func (a *App) adminWebhookDelete(w http.ResponseWriter, r *http.Request) {
	h, err := a.loadWebhook(r.Context(), r.PathValue("id"), "brand", nil)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if h == nil {
		failP(w, "not_found", "Webhook not found.")
		return
	}
	a.deleteWebhook(r.Context(), h.ID)
	w.WriteHeader(204)
}

func (a *App) adminWebhookTest(w http.ResponseWriter, r *http.Request) {
	h, err := a.loadWebhook(r.Context(), r.PathValue("id"), "brand", nil)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if h == nil {
		failP(w, "not_found", "Webhook not found.")
		return
	}
	writeJSON(w, 200, a.deliverNow(r.Context(), h, "webhook.test", map[string]any{}))
}

func (a *App) adminWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	h, err := a.loadWebhook(ctx, r.PathValue("id"), "brand", nil)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if h == nil {
		failP(w, "not_found", "Webhook not found.")
		return
	}
	limit, p := limitParam(r, 50, 200)
	if p != nil {
		problem.Write(w, p)
		return
	}
	where := `WHERE webhook_id = ? AND created_at > ?`
	args := []any{h.ID, db.Fmt(a.now().Add(-7 * 24 * time.Hour))}
	if c := r.URL.Query().Get("cursor"); c != "" {
		var cur struct {
			C string `json:"c"`
			I string `json:"i"`
		}
		if !a.decodeCursor("deliveries", c, &cur) {
			failP(w, "cursor_invalid", "The cursor is not valid.")
			return
		}
		where += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, cur.C, cur.C, cur.I)
	}
	rs, err := a.DB.Query(ctx, `SELECT id, event_id, event_type, created_at, at, status_code, duration_ms, attempt, status, error FROM webhook_deliveries `+where+` ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var items []map[string]any
	var keys [][2]string
	for rs.Next() {
		var id, evt, typ, created, status string
		var at, errText sql.NullString
		var code, dur sql.NullInt64
		var attempt int
		if rs.Scan(&id, &evt, &typ, &created, &at, &code, &dur, &attempt, &status, &errText) != nil {
			continue
		}
		when := created
		if at.Valid {
			when = at.String
		}
		var codeV, errV any
		if code.Valid {
			codeV = code.Int64
		}
		if errText.Valid {
			errV = errText.String
		}
		items = append(items, deliveryJSON(id, evt, typ, when, codeV, dur.Int64, attempt, status == "delivered", errV))
		keys = append(keys, [2]string{created, id})
	}
	rs.Close()
	var next *string
	if len(items) > limit {
		items = items[:limit]
		c := a.encodeCursor("deliveries", map[string]string{"c": keys[limit-1][0], "i": keys[limit-1][1]})
		next = &c
	}
	if items == nil {
		items = []map[string]any{}
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}

var _ = json.Marshal
