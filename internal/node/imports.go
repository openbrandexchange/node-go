package node

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/openbrandexchange/node-go/internal/blobstore"
	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/importfmt"
	"github.com/openbrandexchange/node-go/internal/problem"
)

// ImportRowError is one problem found while importing.
type ImportRowError struct {
	File     string  `json:"file"`
	Row      *int    `json:"row"`
	Column   *string `json:"column"`
	Severity string  `json:"severity"`
	ObxError string  `json:"obx_error"`
	Detail   string  `json:"detail"`
	Value    *string `json:"value"`
}

type importFile struct {
	Field  string `json:"field"`
	Name   string `json:"name"`
	Key    string `json:"key"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type manifestRendition struct {
	SHA256 string `json:"sha256"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type manifestEntry struct {
	File       string                       `json:"file"`
	SHA256     string                       `json:"sha256"`
	Width      *int                         `json:"width,omitempty"`
	Height     *int                         `json:"height,omitempty"`
	Renditions map[string]manifestRendition `json:"renditions,omitempty"`
}

type importOptions struct {
	Mode               string `json:"mode"`
	AllowMassUnpublish bool   `json:"allow_mass_unpublish"`
	DryRun             bool   `json:"dry_run"`
	DefaultLanguage    string `json:"default_language"`
}

type importPayload struct {
	SourceID    string          `json:"source_id,omitempty"`
	Force       bool            `json:"force,omitempty"`
	Fingerprint string          `json:"fingerprint,omitempty"`
	Files       []importFile    `json:"files"`
	Manifest    []manifestEntry `json:"manifest"`
	Options     importOptions   `json:"options"`
	Allowed     []string        `json:"allowed"`
	CanDelete   bool            `json:"can_delete"`
}

type importCounts struct {
	Rows        int `json:"rows"`
	Files       int `json:"files"`
	Created     int `json:"created"`
	Updated     int `json:"updated"`
	Unchanged   int `json:"unchanged"`
	Unpublished int `json:"unpublished"`
	Errors      int `json:"errors"`
	Warnings    int `json:"warnings"`
}

type importResult struct {
	Unchanged bool             `json:"x-node-go-unchanged,omitempty"`
	Mode      string           `json:"mode"`
	DryRun    bool             `json:"dry_run"`
	Files     []map[string]any `json:"files"`
	Counts    importCounts     `json:"counts"`
	Error     *problem.Problem `json:"error"`
}

// imageSource is one image of an import.
type imageSource struct {
	Name     string
	Size     int64
	Open     func() (io.ReadCloser, error)
	Manifest *manifestEntry
}

type variantPlan struct {
	ext string
	doc map[string]any
	row int
}

type productPlan struct {
	ext      string
	style    string
	brandRef string
	doc      map[string]any
	first    map[string]string
	rows     []int
	variants []variantPlan
	colors   map[string]string // lower → spelling
	images   []*imagePlan
	failed   bool
}

type imagePlan struct {
	src       *imageSource
	ext       string
	productEx string
	doc       map[string]any
	sha       string
}

type importer struct {
	a        *App
	ctx      context.Context
	opts     importOptions
	allowed  []string
	lang     string
	errors   []ImportRowError
	counts   importCounts
	plans    []*productPlan
	byExt    map[string]*productPlan
	extra    map[string]*productPlan // image-only products (from the node)
	extraOrd []string
	progress func(done, total int)
}

func (im *importer) add(file string, row int, col, code, detail, value string, warning bool) {
	e := ImportRowError{File: file, Severity: "error", ObxError: code, Detail: truncate(detail, 2000)}
	if warning {
		e.Severity = "warning"
		im.counts.Warnings++
	} else {
		im.counts.Errors++
	}
	if row > 0 {
		r := row
		e.Row = &r
	}
	if col != "" {
		c := col
		e.Column = &c
	}
	if value != "" {
		v := truncate(value, 500)
		e.Value = &v
	}
	im.errors = append(im.errors, e)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (im *importer) count(result string) {
	switch result {
	case "created":
		im.counts.Created++
	case "updated":
		im.counts.Updated++
	case "unchanged":
		im.counts.Unchanged++
	}
}

// problemColumn maps a writer problem pointer to a CSV column.
func problemColumn(p *problem.Problem) string {
	for _, e := range p.Errors {
		if e.Pointer != nil && *e.Pointer != "" {
			c := strings.TrimPrefix(*e.Pointer, "/")
			c = strings.SplitN(c, "/", 2)[0]
			switch c {
			case "product_external_id":
				return "style_number"
			case "color":
				return "color_code"
			}
			return c
		}
	}
	return ""
}

func problemDetail(p *problem.Problem) string {
	d := p.Title
	if p.Detail != nil {
		d = *p.Detail
	}
	for _, e := range p.Errors {
		if e.Pointer != nil && *e.Pointer != "" && e.Detail != "" {
			d += " (" + strings.TrimPrefix(*e.Pointer, "/") + ": " + e.Detail + ")"
			break
		}
	}
	return d
}

// ---- parsing products.csv -------------------------------------------------------

func (im *importer) parseProducts(data []byte) *problem.Problem {
	t, err := importfmt.ParseCSV(data)
	if err != nil {
		return problem.New("import_invalid", "products.csv: %s", err.Error())
	}
	if !t.Has("style_number") {
		return problem.New("import_invalid", "products.csv needs a style_number column.")
	}
	for _, c := range importfmt.UnknownColumns(t.Header, importfmt.KnownColumns) {
		im.add("products.csv", 0, c, "validation_failed", fmt.Sprintf("unknown column %q ignored", c), "", true)
	}
	im.counts.Rows = len(t.Rows)
	variantCols := []string{"gtin", "variant_id", "sku", "color_code", "size"}
	for _, row := range t.Rows {
		style := row.Get("style_number")
		if style == "" {
			im.add("products.csv", row.Num, "style_number", "validation_failed", "style_number is required", "", false)
			continue
		}
		ext := row.Get("product_id")
		if ext == "" {
			ext = style
		}
		if _, ok := ValidExternalID(ext); !ok {
			im.add("products.csv", row.Num, "product_id", "validation_failed", "invalid product id", ext, false)
			continue
		}
		plan := im.byExt[ext]
		if plan == nil {
			doc, perr := im.productDoc(row, t.Header, style)
			if perr != nil {
				im.add("products.csv", row.Num, perr.col, perr.code, perr.detail, perr.value, false)
				continue
			}
			plan = &productPlan{ext: ext, style: style, brandRef: row.Get("brand"), doc: doc, first: map[string]string{}, colors: map[string]string{}}
			for _, h := range t.Header {
				if isProductColumn(h) {
					plan.first[h] = row.Get(h)
				}
			}
			im.byExt[ext] = plan
			im.plans = append(im.plans, plan)
		} else {
			for _, h := range t.Header {
				if !isProductColumn(h) {
					continue
				}
				if v := row.Get(h); v != "" && plan.first[h] != "" && plan.first[h] != v {
					im.add("products.csv", row.Num, h, "validation_failed", fmt.Sprintf("different %s than on the product's first row; the first value is used", h), v, true)
				} else if v != "" && plan.first[h] == "" {
					plan.first[h] = v
				}
			}
		}
		plan.rows = append(plan.rows, row.Num)
		hasVariant := false
		for _, c := range variantCols {
			if row.Get(c) != "" {
				hasVariant = true
			}
		}
		if !hasVariant {
			continue
		}
		vdoc, verr := im.variantDoc(row, t.Header, plan)
		if verr != nil {
			im.add("products.csv", row.Num, verr.col, verr.code, verr.detail, verr.value, false)
			continue
		}
		plan.variants = append(plan.variants, *vdoc)
	}
	return nil
}

func isProductColumn(h string) bool {
	base, _, _ := strings.Cut(h, ":")
	switch base {
	case "brand", "item_type", "target_group", "season", "collection", "gpc_brick", "materials", "care", "country_of_origin", "dpp_url", "status", "name", "description", "attr":
		return true
	}
	return false
}

type cellErr struct{ col, code, detail, value string }

func (im *importer) productDoc(row importfmt.Row, header []string, style string) (map[string]any, *cellErr) {
	doc := map[string]any{"style_number": style}
	name := importfmt.Localized(row, header, "name", im.lang)
	if name == nil {
		return nil, &cellErr{"name", "validation_failed", "a name is required (name or name:<language>)", ""}
	}
	doc["name"] = toAnyMap(name)
	if d := importfmt.Localized(row, header, "description", im.lang); d != nil {
		doc["description"] = toAnyMap(d)
	}
	for _, c := range []string{"item_type", "target_group"} {
		if v := row.Get(c); v != "" {
			k := importfmt.Key(v)
			if !importfmt.ValidKey(k) {
				return nil, &cellErr{c, "validation_failed", c + " must be a lowercase key like t_shirt", v}
			}
			doc[c] = k
		}
	}
	for _, c := range []string{"season", "collection", "dpp_url"} {
		if v := row.Get(c); v != "" {
			doc[c] = v
		}
	}
	if v := row.Get("gpc_brick"); v != "" {
		doc["gpc_brick"] = v
	}
	if v := row.Get("country_of_origin"); v != "" {
		doc["country_of_origin"] = strings.ToUpper(v)
	}
	if v := row.Get("materials"); v != "" {
		parts, warns, err := importfmt.ParseMaterials(v)
		if err != nil {
			return nil, &cellErr{"materials", "validation_failed", "materials: " + err.Error(), v}
		}
		for _, w := range warns {
			im.add("products.csv", row.Num, "materials", "validation_failed", w, v, true)
		}
		var ms []any
		for _, p := range parts {
			var fibers []any
			for _, f := range p.Fibers {
				fibers = append(fibers, map[string]any{"fiber": f.Key, "percentage": f.Percentage})
			}
			ms = append(ms, map[string]any{"part": p.Name, "fibers": fibers})
		}
		doc["materials"] = ms
	}
	if v := row.Get("care"); v != "" {
		codes, warns, err := importfmt.ParseCare(v)
		if err != nil {
			return nil, &cellErr{"care", "validation_failed", err.Error(), v}
		}
		for _, w := range warns {
			im.add("products.csv", row.Num, "care", "validation_failed", w, v, true)
		}
		var cs []any
		for _, c := range codes {
			cs = append(cs, map[string]any{"code": c})
		}
		doc["care"] = cs
	}
	if v := row.Get("status"); v != "" {
		s := strings.ToLower(v)
		if s != "published" && s != "unpublished" {
			return nil, &cellErr{"status", "validation_failed", "status must be published or unpublished", v}
		}
		doc["status"] = s
	}
	attrs := map[string]any{}
	for _, h := range header {
		if strings.HasPrefix(h, "attr:") {
			if v := row.Get(h); v != "" {
				k := strings.TrimPrefix(h, "attr:")
				if !importfmt.ValidKey(k) {
					return nil, &cellErr{h, "validation_failed", "attribute keys are lowercase like fit_type", v}
				}
				attrs[k] = v
			}
		}
	}
	if len(attrs) > 0 {
		doc["attributes"] = attrs
	}
	return doc, nil
}

func toAnyMap(m map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (im *importer) variantDoc(row importfmt.Row, header []string, plan *productPlan) (*variantPlan, *cellErr) {
	color := row.Get("color_code")
	if color == "" {
		return nil, &cellErr{"color_code", "validation_failed", "color_code is required for a variant row", ""}
	}
	doc := map[string]any{"product_external_id": plan.ext}
	gtinRaw := row.Get("gtin")
	if gtinRaw != "" {
		g, err := importfmt.GTINCell(gtinRaw)
		if err != nil {
			return nil, &cellErr{"gtin", "invalid_gtin", err.Error(), gtinRaw}
		}
		doc["gtin"] = g
	}
	c := map[string]any{"code": color}
	if n := importfmt.Localized(row, header, "color_name", im.lang); n != nil {
		c["name"] = toAnyMap(n)
	}
	if v := row.Get("supplier_color"); v != "" {
		c["supplier_color"] = v
	}
	doc["color"] = c
	for _, k := range []string{"sku", "size", "size_system"} {
		if v := row.Get(k); v != "" {
			doc[k] = v
		}
	}
	if v := row.Get("variant_status"); v != "" {
		s := strings.ToLower(v)
		if s != "published" && s != "unpublished" {
			return nil, &cellErr{"variant_status", "validation_failed", "variant_status must be published or unpublished", v}
		}
		doc["status"] = s
	}
	ext := row.Get("variant_id")
	if ext == "" {
		if g, ok := doc["gtin"].(string); ok {
			ext = g
		} else {
			ext = plan.style + "-" + color
			if s := row.Get("size"); s != "" {
				ext += "-" + s
			}
		}
	}
	if _, ok := ValidExternalID(ext); !ok {
		return nil, &cellErr{"variant_id", "validation_failed", "invalid variant id", ext}
	}
	plan.colors[strings.ToLower(color)] = color
	return &variantPlan{ext: ext, doc: doc, row: row.Num}, nil
}

// ---- assets.csv and images --------------------------------------------------------

type assetRowInfo struct {
	row   importfmt.Row
	used  bool
	table *importfmt.Table
}

func (im *importer) parseAssets(data []byte) (map[string]*assetRowInfo, *problem.Problem) {
	t, err := importfmt.ParseCSV(data)
	if err != nil {
		return nil, problem.New("import_invalid", "assets.csv: %s", err.Error())
	}
	if !t.Has("file") {
		return nil, problem.New("import_invalid", "assets.csv needs a file column.")
	}
	for _, c := range importfmt.UnknownColumns(t.Header, importfmt.KnownAssetColumns) {
		im.add("assets.csv", 0, c, "validation_failed", fmt.Sprintf("unknown column %q ignored", c), "", true)
	}
	out := map[string]*assetRowInfo{}
	for _, r := range t.Rows {
		f := path.Base(strings.ReplaceAll(r.Get("file"), "\\", "/"))
		if r.Get("file") == "" {
			im.add("assets.csv", r.Num, "file", "validation_failed", "file is required", "", false)
			continue
		}
		out[strings.ToLower(f)] = &assetRowInfo{row: r, table: t}
	}
	return out, nil
}

// catalogProduct finds a product by style number (case-insensitive) in the
// import, then on the node.
func (im *importer) catalogProduct(style string) *productPlan {
	ls := strings.ToLower(style)
	for _, p := range im.plans {
		if strings.ToLower(p.style) == ls || strings.ToLower(p.ext) == ls {
			return p
		}
	}
	if p := im.extra[ls]; p != nil {
		return p
	}
	var ext, sn string
	err := im.a.DB.QueryRow(im.ctx, `SELECT external_id, style_number FROM products WHERE LOWER(style_number) = ? OR LOWER(external_id) = ? ORDER BY CASE WHEN external_id = style_number THEN 0 ELSE 1 END LIMIT 1`, ls, ls).Scan(&ext, &sn)
	if err != nil {
		return nil
	}
	p := &productPlan{ext: ext, style: sn, colors: map[string]string{}}
	rs, err := im.a.DB.Query(im.ctx, `SELECT DISTINCT v.color_code FROM variants v JOIN products p ON p.id = v.product_id WHERE p.external_id = ?`, ext)
	if err == nil {
		for rs.Next() {
			var c string
			if rs.Scan(&c) == nil {
				p.colors[strings.ToLower(c)] = c
			}
		}
		rs.Close()
	}
	im.extra[ls] = p
	im.extraOrd = append(im.extraOrd, ls)
	return p
}

func (im *importer) planImages(images []*imageSource, assetRows map[string]*assetRowInfo) {
	sort.SliceStable(images, func(i, j int) bool { return images[i].Name < images[j].Name })
	byExt := map[string][]*imagePlan{}
	var order []string
	for _, src := range images {
		base := path.Base(strings.ReplaceAll(src.Name, "\\", "/"))
		ar := assetRows[strings.ToLower(base)]
		if ar != nil {
			ar.used = true
		}
		plan, doc, err := im.imageDoc(base, ar)
		if err != nil {
			im.add(base, 0, err.col, err.code, err.detail, err.value, false)
			continue
		}
		ip := &imagePlan{src: src, ext: doc["__ext"].(string), productEx: plan.ext, doc: doc}
		delete(doc, "__ext")
		if _, seen := byExt[ip.ext]; !seen {
			order = append(order, ip.ext)
		}
		byExt[ip.ext] = append(byExt[ip.ext], ip)
		ip.doc["__plan"] = plan
	}
	for _, ext := range order {
		list := byExt[ext]
		if len(list) > 1 {
			for _, ip := range list {
				im.add(path.Base(ip.src.Name), 0, "", "validation_failed", fmt.Sprintf("more than one file resolves to image %s", ext), "", false)
			}
			continue
		}
		ip := list[0]
		plan := ip.doc["__plan"].(*productPlan)
		delete(ip.doc, "__plan")
		plan.images = append(plan.images, ip)
	}
	for f, ar := range assetRows {
		if !ar.used {
			im.add("assets.csv", ar.row.Num, "file", "files_missing", fmt.Sprintf("image %s is not part of this import", ar.row.Get("file")), f, false)
		}
	}
}

// colourlessCode stands for "all colours" in image file names and asset
// external ids: KO-1001_ALL_1.jpg is an image of the style, not of one
// colour (docs/import-format.md).
const colourlessCode = "ALL"

func (im *importer) imageDoc(base string, ar *assetRowInfo) (*productPlan, map[string]any, *cellErr) {
	n, perr := importfmt.ParseImageName(base)
	style, color, pos, role, view := "", "", 0, "", ""
	if n != nil {
		style, color, pos, role, view = n.Style, n.Color, n.Position, n.Role, n.View
	}
	var r importfmt.Row
	if ar != nil {
		r = ar.row
		if v := r.Get("product_id"); v != "" {
			style = v
		} else if v := r.Get("style_number"); v != "" {
			style = v
		}
		if _, ok := r.Values["color_code"]; ok && (r.Get("color_code") != "" || n == nil) {
			color = r.Get("color_code")
		}
		if v := r.Get("position"); v != "" {
			p, err := strconv.Atoi(v)
			if err != nil || p < 1 || p > 999 {
				return nil, nil, &cellErr{"position", "validation_failed", "position must be 1-999", v}
			}
			pos = p
		}
		if v := importfmt.Key(r.Get("role")); v != "" {
			if !importfmt.Roles[v] {
				return nil, nil, &cellErr{"role", "validation_failed", "unknown role", v}
			}
			role = v
		}
		if v := importfmt.Key(r.Get("view")); v != "" {
			if !importfmt.Views[v] {
				return nil, nil, &cellErr{"view", "validation_failed", "unknown view", v}
			}
			view = v
		}
	}
	if style == "" || pos == 0 {
		if perr != nil {
			return nil, nil, &cellErr{"", "validation_failed", perr.Error(), ""}
		}
		return nil, nil, &cellErr{"", "validation_failed", "cannot tell which product this image belongs to", ""}
	}
	plan := im.catalogProduct(style)
	if plan == nil {
		return nil, nil, &cellErr{"", "unknown_parent", fmt.Sprintf("style %s is not in the catalog", style), style}
	}
	doc := map[string]any{"product_external_id": plan.ext, "position": pos, "filename": base}
	var colorOut any
	if color != "" {
		spelled, ok := plan.colors[strings.ToLower(color)]
		switch {
		case strings.EqualFold(color, colourlessCode) && (n != nil && strings.EqualFold(n.Color, color)):
			// {style}_ALL_{position}: an image for every colour of the style.
			// ALL is reserved in file names (import-format.md "all colours");
			// a colour really coded ALL is reached through assets.csv.
			color = ""
		case ok:
			colorOut = spelled
			color = spelled
		default:
			return nil, nil, &cellErr{"", "unknown_parent", fmt.Sprintf("colour %s of style %s is not in the catalog", color, plan.style), color}
		}
	}
	doc["color_code"] = colorOut
	if role == "" {
		role = "packshot"
	}
	doc["role"] = role
	if view != "" {
		doc["view"] = view
	}
	if color == "" {
		color = colourlessCode
	}
	ext := plan.style + "_" + color + "_" + strconv.Itoa(pos)
	if ar != nil {
		if v := r.Get("asset_id"); v != "" {
			ext = v
		}
		if err := im.assetRowFields(r, ar.table.Header, doc); err != nil {
			return nil, nil, err
		}
	}
	if _, ok := ValidExternalID(ext); !ok {
		return nil, nil, &cellErr{"asset_id", "validation_failed", "invalid asset id", ext}
	}
	doc["__ext"] = ext
	return plan, doc, nil
}

func (im *importer) assetRowFields(r importfmt.Row, header []string, doc map[string]any) *cellErr {
	if alt := importfmt.Localized(r, header, "alt_text", im.lang); alt != nil {
		doc["alt_text"] = toAnyMap(alt)
	}
	rights := map[string]any{}
	if v := r.Get("license"); v != "" {
		rights["license"] = strings.ToLower(v)
	}
	if v := r.Get("license_text"); v != "" {
		rights["license_text"] = v
	}
	if v := r.Get("channels"); v != "" {
		var ch []any
		for _, c := range strings.Split(v, "|") {
			if c = importfmt.Key(c); c != "" {
				ch = append(ch, c)
			}
		}
		rights["channels"] = ch
	}
	if v := r.Get("territories"); v != "" {
		var ts []any
		for _, c := range strings.Split(v, "|") {
			if c = strings.ToUpper(strings.TrimSpace(c)); c != "" {
				ts = append(ts, c)
			}
		}
		rights["territories"] = ts
	}
	for _, k := range []string{"embargo_until", "expires_at"} {
		if v := r.Get(k); v != "" {
			d, ok := importfmt.ParseDate(v)
			if !ok {
				return &cellErr{k, "validation_failed", k + " must be a date (YYYY-MM-DD) or RFC 3339 timestamp", v}
			}
			rights[k] = d
		}
	}
	if v := r.Get("credit"); v != "" {
		rights["credit"] = v
	}
	if v := r.Get("model_release"); v != "" {
		b, ok := importfmt.ParseBool(v)
		if !ok {
			return &cellErr{"model_release", "validation_failed", "use yes or no", v}
		}
		rights["model_release"] = b
	}
	if len(rights) > 0 {
		doc["rights"] = rights
	}
	prov := map[string]any{}
	for _, k := range []string{"ai_generated", "ai_edited"} {
		if v := r.Get(k); v != "" {
			b, ok := importfmt.ParseBool(v)
			if !ok {
				return &cellErr{k, "validation_failed", "use yes or no", v}
			}
			prov[k] = b
		}
	}
	for _, k := range []string{"source", "c2pa"} {
		if v := r.Get(k); v != "" {
			prov[k] = strings.ToLower(v)
		}
	}
	for _, k := range []string{"c2pa_manifest_url", "generator"} {
		if v := r.Get(k); v != "" {
			prov[k] = v
		}
	}
	if len(prov) > 0 {
		doc["provenance"] = prov
	}
	if v := r.Get("status"); v != "" {
		s := strings.ToLower(v)
		if s != "published" && s != "unpublished" {
			return &cellErr{"status", "validation_failed", "status must be published or unpublished", v}
		}
		doc["status"] = s
	}
	return nil
}

// ---- applying ---------------------------------------------------------------------

// resolveBrand returns the brand external id for a plan (creating the brand
// when it is new). It runs inside w's transaction.
func (im *importer) resolveBrand(w *writer, plan *productPlan) (string, *problem.Problem) {
	ref := strings.TrimSpace(plan.brandRef)
	if ref == "" {
		var n int
		_ = w.tx.QueryRow(w.ctx, `SELECT COUNT(*) FROM brands`).Scan(&n)
		if n == 1 {
			var e string
			_ = w.tx.QueryRow(w.ctx, `SELECT external_id FROM brands`).Scan(&e)
			return e, nil
		}
		return "", problem.New("validation_failed", "brand is required because this node has more than one brand.").Field("/brand", "validation_failed", "required")
	}
	var ext string
	lr := strings.ToLower(ref)
	err := w.tx.QueryRow(w.ctx, `SELECT external_id FROM brands WHERE LOWER(name) = ? OR LOWER(external_id) = ? OR external_id = ? ORDER BY external_id LIMIT 1`, lr, lr, importfmt.Slug(ref)).Scan(&ext)
	if err == nil {
		return ext, nil
	}
	slug := importfmt.Slug(ref)
	if slug == "" {
		return "", problem.New("validation_failed", "Brand name %q cannot be turned into an id.", ref).Field("/brand", "validation_failed", "invalid brand")
	}
	res, prob := w.Upsert(KindBrand, slug, map[string]any{"name": ref}, "")
	if prob != nil {
		return "", prob
	}
	im.count(res.Result)
	return slug, nil
}

// prepareImages stores image bytes as blobs and renders them (outside the
// write lock).
func (im *importer) prepareImages(plan *productPlan) {
	kept := plan.images[:0]
	for _, ip := range plan.images {
		base := path.Base(ip.src.Name)
		files := map[string]any{}
		if m := ip.src.Manifest; m != nil {
			if !im.a.HasBlob(im.ctx, m.SHA256) {
				im.add(base, 0, "", "blob_missing", "the image in images_manifest has not been uploaded ("+m.SHA256+")", "", false)
				continue
			}
			ip.sha = m.SHA256
			orig := map[string]any{"sha256": m.SHA256}
			if m.Width != nil {
				orig["width"] = *m.Width
			}
			if m.Height != nil {
				orig["height"] = *m.Height
			}
			files["original"] = orig
			for n, rr := range m.Renditions {
				if n != "web" && n != "square" && n != "thumb" {
					continue
				}
				if !im.a.HasBlob(im.ctx, rr.SHA256) {
					im.add(base, 0, "", "blob_missing", "rendition "+n+" in images_manifest has not been uploaded", "", false)
					continue
				}
				files[n] = map[string]any{"sha256": rr.SHA256, "width": rr.Width, "height": rr.Height}
			}
		} else {
			rc, err := ip.src.Open()
			if err != nil {
				im.add(base, 0, "", "import_invalid", "cannot read the image: "+err.Error(), "", false)
				continue
			}
			if im.opts.DryRun {
				h := sha256.New()
				_, err = io.Copy(h, io.LimitReader(rc, MaxUploadBytes+1))
				rc.Close()
				if err != nil {
					im.add(base, 0, "", "import_invalid", "cannot read the image", "", false)
					continue
				}
				ip.sha = hex.EncodeToString(h.Sum(nil))
				if im.a.HasBlob(im.ctx, ip.sha) {
					files["original"] = map[string]any{"sha256": ip.sha}
				}
			} else {
				b, err := im.a.PutBlob(im.ctx, rc, "", MaxUploadBytes)
				rc.Close()
				if err != nil {
					var p *problem.Problem
					if errors.As(err, &p) {
						im.add(base, 0, "", p.ObxError, problemDetail(p), "", false)
					} else {
						im.add(base, 0, "", "internal_error", "the image could not be stored", "", false)
						im.a.Log.Error("import image store failed", "file", base, "error", err)
					}
					continue
				}
				ip.sha = b.SHA256
				files["original"] = map[string]any{"sha256": b.SHA256}
			}
		}
		if !im.opts.DryRun && ip.sha != "" {
			if err := im.a.ensureRenditions(im.ctx, ip.sha); err != nil {
				im.add(base, 0, "", "validation_failed", "the image could not be rendered: "+err.Error(), "", true)
			}
		}
		if len(files) > 0 {
			ip.doc["files"] = files
		}
		kept = append(kept, ip)
	}
	plan.images = kept
}

// applyPlan writes one product with its variants and images.
func (im *importer) applyPlan(w *writer, plan *productPlan) error {
	if plan.doc != nil {
		bext, prob := im.resolveBrand(w, plan)
		if prob != nil {
			im.add("products.csv", plan.rows[0], "brand", prob.ObxError, problemDetail(prob), plan.brandRef, false)
			plan.failed = true
			return nil
		}
		doc := cloneDoc(plan.doc)
		doc["brand_external_id"] = bext
		res, prob := w.Upsert(KindProduct, plan.ext, doc, "")
		if prob != nil {
			if prob.ObxError == "internal_error" {
				return prob
			}
			im.add("products.csv", plan.rows[0], problemColumn(prob), prob.ObxError, problemDetail(prob), "", false)
			plan.failed = true
			return nil
		}
		im.count(res.Result)
		for _, v := range plan.variants {
			res, prob := w.Upsert(KindVariant, v.ext, v.doc, "")
			if prob != nil {
				if prob.ObxError == "internal_error" {
					return prob
				}
				val := ""
				if g, ok := v.doc["gtin"].(string); ok && (prob.ObxError == "invalid_gtin" || prob.ObxError == "gtin_conflict") {
					val = g
				}
				im.add("products.csv", v.row, problemColumn(prob), prob.ObxError, problemDetail(prob), val, false)
				continue
			}
			im.count(res.Result)
		}
	}
	if len(plan.images) == 0 {
		return nil
	}
	// Positions of one product colour are applied atomically: existing
	// assets that this import rewrites do not block each other.
	w.positionExclude = map[string]bool{}
	for _, ip := range plan.images {
		var id string
		if w.tx.QueryRow(w.ctx, `SELECT id FROM assets WHERE external_id = ?`, ip.ext).Scan(&id) == nil {
			w.positionExclude[id] = true
		}
	}
	for _, ip := range plan.images {
		doc := ip.doc
		if existing, err := loadRow(w.ctx, w.tx, KindAsset, ip.ext); err != nil {
			return err
		} else if existing != nil {
			doc = keepImageFacts(doc, existing.Data)
		}
		res, prob := w.Upsert(KindAsset, ip.ext, doc, "")
		if prob != nil {
			if prob.ObxError == "internal_error" {
				return prob
			}
			im.add(path.Base(ip.src.Name), 0, problemColumn(prob), prob.ObxError, problemDetail(prob), "", false)
			continue
		}
		im.count(res.Result)
	}
	w.positionExclude = nil
	return nil
}

// keepImageFacts applies import-format.md rule 8 to an image the node
// already has: alt text (per language), rights and provenance fields the
// import does not state keep their stored values (the file name and the
// assets.csv row still decide the file, product, colour, position, role and
// view; a PUT without status keeps the stored status anyway). A nightly
// import of the images alone therefore never lifts an embargo or clears an
// AI label set in the admin.
func keepImageFacts(doc map[string]any, stored string) map[string]any {
	prev := map[string]any{}
	dec := json.NewDecoder(strings.NewReader(stored))
	dec.UseNumber()
	if dec.Decode(&prev) != nil {
		return doc
	}
	out := cloneDoc(doc)
	for _, k := range []string{"alt_text", "rights", "provenance"} {
		old, _ := prev[k].(map[string]any)
		if len(old) == 0 {
			continue
		}
		merged := map[string]any{}
		for f, v := range old {
			merged[f] = v
		}
		stated, _ := out[k].(map[string]any)
		for f, v := range stated {
			merged[f] = v
		}
		out[k] = merged
	}
	return out
}

func cloneDoc(m map[string]any) map[string]any {
	var out map[string]any
	_ = json.Unmarshal(mustJSON(m), &out)
	return out
}

// mirrorPlan returns the products a mirror import would unpublish, or a
// refusal.
func (im *importer) mirrorPlan() ([]string, *problem.Problem) {
	brands := map[string]bool{}
	for _, p := range im.plans {
		ref := strings.ToLower(strings.TrimSpace(p.brandRef))
		var id string
		if ref == "" {
			_ = im.a.DB.QueryRow(im.ctx, `SELECT id FROM brands ORDER BY created_at LIMIT 1`).Scan(&id)
		} else {
			_ = im.a.DB.QueryRow(im.ctx, `SELECT id FROM brands WHERE LOWER(name) = ? OR LOWER(external_id) = ? OR external_id = ?`, ref, ref, importfmt.Slug(p.brandRef)).Scan(&id)
		}
		if id != "" {
			brands[id] = true
		}
	}
	inFile := map[string]bool{}
	for _, p := range im.plans {
		inFile[p.ext] = true
	}
	var toUnpublish []string
	total := 0
	for b := range brands {
		rs, err := im.a.DB.Query(im.ctx, `SELECT external_id FROM products WHERE brand_id = ? AND status = 'published' ORDER BY external_id`, b)
		if err != nil {
			return nil, problem.As(err)
		}
		for rs.Next() {
			var e string
			if rs.Scan(&e) == nil {
				total++
				if !inFile[e] {
					toUnpublish = append(toUnpublish, e)
				}
			}
		}
		rs.Close()
	}
	sort.Strings(toUnpublish)
	if len(toUnpublish)*2 > total && !im.opts.AllowMassUnpublish {
		return nil, problem.New("mass_unpublish_refused", "This mirror import would unpublish %d of %d products. Check the file, or allow mass unpublishing explicitly.", len(toUnpublish), total)
	}
	return toUnpublish, nil
}

// run executes the whole import.
func (im *importer) run(productsCSV, assetsCSV []byte, images []*imageSource) *problem.Problem {
	if productsCSV != nil {
		if p := im.parseProducts(productsCSV); p != nil {
			return p
		}
	}
	assetRows := map[string]*assetRowInfo{}
	if assetsCSV != nil {
		var p *problem.Problem
		if assetRows, p = im.parseAssets(assetsCSV); p != nil {
			return p
		}
	}
	im.counts.Files = len(images)
	im.planImages(images, assetRows)
	var toUnpublish []string
	if im.opts.Mode == "mirror" {
		var p *problem.Problem
		if toUnpublish, p = im.mirrorPlan(); p != nil {
			return p
		}
	}
	all := append([]*productPlan{}, im.plans...)
	for _, k := range im.extraOrd {
		if len(im.extra[k].images) > 0 {
			all = append(all, im.extra[k])
		}
	}
	apply := func(tx *db.Tx, plan *productPlan) error {
		w := im.a.newWriter(im.ctx, tx)
		w.allowedBrands = im.allowed
		return im.applyPlan(w, plan)
	}
	if im.opts.DryRun {
		err := im.a.DB.WriteTx(im.ctx, func(tx *db.Tx) error {
			for _, plan := range all {
				im.prepareImages(plan)
				if err := apply(tx, plan); err != nil {
					return err
				}
			}
			im.counts.Unpublished = len(toUnpublish)
			return errRollback
		})
		if err != nil && !errors.Is(err, errRollback) {
			return problem.As(err)
		}
		return nil
	}
	for i, plan := range all {
		im.prepareImages(plan)
		if err := im.a.DB.WriteTx(im.ctx, func(tx *db.Tx) error { return apply(tx, plan) }); err != nil {
			return problem.As(err)
		}
		if im.progress != nil {
			im.progress(i+1, len(all))
		}
	}
	for _, ext := range toUnpublish {
		res, prob := im.a.WriteOp(im.ctx, im.allowed, func(w *writer) (*IngestResult, *problem.Problem) {
			return w.SetStatus("unpublish", KindProduct, ext)
		})
		if prob != nil {
			im.add("products.csv", 0, "", prob.ObxError, "could not unpublish "+ext+": "+problemDetail(prob), ext, false)
			continue
		}
		if res.Result == "updated" {
			im.counts.Unpublished++
		}
	}
	return nil
}

// ---- the import job -----------------------------------------------------------------

func (a *App) runImportJob(jc *jobContext) (any, error) {
	var p importPayload
	if err := json.Unmarshal([]byte(jc.job.Payload), &p); err != nil {
		return nil, err
	}
	if p.SourceID != "" && len(p.Files) == 0 {
		unchanged, prob := a.fetchSource(jc.ctx, jc.job.ID, &p)
		_, _ = a.DB.Exec(jc.ctx, `UPDATE jobs SET payload = ? WHERE id = ?`, string(mustJSON(p)), jc.job.ID)
		if prob != nil {
			res := &importResult{Mode: p.Options.Mode, DryRun: p.Options.DryRun, Files: []map[string]any{}, Error: prob}
			return res, errors.New(problemDetail(prob))
		}
		if unchanged {
			return &importResult{Unchanged: true, Mode: p.Options.Mode, DryRun: p.Options.DryRun, Files: []map[string]any{}}, nil
		}
	}
	defer func() {
		for _, f := range p.Files {
			_ = a.Store.Delete(context.Background(), f.Key)
		}
	}()
	s := a.Settings(jc.ctx)
	im := &importer{a: a, ctx: jc.ctx, opts: p.Options, allowed: p.Allowed, byExt: map[string]*productPlan{}, extra: map[string]*productPlan{}}
	im.lang = p.Options.DefaultLanguage
	if im.lang == "" {
		im.lang = s.ContentLanguages()[0]
	}
	if im.opts.Mode == "" {
		im.opts.Mode = "upsert"
	}
	im.progress = func(done, total int) { jc.progress(done, &total) }
	res := &importResult{Mode: im.opts.Mode, DryRun: im.opts.DryRun, Files: []map[string]any{}}
	for _, f := range p.Files {
		res.Files = append(res.Files, map[string]any{"name": f.Name, "bytes": f.Bytes, "sha256": nullIfEmpty(f.SHA256)})
	}
	var tmpFiles []*os.File
	defer func() {
		for _, f := range tmpFiles {
			f.Close()
		}
	}()
	productsCSV, assetsCSV, images, prob := a.importInputs(jc.ctx, p, &tmpFiles)
	if prob == nil {
		prob = im.run(productsCSV, assetsCSV, images)
	}
	res.Counts = im.counts
	items := make([]any, len(im.errors))
	for i := range im.errors {
		items[i] = im.errors[i]
	}
	if err := jc.addItems(0, items, func(x any) bool { return x.(ImportRowError).Severity == "error" }); err != nil {
		return nil, err
	}
	if prob != nil {
		res.Error = prob
		return res, errors.New(problemDetail(prob))
	}
	if p.SourceID != "" && !p.Options.DryRun {
		a.recordFingerprint(jc.ctx, p.SourceID, p.Fingerprint)
	}
	return res, nil
}

// importInputs loads the uploaded files of an import from the blob store.
func (a *App) importInputs(ctx context.Context, p importPayload, tmp *[]*os.File) (productsCSV, assetsCSV []byte, images []*imageSource, prob *problem.Problem) {
	readAll := func(key string) ([]byte, error) {
		obj, err := a.Store.Open(ctx, key)
		if err != nil {
			return nil, err
		}
		defer obj.Close()
		return io.ReadAll(io.LimitReader(obj, 256<<20))
	}
	for _, f := range p.Files {
		switch f.Field {
		case "csv":
			b, err := readAll(f.Key)
			if err != nil {
				return nil, nil, nil, problem.New("import_invalid", "products.csv cannot be read.")
			}
			productsCSV = b
		case "assets_csv":
			b, err := readAll(f.Key)
			if err != nil {
				return nil, nil, nil, problem.New("import_invalid", "assets.csv cannot be read.")
			}
			assetsCSV = b
		case "package", "images":
			if f.Field == "images" && !strings.HasSuffix(strings.ToLower(f.Name), ".zip") {
				key, name, size := f.Key, f.Name, f.Bytes
				images = append(images, &imageSource{Name: name, Size: size, Open: func() (io.ReadCloser, error) {
					return a.Store.Open(ctx, key)
				}})
				continue
			}
			zf, err := a.openZip(ctx, f.Key, tmp)
			if err != nil {
				return nil, nil, nil, problem.New("import_invalid", "%s is not a readable zip file.", f.Name)
			}
			pc, ac, imgs, zp := readPackage(zf, f.Field == "package")
			if zp != nil {
				return nil, nil, nil, zp
			}
			if pc != nil {
				productsCSV = pc
			}
			if ac != nil {
				assetsCSV = ac
			}
			images = append(images, imgs...)
		}
	}
	for i := range p.Manifest {
		m := p.Manifest[i]
		images = append(images, &imageSource{Name: m.File, Manifest: &m})
	}
	return productsCSV, assetsCSV, images, nil
}

func (a *App) openZip(ctx context.Context, key string, tmp *[]*os.File) (*zip.Reader, error) {
	obj, err := a.Store.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	f, err := os.CreateTemp("", "obx-import-*.zip")
	if err != nil {
		return nil, err
	}
	os.Remove(f.Name())
	*tmp = append(*tmp, f)
	n, err := io.Copy(f, obj)
	if err != nil {
		return nil, err
	}
	return zip.NewReader(f, n)
}

const (
	maxZipEntries      = 100000
	maxZipUncompressed = 10 * MaxUploadBytes * 3
)

// readPackage extracts products.csv, assets.csv and images from a zip,
// refusing unsafe archives (zip-slip, symlinks, bombs).
func readPackage(z *zip.Reader, isPackage bool) (productsCSV, assetsCSV []byte, images []*imageSource, prob *problem.Problem) {
	if len(z.File) > maxZipEntries {
		return nil, nil, nil, problem.New("import_invalid", "The zip has more than %d entries.", maxZipEntries)
	}
	var total uint64
	var csvs []*zip.File
	for _, f := range z.File {
		if err := importfmt.SafeEntry(f); err != nil {
			return nil, nil, nil, problem.New("import_invalid", "Unsafe zip entry: %s", err.Error())
		}
		total += f.UncompressedSize64
		if total > uint64(maxZipUncompressed) {
			return nil, nil, nil, problem.New("import_invalid", "The zip unpacks to more than %d bytes.", maxZipUncompressed)
		}
	}
	for _, f := range z.File {
		if f.FileInfo().IsDir() || importfmt.Ignored(f.Name) {
			continue
		}
		base := path.Base(strings.ReplaceAll(f.Name, "\\", "/"))
		lower := strings.ToLower(base)
		switch {
		case strings.HasSuffix(lower, ".csv") && isPackage:
			csvs = append(csvs, f)
		case importfmt.IsImage(base):
			zf := f
			images = append(images, &imageSource{Name: base, Size: int64(f.UncompressedSize64), Open: func() (io.ReadCloser, error) {
				rc, err := zf.Open()
				if err != nil {
					return nil, err
				}
				return struct {
					io.Reader
					io.Closer
				}{io.LimitReader(rc, MaxUploadBytes+1), rc}, nil
			}})
		}
	}
	read := func(f *zip.File) ([]byte, *problem.Problem) {
		rc, err := f.Open()
		if err != nil {
			return nil, problem.New("import_invalid", "%s cannot be read.", f.Name)
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, 256<<20))
		if err != nil {
			return nil, problem.New("import_invalid", "%s cannot be read.", f.Name)
		}
		return b, nil
	}
	var other []*zip.File
	for _, f := range csvs {
		switch strings.ToLower(path.Base(f.Name)) {
		case "products.csv":
			if productsCSV, prob = read(f); prob != nil {
				return
			}
		case "assets.csv":
			if assetsCSV, prob = read(f); prob != nil {
				return
			}
		default:
			other = append(other, f)
		}
	}
	if productsCSV == nil && len(other) == 1 {
		if productsCSV, prob = read(other[0]); prob != nil {
			return
		}
	}
	return productsCSV, assetsCSV, images, nil
}

// ---- starting imports ---------------------------------------------------------------

// stageFile stores an uploaded import file in the blob store.
func (a *App) stageFile(ctx context.Context, jobKey, field, name string, r io.Reader, limit int64) (*importFile, error) {
	f, sha, n, err := spool(r, limit)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	name = sanitizeFilename(name)
	key := "imports/" + jobKey + "/" + field + "/" + strconv.Itoa(int(a.now().UnixNano()%1e9)) + "-" + name
	if err := a.Store.Put(ctx, key, f, n, "application/octet-stream"); err != nil {
		return nil, err
	}
	return &importFile{Field: field, Name: name, Key: key, Bytes: n, SHA256: sha}, nil
}

// importRunJSON renders the ImportRun schema for an import job.
func (a *App) importRunJSON(ctx context.Context, j *jobRow) map[string]any {
	var res importResult
	var payload importPayload
	_ = json.Unmarshal([]byte(j.Payload), &payload)
	res.Mode, res.DryRun = payload.Options.Mode, payload.Options.DryRun
	if res.Mode == "" {
		res.Mode = "upsert"
	}
	res.Files = []map[string]any{}
	for _, f := range payload.Files {
		res.Files = append(res.Files, map[string]any{"name": f.Name, "bytes": f.Bytes, "sha256": nullIfEmpty(f.SHA256)})
	}
	for _, m := range payload.Manifest {
		res.Files = append(res.Files, map[string]any{"name": m.File, "bytes": 0, "sha256": m.SHA256})
	}
	if j.Result != nil {
		_ = json.Unmarshal([]byte(*j.Result), &res)
	}
	trigger := "api"
	if j.Trigger != nil {
		trigger = *j.Trigger
	}
	status := j.Status
	out := map[string]any{
		"id": j.ID, "source_id": j.SourceID, "trigger": trigger, "status": status, "mode": res.Mode, "dry_run": res.DryRun,
		"created_at": j.CreatedAt, "started_at": j.StartedAt, "finished_at": j.FinishedAt, "files": res.Files,
		"counts": map[string]any{"rows": res.Counts.Rows, "files": res.Counts.Files, "created": res.Counts.Created, "updated": res.Counts.Updated,
			"unchanged": res.Counts.Unchanged, "unpublished": res.Counts.Unpublished, "errors": res.Counts.Errors, "warnings": res.Counts.Warnings},
		"error": nil, "errors_preview": []any{},
	}
	if res.Unchanged {
		out["x-node-go-unchanged"] = true
	}
	if res.Error != nil {
		out["error"] = res.Error
	} else if j.Status == "failed" && j.Error != nil {
		out["error"] = problem.New("internal_error", "%s", *j.Error)
	}
	rs, err := a.DB.Query(ctx, `SELECT item FROM job_items WHERE job_id = ? AND is_error = 1 ORDER BY idx LIMIT 50`, j.ID)
	if err == nil {
		var prev []any
		for rs.Next() {
			var s string
			if rs.Scan(&s) == nil {
				var e ImportRowError
				_ = json.Unmarshal([]byte(s), &e)
				prev = append(prev, e)
			}
		}
		rs.Close()
		if prev != nil {
			out["errors_preview"] = prev
		}
	}
	return out
}

// ensure blobstore is referenced (key helper used by exports too).
var _ = blobstore.BlobKey
