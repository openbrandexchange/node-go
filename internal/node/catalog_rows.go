package node

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/openbrandexchange/node-go/internal/db"
)

// Kinds of catalog objects.
const (
	KindBrand   = "brand"
	KindProduct = "product"
	KindVariant = "variant"
	KindAsset   = "asset"
)

func tableOf(kind string) string {
	switch kind {
	case KindBrand:
		return "brands"
	case KindProduct:
		return "products"
	case KindVariant:
		return "variants"
	case KindAsset:
		return "assets"
	}
	panic("unknown kind " + kind)
}

// row is a stored catalog object.
type row struct {
	Kind            string
	ID              string
	ExternalID      string
	Status          string
	Data            string
	ETag            string
	Visible         bool
	Rep             string
	RepHash         string
	LastSeq         int64
	CreatedAt       string
	UpdatedAt       string
	RecordUpdatedAt string
	BrandID         string
	ProductID       string
	// assets
	ColorCode   *string
	Position    int
	OriginalSHA *string
	LiveFiles   *string
	Pending     bool
}

func rowColumns(kind string) string {
	base := "id, external_id, status, data, etag, visible, rep, rep_hash, last_seq, created_at, updated_at, record_updated_at"
	switch kind {
	case KindProduct:
		return base + ", brand_id, ''"
	case KindVariant:
		return base + ", brand_id, product_id"
	case KindAsset:
		return base + ", brand_id, product_id, color_code, position, original_sha, live_files, pending"
	}
	return base + ", '', ''"
}

func scanRow(kind string, sc interface{ Scan(...any) error }) (*row, error) {
	r := &row{Kind: kind}
	var visible, pending int
	dest := []any{&r.ID, &r.ExternalID, &r.Status, &r.Data, &r.ETag, &visible, &r.Rep, &r.RepHash, &r.LastSeq,
		&r.CreatedAt, &r.UpdatedAt, &r.RecordUpdatedAt, &r.BrandID, &r.ProductID}
	var color, orig, live sql.NullString
	var pos sql.NullInt64
	if kind == KindAsset {
		dest = append(dest, &color, &pos, &orig, &live, &pending)
	}
	if err := sc.Scan(dest...); err != nil {
		return nil, err
	}
	r.Visible = visible == 1
	r.Pending = pending == 1
	if color.Valid {
		r.ColorCode = &color.String
	}
	if orig.Valid {
		r.OriginalSHA = &orig.String
	}
	if live.Valid {
		r.LiveFiles = &live.String
	}
	r.Position = int(pos.Int64)
	return r, nil
}

// loadRow loads by external id (nil, nil when absent).
func loadRow(ctx context.Context, q db.Querier, kind, externalID string) (*row, error) {
	r, err := scanRow(kind, q.QueryRow(ctx, `SELECT `+rowColumns(kind)+` FROM `+tableOf(kind)+` WHERE external_id = ?`, externalID))
	if db.NotFound(err) {
		return nil, nil
	}
	return r, err
}

// loadRowByID loads by protocol id (nil, nil when absent).
func loadRowByID(ctx context.Context, q db.Querier, kind, id string) (*row, error) {
	r, err := scanRow(kind, q.QueryRow(ctx, `SELECT `+rowColumns(kind)+` FROM `+tableOf(kind)+` WHERE id = ?`, id))
	if db.NotFound(err) {
		return nil, nil
	}
	return r, err
}

