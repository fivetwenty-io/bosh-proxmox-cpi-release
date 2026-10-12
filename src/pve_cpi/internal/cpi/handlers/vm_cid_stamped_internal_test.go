package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestHandleDeleteVMStampedCIDFindsManagedRecord checks that delete_vm matches
// a stamped CID to the journal record, which stores the bare VMID. Without the
// match the lookup finds no record and refuses on the allocation marker.
func TestHandleDeleteVMStampedCIDFindsManagedRecord(t *testing.T) {
	deps, j, c, record := deleteManagedFixture(t)
	deps.Config.StorageSets = nil
	deps.Config.EphemeralStorageSet = "removed"

	arg, err := json.Marshal("123-0a1b2c3d-4e5f6a7b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := HandleDeleteVM(deps).Handle(t.Context(), []json.RawMessage{arg}, jsonrpc.Context{}); err != nil {
		t.Fatalf("delete_vm: %v", err)
	}
	after, err := j.Inspect(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != aj.Deleted || c.destroyCount != 1 {
		t.Fatalf("state %s destroy %d, want deleted and one destroy", after.State, c.destroyCount)
	}
}

// A managed create_disk that carries tags and a stamped vm_cid hint must
// accept the metadata write against the bare VMID the mutation names.
func TestManagedDiskApplyTagsAcceptsStampedHint(t *testing.T) {
	m, h, state := managedDiskFixture(t, "spread", false)
	if state.configs == nil {
		state.configs = map[int]map[string]any{}
	}
	state.configs[101] = map[string]any{"name": "w101", "digest": "1"}
	m.hint = "101-0a1b2c3d-4e5f6a7b"
	m.tags = map[string]string{"team": "alpha"}
	if _, err := m.execute(t.Context(), h); err != nil {
		t.Fatalf("stamped hint rejected: %v", err)
	}
	if got := fmt.Sprint(state.configs[101]["tags"]); !strings.Contains(got, "alpha") {
		t.Fatalf("tags were not written to VM 101: %v", state.configs[101])
	}
}

// The legacy create_disk tag path takes the stamped hint, resolves the bare
// VMID, and writes the tags to that VM.
func TestApplyCreateDiskTagsAcceptsStampedHint(t *testing.T) {
	m, _, state := managedDiskFixture(t, "spread", false)
	if state.configs == nil {
		state.configs = map[int]map[string]any{}
	}
	state.configs[101] = map[string]any{"name": "w101", "digest": "1"}
	applyCreateDiskTags(t.Context(), m.deps, "n1", "101-0a1b2c3d-4e5f6a7b", "disk", map[string]string{"team": "alpha"})
	if got := fmt.Sprint(state.configs[101]["tags"]); !strings.Contains(got, "alpha") {
		t.Fatalf("tags were not written to VM 101: %v", state.configs[101])
	}
}

// The park-after-detach writer records the bare VMID as the disk's source VM.
func TestParkAfterDetachRecordsBareSourceVMID(t *testing.T) {
	var got pve.ParkContext
	called := false
	t.Cleanup(setParkDiskForTest(func(_ context.Context, _ pve.Client, _ *log.Logger, _, _ string, _ pve.ParkerConfig, pctx pve.ParkContext) error {
		called, got = true, pctx
		return nil
	}))
	deps := Deps{Config: parkerCfgTestConfig(), Logger: log.NewNopLogger()}
	if err := parkAfterDetach(t.Context(), deps, 101, "pvd-x", "local-lvm:vm-9001-disk-0", "n1", nil); err != nil {
		t.Fatal(err)
	}
	if !called || got.SourceVMCID != "101" {
		t.Fatalf("park called=%v SourceVMCID=%q, want 101", called, got.SourceVMCID)
	}
}
