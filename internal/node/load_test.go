package node

import (
	"context"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
)

// TestLoad seeds a large catalog and measures the hot read paths. It runs
// only with OBX_LOAD_TEST=1 (a few minutes); docs/performance.md lists the
// numbers and how to run it. OBX_LOAD_PRODUCTS scales the catalog (default
// 10000 products = 60000 variants and 40000 assets).
func TestLoad(t *testing.T) {
	if os.Getenv("OBX_LOAD_TEST") == "" {
		t.Skip("set OBX_LOAD_TEST=1 to run the load test")
	}
	products := 10000
	if v, err := strconv.Atoi(os.Getenv("OBX_LOAD_PRODUCTS")); err == nil && v > 0 {
		products = v
	}
	n := newTestNode(t)
	admin := n.setup()
	ctx := context.Background()
	start := time.Now()

	// Four distinct originals, rendered up front, shared by all assets
	// (the metadata is what is measured, not image bytes).
	imgs, _ := filepath.Glob(filepath.Join(specDir(t), "kestrel", "images", "*.png"))
	var shas []string
	for _, p := range imgs[:4] {
		data, _ := os.ReadFile(p)
		b, err := n.app.PutBlobBytes(ctx, data)
		if err != nil {
			t.Fatal(err)
		}
		if err := n.app.RenderOriginal(ctx, b.SHA256); err != nil {
			t.Fatal(err)
		}
		shas = append(shas, b.SHA256)
	}
	colors := []string{"BLK", "NVY", "OAT"}
	sizes := []string{"S", "M", "L"}
	seasons := []string{"SS26", "AW26", "SS27"}
	types := []string{"t_shirt", "sweater", "jacket", "trousers", "dress"}
	var gtins []string
	const chunk = 100
	for from := 0; from < products; from += chunk {
		err := n.app.DB.WriteTx(ctx, func(tx *db.Tx) error {
			w := n.app.newWriter(ctx, tx)
			if from == 0 {
				if _, p := w.Upsert(KindBrand, "loadbrand", map[string]any{"name": "Load Brand"}, ""); p != nil {
					return p
				}
			}
			for i := from; i < from+chunk && i < products; i++ {
				style := fmt.Sprintf("LD-%05d", i)
				if _, p := w.Upsert(KindProduct, style, map[string]any{"brand_external_id": "loadbrand", "style_number": style,
					"name":      map[string]any{"en": "Load product " + strconv.Itoa(i), "nl": "Laadproduct " + strconv.Itoa(i)},
					"item_type": types[i%len(types)], "target_group": "unisex", "season": seasons[i%len(seasons)]}, ""); p != nil {
					return p
				}
				for c := 0; c < 2; c++ {
					for s := 0; s < 3; s++ {
						base := fmt.Sprintf("20%07d%d%d0", i, c, s)
						g := base + ean13Check(base)
						gtins = append(gtins, g)
						if _, p := w.Upsert(KindVariant, g, map[string]any{"product_external_id": style, "gtin": g,
							"color": map[string]any{"code": colors[(i+c)%len(colors)]}, "size": sizes[s]}, ""); p != nil {
							return p
						}
					}
				}
				for pos := 1; pos <= 4; pos++ {
					col := colors[(i+(pos-1)/2)%len(colors)]
					if _, p := w.Upsert(KindAsset, fmt.Sprintf("%s_%s_%d", style, col, pos), map[string]any{"product_external_id": style,
						"color_code": col, "role": "packshot", "position": pos,
						"files": map[string]any{"original": map[string]any{"sha256": shas[(i+pos)%len(shas)]}}}, ""); p != nil {
						return p
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("seed at %d: %v", from, err)
		}
	}
	seeded := time.Since(start)
	var nv, na, nc int
	_ = n.app.DB.QueryRow(ctx, `SELECT COUNT(*) FROM variants WHERE visible = 1`).Scan(&nv)
	_ = n.app.DB.QueryRow(ctx, `SELECT COUNT(*) FROM assets WHERE visible = 1`).Scan(&na)
	_ = n.app.DB.QueryRow(ctx, `SELECT COUNT(*) FROM changes`).Scan(&nc)
	t.Logf("seeded %d products, %d visible variants, %d visible assets, %d change entries in %s", products, nv, na, nc, seeded.Round(time.Second))
	if nv != products*6 || na != products*4 {
		t.Fatalf("seed: %d variants, %d assets visible", nv, na)
	}
	if keep := os.Getenv("OBX_LOAD_KEEP"); keep != "" {
		// A copy of the seeded database for EXPLAIN QUERY PLAN sessions.
		_ = os.Remove(keep)
		if _, err := n.app.DB.SQL.ExecContext(ctx, `VACUUM INTO ?`, keep); err != nil {
			t.Fatal(err)
		}
	}

	api, _ := n.apiClient(admin, "Load Retailer", map[string]any{})
	results := map[string][]time.Duration{}
	measure := func(name string, runs int, fn func(i int)) {
		for i := 0; i < runs; i++ {
			t0 := time.Now()
			fn(i)
			results[name] = append(results[name], time.Since(t0))
		}
	}
	rng := rand.New(rand.NewSource(1))

	// Portal: first page, filtered, searched, deep pages, facets.
	measure("portal browse: first page", 30, func(int) { admin.must(200, "GET", "/admin/api/portal/products", nil) })
	measure("portal browse: filter season+type", 30, func(i int) {
		admin.must(200, "GET", "/admin/api/portal/products?season="+seasons[i%3]+"&item_type="+types[i%5], nil)
	})
	measure("portal search: style number", 30, func(i int) {
		admin.must(200, "GET", "/admin/api/portal/products?q="+fmt.Sprintf("LD-%05d", rng.Intn(products)), nil)
	})
	measure("portal search: word", 30, func(i int) { admin.must(200, "GET", "/admin/api/portal/products?q=laadproduct", nil) })
	measure("portal facets", 30, func(int) { admin.must(200, "GET", "/admin/api/portal/facets", nil) })
	cursor := ""
	measure("portal browse: next page (walk 50 pages)", 50, func(int) {
		q := "/admin/api/portal/products"
		if cursor != "" {
			q += "?cursor=" + url.QueryEscape(cursor)
		}
		page := admin.must(200, "GET", q, nil)
		if c, ok := page["next_cursor"].(string); ok {
			cursor = c
		}
	})
	measure("portal product page", 50, func(i int) {
		list := admin.must(200, "GET", "/admin/api/portal/products?q="+fmt.Sprintf("LD-%05d", rng.Intn(products)), nil)
		items := list["items"].([]any)
		if len(items) > 0 {
			admin.must(200, "GET", "/admin/api/portal/products/"+items[0].(map[string]any)["id"].(string), nil)
		}
	})
	measure("portal EAN list (100 EANs)", 40, func(int) {
		var list []string
		for j := 0; j < 100; j++ {
			list = append(list, gtins[rng.Intn(len(gtins))])
		}
		admin.must(200, "POST", "/admin/api/portal/gtin-lookup", map[string]any{"gtins": list})
	})

	// Protocol: full /changes snapshot at limit 500, by-gtin, product pages.
	var pages int
	cursor = ""
	for {
		t0 := time.Now()
		q := "/obx/v1/changes?limit=500"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		page := api.must(200, "GET", q, nil)
		results["/changes page (limit 500, full snapshot)"] = append(results["/changes page (limit 500, full snapshot)"], time.Since(t0))
		pages++
		cursor = page["next_cursor"].(string)
		if !page["has_more"].(bool) {
			break
		}
	}
	measure("/products:by-gtin GET (1 EAN)", 200, func(int) {
		api.must(200, "GET", "/obx/v1/products:by-gtin?gtin="+gtins[rng.Intn(len(gtins))], nil)
	})
	measure("/products:by-gtin POST (500 EANs)", 20, func(int) {
		var list []string
		for j := 0; j < 500; j++ {
			list = append(list, gtins[rng.Intn(len(gtins))])
		}
		api.must(200, "POST", "/obx/v1/products:by-gtin", map[string]any{"gtins": list})
	})
	measure("/products list page (limit 100)", 30, func(int) { api.must(200, "GET", "/obx/v1/products?limit=100", nil) })
	measure("admin catalog list", 30, func(int) { admin.must(200, "GET", "/admin/api/catalog/products", nil) })

	names := make([]string, 0, len(results))
	for k := range results {
		names = append(names, k)
	}
	sort.Strings(names)
	var report strings.Builder
	fmt.Fprintf(&report, "\n| Request | runs | p50 | p95 | max |\n|---|---:|---:|---:|---:|\n")
	failures := []string{}
	for _, name := range names {
		d := results[name]
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		p50, p95, mx := d[len(d)/2], d[(len(d)*95)/100], d[len(d)-1]
		if len(d) < 20 {
			p95 = mx
		}
		fmt.Fprintf(&report, "| %s | %d | %s | %s | %s |\n", name, len(d), ms(p50), ms(p95), ms(mx))
		// Budgets: generous for slow CI machines, tight enough to catch a
		// missing index or an N+1 (those are 10-100x slower).
		budget := 300 * time.Millisecond
		if strings.Contains(name, "(1 EAN)") {
			budget = 50 * time.Millisecond
		}
		if strings.Contains(name, "EANs") || strings.Contains(name, "/changes") {
			budget = 800 * time.Millisecond
		}
		if p95 > budget {
			failures = append(failures, fmt.Sprintf("%s: p95 %s > %s", name, ms(p95), ms(budget)))
		}
	}
	t.Logf("load test: %d products, %d change pages%s", products, pages, report.String())
	for _, f := range failures {
		t.Error(f)
	}
}

// ean13Check returns the check digit for 12 digits.
func ean13Check(base string) string {
	sum := 0
	for j, ch := range base {
		d := int(ch - '0')
		if j%2 == 1 {
			d *= 3
		}
		sum += d
	}
	return strconv.Itoa((10 - sum%10) % 10)
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }
