package pve

import (
	"context"
	"encoding/json"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"strings"
	"testing"
)

func TestAllocationVolumeProtocolGolden(t *testing.T) {
	t.Parallel()
	const id = "12345678-1234-4234-8234-123456789abc"
	name, err := AllocationVolumeName(9000, "abc", id, "qcow2")
	if err != nil {
		t.Fatal(err)
	}
	const want = "vm-9000-bosh-ba7816bf8f01cfea-alloc-12345678-1234-4234-8234-123456789abc.qcow2"
	if name != want {
		t.Fatalf("protocol name %q, want %q", name, want)
	}
	locator, got, ok := ParseAllocationVolumeID("nfs:9000/" + name)
	if !ok || got != id || locator != "ba7816bf8f01cfea" {
		t.Fatalf("parsed %q %q %t", locator, got, ok)
	}
	for _, bad := range []string{"nfs:" + name, "nfs:9001/" + name, "nfs:9000/../9000/" + name, "nfs:9000/prefix" + name, "nfs:9000/" + name + ".bak", "nfs:9000/" + strings.Replace(name, "4234", "1234", 1)} {
		if _, _, ok := ParseAllocationVolumeID(bad); ok {
			t.Errorf("accepted invalid provenance %q", bad)
		}
	}
	for _, format := range []string{"raw", "qcow2", "vmdk"} {
		if _, err := AllocationVolumeName(9000, "abc", id, format); err != nil {
			t.Fatal(err)
		}
	}
	for _, format := range []string{"", "qcow2/evil", "qcow2,cache=unsafe"} {
		if _, err := AllocationVolumeName(9000, "abc", id, format); err == nil {
			t.Errorf("accepted format %q", format)
		}
	}
}

type allocationScanClient struct {
	Client
	entries nodes.ListStorageContentResponse
}
type allocationScanNodes struct {
	nodes.Service
	entries nodes.ListStorageContentResponse
}

func (c allocationScanClient) Nodes() nodes.Service { return allocationScanNodes{entries: c.entries} }
func (n allocationScanNodes) ListStorageContent(context.Context, string, string, *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	r := n.entries
	return &r, nil
}

func TestAllocationVolumeOrphansReserveVMID(t *testing.T) {
	const id = "12345678-1234-4234-8234-123456789abc"
	a, err := AllocationVolumeName(9000, "namespace", id, "qcow2")
	if err != nil {
		t.Fatal(err)
	}
	b, err := AllocationVolumeName(9001, "namespace", id, "raw")
	if err != nil {
		t.Fatal(err)
	}
	entries := make(nodes.ListStorageContentResponse, 0, 4)
	for _, entry := range []map[string]string{{"volid": "nfs:9000/" + a}, {"filename": b}, {"volid": "nfs:9002/vm-9002-disk-0.qcow2"}, {"volid": "nfs:iso/image.iso"}} {
		raw, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, raw)
	}
	used, err := listStorageVMIDs(t.Context(), allocationScanClient{entries: entries}, "node", "nfs")
	if err != nil {
		t.Fatal(err)
	}
	for _, vmid := range []int{9000, 9001, 9002} {
		if _, ok := used[vmid]; !ok {
			t.Errorf("orphan VMID %d not reserved", vmid)
		}
	}
	if len(used) != 3 {
		t.Fatalf("unexpected reserved VMIDs %v", used)
	}
}

func TestAllocationParkerProvenanceUpdatePreservesIdentity(t *testing.T) {
	const id = "12345678-1234-4234-8234-123456789abc"
	cfg := ParkerConfig{}
	first := buildParkerProvEntry("node", "nfs:9000/disk.qcow2", "scsi0", cfg, ParkContext{StableID: "bpd-1234567890abcdef", AllocationID: id, AllocationNamespace: "abc"})
	desc, _, err := projectParkerProvenance(map[string]any{}, "node", 9000, "bpd-1234567890abcdef", first, cfg)
	if err != nil {
		t.Fatal(err)
	}
	vm := map[string]any{"description": desc, "scsi0": "nfs:9000/disk.qcow2,serial=bpd-1234567890abcdef"}
	updated := buildParkerProvEntry("node", "nfs:9000/disk.qcow2", "scsi0", cfg, ParkContext{StableID: "bpd-1234567890abcdef"})
	desc, _, err = projectParkerProvenance(vm, "node", 9000, "bpd-1234567890abcdef", updated, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, entries, _ := parseParkerSentinel(desc)
	if entries["bpd-1234567890abcdef"].AllocationID != id || entries["bpd-1234567890abcdef"].AllocationNamespace != "abc" {
		t.Fatal("legacy update discarded full allocation provenance")
	}
	updated.AllocationID = "22345678-1234-4234-8234-123456789abc"
	updated.AllocationNamespace = "abc"
	if _, _, err = projectParkerProvenance(vm, "node", 9000, "bpd-1234567890abcdef", updated, cfg); err == nil {
		t.Fatal("accepted identity replacement under existing token")
	}
}
