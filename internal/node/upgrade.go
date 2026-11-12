package node

import (
	"context"
	"encoding/json"

	"github.com/openbrandexchange/node-go/internal/db"
)

// upgradeArticleData rewrites the stored products and variants once after
// migration 3 (spec 0.2): the write model gets its canonical 0.2 form (0.1
// fibre keys recycled_<fibre> / organic_<fibre> become the fibre with its
// flag, empty features and measurements are null) and the representation
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
					normalizeMaterials(d.Materials)
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
