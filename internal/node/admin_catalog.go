package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/openbrandexchange/node-go/internal/problem"
)

// adminCatalogPut/Patch/Delete reuse the ingest write path (session auth).
func (a *App) adminCatalogPut(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		doc, p := readJSONObject(r)
		if p != nil {
			problem.Write(w, p)
			return
		}
		if kind == KindAsset {
			a.prerenderDoc(r.Context(), doc)
		}
		ext := r.PathValue("external_id")
		res, prob := a.WriteOp(r.Context(), nil, func(wr *writer) (*IngestResult, *problem.Problem) {
			return wr.Upsert(kind, ext, doc, r.Header.Get("If-Match"))
		})
		a.writeResult(w, r, res, prob, "")
	}
}

func (a *App) adminCatalogPatch(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		doc, p := readJSONObject(r)
		if p != nil {
			problem.Write(w, p)
			return
		}
		if kind == KindAsset {
			a.prerenderDoc(r.Context(), doc)
		}
		ext := r.PathValue("external_id")
		res, prob := a.WriteOp(r.Context(), nil, func(wr *writer) (*IngestResult, *problem.Problem) {
			return wr.Patch("patch", kind, ext, doc, r.Header.Get("If-Match"))
		})
		a.writeResult(w, r, res, prob, "")
	}
}

func (a *App) adminCatalogDelete(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ext := r.PathValue("external_id")
		res, prob := a.WriteOp(r.Context(), nil, func(wr *writer) (*IngestResult, *problem.Problem) {
			return wr.Delete(kind, ext)
		})
		a.writeResult(w, r, res, prob, "")
	}
}

