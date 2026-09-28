package node

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/imaging"
	"github.com/openbrandexchange/node-go/internal/importfmt"
	"github.com/openbrandexchange/node-go/internal/problem"
)

func (a *App) settingsJSON(ctx context.Context) map[string]any {
	s := a.Settings(ctx)
	var m map[string]any
	_ = json.Unmarshal(mustJSON(s), &m)
	delete(m, "logo_sha")
	delete(m, "logo_type")
	if b, ok := m["branding"].(map[string]any); ok {
		b["logo_url"] = a.logoURL(s)
	}
	help := a.Cfg.CustomDomainHelp
	if help == "" {
		help = "Point a DNS CNAME record for your domain (for example `images.yourbrand.com`) at this server, make sure HTTPS works for it, and set `OBX_PUBLIC_URL` to the new address. See the hosting guide of your setup for details."
	}
	m["node"] = map[string]any{
		"public_url": a.Cfg.PublicURL, "discovery_url": a.Cfg.PublicURL + "/.well-known/obx", "software": Software, "version": Version,
		"email_configured": a.Mail.Configured(), "renditions_server_generated": true, "max_upload_bytes": MaxUploadBytes, "custom_domain_help": help,
	}
	return m
}

func (a *App) adminSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.settingsJSON(r.Context()))
}

// applySettingsPatch validates a merge patch against the current settings.
func (a *App) applySettingsPatch(cur *Settings, patch map[string]any) (*Settings, *problem.Problem) {
	for _, k := range []string{"node"} {
		delete(patch, k)
	}
	if b, ok := patch["branding"].(map[string]any); ok {
		delete(b, "logo_url")
	}
	base := map[string]any{}
	_ = json.Unmarshal(mustJSON(cur), &base)
	merged := mergePatch(base, patch).(map[string]any)
	var next Settings
	if err := decodeInto(merged, &next); err != nil {
		return nil, problem.New("validation_failed", "Settings are not valid: %s", err.Error())
	}
	next.LogoSHA, next.LogoType = cur.LogoSHA, cur.LogoType
	p := problem.New("validation_failed", "Please check the highlighted fields.")
	if strings.TrimSpace(next.Company.Name) == "" {
		p.Field("/company/name", "validation_failed", "required")
	}
	if next.Branding.DisplayName == "" {
		next.Branding.DisplayName = next.Company.Name
	}
	if next.Branding.AccentColor != nil && !isHexColor(*next.Branding.AccentColor) {
		p.Field("/branding/accent_color", "validation_failed", "#rrggbb")
	}
	if next.Company.ContactEmail != nil && !validEmail(normEmail(*next.Company.ContactEmail)) {
		p.Field("/company/contact_email", "validation_failed", "invalid e-mail")
	}
	if next.Company.Country != nil {
		c := strings.ToUpper(*next.Company.Country)
		if len(c) != 2 {
			p.Field("/company/country", "validation_failed", "two-letter code")
		}
		next.Company.Country = &c
	}
	if next.Company.Website != nil && !strings.HasPrefix(*next.Company.Website, "http") {
		p.Field("/company/website", "validation_failed", "absolute URL")
	}
	if len(next.Portal.Languages) == 0 {
		p.Field("/portal/languages", "validation_failed", "at least one language")
	}
	for i, l := range next.Portal.Languages {
		n := importfmt.NormalizeLang(l)
		if !langOK(n) {
			p.Field("/portal/languages", "validation_failed", "language like nl or de-CH")
		}
		next.Portal.Languages[i] = n
	}
	next.Portal.DefaultLanguage = importfmt.NormalizeLang(next.Portal.DefaultLanguage)
	if next.Portal.DefaultLanguage == "" && len(next.Portal.Languages) > 0 {
		next.Portal.DefaultLanguage = next.Portal.Languages[0]
	}
	found := false
	for _, l := range next.Portal.Languages {
		if l == next.Portal.DefaultLanguage {
			found = true
		}
	}
	if !found {
		next.Portal.Languages = append([]string{next.Portal.DefaultLanguage}, next.Portal.Languages...)
	}
	if next.Defaults.IngestStatus != "published" && next.Defaults.IngestStatus != "unpublished" {
		p.Field("/defaults/ingest_status", "validation_failed", "published or unpublished")
	}
	if errs := lenientValidate("Rights", next.Defaults.Rights); errs != nil {
		for _, e := range errs {
			p.Field("/defaults/rights"+e.Pointer, "validation_failed", e.Message)
		}
	}
	if next.Defaults.Rights.License == "custom" && strings.TrimSpace(deref(next.Defaults.Rights.LicenseText)) == "" {
		p.Field("/defaults/rights/license_text", "validation_failed", "required for a custom licence")
	}
	if len(p.Errors) > 0 {
		return nil, p
	}
	return &next, nil
}

