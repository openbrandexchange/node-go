package node

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openbrandexchange/node-go/internal/blobstore"
	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/problem"
)

const downloadTTL = 7 * 24 * time.Hour

// chooseVariant resolves ?variant= for portal downloads.
func chooseVariant(v *Viewer, param string) (string, *problem.Problem) {
	switch param {
	case "":
		if v.OriginalAllowed {
			return "original", nil
		}
		return "web", nil
	case "original":
		if !v.OriginalAllowed {
			return "", problem.New("original_not_allowed", "Original files are not available to you.")
		}
		return "original", nil
	case "web", "square":
		return param, nil
	}
	return "", problem.New("invalid_request", "variant must be original, web or square.")
}

// assetFile returns the stored file and download name of an asset in a
// variant.
func assetFile(as *Asset, variant string) (*Rendition, string) {
	r := as.Renditions[variant]
	if r == nil {
		return nil, ""
	}
	name := as.Filename
	if variant != "original" {
		name = renditionFilename(as.Filename, variant)
	}
	return r, sanitizeFilename(name)
}

func (a *App) handlePortalAssetDownload(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalViewer(w, r)
	if !ok {
		return
	}
	variant, p := chooseVariant(v, r.URL.Query().Get("variant"))
	if p != nil {
		problem.Write(w, p)
		return
	}
	row, err := a.visibleAsset(r.Context(), v, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if row == nil {
		failP(w, "not_found", "Image not found.")
		return
	}
	as := a.renderAsset(row.Rep, row.UpdatedAt, &Viewer{Kind: "admin", OriginalAllowed: true})
	rend, name := assetFile(as, variant)
	if rend == nil {
		failP(w, "not_found", "Image not found.")
		return
	}
	if err := a.serveBlob(w, r, rend.SHA256, rend.ContentType, contentDisposition("attachment", name)); err != nil {
		a.fail(w, r, err)
		return
	}
	if r.Method == http.MethodGet && !isContinuationRange(r) {
		a.logDownload(r.Context(), downloadEvent{Kind: "image", Viewer: v, Variant: variant, ProductID: as.ProductID, AssetID: as.ID, Files: 1, Bytes: rend.Bytes})
	}
}

// writeZip streams files into a zip on w.
func (a *App) writeZip(ctx context.Context, w io.Writer, files []zipFile, extra map[string][]byte) (int, int64, error) {
	zw := zip.NewWriter(w)
	seen := map[string]bool{}
	var total int64
	var written []zipFile
	for _, f := range files {
		p := f.Path
		for i := 2; seen[p]; i++ {
			base, ext := f.Path, ""
			if j := strings.LastIndexByte(f.Path, '.'); j > strings.LastIndexByte(f.Path, '/') {
				base, ext = f.Path[:j], f.Path[j:]
			}
			p = fmt.Sprintf("%s-%d%s", base, i, ext)
		}
		seen[p] = true
		obj, err := a.Store.Open(ctx, blobstore.BlobKey(f.SHA256))
		if err != nil {
			return 0, 0, err
		}
		zf, err := zw.CreateHeader(&zip.FileHeader{Name: p, Method: zip.Store, Modified: a.now()})
		if err != nil {
			obj.Close()
			return 0, 0, err
		}
		n, err := io.Copy(zf, obj)
		obj.Close()
		if err != nil {
			return 0, 0, err
		}
		total += n
		f.Path = p
		written = append(written, f)
	}
	if rows := downloadAssetsCSV(written); rows != nil {
		if extra == nil {
			extra = map[string][]byte{}
		}
		extra["assets.csv"] = rows
	}
	names := make([]string, 0, len(extra))
	for k := range extra {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		zf, err := zw.Create(k)
		if err != nil {
			return 0, 0, err
		}
		if _, err := zf.Write(extra[k]); err != nil {
			return 0, 0, err
		}
	}
	return len(files), total, zw.Close()
}

// zipFilesFor lists the files of products in a variant; folders=true puts
// each product in a folder named after its style number.
func (a *App) zipFilesFor(ctx context.Context, v *Viewer, productIDs []string, variant string, folders bool) ([]zipFile, error) {
	var out []zipFile
	full := &Viewer{Kind: v.Kind, RetailerID: v.RetailerID, BrandIDs: v.BrandIDs, OriginalAllowed: true, UserID: v.UserID}
	for i := 0; i < len(productIDs); i += 100 {
		products, err := a.productDetails(ctx, full, productIDs[i:min(i+100, len(productIDs))])
		if err != nil {
			return nil, err
		}
		for _, p := range products {
			for k := range *p.Assets {
				as := &(*p.Assets)[k]
				rend, name := assetFile(as, variant)
				if rend == nil {
					continue
				}
				path := name
				if folders {
					path = safeZipSegment(p.StyleNumber) + "/" + name
				}
				out = append(out, zipFile{Path: path, SHA256: rend.SHA256, Bytes: rend.Bytes, Asset: as, Style: p.StyleNumber})
			}
		}
	}
	return out, nil
}

func (a *App) handlePortalProductZip(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalViewer(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	variant, p := chooseVariant(v, r.URL.Query().Get("variant"))
	if p != nil {
		problem.Write(w, p)
		return
	}
	items, err := a.productDetails(ctx, v, []string{r.PathValue("id")})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if len(items) == 0 {
		failP(w, "not_found", "Product not found.")
		return
	}
	files, err := a.zipFilesFor(ctx, v, []string{items[0].ID}, variant, false)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", items[0].StyleNumber+".zip"))
	n, total, err := a.writeZip(ctx, w, files, nil)
	if err != nil {
		a.Log.Warn("zip stream failed", "error", err)
		return
	}
	a.logDownload(ctx, downloadEvent{Kind: "product_zip", Viewer: v, Variant: variant, ProductID: items[0].ID, Files: n, Bytes: total})
}

func (a *App) handleSelectionZip(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalViewer(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	variant, p := chooseVariant(v, r.URL.Query().Get("variant"))
	if p != nil {
		problem.Write(w, p)
		return
	}
	ids := a.visibleProductIDs(ctx, v, a.selectionIDs(ctx, sessionFrom(ctx).User.ID))
	if len(ids) == 0 {
		failP(w, "invalid_request", "Your selection is empty.")
		return
	}
	files, err := a.zipFilesFor(ctx, v, ids, variant, true)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if len(files) > syncZipMaxFiles {
		failP(w, "limit_exceeded", "The selection has %d images; downloads of more than %d images are prepared in the background.", len(files), syncZipMaxFiles)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", "selection-"+a.now().Format("2006-01-02")+".zip"))
	n, total, err := a.writeZip(ctx, w, files, nil)
	if err != nil {
		a.Log.Warn("zip stream failed", "error", err)
		return
	}
	a.logDownload(ctx, downloadEvent{Kind: "selection_zip", Viewer: v, Variant: variant, Files: n, Bytes: total})
}

// downloadAssetsCSV builds the assets.csv of a portal download
// (import-format.md §2.3): one row per image in the zip with its effective
// rights, embargo and AI provenance, in the import format's columns, so they
// travel with the files. nil when the zip has no images.
func downloadAssetsCSV(files []zipFile) []byte {
	var images []zipFile
	langSet := map[string]bool{}
	for _, f := range files {
		if f.Asset == nil {
			continue
		}
		images = append(images, f)
		for l := range f.Asset.AltText {
			langSet[l] = true
		}
	}
	if len(images) == 0 {
		return nil
	}
	langs := make([]string, 0, len(langSet))
	for l := range langSet {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	header := []string{"file", "style_number", "color_code", "position", "role", "view"}
	for _, l := range langs {
		header = append(header, "alt_text:"+l)
	}
	header = append(header, "license", "license_text", "channels", "territories", "embargo_until", "expires_at", "credit",
		"model_release", "ai_generated", "ai_edited", "source", "c2pa", "generator")
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	_ = cw.Write(header)
	for _, f := range images {
		as := f.Asset
		row := []string{path.Base(f.Path), f.Style, str(as.ColorCode), strconv.Itoa(as.Position), as.Role, str(as.View)}
		for _, l := range langs {
			row = append(row, as.AltText[l])
		}
		release := ""
		if as.Rights.ModelRelease != nil {
			release = strconv.FormatBool(*as.Rights.ModelRelease)
		}
		row = append(row, as.Rights.License, str(as.Rights.LicenseText), strings.Join(as.Rights.Channels, "|"),
			strings.Join(as.Rights.Territories, "|"), str(as.Rights.EmbargoUntil), str(as.Rights.ExpiresAt), str(as.Rights.Credit),
			release, strconv.FormatBool(as.Provenance.AIGenerated), strconv.FormatBool(as.Provenance.AIEdited),
			as.Provenance.Source, as.Provenance.C2PA, str(as.Provenance.Generator))
		for i, c := range row {
			row[i] = csvSafe(c)
		}
		_ = cw.Write(row)
	}
	cw.Flush()
	return buf.Bytes()
}

// ---- CSV export --------------------------------------------------------------------------

var numericCell = regexp.MustCompile(`^[+-]?[0-9]+([.,][0-9]+)?$`)

// csvSafe neutralises spreadsheet formulas.
func csvSafe(s string) string {
	if s == "" || numericCell.MatchString(s) {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

func formatMaterials(ms []MaterialPart) string {
	var parts []string
	for _, m := range ms {
		var fs []string
		for _, f := range m.Fibers {
			fs = append(fs, strconv.FormatFloat(f.Percentage, 'f', -1, 64)+"% "+f.Fiber)
		}
		parts = append(parts, m.Part+": "+strings.Join(fs, ", "))
	}
	return strings.Join(parts, "; ")
}

// buildCSV renders canonical CSV rows (one per visible variant).
func (a *App) buildCSV(ctx context.Context, v *Viewer, productIDs []string, delimiter rune) ([]byte, error) {
	langs := a.Settings(ctx).ContentLanguages()
	names := a.brandNames(ctx)
	var products []*Product
	full := &Viewer{Kind: v.Kind, RetailerID: v.RetailerID, BrandIDs: v.BrandIDs, OriginalAllowed: true}
	for i := 0; i < len(productIDs); i += 100 {
		ps, err := a.productDetails(ctx, full, productIDs[i:min(i+100, len(productIDs))])
		if err != nil {
			return nil, err
		}
		products = append(products, ps...)
	}
	attrKeys := map[string]bool{}
	for _, p := range products {
		for k := range p.Attributes {
			attrKeys[k] = true
		}
	}
	var attrs []string
	for k := range attrKeys {
		attrs = append(attrs, k)
	}
	sort.Strings(attrs)
	header := []string{"brand", "product_id", "style_number"}
	for _, l := range langs {
		header = append(header, "name:"+l)
	}
	for _, l := range langs {
		header = append(header, "description:"+l)
	}
	header = append(header, "item_type", "target_group", "season", "collection", "gpc_brick", "materials", "care", "country_of_origin", "dpp_url", "status")
	for _, k := range attrs {
		header = append(header, "attr:"+k)
	}
	header = append(header, "variant_id", "gtin", "sku", "color_code")
	for _, l := range langs {
		header = append(header, "color_name:"+l)
	}
	header = append(header, "supplier_color", "size", "size_system", "variant_status", "image_files")
	var buf bytes.Buffer
	buf.WriteString("\ufeff")
	cw := csv.NewWriter(&buf)
	cw.Comma = delimiter
	cw.UseCRLF = true
	_ = cw.Write(header)
	for _, p := range products {
		base := []string{names[p.BrandID], p.ExternalID, p.StyleNumber}
		for _, l := range langs {
			base = append(base, p.Name[l])
		}
		for _, l := range langs {
			base = append(base, p.Description[l])
		}
		var care []string
		for _, c := range p.Care {
			care = append(care, c.Code)
		}
		base = append(base, deref(p.ItemType), deref(p.TargetGroup), deref(p.Season), deref(p.Collection), deref(p.GPCBrick),
			formatMaterials(p.Materials), strings.Join(care, "|"), deref(p.CountryOfOrigin), deref(p.DPPURL), "published")
		for _, k := range attrs {
			val := ""
			if x, ok := p.Attributes[k]; ok && x != nil {
				switch t := x.(type) {
				case string:
					val = t
				case map[string]any:
					b, _ := json.Marshal(t)
					val = string(b)
				default:
					val = fmt.Sprint(t)
				}
			}
			base = append(base, val)
		}
		filesByColor := map[string][]string{}
		for _, as := range *p.Assets {
			key := ""
			if as.ColorCode != nil {
				key = *as.ColorCode
			}
			filesByColor[key] = append(filesByColor[key], as.Filename)
		}
		write := func(cells []string) {
			for i := range cells {
				cells[i] = csvSafe(cells[i])
			}
			_ = cw.Write(cells)
		}
		if len(*p.Variants) == 0 {
			row := append(append([]string{}, base...), "", "", "", "")
			for range langs {
				row = append(row, "")
			}
			row = append(row, "", "", "", "", strings.Join(filesByColor[""], "|"))
			write(row)
			continue
		}
		for _, vv := range *p.Variants {
			row := append(append([]string{}, base...), vv.ExternalID, deref(vv.GTINOriginal), deref(vv.SKU), vv.Color.Code)
			for _, l := range langs {
				row = append(row, vv.Color.Name[l])
			}
			files := append(append([]string{}, filesByColor[vv.Color.Code]...), filesByColor[""]...)
			row = append(row, deref(vv.Color.SupplierColor), deref(vv.Size), deref(vv.SizeSystem), "published", strings.Join(files, "|"))
			write(row)
		}
	}
	cw.Flush()
	return buf.Bytes(), cw.Error()
}

func delimiterParam(r *http.Request) rune {
	if r.URL.Query().Get("delimiter") == "semicolon" {
		return ';'
	}
	return ','
}

func (a *App) sendCSV(w http.ResponseWriter, r *http.Request, v *Viewer, ids []string, name string) {
	ctx := r.Context()
	if !a.Settings(ctx).Portal.AllowArticleDataExport && !v.Preview() {
		failP(w, "forbidden", "The brand has switched off article data downloads.")
		return
	}
	data, err := a.buildCSV(ctx, v, ids, delimiterParam(r))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", name))
	_, _ = w.Write(data)
	a.logDownload(ctx, downloadEvent{Kind: "csv", Viewer: v, Files: 0, Bytes: int64(len(data))})
}

func (a *App) handleSelectionCSV(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalViewer(w, r)
	if !ok {
		return
	}
	ids := a.visibleProductIDs(r.Context(), v, a.selectionIDs(r.Context(), sessionFrom(r.Context()).User.ID))
	a.sendCSV(w, r, v, ids, "selection-"+a.now().Format("2006-01-02")+".csv")
}

func (a *App) filteredProductIDs(ctx context.Context, v *Viewer, f portalFilter, max int) ([]string, error) {
	where, args := f.where(v, "")
	rs, err := a.DB.Query(ctx, `SELECT p.id FROM products p`+where+` ORDER BY p.style_number, p.id LIMIT ?`, append(args, max)...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var ids []string
	for rs.Next() {
		var id string
		if rs.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (a *App) handleProductsCSV(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalViewer(w, r)
	if !ok {
		return
	}
	ids, err := a.filteredProductIDs(r.Context(), v, filterFromQuery(r.URL.Query()), 50000)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.sendCSV(w, r, v, ids, "products-"+a.now().Format("2006-01-02")+".csv")
}

// ---- background download jobs --------------------------------------------------------------

type downloadPayload struct {
	UserID         string   `json:"user_id"`
	Source         string   `json:"source"`
	ProductIDs     []string `json:"product_ids"`
	Variant        string   `json:"variant"`
	IncludeDataCSV bool     `json:"include_data_csv"`
	Delimiter      string   `json:"delimiter"`
}

type downloadResult struct {
	Bytes int64 `json:"bytes"`
	Files int   `json:"files"`
}

func (a *App) downloadJobJSON(j *jobRow) map[string]any {
	var p downloadPayload
	_ = json.Unmarshal([]byte(j.Payload), &p)
	status := j.Status
	if j.ExpiresAt != nil && db.Parse(*j.ExpiresAt).Before(a.now()) {
		status = "expired"
	}
	out := map[string]any{"id": j.ID, "status": status, "source": p.Source, "variant": p.Variant, "include_data_csv": p.IncludeDataCSV,
		"created_at": j.CreatedAt, "finished_at": j.FinishedAt, "expires_at": j.ExpiresAt, "progress": map[string]any{"done": j.Done, "total": j.Total},
		"file_url": nil, "bytes": nil, "files": nil, "error": j.Error}
	if status == "succeeded" && j.Result != nil {
		var res downloadResult
		_ = json.Unmarshal([]byte(*j.Result), &res)
		out["file_url"] = "/admin/api/portal/download-jobs/" + j.ID + "/file"
		out["bytes"], out["files"] = res.Bytes, res.Files
	}
	return out
}

func (a *App) handleDownloadJobs(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.portalViewer(w, r); !ok {
		return
	}
	ctx := r.Context()
	rs, err := a.DB.Query(ctx, `SELECT `+jobCols+` FROM jobs WHERE kind = 'download' AND owner_type = 'user' AND owner_id = ? AND created_at > ? ORDER BY created_at DESC`,
		sessionFrom(ctx).User.ID, db.Fmt(a.now().Add(-downloadTTL)))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items := []any{}
	for rs.Next() {
		if j, err := scanJob(rs); err == nil {
			items = append(items, a.downloadJobJSON(j))
		}
	}
	rs.Close()
	writeJSON(w, 200, map[string]any{"items": items})
}

func (a *App) handleDownloadJobCreate(w http.ResponseWriter, r *http.Request) {
	v, ok := a.portalViewer(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var body struct {
		Source         string       `json:"source"`
		ProductIDs     []string     `json:"product_ids"`
		Filter         portalFilter `json:"filter"`
		Variant        string       `json:"variant"`
		IncludeDataCSV *bool        `json:"include_data_csv"`
	}
	if p := readJSON(r, &body, 4<<20); p != nil {
		problem.Write(w, p)
		return
	}
	variant, p := chooseVariant(v, body.Variant)
	if p != nil || body.Variant == "" {
		if p == nil {
			p = problem.New("validation_failed", "Choose original, web or square.").Field("/variant", "validation_failed", "required")
		}
		problem.Write(w, p)
		return
	}
	uid := sessionFrom(ctx).User.ID
	var ids []string
	switch body.Source {
	case "selection":
		ids = a.visibleProductIDs(ctx, v, a.selectionIDs(ctx, uid))
	case "products":
		if len(body.ProductIDs) > 10000 {
			failP(w, "limit_exceeded", "At most 10,000 products.")
			return
		}
		ids = a.visibleProductIDs(ctx, v, body.ProductIDs)
	case "filter":
		var err error
		ids, err = a.filteredProductIDs(ctx, v, body.Filter, 100000)
		if err != nil {
			a.fail(w, r, err)
			return
		}
	default:
		failP(w, "validation_failed", "source must be selection, products or filter.")
		return
	}
	if len(ids) == 0 {
		failP(w, "validation_failed", "There are no products to download.")
		return
	}
	var running int
	_ = a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM jobs WHERE kind = 'download' AND owner_id = ? AND status IN ('queued', 'running')`, uid).Scan(&running)
	if running >= 2 {
		failP(w, "conflict", "Two downloads are already being prepared; wait until one is ready.")
		return
	}
	include := body.IncludeDataCSV == nil || *body.IncludeDataCSV
	delim := "comma"
	if strings.HasPrefix(v.Language, "nl") {
		delim = "semicolon"
	}
	job, err := a.createJob(ctx, jobSpec{Kind: "download", OwnerType: "user", OwnerID: uid, Total: len(ids),
		Payload: downloadPayload{UserID: uid, Source: body.Source, ProductIDs: ids, Variant: variant, IncludeDataCSV: include, Delimiter: delim}})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 202, a.downloadJobJSON(job))
}

func (a *App) ownDownloadJob(w http.ResponseWriter, r *http.Request) (*Viewer, *jobRow, bool) {
	v, ok := a.portalViewer(w, r)
	if !ok {
		return nil, nil, false
	}
	j, err := a.loadJob(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return nil, nil, false
	}
	if j == nil || j.Kind != "download" || j.OwnerID == nil || *j.OwnerID != sessionFrom(r.Context()).User.ID {
		failP(w, "not_found", "Download not found.")
		return nil, nil, false
	}
	return v, j, true
}

func (a *App) handleDownloadJob(w http.ResponseWriter, r *http.Request) {
	_, j, ok := a.ownDownloadJob(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, a.downloadJobJSON(j))
}

func (a *App) handleDownloadJobFile(w http.ResponseWriter, r *http.Request) {
	v, j, ok := a.ownDownloadJob(w, r)
	if !ok {
		return
	}
	if j.ExpiresAt != nil && db.Parse(*j.ExpiresAt).Before(a.now()) {
		failP(w, "export_expired", "This download expired; start a new one.")
		return
	}
	if j.Status != "succeeded" {
		failP(w, "not_found", "The download is not ready yet.")
		return
	}
	obj, err := a.Store.Open(r.Context(), "downloads/"+j.ID+".zip")
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			failP(w, "export_expired", "The file was deleted.")
			return
		}
		a.fail(w, r, err)
		return
	}
	defer obj.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", "download-"+j.CreatedAt[:10]+".zip"))
	http.ServeContent(w, r, "", time.Time{}, obj)
	if r.Method == http.MethodGet && !isContinuationRange(r) {
		var res downloadResult
		if j.Result != nil {
			_ = json.Unmarshal([]byte(*j.Result), &res)
		}
		a.logDownload(r.Context(), downloadEvent{Kind: "bulk_zip", Viewer: v, Files: res.Files, Bytes: res.Bytes})
	}
}

func (a *App) runDownloadJob(jc *jobContext) (any, error) {
	var p downloadPayload
	if err := json.Unmarshal([]byte(jc.job.Payload), &p); err != nil {
		return nil, err
	}
	ctx := jc.ctx
	u, err := a.loadUser(ctx, a.DB, "id = ?", p.UserID)
	if err != nil || u == nil || u.Status != "active" {
		return nil, errors.New("the account is no longer active")
	}
	v, prob := a.viewerForUser(ctx, u)
	if prob != nil {
		return nil, errors.New(problemDetail(prob))
	}
	ids := a.visibleProductIDs(ctx, v, p.ProductIDs)
	files, err := a.zipFilesFor(ctx, v, ids, p.Variant, true)
	if err != nil {
		return nil, err
	}
	b := &zipBuilder{a: a, ctx: ctx, max: 1 << 62}
	defer b.cleanup()
	var written []zipFile
	b.onEntry = func(_ int, f zipFile) { written = append(written, f) }
	total := len(files)
	for i, f := range files {
		if err := b.add(f); err != nil {
			return nil, err
		}
		if i%20 == 0 {
			jc.progress(i, &total)
		}
	}
	if rows := downloadAssetsCSV(written); rows != nil {
		if err := b.addBytes("assets.csv", rows); err != nil {
			return nil, err
		}
	}
	if p.IncludeDataCSV && a.Settings(ctx).Portal.AllowArticleDataExport {
		d := ','
		if p.Delimiter == "semicolon" {
			d = ';'
		}
		data, err := a.buildCSV(ctx, v, ids, d)
		if err != nil {
			return nil, err
		}
		if err := b.addBytes("data.csv", data); err != nil {
			return nil, err
		}
	}
	if err := b.finish(); err != nil {
		return nil, err
	}
	f := b.parts[0]
	size, _ := f.Seek(0, io.SeekEnd)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if err := a.Store.Put(ctx, "downloads/"+jc.job.ID+".zip", f, size, "application/zip"); err != nil {
		return nil, err
	}
	jc.progress(total, &total)
	_, _ = a.DB.Exec(ctx, `UPDATE jobs SET expires_at = ? WHERE id = ?`, db.Fmt(a.now().Add(downloadTTL)), jc.job.ID)
	return downloadResult{Bytes: size, Files: len(files)}, nil
}

func (a *App) notifyDownloadReady(ctx context.Context, j *jobRow) {
	if j.Status != "succeeded" || j.OwnerID == nil {
		return
	}
	u, _ := a.loadUser(ctx, a.DB, "id = ?", *j.OwnerID)
	if u == nil {
		return
	}
	var res downloadResult
	if j.Result != nil {
		_ = json.Unmarshal([]byte(*j.Result), &res)
	}
	a.sendMail(ctx, u.Email, u.Language, mailZipReady, map[string]string{"name": u.Name, "files": strconv.Itoa(res.Files),
		"expires": mailDate(a.now().Add(7 * 24 * time.Hour)), "url": a.Cfg.PublicURL + "/downloads"})
}
