package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

func TestConfigure_OrphanISOWarnOnlyWhenListed(t *testing.T) {
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
		wantWarn bool
	}{
		// Directory storage answers a DELETE of a missing file with success,
		// so the delete reports "existed" even though nothing was there.
		{name: "dir storage volume missing", listing: empty},
		{name: "orphan listed", listing: present, wantWarn: true},
		{name: "listing fails", listing: func() (*sdknodes.ListStorageContentResponse, error) {
			return nil, errors.New("500 no format")
		}},
		{name: "other iso listed, not ours", listing: func() (*sdknodes.ListStorageContentResponse, error) {
			l := sdknodes.ListStorageContentResponse{
				json.RawMessage(`{"volid":"local:iso/other.iso"}`),
				json.RawMessage(`{}`),
			}
			return &l, nil
		}},
		{name: "malformed item", listing: func() (*sdknodes.ListStorageContentResponse, error) {
			l := sdknodes.ListStorageContentResponse{json.RawMessage(`[1]`)}
			return &l, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage := &fakeStorageSvc{} // default delete reports existed=true, as directory storage does
			nodes := &fakeNodesSvc{listContentFn: func(string, string) (*sdknodes.ListStorageContentResponse, error) {
				return tc.listing()
			}}
			obsLogger, obs := log.NewObservedLogger(log.LevelDebug)
			base := &fakePVEClient{storageSvc: storage, nodesSvc: nodes}
			a := NewConfigDrive(base, "local", nil, obsLogger)
			if err := a.Configure(t.Context(), "pve1", 200, baseISOConfig()); err != nil {
				t.Fatalf("Configure: %v", err)
			}
			if len(storage.deleteVolumeIfExistsCalls) == 0 {
				t.Fatal("delete was skipped; deletion must stay unconditional")
			}
			if len(nodes.listContentCalls) != 1 {
				t.Fatalf("listings = %d, want exactly 1", len(nodes.listContentCalls))
			}
			got := nodes.listContentCalls[0]
			if got.node != "pve1" || got.storage != "local" {
				t.Fatalf("listing on %s/%s, want pve1/local", got.node, got.storage)
			}
			if got.params == nil || got.params.Content == nil || *got.params.Content != "iso" ||
				got.params.Vmid != nil {
				t.Fatalf("listing params = %+v, want content=iso only", got.params)
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
