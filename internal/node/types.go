package node

import (
	"encoding/json"
	"sort"
	"strings"
)

// LocalizedText is text per BCP 47 tag. nil marshals as null.
type LocalizedText map[string]string

// Pick returns the text for lang with the shared fallback rule: the
// requested language, then English, then the first available language (in
// the node's content-language order, then alphabetical).
func (t LocalizedText) Pick(lang string, order ...string) string {
	if len(t) == 0 {
		return ""
	}
	if lang != "" {
		if v := t[lang]; v != "" {
			return v
		}
		if i := strings.IndexByte(lang, '-'); i > 0 {
			if v := t[lang[:i]]; v != "" {
				return v
			}
		}
	}
	if v := t["en"]; v != "" {
		return v
	}
	for _, o := range order {
		if v := t[o]; v != "" {
			return v
		}
	}
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if t[k] != "" {
			return t[k]
		}
	}
	return ""
}

// Fiber is one fibre of a material part.
type Fiber struct {
	Fiber      string  `json:"fiber"`
	Percentage float64 `json:"percentage"`
}

// MaterialPart is the composition of one part.
type MaterialPart struct {
	Part   string  `json:"part"`
	Fibers []Fiber `json:"fibers"`
}

// CareItem is one care instruction.
type CareItem struct {
	Code string        `json:"code"`
	Text LocalizedText `json:"text"`
}

// Color of a variant.
type Color struct {
	Code          string        `json:"code"`
	Name          LocalizedText `json:"name"`
	SupplierColor *string       `json:"supplier_color"`
}

// ---- stored write models (IngestRecord.data) --------------------------------

// BrandData is the stored BrandInput (without status).
type BrandData struct {
	Name        string        `json:"name"`
	LogoURL     *string       `json:"logo_url"`
	Website     *string       `json:"website"`
	Description LocalizedText `json:"description"`
}

// ProductData is the stored ProductInput (without status).
type ProductData struct {
	BrandExternalID string         `json:"brand_external_id"`
	StyleNumber     string         `json:"style_number"`
	Name            LocalizedText  `json:"name"`
	Description     LocalizedText  `json:"description"`
	ItemType        *string        `json:"item_type"`
	GPCBrick        *string        `json:"gpc_brick"`
	TargetGroup     *string        `json:"target_group"`
	Season          *string        `json:"season"`
	Collection      *string        `json:"collection"`
	Materials       []MaterialPart `json:"materials"`
	Care            []CareItem     `json:"care"`
	CountryOfOrigin *string        `json:"country_of_origin"`
	DPPURL          *string        `json:"dpp_url"`
	Attributes      map[string]any `json:"attributes"`
}

// VariantData is the stored VariantInput (without status).
type VariantData struct {
	ProductExternalID string  `json:"product_external_id"`
	GTIN              *string `json:"gtin"`
	SKU               *string `json:"sku"`
	Color             Color   `json:"color"`
	Size              *string `json:"size"`
	SizeSystem        *string `json:"size_system"`
}

// FileRef references a blob in AssetData.files.
type FileRef struct {
	SHA256 string `json:"sha256"`
	Width  *int   `json:"width"`
	Height *int   `json:"height"`
}

// AssetData is the stored AssetInput (without status). Rights and
// provenance keep only the fields the brand sent (omitted = node default).
type AssetData struct {
	ProductExternalID  string                     `json:"product_external_id"`
	ColorCode          *string                    `json:"color_code"`
	VariantExternalIDs []string                   `json:"variant_external_ids"`
	MediaType          string                     `json:"media_type"`
	Role               string                     `json:"role"`
	View               *string                    `json:"view"`
	Position           int                        `json:"position"`
	Filename           *string                    `json:"filename"`
	AltText            LocalizedText              `json:"alt_text"`
	Rights             map[string]json.RawMessage `json:"rights"`
	Provenance         map[string]json.RawMessage `json:"provenance"`
	Files              map[string]FileRef         `json:"files"`
}

// ---- protocol representations -------------------------------------------------

// Brand is the protocol Brand.
type Brand struct {
	ID          string        `json:"id"`
	ExternalID  string        `json:"external_id"`
	Name        string        `json:"name"`
	LogoURL     *string       `json:"logo_url"`
	Website     *string       `json:"website"`
	Description LocalizedText `json:"description"`
	UpdatedAt   string        `json:"updated_at"`
}

// Product is the protocol Product (Variants/Assets set = ProductDetail).
type Product struct {
	ID              string         `json:"id"`
	ExternalID      string         `json:"external_id"`
	BrandID         string         `json:"brand_id"`
	StyleNumber     string         `json:"style_number"`
	Name            LocalizedText  `json:"name"`
	Description     LocalizedText  `json:"description"`
	ItemType        *string        `json:"item_type"`
	GPCBrick        *string        `json:"gpc_brick"`
	TargetGroup     *string        `json:"target_group"`
	Season          *string        `json:"season"`
	Collection      *string        `json:"collection"`
	Materials       []MaterialPart `json:"materials"`
	Care            []CareItem     `json:"care"`
	CountryOfOrigin *string        `json:"country_of_origin"`
	DPPURL          *string        `json:"dpp_url"`
	Attributes      map[string]any `json:"attributes"`
	CreatedAt       string         `json:"created_at"`
	UpdatedAt       string         `json:"updated_at"`
	Variants        *[]Variant     `json:"variants,omitempty"`
	Assets          *[]Asset       `json:"assets,omitempty"`
}

