package node

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/gtin"
	"github.com/openbrandexchange/node-go/internal/ids"
	"github.com/openbrandexchange/node-go/internal/problem"
	"github.com/openbrandexchange/node-go/internal/schemas"
)

var knownFields = map[string][]string{
	KindBrand:   {"name", "logo_url", "website", "description", "status"},
	KindProduct: {"brand_external_id", "style_number", "name", "description", "item_type", "gpc_brick", "target_group", "season", "collection", "materials", "care", "country_of_origin", "dpp_url", "attributes", "status"},
	KindVariant: {"product_external_id", "gtin", "sku", "color", "size", "size_system", "status"},
	KindAsset:   {"product_external_id", "color_code", "variant_external_ids", "media_type", "role", "view", "position", "filename", "alt_text", "rights", "provenance", "files", "status"},
}

var inputSchema = map[string]string{KindBrand: "BrandInput", KindProduct: "ProductInput", KindVariant: "VariantInput", KindAsset: "AssetInput"}

var idPrefix = map[string]string{KindBrand: "brd", KindProduct: "prd", KindVariant: "var", KindAsset: "ast"}

// ValidExternalID checks the ExternalId rules (1-200 chars, no control
// characters, no '/'); the value is trimmed first.
func ValidExternalID(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > 200 || !utf8.ValidString(s) {
		return s, false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '/' {
			return s, false
		}
	}
	return s, true
}

func normalizeGTIN(s string) (string, bool) { return gtin.Normalize(s) }

// lenientValidate validates doc (round-tripped through JSON so any Go
// numeric type works) against a schema, ignoring unknown properties.
func lenientValidate(name string, doc any) []schemas.FieldError {
	raw, err := json.Marshal(doc)
	if err != nil {
		return []schemas.FieldError{{Message: err.Error()}}
	}
	return schemas.Lenient().ValidateJSON(name, raw)
}

// validationProblem converts schema errors to a problem.
func validationProblem(errs []schemas.FieldError) *problem.Problem {
	p := problem.New("validation_failed", "The request body is not valid.")
	for _, e := range errs {
		p.Field(e.Pointer, "validation_failed", e.Message)
	}
	return p
}

// decodeInto re-encodes a decoded document into a typed value.
func decodeInto(doc any, v any) error {
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v)
}

// normalizeNumbers turns json.Number into float64/int64-friendly values so
// stored JSON is canonical.
func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			t[k] = normalizeNumbers(c)
		}
	case []any:
		for i := range t {
			t[i] = normalizeNumbers(t[i])
		}
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
	}
	return v
}

func (w *writer) brandAllowed(brandExternalID string) bool {
	if w.allowedBrands == nil {
		return true
	}
	for _, b := range w.allowedBrands {
		if b == brandExternalID {
			return true
		}
	}
	return false
}

func (w *writer) brandExternalIDOf(brandID string) string {
	var e string
	_ = w.tx.QueryRow(w.ctx, `SELECT external_id FROM brands WHERE id = ?`, brandID).Scan(&e)
	return e
}

func forbiddenBrand() *problem.Problem {
	return problem.New("forbidden", "This token may not write to this brand.")
}

// normalizeTime converts an RFC 3339 value to UTC.
func normalizeTime(s string) (string, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "", err
	}
	return t.UTC().Format(time.RFC3339Nano), nil
}

// prepared is a validated write: the canonical stored data, status and the
// derived columns.
type prepared struct {
	data      string
	status    *string
	brandID   string
	productID string
	cols      map[string]any
	warnings  []string
	files     map[string]FileRef
}

