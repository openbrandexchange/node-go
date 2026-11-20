package node

// Portal appearance (node-admin.yaml, spec 0.3.x): branding.theme,
// branding.login, branding.home, the favicon and the branding images.
// theme, login and home are stored as sent: members and block types this
// node does not know are kept and returned unchanged; the known ones are
// validated here.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/ids"
	"github.com/openbrandexchange/node-go/internal/importfmt"
	"github.com/openbrandexchange/node-go/internal/problem"
)

// Appearance limits (node-admin.yaml, docs/node-admin.md of the spec).
const (
	maxBrandingJSON      = 64 << 10
	maxBrandingText      = 2000
	maxBrandingBody      = 10000
	maxHomeBlocks        = 30
	maxBlockID           = 40
	maxBrandingImages    = 200
	maxBrandingImageSize = 10 << 20
	maxBrandingImageSide = 8000
	maxFaviconSize       = 1 << 20
)

var (
	themeFonts = []string{"system", "inter", "dm-sans", "manrope", "work-sans", "jost", "space-grotesk", "fraunces",
		"playfair-display", "libre-baskerville", "cormorant-garamond"}
	themeEnums = map[string][]string{
		"color_scheme": {"light", "dark", "auto"},
		"heading_font": themeFonts,
		"body_font":    themeFonts,
		"radius":       {"none", "small", "medium", "large"},
		"grid_density": {"compact", "comfortable", "editorial"},
		"card_style":   {"plain", "bordered", "elevated"},
		"image_fit":    {"contain", "cover"},
	}
	themeColors = []string{"primary", "secondary", "background", "surface", "text"}
)

// Member kinds of the known home blocks.
const (
	kText  = "text"  // per-language text, ≤ 2,000 per language
	kBody  = "body"  // per-language text, ≤ 10,000 per language
	kImage = "image" // branding image id
	kLink  = "link"  // relative portal path or https URL
	kPlain = "plain" // plain string
	kEmail = "email" // e-mail address
)

type memberRule struct {
	kind string
	enum []string
	max  int // kPlain: maximum length (default 2,000)
}

func enumRule(v ...string) memberRule { return memberRule{enum: v} }

var blockRules = map[string]map[string]memberRule{
	"hero": {"image": {kind: kImage}, "title": {kind: kText}, "text": {kind: kText}, "button_label": {kind: kText}, "link": {kind: kLink},
		"align": enumRule("left", "center"), "height": enumRule("medium", "large"), "overlay": enumRule("none", "dark", "light")},
	"tiles":        {"title": {kind: kText}},
	"text":         {"title": {kind: kText}, "body": {kind: kBody}},
	"image_text":   {"image": {kind: kImage}, "title": {kind: kText}, "text": {kind: kText}, "button_label": {kind: kText}, "link": {kind: kLink}, "image_side": enumRule("left", "right")},
	"products":     {"title": {kind: kText}},
	"announcement": {"text": {kind: kText}, "tone": enumRule("neutral", "info", "success")},
	"contact":      {"title": {kind: kText}, "name": {kind: kPlain, max: 200}, "email": {kind: kEmail}, "phone": {kind: kPlain, max: 50}},
}

var tileItemRules = map[string]memberRule{"image": {kind: kImage}, "title": {kind: kText}, "link": {kind: kLink}}

// appearanceCheck collects field errors of branding.theme/login/home.
type appearanceCheck struct {
	p      *problem.Problem
	images map[string]bool
	fatal  bool // a known member has a shape the typed settings cannot hold
}

func (c *appearanceCheck) err(ptr, detail string) { c.p.Field(ptr, "validation_failed", detail) }

// validateAppearance validates the appearance members of the merged
// branding object. It reports true when the errors must be returned
// before the settings are decoded.
func (a *App) validateAppearance(ctx context.Context, branding any, p *problem.Problem) bool {
	b, ok := branding.(map[string]any)
	if !ok {
		return false
	}
	c := &appearanceCheck{p: p, images: a.brandingImageSet(ctx)}
	for _, k := range []string{"theme", "login", "home"} {
		if v, ok := b[k]; ok && v != nil {
			if _, ok := v.(map[string]any); !ok {
				c.err("/branding/"+k, "object or null")
				c.fatal = true
			}
		}
	}
	if raw, err := json.Marshal(b); err == nil && len(raw) > maxBrandingJSON {
		c.err("/branding", fmt.Sprintf("at most %d bytes of JSON", maxBrandingJSON))
	}
	if t, ok := b["theme"].(map[string]any); ok {
		c.theme(t)
	}
	if l, ok := b["login"].(map[string]any); ok {
		c.member("/branding/login", l, "layout", enumRule("centered", "split"))
		c.member("/branding/login", l, "background_image", memberRule{kind: kImage})
		c.member("/branding/login", l, "title", memberRule{kind: kText})
		c.member("/branding/login", l, "text", memberRule{kind: kText})
	}
	if h, ok := b["home"].(map[string]any); ok {
		c.home(h)
	}
	return c.fatal
}

