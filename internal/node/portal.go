package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/problem"
)

const (
	selectionMax    = 1000
	syncZipMaxFiles = 400
)

// portalViewer returns the viewer of the logged-in user.
func (a *App) portalViewer(w http.ResponseWriter, r *http.Request) (*Viewer, bool) {
	s := sessionFrom(r.Context())
	v, p := a.viewerForUser(r.Context(), s.User)
	if p != nil {
		problem.Write(w, p)
		return nil, false
	}
	if !a.limit(w, "portal:"+s.User.ID, 1200, time.Minute) {
		return nil, false
	}
	return v, true
}

func (a *App) handlePortalViewer(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalViewer(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	s := a.Settings(ctx)
	bf, bargs := v.brandFilterSQL("id")
	rs, err := a.DB.Query(ctx, `SELECT rep, updated_at FROM brands WHERE visible = 1`+bf+` ORDER BY name`, bargs...)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	brands := []*Brand{}
	for rs.Next() {
		var rep, up string
		if rs.Scan(&rep, &up) == nil {
			brands = append(brands, renderBrand(rep, up))
		}
	}
	rs.Close()
	var retailer any
	if v.RetailerID != "" {
		retailer = map[string]any{"id": v.RetailerID, "name": v.RetailerName}
	}
	sel := a.selectionIDs(ctx, sessionFrom(ctx).User.ID)
	writeJSON(w, 200, map[string]any{
		"preview":  v.Preview(),
		"retailer": retailer,
		"brands":   brands,
		"permissions": map[string]any{"original_download": v.OriginalAllowed && (v.Preview() || s.Portal.AllowOriginalDownload),
			"article_data_export": s.Portal.AllowArticleDataExport, "api_self_service": s.Portal.RetailerAPISelfService && !v.Preview()},
		"branding":        a.publicBranding(ctx),
		"languages":       s.ContentLanguages(),
		"selection_count": len(sel),
		"limits":          map[string]any{"selection_max_products": selectionMax, "sync_zip_max_images": syncZipMaxFiles, "gtin_lookup_max": 2000},
	})
}

// portalFilter is the product filter of the portal.
type portalFilter struct {
	Q            string   `json:"q"`
	BrandID      []string `json:"brand_id"`
	ItemType     []string `json:"item_type"`
	Color        []string `json:"color"`
	Season       []string `json:"season"`
	Collection   []string `json:"collection"`
	TargetGroup  []string `json:"target_group"`
	UpdatedSince string   `json:"updated_since"`
}

func filterFromQuery(q url.Values) portalFilter {
	multi := func(k string) []string {
		var out []string
		for _, v := range q[k] {
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, p)
				}
			}
		}
		for _, v := range q[k+"[]"] {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	return portalFilter{Q: strings.TrimSpace(q.Get("q")), BrandID: multi("brand_id"), ItemType: multi("item_type"), Color: multi("color"),
		Season: multi("season"), Collection: multi("collection"), TargetGroup: multi("target_group"), UpdatedSince: q.Get("updated_since")}
}

// where builds the SQL for f; skip names a dimension to leave out (facets).
func (f portalFilter) where(v *Viewer, skip string) (string, []any) {
	w := ` WHERE p.visible = 1`
	var args []any
	bf, bargs := v.brandFilterSQL("p.brand_id")
	w += bf
	args = append(args, bargs...)
	if f.Q != "" {
		w += ` AND (p.search_text LIKE ?)`
		args = append(args, "%"+strings.ToLower(f.Q)+"%")
	}
	in := func(col string, vals []string, dim string) {
		if len(vals) == 0 || dim == skip {
			return
		}
		w += ` AND ` + col + ` IN ` + db.In(len(vals))
		for _, x := range vals {
			args = append(args, x)
		}
	}
	in("p.brand_id", f.BrandID, "brand")
	in("p.item_type", f.ItemType, "item_type")
	in("p.season", f.Season, "season")
	in("p.collection", f.Collection, "collection")
	in("p.target_group", f.TargetGroup, "target_group")
	if len(f.Color) > 0 && skip != "color" {
		w += ` AND EXISTS (SELECT 1 FROM variants v WHERE v.product_id = p.id AND v.visible = 1 AND v.color_code IN ` + db.In(len(f.Color)) + `)`
		for _, x := range f.Color {
			args = append(args, x)
		}
	}
	if f.UpdatedSince != "" {
		if t, err := time.Parse(time.RFC3339Nano, f.UpdatedSince); err == nil {
			w += ` AND p.touched_at >= ?`
			args = append(args, db.Fmt(t))
		}
	}
	return w, args
}