func (a *App) adminBrands(w http.ResponseWriter, r *http.Request) {
	rows, err := loadRows(r.Context(), a.DB, KindBrand, `ORDER BY name, external_id`)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items := []any{}
	for _, row := range rows {
		items = append(items, a.record(r.Context(), a.DB, row))
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// adminFileURL signs a URL for any stored blob (admin previews of
// unpublished assets).
func (a *App) adminFileURL(sha string) (string, string) {
	return a.signedURL("/admin/api/files/"+sha, "a")
}

func (a *App) handleAdminFile(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha256")
	if tag, ok := a.verifySigned(r.URL.Path, r.URL.Query()); !ok || tag != "a" || !isSHA256(sha) {
		failP(w, "unauthorized", "This link is invalid or expired.")
		return
	}
	b, err := a.getBlob(r.Context(), sha)
	if err != nil || b == nil {
		failP(w, "not_found", "File not found.")
		return
	}
	if err := a.serveBlob(w, r, sha, b.ContentType, ""); err != nil {
		a.fail(w, r, err)
	}
}

func (a *App) adminAssetJSON(ctx context.Context, row *row) map[string]any {
	rec := a.record(ctx, a.DB, row)
	rends := map[string]any{}
	var d AssetData
	_ = json.Unmarshal([]byte(row.Data), &d)
	files := map[string]LiveFile{}
	if row.LiveFiles != nil {
		_ = json.Unmarshal([]byte(*row.LiveFiles), &files)
	}
	// Prefer what the brand uploaded most recently.
	if orig, ok := d.Files["original"]; ok {
		if b, _ := a.getBlob(ctx, orig.SHA256); b != nil {
			f := LiveFile{SHA256: b.SHA256, ContentType: b.ContentType, Bytes: b.Bytes}
			if b.Width != nil {
				f.Width, f.Height = *b.Width, *b.Height
			}
			files["original"] = f
			for _, n := range DerivedRenditions {
				var s string
				var wd, ht int
				var by int64
				if a.DB.QueryRow(ctx, `SELECT sha256, width, height, bytes FROM renditions WHERE original_sha = ? AND name = ?`, orig.SHA256, n).Scan(&s, &wd, &ht, &by) == nil {
					files[n] = LiveFile{SHA256: s, ContentType: "image/jpeg", Width: wd, Height: ht, Bytes: by}
				}
			}
		}
	}
	for n, f := range files {
		u, exp := a.adminFileURL(f.SHA256)
		rends[n] = &Rendition{Name: n, URL: u, URLExpiresAt: &exp, ContentType: f.ContentType, Width: max(f.Width, 1), Height: max(f.Height, 1), Bytes: max(f.Bytes, 1), SHA256: f.SHA256}
	}
	return map[string]any{"record": rec, "renditions": rends}
}

func (a *App) adminProducts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, p := limitParam(r, 50, 200)
	if p != nil {
		problem.Write(w, p)
		return
	}
	q := r.URL.Query()
	where := `WHERE 1 = 1`
	var args []any
	if s := strings.ToLower(strings.TrimSpace(q.Get("q"))); s != "" {
		where += ` AND (search_text LIKE ? OR LOWER(external_id) LIKE ?)`
		args = append(args, "%"+s+"%", "%"+s+"%")
	}
	if b := q.Get("brand"); b != "" {
		where += ` AND brand_id IN (SELECT id FROM brands WHERE external_id = ?)`
		args = append(args, b)
	}
	if s := q.Get("status"); s != "" {
		where += ` AND status = ?`
		args = append(args, s)
	}
	switch q.Get("visibility") {
	case "visible":
		where += ` AND visible = 1`
	case "hidden":
		where += ` AND visible = 0`
	case "pending_files":
		where += ` AND id IN (SELECT product_id FROM assets WHERE pending = 1 OR live_files IS NULL)`
	}
	order := `ORDER BY record_updated_at DESC, id DESC`
	switch q.Get("sort") {
	case "style_number":
		order = `ORDER BY style_number, id`
	case "name":
		order = `ORDER BY name_sort, id`
	}
	offset := 0
	if c := q.Get("cursor"); c != "" {
		var cur struct {
			O int `json:"o"`
		}
		if !a.decodeCursor("admin-products", c, &cur) {
			failP(w, "cursor_invalid", "The cursor is not valid.")
			return
		}
		offset = cur.O
	}
	var total int
	_ = a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM products `+where, args...).Scan(&total)
	rows, err := loadRows(ctx, a.DB, KindProduct, where+` `+order+` LIMIT ? OFFSET ?`, append(args, limit+1, offset)...)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		c := a.encodeCursor("admin-products", map[string]int{"o": offset + limit})
		next = &c
	}
	items := []any{}
	for _, row := range rows {
		var d ProductData
		_ = json.Unmarshal([]byte(row.Data), &d)
		var vc, ac, pc int
		_ = a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM variants WHERE product_id = ?`, row.ID).Scan(&vc)
		_ = a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM assets WHERE product_id = ?`, row.ID).Scan(&ac)
		_ = a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM assets WHERE product_id = ? AND (pending = 1 OR live_files IS NULL)`, row.ID).Scan(&pc)
		var thumb *string
		var live *string
		if a.DB.QueryRow(ctx, `SELECT live_files FROM assets WHERE product_id = ? AND live_files IS NOT NULL `+assetOrder+` LIMIT 1`, row.ID).Scan(&live) == nil && live != nil {
			var files map[string]LiveFile
			_ = json.Unmarshal([]byte(*live), &files)
			if t, ok := files["thumb"]; ok {
				u, _ := a.adminFileURL(t.SHA256)
				thumb = &u
			}
		}
		vis := "hidden"
		if row.Visible {
			vis = "visible"
		}
		items = append(items, map[string]any{"id": row.ID, "external_id": row.ExternalID, "brand_external_id": d.BrandExternalID,
			"style_number": d.StyleNumber, "name": d.Name, "status": row.Status, "visibility": vis, "variant_count": vc,
			"asset_count": ac, "pending_asset_count": pc, "thumb_url": thumb, "updated_at": row.RecordUpdatedAt})
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next, "total": total})
}

