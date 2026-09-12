package handlers

import (
	"math"
	"net"
	"strings"
	"testing"
)

func TestManagedVMNICIdentityAndSizeBounds(t *testing.T) {
	a := managedVMNICValue("00000000-0000-4000-8000-000000000001", 0, "virtio,bridge=vmbr0,tag=3")
	if a != managedVMNICValue("00000000-0000-4000-8000-000000000001", 0, "virtio,bridge=vmbr0,tag=3") {
		t.Fatal("unstable identity")
	}
	if a == managedVMNICValue("00000000-0000-4000-8000-000000000002", 0, "virtio,bridge=vmbr0,tag=3") || a == managedVMNICValue("00000000-0000-4000-8000-000000000001", 1, "virtio,bridge=vmbr0,tag=3") {
		t.Fatal("identity collision across generation/device")
	}
	field, options, _ := strings.Cut(a, ",")
	_, mac, _ := strings.Cut(field, "=")
	parsed, err := net.ParseMAC(mac)
	if err != nil || parsed[0]&3 != 2 || options != "bridge=vmbr0,tag=3" {
		t.Fatalf("invalid NIC %s", a)
	}
	for _, size := range []uint64{0, 1, (1 << 30) + 1, math.MaxUint64} {
		if _, err := managedVMGiB(size); err == nil {
			t.Fatal("invalid size accepted")
		}
	}
	if got, err := managedVMGiB(8 << 30); err != nil || got != 8 {
		t.Fatal("valid size rejected")
	}
}
