package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/configdrive"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

type managedISONodes struct {
	nodes.Service
	listing *nodes.ListStorageContentResponse
	err     error
	calls   int
}

func (n *managedISONodes) ListStorageContent(_ context.Context, node, pool string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	n.calls++
	if node != "node" || pool != "iso" || params != nil {
		return nil, errors.New("unexpected or filtered content listing")
	}
	return n.listing, n.err
}

type managedISOListingClient struct {
	managedISOClient
	n             nodes.Service
	visibilityErr error
}

func (c managedISOListingClient) Nodes() nodes.Service { return c.n }
func (c managedISOListingClient) StorageAuditVisibility(context.Context) error {
	return c.visibilityErr
}

func TestManagedISOCollisionNeverDeletes(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		entries                []string
		null                   bool
		listErr, visibilityErr error
		wantErr                bool
	}{
		{name: "absent NFS ISO"},
		{name: "collision", entries: []string{`{"volid":"iso:iso/allocation.iso"}`}, wantErr: true},
		{name: "listing error", listErr: errors.New("unavailable"), wantErr: true},
		{name: "permission-filtered absence", visibilityErr: errors.New("denied"), wantErr: true},
		{name: "null listing", null: true, wantErr: true},
		{name: "malformed listing", entries: []string{`{}`}, wantErr: true},
		{name: "wrong storage", entries: []string{`{"volid":"other:iso/allocation.iso"}`}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listing := make(nodes.ListStorageContentResponse, 0, len(tc.entries))
			if tc.null {
				listing = nil
			}
			for _, entry := range tc.entries {
				listing = append(listing, json.RawMessage(entry))
			}
			s := &managedISOStorage{err: errors.New("volume_size_info failed - no format")}
			n := &managedISONodes{listing: &listing, err: tc.listErr}
			a := NewConfigDrive(managedISOListingClient{managedISOClient: managedISOClient{s: s}, n: n, visibilityErr: tc.visibilityErr}, "iso", nil, log.NewNopLogger())
			a.managedAllocationBytes = configdrive.AllocationBytes()
			_, err := a.removeISOIfExists(t.Context(), "node", "allocation.iso")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if s.calls != 0 || n.calls != 1 {
				t.Fatal("managed collision probe did not use authoritative content membership")
			}
			// Any attempted deletion calls the nil embedded service and panics.
		})
	}
}

func TestManagedISOResumeRequiresExactObservedArtifact(t *testing.T) {
	for _, present := range []bool{true, false} {
		t.Run(map[bool]string{true: "present", false: "absent"}[present], func(t *testing.T) {
			listing := nodes.ListStorageContentResponse{}
			if present {
				listing = append(listing, json.RawMessage(`{"volid":"iso:iso/vm-200-config.iso"}`))
			}
			updates := &fakeNodesSvc{}
			n := &managedISONodes{Service: updates, listing: &listing}
			s := &managedISOStorage{err: errors.New("direct image GET unavailable")}
			a := NewConfigDrive(managedISOListingClient{managedISOClient: managedISOClient{s: s}, n: n}, "iso", nil, log.NewNopLogger())
			a.managedAllocationBytes = configdrive.AllocationBytes()
			a.managedExistingISO = "iso:iso/vm-200-config.iso"
			payloadChecked := false
			a.managedPayloadCheck = func(payload []byte) error { payloadChecked = json.Valid(payload); return nil }
			err := a.Configure(t.Context(), "node", 200, baseISOConfig())
			if (err == nil) != present || !payloadChecked || s.calls != 0 || n.calls != 1 {
				t.Fatalf("unexpected ISO reuse result: %v", err)
			}
			if present {
				if len(updates.updateConfigCalls) != 1 || updates.updateConfigCalls[0].params.Scsi[configDriveSlotIndex] != "iso:iso/vm-200-config.iso,media=cdrom" {
					t.Fatal("proven ISO not attached exactly once")
				}
			} else if len(updates.updateConfigCalls) != 0 {
				t.Fatal("missing ISO was attached")
			}
			// Upload and delete have no fake implementation and must not occur.
		})
	}
}