// prepare validates doc for kind and builds the stored write model.
func (w *writer) prepare(kind, ext string, doc map[string]any, existing *row) (*prepared, *problem.Problem) {
	if errs := lenientValidate(inputSchema[kind], doc); errs != nil {
		// Map GTIN pattern failures on variants to invalid_gtin.
		if kind == KindVariant {
			for _, e := range errs {
				if e.Pointer == "/gtin" {
					return nil, problem.New("invalid_gtin", "The GTIN must have 8, 12, 13 or 14 digits and a valid check digit.").Field("/gtin", "invalid_gtin", e.Message)
				}
			}
		}
		return nil, validationProblem(errs)
	}
	p := &prepared{cols: map[string]any{}}
	known := map[string]bool{}
	for _, k := range knownFields[kind] {
		known[k] = true
	}
	var unknown []string
	for k := range doc {
		if !known[k] && !strings.HasPrefix(k, "x-") {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		p.warnings = append(p.warnings, fmt.Sprintf("unknown field %q ignored", k))
	}
	if s, ok := doc["status"].(string); ok {
		p.status = &s
	}
	switch kind {
	case KindBrand:
		if !w.brandAllowed(ext) {
			return nil, forbiddenBrand()
		}
		var d BrandData
		if err := decodeInto(doc, &d); err != nil {
			return nil, problem.New("validation_failed", "%s", err.Error())
		}
		d.Name = strings.TrimSpace(d.Name)
		p.data = string(mustJSON(d))
		p.cols["name"] = d.Name
	case KindProduct:
		var d ProductData
		if err := decodeInto(doc, &d); err != nil {
			return nil, problem.New("validation_failed", "%s", err.Error())
		}
		bext := ""
		if v, ok := doc["brand_external_id"].(string); ok {
			bext = strings.TrimSpace(v)
		}
		if bext == "" {
			var n int
			_ = w.tx.QueryRow(w.ctx, `SELECT COUNT(*) FROM brands`).Scan(&n)
			if n == 1 {
				_ = w.tx.QueryRow(w.ctx, `SELECT external_id FROM brands`).Scan(&bext)
			} else {
				return nil, problem.New("validation_failed", "brand_external_id is required because this node has more than one brand.").
					Field("/brand_external_id", "validation_failed", "required")
			}
		}
		var brandID string
		if err := w.tx.QueryRow(w.ctx, `SELECT id FROM brands WHERE external_id = ?`, bext).Scan(&brandID); err != nil {
			return nil, problem.New("unknown_parent", "Brand %q does not exist.", bext).Field("/brand_external_id", "unknown_parent", "unknown brand")
		}
		if !w.brandAllowed(bext) || (existing != nil && !w.brandAllowed(w.brandExternalIDOf(existing.BrandID))) {
			return nil, forbiddenBrand()
		}
		d.BrandExternalID = bext
		d.StyleNumber = strings.TrimSpace(d.StyleNumber)
		if d.Attributes != nil {
			d.Attributes = normalizeNumbers(map[string]any(d.Attributes)).(map[string]any)
		}
		p.data = string(mustJSON(d))
		p.brandID = brandID
	case KindVariant:
		var d VariantData
		if err := decodeInto(doc, &d); err != nil {
			return nil, problem.New("validation_failed", "%s", err.Error())
		}
		var productID, brandID string
		if err := w.tx.QueryRow(w.ctx, `SELECT id, brand_id FROM products WHERE external_id = ?`, d.ProductExternalID).Scan(&productID, &brandID); err != nil {
			return nil, problem.New("unknown_parent", "Product %q does not exist.", d.ProductExternalID).Field("/product_external_id", "unknown_parent", "unknown product")
		}
		if !w.brandAllowed(w.brandExternalIDOf(brandID)) || (existing != nil && !w.brandAllowed(w.brandExternalIDOf(existing.BrandID))) {
			return nil, forbiddenBrand()
		}
		var norm any
		if d.GTIN != nil {
			g := strings.TrimSpace(*d.GTIN)
			d.GTIN = &g
			if !gtin.ValidCheckDigit(g) {
				return nil, problem.New("invalid_gtin", "GTIN %s has a wrong check digit.", g).Field("/gtin", "invalid_gtin", "wrong check digit")
			}
			n, _ := gtin.Normalize(g)
			var other string
			q := `SELECT external_id FROM variants WHERE gtin = ?`
			args := []any{n}
			if existing != nil {
				q += ` AND id <> ?`
				args = append(args, existing.ID)
			}
			if w.tx.QueryRow(w.ctx, q, args...).Scan(&other) == nil {
				return nil, problem.New("gtin_conflict", "GTIN %s already belongs to variant %q.", g, other).Field("/gtin", "gtin_conflict", "already in use")
			}
			norm = n
		}
		d.Color.Code = strings.TrimSpace(d.Color.Code)
		p.data = string(mustJSON(d))
		p.productID, p.brandID = productID, brandID
		p.cols["gtin"] = norm
		p.cols["color_code"] = d.Color.Code
		p.cols["sku"] = d.SKU
	case KindAsset:
		var d AssetData
		if err := decodeInto(doc, &d); err != nil {
			return nil, problem.New("validation_failed", "%s", err.Error())
		}
		var productID, brandID string
		if err := w.tx.QueryRow(w.ctx, `SELECT id, brand_id FROM products WHERE external_id = ?`, d.ProductExternalID).Scan(&productID, &brandID); err != nil {
			return nil, problem.New("unknown_parent", "Product %q does not exist.", d.ProductExternalID).Field("/product_external_id", "unknown_parent", "unknown product")
		}
		if !w.brandAllowed(w.brandExternalIDOf(brandID)) || (existing != nil && !w.brandAllowed(w.brandExternalIDOf(existing.BrandID))) {
			return nil, forbiddenBrand()
		}
		if d.MediaType == "" {
			d.MediaType = "image"
		}
		if d.ColorCode != nil {
			c := strings.TrimSpace(*d.ColorCode)
			if c == "" {
				d.ColorCode = nil
			} else {
				d.ColorCode = &c
			}
		}
		// rights / provenance: keep only the fields that differ from the
		// defaults (so a record read with defaults applied and written back
		// is unchanged); normalise timestamps.
		if raw, ok := doc["rights"].(map[string]any); ok {
			d.Rights = map[string]json.RawMessage{}
			defaults := map[string]json.RawMessage{}
			_ = json.Unmarshal(mustJSON(w.settings.Defaults.Rights), &defaults)
			for k, v := range raw {
				if strings.HasPrefix(k, "x-") {
					continue
				}
				if (k == "embargo_until" || k == "expires_at") && v != nil {
					s, err := normalizeTime(fmt.Sprint(v))
					if err != nil {
						return nil, problem.New("validation_failed", "Invalid %s.", k).Field("/rights/"+k, "validation_failed", "not an RFC 3339 timestamp")
					}
					v = s
				}
				val := mustJSON(normalizeNumbers(v))
				if def, ok := defaults[k]; ok && string(def) == string(val) {
					continue
				}
				d.Rights[k] = val
			}
			if len(d.Rights) == 0 {
				d.Rights = nil
			}
			if lic, _ := raw["license"].(string); lic == "custom" {
				if lt, _ := raw["license_text"].(string); strings.TrimSpace(lt) == "" {
					return nil, problem.New("validation_failed", "license_text is required for a custom licence.").Field("/rights/license_text", "validation_failed", "required when license is custom")
				}
			}
		} else {
			d.Rights = nil
		}
		if raw, ok := doc["provenance"].(map[string]any); ok {
			aiGen, _ := raw["ai_generated"].(bool)
			defSource := "photo"
			if aiGen {
				defSource = "generated"
			}
			defaults := map[string]string{"ai_generated": "false", "ai_edited": "false", "c2pa": `"none"`, "c2pa_manifest_url": "null", "generator": "null", "source": `"` + defSource + `"`}
			d.Provenance = map[string]json.RawMessage{}
			for k, v := range raw {
				if strings.HasPrefix(k, "x-") {
					continue
				}
				val := mustJSON(v)
				if defaults[k] == string(val) {
					continue
				}
				d.Provenance[k] = val
			}
			if len(d.Provenance) == 0 {
				d.Provenance = nil
			}
		} else {
			d.Provenance = nil
		}
		// files: omitted keeps the current files.
		if _, present := doc["files"]; !present && existing != nil {
			var cur AssetData
			_ = json.Unmarshal([]byte(existing.Data), &cur)
			d.Files = cur.Files
		} else if d.Files != nil {
			for name, ref := range d.Files {
				var bw, bh *int
				if err := w.tx.QueryRow(w.ctx, `SELECT width, height FROM blobs WHERE sha256 = ?`, ref.SHA256).Scan(&bw, &bh); err != nil {
					return nil, problem.New("blob_missing", "File %s (%s) has not been uploaded.", name, ref.SHA256).Field("/files/"+name+"/sha256", "blob_missing", "unknown sha256")
				}
				if bw != nil && bh != nil {
					// The node knows the dimensions from the file itself.
					ref.Width, ref.Height = nil, nil
				}
				d.Files[name] = ref
			}
			if len(d.Files) == 0 {
				d.Files = nil
			}
		}
		if d.Files != nil {
			if _, ok := d.Files["original"]; !ok {
				return nil, problem.New("validation_failed", "files needs an original.").Field("/files/original", "validation_failed", "required")
			}
		}
		// position uniqueness among the product's assets of this colour.
		q := `SELECT id, external_id FROM assets WHERE product_id = ? AND position = ?`
		args := []any{productID, d.Position}
		if d.ColorCode == nil {
			q += ` AND color_code IS NULL`
		} else {
			q += ` AND color_code = ?`
			args = append(args, *d.ColorCode)
		}
		if existing != nil {
			q += ` AND id <> ?`
			args = append(args, existing.ID)
		}
		rs, err := w.tx.Query(w.ctx, q, args...)
		if err != nil {
			return nil, problem.As(err)
		}
		var conflict string
		for rs.Next() {
			var id, e string
			_ = rs.Scan(&id, &e)
			if !w.positionExclude[id] {
				conflict = e
			}
		}
		rs.Close()
		if conflict != "" {
			return nil, problem.New("position_conflict", "Asset %q already has position %d for this product and colour.", conflict, d.Position).Field("/position", "position_conflict", "position in use")
		}
		p.data = string(mustJSON(d))
		p.productID, p.brandID = productID, brandID
		p.cols["color_code"] = d.ColorCode
		p.cols["position"] = d.Position
		var orig any
		if o, ok := d.Files["original"]; ok {
			orig = o.SHA256
		}
		p.cols["original_sha"] = orig
		p.files = d.Files
	}
	return p, nil
}

// Upsert creates or replaces an object (PUT).
func (w *writer) Upsert(kind, ext string, doc map[string]any, ifMatch string) (*IngestResult, *problem.Problem) {
	return w.write("upsert", kind, ext, doc, ifMatch)
}

func (w *writer) write(op, kind, ext string, doc map[string]any, ifMatch string) (*IngestResult, *problem.Problem) {
	ext, ok := ValidExternalID(ext)
	res := &IngestResult{Op: op, Kind: kind, ExternalID: ext}
	if !ok {
		return res, problem.New("validation_failed", "Invalid external id.").Field("", "validation_failed", "external id must be 1-200 characters without control characters or '/'")
	}
	existing, err := loadRow(w.ctx, w.tx, kind, ext)
	if err != nil {
		return res, problem.As(err)
	}
	if ifMatch != "" && (existing == nil || (ifMatch != "*" && ifMatch != existing.ETag)) {
		return res, problem.New("precondition_failed", "If-Match does not match the current version.")
	}
	if doc == nil {
		return res, problem.New("invalid_request", "A JSON object is required.")
	}
	p, prob := w.prepare(kind, ext, doc, existing)
	if prob != nil {
		if existing != nil {
			res.ID = &existing.ID
		}
		return res, prob
	}
	status := w.settings.Defaults.IngestStatus
	if existing != nil {
		status = existing.Status
	}
	if p.status != nil {
		status = *p.status
	}
	res.Warnings = p.warnings
	if existing != nil && existing.Data == p.data && existing.Status == status &&
		(kind == KindBrand || kind == KindProduct && existing.BrandID == p.brandID || existing.ProductID == p.productID) {
		res.Result = "unchanged"
		res.ID = &existing.ID
		vis := w.a.visibilityOf(w.ctx, w.tx, existing)
		res.Visibility = &vis
		res.ETag = &existing.ETag
		return res, nil
	}
	etag := etagFor(status, p.data)
	var r *row
	statusChanged, parentChanged := false, false
	if existing == nil {
		id := ids.New(idPrefix[kind])
		r = &row{Kind: kind, ID: id, ExternalID: ext, Status: status, Data: p.data, ETag: etag, CreatedAt: w.now,
			UpdatedAt: w.now, RecordUpdatedAt: w.now, BrandID: p.brandID, ProductID: p.productID}
		if err := w.insertRow(r, p); err != nil {
			if db.IsUniqueViolation(err) && kind == KindVariant {
				return res, problem.New("gtin_conflict", "The GTIN already belongs to another variant.")
			}
			return res, problem.As(err)
		}
		res.Result = "created"
	} else {
		r = existing
		statusChanged = existing.Status != status
		parentChanged = (kind == KindProduct && existing.BrandID != p.brandID) || ((kind == KindVariant || kind == KindAsset) && existing.ProductID != p.productID)
		r.Status, r.Data, r.ETag, r.RecordUpdatedAt = status, p.data, etag, w.now
		if p.brandID != "" {
			r.BrandID = p.brandID
		}
		if p.productID != "" {
			r.ProductID = p.productID
		}
		if err := w.updateRow(r, p); err != nil {
			if db.IsUniqueViolation(err) && kind == KindVariant {
				return res, problem.New("gtin_conflict", "The GTIN already belongs to another variant.")
			}
			return res, problem.As(err)
		}
		res.Result = "updated"
	}
	if err := w.afterWrite(r, existing == nil || statusChanged || parentChanged, existing); err != nil {
		return res, problem.As(err)
	}
	res.ID = &r.ID
	vis := w.a.visibilityOf(w.ctx, w.tx, r)
	res.Visibility = &vis
	res.ETag = &r.ETag
	return res, nil
}

func (w *writer) insertRow(r *row, p *prepared) error {
	switch r.Kind {
	case KindBrand:
		_, err := w.tx.Exec(w.ctx, `INSERT INTO brands (id, external_id, status, name, data, etag, created_at, updated_at, record_updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.ExternalID, r.Status, p.cols["name"], r.Data, r.ETag, w.now, w.now, w.now)
		return err
	case KindProduct:
		var d ProductData
		_ = json.Unmarshal([]byte(r.Data), &d)
		_, err := w.tx.Exec(w.ctx, `INSERT INTO products (id, external_id, brand_id, status, data, etag, created_at, updated_at, record_updated_at, touched_at, style_number) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.ExternalID, r.BrandID, r.Status, r.Data, r.ETag, w.now, w.now, w.now, w.now, d.StyleNumber)
		return err
	case KindVariant:
		_, err := w.tx.Exec(w.ctx, `INSERT INTO variants (id, external_id, product_id, brand_id, status, data, etag, created_at, updated_at, record_updated_at, gtin, color_code, sku) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.ExternalID, r.ProductID, r.BrandID, r.Status, r.Data, r.ETag, w.now, w.now, w.now, p.cols["gtin"], p.cols["color_code"], p.cols["sku"])
		return err
	case KindAsset:
		_, err := w.tx.Exec(w.ctx, `INSERT INTO assets (id, external_id, product_id, brand_id, status, data, etag, created_at, updated_at, record_updated_at, color_code, position, original_sha) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.ExternalID, r.ProductID, r.BrandID, r.Status, r.Data, r.ETag, w.now, w.now, w.now, p.cols["color_code"], p.cols["position"], p.cols["original_sha"])
		return err
	}
	return nil
}

func (w *writer) updateRow(r *row, p *prepared) error {
	switch r.Kind {
	case KindBrand:
		_, err := w.tx.Exec(w.ctx, `UPDATE brands SET status = ?, name = ?, data = ?, etag = ?, record_updated_at = ? WHERE id = ?`,
			r.Status, p.cols["name"], r.Data, r.ETag, w.now, r.ID)
		return err
	case KindProduct:
		_, err := w.tx.Exec(w.ctx, `UPDATE products SET status = ?, brand_id = ?, data = ?, etag = ?, record_updated_at = ? WHERE id = ?`,
			r.Status, r.BrandID, r.Data, r.ETag, w.now, r.ID)
		if err == nil {
			_, err = w.tx.Exec(w.ctx, `UPDATE variants SET brand_id = ? WHERE product_id = ?`, r.BrandID, r.ID)
		}
		if err == nil {
			_, err = w.tx.Exec(w.ctx, `UPDATE assets SET brand_id = ? WHERE product_id = ?`, r.BrandID, r.ID)
		}
		return err
	case KindVariant:
		_, err := w.tx.Exec(w.ctx, `UPDATE variants SET status = ?, product_id = ?, brand_id = ?, data = ?, etag = ?, record_updated_at = ?, gtin = ?, color_code = ?, sku = ? WHERE id = ?`,
			r.Status, r.ProductID, r.BrandID, r.Data, r.ETag, w.now, p.cols["gtin"], p.cols["color_code"], p.cols["sku"], r.ID)
		return err
	case KindAsset:
		_, err := w.tx.Exec(w.ctx, `UPDATE assets SET status = ?, product_id = ?, brand_id = ?, data = ?, etag = ?, record_updated_at = ?, color_code = ?, position = ?, original_sha = ? WHERE id = ?`,
			r.Status, r.ProductID, r.BrandID, r.Data, r.ETag, w.now, p.cols["color_code"], p.cols["position"], p.cols["original_sha"], r.ID)
		return err
	}
	return nil
}

// afterWrite recomputes the written object and, when its visibility may
// have changed, its children.
func (w *writer) afterWrite(r *row, cascade bool, existing *row) error {
	oldProduct := ""
	if existing != nil && existing.Kind != KindBrand {
		oldProduct = existing.ProductID
	}
	switch r.Kind {
	case KindBrand:
		if err := w.recompute(r); err != nil {
			return err
		}
		if cascade {
			return w.cascadeBrand(r.ID)
		}
	case KindProduct:
		if err := w.recompute(r); err != nil {
			return err
		}
		if err := w.refreshProductIndex(r.ID); err != nil {
			return err
		}
		if cascade {
			return w.cascadeProduct(r.ID)
		}
	case KindVariant:
		if err := w.recompute(r); err != nil {
			return err
		}
		if err := w.refreshProductIndex(r.ProductID); err != nil {
			return err
		}
		if oldProduct != "" && oldProduct != r.ProductID {
			return w.refreshProductIndex(oldProduct)
		}
	case KindAsset:
		return w.recompute(r)
	}
	return nil
}

// Patch applies a JSON merge patch (RFC 7396) to the stored write model.
func (w *writer) Patch(op, kind, ext string, patch map[string]any, ifMatch string) (*IngestResult, *problem.Problem) {
	ext, ok := ValidExternalID(ext)
	res := &IngestResult{Op: op, Kind: kind, ExternalID: ext}
	if !ok {
		return res, problem.New("validation_failed", "Invalid external id.")
	}
	existing, err := loadRow(w.ctx, w.tx, kind, ext)
	if err != nil {
		return res, problem.As(err)
	}
	if existing == nil {
		return res, problem.New("not_found", "%s %q does not exist.", kind, ext)
	}
	if patch == nil {
		return res, problem.New("invalid_request", "A JSON object is required.")
	}
	doc := map[string]any{}
	dec := json.NewDecoder(strings.NewReader(existing.Data))
	dec.UseNumber()
	_ = dec.Decode(&doc)
	doc["status"] = existing.Status
	merged := mergePatch(doc, patch).(map[string]any)
	stripNulls(merged)
	return w.write(op, kind, ext, merged, ifMatch)
}

// stripNulls removes top-level nulls for required-string fields so that a
// merge patch result validates like a PUT with omitted fields.
func stripNulls(m map[string]any) {
	for k, v := range m {
		if v == nil {
			delete(m, k)
		}
	}
}

func mergePatch(target any, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	tm, ok := target.(map[string]any)
	if !ok {
		tm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(tm, k)
			continue
		}
		tm[k] = mergePatch(tm[k], v)
	}
	return tm
}

