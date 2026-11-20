package node

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// testPNG is a w×h PNG.
func testPNG(t testing.TB, w, h int, shade uint8) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, 0, color.RGBA{shade, uint8(x), 0, 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func fileForm(t testing.TB, data []byte) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "image.png")
	_, _ = fw.Write(data)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func (c *client) uploadImage(data []byte) *resp {
	body, ct := fileForm(c.n.t, data)
	return c.do("POST", "/admin/api/settings/images", body, "Content-Type", ct)
}

// stripNulls removes null members (a JSON merge patch stores no nulls).
func withoutNulls(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, c := range t {
			if c != nil {
				out[k] = withoutNulls(c)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, c := range t {
			out[i] = withoutNulls(c)
		}
		return out
	}
	return v
}

// replaceImageIDs maps every string member named image/background_image
// through ids (uploading a new image for each id not seen yet).
func replaceImageIDs(v any, ids map[string]string, upload func() string) {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			if s, ok := c.(string); ok && (k == "image" || k == "background_image") {
				if ids[s] == "" {
					ids[s] = upload()
				}
				t[k] = ids[s]
				continue
			}
			replaceImageIDs(c, ids, upload)
		}
	case []any:
		for _, c := range t {
			replaceImageIDs(c, ids, upload)
		}
	}
}