func (a *App) adminSettingsPatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	patch, p := readJSONObject(r)
	if p != nil {
		problem.Write(w, p)
		return
	}
	cur := a.Settings(ctx).clone()
	next, p := a.applySettingsPatch(cur, patch)
	if p != nil {
		problem.Write(w, p)
		return
	}
	if err := a.saveSettingsWithEffects(ctx, cur, next); err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, a.settingsJSON(ctx))
}

// saveSettingsWithEffects stores settings and writes the feed effects of
// changed default rights (asset upserts) and original downloads (grant
// resyncs).
func (a *App) saveSettingsWithEffects(ctx context.Context, cur, next *Settings) error {
	rightsChanged := string(mustJSON(cur.Defaults.Rights)) != string(mustJSON(next.Defaults.Rights))
	origChanged := cur.Portal.AllowOriginalDownload != next.Portal.AllowOriginalDownload
	var retailers []string
	err := a.DB.WriteTx(ctx, func(tx *db.Tx) error {
		if err := a.saveSettings(ctx, tx, next); err != nil {
			return err
		}
		w := a.newWriter(ctx, tx)
		w.settings = next
		if rightsChanged {
			rows, err := loadRows(ctx, tx, KindAsset, `ORDER BY product_id, CASE WHEN color_code IS NULL THEN 0 ELSE 1 END, color_code, position`)
			if err != nil {
				return err
			}
			for _, r := range rows {
				if err := w.recompute(r); err != nil {
					return err
				}
			}
		}
		if origChanged {
			rs, err := tx.Query(ctx, `SELECT id FROM retailers WHERE status = 'active'`)
			if err != nil {
				return err
			}
			for rs.Next() {
				var id string
				if rs.Scan(&id) == nil {
					retailers = append(retailers, id)
				}
			}
			rs.Close()
			for _, id := range retailers {
				if _, err := tx.Exec(ctx, `UPDATE retailers SET grant_updated_at = ? WHERE id = ?`, w.now, id); err != nil {
					return err
				}
				if err := w.emitGrant(id, true); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err == nil && origChanged {
		go func() {
			for _, id := range retailers {
				a.retailerGrantEvent(context.Background(), id)
			}
		}()
	}
	return err
}

func (a *App) adminLogoPut(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.ContentLength > 5<<20 {
		failP(w, "payload_too_large", "The logo can be at most 5 MB.")
		return
	}
	b, err := a.PutBlob(ctx, r.Body, "", 5<<20)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if b.ContentType == "image/tiff" {
		failP(w, "unsupported_media_type", "Use a PNG, JPEG or WebP logo.")
		return
	}
	cur := a.Settings(ctx).clone()
	next := cur.clone()
	next.LogoSHA, next.LogoType = b.SHA256, b.ContentType
	if err := a.saveSettingsWithEffects(ctx, cur, next); err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, a.settingsJSON(ctx))
}

func (a *App) adminLogoDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cur := a.Settings(ctx).clone()
	next := cur.clone()
	next.LogoSHA, next.LogoType = "", ""
	if err := a.saveSettingsWithEffects(ctx, cur, next); err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, a.settingsJSON(ctx))
}

var _ = imaging.Sniff