// SetStatus publishes or unpublishes (a patch of status).
func (w *writer) SetStatus(op, kind, ext string) (*IngestResult, *problem.Problem) {
	status := "published"
	if op == "unpublish" {
		status = "unpublished"
	}
	return w.Patch(op, kind, ext, map[string]any{"status": status}, "")
}

// Delete removes an object (and a product's variants and assets).
func (w *writer) Delete(kind, ext string) (*IngestResult, *problem.Problem) {
	ext, _ = ValidExternalID(ext)
	res := &IngestResult{Op: "delete", Kind: kind, ExternalID: ext}
	existing, err := loadRow(w.ctx, w.tx, kind, ext)
	if err != nil {
		return res, problem.As(err)
	}
	if existing == nil {
		res.Result = "unchanged"
		return res, nil
	}
	brandExt := ""
	if kind == KindBrand {
		brandExt = existing.ExternalID
	} else {
		brandExt = w.brandExternalIDOf(existing.BrandID)
	}
	if !w.brandAllowed(brandExt) {
		return res, forbiddenBrand()
	}
	res.ID = &existing.ID
	switch kind {
	case KindBrand:
		var n int
		_ = w.tx.QueryRow(w.ctx, `SELECT COUNT(*) FROM products WHERE brand_id = ?`, existing.ID).Scan(&n)
		if n > 0 {
			return res, problem.New("conflict", "Brand %q still has %d products; delete or move them first.", ext, n)
		}
		if err := w.deleteRow(existing); err != nil {
			return res, problem.As(err)
		}
	case KindProduct:
		if err := w.deleteRow(existing); err != nil {
			return res, problem.As(err)
		}
		vs, err := loadRows(w.ctx, w.tx, KindVariant, `WHERE product_id = ? ORDER BY external_id`, existing.ID)
		if err != nil {
			return res, problem.As(err)
		}
		for _, v := range vs {
			if err := w.deleteRow(v); err != nil {
				return res, problem.As(err)
			}
		}
		as, err := loadRows(w.ctx, w.tx, KindAsset, `WHERE product_id = ?`+assetOrder, existing.ID)
		if err != nil {
			return res, problem.As(err)
		}
		for _, x := range as {
			if err := w.deleteRow(x); err != nil {
				return res, problem.As(err)
			}
		}
		_, _ = w.tx.Exec(w.ctx, `DELETE FROM products WHERE id = ?`, existing.ID)
	case KindVariant:
		if err := w.deleteRow(existing); err != nil {
			return res, problem.As(err)
		}
		if err := w.refreshProductIndex(existing.ProductID); err != nil {
			return res, problem.As(err)
		}
	case KindAsset:
		if err := w.deleteRow(existing); err != nil {
			return res, problem.As(err)
		}
	}
	res.Result = "deleted"
	return res, nil
}

