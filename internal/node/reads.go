package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
)

// Viewer is whoever reads the catalog: a retailer's API client, a retailer
// user in the portal, or a brand admin previewing the portal.
type Viewer struct {
	Kind            string // client | user | admin
	RetailerID      string
	RetailerName    string
	BrandIDs        []string // nil = all published brands
	OriginalAllowed bool
	Scopes          map[string]bool
	ClientRowID     string
	ClientName      string
	UserID          string
	UserName        string
	Language        string
}

// Preview reports whether this is a brand admin looking at the portal.
func (v *Viewer) Preview() bool { return v.Kind == "admin" }

// tag identifies the viewer in signed URLs.
func (v *Viewer) tag() string {
	switch v.Kind {
	case "client":
		return "c." + v.ClientRowID
	case "user":
		return "u." + v.UserID
	}
	return "a"
}

// SeesBrand reports whether the brand is inside the viewer's grant.
func (v *Viewer) SeesBrand(brandID string) bool {
	if v.BrandIDs == nil {
		return true
	}
	for _, b := range v.BrandIDs {
		if b == brandID {
			return true
		}
	}
	return false
}

// brandFilterSQL returns " AND <col> IN (...)" for restricted viewers.
func (v *Viewer) brandFilterSQL(col string) (string, []any) {
	if v.BrandIDs == nil {
		return "", nil
	}
	if len(v.BrandIDs) == 0 {
		return " AND 1 = 0", nil
	}
	args := make([]any, len(v.BrandIDs))
	for i, b := range v.BrandIDs {
		args[i] = b
	}
	return " AND " + col + " IN " + db.In(len(args)), args
}

// retailerRow is a stored retailer.
type retailerRow struct {
	ID, Name, Status, GrantUpdatedAt, CreatedAt, UpdatedAt string
	Reference, Website, VAT, Country, Notes, LastActivity  *string
	BrandIDs                                               []string // nil = all
	AllowOriginal                                          *bool
}

func (a *App) loadRetailer(ctx context.Context, q db.Querier, id string) (*retailerRow, error) {
	r := &retailerRow{}
	var brandIDs sql.NullString
	var allow sql.NullInt64
	err := q.QueryRow(ctx, `SELECT id, name, status, grant_updated_at, created_at, updated_at, reference, website, vat_number, country, notes, last_activity_at, brand_ids, allow_original FROM retailers WHERE id = ?`, id).
		Scan(&r.ID, &r.Name, &r.Status, &r.GrantUpdatedAt, &r.CreatedAt, &r.UpdatedAt, &r.Reference, &r.Website, &r.VAT, &r.Country, &r.Notes, &r.LastActivity, &brandIDs, &allow)
	if db.NotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if brandIDs.Valid {
		r.BrandIDs = []string{}
		_ = json.Unmarshal([]byte(brandIDs.String), &r.BrandIDs)
	}
	if allow.Valid {
		b := allow.Int64 == 1
		r.AllowOriginal = &b
	}
	return r, nil
}

// originalAllowed combines the brand setting and the retailer's override.
func (a *App) originalAllowed(ctx context.Context, r *retailerRow) bool {
	if !a.Settings(ctx).Portal.AllowOriginalDownload {
		return false
	}
	return r.AllowOriginal == nil || *r.AllowOriginal
}

// grantOf builds the Grant for a retailer.
func (a *App) grantOf(ctx context.Context, r *retailerRow) *Grant {
	orig := a.originalAllowed(ctx, r)
	scopes := []string{"catalog:read", "media:read"}
	if orig {
		scopes = append(scopes, "media:original")
	}
	scopes = append(scopes, "usage:write")
	status := "active"
	if r.Status != "active" {
		status = "revoked"
	}
	return &Grant{ID: grantIDFor(r.ID), Retailer: GrantParty{ID: r.ID, Name: r.Name}, BrandIDs: r.BrandIDs,
		Scopes: scopes, OriginalAllowed: orig, Status: status, UpdatedAt: r.GrantUpdatedAt}
}

// ---- signed rendition URLs -------------------------------------------------

const signedURLLifetime = time.Hour

func (a *App) signPath(path, tag string, exp int64) string {
	return a.Keys.Sign("url", "GET\n"+path+"\n"+strconv.FormatInt(exp, 10)+"\n"+tag)
}

// signedURL returns an absolute signed URL for path and its expiry.
func (a *App) signedURL(path, tag string) (string, string) {
	exp := a.now().Add(signedURLLifetime).Truncate(time.Second)
	e := exp.Unix()
	q := url.Values{}
	q.Set("exp", strconv.FormatInt(e, 10))
	if tag != "" {
		q.Set("v", tag)
	}
	q.Set("sig", a.signPath(path, tag, e))
	return a.Cfg.PublicURL + path + "?" + q.Encode(), exp.UTC().Format(time.RFC3339)
}

// verifySigned checks exp/v/sig query parameters for path; it returns the
// viewer tag.
func (a *App) verifySigned(path string, q url.Values) (string, bool) {
	sig := q.Get("sig")
	if sig == "" {
		return "", false
	}
	e, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil || time.Unix(e, 0).Before(a.now()) || time.Unix(e, 0).After(a.now().Add(25*time.Hour)) {
		return "", false
	}
	tag := q.Get("v")
	return tag, constantEq(strings.ToLower(sig), a.signPath(path, tag, e))
}

