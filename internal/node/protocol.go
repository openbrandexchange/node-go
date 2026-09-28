package node

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/openbrandexchange/node-go/internal/blobstore"
	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/problem"
)

// Capabilities offered by this node.
func (a *App) capabilities(ctx context.Context) []string {
	c := []string{"changes", "products", "by_gtin", "renditions", "exports", "webhooks", "usage"}
	if a.Settings(ctx).Portal.AccessRequestsEnabled {
		c = append(c, "access_requests")
	}
	return append(c, "ingest", "ingest.server_renditions", "portal")
}

// logoURL is the public URL of the uploaded portal logo.
func (a *App) logoURL(s *Settings) *string {
	if s.LogoSHA == "" {
		return nil
	}
	u := a.Cfg.PublicURL + "/admin/api/public/logo?v=" + s.LogoSHA[:16]
	return &u
}

func (a *App) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if !a.limit(w, "disc:"+a.clientIP(r), 600, time.Minute) {
		return
	}
	ctx := r.Context()
	s := a.Settings(ctx)
	base := a.Cfg.PublicURL
	brands := []map[string]any{}
	rs, err := a.DB.Query(ctx, `SELECT id, rep FROM brands WHERE visible = 1 ORDER BY name, id`)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	for rs.Next() {
		var id, rep string
		if rs.Scan(&id, &rep) == nil {
			b := renderBrand(rep, "")
			brands = append(brands, map[string]any{"id": id, "name": b.Name, "logo_url": b.LogoURL})
		}
	}
	rs.Close()
	var accessURL *string
	if s.Portal.AccessRequestsEnabled {
		u := base + "/obx/v1/access-requests"
		accessURL = &u
	}
	portal := base + "/"
	ingest := base + "/obx/v1/ingest"
	name := s.Branding.DisplayName
	if name == "" {
		name = s.Company.Name
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, 200, map[string]any{
		"obx_version":  OBXVersion,
		"spec_version": SpecVersion,
		"node":         map[string]any{"software": Software, "version": Version},
		"publisher": map[string]any{
			"id": a.nodeID, "name": name, "logo_url": a.logoURL(s), "accent_color": s.Branding.AccentColor,
			"website": s.Company.Website, "contact_email": s.Company.ContactEmail, "welcome_text": s.Branding.WelcomeText,
		},
		"api_base":   base + "/obx/v1",
		"portal_url": portal,
		"auth": map[string]any{
			"token_endpoint":                        base + "/obx/v1/oauth/token",
			"scopes_supported":                      AllScopes,
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		},
		"access_requests_url": accessURL,
		"ingest_url":          ingest,
		"capabilities":        a.capabilities(ctx),
		"brands":              brands,
		"languages":           s.ContentLanguages(),
		"limits": map[string]any{
			"changes_page_max": 500, "products_page_max": 100, "gtin_lookup_max": 2000,
			"tombstone_retention_days": a.Cfg.ChangeRetentionDays,
		},
	})
}

func (a *App) handleGrant(w http.ResponseWriter, r *http.Request) {
	v, ok := a.protocolAuth(w, r, "")
	if !ok {
		return
	}
	ret, err := a.loadRetailer(r.Context(), a.DB, v.RetailerID)
	if err != nil || ret == nil {
		a.fail(w, r, errors.Join(err, errors.New("retailer missing")))
		return
	}
	writeJSON(w, 200, a.grantOf(r.Context(), ret))
}