func (c *appearanceCheck) theme(t map[string]any) {
	for k, e := range themeEnums {
		c.member("/branding/theme", t, k, memberRule{enum: e})
	}
	if v, ok := t["image_background"]; ok && v != nil {
		if s, ok := v.(string); !ok || !isHexColor(s) {
			c.err("/branding/theme/image_background", "#rrggbb")
		}
	}
	switch cs := t["colors"].(type) {
	case nil:
	case map[string]any:
		for _, k := range themeColors {
			if v, ok := cs[k]; ok && v != nil {
				if s, ok := v.(string); !ok || !isHexColor(s) {
					c.err("/branding/theme/colors/"+k, "#rrggbb")
				}
			}
		}
	default:
		c.err("/branding/theme/colors", "object or null")
	}
}

func (c *appearanceCheck) home(h map[string]any) {
	raw, ok := h["blocks"]
	if !ok || raw == nil {
		return
	}
	blocks, ok := raw.([]any)
	if !ok {
		c.err("/branding/home/blocks", "array")
		return
	}
	if len(blocks) > maxHomeBlocks {
		c.err("/branding/home/blocks", fmt.Sprintf("at most %d blocks", maxHomeBlocks))
	}
	seen := map[string]bool{}
	for i, rb := range blocks {
		ptr := "/branding/home/blocks/" + strconv.Itoa(i)
		blk, ok := rb.(map[string]any)
		if !ok {
			c.err(ptr, "object")
			continue
		}
		id, _ := blk["id"].(string)
		switch {
		case strings.TrimSpace(id) == "":
			c.err(ptr+"/id", "required")
		case utf8.RuneCountInString(id) > maxBlockID:
			c.err(ptr+"/id", fmt.Sprintf("at most %d characters", maxBlockID))
		case seen[id]:
			c.err(ptr+"/id", "used by another block")
		}
		seen[id] = true
		typ, _ := blk["type"].(string)
		if typ == "" {
			c.err(ptr+"/type", "required")
			continue
		}
		rules, known := blockRules[typ]
		if !known {
			// A newer block type: stored and returned as is.
			if !blockTypeRe.MatchString(typ) {
				c.err(ptr+"/type", "a block type like hero or image_text")
			}
			continue
		}
		for k, r := range rules {
			c.member(ptr, blk, k, r)
		}
		switch typ {
		case "tiles":
			c.tiles(ptr, blk)
		case "products":
			c.products(ptr, blk)
		}
	}
}

func (c *appearanceCheck) tiles(ptr string, blk map[string]any) {
	items, ok := blk["items"].([]any)
	if !ok || len(items) < 1 || len(items) > 12 {
		c.err(ptr+"/items", "1 to 12 tiles")
	}
	for j, ri := range items {
		iptr := ptr + "/items/" + strconv.Itoa(j)
		it, ok := ri.(map[string]any)
		if !ok {
			c.err(iptr, "object")
			continue
		}
		for k, r := range tileItemRules {
			c.member(iptr, it, k, r)
		}
	}
	if v, ok := blk["columns"]; ok && v != nil {
		if n, ok := jsonInt(v); !ok || n < 2 || n > 4 {
			c.err(ptr+"/columns", "2, 3 or 4")
		}
	}
}

// products checks a products block; when both style_numbers and query
// are set, style_numbers wins (the UI ignores query).
func (c *appearanceCheck) products(ptr string, blk map[string]any) {
	if v, ok := blk["style_numbers"]; ok && v != nil {
		list, ok := v.([]any)
		if !ok || len(list) > 24 {
			c.err(ptr+"/style_numbers", "a list of at most 24 style numbers")
		}
		for j, s := range list {
			if str, ok := s.(string); !ok || str == "" || utf8.RuneCountInString(str) > 100 {
				c.err(ptr+"/style_numbers/"+strconv.Itoa(j), "style number of 1 to 100 characters")
			}
		}
	}
	if v, ok := blk["query"]; ok && v != nil {
		q, ok := v.(string)
		if !ok || len(q) > maxBrandingText || strings.ContainsAny(q, "?#") || hasSpace(q) {
			c.err(ptr+"/query", "the query string of /admin/api/portal/products without ?")
		} else if _, err := url.ParseQuery(q); err != nil {
			c.err(ptr+"/query", "the query string of /admin/api/portal/products without ?")
		}
	}
	if v, ok := blk["max"]; ok && v != nil {
		if n, ok := jsonInt(v); !ok || n < 1 || n > 24 {
			c.err(ptr+"/max", "1 to 24")
		}
	}
}

