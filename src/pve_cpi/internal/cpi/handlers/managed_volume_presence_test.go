package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
)

type presenceNodes struct {
	nodes.Service
	listing *nodes.ListStorageContentResponse
	err     error
}

func (n presenceNodes) ListStorageContent(context.Context, string, string, *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	return n.listing, n.err
}

type presencePVE struct {
	pve.Client
	nodeClient    nodes.Service
	visibilityErr error
}

func (p presencePVE) Nodes() nodes.Service                         { return p.nodeClient }
func (p presencePVE) StorageAuditVisibility(context.Context) error { return p.visibilityErr }

func TestManagedDiskPresenceRequiresCompleteListing(t *testing.T) {
	for _, tc := range []struct {
		name          string
		entries       []string
		nilListing    bool
		nullListing   bool
		listErr       error
		visibilityErr error
		want          bool
		wantErr       bool
	}{
		{name: "empty listing"},
		{name: "exact match", entries: []string{`{"volid":"nfs:20000/disk.qcow2"}`}, want: true},
		{name: "different volume", entries: []string{`{"volid":"nfs:20000/other.qcow2"}`}},
		{name: "permission error", listErr: errors.New("permission denied"), wantErr: true},
		{name: "backend error", listErr: errors.New("backend unavailable"), wantErr: true},
		{name: "nil listing", nilListing: true, wantErr: true},
		{name: "null listing", nullListing: true, wantErr: true},
		{name: "permission-filtered empty listing", visibilityErr: errors.New("VM.Config.Disk unavailable"), wantErr: true},
		{name: "missing identity", entries: []string{`{}`}, wantErr: true},
		{name: "wrong storage", entries: []string{`{"volid":"other:20000/disk.qcow2"}`}, wantErr: true},
		{name: "malformed after matching entry", entries: []string{`{"volid":"nfs:20000/disk.qcow2"}`, `broken`}, wantErr: true},
		{name: "duplicate identity", entries: []string{`{"volid":"nfs:20000/disk.qcow2"}`, `{"volid":"nfs:20000/disk.qcow2"}`}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listing := nodes.ListStorageContentResponse{}
			if tc.nullListing {
				listing = nil
			}
			for _, raw := range tc.entries {
				listing = append(listing, json.RawMessage(raw))
			}
			n := presenceNodes{listing: &listing, err: tc.listErr}
			if tc.nilListing {
				n.listing = nil
			}
			got, err := managedVolumePresent(t.Context(), Deps{PVE: presencePVE{nodeClient: n, visibilityErr: tc.visibilityErr}}, "pve1", "nfs:20000/disk.qcow2")
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("got %v %v", got, err)
			}
		})
	}
}

type absentImagePVE struct{ managedDiskTestPVE }

func (p absentImagePVE) Storage() storage.Service {
	return absentImageStorage{managedDiskTestStorage: managedDiskTestStorage{state: p.state}}
}

type absentImageStorage struct{ managedDiskTestStorage }

func (absentImageStorage) Exists(context.Context, string, string, string) (bool, error) {
	return false, errors.New("volume_size_info failed - no format")
}

func TestManagedDiskAllocationUsesListingForMissingNFSImage(t *testing.T) {
	m, handle, state := managedDiskFixture(t, "spread", false)
	m.deps.PVE = absentImagePVE{managedDiskTestPVE{state: state}}
	if _, err := m.execute(t.Context(), handle); err != nil {
		t.Fatal(err)
	}
	if len(state.created) != 1 {
		t.Fatal("allocation was not submitted exactly once")
	}
}

func TestManagedDiskAbsentLifecycleDoesNotQueryMissingImage(t *testing.T) {
	listing := nodes.ListStorageContentResponse{}
	deps := Deps{PVE: presencePVE{nodeClient: presenceNodes{listing: &listing}}}
	// The fake deliberately has no image GET implementation. A missing image
	// is resolved by the successful listing, before NFS's failing image GET.
	present, err := observeManagedDiskVolume(t.Context(), deps, "pve1", "nfs:20000/disk.qcow2", nil)
	if err != nil || present {
		t.Fatalf("absence not observed: %v %v", present, err)
	}
}
