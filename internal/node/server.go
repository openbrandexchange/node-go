package node

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"runtime/debug"
	"strings"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/problem"
)

// Handler returns the node's HTTP handler.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	a.routes(mux)
	var h http.Handler = mux
	h = a.middleware(mux, h)
	return a.logRequests(a.recoverPanics(h))
}

// recoverPanics turns a handler panic into a 500 problem.
func (a *App) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				a.Log.Error("panic", "method", r.Method, "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				problem.Write(w, problem.New("internal_error", "An unexpected error occurred."))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *App) routes(m *http.ServeMux) {
	// ---- OBX Protocol ----
	m.HandleFunc("GET /.well-known/obx", a.handleDiscovery)
	m.HandleFunc("OPTIONS /.well-known/obx", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET")
		w.WriteHeader(204)
	})
	m.HandleFunc("POST /obx/v1/access-requests", a.handleAccessRequestCreate)
	m.HandleFunc("GET /obx/v1/access-requests/{id}", a.handleAccessRequestStatus)
	m.HandleFunc("POST /obx/v1/oauth/token", a.handleToken)
	m.HandleFunc("GET /obx/v1/grant", a.handleGrant)
	m.HandleFunc("GET /obx/v1/changes", a.handleChanges)
	m.HandleFunc("GET /obx/v1/brands", a.handleBrands)
	m.HandleFunc("GET /obx/v1/brands/{id}", a.handleBrand)
	m.HandleFunc("GET /obx/v1/products", a.handleProducts)
	m.HandleFunc("GET /obx/v1/products/{id}", a.handleProduct)
	m.HandleFunc("GET /obx/v1/products:by-gtin", a.handleByGTIN)
	m.HandleFunc("POST /obx/v1/products:by-gtin", a.handleByGTIN)
	m.HandleFunc("GET /obx/v1/assets/{id}", a.handleAsset)
	m.HandleFunc("GET /obx/v1/assets/{id}/renditions/{name}", a.handleRendition)
	m.HandleFunc("POST /obx/v1/exports", a.handleExportCreate)
	m.HandleFunc("GET /obx/v1/exports/{id}", a.handleExportGet)
	m.HandleFunc("GET /obx/v1/exports/{id}/manifest", a.handleExportManifest)
	m.HandleFunc("GET /obx/v1/exports/{id}/parts/{number}", a.handleExportPart)
	m.HandleFunc("POST /obx/v1/usage", a.handleUsage)
	m.HandleFunc("GET /obx/v1/webhooks", a.handleWebhookList)
	m.HandleFunc("POST /obx/v1/webhooks", a.handleWebhookCreate)
	m.HandleFunc("DELETE /obx/v1/webhooks/{id}", a.handleWebhookDelete)
	m.HandleFunc("POST /obx/v1/webhooks/{id}/test", a.handleWebhookTest)

	// ---- OBX Ingest ----
	in := func(scope string, h func(http.ResponseWriter, *http.Request, *ingestToken)) http.HandlerFunc {
		return a.ingestAPI(scope, h)
	}
	m.HandleFunc("GET /obx/v1/ingest", in("", a.handleIngestInfo))
	m.HandleFunc("GET /obx/v1/ingest/{$}", in("", a.handleIngestInfo))
	for _, seg := range []string{"brands", "products", "variants", "assets"} {
		kind := kindFromPath(seg)
		base := "/obx/v1/ingest/" + seg + "/{external_id}"
		m.HandleFunc("GET "+base, in("ingest:read", a.handleRecordGet(kind)))
		m.HandleFunc("PUT "+base, in("ingest:write", a.handleIngestPut(kind)))
		m.HandleFunc("PATCH "+base, in("ingest:write", a.handleIngestPatch(kind)))
		m.HandleFunc("DELETE "+base, in("ingest:delete", a.handleIngestDelete(kind)))
	}
	m.HandleFunc("PUT /obx/v1/ingest/products/{external_id}/asset-order", in("ingest:write", func(w http.ResponseWriter, r *http.Request, t *ingestToken) {
		a.doAssetOrder(w, r, t.BrandExternalIDs)
	}))
	m.HandleFunc("PUT /obx/v1/ingest/assets/{external_id}/files/{rendition}", in("ingest:write", func(w http.ResponseWriter, r *http.Request, t *ingestToken) {
		a.putFile(w, r, t.BrandExternalIDs)
	}))
	m.HandleFunc("HEAD /obx/v1/ingest/blobs/{sha256}", in("ingest:read", func(w http.ResponseWriter, r *http.Request, _ *ingestToken) { a.headBlob(w, r) }))
	m.HandleFunc("PUT /obx/v1/ingest/blobs/{sha256}", in("ingest:write", func(w http.ResponseWriter, r *http.Request, _ *ingestToken) { a.putBlob(w, r) }))
	m.HandleFunc("POST /obx/v1/ingest/uploads", in("ingest:write", a.handleUploadCreate))
	m.HandleFunc("GET /obx/v1/ingest/uploads/{id}", in("ingest:read", a.handleUploadGet))
	m.HandleFunc("DELETE /obx/v1/ingest/uploads/{id}", in("ingest:write", a.handleUploadDelete))
	m.HandleFunc("PUT /obx/v1/ingest/uploads/{id}/parts/{number}", in("ingest:write", a.handleUploadPart))
	m.HandleFunc("POST /obx/v1/ingest/uploads/{id}/complete", in("ingest:write", a.handleUploadComplete))
	m.HandleFunc("POST /obx/v1/ingest/batch", in("ingest:write", a.handleBatch))
	m.HandleFunc("POST /obx/v1/ingest/imports", in("ingest:write", a.handleIngestImport))
	m.HandleFunc("GET /obx/v1/ingest/jobs", in("ingest:read", a.handleIngestJobs))
	m.HandleFunc("GET /obx/v1/ingest/jobs/{id}", in("ingest:read", a.handleIngestJob))
	m.HandleFunc("GET /obx/v1/ingest/jobs/{id}/results", in("ingest:read", a.handleIngestJobResults))

	// ---- Admin + portal ----
	pub := func(h http.HandlerFunc) http.HandlerFunc { return a.adminAPI("public", h) }
	adm := func(h http.HandlerFunc) http.HandlerFunc { return a.adminAPI("admin", h) }
	anyUser := func(h http.HandlerFunc) http.HandlerFunc { return a.adminAPI("any", h) }
	setup := func(h http.HandlerFunc) http.HandlerFunc { return a.adminAPI("setup", h) }
	m.HandleFunc("GET /admin/api/public/branding", setup(a.handlePublicBranding))
	m.HandleFunc("GET /admin/api/public/logo", setup(a.handlePublicLogo))
	m.HandleFunc("GET /admin/api/setup", setup(a.handleSetupGet))
	m.HandleFunc("POST /admin/api/setup", setup(a.handleSetup))
	m.HandleFunc("GET /admin/api/session", setup(a.handleSession))
	m.HandleFunc("POST /admin/api/session/login", pub(a.handleLogin))
	m.HandleFunc("POST /admin/api/session/logout", pub(a.handleLogout))
	m.HandleFunc("POST /admin/api/password/forgot", pub(a.handleForgot))
	m.HandleFunc("POST /admin/api/password/reset", pub(a.handleReset))
	m.HandleFunc("GET /admin/api/invites/{token}", pub(a.handleInviteGet))
	m.HandleFunc("POST /admin/api/invites/{token}/accept", pub(a.handleInviteAccept))
	m.HandleFunc("PATCH /admin/api/me", anyUser(a.handleMe))
	m.HandleFunc("GET /admin/api/files/{sha256}", func(w http.ResponseWriter, r *http.Request) { a.handleAdminFile(w, r) })

	m.HandleFunc("GET /admin/api/settings", adm(a.adminSettingsGet))
	m.HandleFunc("PATCH /admin/api/settings", adm(a.adminSettingsPatch))
	m.HandleFunc("PUT /admin/api/settings/logo", adm(a.adminLogoPut))
	m.HandleFunc("DELETE /admin/api/settings/logo", adm(a.adminLogoDelete))

	m.HandleFunc("GET /admin/api/catalog/brands", adm(a.adminBrands))
	m.HandleFunc("GET /admin/api/catalog/products", adm(a.adminProducts))
	m.HandleFunc("GET /admin/api/catalog/products/{external_id}", adm(a.adminProduct))
	for _, seg := range []string{"brands", "products", "variants", "assets"} {
		kind := kindFromPath(seg)
		base := "/admin/api/catalog/" + seg + "/{external_id}"
		m.HandleFunc("PUT "+base, adm(a.adminCatalogPut(kind)))
		m.HandleFunc("PATCH "+base, adm(a.adminCatalogPatch(kind)))
		m.HandleFunc("DELETE "+base, adm(a.adminCatalogDelete(kind)))
	}
	m.HandleFunc("PUT /admin/api/catalog/products/{external_id}/asset-order", adm(a.adminAssetOrder))
	m.HandleFunc("PUT /admin/api/catalog/assets/{external_id}/files/{rendition}", adm(a.adminPutFile))
	m.HandleFunc("HEAD /admin/api/catalog/blobs/{sha256}", adm(a.headBlob))
	m.HandleFunc("PUT /admin/api/catalog/blobs/{sha256}", adm(a.putBlob))

	m.HandleFunc("GET /admin/api/imports", adm(a.adminImports))
	m.HandleFunc("POST /admin/api/imports", adm(a.adminImportCreate))
	m.HandleFunc("GET /admin/api/imports/{id}", adm(a.adminImport))
	m.HandleFunc("GET /admin/api/imports/{id}/errors", adm(a.adminImportErrors))
	m.HandleFunc("GET /admin/api/import-template.csv", adm(a.adminImportTemplate))
	m.HandleFunc("GET /admin/api/import-sources", adm(a.adminSources))
	m.HandleFunc("POST /admin/api/import-sources", adm(a.adminSourceCreate))
	m.HandleFunc("GET /admin/api/import-sources/{id}", adm(a.adminSourceGet))
	m.HandleFunc("PATCH /admin/api/import-sources/{id}", adm(a.adminSourcePatch))
	m.HandleFunc("DELETE /admin/api/import-sources/{id}", adm(a.adminSourceDelete))
	m.HandleFunc("POST /admin/api/import-sources/{id}/run", adm(a.adminSourceRun))
	m.HandleFunc("POST /admin/api/import-sources/{id}/test", adm(a.adminSourceTest))

	m.HandleFunc("GET /admin/api/retailers", adm(a.adminRetailers))
	m.HandleFunc("POST /admin/api/retailers", adm(a.adminRetailerCreate))
	m.HandleFunc("GET /admin/api/retailers/{id}", adm(a.adminRetailerGet))
	m.HandleFunc("PATCH /admin/api/retailers/{id}", adm(a.adminRetailerPatch))
	m.HandleFunc("DELETE /admin/api/retailers/{id}", adm(a.adminRetailerDelete))
	m.HandleFunc("GET /admin/api/access-requests", adm(a.adminAccessRequests))
	m.HandleFunc("GET /admin/api/access-requests/{id}", adm(a.adminAccessRequest))
	m.HandleFunc("POST /admin/api/access-requests/{id}/approve", adm(a.adminAccessApprove))
	m.HandleFunc("POST /admin/api/access-requests/{id}/reject", adm(a.adminAccessReject))
	m.HandleFunc("GET /admin/api/users", adm(a.adminUsers))
	m.HandleFunc("POST /admin/api/users/invites", adm(a.adminUserInvite))
	m.HandleFunc("PATCH /admin/api/users/{id}", adm(a.adminUserPatch))
	m.HandleFunc("DELETE /admin/api/users/{id}", adm(a.adminUserDelete))
	m.HandleFunc("POST /admin/api/users/{id}/resend-invite", adm(a.adminUserResendInvite))
	m.HandleFunc("GET /admin/api/api-clients", adm(a.adminAPIClients))
	m.HandleFunc("POST /admin/api/api-clients", adm(a.adminAPIClientCreate))
	m.HandleFunc("POST /admin/api/api-clients/{id}/rotate", adm(a.adminAPIClientRotate))
	m.HandleFunc("DELETE /admin/api/api-clients/{id}", adm(a.adminAPIClientRevoke))
	m.HandleFunc("GET /admin/api/ingest-tokens", adm(a.adminIngestTokens))
	m.HandleFunc("POST /admin/api/ingest-tokens", adm(a.adminIngestTokenCreate))
	m.HandleFunc("POST /admin/api/ingest-tokens/{id}/rotate", adm(a.adminIngestTokenRotate))
	m.HandleFunc("DELETE /admin/api/ingest-tokens/{id}", adm(a.adminIngestTokenRevoke))
	m.HandleFunc("GET /admin/api/webhooks", adm(a.adminWebhooksList))
	m.HandleFunc("POST /admin/api/webhooks", adm(a.adminWebhookCreate))
	m.HandleFunc("PATCH /admin/api/webhooks/{id}", adm(a.adminWebhookPatch))
	m.HandleFunc("DELETE /admin/api/webhooks/{id}", adm(a.adminWebhookDelete))
	m.HandleFunc("POST /admin/api/webhooks/{id}/test", adm(a.adminWebhookTest))
	m.HandleFunc("GET /admin/api/webhooks/{id}/deliveries", adm(a.adminWebhookDeliveries))
	m.HandleFunc("GET /admin/api/stats", adm(a.handleStats))
	m.HandleFunc("GET /admin/api/stats/events", adm(a.handleStatsEvents))

	m.HandleFunc("GET /admin/api/portal/viewer", anyUser(a.handlePortalViewer))
	m.HandleFunc("GET /admin/api/portal/products", anyUser(a.handlePortalProducts))
	m.HandleFunc("GET /admin/api/portal/facets", anyUser(a.handlePortalFacets))
	m.HandleFunc("GET /admin/api/portal/products/data.csv", anyUser(a.handleProductsCSV))
	m.HandleFunc("GET /admin/api/portal/products/{id}", anyUser(a.handlePortalProduct))
	m.HandleFunc("GET /admin/api/portal/products/{id}/download.zip", anyUser(a.handlePortalProductZip))
	m.HandleFunc("POST /admin/api/portal/gtin-lookup", anyUser(a.handlePortalGTIN))
	m.HandleFunc("GET /admin/api/portal/selection", anyUser(a.handleSelectionGet))
	m.HandleFunc("PUT /admin/api/portal/selection", anyUser(a.handleSelectionPut))
	m.HandleFunc("DELETE /admin/api/portal/selection", anyUser(a.handleSelectionDelete))
	m.HandleFunc("GET /admin/api/portal/selection/download.zip", anyUser(a.handleSelectionZip))
	m.HandleFunc("GET /admin/api/portal/selection/data.csv", anyUser(a.handleSelectionCSV))
	m.HandleFunc("GET /admin/api/portal/assets/{id}/download", anyUser(a.handlePortalAssetDownload))
	m.HandleFunc("GET /admin/api/portal/download-jobs", anyUser(a.handleDownloadJobs))
	m.HandleFunc("POST /admin/api/portal/download-jobs", anyUser(a.handleDownloadJobCreate))
	m.HandleFunc("GET /admin/api/portal/download-jobs/{id}", anyUser(a.handleDownloadJob))
	m.HandleFunc("GET /admin/api/portal/download-jobs/{id}/file", anyUser(a.handleDownloadJobFile))
	m.HandleFunc("GET /admin/api/portal/api-clients", anyUser(a.handlePortalClients))
	m.HandleFunc("POST /admin/api/portal/api-clients", anyUser(a.handlePortalClientCreate))
	m.HandleFunc("POST /admin/api/portal/api-clients/{id}/rotate", anyUser(a.handlePortalClientRotate))
	m.HandleFunc("DELETE /admin/api/portal/api-clients/{id}", anyUser(a.handlePortalClientRevoke))

	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := a.DB.SQL.PingContext(r.Context()); err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})

	// Catch-alls: API paths answer problem+json; everything else is the UI.
	m.HandleFunc("/obx/", func(w http.ResponseWriter, r *http.Request) { a.apiFallback(m, w, r) })
	m.HandleFunc("/admin/api/", func(w http.ResponseWriter, r *http.Request) { a.apiFallback(m, w, r) })
	m.HandleFunc("/", a.serveUI)
}

