package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestManagedVMDisposalRetainRule(t *testing.T) {
	const id = "8b0e3c1a-5f4d-4e2b-9c7a-1d2e3f4a5b6c"
	token := managedVMRetentionToken(id)
	serialStep := func(vmid int, state aj.State) aj.Step {
		return aj.Step{ID: "serial", Kind: managedVMRetentionStepPrefix + "Nodes_UpdateQemuConfig", State: state, Target: aj.Target{Node: "n1", VMID: vmid}}
	}
	createStep := aj.Step{ID: "create", Kind: managedVMRetentionStepPrefix + "QEMU_Create", State: aj.Observed, Target: aj.Target{Node: "n1", VMID: 90100}}
	tagged := map[string]any{"tags": tagRetainEphemeral, "scsi1": "a:777/vm-777-ephemeral-0.raw,size=5G"}
	untagged := map[string]any{"scsi1": "a:777/vm-777-ephemeral-0.raw,size=5G"}
	ours := map[string]any{"scsi1": "a:777/vm-777-ephemeral-0.raw,size=5G,serial=" + token}
	for _, row := range []struct {
		name      string
		disposal  managedVMDisposal
		requested bool
		cid       string
		steps     []aj.Step
		guest     map[string]any
		want      bool
	}{
		{"rollback with the tag, a CID, and an observed serial step", managedVMCreateRollback, true, "777", []aj.Step{serialStep(777, aj.Observed)}, ours, false},
		{"delete_vm asked", managedVMDeletion, true, "", nil, untagged, true},
		{"observed serial step aimed at the guest", managedVMDeletion, false, "", []aj.Step{serialStep(777, aj.Observed)}, untagged, true},
		{"our token on a guest drive", managedVMDeletion, false, "", nil, ours, true},
		{"CID and the live retain tag", managedVMDeletion, false, "777", nil, tagged, true},
		{"planned serial step alone", managedVMDeletion, false, "", []aj.Step{serialStep(777, aj.Planned)}, untagged, false},
		{"parker create step alone", managedVMDeletion, false, "777", []aj.Step{createStep}, untagged, false},
		{"tag without a CID", managedVMDeletion, false, "", nil, tagged, false},
		{"serial step aimed at another VMID", managedVMDeletion, false, "", []aj.Step{serialStep(778, aj.Observed)}, untagged, false},
		{"no live guest", managedVMDeletion, false, "777", nil, nil, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			record := aj.Record{ID: id, Kind: "vm", CID: row.cid, Steps: row.steps}
			if got := managedVMDisposalRetains(row.disposal, row.requested, record, 777, row.guest); got != row.want {
				t.Errorf("retains = %t, want %t", got, row.want)
			}
		})
	}
}

// cleanRetentionTimeout runs a fresh-parker delete_vm out of its parker lock
// wait and returns the check that judged it clean, with the record it read.
func cleanRetentionTimeout(t *testing.T) (*retentionCase, managedRetentionTimeoutCheck, aj.Record, int) {
	t.Helper()
	c := newRetentionCase(t, false)
	c.holdEveryParkerLock()
	c.assertReturnedTimeout(t, "first call", c.deleteVM())
	record := c.record(t)
	written, _ := pve.ConfigString(c.client.state.configs[777], "scsi1")
	cid, err := pve.EncodeDiskCID(retentionEphemeral, &pve.DiskCIDMeta{ID: c.token()})
	if err != nil {
		t.Fatal(err)
	}
	check := managedRetentionTimeoutCheck{start: 1, node: "n1", vmid: 777, slot: "scsi1", written: written, volume: retentionEphemeral, cid: cid, token: c.token()}
	if !check.returned(t.Context(), c.deps, record, lockTimeoutError(), nil) {
		t.Fatal("the clean timeout's own check refuses it")
	}
	parker := c.createdParker(t)
	return c, check, record, parker
}

