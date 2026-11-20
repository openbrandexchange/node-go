package node

import (
	"context"
	"testing"
)

// TestDevSetup: "node-go dev" completes the setup through the wizard's API
// once; the account can log in, and a second call changes nothing.
func TestDevSetup(t *testing.T) {
	n := newTestNode(t)
	ctx := context.Background()
	acc := DevAccount{SetupCode: testSetupCode, Company: "MAISON NOOR Group", Brand: "MAISON NOOR", Name: "Demo Admin", Email: adminEmail, Password: adminPassword}
	bad := acc
	bad.SetupCode = "WRONG-CODE"
	if err := n.app.DevSetup(ctx, bad); err == nil || n.app.SetupDone(ctx) {
		t.Fatal("a wrong setup code must fail")
	}
	if err := n.app.DevSetup(ctx, acc); err != nil || !n.app.SetupDone(ctx) {
		t.Fatalf("dev setup: %v", err)
	}
	if err := n.app.DevSetup(ctx, acc); err != nil {
		t.Fatalf("second call: %v", err)
	}
	c := n.client()
	c.login(adminEmail, adminPassword)
	if b := c.must(200, "GET", "/admin/api/catalog/brands", nil)["items"].([]any); len(b) != 1 {
		t.Fatalf("brands %v", b)
	}
}