// member validates obj[key] (absent and null are always fine).
func (c *appearanceCheck) member(ptr string, obj map[string]any, key string, r memberRule) {
	v, ok := obj[key]
	if !ok || v == nil {
		return
	}
	ptr += "/" + key
	if r.enum != nil {
		s, _ := v.(string)
		for _, e := range r.enum {
			if s == e {
				return
			}
		}
		c.err(ptr, "one of "+strings.Join(r.enum, ", "))
		return
	}
	switch r.kind {
	case kText, kBody:
		limit := maxBrandingText
		if r.kind == kBody {
			limit = maxBrandingBody
		}
		m, ok := v.(map[string]any)
		if !ok {
			c.err(ptr, `per-language text like {"en": "..."}`)
			return
		}
		for lang, t := range m {
			if !importfmt.ValidLang(lang) {
				c.err(ptr+"/"+lang, "language like nl or de-CH")
				continue
			}
			if t == nil {
				continue
			}
			s, ok := t.(string)
			if !ok {
				c.err(ptr+"/"+lang, "text")
			} else if utf8.RuneCountInString(s) > limit {
				c.err(ptr+"/"+lang, fmt.Sprintf("at most %d characters", limit))
			}
		}
	case kPlain:
		limit := r.max
		if limit == 0 {
			limit = maxBrandingText
		}
		if s, ok := v.(string); !ok || utf8.RuneCountInString(s) > limit {
			c.err(ptr, fmt.Sprintf("text of at most %d characters", limit))
		}
	case kEmail:
		if s, ok := v.(string); !ok || !validEmail(normEmail(s)) {
			c.err(ptr, "invalid e-mail")
		}
	case kLink:
		if s, ok := v.(string); !ok || !validPortalLink(s) {
			c.err(ptr, "a portal path starting with / or an https:// URL")
		}
	case kImage:
		s, ok := v.(string)
		if !ok || !imageIDRe.MatchString(s) || !c.images[s] {
			c.err(ptr, "unknown image (upload it with POST /admin/api/settings/images)")
		}
	}
}

var (
	blockTypeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	imageIDRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

func hasSpace(s string) bool { return strings.IndexFunc(s, unicode.IsSpace) >= 0 }

// validPortalLink is PortalLink: a portal path starting with a single "/"
// (not "//" or "/\") or an https URL with a host; no whitespace.
func validPortalLink(s string) bool {
	if len(s) > maxBrandingText || hasSpace(s) || strings.ContainsRune(s, 0) {
		return false
	}
	if strings.HasPrefix(s, "/") {
		return len(s) == 1 || (s[1] != '/' && s[1] != '\\')
	}
	rest, ok := strings.CutPrefix(s, "https://")
	return ok && rest != "" && rest[0] != '/'
}

// jsonInt reads an integer from a decoded JSON number.
func jsonInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), n == float64(int(n))
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case int:
		return n, true
	}
	return 0, false
}

// ---- image references ----

type imageRef struct{ ptr, id string }

// appearanceImageRefs lists every image id referenced from branding: the
// login background and the `image` of every block and tile (also of block
// types this node does not know, so their images are never deleted).
func appearanceImageRefs(s *Settings) []imageRef {
	var out []imageRef
	if id, ok := s.Branding.Login["background_image"].(string); ok && id != "" {
		out = append(out, imageRef{"/branding/login/background_image", id})
	}
	blocks, _ := s.Branding.Home["blocks"].([]any)
	for i, rb := range blocks {
		blk, _ := rb.(map[string]any)
		ptr := "/branding/home/blocks/" + strconv.Itoa(i)
		if id, ok := blk["image"].(string); ok && id != "" {
			out = append(out, imageRef{ptr + "/image", id})
		}
		items, _ := blk["items"].([]any)
		for j, ri := range items {
			it, _ := ri.(map[string]any)
			if id, ok := it["image"].(string); ok && id != "" {
				out = append(out, imageRef{ptr + "/items/" + strconv.Itoa(j) + "/image", id})
			}
		}
	}
	return out
}

