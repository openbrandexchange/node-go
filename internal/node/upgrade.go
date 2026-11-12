package node

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/openbrandexchange/node-go/internal/db"
)

// upgradeArticleData rewrites the stored products and variants once after
// migration 3 (spec 0.2): the write model gets its canonical form (0.1
// fibre keys recycled_<fibre> / organic_<fibre> become the material with its
// qualifier, empty features and measurements are null) and the representation
// gets the new properties (null when not stated). An object whose content
// changed (normalised fibres) gets an upsert in /changes; one whose
// representation only gained the new null properties is stored without a
// change entry, so an upgrade does not make every consumer fetch the whole
// catalog again.
func (a *App) upgradeArticleData(ctx context.Context) error {
	pending, err := getMeta(ctx, a.DB, "upgrade_article_data")
	if err != nil || pending == "" {
		return err
	}
	return a.DB.WriteTx(ctx, func(tx *db.Tx) error {
		w := a.newWriter(ctx, tx)
		for _, kind := range []string{KindProduct, KindVariant} {
			rows, err := loadRows(ctx, tx, kind, `ORDER BY external_id`)
			if err != nil {
				return err
			}
			for _, r := range rows {
				var data, oldRep, newRep string
				switch kind {
				case KindProduct:
					var d ProductData
					_ = json.Unmarshal([]byte(r.Data), &d)
					normalizeTerms(d.Materials, d.Care)
					if len(d.Features) == 0 {
						d.Features = nil
					}
					if len(d.Certifications) == 0 {
						d.Certifications = nil
					}
					data = string(mustJSON(d))
					var old Product
					_ = json.Unmarshal([]byte(r.Rep), &old)
					oldRep, newRep = string(mustJSON(old)), string(mustJSON(productRep(r, &d)))
				case KindVariant:
					var d VariantData
					_ = json.Unmarshal([]byte(r.Data), &d)
					if d.Measurements.empty() {
						d.Measurements = nil
					}
					data = string(mustJSON(d))
					var old Variant
					_ = json.Unmarshal([]byte(r.Rep), &old)
					oldRep, newRep = string(mustJSON(old)), string(mustJSON(variantRep(r, &d)))
				}
				if data != r.Data {
					r.Data, r.ETag = data, etagFor(r.Status, data)
					if _, err := tx.Exec(ctx, `UPDATE `+tableOf(kind)+` SET data = ?, etag = ? WHERE id = ?`, r.Data, r.ETag, r.ID); err != nil {
						return err
					}
				}
				w.silent = oldRep == newRep
				err := w.recompute(r)
				w.silent = false
				if err != nil {
					return err
				}
			}
		}
		_, err := tx.Exec(ctx, `DELETE FROM meta WHERE key = 'upgrade_article_data'`)
		return err
	})
}

// upgradeTerms rewrites the stored products once after migration 4 (spec
// 0.3, proposal 0004): materials and care in the write model change from the
// 0.1/0.2 shapes (part and fiber strings, the flags recycled and organic,
// care {code, text}) to terms with names from the vocabularies. A fibre
// with both flags keeps one qualifier, recycled. Every product whose
// representation changed gets an upsert in /changes, so consumers fetch the
// new shape. It runs before upgradeArticleData, which decodes the 0.3 shape.
func (a *App) upgradeTerms(ctx context.Context) error {
	pending, err := getMeta(ctx, a.DB, "upgrade_terms")
	if err != nil || pending == "" {
		return err
	}
	return a.DB.WriteTx(ctx, func(tx *db.Tx) error {
		w := a.newWriter(ctx, tx)
		rows, err := loadRows(ctx, tx, KindProduct, `ORDER BY external_id`)
		if err != nil {
			return err
		}
		for _, r := range rows {
			raw := map[string]any{}
			if err := json.Unmarshal([]byte(r.Data), &raw); err != nil {
				return fmt.Errorf("product %s: %w", r.ExternalID, err)
			}
			raw["materials"] = upgradeMaterialsJSON(raw["materials"])
			var d ProductData
			if err := json.Unmarshal(mustJSON(raw), &d); err != nil {
				return fmt.Errorf("product %s: %w", r.ExternalID, err)
			}
			// A 0.2 care entry {code, text} decodes as it is; normalizeTerms
			// adds category, symbol and name.
			normalizeTerms(d.Materials, d.Care)
			data := string(mustJSON(d))
			if data != r.Data {
				r.Data, r.ETag = data, etagFor(r.Status, data)
				if _, err := tx.Exec(ctx, `UPDATE products SET data = ?, etag = ? WHERE id = ?`, r.Data, r.ETag, r.ID); err != nil {
					return err
				}
			}
			var old any
			_ = json.Unmarshal([]byte(r.Rep), &old)
			oldRep, newRep := string(mustJSON(old)), string(mustJSON(productRep(r, &d)))
			w.silent = oldRep == newRep
			err := w.recompute(r)
			w.silent = false
			if err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `DELETE FROM meta WHERE key = 'upgrade_terms'`)
		return err
	})
}

// upgradeMaterialsJSON turns 0.1/0.2 materials into the 0.3 write shape
// (codes only; normalizeTerms adds the names). 0.3 entries pass unchanged.
func upgradeMaterialsJSON(v any) any {
	parts, ok := v.([]any)
	if !ok {
		return v
	}
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := pm["part"].(string); ok {
			pm["part"] = map[string]any{"code": s}
		}
		fibers, _ := pm["fibers"].([]any)
		for _, f := range fibers {
			fm, ok := f.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := fm["fiber"].(string); ok {
				fm["material"] = map[string]any{"code": s}
			}
			if _, ok := fm["qualifier"]; !ok {
				switch {
				case fm["recycled"] == true:
					fm["qualifier"] = map[string]any{"code": "recycled"}
				case fm["organic"] == true:
					fm["qualifier"] = map[string]any{"code": "organic"}
				}
			}
			delete(fm, "fiber")
			delete(fm, "recycled")
			delete(fm, "organic")
		}
	}
	return parts
}