// apiFallback distinguishes unknown paths (404) from wrong methods (405).
func (a *App) apiFallback(m *http.ServeMux, w http.ResponseWriter, r *http.Request) {
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"} {
		if method == r.Method {
			continue
		}
		probe := r.Clone(r.Context())
		probe.Method = method
		if _, pattern := m.Handler(probe); pattern != "" && pattern != "/obx/" && pattern != "/admin/api/" && pattern != "/" {
			failP(w, "method_not_allowed", "%s is not supported here.", r.Method)
			return
		}
	}
	failP(w, "not_found", "There is nothing at %s.", r.URL.Path)
}

// middleware adds security headers, OBX-Version and the setup gate.
func (a *App) middleware(_ *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		p := r.URL.Path
		isProtocol := strings.HasPrefix(p, "/obx/") || p == "/.well-known/obx"
		if isProtocol {
			h.Set("OBX-Version", OBXVersion)
			if !a.SetupDone(r.Context()) {
				problem.Write(w, problem.New("setup_required", "This node has not been set up yet.").WithStatus(503))
				return
			}
		}
		if !isProtocol && !strings.HasPrefix(p, "/admin/api/") {
			h.Set("X-Frame-Options", "DENY")
			h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' data: https://fonts.gstatic.com; img-src 'self' data: blob:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'; object-src 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

// serveUI serves the embedded admin-ui with SPA fallback.
func (a *App) serveUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		failP(w, "method_not_allowed", "Only GET is supported here.")
		return
	}
	if a.UI == nil {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name != "" && name != "index.html" {
		if st, err := fs.Stat(a.UI, name); err == nil && !st.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			http.ServeFileFS(w, r, a.UI, name)
			return
		}
		if path.Ext(name) != "" && !strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.NotFound(w, r)
			return
		}
	}
	b, err := fs.ReadFile(a.UI, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

// Start launches the background workers.
func (a *App) Start() {
	for i := 0; i < a.Cfg.Workers; i++ {
		a.wg.Add(1)
		go a.workerLoop()
	}
	a.wg.Add(3)
	go a.renderLoop()
	go a.webhookLoop()
	go a.housekeepingLoop()
	a.renderSweep(context.Background())
}

// Stop stops the background workers (running jobs finish their step).
func (a *App) Stop() {
	close(a.stop)
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
}

func (a *App) housekeepingLoop() {
	defer a.wg.Done()
	ctx := context.Background()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-a.stop:
			return
		case <-tick.C:
		}
		n++
		a.scheduleDue(ctx)
		a.expireAssets(ctx)
		if n%10 == 1 {
			a.renderSweep(ctx)
			a.cleanup(ctx)
		}
		if n%120 == 1 {
			a.pruneChanges(ctx)
		}
	}
}

