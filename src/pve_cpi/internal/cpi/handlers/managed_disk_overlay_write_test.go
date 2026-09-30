package handlers

import (
	"encoding/json"
	"errors"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// overlayWriteDescription rebuilds a holder description from its current one.
// It records a drive-option overlay entry and then lets edit change anything
// else, so each case starts from the description the guard is about to read.
func overlayWriteDescription(t *testing.T, current string, edit func(nonBOSH string, raw map[string]json.RawMessage) string) string {
	t.Helper()
	nonBOSH, raw := pve.ParseSentinel(current)
	raw[pve.DiskOptOverlaysSentinelKey] = json.RawMessage(`{"disk-token":{"discard":"on","iothread":"1","ssd":"1"}}`)
	if edit != nil {
		nonBOSH = edit(nonBOSH, raw)
	}
	description, err := pve.RenderSentinel(nonBOSH, raw)
	if err != nil {
		t.Fatal(err)
	}
	return description
}

// overlayWriteCase is one guarded description write onto the disk's holder.
type overlayWriteCase struct {
	name       string
	edit       func(nonBOSH string, raw map[string]json.RawMessage) string
	protection *bool
	digest     bool
	admitted   bool
	want       aj.State
}

// TestManagedGuardExemptsOnlyTheOverlayNote drives guarded description writes
// onto the disk's holder and then lets a lock timeout finish the lifecycle. A
// write that changes nothing but the bosh_disk_opt_overlays note is journaled
// and observed like any other write, yet it leaves the disk untouched, so the
// timeout still returns the allocation. A write that changes the text outside
// the sentinel, another sentinel key, or any field beside the description is a
// disk mutation, and the same timeout leaves the allocation for reconciliation.
func TestManagedGuardExemptsOnlyTheOverlayNote(t *testing.T) {
	protect := true
	for _, tc := range []overlayWriteCase{
		{name: "overlay note only", want: aj.ReadyToReturn},
		{name: "overlay note with the caller's digest", digest: true, want: aj.ReadyToReturn},
		{name: "non-BOSH text changes", admitted: true, want: aj.ReconciliationRequired, edit: func(nonBOSH string, _ map[string]json.RawMessage) string {
			return nonBOSH + " operator note"
		}},
		{name: "provenance key changes", admitted: true, want: aj.ReconciliationRequired, edit: func(nonBOSH string, raw map[string]json.RawMessage) string {
			raw["bosh_stemcell"] = json.RawMessage(`{"cid":"stemcell-other"}`)
			return nonBOSH
		}},
		{name: "another sentinel key is dropped", admitted: true, want: aj.ReconciliationRequired, edit: func(nonBOSH string, raw map[string]json.RawMessage) string {
			delete(raw, "bosh_pool")
			return nonBOSH
		}},
		{name: "another sentinel key is added", admitted: true, want: aj.ReconciliationRequired, edit: func(nonBOSH string, raw map[string]json.RawMessage) string {
			raw["bosh_disk_metadata"] = json.RawMessage(`{"disk-token":{"added":"1"}}`)
			return nonBOSH
		}},
		{name: "description beside another field", protection: &protect, admitted: true, want: aj.ReconciliationRequired},
	} {
		t.Run(tc.name, func(t *testing.T) { runOverlayWriteCase(t, tc) })
	}
}

// runOverlayWriteCase opens an attach_disk lifecycle, makes the case's write
// through its guard, checks the admission flag and the journaled step, and
// then finishes the lifecycle with a lock timeout.
func runOverlayWriteCase(t *testing.T, tc overlayWriteCase) {
	t.Helper()
	deps, client, journal, id, cid := lifecycleFlowFixtureState(t, true, true)
	bare, meta, err := decodeDiskCID(t.Context(), deps, "attach_disk", cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(t.Context(), deps, "attach_disk", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	local, lifecycle, err := managedDiskOperation(t.Context(), deps, rd, "attach_disk")
	if err != nil || lifecycle == nil {
		t.Fatalf("lifecycle admission: %v", err)
	}
	// The holder starts with operator text, stemcell provenance, and a pool
	// record, so every case has something it could disturb.
	current := client.state.configs[777]
	seeded, err := pve.RenderSentinel("operator text", map[string]json.RawMessage{
		"bosh_stemcell": json.RawMessage(`{"cid":"stemcell-original"}`),
		"bosh_pool":     json.RawMessage(`{"pool":"bosh-pool"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	current[pveConfigKeyDescription] = seeded
	description := overlayWriteDescription(t, pve.DescriptionFromConfig(current), tc.edit)
	params := &nodes.UpdateQemuConfigParams{Description: &description, Protection: tc.protection}
	if tc.digest {
		digest, _ := pve.ConfigString(current, "digest")
		params.Digest = &digest
	}
	if err := local.PVE.Nodes().UpdateQemuConfig(t.Context(), "n1", "777", params); err != nil {
		t.Fatalf("guarded holder write: %v", err)
	}
	if lifecycle.diskMutationAdmitted != tc.admitted {
		t.Fatalf("diskMutationAdmitted = %t, want %t", lifecycle.diskMutationAdmitted, tc.admitted)
	}
	assertHolderWriteObserved(t, journal, id)

	timeout := cpierrors.WrapAs(errors.Join(errors.New("held"), pve.ErrClusterLockTimeout), cpierrors.TypeRetriableCloud, "AcquireClusterLock: timed out")
	result := lifecycle.finish(t.Context(), timeout, false)
	if !errors.Is(result, pve.ErrClusterLockTimeout) {
		t.Fatalf("finish lost the timeout: %v", result)
	}
	if returned := isDiskReturnedAfterLockTimeout(result); returned != (tc.want == aj.ReadyToReturn) {
		t.Fatalf("returned-disk marker = %t, want %t", returned, tc.want == aj.ReadyToReturn)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != tc.want {
		t.Fatalf("record state = %s (reason %q), want %s", record.State, record.Reason, tc.want)
	}
}

// assertHolderWriteObserved checks that the guard journaled the write onto VM
// 777 as an observed step, whether or not it counted as a disk mutation.
func assertHolderWriteObserved(t *testing.T, journal *aj.Journal, id string) {
	t.Helper()
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	for i := range record.Steps {
		if record.Steps[i].Kind == "lifecycle_attach_disk_Nodes_UpdateQemuConfig" && record.Steps[i].State == aj.Observed && record.Steps[i].Target.VMID == 777 {
			return
		}
	}
	t.Fatalf("the write was not journaled as an observed step: %+v", record.Steps)
}
