package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/imaging"
)

// writer performs catalog writes inside one serialised transaction and
// writes the resulting change-feed entries (docs/ingest.md §8).
type writer struct {
	a        *App
	ctx      context.Context
	tx       *db.Tx
	now      string
	settings *Settings
	renders  map[string]bool // originals that need renditions
	entries  int
	// allowedBrands restricts writes to these brand external ids (nil = all).
	allowedBrands []string
	// exclude positions of these asset ids from position conflict checks
	// (an import applying a product colour's positions atomically).
	positionExclude map[string]bool
	// silent stores a changed representation of a visible object without a
	// change entry (the one-time upgrade of objects whose content did not
	// change, only the shape of their representation).
	silent bool
}

func (a *App) newWriter(ctx context.Context, tx *db.Tx) *writer {
	w := &writer{a: a, ctx: ctx, tx: tx, now: a.nowStr(), settings: a.Settings(ctx), renders: map[string]bool{}}
	tx.AfterCommit(func() {
		if w.entries > 0 {
			a.changed()
		}
		for sha := range w.renders {
			a.queueRender(sha)
		}
	})
	return w
}

// emit writes one change entry and returns its seq.
func (w *writer) emit(typ, op string, r *row) (int64, error) {
	var seq int64
	var brandID, productID any
	if r.Kind == KindBrand {
		brandID = r.ID
	} else {
		brandID = r.BrandID
	}
	if r.Kind == KindVariant || r.Kind == KindAsset {
		productID = r.ProductID
	}
	err := w.tx.QueryRow(w.ctx, `INSERT INTO changes (type, op, object_id, external_id, brand_id, product_id, at) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING seq`,
		typ, op, r.ID, r.ExternalID, brandID, productID, w.now).Scan(&seq)
	if err != nil {
		return 0, err
	}
	w.entries++
	if r.Kind == KindProduct {
		_, _ = w.tx.Exec(w.ctx, `UPDATE products SET touched_at = ? WHERE id = ?`, w.now, r.ID)
	} else if r.ProductID != "" {
		_, _ = w.tx.Exec(w.ctx, `UPDATE products SET touched_at = ? WHERE id = ?`, w.now, r.ProductID)
	}
	return seq, nil
}

// emitGrant writes a grant entry for one retailer.
func (w *writer) emitGrant(retailerID string, resync bool) error {
	_, err := w.tx.Exec(w.ctx, `INSERT INTO changes (type, op, object_id, external_id, retailer_id, resync, at) VALUES ('grant', 'upsert', ?, NULL, ?, ?, ?)`,
		grantIDFor(retailerID), retailerID, db.Bool(resync), w.now)
	if err == nil {
		w.entries++
	}
	return err
}

func grantIDFor(retailerID string) string {
	return "grt_" + strings.TrimPrefix(retailerID, "rtl_")
}

func (w *writer) parentVisible(kind, id string) bool {
	var v int
	_ = w.tx.QueryRow(w.ctx, `SELECT visible FROM `+tableOf(kind)+` WHERE id = ?`, id).Scan(&v)
	return v == 1
}

