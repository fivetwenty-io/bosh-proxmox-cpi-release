package netguard

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
)

type fakeResolver struct {
	ips []string
	err error
}

func (f fakeResolver) LookupHost(_ context.Context, _ string) ([]string, error) {
	return f.ips, f.err
}

func testGuard(res Resolver) DialGuard {
	return DialGuard{
		Resolver:  res,
		ErrPrefix: "testguard",
		Hint:      "set the-override to allow",
	}
}

// TestDialContext_BlocksPrivateIPLiteral verifies loopback and RFC1918
// literals are rejected before any dial, with the subsystem prefix and the
// operator hint in the message.
func TestDialContext_BlocksPrivateIPLiteral(t *testing.T) {
	t.Parallel()
	dial := testGuard(nil).DialContext()
	for _, addr := range []string{"127.0.0.1:443", "10.1.2.3:443", "192.168.1.50:8006", "[::1]:443", "169.254.169.254:80"} {
		_, err := dial(context.Background(), "tcp", addr)
		if err == nil {
			t.Errorf("dial %s: expected block, got nil error", addr)
			continue
		}
		if !strings.Contains(err.Error(), "testguard:") || !strings.Contains(err.Error(), "the-override") {
			t.Errorf("dial %s: error must carry prefix and hint: %v", addr, err)
		}
	}
}

// TestDialContext_BlocksHostnameResolvingPrivate verifies the per-dial DNS
// check rejects a hostname that resolves (or has flipped, DNS-rebinding
// style) to a private address.
func TestDialContext_BlocksHostnameResolvingPrivate(t *testing.T) {
	t.Parallel()
	dial := testGuard(fakeResolver{ips: []string{"93.184.216.34", "10.0.0.5"}}).DialContext()
	_, err := dial(context.Background(), "tcp", "mirror.example.com:443")
	if err == nil {
		t.Fatal("expected block when any resolved IP is private, got nil")
	}
	if !strings.Contains(err.Error(), "10.0.0.5") {
		t.Errorf("error should name the offending resolved IP: %v", err)
	}
}

// TestDialContext_ResolutionFailureFailsClosed verifies a DNS failure blocks
// the dial rather than falling through to an unchecked connection.
func TestDialContext_ResolutionFailureFailsClosed(t *testing.T) {
	t.Parallel()
	dial := testGuard(fakeResolver{err: fmt.Errorf("NXDOMAIN")}).DialContext()
	if _, err := dial(context.Background(), "tcp", "gone.example.com:443"); err == nil {
		t.Fatal("expected error on resolver failure, got nil")
	}
}

// TestIsPrivateOrSpecial spot-checks the classification both ways.
func TestIsPrivateOrSpecial(t *testing.T) {
	t.Parallel()
	for ip, want := range map[string]bool{
		"127.0.0.1":       true,
		"10.0.0.1":        true,
		"172.16.0.1":      true,
		"192.168.0.1":     true,
		"169.254.169.254": true,
		"0.0.0.0":         true,
		"::1":             true,
		"fc00::1":         true,
		"93.184.216.34":   false,
		"2606:2800::1":    false,
	} {
		if got := IsPrivateOrSpecial(net.ParseIP(ip)); got != want {
			t.Errorf("IsPrivateOrSpecial(%s) = %v, want %v", ip, got, want)
		}
	}
}
