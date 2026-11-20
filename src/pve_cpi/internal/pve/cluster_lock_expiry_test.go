package pve_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestClusterLockExpiry(t *testing.T) {
	exp := time.Unix(1_790_000_000, 0)
	got, ok := pve.ClusterLockExpiry("owner=ha-rebalance/dir-1 exp=" + strconv.FormatInt(exp.Unix(), 10))
	if !ok || !got.Equal(exp) {
		t.Fatalf("expiry %v %v, want %v", got, ok, exp)
	}
	for _, c := range []string{"", "owner=x", "owner=x exp=soon"} {
		if _, ok := pve.ClusterLockExpiry(c); ok {
			t.Fatalf("comment %q gave an expiry", c)
		}
	}
}

func TestIsClusterLockPool(t *testing.T) {
	for pool, want := range map[string]bool{
		"bosh-lock-ha-rebalance": true, "bosh-lock-vm-100": true, "bosh-parked": false, "lock-bosh": false, "": false,
	} {
		if got := pve.IsClusterLockPool(pool); got != want {
			t.Fatalf("IsClusterLockPool(%q) = %v, want %v", pool, got, want)
		}
	}
}