// deleteRow writes a tombstone when r was visible and removes it (products
// are removed by the caller after their children).
func (w *writer) deleteRow(r *row) error {
	if r.Visible {
		if _, err := w.emit(r.Kind, "delete", r); err != nil {
			return err
		}
	}
	if r.Kind == KindProduct {
		return nil
	}
	_, err := w.tx.Exec(w.ctx, `DELETE FROM `+tableOf(r.Kind)+` WHERE id = ?`, r.ID)
	return err
}

// AssetOrder assigns positions 1..n to all assets of a product colour.
func (w *writer) AssetOrder(productExt string, colorCode *string, list []string) ([]IngestResult, *problem.Problem) {
	prod, err := loadRow(w.ctx, w.tx, KindProduct, productExt)
	if err != nil {
		return nil, problem.As(err)
	}
	if prod == nil {
		return nil, problem.New("not_found", "Product %q does not exist.", productExt)
	}
	if !w.brandAllowed(w.brandExternalIDOf(prod.BrandID)) {
		return nil, forbiddenBrand()
	}
	where := `WHERE product_id = ? AND color_code IS NULL`
	args := []any{prod.ID}
	if colorCode != nil {
		where = `WHERE product_id = ? AND color_code = ?`
		args = append(args, *colorCode)
	}
	cur, err := loadRows(w.ctx, w.tx, KindAsset, where+assetOrder, args...)
	if err != nil {
		return nil, problem.As(err)
	}
	byExt := map[string]*row{}
	for _, r := range cur {
		byExt[r.ExternalID] = r
	}
	seen := map[string]bool{}
	for _, e := range list {
		if byExt[e] == nil || seen[e] {
			return nil, problem.New("validation_failed", "Asset %q is not an asset of this product and colour (or listed twice).", e).Field("/asset_external_ids", "validation_failed", "unknown or duplicate asset")
		}
		seen[e] = true
	}
	if len(list) != len(cur) {
		return nil, problem.New("validation_failed", "List all %d assets of this product and colour.", len(cur)).Field("/asset_external_ids", "validation_failed", "all assets must be listed")
	}
	// Move everything out of the way first (negative positions), then set.
	for i, e := range list {
		if _, err := w.tx.Exec(w.ctx, `UPDATE assets SET position = ? WHERE id = ?`, -(i + 1), byExt[e].ID); err != nil {
			return nil, problem.As(err)
		}
	}
	var out []IngestResult
	for i, e := range list {
		r := byExt[e]
		var d AssetData
		_ = json.Unmarshal([]byte(r.Data), &d)
		res := IngestResult{Op: "patch", Kind: KindAsset, ExternalID: e, ID: &r.ID}
		if d.Position == i+1 {
			if _, err := w.tx.Exec(w.ctx, `UPDATE assets SET position = ? WHERE id = ?`, i+1, r.ID); err != nil {
				return nil, problem.As(err)
			}
			res.Result = "unchanged"
		} else {
			d.Position = i + 1
			r.Data = string(mustJSON(d))
			r.ETag = etagFor(r.Status, r.Data)
			if _, err := w.tx.Exec(w.ctx, `UPDATE assets SET position = ?, data = ?, etag = ?, record_updated_at = ? WHERE id = ?`, i+1, r.Data, r.ETag, w.now, r.ID); err != nil {
				return nil, problem.As(err)
			}
			res.Result = "updated"
		}
		out = append(out, res)
	}
	// Recompute in the new order so upserts appear in that order.
	for i, e := range list {
		r := byExt[e]
		if err := w.recompute(r); err != nil {
			return nil, problem.As(err)
		}
		vis := w.a.visibilityOf(w.ctx, w.tx, r)
		out[i].Visibility = &vis
		out[i].ETag = &r.ETag
	}
	return out, nil
}