// expireAssets writes tombstones for assets whose rights expired.
func (a *App) expireAssets(ctx context.Context) {
	rows, err := loadRows(ctx, a.DB, KindAsset, `WHERE visible = 1 AND expires_at IS NOT NULL AND expires_at <= ?`, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil || len(rows) == 0 {
		return
	}
	var due []*row
	for _, r := range rows {
		var as Asset
		_ = json.Unmarshal([]byte(r.Rep), &as)
		if expired(as.Rights.ExpiresAt, a.now()) {
			due = append(due, r)
		}
	}
	if len(due) == 0 {
		return
	}
	_ = a.DB.WriteTx(ctx, func(tx *db.Tx) error {
		w := a.newWriter(ctx, tx)
		for _, r := range due {
			if err := w.recompute(r); err != nil {
				return err
			}
		}
		return nil
	})
}

// cleanup removes expired sessions, uploads, exports, downloads and
// idempotency records.
func (a *App) cleanup(ctx context.Context) {
	now := a.nowStr()
	_, _ = a.DB.Exec(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now)
	_, _ = a.DB.Exec(ctx, `DELETE FROM idempotency WHERE created_at < ?`, db.Fmt(a.now().Add(-25*time.Hour)))
	_, _ = a.DB.Exec(ctx, `DELETE FROM webhook_deliveries WHERE created_at < ? AND status <> 'pending'`, db.Fmt(a.now().Add(-8*24*time.Hour)))
	rs, err := a.DB.Query(ctx, `SELECT id FROM uploads WHERE status = 'open' AND expires_at < ?`, now)
	if err == nil {
		var list []string
		for rs.Next() {
			var id string
			if rs.Scan(&id) == nil {
				list = append(list, id)
			}
		}
		rs.Close()
		for _, id := range list {
			_ = a.Store.DeletePrefix(ctx, "uploads/"+id+"/")
			_, _ = a.DB.Exec(ctx, `UPDATE uploads SET status = 'expired' WHERE id = ?`, id)
		}
	}
	rs, err = a.DB.Query(ctx, `SELECT id, kind FROM jobs WHERE kind IN ('export', 'download') AND expires_at IS NOT NULL AND expires_at < ? AND status = 'succeeded'`, now)
	if err == nil {
		type j struct{ id, kind string }
		var list []j
		for rs.Next() {
			var x j
			if rs.Scan(&x.id, &x.kind) == nil {
				list = append(list, x)
			}
		}
		rs.Close()
		for _, x := range list {
			if x.kind == "export" {
				_ = a.Store.DeletePrefix(ctx, "exports/"+x.id+"/")
			} else {
				_ = a.Store.Delete(ctx, "downloads/"+x.id+".zip")
			}
			_, _ = a.DB.Exec(ctx, `UPDATE jobs SET status = 'expired' WHERE id = ?`, x.id)
		}
	}
	// Jobs and their items are kept 30 days.
	old := db.Fmt(a.now().Add(-30 * 24 * time.Hour))
	_, _ = a.DB.Exec(ctx, `DELETE FROM job_items WHERE job_id IN (SELECT id FROM jobs WHERE created_at < ? AND kind IN ('batch', 'export', 'download'))`, old)
	_, _ = a.DB.Exec(ctx, `DELETE FROM jobs WHERE created_at < ? AND kind IN ('batch', 'export', 'download')`, old)
	// Download statistics keep IPs nowhere; events are kept for a year.
	_, _ = a.DB.Exec(ctx, `DELETE FROM download_events WHERE at < ?`, db.Fmt(a.now().AddDate(-1, 0, -7)))
}