func TestRetentionReturnedAfterLockTimeoutRules(t *testing.T) {
	step := func(kind string, vmid int) aj.Step {
		return aj.Step{ID: "extra-" + kind, Kind: managedVMRetentionStepPrefix + kind, State: aj.Observed, Target: aj.Target{Node: "n1", VMID: vmid, Storage: "a", IntendedVolume: retentionEphemeral}}
	}
	setEntry := func(t *testing.T, config map[string]any, token string, entry map[string]any) {
		t.Helper()
		encoded, err := json.Marshal(map[string]any{token: entry})
		if err != nil {
			t.Fatal(err)
		}
		description, err := pve.RenderSentinel("", map[string]json.RawMessage{"bosh_parked_disks": encoded})
		if err != nil {
			t.Fatal(err)
		}
		config[pveConfigKeyDescription] = description
	}
	type edit func(t *testing.T, c *retentionCase, check *managedRetentionTimeoutCheck, record *aj.Record, parker int) (err, guardErr error)
	rows := []struct {
		name string
		edit edit
	}{
		{"wrong error kind", func(*testing.T, *retentionCase, *managedRetentionTimeoutCheck, *aj.Record, int) (error, error) {
			return cpierrors.Cloud("transfer failed"), nil
		}},
		{"poisoned guard", func(*testing.T, *retentionCase, *managedRetentionTimeoutCheck, *aj.Record, int) (error, error) {
			return lockTimeoutError(), errors.New("guard poisoned")
		}},
		{"unsettled step", func(_ *testing.T, _ *retentionCase, _ *managedRetentionTimeoutCheck, record *aj.Record, _ int) (error, error) {
			record.Steps[len(record.Steps)-1].State = aj.Planned
			return lockTimeoutError(), nil
		}},
		{"slot delete", func(_ *testing.T, c *retentionCase, _ *managedRetentionTimeoutCheck, record *aj.Record, _ int) (error, error) {
			record.Steps = append(record.Steps, step("Nodes_UpdateQemuConfig", 777))
			c.client.state.configs[777]["unused0"] = retentionEphemeral
			delete(c.client.state.configs[777], "scsi1")
			return lockTimeoutError(), nil
		}},
		{"move", func(_ *testing.T, _ *retentionCase, _ *managedRetentionTimeoutCheck, record *aj.Record, _ int) (error, error) {
			record.Steps = append(record.Steps, step("Nodes_CreateQemuMoveDisk", 777))
			return lockTimeoutError(), nil
		}},
		{"attach", func(_ *testing.T, _ *retentionCase, _ *managedRetentionTimeoutCheck, record *aj.Record, parker int) (error, error) {
			record.Steps = append(record.Steps, step("QEMU_AttachDisk", parker))
			return lockTimeoutError(), nil
		}},
		{"parker config write", func(_ *testing.T, _ *retentionCase, _ *managedRetentionTimeoutCheck, record *aj.Record, parker int) (error, error) {
			record.Steps = append(record.Steps, step("Nodes_UpdateQemuConfig", parker))
			return lockTimeoutError(), nil
		}},
		{"third guest config write", func(_ *testing.T, _ *retentionCase, _ *managedRetentionTimeoutCheck, record *aj.Record, _ int) (error, error) {
			record.Steps = append(record.Steps, step("Nodes_UpdateQemuConfig", 777))
			return lockTimeoutError(), nil
		}},
		{"second holder create", func(_ *testing.T, _ *retentionCase, _ *managedRetentionTimeoutCheck, record *aj.Record, parker int) (error, error) {
			record.Steps = append(record.Steps, step("QEMU_Create", parker+1))
			return lockTimeoutError(), nil
		}},
		{"create aimed at the guest", func(_ *testing.T, _ *retentionCase, _ *managedRetentionTimeoutCheck, record *aj.Record, _ int) (error, error) {
			for index := range record.Steps {
				if record.Steps[index].Kind == managedVMRetentionStepPrefix+"QEMU_Create" {
					record.Steps[index].Target.VMID = 777
				}
			}
			return lockTimeoutError(), nil
		}},
		{"drive string differs", func(_ *testing.T, c *retentionCase, check *managedRetentionTimeoutCheck, _ *aj.Record, _ int) (error, error) {
			c.client.state.configs[777]["scsi1"] = check.written + ",cache=none"
			return lockTimeoutError(), nil
		}},
		{"volume absent", func(_ *testing.T, c *retentionCase, _ *managedRetentionTimeoutCheck, _ *aj.Record, _ int) (error, error) {
			delete(c.client.state.volumes, retentionEphemeral)
			return lockTimeoutError(), nil
		}},
		{"CID entry differs", func(_ *testing.T, _ *retentionCase, check *managedRetentionTimeoutCheck, _ *aj.Record, _ int) (error, error) {
			check.cid = "pvd-another"
			return lockTimeoutError(), nil
		}},
		{"marker gone", func(_ *testing.T, c *retentionCase, _ *managedRetentionTimeoutCheck, _ *aj.Record, _ int) (error, error) {
			attached := pve.GetAttachedDiskCIDs(pve.DescriptionFromConfig(c.client.state.configs[777]))
			raw, err := json.Marshal(attached)
			if err != nil {
				return nil, err
			}
			description, err := pve.RenderSentinel("", map[string]json.RawMessage{"bosh_attached_disks": raw})
			if err != nil {
				return nil, err
			}
			c.client.state.configs[777][pveConfigKeyDescription] = description
			return lockTimeoutError(), nil
		}},
		{"created parker holds a volume", func(_ *testing.T, c *retentionCase, _ *managedRetentionTimeoutCheck, _ *aj.Record, parker int) (error, error) {
			other := fmt.Sprintf("a:%d/vm-%d-disk-0.raw", parker, parker)
			c.client.state.volumes[other] = c.client.state.volumes[retentionEphemeral]
			c.client.state.configs[parker]["scsi0"] = other
			return lockTimeoutError(), nil
		}},
		{"created parker has a slotted entry", func(t *testing.T, c *retentionCase, check *managedRetentionTimeoutCheck, _ *aj.Record, parker int) (error, error) {
			setEntry(t, c.client.state.configs[parker], check.token, map[string]any{"disk_cid": check.cid, "volid": retentionEphemeral, "node": "n1", "slot": "scsi0"})
			return lockTimeoutError(), nil
		}},
		{"token on another holder", func(_ *testing.T, c *retentionCase, check *managedRetentionTimeoutCheck, _ *aj.Record, _ int) (error, error) {
			bare := retentionEphemeral + ",size=5G"
			c.client.state.configs[777]["scsi1"] = bare
			check.written = bare
			other := "a:778/vm-778-disk-0.raw"
			c.client.state.volumes[other] = c.client.state.volumes[retentionEphemeral]
			c.client.state.configs[778] = map[string]any{"name": "other", "digest": "1", "scsi0": other + ",serial=" + check.token}
			return lockTimeoutError(), nil
		}},
		{"token resolves through an intent", func(t *testing.T, c *retentionCase, check *managedRetentionTimeoutCheck, _ *aj.Record, parker int) (error, error) {
			bare := retentionEphemeral + ",size=5G"
			c.client.state.configs[777]["scsi1"] = bare
			check.written = bare
			setEntry(t, c.client.state.configs[parker], check.token, map[string]any{"disk_cid": check.cid, "volid": retentionEphemeral, "node": "n1", "slot": "scsi0", "parked_at": "2026-10-01T00:00:00Z"})
			return lockTimeoutError(), nil
		}},
		{"read error", func(_ *testing.T, c *retentionCase, _ *managedRetentionTimeoutCheck, _ *aj.Record, _ int) (error, error) {
			c.pve.configAnswer = func(vmid int) error {
				if vmid == 777 {
					return errors.New("connection reset by peer")
				}
				return nil
			}
			return lockTimeoutError(), nil
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c, check, record, parker := cleanRetentionTimeout(t)
			if parker == 0 {
				t.Fatal("the fresh-parker shape recorded no parker")
			}
			record.Steps = append([]aj.Step(nil), record.Steps...)
			err, guardErr := row.edit(t, c, &check, &record, parker)
			if check.returned(t.Context(), c.deps, record, err, guardErr) {
				t.Error("the check returned the record as a clean timeout")
			}
		})
	}
}