func brandingImageURL(id string) string { return "/admin/api/public/images/" + id }

// brandingImageSet returns the ids of the stored branding images.
func (a *App) brandingImageSet(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	rs, err := a.DB.Query(ctx, `SELECT id FROM branding_images`)
	if err != nil {
		return out
	}
	defer rs.Close()
	for rs.Next() {
		var id string
		if rs.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// withImageURL returns a copy of obj with `<key>_url` set for an image id.
func withImageURL(obj map[string]any, key string, images map[string]bool) map[string]any {
	out := copyMap(obj)
	var u any
	if id, ok := obj[key].(string); ok && images[id] {
		u = brandingImageURL(id)
	}
	out[key+"_url"] = u
	return out
}

// publicLogin is branding.login with login.background_image_url.
func publicLogin(s *Settings, images map[string]bool) any {
	if s.Branding.Login == nil {
		return nil
	}
	return withImageURL(s.Branding.Login, "background_image", images)
}

// resolvedHome is branding.home with image_url next to every image id.
func resolvedHome(s *Settings, images map[string]bool) any {
	if s.Branding.Home == nil {
		return nil
	}
	out := copyMap(s.Branding.Home)
	blocks, ok := s.Branding.Home["blocks"].([]any)
	if !ok {
		return out
	}
	rb := make([]any, len(blocks))
	for i, b := range blocks {
		blk, ok := b.(map[string]any)
		if !ok {
			rb[i] = b
			continue
		}
		if _, has := blk["image"]; has {
			blk = withImageURL(blk, "image", images)
		} else {
			blk = copyMap(blk)
		}
		if items, ok := blk["items"].([]any); ok {
			ri := make([]any, len(items))
			for j, it := range items {
				if m, ok := it.(map[string]any); ok {
					ri[j] = withImageURL(m, "image", images)
				} else {
					ri[j] = it
				}
			}
			blk["items"] = ri
		}
		rb[i] = blk
	}
	out["blocks"] = rb
	return out
}

// ---- favicon ----

// faviconURL is the public URL of the uploaded favicon.
func (a *App) faviconURL(s *Settings) *string {
	if s.FaviconSHA == "" {
		return nil
	}
	u := a.Cfg.PublicURL + "/admin/api/public/favicon?v=" + s.FaviconSHA[:16]
	return &u
}

// uploadBody returns the uploaded file: the `file` part of a multipart
// body, or the raw body.
func uploadBody(r *http.Request) (io.Reader, *problem.Problem) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "multipart/form-data" {
		return r.Body, nil
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, problem.New("invalid_request", "The multipart body cannot be read.")
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			return nil, problem.New("validation_failed", "Send the image in the multipart field file.").Field("/file", "validation_failed", "required")
		}
		if part.FormName() == "file" {
			return part, nil
		}
	}
}

func (a *App) adminFaviconPut(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.ContentLength > maxFaviconSize+64<<10 {
		failP(w, "payload_too_large", "The favicon can be at most 1 MB.")
		return
	}
	body, p := uploadBody(r)
	if p != nil {
		problem.Write(w, p)
		return
	}
	b, err := a.PutBlob(ctx, body, "", maxFaviconSize)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if b.ContentType != "image/png" && b.ContentType != "image/webp" {
		failP(w, "unsupported_media_type", "Use a PNG or WebP favicon.")
		return
	}
	if b.Width == nil || b.Height == nil || *b.Width != *b.Height {
		problem.Write(w, problem.New("validation_failed", "The favicon must be square.").Field("/file", "validation_failed", "square image"))
		return
	}
	cur := a.Settings(ctx).clone()
	next := cur.clone()
	next.FaviconSHA, next.FaviconType = b.SHA256, b.ContentType
	if err := a.saveSettingsWithEffects(ctx, cur, next); err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, a.settingsJSON(ctx))
}

func (a *App) adminFaviconDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cur := a.Settings(ctx).clone()
	next := cur.clone()
	next.FaviconSHA, next.FaviconType = "", ""
	if err := a.saveSettingsWithEffects(ctx, cur, next); err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 200, a.settingsJSON(ctx))
}