// recompute derives visibility and representation of r, writes change
// entries for visible effects and stores the result.
func (w *writer) recompute(r *row) error {
	visible := r.Status == "published"
	var rep any
	var liveFiles *string
	pending := false
	var expiresAt *string
	switch r.Kind {
	case KindBrand:
		var d BrandData
		_ = json.Unmarshal([]byte(r.Data), &d)
		rep = Brand{ID: r.ID, ExternalID: r.ExternalID, Name: d.Name, LogoURL: d.LogoURL, Website: d.Website, Description: d.Description}
	case KindProduct:
		var d ProductData
		_ = json.Unmarshal([]byte(r.Data), &d)
		visible = visible && w.parentVisible(KindBrand, r.BrandID)
		rep = productRep(r, &d)
	case KindVariant:
		var d VariantData
		_ = json.Unmarshal([]byte(r.Data), &d)
		visible = visible && w.parentVisible(KindProduct, r.ProductID)
		rep = variantRep(r, &d)
	case KindAsset:
		var d AssetData
		_ = json.Unmarshal([]byte(r.Data), &d)
		live, complete, err := w.resolveFiles(&d)
		if err != nil {
			return err
		}
		if complete {
			s := string(mustJSON(live))
			liveFiles = &s
		} else {
			liveFiles = r.LiveFiles
			pending = true
			if orig, ok := d.Files["original"]; ok {
				w.renders[orig.SHA256] = true
			}
		}
		var files map[string]LiveFile
		if liveFiles != nil {
			_ = json.Unmarshal([]byte(*liveFiles), &files)
		}
		rights := mergeRights(w.settings.Defaults.Rights, d.Rights)
		expiresAt = rights.ExpiresAt
		visible = visible && w.parentVisible(KindProduct, r.ProductID) && files != nil && !expired(rights.ExpiresAt, w.a.now())
		asset := Asset{ID: r.ID, ExternalID: r.ExternalID, ProductID: r.ProductID, BrandID: r.BrandID, MediaType: "image",
			ColorCode: d.ColorCode, Role: d.Role, View: d.View, Position: d.Position, AltText: d.AltText,
			Rights: rights, Provenance: effectiveProvenance(d.Provenance)}
		if len(d.VariantExternalIDs) > 0 {
			asset.VariantIDs = []string{}
			for _, ve := range d.VariantExternalIDs {
				var vid string
				if w.tx.QueryRow(w.ctx, `SELECT id FROM variants WHERE external_id = ? AND product_id = ?`, ve, r.ProductID).Scan(&vid) == nil {
					asset.VariantIDs = append(asset.VariantIDs, vid)
				}
			}
			sort.Strings(asset.VariantIDs)
		}
		if orig, ok := files["original"]; ok {
			asset.ContentType, asset.Width, asset.Height, asset.Bytes, asset.SHA256 = orig.ContentType, orig.Width, orig.Height, orig.Bytes, orig.SHA256
			asset.Filename = deref(d.Filename)
			if asset.Filename == "" {
				asset.Filename = r.ExternalID + "." + imaging.Ext(orig.ContentType)
			}
			asset.Renditions = map[string]*Rendition{}
			for _, n := range RenditionNames {
				f := files[n]
				asset.Renditions[n] = &Rendition{Name: n, ContentType: f.ContentType, Width: f.Width, Height: f.Height, Bytes: f.Bytes, SHA256: f.SHA256}
			}
		}
		rep = asset
	}
	repJSON := string(mustJSON(rep))
	repHash := hashHex([]byte(repJSON))
	updatedAt, lastSeq := r.UpdatedAt, r.LastSeq
	switch {
	case w.silent && visible && r.Visible:
	case visible && (!r.Visible || repHash != r.RepHash):
		seq, err := w.emit(r.Kind, "upsert", r)
		if err != nil {
			return err
		}
		updatedAt, lastSeq = w.now, seq
	case !visible && r.Visible:
		seq, err := w.emit(r.Kind, "delete", r)
		if err != nil {
			return err
		}
		lastSeq = seq
	case repHash != r.RepHash:
		updatedAt = w.now
	}
	var err error
	if r.Kind == KindAsset {
		_, err = w.tx.Exec(w.ctx, `UPDATE assets SET visible = ?, rep = ?, rep_hash = ?, updated_at = ?, last_seq = ?, live_files = ?, pending = ?, expires_at = ? WHERE id = ?`,
			db.Bool(visible), repJSON, repHash, updatedAt, lastSeq, liveFiles, db.Bool(pending), expiresAt, r.ID)
		r.LiveFiles, r.Pending = liveFiles, pending
	} else {
		_, err = w.tx.Exec(w.ctx, `UPDATE `+tableOf(r.Kind)+` SET visible = ?, rep = ?, rep_hash = ?, updated_at = ?, last_seq = ? WHERE id = ?`,
			db.Bool(visible), repJSON, repHash, updatedAt, lastSeq, r.ID)
	}
	r.Visible, r.Rep, r.RepHash, r.UpdatedAt, r.LastSeq = visible, repJSON, repHash, updatedAt, lastSeq
	return err
}

// productRep is the protocol Product of a stored product (without
// updated_at, which is set when it is read).
func productRep(r *row, d *ProductData) Product {
	return Product{ID: r.ID, ExternalID: r.ExternalID, BrandID: r.BrandID, StyleNumber: d.StyleNumber, Name: d.Name,
		Description: d.Description, ItemType: d.ItemType, ItemSubtype: d.ItemSubtype, GPCBrick: d.GPCBrick, TargetGroup: d.TargetGroup,
		Season: d.Season, Collection: d.Collection, Materials: d.Materials, ContainsAnimalOrigin: d.ContainsAnimalOrigin,
		FootwearMaterials: d.FootwearMaterials, Care: d.Care, CountryOfOrigin: d.CountryOfOrigin, Certifications: d.Certifications,
		DPPURL: d.DPPURL, Features: d.Features, Attributes: d.Attributes, CreatedAt: r.CreatedAt}
}

// variantRep is the protocol Variant of a stored variant.
func variantRep(r *row, d *VariantData) Variant {
	v := Variant{ID: r.ID, ExternalID: r.ExternalID, ProductID: r.ProductID, BrandID: r.BrandID, GTINOriginal: d.GTIN,
		SKU: d.SKU, Color: d.Color, Size: d.Size, SizeLength: d.SizeLength, SizeSystem: d.SizeSystem, Measurements: d.Measurements}
	if d.GTIN != nil {
		if n, ok := normalizeGTIN(*d.GTIN); ok {
			v.GTIN = &n
		}
	}
	return v
}

func expired(expiresAt *string, now time.Time) bool {
	if expiresAt == nil || *expiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, *expiresAt)
	if err != nil {
		return false
	}
	return !t.After(now)
}

