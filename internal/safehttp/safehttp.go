// Package safehttp makes outbound requests to attacker-influenced URLs
// (scheduled imports, webhooks, SFTP/S3 endpoints) without reaching private
// networks (SSRF protection). The address check runs in the dialer's
// Control hook, i.e. on the address actually connected to, so DNS rebinding
// between check and connect is not possible; redirects are re-checked.
package safehttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrUnsafe marks a refused destination.
var ErrUnsafe = errors.New("unsafe URL")

// Policy decides which destinations are allowed.
type Policy struct {
	// AllowPrivate allows loopback/private/link-local addresses and plain
	// http (OBX_ALLOW_PRIVATE_URLS, development only).
	AllowPrivate bool
}

var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// IsPublicAddr reports whether ip is a public unicast address.
func IsPublicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

func (p Policy) control(_, address string, _ syscall.RawConn) error {
	if p.AllowPrivate {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: cannot parse %s", ErrUnsafe, host)
	}
	if !IsPublicAddr(ip) {
		return fmt.Errorf("%w: %s is a private, loopback or link-local address", ErrUnsafe, ip)
	}
	return nil
}

// Dialer returns a net.Dialer that refuses unsafe addresses.
func (p Policy) Dialer() *net.Dialer {
	return &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Control: p.control}
}

// CheckURL validates the scheme and resolves the host now, so users get an
// immediate `unsafe_url` when saving a configuration. schemes lists the
// allowed schemes (http is added when AllowPrivate).
func (p Policy) CheckURL(ctx context.Context, raw string, schemes ...string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: not an absolute URL", ErrUnsafe)
	}
	ok := false
	for _, s := range schemes {
		if u.Scheme == s {
			ok = true
		}
	}
	if !ok && !(p.AllowPrivate && u.Scheme == "http") {
		return fmt.Errorf("%w: scheme %q is not allowed (use %s)", ErrUnsafe, u.Scheme, strings.Join(schemes, " or "))
	}
	if u.User != nil && u.Scheme != "sftp" {
		return fmt.Errorf("%w: credentials in the URL are not allowed", ErrUnsafe)
	}
	return p.CheckHost(ctx, u.Hostname())
}

// CheckHost resolves host and refuses it when any address is not public.
func (p Policy) CheckHost(ctx context.Context, host string) error {
	if p.AllowPrivate {
		return nil
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !IsPublicAddr(ip) {
			return fmt.Errorf("%w: %s is a private, loopback or link-local address", ErrUnsafe, ip)
		}
		return nil
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return fmt.Errorf("%w: localhost is not allowed", ErrUnsafe)
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(rctx, "ip", host)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve %s", ErrUnsafe, host)
	}
	for _, a := range addrs {
		if !IsPublicAddr(a) {
			return fmt.Errorf("%w: %s resolves to a private, loopback or link-local address", ErrUnsafe, host)
		}
	}
	return nil
}

// Client returns an HTTP client that dials only public addresses.
// maxRedirects 0 disables redirects (webhooks).
func (p Policy) Client(timeout time.Duration, maxRedirects int, schemes ...string) *http.Client {
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           p.Dialer().DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				if maxRedirects == 0 {
					return http.ErrUseLastResponse
				}
				return fmt.Errorf("too many redirects")
			}
			ok := false
			for _, s := range schemes {
				if req.URL.Scheme == s {
					ok = true
				}
			}
			if !ok && !(p.AllowPrivate && req.URL.Scheme == "http") {
				return fmt.Errorf("%w: redirect to %s is not allowed", ErrUnsafe, req.URL.Scheme)
			}
			return nil
		},
	}
}