// renderAsset turns a stored asset rep into the protocol Asset for v.
func (a *App) renderAsset(rep, updatedAt string, v *Viewer) *Asset {
	var as Asset
	_ = json.Unmarshal([]byte(rep), &as)
	as.UpdatedAt = updatedAt
	if as.VariantIDs == nil {
		as.VariantIDs = nil
	}
	if !v.OriginalAllowed {
		delete(as.Renditions, "original")
	}
	for name, r := range as.Renditions {
		u, exp := a.signedURL("/obx/v1/assets/"+as.ID+"/renditions/"+name, v.tag())
		r.URL, r.URLExpiresAt = u, &exp
	}
	return &as
}

func renderBrand(rep, updatedAt string) *Brand {
	var b Brand
	_ = json.Unmarshal([]byte(rep), &b)
	b.UpdatedAt = updatedAt
	return &b
}

func renderProduct(rep, updatedAt string) *Product {
	var p Product
	_ = json.Unmarshal([]byte(rep), &p)
	p.UpdatedAt = updatedAt
	return &p
}

func renderVariant(rep, updatedAt string) *Variant {
	var x Variant
	_ = json.Unmarshal([]byte(rep), &x)
	x.UpdatedAt = updatedAt
	return &x
}

// assetVisibleNow re-checks rights expiry at read time.
func (a *App) assetVisibleNow(expiresAt *string) bool { return !expired(expiresAt, a.now()) }

// productDetails loads ProductDetail for visible product ids (in the order
// given), with visible variants and assets.
func (a *App) productDetails(ctx context.Context, v *Viewer, productIDs []string) ([]*Product, error) {
	if len(productIDs) == 0 {
		return []*Product{}, nil
	}
	args := make([]any, len(productIDs))
	for i, id := range productIDs {
		args[i] = id
	}
	bf, bargs := v.brandFilterSQL("brand_id")
	rs, err := a.DB.Query(ctx, `SELECT id, rep, updated_at FROM products WHERE visible = 1 AND id IN `+db.In(len(args))+bf, append(args, bargs...)...)
	if err != nil {
		return nil, err
	}
	byID := map[string]*Product{}
	for rs.Next() {
		var id, rep, up string
		if err := rs.Scan(&id, &rep, &up); err != nil {
			rs.Close()
			return nil, err
		}
		p := renderProduct(rep, up)
		vs, as := []Variant{}, []Asset{}
		p.Variants, p.Assets = &vs, &as
		byID[id] = p
	}
	rs.Close()
	if len(byID) == 0 {
		return []*Product{}, nil
	}
	ids := make([]any, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	vr, err := a.DB.Query(ctx, `SELECT product_id, rep, updated_at FROM variants WHERE visible = 1 AND product_id IN `+db.In(len(ids))+` ORDER BY product_id, external_id`, ids...)
	if err != nil {
		return nil, err
	}
	for vr.Next() {
		var pid, rep, up string
		if err := vr.Scan(&pid, &rep, &up); err != nil {
			vr.Close()
			return nil, err
		}
		*byID[pid].Variants = append(*byID[pid].Variants, *renderVariant(rep, up))
	}
	vr.Close()
	// Variants must be in byte order of external_id; SQL collation may differ.
	for _, p := range byID {
		sort.SliceStable(*p.Variants, func(i, j int) bool { return (*p.Variants)[i].ExternalID < (*p.Variants)[j].ExternalID })
	}
	ar, err := a.DB.Query(ctx, `SELECT product_id, rep, updated_at, expires_at FROM assets WHERE visible = 1 AND product_id IN `+db.In(len(ids))+` ORDER BY product_id, CASE WHEN color_code IS NULL THEN 0 ELSE 1 END, color_code, position`, ids...)
	if err != nil {
		return nil, err
	}
	for ar.Next() {
		var pid, rep, up string
		var exp *string
		if err := ar.Scan(&pid, &rep, &up, &exp); err != nil {
			ar.Close()
			return nil, err
		}
		if !a.assetVisibleNow(exp) {
			continue
		}
		*byID[pid].Assets = append(*byID[pid].Assets, *a.renderAsset(rep, up, v))
	}
	ar.Close()
	out := []*Product{}
	for _, id := range productIDs {
		if p, ok := byID[id]; ok {
			out = append(out, p)
			delete(byID, id)
		}
	}
	return out, nil
}

// visibleAsset loads one asset visible to v (nil when not visible).
func (a *App) visibleAsset(ctx context.Context, v *Viewer, id string) (*row, error) {
	r, err := loadRowByID(ctx, a.DB, KindAsset, id)
	if err != nil || r == nil {
		return nil, err
	}
	if !r.Visible || !v.SeesBrand(r.BrandID) {
		return nil, nil
	}
	var as Asset
	_ = json.Unmarshal([]byte(r.Rep), &as)
	if !a.assetVisibleNow(as.Rights.ExpiresAt) {
		return nil, nil
	}
	return r, nil
}