// resolveFiles resolves the asset's wanted files to blobs; complete is true
// when the original and all renditions exist.
func (w *writer) resolveFiles(d *AssetData) (map[string]LiveFile, bool, error) {
	orig, ok := d.Files["original"]
	if !ok {
		return nil, false, nil
	}
	out := map[string]LiveFile{}
	of, ok, err := w.blobFile(orig)
	if err != nil || !ok {
		return nil, false, err
	}
	out["original"] = of
	for _, n := range DerivedRenditions {
		if ref, ok := d.Files[n]; ok {
			f, ok, err := w.blobFile(ref)
			if err != nil || !ok {
				return nil, false, err
			}
			out[n] = f
			continue
		}
		var f LiveFile
		err := w.tx.QueryRow(w.ctx, `SELECT sha256, width, height, bytes FROM renditions WHERE original_sha = ? AND name = ?`, orig.SHA256, n).
			Scan(&f.SHA256, &f.Width, &f.Height, &f.Bytes)
		if db.NotFound(err) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		f.ContentType = "image/jpeg"
		out[n] = f
	}
	return out, true, nil
}

func (w *writer) blobFile(ref FileRef) (LiveFile, bool, error) {
	var f LiveFile
	var wd, ht sql.NullInt64
	err := w.tx.QueryRow(w.ctx, `SELECT sha256, content_type, bytes, width, height FROM blobs WHERE sha256 = ?`, ref.SHA256).
		Scan(&f.SHA256, &f.ContentType, &f.Bytes, &wd, &ht)
	if db.NotFound(err) {
		return f, false, nil
	}
	if err != nil {
		return f, false, err
	}
	f.Width, f.Height = int(wd.Int64), int(ht.Int64)
	if f.Width == 0 && ref.Width != nil {
		f.Width = *ref.Width
	}
	if f.Height == 0 && ref.Height != nil {
		f.Height = *ref.Height
	}
	if f.Width == 0 || f.Height == 0 {
		f.Width, f.Height = max(f.Width, 1), max(f.Height, 1)
	}
	return f, true, nil
}

// cascadeProduct recomputes a product's variants and assets in feed order.
func (w *writer) cascadeProduct(productID string) error {
	vs, err := loadRows(w.ctx, w.tx, KindVariant, `WHERE product_id = ? ORDER BY external_id`, productID)
	if err != nil {
		return err
	}
	for _, v := range vs {
		if err := w.recompute(v); err != nil {
			return err
		}
	}
	as, err := loadRows(w.ctx, w.tx, KindAsset, `WHERE product_id = ?`+assetOrder, productID)
	if err != nil {
		return err
	}
	for _, x := range as {
		if err := w.recompute(x); err != nil {
			return err
		}
	}
	return nil
}

// cascadeBrand recomputes a brand's products, then variants, then assets.
func (w *writer) cascadeBrand(brandID string) error {
	ps, err := loadRows(w.ctx, w.tx, KindProduct, `WHERE brand_id = ? ORDER BY external_id`, brandID)
	if err != nil {
		return err
	}
	for _, p := range ps {
		if err := w.recompute(p); err != nil {
			return err
		}
	}
	vs, err := loadRows(w.ctx, w.tx, KindVariant, `WHERE brand_id = ? ORDER BY product_id, external_id`, brandID)
	if err != nil {
		return err
	}
	for _, v := range vs {
		if err := w.recompute(v); err != nil {
			return err
		}
	}
	as, err := loadRows(w.ctx, w.tx, KindAsset, `WHERE brand_id = ? ORDER BY product_id, CASE WHEN color_code IS NULL THEN 0 ELSE 1 END, color_code, position, external_id`, brandID)
	if err != nil {
		return err
	}
	for _, x := range as {
		if err := w.recompute(x); err != nil {
			return err
		}
	}
	return nil
}

// refreshProductIndex rebuilds the search text of a product.
func (w *writer) refreshProductIndex(productID string) error {
	var data string
	if err := w.tx.QueryRow(w.ctx, `SELECT data FROM products WHERE id = ?`, productID).Scan(&data); err != nil {
		if db.NotFound(err) {
			return nil
		}
		return err
	}
	var d ProductData
	_ = json.Unmarshal([]byte(data), &d)
	parts := []string{d.StyleNumber}
	for _, v := range d.Name {
		parts = append(parts, v)
	}
	rs, err := w.tx.Query(w.ctx, `SELECT data, gtin FROM variants WHERE product_id = ?`, productID)
	if err != nil {
		return err
	}
	for rs.Next() {
		var vd string
		var g sql.NullString
		if err := rs.Scan(&vd, &g); err != nil {
			rs.Close()
			return err
		}
		var v VariantData
		_ = json.Unmarshal([]byte(vd), &v)
		parts = append(parts, v.Color.Code, deref(v.SKU), deref(v.GTIN), g.String, deref(v.Color.SupplierColor))
		for _, n := range v.Color.Name {
			parts = append(parts, n)
		}
	}
	rs.Close()
	text := strings.ToLower(strings.Join(parts, " "))
	nameSort := strings.ToLower(d.Name.Pick(w.settings.Portal.DefaultLanguage))
	_, err = w.tx.Exec(w.ctx, `UPDATE products SET search_text = ?, name_sort = ?, style_number = ?, item_type = ?, target_group = ?, season = ?, collection = ? WHERE id = ?`,
		text, nameSort, d.StyleNumber, d.ItemType, d.TargetGroup, d.Season, d.Collection, productID)
	return err
}
