package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
)

// DevAccount is the first-run setup "node-go dev" completes.
type DevAccount struct {
	SetupCode, Company, Brand, Name, Email, Password string
}

// DevSetup completes the first-run setup through the setup wizard's own API
// (the same checks and defaults as a person in the browser), so "node-go
// dev" starts with a brand and an admin account. It does nothing when the
// node is set up already.
func (a *App) DevSetup(ctx context.Context, acc DevAccount) error {
	if a.SetupDone(ctx) {
		return nil
	}
	h := a.Handler()
	call := func(method, path string, body any, cookies []*http.Cookie, csrf string) (*httptest.ResponseRecorder, map[string]any) {
		var rd *bytes.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		} else {
			rd = bytes.NewReader(nil)
		}
		req := httptest.NewRequestWithContext(ctx, method, a.Cfg.PublicURL+path, rd)
		req.RemoteAddr = "127.0.0.1:1"
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out
	}
	rec, sess := call("GET", "/admin/api/session", nil, nil, "")
	csrf, _ := sess["csrf_token"].(string)
	if rec.Code != 200 || csrf == "" {
		return fmt.Errorf("setup: session: %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = call("POST", "/admin/api/setup", map[string]any{
		"setup_code": acc.SetupCode,
		"company":    map[string]any{"name": acc.Company, "first_brand_name": acc.Brand, "default_language": "en", "languages": []string{"en", "nl", "de", "fr"}},
		"admin":      map[string]any{"name": acc.Name, "email": acc.Email, "password": acc.Password},
	}, rec.Result().Cookies(), csrf)
	if rec.Code != 201 {
		return fmt.Errorf("setup: %d %s", rec.Code, rec.Body.String())
	}
	return nil
}
