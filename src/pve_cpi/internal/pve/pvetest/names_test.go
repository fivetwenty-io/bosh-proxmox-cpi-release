package pvetest_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// TestInstallNameServerResolvesToLoopbackAndRefusesWhatItIsTold covers the
// three answers of the stub: an address for a name, no such host for a refused
// name, and an empty answer for a type it holds no record of. The resolver is
// process-wide, so the test is serial.
func TestInstallNameServerResolvesToLoopbackAndRefusesWhatItIsTold(t *testing.T) {
	before := net.DefaultResolver
	t.Run("installed", func(t *testing.T) {
		pvetest.InstallNameServer(t, func(name string) bool { return strings.HasPrefix(name, "down.") })
		ctx := context.Background()

		addrs, err := net.DefaultResolver.LookupHost(ctx, "pve2.dc1.example.com")
		if err != nil || len(addrs) != 1 || addrs[0] != "127.0.0.1" {
			t.Fatalf("LookupHost(pve2) = %v, %v, want 127.0.0.1", addrs, err)
		}
		_, err = net.DefaultResolver.LookupHost(ctx, "down.dc1.example.com")
		var dnsErr *net.DNSError
		if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
			t.Fatalf("LookupHost(down) error = %v, want no such host", err)
		}
		aaaa, err := net.DefaultResolver.LookupIP(ctx, "ip6", "pve2.dc1.example.com")
		if err == nil || len(aaaa) != 0 {
			t.Fatalf("LookupIP ip6 = %v, %v, want no address", aaaa, err)
		}
	})
	if net.DefaultResolver != before {
		t.Fatal("the resolver was not restored when the test ended")
	}
}

func TestInstallNameServerWithoutRefusalsResolvesEveryName(t *testing.T) {
	pvetest.InstallNameServer(t, nil)
	addrs, err := net.DefaultResolver.LookupHost(context.Background(), "down.dc1.example.com")
	if err != nil || len(addrs) != 1 || addrs[0] != "127.0.0.1" {
		t.Fatalf("LookupHost = %v, %v, want 127.0.0.1", addrs, err)
	}
}
