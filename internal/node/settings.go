package node

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/openbrandexchange/node-go/internal/db"
)

// Settings is the brand's node configuration (admin Settings, minus the
// read-only `node` facts which are computed).
type Settings struct {
	Company struct {
		Name         string  `json:"name"`
		Website      *string `json:"website"`
		ContactEmail *string `json:"contact_email"`
		Country      *string `json:"country"`
	} `json:"company"`
	Branding struct {
		DisplayName string        `json:"display_name"`
		LogoURL     *string       `json:"logo_url"`
		AccentColor *string       `json:"accent_color"`
		WelcomeText LocalizedText `json:"welcome_text"`
		// PortalName overrides the local word in "<Brand> · <local word>",
		// per language (optional; same shape as admin-ui expects).
		PortalName LocalizedText `json:"portal_name"`
	} `json:"branding"`
	Portal struct {
		AccessRequestsEnabled  bool     `json:"access_requests_enabled"`
		AllowOriginalDownload  bool     `json:"allow_original_download"`
		AllowArticleDataExport bool     `json:"allow_article_data_export"`
		RetailerAPISelfService bool     `json:"retailer_api_self_service"`
		Languages              []string `json:"languages"`
		DefaultLanguage        string   `json:"default_language"`
	} `json:"portal"`
	Defaults struct {
		Rights       Rights `json:"rights"`
		IngestStatus string `json:"ingest_status"`
	} `json:"defaults"`
	// Retention of personal and usage data, in days (the same settings as
	// the other OBX portals; docs/operations.md#personal-data-gdpr).
	Retention struct {
		DownloadEventsDays int `json:"download_events_days"`
		AuditLogDays       int `json:"audit_log_days"`
		AccessRequestsDays int `json:"access_requests_days"`
	} `json:"retention"`
	// LogoSHA is the blob hash of the uploaded logo (internal).
	LogoSHA string `json:"logo_sha,omitempty"`
	// LogoType is the logo media type (internal).
	LogoType string `json:"logo_type,omitempty"`
}

// DefaultRights are the protocol default rights (docs/protocol.md §5.6).
func DefaultRights() Rights {
	return Rights{License: "retailer-use", Channels: []string{"webshop", "marketplace", "social"}}
}

func defaultSettings() *Settings {
	s := &Settings{}
	s.Portal.AccessRequestsEnabled = true
	s.Portal.AllowOriginalDownload = true
	s.Portal.AllowArticleDataExport = true
	s.Portal.Languages = []string{"en"}
	s.Portal.DefaultLanguage = "en"
	s.Defaults.Rights = DefaultRights()
	s.Defaults.IngestStatus = "published"
	s.Retention.DownloadEventsDays = 730
	s.Retention.AuditLogDays = 365
	s.Retention.AccessRequestsDays = 365
	return s
}

// Retention limits (days) a brand can choose.
const (
	minRetentionDays = 30
	maxRetentionDays = 3650
)

// Languages returns the content languages (first = default).
func (s *Settings) ContentLanguages() []string {
	out := []string{}
	if s.Portal.DefaultLanguage != "" {
		out = append(out, s.Portal.DefaultLanguage)
	}
	for _, l := range s.Portal.Languages {
		if l != s.Portal.DefaultLanguage {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		out = []string{"en"}
	}
	return out
}

func (s *Settings) clone() *Settings {
	var c Settings
	_ = json.Unmarshal(mustJSON(s), &c)
	return &c
}

// getMeta reads a meta value ("" when absent).
func getMeta(ctx context.Context, q db.Querier, key string) (string, error) {
	var v string
	err := q.QueryRow(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if db.NotFound(err) {
		return "", nil
	}
	return v, err
}

// setMeta upserts a meta value.
func setMeta(ctx context.Context, q db.Querier, key, value string) error {
	_, err := q.Exec(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Settings returns the current settings (cached; invalidated on save).
func (a *App) Settings(ctx context.Context) *Settings {
	a.settingsMu.RLock()
	s := a.settings
	a.settingsMu.RUnlock()
	if s != nil {
		return s
	}
	raw, err := getMeta(ctx, a.DB, "settings")
	st := defaultSettings()
	if err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), st)
	}
	if st.Defaults.IngestStatus == "" {
		st.Defaults.IngestStatus = "published"
	}
	if st.Defaults.Rights.License == "" {
		st.Defaults.Rights = DefaultRights()
	}
	if st.Defaults.Rights.Channels == nil {
		st.Defaults.Rights.Channels = []string{}
	}
	a.settingsMu.Lock()
	a.settings = st
	a.settingsMu.Unlock()
	return st
}

// saveSettings stores settings inside tx and refreshes the cache after commit.
func (a *App) saveSettings(ctx context.Context, tx *db.Tx, s *Settings) error {
	if err := setMeta(ctx, tx, "settings", string(mustJSON(s))); err != nil {
		return err
	}
	tx.AfterCommit(func() {
		a.settingsMu.Lock()
		a.settings = nil
		a.settingsMu.Unlock()
	})
	return nil
}

// SetupDone reports whether first-run setup completed.
func (a *App) SetupDone(ctx context.Context) bool {
	if a.setupDone.Load() {
		return true
	}
	v, _ := getMeta(ctx, a.DB, "setup_done")
	if v == "1" {
		a.setupDone.Store(true)
		return true
	}
	return false
}

// PortalName returns "<Brand> · <local word>" for lang. The word is the
// brand's own portal_name (language fallback rule) or the glossary word
// from the vendored admin-ui locale files.
func (a *App) PortalName(ctx context.Context, lang string) string {
	s := a.Settings(ctx)
	name := s.Branding.DisplayName
	if name == "" {
		name = s.Company.Name
	}
	word := strings.TrimSpace(s.Branding.PortalName.Pick(lang, s.ContentLanguages()...))
	if word == "" && a.locales != nil {
		word = a.locales.get(supportedLang(lang))["glossary.portal"]
	}
	if word == "" {
		word = portalWord(lang)
	}
	if name == "" {
		return word
	}
	return name + " · " + word
}

// portalWord is the local word for the retailer portal (glossary).
func portalWord(lang string) string {
	base := lang
	if i := strings.IndexByte(base, '-'); i > 0 {
		base = base[:i]
	}
	if w, ok := portalWords[base]; ok {
		return w
	}
	return portalWords["en"]
}

var portalWords = map[string]string{
	"bg": "Портал за търговци",
	"cs": "Portál pro prodejce",
	"da": "Forhandlerportal",
	"de": "Händlerportal",
	"el": "Πύλη μεταπωλητών",
	"en": "Brand portal",
	"es": "Portal para distribuidores",
	"et": "Edasimüüjate portaal",
	"fi": "Jälleenmyyjäportaali",
	"fr": "Espace revendeurs",
	"ga": "Tairseach miondíoltóirí",
	"hr": "Portal za trgovce",
	"hu": "Viszonteladói portál",
	"it": "Area rivenditori",
	"lt": "Prekybininkų portalas",
	"lv": "Tirgotāju portāls",
	"mt": "Portal għall-bejjiegħa",
	"nb": "Forhandlerportal",
	"nl": "Beeldbank",
	"pl": "Portal dla sprzedawców",
	"pt": "Portal de revendedores",
	"ro": "Portal pentru revânzători",
	"sk": "Portál pre predajcov",
	"sl": "Portal za trgovce",
	"sv": "Återförsäljarportal",
}
