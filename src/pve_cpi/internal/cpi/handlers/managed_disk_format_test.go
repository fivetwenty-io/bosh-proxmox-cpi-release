package handlers

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdk "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// retypedPlan returns a copy of plan whose persistent pool has another PVE
// type. Inventory admits only shared NFS as a storage-set member today, so the
// planner never hands the allocator another type, and revalidation refuses a
// plan whose recorded definition differs from inventory. These tests drive the
// allocator's format step directly to prove what it does with each type.
func retypedPlan(plan *StorageAllocationPlan, storageType string) *StorageAllocationPlan {
	out := *plan
	out.Definitions = maps.Clone(plan.Definitions)
	id := plan.Targets[0].StorageID
	info := out.Definitions[id]
	info.Type = storageType
	out.Definitions[id] = info
	return &out
}

// A journal-managed persistent disk on file storage takes its format from the
// request, or qcow2 when the request is unset, and the volume name, the CID
// record, and the readback all agree with that format.
func TestManagedDiskFormatOnFileStorage(t *testing.T) {
	cases := []struct {
		name, poolType, requested, wantFormat string
	}{
		{"unset on nfs is qcow2", "nfs", "", "qcow2"},
		{"unset on dir is qcow2", "dir", "", "qcow2"},
		{"explicit raw on nfs keeps raw", "nfs", "raw", "raw"},
		{"explicit vmdk on nfs keeps vmdk", "nfs", "vmdk", "vmdk"},
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
			name, err := pve.AllocationVolumeName(9000, m.plan.Namespace, m.id, m.format)
			if err != nil {
				t.Fatal(err)
			}
			volume := fmt.Sprintf("%s:9000/%s", target.StorageID, name)
			if !strings.HasSuffix(name, "."+tc.wantFormat) {
				t.Errorf("volume name %q lacks the .%s extension", name, tc.wantFormat)
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
			if warned := formatWarnings(obs); len(warned) != 0 {
				t.Errorf("unexpected format warnings: %v", warned)
			}
		})
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

// The allocator fails closed. A plan whose persistent pool is not file storage
// is refused with a non-retriable error that names the pool and its type,
// before the format, the options, or anything in PVE changes.
func TestManagedDiskRefusesPoolThatIsNotFileStorage(t *testing.T) {
	for _, typ := range []string{"lvmthin", "lvm", "zfspool", "rbd", "iscsi", "iscsidirect", "esxi", ""} {
		t.Run("type "+typ, func(t *testing.T) {
			m, _, state, _ := managedDiskCallFixture(t, "spread", false, nil)
			before := m.format
			plan := retypedPlan(m.plan, typ)
			storage := plan.Targets[0].StorageID
			err := m.applyTargetFormat(t.Context(), plan)
			var cloud *cpierrors.Error
			if !errors.As(err, &cloud) || cloud.OkToRetry() {
				t.Fatalf("applyTargetFormat = %v; want a non-retriable cloud error", err)
			}
			for _, want := range []string{fmt.Sprintf("%q", storage), fmt.Sprintf("type %q", typ), "no allocation submitted"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not mention %s", err, want)
				}
			}
			if m.format != before || len(state.created) != 0 {
				t.Fatalf("the refusal changed the request (format %q) or submitted %v", m.format, state.created)
			}
		})
	}
}

// A retry resolves the format against the plan it is handed, not the plan it
// is leaving, so a retry onto a pool that is not file storage stops there.
func TestManagedDiskApplyTargetFormatFollowsTheNewPlan(t *testing.T) {
	m, _, _, _ := managedDiskCallFixture(t, "spread", true, nil)
	m.requested = "raw"
	if err := m.applyTargetFormat(t.Context(), retypedPlan(m.plan, "nfs")); err != nil || m.format != "raw" {
		t.Fatalf("file plan: format = %q, err = %v; want raw", m.format, err)
	}
	if err := m.applyTargetFormat(t.Context(), retypedPlan(m.plan, "lvmthin")); err == nil {
		t.Fatal("block plan on retry was accepted")
	}
}