// loadRows runs a query selecting rowColumns(kind).
func loadRows(ctx context.Context, q db.Querier, kind, where string, args ...any) ([]*row, error) {
	rs, err := q.Query(ctx, `SELECT `+rowColumns(kind)+` FROM `+tableOf(kind)+` `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []*row
	for rs.Next() {
		r, err := scanRow(kind, rs)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// assetOrder is the canonical asset order: colour (null first), position.
const assetOrder = ` ORDER BY CASE WHEN color_code IS NULL THEN 0 ELSE 1 END, color_code, position, external_id`

func hashHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func etagFor(status, data string) string {
	return `"` + hashHex([]byte(status + "\n" + data))[:32] + `"`
}

// visibilityOf returns the IngestRecord visibility of a row.
func (a *App) visibilityOf(ctx context.Context, q db.Querier, r *row) string {
	if r.Visible && !(r.Kind == KindAsset && r.Pending) {
		return "visible"
	}
	if r.Kind == KindAsset && r.Status == "published" {
		var pv int
		_ = q.QueryRow(ctx, `SELECT visible FROM products WHERE id = ?`, r.ProductID).Scan(&pv)
		if pv == 1 && (r.Pending || r.LiveFiles == nil) {
			return "pending_files"
		}
	}
	return "hidden"
}

// missingFiles lists the files an asset still needs.
func (a *App) missingFiles(ctx context.Context, q db.Querier, r *row) []string {
	var d AssetData
	_ = json.Unmarshal([]byte(r.Data), &d)
	orig, ok := d.Files["original"]
	if !ok {
		return []string{"original", "web", "square", "thumb"}
	}
	missing := []string{}
	for _, n := range DerivedRenditions {
		if ref, ok := d.Files[n]; ok {
			if !blobExists(ctx, q, ref.SHA256) {
				missing = append(missing, n)
			}
			continue
		}
		var s string
		if err := q.QueryRow(ctx, `SELECT sha256 FROM renditions WHERE original_sha = ? AND name = ?`, orig.SHA256, n).Scan(&s); err != nil {
			missing = append(missing, n)
		}
	}
	return missing
}

func blobExists(ctx context.Context, q db.Querier, sha string) bool {
	var n int
	return q.QueryRow(ctx, `SELECT 1 FROM blobs WHERE sha256 = ?`, sha).Scan(&n) == nil
}

// record renders an IngestRecord for a row.
func (a *App) record(ctx context.Context, q db.Querier, r *row) *IngestRecord {
	data := map[string]any{}
	_ = json.Unmarshal([]byte(r.Data), &data)
	data["status"] = r.Status
	if r.Kind == KindAsset {
		var d AssetData
		_ = json.Unmarshal([]byte(r.Data), &d)
		data["rights"] = a.effectiveRights(ctx, d.Rights)
		data["provenance"] = effectiveProvenance(d.Provenance)
		data["files"] = a.currentFiles(ctx, q, d.Files)
	}
	rec := &IngestRecord{Kind: r.Kind, ID: r.ID, ExternalID: r.ExternalID, Status: r.Status,
		Visibility: a.visibilityOf(ctx, q, r), ETag: r.ETag, CreatedAt: r.CreatedAt, UpdatedAt: r.RecordUpdatedAt, Data: data}
	if r.Kind == KindAsset {
		missing := a.missingFiles(ctx, q, r)
		if missing == nil {
			missing = []string{}
		}
		rec.MissingFiles = &missing
	}
	return rec
}

// currentFiles lists every current blob of an asset (IngestRecord.data.files):
// the files the brand referenced, completed with their dimensions, plus the
// renditions the node made itself.
func (a *App) currentFiles(ctx context.Context, q db.Querier, own map[string]FileRef) map[string]FileRef {
	files := map[string]FileRef{}
	for n, f := range own {
		if f.Width == nil || f.Height == nil {
			var wd, ht sql.NullInt64
			if q.QueryRow(ctx, `SELECT width, height FROM blobs WHERE sha256 = ?`, f.SHA256).Scan(&wd, &ht) == nil && wd.Valid && ht.Valid {
				w, h := int(wd.Int64), int(ht.Int64)
				f.Width, f.Height = &w, &h
			}
		}
		files[n] = f
	}
	orig, ok := files["original"]
	if !ok {
		return files
	}
	for _, n := range DerivedRenditions {
		if _, ok := files[n]; ok {
			continue
		}
		var sha string
		var wd, ht int
		if q.QueryRow(ctx, `SELECT sha256, width, height FROM renditions WHERE original_sha = ? AND name = ?`, orig.SHA256, n).Scan(&sha, &wd, &ht) == nil {
			files[n] = FileRef{SHA256: sha, Width: &wd, Height: &ht}
		}
	}
	return files
}

// effectiveRights merges the node default rights with the asset's own fields.
func (a *App) effectiveRights(ctx context.Context, own map[string]json.RawMessage) Rights {
	def := a.Settings(ctx).Defaults.Rights
	return mergeRights(def, own)
}

func mergeRights(def Rights, own map[string]json.RawMessage) Rights {
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(mustJSON(def), &m)
	for k, v := range own {
		if strings.HasPrefix(k, "x-") {
			continue
		}
		m[k] = v
	}
	var r Rights
	_ = json.Unmarshal(mustJSON(m), &r)
	if r.Channels == nil {
		r.Channels = []string{}
	}
	if r.License == "" {
		r.License = "retailer-use"
	}
	return r
}

func effectiveProvenance(own map[string]json.RawMessage) Provenance {
	p := Provenance{Source: "", C2PA: "none"}
	m := map[string]json.RawMessage{}
	for k, v := range own {
		if !strings.HasPrefix(k, "x-") {
			m[k] = v
		}
	}
	_ = json.Unmarshal(mustJSON(m), &p)
	if p.Source == "" {
		if p.AIGenerated {
			p.Source = "generated"
		} else {
			p.Source = "photo"
		}
	}
	if p.C2PA == "" {
		p.C2PA = "none"
	}
	return p
}
