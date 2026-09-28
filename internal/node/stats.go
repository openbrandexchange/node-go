package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/ids"
	"github.com/openbrandexchange/node-go/internal/problem"
)

// downloadEvent is one logged delivery.
type downloadEvent struct {
	Kind       string // image, product_zip, selection_zip, bulk_zip, csv, api_rendition, api_export, usage
	Viewer     *Viewer
	Variant    string
	ProductID  string
	AssetID    string
	UsageEvent string
	Files      int
	Bytes      int64
	At         string
}

// logDownload stores a statistics event. Admin previews are not logged.
func (a *App) logDownload(ctx context.Context, e downloadEvent) {
	v := e.Viewer
	if v == nil || v.Preview() || v.RetailerID == "" {
		return
	}
	actorType, actorID, actorName := "user", v.UserID, v.UserName
	if v.Kind == "client" {
		actorType, actorID, actorName = "api_client", v.ClientRowID, v.ClientName
	}
	var style *string
	if e.ProductID != "" {
		var s string
		if a.DB.QueryRow(ctx, `SELECT style_number FROM products WHERE id = ?`, e.ProductID).Scan(&s) == nil {
			style = &s
		}
	}
	at := e.At
	if at == "" {
		at = a.nowStr()
	}
	var bytes any
	if e.Bytes > 0 {
		bytes = e.Bytes
	}
	_, err := a.DB.Exec(ctx, `INSERT INTO download_events (id, at, kind, retailer_id, retailer_name, actor_type, actor_id, actor_name, variant, product_id, style_number, asset_id, usage_event, files, bytes) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ids.New("dle"), at, e.Kind, v.RetailerID, v.RetailerName, actorType, actorID, actorName, nullIfEmpty(e.Variant), nullIfEmpty(e.ProductID), style,
		nullIfEmpty(e.AssetID), nullIfEmpty(e.UsageEvent), e.Files, bytes)
	if err != nil {
		a.Log.Warn("download event not stored", "error", err)
	}
	_, _ = a.DB.Exec(ctx, `UPDATE retailers SET last_activity_at = ? WHERE id = ?`, a.nowStr(), v.RetailerID)
}

// handleUsage accepts usage events (protocol).
func (a *App) handleUsage(w http.ResponseWriter, r *http.Request) {
	v, ok := a.protocolAuth(w, r, "usage:write")
	if !ok {
		return
	}
	doc, p := readJSONObject(r)
	if p != nil {
		problem.Write(w, p)
		return
	}
	if errs := lenientValidate("Usage", doc); errs != nil {
		problem.Write(w, validationProblem(errs))
		return
	}
	var body struct {
		Events []struct {
			AssetID   string  `json:"asset_id"`
			Rendition *string `json:"rendition"`
			Event     string  `json:"event"`
			At        string  `json:"at"`
			Count     *int    `json:"count"`
		} `json:"events"`
	}
	_ = decodeInto(doc, &body)
	accepted := 0
	ctx := r.Context()
	for _, e := range body.Events {
		row, err := a.visibleAsset(ctx, v, e.AssetID)
		if err != nil || row == nil {
			continue
		}
		n := 1
		if e.Count != nil && *e.Count > 0 {
			n = *e.Count
		}
		at := a.nowStr()
		if t, err := time.Parse(time.RFC3339Nano, e.At); err == nil && t.Before(a.now().Add(time.Hour)) {
			at = db.Fmt(t)
		}
		a.logDownload(ctx, downloadEvent{Kind: "usage", Viewer: v, Variant: deref(e.Rendition), ProductID: row.ProductID, AssetID: row.ID, UsageEvent: e.Event, Files: n, At: at})
		accepted++
	}
	writeJSON(w, 202, map[string]int{"accepted": accepted})
}

var imageKinds = map[string]bool{"image": true, "product_zip": true, "selection_zip": true, "bulk_zip": true, "api_rendition": true, "api_export": true}

func (a *App) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	days := atoiDefault(r.URL.Query().Get("days"), 30)
	switch days {
	case 7, 30, 90, 365:
	default:
		failP(w, "invalid_request", "days must be 7, 30, 90 or 365.")
		return
	}
	start := a.now().AddDate(0, 0, -days+1)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	rs, err := a.DB.Query(ctx, `SELECT at, kind, retailer_id, retailer_name, product_id, style_number, files FROM download_events WHERE at >= ?`, db.Fmt(start))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	type agg struct {
		downloads, images int
		last              string
		name              string
		style             string
	}
	totals := map[string]int{"downloads": 0, "images": 0, "zips": 0, "csv_exports": 0, "api_downloads": 0, "usage_events": 0, "active_retailers": 0}
	byDay := map[string]*agg{}
	byRet := map[string]*agg{}
	byProd := map[string]*agg{}
	for rs.Next() {
		var at, kind string
		var ret, retName, prod, style sql.NullString
		var files int
		if err := rs.Scan(&at, &kind, &ret, &retName, &prod, &style, &files); err != nil {
			continue
		}
		day := at[:10]
		if kind == "usage" {
			totals["usage_events"] += files
			continue
		}
		imgs := 0
		if imageKinds[kind] {
			imgs = files
		}
		totals["downloads"]++
		totals["images"] += imgs
		switch kind {
		case "product_zip", "selection_zip", "bulk_zip":
			totals["zips"]++
		case "csv":
			totals["csv_exports"]++
		case "api_rendition", "api_export":
			totals["api_downloads"]++
		}
		d := byDay[day]
		if d == nil {
			d = &agg{}
			byDay[day] = d
		}
		d.downloads++
		d.images += imgs
		if ret.Valid {
			x := byRet[ret.String]
			if x == nil {
				x = &agg{name: retName.String}
				byRet[ret.String] = x
			}
			x.downloads++
			x.images += imgs
			if at > x.last {
				x.last = at
			}
		}
		if prod.Valid {
			x := byProd[prod.String]
			if x == nil {
				x = &agg{style: style.String}
				byProd[prod.String] = x
			}
			x.downloads++
		}
	}
	rs.Close()
	totals["active_retailers"] = len(byRet)
	var days_ []map[string]any
	for i := 0; i < days; i++ {
		d := start.AddDate(0, 0, i).Format("2006-01-02")
		x := byDay[d]
		if x == nil {
			x = &agg{}
		}
		days_ = append(days_, map[string]any{"date": d, "downloads": x.downloads, "images": x.images})
	}
	type kv struct {
		id string
		x  *agg
	}
	var rets []kv
	for id, x := range byRet {
		rets = append(rets, kv{id, x})
	}
	sort.Slice(rets, func(i, j int) bool {
		if rets[i].x.downloads != rets[j].x.downloads {
			return rets[i].x.downloads > rets[j].x.downloads
		}
		return rets[i].id < rets[j].id
	})
	topR := []map[string]any{}
	for i, k := range rets {
		if i == 20 {
			break
		}
		var last *string
		if k.x.last != "" {
			l := k.x.last
			last = &l
		}
		topR = append(topR, map[string]any{"retailer_id": k.id, "name": k.x.name, "downloads": k.x.downloads, "images": k.x.images, "last_activity_at": last})
	}
	var prods []kv
	for id, x := range byProd {
		prods = append(prods, kv{id, x})
	}
	sort.Slice(prods, func(i, j int) bool {
		if prods[i].x.downloads != prods[j].x.downloads {
			return prods[i].x.downloads > prods[j].x.downloads
		}
		return prods[i].id < prods[j].id
	})
	topP := []map[string]any{}
	for i, k := range prods {
		if i == 20 {
			break
		}
		item := map[string]any{"product_id": k.id, "style_number": k.x.style, "downloads": k.x.downloads}
		var data string
		if a.DB.QueryRow(ctx, `SELECT data FROM products WHERE id = ?`, k.id).Scan(&data) == nil {
			var d ProductData
			_ = json.Unmarshal([]byte(data), &d)
			item["name"] = d.Name
		}
		topP = append(topP, item)
	}
	writeJSON(w, 200, map[string]any{"days": days, "totals": totals, "by_day": days_, "top_retailers": topR, "top_products": topP})
}

func (a *App) handleStatsEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, p := limitParam(r, 50, 200)
	if p != nil {
		problem.Write(w, p)
		return
	}
	q := r.URL.Query()
	where := `WHERE 1 = 1`
	var args []any
	if v := q.Get("retailer_id"); v != "" {
		where += ` AND retailer_id = ?`
		args = append(args, v)
	}
	if v := q.Get("kind"); v != "" {
		where += ` AND kind = ?`
		args = append(args, v)
	}
	if c := q.Get("cursor"); c != "" {
		var cur struct {
			At string `json:"a"`
			ID string `json:"i"`
		}
		if !a.decodeCursor("events", c, &cur) {
			failP(w, "cursor_invalid", "The cursor is not valid.")
			return
		}
		where += ` AND (at < ? OR (at = ? AND id < ?))`
		args = append(args, cur.At, cur.At, cur.ID)
	}
	rs, err := a.DB.Query(ctx, `SELECT id, at, kind, retailer_id, retailer_name, actor_type, actor_name, variant, product_id, asset_id, usage_event, files, bytes FROM download_events `+where+` ORDER BY at DESC, id DESC LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items := []map[string]any{}
	for rs.Next() {
		var id, at, kind, actorType string
		var ret, retName, actorName, variant, prod, asset, usage sql.NullString
		var files int
		var bytes sql.NullInt64
		if err := rs.Scan(&id, &at, &kind, &ret, &retName, &actorType, &actorName, &variant, &prod, &asset, &usage, &files, &bytes); err != nil {
			continue
		}
		var b any
		if bytes.Valid {
			b = bytes.Int64
		}
		items = append(items, map[string]any{"id": id, "at": at, "kind": kind, "retailer_id": nullStr(ret), "retailer_name": nullStr(retName),
			"actor_type": actorType, "actor_name": nullStr(actorName), "variant": nullStr(variant), "product_id": nullStr(prod),
			"asset_id": nullStr(asset), "usage_event": nullStr(usage), "files": files, "bytes": b})
	}
	rs.Close()
	var next *string
	if len(items) > limit {
		items = items[:limit]
		last := items[limit-1]
		c := a.encodeCursor("events", map[string]string{"a": last["at"].(string), "i": last["id"].(string)})
		next = &c
	}
	writeJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}

var _ = strconv.Itoa