func (a *App) handlePortalProducts(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalViewer(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	limit, p := limitParam(r, 50, 200)
	if p != nil {
		problem.Write(w, p)
		return
	}
	f := filterFromQuery(r.URL.Query())
	where, args := f.where(v, "")
	offset := 0
	if c := r.URL.Query().Get("cursor"); c != "" {
		var cur struct {
			O int `json:"o"`
		}
		if !a.decodeCursor("portal-products", c, &cur) {
			failP(w, "cursor_invalid", "The cursor is not valid.")
			return
		}
		offset = cur.O
	}
	var total int
	if err := a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM products p`+where, args...).Scan(&total); err != nil {
		a.fail(w, r, err)
		return
	}
	rs, err := a.DB.Query(ctx, `SELECT p.id, p.rep, p.updated_at, p.touched_at, p.brand_id FROM products p`+where+` ORDER BY p.style_number, p.id LIMIT ? OFFSET ?`, append(args, limit+1, offset)...)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	type prow struct{ id, rep, up, touched, brand string }
	var list []prow
	for rs.Next() {
		var x prow
		if rs.Scan(&x.id, &x.rep, &x.up, &x.touched, &x.brand) == nil {
			list = append(list, x)
		}
	}
	rs.Close()
	var next *string
	if len(list) > limit {
		list = list[:limit]
		c := a.encodeCursor("portal-products", map[string]int{"o": offset + limit})
		next = &c
	}
	brandNames := a.brandNames(ctx)
	items := []any{}
	for _, x := range list {
		p := renderProduct(x.rep, x.up)
		items = append(items, a.productSummary(ctx, v, p, brandNames[x.brand], x.touched))
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next, "total": total})
}

func (a *App) brandNames(ctx context.Context) map[string]string {
	out := map[string]string{}
	rs, err := a.DB.Query(ctx, `SELECT id, name FROM brands`)
	if err != nil {
		return out
	}
	defer rs.Close()
	for rs.Next() {
		var id, n string
		if rs.Scan(&id, &n) == nil {
			out[id] = n
		}
	}
	return out
}

// productSummary builds PortalProductSummary.
func (a *App) productSummary(ctx context.Context, v *Viewer, p *Product, brandName, touched string) map[string]any {
	colors := []map[string]any{}
	colorIdx := map[string]int{}
	variantCount := 0
	rs, err := a.DB.Query(ctx, `SELECT rep FROM variants WHERE product_id = ? AND visible = 1 ORDER BY external_id`, p.ID)
	if err == nil {
		for rs.Next() {
			var rep string
			if rs.Scan(&rep) != nil {
				continue
			}
			variantCount++
			vv := renderVariant(rep, "")
			if _, ok := colorIdx[vv.Color.Code]; !ok {
				colorIdx[vv.Color.Code] = len(colors)
				colors = append(colors, map[string]any{"code": vv.Color.Code, "name": vv.Color.Name, "thumb_url": nil})
			}
		}
		rs.Close()
	}
	var thumb *string
	images := 0
	ar, err := a.DB.Query(ctx, `SELECT id, color_code, position, expires_at FROM assets WHERE product_id = ? AND visible = 1`+assetOrder, p.ID)
	if err == nil {
		type arow struct {
			id    string
			color *string
			pos   int
		}
		var list []arow
		for ar.Next() {
			var x arow
			var exp *string
			if ar.Scan(&x.id, &x.color, &x.pos, &exp) == nil && a.assetVisibleNow(exp) {
				list = append(list, x)
			}
		}
		ar.Close()
		images = len(list)
		for _, x := range list {
			u, _ := a.signedURL("/obx/v1/assets/"+x.id+"/renditions/thumb", v.tag())
			if thumb == nil {
				uu := u
				thumb = &uu
			}
			if x.color != nil {
				if i, ok := colorIdx[*x.color]; ok && colors[i]["thumb_url"] == nil {
					colors[i]["thumb_url"] = u
				}
			}
		}
	}
	updated := p.UpdatedAt
	if touched > updated {
		updated = touched
	}
	return map[string]any{"id": p.ID, "style_number": p.StyleNumber, "name": p.Name, "brand": map[string]any{"id": p.BrandID, "name": brandName},
		"item_type": p.ItemType, "target_group": p.TargetGroup, "season": p.Season, "collection": p.Collection, "thumb_url": thumb,
		"colors": colors, "image_count": images, "variant_count": variantCount, "updated_at": updated}
}

func (a *App) handlePortalFacets(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalViewer(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	f := filterFromQuery(r.URL.Query())
	facet := func(col, dim string) []map[string]any {
		where, args := f.where(v, dim)
		out := []map[string]any{}
		rs, err := a.DB.Query(ctx, `SELECT `+col+`, COUNT(*) FROM products p`+where+` AND `+col+` IS NOT NULL GROUP BY `+col+` ORDER BY `+col, args...)
		if err != nil {
			return out
		}
		defer rs.Close()
		for rs.Next() {
			var val string
			var n int
			if rs.Scan(&val, &n) == nil {
				out = append(out, map[string]any{"value": val, "count": n})
			}
		}
		return out
	}
	brands := facet("p.brand_id", "brand")
	names := a.brandNames(ctx)
	for _, b := range brands {
		b["label"] = names[b["value"].(string)]
	}
	sort.SliceStable(brands, func(i, j int) bool { return brands[i]["label"].(string) < brands[j]["label"].(string) })
	where, args := f.where(v, "color")
	colors := []map[string]any{}
	rs, err := a.DB.Query(ctx, `SELECT v.color_code, COUNT(DISTINCT p.id), MIN(v.rep) FROM products p JOIN variants v ON v.product_id = p.id AND v.visible = 1`+where+` GROUP BY v.color_code ORDER BY v.color_code`, args...)
	if err == nil {
		for rs.Next() {
			var code, rep string
			var n int
			if rs.Scan(&code, &n, &rep) == nil {
				vv := renderVariant(rep, "")
				item := map[string]any{"value": code, "count": n}
				if vv.Color.Name != nil {
					item["label"] = vv.Color.Name
				}
				colors = append(colors, item)
			}
		}
		rs.Close()
	}
	writeJSON(w, 200, map[string]any{"brands": brands, "item_types": facet("p.item_type", "item_type"), "colors": colors,
		"seasons": facet("p.season", "season"), "collections": facet("p.collection", "collection"), "target_groups": facet("p.target_group", "target_group")})
}

func (a *App) handlePortalProduct(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalViewer(w, r)
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
	writeJSON(w, 200, items[0])
}

var nonDigits = regexp.MustCompile(`[^0-9]+`)

func (a *App) handlePortalGTIN(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalViewer(w, r)
	if !ok {
		return
	}
	var body struct {
		Text  *string  `json:"text"`
		GTINs []string `json:"gtins"`
	}
	if p := readJSON(r, &body, 1<<20); p != nil {
		problem.Write(w, p)
		return
	}
	var inputs []string
	if body.Text != nil {
		if len(*body.Text) > 100000 {
			failP(w, "limit_exceeded", "Paste at most 100,000 characters.")
			return
		}
		for _, s := range nonDigits.Split(*body.Text, -1) {
			if s != "" {
				inputs = append(inputs, s)
			}
		}
	}
	inputs = append(inputs, body.GTINs...)
	if len(inputs) > 2000 {
		failP(w, "limit_exceeded", "Paste at most 2000 EANs at once.")
		return
	}
	res, err := a.lookupGTINs(r.Context(), v, inputs)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, res)
}

// ---- selection -------------------------------------------------------------------------

func (a *App) selectionIDs(ctx context.Context, userID string) []string {
	var sel *string
	_ = a.DB.QueryRow(ctx, `SELECT selection FROM users WHERE id = ?`, userID).Scan(&sel)
	var ids []string
	if sel != nil {
		_ = json.Unmarshal([]byte(*sel), &ids)
	}
	return ids
}

// visibleProductIDs keeps the ids visible to v (in order).
func (a *App) visibleProductIDs(ctx context.Context, v *Viewer, ids []string) []string {
	if len(ids) == 0 {
		return []string{}
	}
	ok := map[string]bool{}
	for i := 0; i < len(ids); i += 500 {
		chunk := ids[i:min(i+500, len(ids))]
		args := make([]any, len(chunk))
		for j, id := range chunk {
			args[j] = id
		}
		bf, bargs := v.brandFilterSQL("brand_id")
		rs, err := a.DB.Query(ctx, `SELECT id FROM products WHERE visible = 1 AND id IN `+db.In(len(args))+bf, append(args, bargs...)...)
		if err != nil {
			continue
		}
		for rs.Next() {
			var id string
			if rs.Scan(&id) == nil {
				ok[id] = true
			}
		}
		rs.Close()
	}
	out := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if ok[id] && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func (a *App) countImages(ctx context.Context, productIDs []string) int {
	total := 0
	for i := 0; i < len(productIDs); i += 500 {
		chunk := productIDs[i:min(i+500, len(productIDs))]
		args := make([]any, len(chunk))
		for j, id := range chunk {
			args[j] = id
		}
		var n int
		_ = a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM assets WHERE visible = 1 AND product_id IN `+db.In(len(args)), args...).Scan(&n)
		total += n
	}
	return total
}

func (a *App) selectionJSON(ctx context.Context, v *Viewer, userID string) map[string]any {
	ids := a.visibleProductIDs(ctx, v, a.selectionIDs(ctx, userID))
	var up *string
	_ = a.DB.QueryRow(ctx, `SELECT selection_updated_at FROM users WHERE id = ?`, userID).Scan(&up)
	return map[string]any{"product_ids": ids, "image_count": a.countImages(ctx, ids), "updated_at": up}
}

func (a *App) handleSelectionGet(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalViewer(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, a.selectionJSON(r.Context(), v, sessionFrom(r.Context()).User.ID))
}

func (a *App) handleSelectionPut(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalViewer(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var body struct {
		ProductIDs []string `json:"product_ids"`
	}
	if p := readJSON(r, &body, 1<<20); p != nil {
		problem.Write(w, p)
		return
	}
	if body.ProductIDs == nil {
		failP(w, "invalid_request", "Send {\"product_ids\": [...]}.")
		return
	}
	if len(body.ProductIDs) > selectionMax {
		failP(w, "limit_exceeded", "A selection can hold at most %d products.", selectionMax)
		return
	}
	ids := a.visibleProductIDs(ctx, v, body.ProductIDs)
	uid := sessionFrom(ctx).User.ID
	if _, err := a.DB.Exec(ctx, `UPDATE users SET selection = ?, selection_updated_at = ? WHERE id = ?`, string(mustJSON(ids)), a.nowStr(), uid); err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, a.selectionJSON(ctx, v, uid))
}

