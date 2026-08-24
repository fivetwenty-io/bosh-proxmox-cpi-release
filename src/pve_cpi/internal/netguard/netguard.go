// Package netguard provides a per-connection SSRF dial guard shared by every
// CPI HTTP client that talks to operator-supplied endpoints (the LB registrar,
// the stemcell fetcher). The guard re-resolves the target host on every
// connection attempt and rejects private/loopback/link-local/unspecified
// addresses before dialing, which closes the DNS-rebinding (TOCTOU) window: a
// hostname that flips from public to private between construction and request
// time is still caught, and redirect targets are covered automatically because
// every redirected request dials through the same guarded dialer.
package netguard

import (
	"context"
	"fmt"
	"net"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// IsPrivateOrSpecial returns true when ip is a non-globally-routable address.
func IsPrivateOrSpecial(ip net.IP) bool {
	return ip.IsPrivate() ||
		ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsUnspecified()
}

// Resolver is the interface used to inject a test DNS resolver;
// net.DefaultResolver satisfies it.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// DialGuard builds guarded DialContext functions.
type DialGuard struct {
	// Base performs the actual dial once the target passes the guard. Nil
	// means a zero net.Dialer.
	Base *net.Dialer
	// Resolver resolves hostnames; nil means net.DefaultResolver.
	Resolver Resolver
	// ErrPrefix names the subsystem in guard errors (e.g. "lb",
	// "stemcell_fetch") so an operator can tell which client refused.
	ErrPrefix string
	// Hint tells the operator how to allow a legitimately-private endpoint
	// (e.g. "set allow_private_ip=true to override"). Appended to every
	// blocked-dial error.
	Hint string
}

// DialContext returns a DialContext function enforcing the guard. Every
// resolved address must be globally routable; the dial then targets the first
// validated IP explicitly so the OS cannot re-resolve to something else.
func (g DialGuard) DialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	base := g.Base
	if base == nil {
		base = &net.Dialer{}
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("%s: dial: parse addr %q: %w", g.ErrPrefix, addr, err)
		}

		// If addr is already an IP literal, classify directly — no DNS needed.
		if ip := net.ParseIP(host); ip != nil {
			if IsPrivateOrSpecial(ip) {
				return nil, cpierrors.Cloud(
					"%s: dial blocked: %s is a private/loopback address; %s",
					g.ErrPrefix, ip.String(), g.Hint)
			}
			return base.DialContext(ctx, network, addr)
		}

		// Hostname: resolve and check every IP.
		var resolver Resolver = net.DefaultResolver
		if g.Resolver != nil {
			resolver = g.Resolver
		}
		ips, lookupErr := resolver.LookupHost(ctx, host)
		if lookupErr != nil {
			return nil, cpierrors.Cloud("%s: dial: DNS lookup for %q failed: %s", g.ErrPrefix, host, lookupErr.Error())
		}
		if len(ips) == 0 {
			return nil, cpierrors.Cloud("%s: dial: DNS lookup for %q returned no addresses", g.ErrPrefix, host)
		}
		for _, resolved := range ips {
			ip := net.ParseIP(resolved)
			if ip == nil {
				continue
			}
			if IsPrivateOrSpecial(ip) {
				return nil, cpierrors.Cloud(
					"%s: dial blocked: %s (resolved from %q) is a private/loopback address; %s",
					g.ErrPrefix, ip.String(), host, g.Hint)
			}
		}

		// All resolved IPs are public. Dial the first one explicitly so we use
		// the validated address rather than letting the OS re-resolve.
		dialAddr := net.JoinHostPort(ips[0], port)
		return base.DialContext(ctx, network, dialAddr)
	}
}