func (a *App) handleBrands(w http.ResponseWriter, r *http.Request) {
	v, ok := a.protocolAuth(w, r, "catalog:read")
	if !ok {
		return
	}
	bf, args := v.brandFilterSQL("id")
	rs, err := a.DB.Query(r.Context(), `SELECT rep, updated_at FROM brands WHERE visible = 1`+bf+` ORDER BY id`, args...)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	defer rs.Close()
	items := []*Brand{}
	for rs.Next() {
		var rep, up string
		if err := rs.Scan(&rep, &up); err != nil {
			a.fail(w, r, err)
			return
		}
		items = append(items, renderBrand(rep, up))
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (a *App) handleBrand(w http.ResponseWriter, r *http.Request) {
	v, ok := a.protocolAuth(w, r, "catalog:read")
	if !ok {
		return
	}
	row, err := loadRowByID(r.Context(), a.DB, KindBrand, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if row == nil || !row.Visible || !v.SeesBrand(row.ID) {
		failP(w, "not_found", "Brand not found.")
		return
	}
	writeJSON(w, 200, renderBrand(row.Rep, row.UpdatedAt))
}

func (a *App) handleProducts(w http.ResponseWriter, r *http.Request) {
	v, ok := a.protocolAuth(w, r, "catalog:read")
	if !ok {
		return
	}
	limit, p := limitParam(r, 25, 100)
	if p != nil {
		problem.Write(w, p)
		return
	}
	q := r.URL.Query()
	where := `WHERE visible = 1`
	var args []any
	if c := q.Get("cursor"); c != "" {
		var cur struct {
			After string `json:"a"`
		}
		if !a.decodeCursor("products", c, &cur) {
			failP(w, "cursor_invalid", "The cursor is not valid.")
			return
		}
		where += ` AND id > ?`
		args = append(args, cur.After)
	}
	if ids := strings.TrimSpace(q.Get("ids")); ids != "" {
		list := strings.Split(ids, ",")
		if len(list) > 100 {
			failP(w, "limit_exceeded", "At most 100 ids.")
			return
		}
		where += ` AND id IN ` + db.In(len(list))
		for _, id := range list {
			args = append(args, strings.TrimSpace(id))
		}
	}
	if b := q.Get("brand_id"); b != "" {
		where += ` AND brand_id = ?`
		args = append(args, b)
	}
	if us := q.Get("updated_since"); us != "" {
		t, err := time.Parse(time.RFC3339Nano, us)
		if err != nil {
			failP(w, "invalid_request", "updated_since must be an RFC 3339 timestamp.")
			return
		}
		where += ` AND touched_at >= ?`
		args = append(args, db.Fmt(t))
	}
	bf, bargs := v.brandFilterSQL("brand_id")
	where += bf
	args = append(args, bargs...)
	rs, err := a.DB.Query(r.Context(), `SELECT id FROM products `+where+` ORDER BY id LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var idsOut []string
	for rs.Next() {
		var id string
		_ = rs.Scan(&id)
		idsOut = append(idsOut, id)
	}
	rs.Close()
	var next *string
	if len(idsOut) > limit {
		idsOut = idsOut[:limit]
		c := a.encodeCursor("products", map[string]string{"a": idsOut[limit-1]})
		next = &c
	}
	items, err := a.productDetails(r.Context(), v, idsOut)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}

func (a *App) handleProduct(w http.ResponseWriter, r *http.Request) {
	v, ok := a.protocolAuth(w, r, "catalog:read")
	if !ok {
		return
	}
	items, err := a.productDetails(r.Context(), v, []string{r.PathValue("id")})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if len(items) == 0 {
		failP(w, "not_found", "Product not found.")
		return
	}
	// ETag over the representation without volatile URLs.
	etag := `"` + productETag(items[0]) + `"`
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match != "" && (match == etag || match == "*") {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, 200, items[0])
}

func productETag(p *Product) string {
	c := *p
	var parts []string
	parts = append(parts, c.UpdatedAt)
	for _, v := range *c.Variants {
		parts = append(parts, v.ID+v.UpdatedAt)
	}
	for _, as := range *c.Assets {
		n := len(as.Renditions)
		parts = append(parts, as.ID+as.UpdatedAt+string(rune('0'+n)))
	}
	return hashHex([]byte(strings.Join(parts, "|")))[:32]
}

// GTIN lookup -------------------------------------------------------------------

type gtinResult struct {
	Input     string  `json:"input"`
	GTIN      *string `json:"gtin"`
	Status    string  `json:"status"`
	ProductID *string `json:"product_id"`
	VariantID *string `json:"variant_id"`
}

// lookupGTINs implements the by-GTIN matching rules for viewer v.
func (a *App) lookupGTINs(ctx context.Context, v *Viewer, inputs []string) (map[string]any, error) {
	results := []*gtinResult{}
	seen := map[string]bool{}
	var norms []any
	for _, in := range inputs {
		t := strings.TrimSpace(in)
		cleaned := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\t' || r == ' ' {
				return -1
			}
			return r
		}, t)
		res := &gtinResult{Input: t, Status: "invalid"}
		if n, ok := normalizeGTIN(cleaned); ok {
			if seen[n] {
				continue
			}
			seen[n] = true
			nn := n
			res.GTIN = &nn
			res.Status = "unmatched"
			norms = append(norms, n)
		} else {
			if seen["invalid:"+t] {
				continue
			}
			seen["invalid:"+t] = true
		}
		if len(res.Input) > 64 {
			res.Input = res.Input[:64]
		}
		results = append(results, res)
	}
	byGTIN := map[string][2]string{}
	for i := 0; i < len(norms); i += 500 {
		chunk := norms[i:min(i+500, len(norms))]
		bf, bargs := v.brandFilterSQL("v.brand_id")
		rs, err := a.DB.Query(ctx, `SELECT v.gtin, v.id, v.product_id FROM variants v WHERE v.visible = 1 AND v.gtin IN `+db.In(len(chunk))+bf, append(chunk, bargs...)...)
		if err != nil {
			return nil, err
		}
		for rs.Next() {
			var g, vid, pid string
			if rs.Scan(&g, &vid, &pid) == nil {
				byGTIN[g] = [2]string{vid, pid}
			}
		}
		rs.Close()
	}
	var productIDs []string
	pseen := map[string]bool{}
	for _, res := range results {
		if res.GTIN == nil {
			continue
		}
		if m, ok := byGTIN[*res.GTIN]; ok {
			vid, pid := m[0], m[1]
			res.Status, res.VariantID, res.ProductID = "matched", &vid, &pid
			if !pseen[pid] {
				pseen[pid] = true
				productIDs = append(productIDs, pid)
			}
		}
	}
	products, err := a.productDetails(ctx, v, productIDs)
	if err != nil {
		return nil, err
	}
	return map[string]any{"results": results, "products": products}, nil
}

func (a *App) handleByGTIN(w http.ResponseWriter, r *http.Request) {
	v, ok := a.protocolAuth(w, r, "catalog:read")
	if !ok {
		return
	}
	var inputs []string
	if r.Method == http.MethodGet {
		raw := r.URL.Query().Get("gtin")
		if strings.TrimSpace(raw) == "" {
			failP(w, "invalid_request", "Pass ?gtin=a,b,c.")
			return
		}
		inputs = strings.Split(raw, ",")
		if len(inputs) > 100 {
			failP(w, "limit_exceeded", "At most 100 GTINs with GET; use POST for up to 2000.")
			return
		}
	} else {
		var body struct {
			GTINs []any `json:"gtins"`
		}
		if p := readJSON(r, &body, 1<<20); p != nil {
			problem.Write(w, p)
			return
		}
		if body.GTINs == nil {
			failP(w, "invalid_request", "Send {\"gtins\": [...]}.")
			return
		}
		if len(body.GTINs) > 2000 {
			failP(w, "limit_exceeded", "At most 2000 GTINs per request.")
			return
		}
		for _, g := range body.GTINs {
			s, ok := g.(string)
			if !ok {
				if n, isNum := g.(json.Number); isNum {
					s = n.String()
				} else {
					failP(w, "invalid_request", "gtins must be strings.")
					return
				}
			}
			inputs = append(inputs, s)
		}
	}
	res, err := a.lookupGTINs(r.Context(), v, inputs)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, res)
}

// Assets and renditions ----------------------------------------------------------

func (a *App) handleAsset(w http.ResponseWriter, r *http.Request) {
	v, ok := a.protocolAuth(w, r, "media:read")
	if !ok {
		return
	}
	row, err := a.visibleAsset(r.Context(), v, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if row == nil {
		failP(w, "not_found", "Asset not found.")
		return
	}
	writeJSON(w, 200, a.renderAsset(row.Rep, row.UpdatedAt, v))
}

func (a *App) handleRendition(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")
	id := r.PathValue("id")
	var v *Viewer
	if bearer(r) == "" && r.URL.Query().Get("sig") != "" {
		tag, ok := a.verifySigned(r.URL.Path, r.URL.Query())
		if !ok {
			failP(w, "unauthorized", "This link is invalid or expired; read the asset again for a fresh link.")
			return
		}
		vv, p := a.viewerFromTag(ctx, tag)
		if p != nil {
			problem.Write(w, p)
			return
		}
		v = vv
		if v.Kind == "client" {
			if !a.limit(w, "api:"+v.ClientRowID, 3000, time.Minute) {
				return
			}
		}
	} else {
		vv, ok := a.protocolAuth(w, r, "media:read")
		if !ok {
			return
		}
		v = vv
	}
	switch name {
	case "original", "web", "square", "thumb":
	default:
		failP(w, "not_found", "Unknown rendition.")
		return
	}
	row, err := a.visibleAsset(ctx, v, id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if row == nil {
		failP(w, "not_found", "Asset not found.")
		return
	}
	if name == "original" && !v.OriginalAllowed {
		failP(w, "original_not_allowed", "Original files are not available to you.")
		return
	}
	as := a.renderAsset(row.Rep, row.UpdatedAt, &Viewer{Kind: "admin", OriginalAllowed: true})
	rend := as.Renditions[name]
	if rend == nil {
		failP(w, "not_found", "Rendition not found.")
		return
	}
	fname := as.Filename
	if name != "original" {
		fname = renditionFilename(as.Filename, name)
	}
	logged := v.Kind == "client" && r.Method == http.MethodGet && r.Header.Get("If-None-Match") == "" && !isContinuationRange(r)
	if err := a.serveBlob(w, r, rend.SHA256, rend.ContentType, contentDisposition("inline", fname)); err != nil {
		a.fail(w, r, err)
		return
	}
	if logged {
		a.logDownload(ctx, downloadEvent{Kind: "api_rendition", Viewer: v, Variant: name, ProductID: as.ProductID, AssetID: as.ID, Files: 1, Bytes: rend.Bytes})
	}
}

// renditionFilename inserts _web/_square/_thumb before a .jpg extension.
func renditionFilename(filename, name string) string {
	base := filename
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	return base + "_" + name + ".jpg"
}

func isContinuationRange(r *http.Request) bool {
	rg := r.Header.Get("Range")
	return rg != "" && !strings.HasPrefix(rg, "bytes=0-")
}

// serveBlob streams a stored blob with Range, ETag and checksum headers.
func (a *App) serveBlob(w http.ResponseWriter, r *http.Request, sha, contentType, disposition string) error {
	obj, err := a.Store.Open(r.Context(), blobstore.BlobKey(sha))
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			return problem.New("not_found", "File not found.")
		}
		return err
	}
	defer obj.Close()
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("ETag", `"`+sha+`"`)
	h.Set("OBX-Content-SHA256", sha)
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "private, max-age=3600")
	h.Set("X-Content-Type-Options", "nosniff")
	if disposition != "" {
		h.Set("Content-Disposition", disposition)
	}
	http.ServeContent(w, r, "", time.Time{}, obj)
	return nil
}
