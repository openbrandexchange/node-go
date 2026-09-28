package safehttp

import (
	"context"
	"net/netip"
	"testing"
)

func TestIsPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.0.0.1": false, "172.16.5.4": false, "192.168.1.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "::1": false, "fe80::1": false, "fd00::1": false,
		"0.0.0.0": false, "224.0.0.1": false, "::ffff:127.0.0.1": false,
	} {
		if got := IsPublicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: got %v", addr, got)
		}
	}
}

func TestCheckURL(t *testing.T) {
	p := Policy{}
	ctx := context.Background()
	for _, u := range []string{"http://8.8.8.8/x", "https://127.0.0.1/x", "https://localhost/x", "ftp://8.8.8.8/x", "https://user:pw@8.8.8.8/"} {
		if err := p.CheckURL(ctx, u, "https"); err == nil {
			t.Errorf("%s: expected refusal", u)
		}
	}
	if err := p.CheckURL(ctx, "https://8.8.8.8/x", "https"); err != nil {
		t.Errorf("public IP refused: %v", err)
	}
	if err := (Policy{AllowPrivate: true}).CheckURL(ctx, "http://127.0.0.1:9000/x", "https"); err != nil {
		t.Errorf("development switch: %v", err)
	}
}
