package handlers

import (
	"maps"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdk "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// retypedPlan returns a copy of plan whose persistent pool has another PVE
// type. Inventory admits only shared NFS as a storage-set member today, so a
// block pool cannot come out of the planner, and revalidation refuses a plan
// whose recorded definition differs from inventory. The allocator still has to
// handle a block pool if that gate ever widens, so these tests drive its
// format, naming, CID, and readback steps directly.
func retypedPlan(plan *StorageAllocationPlan, storageType string) *StorageAllocationPlan {
	out := *plan
	out.Definitions = maps.Clone(plan.Definitions)
	id := plan.Targets[0].StorageID
	info := out.Definitions[id]
	info.Type = storageType
	out.Definitions[id] = info
	return &out
}

// A journal-managed persistent disk follows the type of the pool its plan
// chose: the format sent, the volume name and its extension, the CID record,
// and the readback verification all agree with that pool.
func TestManagedDiskFormatFollowsPoolType(t *testing.T) {
	cases := []struct {
		name       string
		poolType   string
		requested  string
		wantFormat string
		wantBlock  bool
		wantWarn   bool
	}{
		{"unset on lvmthin is raw with a bare name", "lvmthin", "", "raw", true, false},
		{"explicit qcow2 on lvmthin is raw and warns", "lvmthin", "qcow2", "raw", true, true},
		{"explicit qcow2 on thick lvm is raw and warns", "lvm", "qcow2", "raw", true, true},
		{"unset on zfspool is raw", "zfspool", "", "raw", true, false},
		{"unset on nfs is qcow2 with an extension", "nfs", "", "qcow2", false, false},
		{"unset on dir is qcow2 with an extension", "dir", "", "qcow2", false, false},
		{"explicit raw on nfs keeps raw", "nfs", "raw", "raw", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _, state, obs := managedDiskCallFixture(t, "spread", false, nil)
			m.plan = retypedPlan(m.plan, tc.poolType)
			m.requested = tc.requested
			if err := m.applyTargetFormat(t.Context(), m.plan); err != nil {
				t.Fatal(err)
			}
			if m.format != tc.wantFormat {
				t.Fatalf("format = %q; want %q", m.format, tc.wantFormat)
			}
			target := m.plan.Targets[0]
			name, volume, err := pve.AllocationVolume(m.targetStorageType(), target.StorageID, 9000, m.plan.Namespace, m.id, m.format)
			if err != nil {
				t.Fatal(err)
			}
			checkAllocationVolumeShape(t, target.StorageID, name, volume, tc.wantFormat, tc.wantBlock)
			if _, id, ok := pve.ParseAllocationVolumeID(volume); !ok || id != m.id {
				t.Errorf("ParseAllocationVolumeID(%q) = (%q, %v); want the allocation ID", volume, id, ok)
			}
			checkManagedReadback(t, m, state, volume)
			cid, err := m.cid(volume)
			if err != nil {
				t.Fatal(err)
			}
			cidVolume, meta, err := pve.ParseEncodedDiskCID(cid)
			if err != nil {
				t.Fatal(err)
			}
			if cidVolume != volume || meta.Format != tc.wantFormat {
				t.Errorf("CID = (%q, format %q); want (%q, format %q)", cidVolume, meta.Format, volume, tc.wantFormat)
			}
			if warned := len(formatWarnings(obs)); (warned == 1) != tc.wantWarn || warned > 1 {
				t.Errorf("format warnings = %d; want warning=%v", warned, tc.wantWarn)
			}
		})
	}
}

func checkAllocationVolumeShape(t *testing.T, storageID, name, volume, format string, block bool) {
	t.Helper()
	bare := strings.TrimPrefix(volume, storageID+":")
	if block && (bare != name || strings.Contains(name, ".")) {
		t.Errorf("block volume (%q, %q) has a file-style name", name, volume)
	}
	if !block && (bare != "9000/"+name || !strings.HasSuffix(name, "."+format)) {
		t.Errorf("file volume (%q, %q) lacks the vmid directory or the .%s extension", name, volume, format)
	}
}

// checkManagedReadback proves the readback expects the format we request and
// refuses any other.
func checkManagedReadback(t *testing.T, m *managedDiskRequest, state *managedDiskTestState, volume string) {
	t.Helper()
	state.volumes[volume] = &nodes.GetStorageContentResponse{Format: m.format, Size: sdk.PVEInt(m.plan.Targets[0].VirtualBytes)}
	if err := m.observeVolume(t.Context(), volume); err != nil {
		t.Errorf("readback of the requested format failed: %v", err)
	}
	other := "qcow2"
	if m.format == other {
		other = "raw"
	}
	state.volumes[volume].Format = other
	if err := m.observeVolume(t.Context(), volume); err == nil {
		t.Errorf("readback accepted %s when we requested %s", other, m.format)
	}
}

// An unset or "auto" request reaches the allocator as "", so the pool's type
// decides. This drives the real planner and PVE calls end to end on NFS.
func TestManagedDiskUnsetRequestIsQCOW2OnNFS(t *testing.T) {
	m, h, state, _ := managedDiskCallFixture(t, "spread", false, map[string]any{"disk_format": "auto"})
	if m.requested != "" || m.format != "qcow2" {
		t.Fatalf("request = %q, format = %q; want unset and qcow2", m.requested, m.format)
	}
	if _, err := m.execute(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if len(state.created) != 1 || !strings.HasSuffix(state.created[0], ".qcow2") || state.volumes[state.created[0]].Format != "qcow2" {
		t.Fatalf("created = %v; want one qcow2 volume", state.created)
	}
}

// iscsi and iscsidirect expose LUNs that already exist, so a plan that lands
// a persistent disk there is refused before anything is recorded or sent.
func TestManagedDiskRefusesPoolThatCannotAllocateImages(t *testing.T) {
	for _, typ := range []string{"iscsi", "iscsidirect"} {
		t.Run(typ, func(t *testing.T) {
			m, _, state, _ := managedDiskCallFixture(t, "spread", false, nil)
			err := m.applyTargetFormat(t.Context(), retypedPlan(m.plan, typ))
			if err == nil || !strings.Contains(err.Error(), "cannot allocate disk images; no allocation submitted") {
				t.Fatalf("applyTargetFormat = %v; want a refusal", err)
			}
			if len(state.created) != 0 {
				t.Fatalf("the refusal submitted an allocation: %v", state.created)
			}
		})
	}
}

// A retry that moves the disk to another pool resolves the format against the
// plan it is handed, not the plan it is leaving, so the next attempt's name,
// CID, and readback suit the new pool.
func TestManagedDiskApplyTargetFormatFollowsTheNewPlan(t *testing.T) {
	m, _, _, _ := managedDiskCallFixture(t, "spread", true, nil)
	m.plan = retypedPlan(m.plan, "lvmthin")
	if err := m.applyTargetFormat(t.Context(), m.plan); err != nil || m.format != "raw" {
		t.Fatalf("block plan: format = %q, err = %v; want raw", m.format, err)
	}
	if err := m.applyTargetFormat(t.Context(), retypedPlan(m.plan, "nfs")); err != nil || m.format != "qcow2" {
		t.Fatalf("file plan: format = %q, err = %v; want qcow2", m.format, err)
	}
}
