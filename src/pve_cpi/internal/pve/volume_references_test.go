package pve_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// volumeReferenceClient lists VMs 701 and 702 on pve-01 and 703 on pve-02,
// answering each config read through configFn.
func volumeReferenceClient(configFn func(node string, vmid int) (map[string]any, error)) *diskClusterClient {
	return &diskClusterClient{
		clusterSvc: &diskFakeCluster{
			listFn: func(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return diskClusterResp(
					map[string]any{"vmid": int64(701), "node": "pve-01"},
					map[string]any{"vmid": int64(702), "node": "pve-01"},
					map[string]any{"vmid": int64(703), "node": "pve-02"},
				), nil
			},
		},
		qemuSvc: &diskFakeQEMUFn{fn: configFn},
	}
}

func TestFindVolumeReferencesReadsActiveAndUnusedSlots(t *testing.T) {
	t.Parallel()
	volid := "a:701/vm-701-ephemeral-0.raw"
	c := volumeReferenceClient(func(_ string, vmid int) (map[string]any, error) {
		switch vmid {
		case 701:
			return map[string]any{"scsi0": "a:701/vm-701-disk-0.raw,size=8G", "unused0": volid, "unused1": volid + "x"}, nil
		case 702:
			return map[string]any{"scsi2": volid + ",serial=bpd-0123456789abcdef,size=5G", "ide2": "none,media=cdrom"}, nil
		default:
			return map[string]any{"unused3": volid}, nil
		}
	})
	refs, err := pve.FindVolumeReferences(context.Background(), c, volid)
	if err != nil {
		t.Fatal(err)
	}
	want := []pve.VolumeReference{{VMID: 701, Node: "pve-01", Slot: "unused0"}, {VMID: 702, Node: "pve-01", Slot: "scsi2"}, {VMID: 703, Node: "pve-02", Slot: "unused3"}}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("references: got %+v, want %+v", refs, want)
	}
}

func TestFindVolumeReferencesSkipsDeletedGuestsAndFailsOnOtherReadErrors(t *testing.T) {
	t.Parallel()
	volid := "a:vm-701-ephemeral-0"
	deleted := volumeReferenceClient(func(_ string, vmid int) (map[string]any, error) {
		if vmid == 702 {
			return nil, sdkerrors.ErrNotFound
		}
		return map[string]any{}, nil
	})
	refs, err := pve.FindVolumeReferences(context.Background(), deleted, volid)
	if err != nil || len(refs) != 0 {
		t.Fatalf("a guest deleted after the listing must be skipped: refs=%+v err=%v", refs, err)
	}
	transient := volumeReferenceClient(func(_ string, vmid int) (map[string]any, error) {
		if vmid == 703 {
			return nil, errors.New("pveproxy backend gone (code: 596)")
		}
		return map[string]any{"unused0": volid}, nil
	})
	if _, err := pve.FindVolumeReferences(context.Background(), transient, volid); err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("an unread config must fail the proof retriable, got %v", err)
	}
}

// TestFindVolumeReferencesFailsWhenANodeCannotBeListed pins the strict
// listing. A member that /cluster/status reports offline would be excluded by
// the tolerant listing, but its guests still reference volumes in config, so
// the reference proof refuses instead of answering for a partial fleet.
func TestFindVolumeReferencesFailsWhenANodeCannotBeListed(t *testing.T) {
	t.Parallel()
	volid := "a:vm-701-ephemeral-0"
	fc := &diskFakeCluster{
		listFn: func(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return diskClusterResp(
				map[string]any{"vmid": int64(701), "node": "pve-01"},
				map[string]any{"vmid": int64(703), "node": "pve-02"},
			), nil
		},
		listStatusFn: func(context.Context) (*cluster.ListStatusResponse, error) {
			return diskQuorateStatusWithOffline("pve-02"), nil
		},
	}
	c := &diskClusterClient{
		clusterSvc: fc,
		qemuSvc: &diskFakeQEMUFn{fn: func(string, int) (map[string]any, error) {
			return map[string]any{"unused0": volid}, nil
		}},
		nodesOverride: &diskFakeNodesFromCluster{f: fc, failNode: "pve-02", failErr: errors.New("pve-02: connection refused (member is powered off)")},
	}
	refs, err := pve.FindVolumeReferences(context.Background(), c, volid)
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("an unlisted member must fail the proof retriable, got refs=%+v err=%v", refs, err)
	}
}
