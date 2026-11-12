package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// visibleClient adds the audit-visibility proof an absent-volume observation needs.
type visibleClient struct {
	*fakePVEClient
	visErr error
}

func (c visibleClient) StorageAuditVisibility(context.Context) error { return c.visErr }

func TestConfigure_OrphanISOWarnOnlyWithProof(t *testing.T) {
	const volid = "local:iso/vm-200-config.iso"
	present := func() (*sdknodes.ListStorageContentResponse, error) {
		l := sdknodes.ListStorageContentResponse{json.RawMessage(`{"volid":"` + volid + `"}`)}
		return &l, nil
	}
	empty := func() (*sdknodes.ListStorageContentResponse, error) {
		l := sdknodes.ListStorageContentResponse{}
		return &l, nil
	}
	for _, tc := range []struct {
		name     string
		listing  func() (*sdknodes.ListStorageContentResponse, error)
		visErr   error
		wantWarn bool
	}{
		// Directory storage answers a DELETE of a missing file with success,
		// so the delete reports "existed" even though nothing was there.
		{name: "dir storage volume missing", listing: empty},
		{name: "orphan listed", listing: present, wantWarn: true},
		{name: "listing fails", listing: func() (*sdknodes.ListStorageContentResponse, error) {
			return nil, errors.New("500 no format")
		}},
		{name: "absence unproven", listing: empty, visErr: errors.New("denied")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage := &fakeStorageSvc{} // default delete reports existed=true, as directory storage does
			nodes := &fakeNodesSvc{listContentFn: func(string, string) (*sdknodes.ListStorageContentResponse, error) {
				return tc.listing()
			}}
			obsLogger, obs := log.NewObservedLogger(log.LevelDebug)
			base := &fakePVEClient{storageSvc: storage, nodesSvc: nodes}
			client := visibleClient{fakePVEClient: base, visErr: tc.visErr}
			a := NewConfigDrive(client, "local", nil, obsLogger)
			if err := a.Configure(t.Context(), "pve1", 200, baseISOConfig()); err != nil {
				t.Fatalf("Configure: %v", err)
			}
			if len(storage.deleteVolumeIfExistsCalls) == 0 {
				t.Fatal("delete was skipped; deletion must stay unconditional")
			}
			warned := false
			for _, e := range obs.All() {
				if e.Level == log.LevelWarn && strings.Contains(e.Message, "removed stale orphan iso") {
					warned = true
				}
			}
			if warned != tc.wantWarn {
				t.Fatalf("warned=%v want %v", warned, tc.wantWarn)
			}
		})
	}
}