// Variant is the protocol Variant.
type Variant struct {
	ID           string  `json:"id"`
	ExternalID   string  `json:"external_id"`
	ProductID    string  `json:"product_id"`
	BrandID      string  `json:"brand_id"`
	GTIN         *string `json:"gtin"`
	GTINOriginal *string `json:"gtin_original"`
	SKU          *string `json:"sku"`
	Color        Color   `json:"color"`
	Size         *string `json:"size"`
	SizeSystem   *string `json:"size_system"`
	UpdatedAt    string  `json:"updated_at"`
}

// Rights is the effective rights object.
type Rights struct {
	License      string   `json:"license"`
	LicenseText  *string  `json:"license_text"`
	Channels     []string `json:"channels"`
	Territories  []string `json:"territories"`
	EmbargoUntil *string  `json:"embargo_until"`
	ExpiresAt    *string  `json:"expires_at"`
	Credit       *string  `json:"credit"`
	ModelRelease *bool    `json:"model_release"`
}

// Provenance is the effective provenance object.
type Provenance struct {
	AIGenerated     bool    `json:"ai_generated"`
	AIEdited        bool    `json:"ai_edited"`
	Source          string  `json:"source"`
	C2PA            string  `json:"c2pa"`
	C2PAManifestURL *string `json:"c2pa_manifest_url"`
	Generator       *string `json:"generator"`
}

// Rendition is one downloadable file of an asset.
type Rendition struct {
	Name         string  `json:"name"`
	URL          string  `json:"url"`
	URLExpiresAt *string `json:"url_expires_at"`
	ContentType  string  `json:"content_type"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	Bytes        int64   `json:"bytes"`
	SHA256       string  `json:"sha256"`
}

// Asset is the protocol Asset.
type Asset struct {
	ID          string                `json:"id"`
	ExternalID  string                `json:"external_id"`
	ProductID   string                `json:"product_id"`
	BrandID     string                `json:"brand_id"`
	MediaType   string                `json:"media_type"`
	ColorCode   *string               `json:"color_code"`
	VariantIDs  []string              `json:"variant_ids"`
	Role        string                `json:"role"`
	View        *string               `json:"view"`
	Position    int                   `json:"position"`
	Filename    string                `json:"filename"`
	AltText     LocalizedText         `json:"alt_text"`
	ContentType string                `json:"content_type"`
	Width       int                   `json:"width"`
	Height      int                   `json:"height"`
	Bytes       int64                 `json:"bytes"`
	SHA256      string                `json:"sha256"`
	Rights      Rights                `json:"rights"`
	Provenance  Provenance            `json:"provenance"`
	Renditions  map[string]*Rendition `json:"renditions"`
	UpdatedAt   string                `json:"updated_at"`
}

// LiveFile is one resolved file of an asset (stored in assets.live_files).
type LiveFile struct {
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Bytes       int64  `json:"bytes"`
}

// RenditionNames in canonical order.
var RenditionNames = []string{"original", "web", "square", "thumb"}

// DerivedRenditions are the renditions derived from the original.
var DerivedRenditions = []string{"web", "square", "thumb"}

// Grant is the protocol Grant.
type Grant struct {
	ID              string     `json:"id"`
	Retailer        GrantParty `json:"retailer"`
	BrandIDs        []string   `json:"brand_ids"`
	Scopes          []string   `json:"scopes"`
	OriginalAllowed bool       `json:"original_allowed"`
	Status          string     `json:"status"`
	UpdatedAt       string     `json:"updated_at"`
}

// GrantParty names the retailer of a grant.
type GrantParty struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// IngestResult is the result of one ingest write.
type IngestResult struct {
	Index      *int     `json:"index,omitempty"`
	Op         string   `json:"op"`
	Kind       string   `json:"kind"`
	ExternalID string   `json:"external_id"`
	Result     string   `json:"result"`
	ID         *string  `json:"id"`
	Visibility *string  `json:"visibility"`
	ETag       *string  `json:"etag"`
	Error      any      `json:"error"`
	Warnings   []string `json:"warnings,omitempty"`
}

// IngestRecord is the GET representation in the ingest API and admin catalog.
type IngestRecord struct {
	Kind         string         `json:"kind"`
	ID           string         `json:"id"`
	ExternalID   string         `json:"external_id"`
	Status       string         `json:"status"`
	Visibility   string         `json:"visibility"`
	ETag         string         `json:"etag"`
	CreatedAt    string         `json:"created_at"`
	UpdatedAt    string         `json:"updated_at"`
	Data         map[string]any `json:"data"`
	MissingFiles []string       `json:"missing_files,omitempty"`
}

func strPtr(s string) *string { return &s }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nonEmptyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
