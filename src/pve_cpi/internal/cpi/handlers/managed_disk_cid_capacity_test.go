package handlers

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestManagedDiskCIDNormalNFSNames(t *testing.T) {
	cfg := &config.CPIConfig{DetachedDiskStrategy: "parked"}
	resolver, err := newLayeredResolver(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := managedDiskOptions(resolver, Deps{Config: cfg}, "qcow2", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 256; i++ {
		hash := sha256.Sum256([]byte(fmt.Sprintf("managed-cid-capacity-%d", i)))
		hash[6] = (hash[6] & 15) | 64
		hash[8] = (hash[8] & 63) | 128
		id := fmt.Sprintf("%x-%x-%x-%x-%x", hash[:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])
		token, err := aj.DiskCorrelationToken(id)
		if err != nil {
			t.Fatal(err)
		}
		name, err := pve.AllocationVolumeName(20000, "storage-cert-pve-cpi-20260909", id, "qcow2")
		if err != nil {
			t.Fatal(err)
		}
		volume := "nfs-persistent-1:20000/" + name
		m := managedDiskRequest{deps: Deps{Config: cfg}, token: token, format: "qcow2", opts: opts,
			plan: &StorageAllocationPlan{Targets: []StoragePlanTarget{{StorageID: "nfs-persistent-1"}}}}
		cid, err := m.cid(volume)
		if err != nil {
			t.Fatalf("fixture %d: %v", i, err)
		}
		bare, meta, err := pve.ParseEncodedDiskCID(cid)
		if err != nil {
			t.Fatal(err)
		}
		if len(cid) > 255 || bare != volume || meta.ID != token || !meta.Anchor || meta.Format != "qcow2" || !reflect.DeepEqual(meta.Opts, opts) {
			t.Fatalf("fixture %d lost disk identity or options: %d %+v", i, len(cid), meta)
		}
		locator, gotID, ok := pve.ParseAllocationVolumeID(bare)
		if !ok || gotID != id || locator != pve.AllocationNamespaceLocator("storage-cert-pve-cpi-20260909") {
			t.Fatal("allocation provenance lost")
		}
	}
}