// PutFile attaches uploaded bytes (already stored as blob sha) as one file
// of an asset (ingest PUT /assets/{id}/files/{rendition}).
func (w *writer) PutFile(ext, rendition, sha string) (*IngestResult, *problem.Problem) {
	ext, _ = ValidExternalID(ext)
	res := &IngestResult{Op: "upsert", Kind: KindAsset, ExternalID: ext}
	existing, err := loadRow(w.ctx, w.tx, KindAsset, ext)
	if err != nil {
		return res, problem.As(err)
	}
	if existing == nil {
		return res, problem.New("not_found", "Asset %q does not exist; create its metadata first.", ext)
	}
	var d AssetData
	_ = json.Unmarshal([]byte(existing.Data), &d)
	if cur, ok := d.Files[rendition]; ok && cur.SHA256 == sha {
		res.Result = "unchanged"
		res.ID = &existing.ID
		vis := w.a.visibilityOf(w.ctx, w.tx, existing)
		res.Visibility, res.ETag = &vis, &existing.ETag
		return res, nil
	}
	files := map[string]any{}
	if rendition == "original" {
		// A new original drops client renditions made for the previous one.
		files["original"] = map[string]any{"sha256": sha}
	} else {
		if _, ok := d.Files["original"]; !ok {
			return res, problem.New("validation_failed", "Upload the original first.")
		}
		for n, f := range d.Files {
			files[n] = map[string]any{"sha256": f.SHA256, "width": f.Width, "height": f.Height}
		}
		files[rendition] = map[string]any{"sha256": sha}
	}
	doc := map[string]any{}
	dec := json.NewDecoder(strings.NewReader(existing.Data))
	dec.UseNumber()
	_ = dec.Decode(&doc)
	doc["status"] = existing.Status
	doc["files"] = files
	stripNulls(doc)
	for _, f := range files {
		stripNulls(f.(map[string]any))
	}
	r, prob := w.write("upsert", KindAsset, ext, doc, "")
	if prob == nil {
		r.Op = "upsert"
	}
	return r, prob
}