func loadBrandingFixture(t *testing.T) map[string]any {
	raw, err := os.ReadFile(filepath.Join(specDir(t), "examples", "BrandingSettings.full.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if b, ok := m["branding"].(map[string]any); ok {
		m = b
	}
	for _, k := range []string{"logo_url", "favicon_url"} {
		delete(m, k)
	}
	return m
}

func TestPortalAppearance(t *testing.T) {
	n := newTestNode(t)
	admin := n.setup()

	shade := uint8(0)
	fixture := loadBrandingFixture(t)
	ids := map[string]string{}
	replaceImageIDs(fixture, ids, func() string {
		shade++
		r := admin.uploadImage(testPNG(t, 64, 32, shade))
		if r.Status != 201 {
			t.Fatalf("upload: %d %s", r.Status, r.Body)
		}
		m := r.JSON(t)
		if !strings.HasPrefix(m["id"].(string), "img_") || m["url"] != "/admin/api/public/images/"+m["id"].(string) ||
			m["width"].(float64) != 64 || m["height"].(float64) != 32 || m["content_type"] != "image/png" || m["bytes"].(float64) <= 0 {
			t.Fatalf("upload response: %v", m)
		}
		return m["id"].(string)
	})
	if len(ids) == 0 {
		t.Fatal("the fixture references no images")
	}

	t.Run("full fixture round trip", func(t *testing.T) {
		got := admin.must(200, "PATCH", "/admin/api/settings", map[string]any{"branding": fixture})
		again := admin.must(200, "GET", "/admin/api/settings", nil)
		for _, m := range []map[string]any{got, again} {
			b := m["branding"].(map[string]any)
			if _, ok := b["favicon_url"]; !ok {
				t.Fatalf("favicon_url missing: %v", b)
			}
			for k, want := range withoutNulls(fixture).(map[string]any) {
				if !reflect.DeepEqual(withoutNulls(b[k]), want) {
					t.Fatalf("branding.%s:\n got %v\nwant %v", k, b[k], want)
				}
			}
		}
		// Patching the same thing again changes nothing.
		admin.must(200, "PATCH", "/admin/api/settings", map[string]any{"branding": fixture})
		b := admin.must(200, "GET", "/admin/api/settings", nil)["branding"].(map[string]any)
		if !reflect.DeepEqual(withoutNulls(b["home"]), withoutNulls(fixture["home"])) {
			t.Fatal("second patch changed home")
		}
	})

	t.Run("public branding and viewer resolve images", func(t *testing.T) {
		pb := n.client().must(200, "GET", "/admin/api/public/branding", nil)
		if _, ok := pb["home"]; ok {
			t.Fatal("home must not be public")
		}
		if !reflect.DeepEqual(pb["theme"], withoutNulls(fixture["theme"])) {
			t.Fatalf("theme: %v", pb["theme"])
		}
		login := pb["login"].(map[string]any)
		if id, _ := login["background_image"].(string); id == "" || login["background_image_url"] != "/admin/api/public/images/"+id {
			t.Fatalf("login: %v", login)
		}
		v := admin.must(200, "GET", "/admin/api/portal/viewer", nil)
		b := v["branding"].(map[string]any)
		if b["theme"] == nil {
			t.Fatal("viewer theme missing")
		}
		blocks := b["home"].(map[string]any)["blocks"].([]any)
		images := 0
		for _, rb := range blocks {
			blk := rb.(map[string]any)
			if id, ok := blk["image"].(string); ok {
				images++
				if blk["image_url"] != "/admin/api/public/images/"+id {
					t.Fatalf("block %v", blk)
				}
			}
			items, _ := blk["items"].([]any)
			for _, ri := range items {
				it := ri.(map[string]any)
				if id, ok := it["image"].(string); ok {
					images++
					if it["image_url"] != "/admin/api/public/images/"+id {
						t.Fatalf("tile %v", it)
					}
				}
			}
		}
		if images == 0 {
			t.Fatal("no resolved images in home")
		}
		// The stored settings stay without the resolved URLs.
		sb := admin.must(200, "GET", "/admin/api/settings", nil)["branding"].(map[string]any)
		if strings.Contains(string(mustJSON(sb)), "image_url") {
			t.Fatal("resolved URLs leaked into the settings")
		}
	})

	t.Run("image endpoints", func(t *testing.T) {
		list := admin.must(200, "GET", "/admin/api/settings/images", nil)["items"].([]any)
		if len(list) != len(ids) {
			t.Fatalf("list: %d items, want %d", len(list), len(ids))
		}
		for _, it := range list {
			if it.(map[string]any)["used"] != true {
				t.Fatalf("all fixture images are used: %v", it)
			}
		}
		var someID string
		for _, id := range ids {
			someID = id
		}
		// Public serving, no session, immutable caching.
		r := n.client().do("GET", "/admin/api/public/images/"+someID, nil)
		if r.Status != 200 || r.Header.Get("Content-Type") != "image/png" ||
			r.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" || r.Header.Get("X-Content-Type-Options") != "nosniff" || !bytes.HasPrefix(r.Body, []byte("\x89PNG")) {
			t.Fatalf("public image: %d %v", r.Status, r.Header)
		}
		if r := n.client().do("GET", "/admin/api/public/images/img_unknown", nil); r.Status != 404 {
			t.Fatalf("unknown image: %d", r.Status)
		}
		// Referenced: 409 conflict.
		r = admin.do("DELETE", "/admin/api/settings/images/"+someID, nil)
		if r.Status != 409 || r.JSON(t)["obx_error"] != "conflict" {
			t.Fatalf("delete referenced: %d %s", r.Status, r.Body)
		}
		// Unreferenced: 204, then gone.
		r = admin.uploadImage(testPNG(t, 10, 10, 200))
		if r.Status != 201 {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		spare := r.JSON(t)["id"].(string)
		list = admin.must(200, "GET", "/admin/api/settings/images", nil)["items"].([]any)
		for _, it := range list {
			if m := it.(map[string]any); m["id"] == spare && m["used"] != false {
				t.Fatalf("spare image is unused: %v", m)
			}
		}
		admin.must(204, "DELETE", "/admin/api/settings/images/"+spare, nil)
		if r := n.client().do("GET", "/admin/api/public/images/"+spare, nil); r.Status != 404 {
			t.Fatalf("deleted image still served: %d", r.Status)
		}
		admin.must(404, "DELETE", "/admin/api/settings/images/"+spare, nil)
		// Wrong type, too large, unauthenticated.
		if r := admin.uploadImage([]byte("GIF89a not an image we take")); r.Status != 415 {
			t.Fatalf("gif: %d %s", r.Status, r.Body)
		}
		if r := admin.uploadImage(testPNG(t, 8001, 1, 1)); r.Status != 422 {
			t.Fatalf("too wide: %d %s", r.Status, r.Body)
		}
		// Both style_numbers and query: allowed, style_numbers wins in the UI.
		both := map[string]any{"id": "p", "type": "products", "style_numbers": []any{"MN-1001"}, "query": "season=AW26"}
		admin.must(200, "PATCH", "/admin/api/settings", map[string]any{"branding": map[string]any{"home": map[string]any{"blocks": []any{both}}}})
		admin.must(200, "PATCH", "/admin/api/settings", map[string]any{"branding": fixture})
		// At most 200 images: the 201st is 409 limit_exceeded.
		var count int
		_ = n.app.DB.QueryRow(context.Background(), `SELECT COUNT(*) FROM branding_images`).Scan(&count)
		for i := count; i < maxBrandingImages; i++ {
			if _, err := n.app.DB.Exec(context.Background(), `INSERT INTO branding_images (id, sha256, content_type, width, height, bytes, created_at) VALUES (?, 'x', 'image/png', 1, 1, 1, 'x')`,
				"img_fill_"+strconv.Itoa(i)); err != nil {
				t.Fatal(err)
			}
		}
		if r := admin.uploadImage(testPNG(t, 4, 4, 9)); r.Status != 409 || r.JSON(t)["obx_error"] != "limit_exceeded" {
			t.Fatalf("201st image: %d %s", r.Status, r.Body)
		}
		_, _ = n.app.DB.Exec(context.Background(), `DELETE FROM branding_images WHERE id LIKE 'img_fill_%'`)
		anon := n.client()
		anon.session()
		body, ct := fileForm(t, testPNG(t, 4, 4, 1))
		if r := anon.do("POST", "/admin/api/settings/images", body, "Content-Type", ct); r.Status != 401 {
			t.Fatalf("anonymous upload: %d", r.Status)
		}
	})

	t.Run("favicon", func(t *testing.T) {
		if r := admin.do("PUT", "/admin/api/settings/favicon", testPNG(t, 32, 16, 1), "Content-Type", "image/png"); r.Status != 422 {
			t.Fatalf("non-square favicon: %d %s", r.Status, r.Body)
		}
		m := admin.must(200, "PUT", "/admin/api/settings/favicon", testPNG(t, 32, 32, 1), "Content-Type", "image/png")
		u, _ := m["branding"].(map[string]any)["favicon_url"].(string)
		if !strings.Contains(u, "/admin/api/public/favicon?v=") {
			t.Fatalf("favicon_url %q", u)
		}
		if pb := n.client().must(200, "GET", "/admin/api/public/branding", nil); pb["favicon_url"] != u {
			t.Fatalf("public favicon_url %v", pb["favicon_url"])
		}
		if r := n.client().do("GET", u, nil); r.Status != 200 || r.Header.Get("Content-Type") != "image/png" {
			t.Fatalf("favicon: %d", r.Status)
		}
		// A PATCH cannot set or clear it.
		m = admin.must(200, "PATCH", "/admin/api/settings", map[string]any{"branding": map[string]any{"favicon_url": nil}, "favicon_sha": "x"})
		if m["branding"].(map[string]any)["favicon_url"] != u {
			t.Fatal("PATCH changed the favicon")
		}
		m = admin.must(200, "DELETE", "/admin/api/settings/favicon", nil)
		if m["branding"].(map[string]any)["favicon_url"] != nil {
			t.Fatal("favicon not removed")
		}
		if r := n.client().do("GET", "/admin/api/public/favicon", nil); r.Status != 404 {
			t.Fatalf("removed favicon: %d", r.Status)
		}
	})

	t.Run("unknown members and block types are preserved", func(t *testing.T) {
		home := withoutNulls(fixture["home"]).(map[string]any)
		blocks := append([]any{}, home["blocks"].([]any)...)
		future := map[string]any{"id": "lookbook", "type": "lookbook", "pages": []any{map[string]any{"image": "anything", "n": 3.0}}, "style": "magazine"}
		blocks = append(blocks, future)
		patch := map[string]any{"branding": map[string]any{
			"theme": map[string]any{"x_future": map[string]any{"a": 1.0}, "colors": map[string]any{"accent2": "#123456"}},
			"login": map[string]any{"x_note": "kept"},
			"home":  map[string]any{"blocks": blocks, "x_layout": "wide"},
		}}
		b := admin.must(200, "PATCH", "/admin/api/settings", patch)["branding"].(map[string]any)
		b2 := admin.must(200, "GET", "/admin/api/settings", nil)["branding"].(map[string]any)
		for _, br := range []map[string]any{b, b2} {
			th := br["theme"].(map[string]any)
			if !reflect.DeepEqual(th["x_future"], map[string]any{"a": 1.0}) || th["colors"].(map[string]any)["accent2"] != "#123456" ||
				th["colors"].(map[string]any)["primary"] == nil {
				t.Fatalf("theme: %v", th)
			}
			if br["login"].(map[string]any)["x_note"] != "kept" || br["home"].(map[string]any)["x_layout"] != "wide" {
				t.Fatalf("login/home: %v %v", br["login"], br["home"])
			}
			bl := br["home"].(map[string]any)["blocks"].([]any)
			if !reflect.DeepEqual(bl[len(bl)-1], future) {
				t.Fatalf("future block: %v", bl[len(bl)-1])
			}
		}
		// The viewer passes it through as well.
		v := admin.must(200, "GET", "/admin/api/portal/viewer", nil)
		bl := v["branding"].(map[string]any)["home"].(map[string]any)["blocks"].([]any)
		if bl[len(bl)-1].(map[string]any)["type"] != "lookbook" {
			t.Fatal("viewer dropped the unknown block")
		}
		// Clearing works with null.
		admin.must(200, "PATCH", "/admin/api/settings", map[string]any{"branding": map[string]any{"home": map[string]any{"x_layout": nil}}})
	})

	t.Run("validation errors", func(t *testing.T) {
		long := strings.Repeat("a", 2001)
		cases := []struct {
			name    string
			patch   map[string]any
			pointer string
		}{
			{"colour", map[string]any{"theme": map[string]any{"colors": map[string]any{"primary": "red"}}}, "/branding/theme/colors/primary"},
			{"image background", map[string]any{"theme": map[string]any{"image_background": "#12345"}}, "/branding/theme/image_background"},
			{"scheme", map[string]any{"theme": map[string]any{"color_scheme": "sepia"}}, "/branding/theme/color_scheme"},
			{"font", map[string]any{"theme": map[string]any{"heading_font": "comic-sans"}}, "/branding/theme/heading_font"},
			{"radius", map[string]any{"theme": map[string]any{"radius": 4}}, "/branding/theme/radius"},
			{"theme not object", map[string]any{"theme": "dark"}, "/branding/theme"},
			{"login layout", map[string]any{"login": map[string]any{"layout": "left"}}, "/branding/login/layout"},
			{"login image", map[string]any{"login": map[string]any{"background_image": "img_missing"}}, "/branding/login/background_image"},
			{"login title length", map[string]any{"login": map[string]any{"title": map[string]any{"en": long}}}, "/branding/login/title/en"},
			{"login title shape", map[string]any{"login": map[string]any{"title": "plain"}}, "/branding/login/title"},
			{"text block body", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "t", "type": "text", "body": map[string]any{"en": strings.Repeat("b", 10001)}}}}}, "/branding/home/blocks/0/body/en"},
			{"block id", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"type": "text"}}}}, "/branding/home/blocks/0/id"},
			{"block id length", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": strings.Repeat("x", 41), "type": "text"}}}}, "/branding/home/blocks/0/id"},
			{"duplicate id", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "a", "type": "text"}, map[string]any{"id": "a", "type": "text"}}}}, "/branding/home/blocks/1/id"},
			{"too many blocks", map[string]any{"home": map[string]any{"blocks": manyBlocks(31)}}, "/branding/home/blocks"},
			{"javascript link", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "h", "type": "hero", "link": "javascript:alert(1)"}}}}, "/branding/home/blocks/0/link"},
			{"http link", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "h", "type": "hero", "link": "http://example.com"}}}}, "/branding/home/blocks/0/link"},
			{"protocol-relative link", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "h", "type": "hero", "link": "//evil.example/x"}}}}, "/branding/home/blocks/0/link"},
			{"hero image", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "h", "type": "hero", "image": "img_nope"}}}}, "/branding/home/blocks/0/image"},
			{"hero align", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "h", "type": "hero", "align": "right"}}}}, "/branding/home/blocks/0/align"},
			{"no tiles", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "t", "type": "tiles", "items": []any{}}}}}, "/branding/home/blocks/0/items"},
			{"tile columns", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "t", "type": "tiles", "columns": 5, "items": []any{map[string]any{}}}}}}, "/branding/home/blocks/0/columns"},
			{"tile link", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "t", "type": "tiles", "items": []any{map[string]any{"link": "ftp://x"}}}}}}, "/branding/home/blocks/0/items/0/link"},
			{"products max", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "p", "type": "products", "max": 25}}}}, "/branding/home/blocks/0/max"},
			{"products query with ?", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "p", "type": "products", "query": "?season=AW26"}}}}, "/branding/home/blocks/0/query"},
			{"style number length", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "p", "type": "products", "style_numbers": []any{strings.Repeat("s", 101)}}}}}, "/branding/home/blocks/0/style_numbers/0"},
			{"unknown type name", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "u", "type": "Look-Book"}}}}, "/branding/home/blocks/0/type"},
			{"backslash link", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "h", "type": "hero", "link": "/\\evil.example"}}}}, "/branding/home/blocks/0/link"},
			{"contact email", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "c", "type": "contact", "email": "not-an-address"}}}}, "/branding/home/blocks/0/email"},
			{"contact phone", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "c", "type": "contact", "phone": strings.Repeat("1", 51)}}}}, "/branding/home/blocks/0/phone"},
			{"announcement tone", map[string]any{"home": map[string]any{"blocks": []any{map[string]any{"id": "a", "type": "announcement", "tone": "danger"}}}}, "/branding/home/blocks/0/tone"},
			{"whole branding size", map[string]any{"home": map[string]any{"x_big": strings.Repeat("z", 70<<10)}}, "/branding"},
		}
		before := admin.must(200, "GET", "/admin/api/settings", nil)["branding"]
		for _, tc := range cases {
			r := admin.do("PATCH", "/admin/api/settings", map[string]any{"branding": tc.patch})
			if r.Status != 422 {
				t.Fatalf("%s: want 422, got %d %s", tc.name, r.Status, r.Body)
			}
			m := r.JSON(t)
			found := false
			for _, e := range m["errors"].([]any) {
				if e.(map[string]any)["pointer"] == tc.pointer {
					found = true
				}
			}
			if m["obx_error"] != "validation_failed" || !found {
				t.Fatalf("%s: want pointer %s: %s", tc.name, tc.pointer, r.Body)
			}
		}
		after := admin.must(200, "GET", "/admin/api/settings", nil)["branding"]
		if !reflect.DeepEqual(before, after) {
			t.Fatal("a refused patch changed the settings")
		}
	})
}

func manyBlocks(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = map[string]any{"id": "b" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26)), "type": "text"}
	}
	return out
}
