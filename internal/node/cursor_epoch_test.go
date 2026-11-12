package node

import (
	"context"
	"net/url"
	"testing"

	"github.com/openbrandexchange/node-go/internal/db"
)

// expectCursorExpired asserts that /changes with cursor answers 410
// cursor_expired.
func expectCursorExpired(t *testing.T, c *client, cursor, what string) {
	t.Helper()
	r := c.do("GET", "/obx/v1/changes?limit=5&cursor="+url.QueryEscape(cursor), nil)
	if r.Status != 410 || r.JSON(t)["obx_error"] != "cursor_expired" {
		t.Fatalf("%s: want 410 cursor_expired, got %d %s", what, r.Status, truncateForLog(r.Body))
	}
}

// TestCursorsExpireOnSpecUpgrade (protocol.md §6.4, P-FEED-7): a node at
// schema 3 (spec 0.2) issues a live and a snapshot cursor; migration 4
// (spec 0.3) then runs, and both cursors answer 410 cursor_expired, while a
// feed started without a cursor works and its cursors keep working.
func TestCursorsExpireOnSpecUpgrade(t *testing.T) {
	k := newKestrelNode(t)
	ctx := context.Background()
	// Back to the state of a 0.2 node: migration 4 not applied, no epoch.
	if err := k.app.DB.WriteTx(ctx, func(tx *db.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = 4`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM meta WHERE key IN ('cursor_epoch', 'upgrade_terms')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, live, _ := drainChanges(t, k.all, "", 500)
	first := k.all.must(200, "GET", "/obx/v1/changes?limit=2", nil)
	snapshot := first["next_cursor"].(string)
	if !first["has_more"].(bool) || first["phase"] != "snapshot" {
		t.Fatalf("want a snapshot cursor mid-way: %v", first)
	}
	// The 0.2 node's cursors work before the upgrade.
	k.all.must(200, "GET", "/obx/v1/changes?cursor="+url.QueryEscape(live), nil)

	// Upgrade: migration 4 and the one-time rewrite, as on start.
	if err := k.app.DB.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := k.app.upgradeTerms(ctx); err != nil {
		t.Fatal(err)
	}
	expectCursorExpired(t, k.all, live, "live cursor from before the upgrade")
	expectCursorExpired(t, k.all, snapshot, "snapshot cursor from before the upgrade")

	// Restarting without a cursor works, and so do the new cursors.
	all, fresh, _ := drainChanges(t, k.all, "", 3)
	if len(all) == 0 {
		t.Fatal("the restarted feed is empty")
	}
	k.all.must(200, "GET", "/obx/v1/changes?cursor="+url.QueryEscape(fresh), nil)
	mid := k.all.must(200, "GET", "/obx/v1/changes?limit=2", nil)["next_cursor"].(string)
	k.all.must(200, "GET", "/obx/v1/changes?limit=2&cursor="+url.QueryEscape(mid), nil)
}

// TestSimulateSpecUpgrade is the operator action simulate_spec_upgrade:
// every cursor issued so far expires, new ones work.
func TestSimulateSpecUpgrade(t *testing.T) {
	k := newKestrelNode(t)
	ctx := context.Background()
	_, live, _ := drainChanges(t, k.all, "", 500)
	if err := k.app.SimulateSpecUpgrade(ctx); err != nil {
		t.Fatal(err)
	}
	if e, _ := k.app.cursorEpoch(ctx, k.app.DB); e != 2 {
		t.Fatalf("cursor epoch after a fresh install and one simulated upgrade: %d, want 2", e)
	}
	expectCursorExpired(t, k.all, live, "live cursor from before the simulated upgrade")
	_, fresh, _ := drainChanges(t, k.all, "", 500)
	k.all.must(200, "GET", "/obx/v1/changes?cursor="+url.QueryEscape(fresh), nil)
}