func (a *App) handlePublicFavicon(w http.ResponseWriter, r *http.Request) {
	s := a.Settings(r.Context())
	if s.FaviconSHA == "" {
		failP(w, "not_found", "No favicon.")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if err := a.serveBlob(w, r, s.FaviconSHA, s.FaviconType, ""); err != nil {
		a.fail(w, r, err)
	}
}

// ---- branding images ----

type brandingImage struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Bytes       int64  `json:"bytes"`
	ContentType string `json:"content_type"`
	Used        *bool  `json:"used,omitempty"`
	sha         string
}

func (a *App) adminImageUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.ContentLength > maxBrandingImageSize+64<<10 {
		failP(w, "payload_too_large", "An image can be at most 10 MB.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBrandingImageSize+64<<10)
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "multipart/form-data" {
		failP(w, "unsupported_media_type", "Send multipart/form-data with the image in the field file.")
		return
	}
	var n int
	if err := a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM branding_images`).Scan(&n); err != nil {
		a.fail(w, r, err)
		return
	}
	if n >= maxBrandingImages {
		problem.Write(w, problem.New("limit_exceeded", "This node keeps at most %d branding images; delete unused ones first.", maxBrandingImages).WithStatus(409))
		return
	}
	body, p := uploadBody(r)
	if p != nil {
		problem.Write(w, p)
		return
	}
	b, err := a.PutBlob(ctx, body, "", maxBrandingImageSize)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if b.ContentType != "image/png" && b.ContentType != "image/jpeg" && b.ContentType != "image/webp" {
		failP(w, "unsupported_media_type", "Use a PNG, JPEG or WebP image.")
		return
	}
	if b.Width == nil || b.Height == nil || *b.Width > maxBrandingImageSide || *b.Height > maxBrandingImageSide {
		problem.Write(w, problem.New("validation_failed", "The image can be at most %d pixels per side.", maxBrandingImageSide).
			Field("/file", "validation_failed", fmt.Sprintf("at most %d px per side", maxBrandingImageSide)))
		return
	}
	img := brandingImage{ID: ids.New("img"), Width: *b.Width, Height: *b.Height, Bytes: b.Bytes, ContentType: b.ContentType}
	img.URL = brandingImageURL(img.ID)
	err = a.DB.WriteTx(ctx, func(tx *db.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO branding_images (id, sha256, content_type, width, height, bytes, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			img.ID, b.SHA256, img.ContentType, img.Width, img.Height, img.Bytes, a.nowStr())
		return err
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, 201, img)
}

func (a *App) loadBrandingImages(ctx context.Context, q db.Querier, where string, args ...any) ([]brandingImage, error) {
	rs, err := q.Query(ctx, `SELECT id, sha256, content_type, width, height, bytes FROM branding_images `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []brandingImage{}
	for rs.Next() {
		var im brandingImage
		if err := rs.Scan(&im.ID, &im.sha, &im.ContentType, &im.Width, &im.Height, &im.Bytes); err != nil {
			return nil, err
		}
		im.URL = brandingImageURL(im.ID)
		out = append(out, im)
	}
	return out, rs.Err()
}

func (a *App) adminImageList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, err := a.loadBrandingImages(ctx, a.DB, `ORDER BY id DESC`)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	used := map[string]bool{}
	for _, ref := range appearanceImageRefs(a.Settings(ctx)) {
		used[ref.id] = true
	}
	for i := range items {
		u := used[items[i].ID]
		items[i].Used = &u
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (a *App) adminImageDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	err := a.DB.WriteTx(ctx, func(tx *db.Tx) error {
		raw, err := getMeta(ctx, tx, "settings")
		if err != nil {
			return err
		}
		s := defaultSettings()
		if raw != "" {
			_ = json.Unmarshal([]byte(raw), s)
		}
		for _, ref := range appearanceImageRefs(s) {
			if ref.id == id {
				return problem.New("conflict", "The image is still used at %s; remove it from the appearance settings first.", ref.ptr)
			}
		}
		res, err := tx.Exec(ctx, `DELETE FROM branding_images WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return problem.New("not_found", "No such image.")
		}
		return nil
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePublicImage serves a branding image without authentication (they
// appear on the login page). Ids are never reused, so it is cached
// immutably.
func (a *App) handlePublicImage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, err := a.loadBrandingImages(ctx, a.DB, `WHERE id = ?`, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if len(items) == 0 {
		failP(w, "not_found", "No such image.")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if err := a.serveBlob(w, r, items[0].sha, items[0].ContentType, ""); err != nil {
		w.Header().Set("Cache-Control", "no-store")
		a.fail(w, r, err)
	}
}