func (a *App) adminProduct(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ext, _ := ValidExternalID(r.PathValue("external_id"))
	row, err := loadRow(ctx, a.DB, KindProduct, ext)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if row == nil {
		failP(w, "not_found", "Product %q does not exist.", ext)
		return
	}
	vs, err := loadRows(ctx, a.DB, KindVariant, `WHERE product_id = ? ORDER BY external_id`, row.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	as, err := loadRows(ctx, a.DB, KindAsset, `WHERE product_id = ?`+assetOrder, row.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	variants := []any{}
	for _, v := range vs {
		variants = append(variants, a.record(ctx, a.DB, v))
	}
	assets := []any{}
	for _, x := range as {
		assets = append(assets, a.adminAssetJSON(ctx, x))
	}
	writeJSON(w, 200, map[string]any{"record": a.record(ctx, a.DB, row), "variants": variants, "assets": assets})
}

func (a *App) adminAssetOrder(w http.ResponseWriter, r *http.Request) { a.doAssetOrder(w, r, nil) }

func (a *App) adminPutFile(w http.ResponseWriter, r *http.Request) { a.putFile(w, r, nil) }

// importTemplate is the canonical CSV template with two example rows.
const importTemplate = "brand,style_number,name:en,name:nl,description:en,description:nl,item_type,target_group,season,collection,materials,care,country_of_origin,gtin,sku,color_code,color_name:en,color_name:nl,size,size_system\r\n" +
	"Your Brand,ST-1001,Crew neck sweater,Trui met ronde hals,Soft knit sweater.,Zachte gebreide trui.,sweater,unisex,AW26,Essentials,main: 100% merino_wool,wash_30_very_mild|do_not_bleach|dry_flat,PT,2000000000015,ST-1001-NVY-M,NVY,Navy,Marineblauw,M,INT\r\n" +
	"Your Brand,ST-1001,Crew neck sweater,Trui met ronde hals,Soft knit sweater.,Zachte gebreide trui.,sweater,unisex,AW26,Essentials,main: 100% merino_wool,wash_30_very_mild|do_not_bleach|dry_flat,PT,2000000000022,ST-1001-NVY-L,NVY,Navy,Marineblauw,L,INT\r\n"

func (a *App) adminImportTemplate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", "products-template.csv"))
	_, _ = w.Write([]byte("\ufeff" + importTemplate))
}

func (a *App) adminImportCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, p := a.parseImportUpload(r)
	if p != nil {
		problem.Write(w, p)
		return
	}
	s := sessionFrom(ctx)
	job, err := a.enqueueImport(ctx, req, "upload", "user", s.User.ID, "", nil, true)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 202, a.importRunJSON(ctx, job))
}

func (a *App) adminImports(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, p := limitParam(r, 50, 200)
	if p != nil {
		problem.Write(w, p)
		return
	}
	where := `WHERE kind = 'import'`
	var args []any
	if s := r.URL.Query().Get("source_id"); s != "" {
		where += ` AND source_id = ?`
		args = append(args, s)
	}
	if c := r.URL.Query().Get("cursor"); c != "" {
		var cur struct {
			C string `json:"c"`
			I string `json:"i"`
		}
		if !a.decodeCursor("imports", c, &cur) {
			failP(w, "cursor_invalid", "The cursor is not valid.")
			return
		}
		where += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, cur.C, cur.C, cur.I)
	}
	rs, err := a.DB.Query(ctx, `SELECT `+jobCols+` FROM jobs `+where+` ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var jobs []*jobRow
	for rs.Next() {
		if j, err := scanJob(rs); err == nil {
			jobs = append(jobs, j)
		}
	}
	rs.Close()
	var next *string
	if len(jobs) > limit {
		jobs = jobs[:limit]
		c := a.encodeCursor("imports", map[string]string{"c": jobs[limit-1].CreatedAt, "i": jobs[limit-1].ID})
		next = &c
	}
	items := []any{}
	for _, j := range jobs {
		items = append(items, a.importRunJSON(ctx, j))
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}

func (a *App) adminImport(w http.ResponseWriter, r *http.Request) {
	j, err := a.loadJob(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if j == nil || j.Kind != "import" {
		failP(w, "not_found", "Import not found.")
		return
	}
	writeJSON(w, 200, a.importRunJSON(r.Context(), j))
}

func (a *App) adminImportErrors(w http.ResponseWriter, r *http.Request) {
	j, err := a.loadJob(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if j == nil || j.Kind != "import" {
		failP(w, "not_found", "Import not found.")
		return
	}
	a.jobItems(w, r, j.ID, 50, 200)
}

var _ = url.PathEscape