func (a *App) handleSelectionDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.portalViewer(w, r); !ok {
		return
	}
	_, _ = a.DB.Exec(r.Context(), `UPDATE users SET selection = NULL, selection_updated_at = ? WHERE id = ?`, a.nowStr(), sessionFrom(r.Context()).User.ID)
	w.WriteHeader(204)
}

// ---- portal API clients (self-service) ----------------------------------------------------

func (a *App) portalRetailer(w http.ResponseWriter, r *http.Request) (*Viewer, bool) {
	v, ok := a.portalViewer(w, r)
	if !ok {
		return nil, false
	}
	if v.RetailerID == "" {
		failP(w, "forbidden", "Only retailer accounts have their own API keys.")
		return nil, false
	}
	return v, true
}

func (a *App) handlePortalClients(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalRetailer(w, r)
	if !ok {
		return
	}
	a.listClients(w, r, v.RetailerID)
}

func (a *App) handlePortalClientCreate(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalRetailer(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if !a.Settings(ctx).Portal.RetailerAPISelfService {
		failP(w, "forbidden", "Ask the brand to create an API key for you.")
		return
	}
	var body struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if p := readJSON(r, &body, 0); p != nil {
		problem.Write(w, p)
		return
	}
	var n int
	_ = a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM api_clients WHERE retailer_id = ? AND status = 'active'`, v.RetailerID).Scan(&n)
	if n >= 5 {
		failP(w, "conflict", "You can have at most 5 active API keys; revoke one first.")
		return
	}
	ret, _ := a.loadRetailer(ctx, a.DB, v.RetailerID)
	g := a.grantOf(ctx, ret)
	allowed := map[string]bool{}
	for _, s := range g.Scopes {
		allowed[s] = true
	}
	scopes := body.Scopes
	if scopes == nil {
		scopes = g.Scopes
	}
	for _, s := range scopes {
		if !allowed[s] {
			failP(w, "validation_failed", "Scope %s is not available to you.", s)
			return
		}
	}
	c, secret, p := a.createAPIClient(ctx, v.RetailerID, body.Name, scopes, "retailer")
	if p != nil {
		problem.Write(w, p)
		return
	}
	writeJSON(w, 201, a.clientWithSecret(c, secret))
}

func (a *App) handlePortalClientRotate(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalRetailer(w, r)
	if !ok {
		return
	}
	a.rotateClient(w, r, v.RetailerID)
}

func (a *App) handlePortalClientRevoke(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalRetailer(w, r)
	if !ok {
		return
	}
	a.revokeClient(w, r, v.RetailerID)
}

var _ = sql.ErrNoRows
