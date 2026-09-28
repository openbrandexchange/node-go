package safehttp

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"
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

// The development switch opens private networks, never cloud metadata.
func TestMetadataAlwaysRefused(t *testing.T) {
	dev := Policy{AllowPrivate: true}
	ctx := context.Background()
	for _, u := range []string{
		"http://169.254.169.254/latest/meta-data/", "http://[::ffff:169.254.169.254]/", "http://[64:ff9b::a9fe:a9fe]/",
		"http://[fe80::1]/", "http://100.100.100.200/", "http://[fd00:ec2::254]/", "http://metadata.google.internal/",
		"http://METADATA/", "http://instance-data.ec2.internal./",
	} {
		if err := dev.CheckURL(ctx, u, "https"); err == nil {
			t.Errorf("%s: allowed in development mode", u)
		}
	}
	for _, u := range []string{"http://10.0.0.5/", "http://[fd12::1]/", "http://100.64.1.1/"} {
		if err := dev.CheckURL(ctx, u, "https"); err != nil {
			t.Errorf("%s: private address refused in development mode: %v", u, err)
		}
	}
}

// The dialer checks the address actually connected to (a name that resolves
// to a metadata address after the check is refused at connect time).
func TestDialerPinsCheckedAddress(t *testing.T) {
	for _, pol := range []Policy{{}, {AllowPrivate: true}} {
		for _, addr := range []string{"169.254.169.254:80", "[fd00:ec2::254]:80", "100.100.100.200:80"} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := pol.Dialer().DialContext(ctx, "tcp", addr)
			cancel()
			if err == nil || !errors.Is(err, ErrUnsafe) {
				t.Errorf("private=%v %s: want ErrUnsafe, got %v", pol.AllowPrivate, addr, err)
			}
		}
	}
	if _, err := (Policy{}).Dialer().DialContext(context.Background(), "tcp", "127.0.0.1:1"); err == nil || !errors.Is(err, ErrUnsafe) {
		t.Errorf("loopback: want ErrUnsafe, got %v", err)
	}
}
