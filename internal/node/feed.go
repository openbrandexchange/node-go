package node

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"time"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/problem"
)

// feedCursor is the decoded /changes cursor.
type feedCursor struct {
	Phase string `json:"p"` // s = snapshot, l = live
	HW    int64  `json:"h,omitempty"`
	Type  int    `json:"t,omitempty"`
	After string `json:"a,omitempty"`
	At    int64  `json:"c,omitempty"`
	Seq   int64  `json:"s,omitempty"`
	// Epoch is the node's cursor epoch when the cursor was issued (see
	// cursorEpoch); cursors from an earlier epoch are cursor_expired.
	Epoch int64 `json:"e,omitempty"`
}

// cursorEpoch is the node's current /changes cursor epoch (meta
// cursor_epoch, 0 when absent). A spec upgrade with incompatible object
// shapes bumps it in the same transaction as the migration (migration 4 for
// 0.2 -> 0.3), so every cursor issued before the upgrade, which carries the
// old epoch or none at all, answers 410 cursor_expired (protocol.md §6.4).
func (a *App) cursorEpoch(ctx context.Context, q db.Querier) (int64, error) {
	v, err := getMeta(ctx, q, "cursor_epoch")
	if err != nil || v == "" {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// SimulateSpecUpgrade bumps the cursor epoch the way an upgrade to
// incompatible object shapes does, so every /changes cursor issued so far
// expires. The conformance operator action simulate_spec_upgrade (P-FEED-7)
// uses it; the catalog is not touched.
func (a *App) SimulateSpecUpgrade(ctx context.Context) error {
	return a.DB.WriteTx(ctx, func(tx *db.Tx) error {
		e, err := a.cursorEpoch(ctx, tx)
		if err != nil {
			return err
		}
		return setMeta(ctx, tx, "cursor_epoch", strconv.FormatInt(e+1, 10))
	})
}

var snapshotTypes = []string{KindBrand, KindProduct, KindVariant, KindAsset}

// Change is one feed entry.
type Change struct {
	Seq        int64   `json:"seq"`
	Type       string  `json:"type"`
	Op         string  `json:"op"`
	ID         string  `json:"id"`
	ExternalID *string `json:"external_id"`
	BrandID    *string `json:"brand_id"`
	ProductID  *string `json:"product_id"`
	UpdatedAt  string  `json:"updated_at"`
	Data       any     `json:"data,omitempty"`
	Resync     *bool   `json:"resync,omitempty"`
}

// ChangesPage is one page of the feed.
type ChangesPage struct {
	Changes    []Change `json:"changes"`
	NextCursor string   `json:"next_cursor"`
	HasMore    bool     `json:"has_more"`
	Phase      string   `json:"phase"`
}

func (a *App) maxSeq(ctx context.Context) (int64, error) {
	var s sql.NullInt64
	err := a.DB.QueryRow(ctx, `SELECT MAX(seq) FROM changes`).Scan(&s)
	return s.Int64, err
}

func (a *App) handleChanges(w http.ResponseWriter, r *http.Request) {
	v, ok := a.protocolAuth(w, r, "catalog:read")
	if !ok {
		return
	}
	limit, p := limitParam(r, 100, 500)
	if p != nil {
		problem.Write(w, p)
		return
	}
	page, prob, err := a.Changes(r.Context(), v, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if prob != nil {
		problem.Write(w, prob)
		return
	}
	writeJSON(w, 200, page)
}

// Changes returns one feed page for viewer v.
func (a *App) Changes(ctx context.Context, v *Viewer, cursor string, limit int) (*ChangesPage, *problem.Problem, error) {
	epoch, err := a.cursorEpoch(ctx, a.DB)
	if err != nil {
		return nil, nil, err
	}
	var cur feedCursor
	if cursor == "" {
		hw, err := a.maxSeq(ctx)
		if err != nil {
			return nil, nil, err
		}
		cur = feedCursor{Phase: "s", HW: hw, At: a.now().Unix(), Epoch: epoch}
	} else if !a.decodeCursor("changes", cursor, &cur) || (cur.Phase != "s" && cur.Phase != "l") {
		return nil, problem.New("cursor_invalid", "The cursor is malformed or from another node; restart without a cursor."), nil
	} else if cur.Epoch != epoch {
		return nil, problem.New("cursor_expired", "This cursor was issued before the node upgraded to incompatible object shapes (now spec "+SpecVersion+"); restart without a cursor."), nil
	}
	if cur.Phase == "s" {
		if cursor != "" && a.now().Sub(time.Unix(cur.At, 0)) > 24*time.Hour {
			return nil, problem.New("cursor_expired", "This snapshot is older than 24 hours; restart without a cursor."), nil
		}
		return a.snapshotPage(ctx, v, cur, limit)
	}
	pruned, _ := getMeta(ctx, a.DB, "changes_pruned_below")
	if pb, _ := strconv.ParseInt(pruned, 10, 64); pb > 0 && cur.Seq < pb {
		return nil, problem.New("cursor_expired", "This cursor is older than the retained history; restart without a cursor."), nil
	}
	return a.livePage(ctx, v, cur.Seq, epoch, limit)
}

func (a *App) snapshotPage(ctx context.Context, v *Viewer, cur feedCursor, limit int) (*ChangesPage, *problem.Problem, error) {
	page := &ChangesPage{Changes: []Change{}, Phase: "snapshot", HasMore: true}
	for cur.Type < len(snapshotTypes) {
		kind := snapshotTypes[cur.Type]
		col := "brand_id"
		if kind == KindBrand {
			col = "id"
		}
		bf, bargs := v.brandFilterSQL(col)
		extra := ""
		switch kind {
		case KindVariant, KindAsset:
			extra = ", product_id"
		default:
			extra = ", ''"
		}
		exp := ", NULL"
		if kind == KindAsset {
			exp = ", expires_at"
		}
		args := append([]any{cur.After}, bargs...)
		args = append(args, limit-len(page.Changes)+1)
		rs, err := a.DB.Query(ctx, `SELECT id, external_id, `+col+`, rep, updated_at, last_seq`+extra+exp+` FROM `+tableOf(kind)+` WHERE visible = 1 AND id > ?`+bf+` ORDER BY id LIMIT ?`, args...)
		if err != nil {
			return nil, nil, err
		}
		type item struct {
			id, ext, brand, rep, up, prod string
			seq                           int64
			exp                           *string
		}
		var items []item
		for rs.Next() {
			var it item
			if err := rs.Scan(&it.id, &it.ext, &it.brand, &it.rep, &it.up, &it.seq, &it.prod, &it.exp); err != nil {
				rs.Close()
				return nil, nil, err
			}
			items = append(items, it)
		}
		rs.Close()
		room := limit - len(page.Changes)
		more := len(items) > room
		if more {
			items = items[:room]
		}
		for _, it := range items {
			cur.After = it.id
			if kind == KindAsset && !a.assetVisibleNow(it.exp) {
				continue
			}
			ext, brand := it.ext, it.brand
			c := Change{Seq: max(it.seq, 1), Type: kind, Op: "upsert", ID: it.id, ExternalID: &ext, BrandID: &brand, UpdatedAt: it.up}
			if it.prod != "" {
				pid := it.prod
				c.ProductID = &pid
			}
			c.Data = a.renderData(kind, it.rep, it.up, v)
			page.Changes = append(page.Changes, c)
		}
		if more || len(page.Changes) >= limit {
			page.NextCursor = a.encodeCursor("changes", cur)
			return page, nil, nil
		}
		cur.Type++
		cur.After = ""
	}
	// Snapshot exhausted: continue live from the high-water mark.
	page.NextCursor = a.encodeCursor("changes", feedCursor{Phase: "l", Seq: cur.HW, Epoch: cur.Epoch})
	return page, nil, nil
}

func (a *App) renderData(kind, rep, up string, v *Viewer) any {
	switch kind {
	case KindBrand:
		return renderBrand(rep, up)
	case KindProduct:
		return renderProduct(rep, up)
	case KindVariant:
		return renderVariant(rep, up)
	case KindAsset:
		return a.renderAsset(rep, up, v)
	}
	return nil
}

func (a *App) livePage(ctx context.Context, v *Viewer, after, epoch int64, limit int) (*ChangesPage, *problem.Problem, error) {
	page := &ChangesPage{Changes: []Change{}, Phase: "live"}
	last := after
	scanned := 0
	for len(page.Changes) < limit && scanned < 5000 {
		rs, err := a.DB.Query(ctx, `SELECT seq, type, op, object_id, external_id, brand_id, product_id, retailer_id, resync, at FROM changes WHERE seq > ? ORDER BY seq LIMIT 500`, last)
		if err != nil {
			return nil, nil, err
		}
		type entry struct {
			seq                        int64
			typ, op, id, at            string
			ext, brand, prod, retailer sql.NullString
			resync                     int
		}
		var batch []entry
		for rs.Next() {
			var e entry
			if err := rs.Scan(&e.seq, &e.typ, &e.op, &e.id, &e.ext, &e.brand, &e.prod, &e.retailer, &e.resync, &e.at); err != nil {
				rs.Close()
				return nil, nil, err
			}
			batch = append(batch, e)
		}
		rs.Close()
		if len(batch) == 0 {
			break
		}
		for _, e := range batch {
			if len(page.Changes) >= limit {
				break
			}
			last = e.seq
			scanned++
			if e.typ == "grant" {
				if !e.retailer.Valid || e.retailer.String != v.RetailerID {
					continue
				}
				ret, err := a.loadRetailer(ctx, a.DB, v.RetailerID)
				if err != nil || ret == nil {
					continue
				}
				g := a.grantOf(ctx, ret)
				resync := e.resync == 1
				page.Changes = append(page.Changes, Change{Seq: e.seq, Type: "grant", Op: "upsert", ID: e.id, UpdatedAt: g.UpdatedAt, Data: g, Resync: &resync})
				continue
			}
			if !e.brand.Valid || !v.SeesBrand(e.brand.String) {
				continue
			}
			c := Change{Seq: e.seq, Type: e.typ, Op: e.op, ID: e.id, ExternalID: nullStr(e.ext), BrandID: nullStr(e.brand), ProductID: nullStr(e.prod), UpdatedAt: e.at}
			if e.op == "upsert" {
				row, err := loadRowByID(ctx, a.DB, e.typ, e.id)
				if err != nil {
					return nil, nil, err
				}
				if row == nil || !row.Visible {
					continue // a later delete entry covers it
				}
				if e.typ == KindAsset {
					var exp *string
					_ = a.DB.QueryRow(ctx, `SELECT expires_at FROM assets WHERE id = ?`, e.id).Scan(&exp)
					if !a.assetVisibleNow(exp) {
						continue
					}
				}
				c.UpdatedAt = row.UpdatedAt
				c.Data = a.renderData(e.typ, row.Rep, row.UpdatedAt, v)
			}
			page.Changes = append(page.Changes, c)
		}
		if len(batch) < 500 {
			break
		}
	}
	var n int
	_ = a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM (SELECT seq FROM changes WHERE seq > ? LIMIT 1) x`, last).Scan(&n)
	page.HasMore = n > 0
	page.NextCursor = a.encodeCursor("changes", feedCursor{Phase: "l", Seq: last, Epoch: epoch})
	return page, nil, nil
}

// pruneChanges removes entries older than the retention window and
// remembers the watermark so older cursors get cursor_expired.
func (a *App) pruneChanges(ctx context.Context) {
	cutoff := db.Fmt(a.now().AddDate(0, 0, -a.Cfg.ChangeRetentionDays))
	var s sql.NullInt64
	if err := a.DB.QueryRow(ctx, `SELECT MAX(seq) FROM changes WHERE at < ?`, cutoff).Scan(&s); err != nil || !s.Valid {
		return
	}
	_ = a.DB.WriteTx(ctx, func(tx *db.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM changes WHERE seq <= ?`, s.Int64); err != nil {
			return err
		}
		return setMeta(ctx, tx, "changes_pruned_below", strconv.FormatInt(s.Int64, 10))
	})
}
