package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// migratedHolderVMID is the holder each row here migrates or deletes.
const migratedHolderVMID = 90144

// migratedHolderCase is the flow fixture's managed disk moved onto holder
// migratedHolderVMID, which a migration has just moved from n1 to n2. The
// disk's volume is on shared storage, so it keeps its name across the move.
type migratedHolderCase struct {
	ctx     context.Context
	client  *lifecycleFlowPVE
	guard   *managedDiskLifecycleGuard
	volume  string
	token   string
	backing string
}

func newMigratedHolderCase(t *testing.T) *migratedHolderCase {
	t.Helper()
	deps, client, journal, id, _ := lifecycleFlowFixture(t)
	ctx := pve.WithTestBackoff(t.Context(), func(int) time.Duration { return 0 })
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := pve.ConfigString(client.state.configs[777], "scsi1")
	volume := strings.Split(value, ",")[0]
	delete(client.state.configs[777], "scsi1")
	client.state.configs[migratedHolderVMID] = map[string]any{
		"name": "mover", "tags": "bosh-cpi;bosh-parker;" + pve.DiskMoverTag, "digest": "1",
		"scsi0": volume + ",serial=" + record.DiskToken + ",size=5G",
	}
	if client.vmNodes == nil {
		client.vmNodes = map[int]string{}
	}
	client.vmNodes[migratedHolderVMID] = "n2"
	storage, _, err := pve.ParseDiskCID(volume)
	if err != nil {
		t.Fatal(err)
	}
	backing, err := managedDiskActualBacking(ctx, deps, storage)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := &managedDiskLifecycle{deps: deps, disk: resolvedDisk{
		volid: volume, stableID: record.DiskToken,
		allocation: &managedDiskIdentity{provenance: pve.DiskAllocationProvenance{Node: "n1", Backing: backing}},
	}}
	return &migratedHolderCase{
		ctx: ctx, client: client, guard: &managedDiskLifecycleGuard{lifecycle: lifecycle},
		volume: volume, token: record.DiskToken, backing: backing,
	}
}

// answerOn makes every config read of the holder on node fail with err.
func (c *migratedHolderCase) answerOn(node string, err error) {
	c.client.onNodeConfigRead = func(read string, vmid int) error {
		if read == node && vmid == migratedHolderVMID {
			return err
		}
		return nil
	}
}

func (c *migratedHolderCase) migration() managedDiskMutationObservation {
	return managedDiskMutationObservation{node: "n1", targetNode: "n2", vmid: migratedHolderVMID}
}

func (c *migratedHolderCase) observeHolder() (string, string, error) {
	return observeMigratedHolder(c.ctx, c.guard.lifecycle.deps, migratedHolderVMID, "n1", "n2", c.token, c.backing)
}

// errMigrateSourceTimeout is a 500 from the source node that doesn't say the
// configuration file is gone.
var errMigrateSourceTimeout = &sdkerrors.APIError{HTTPCode: 500, Message: "got timeout"}

// TestObserveMigration_AcceptsTheSourceNodesMissingConfigAnswer observes a
// finished migration whose source node answers the config read with PVE's
// real 500 for a configuration file that is gone, and then with a plain 404.
// Both prove the source empty, the probe finds the holder on the target, and
// the step observes the volume it held before and the one that landed.
func TestObserveMigration_AcceptsTheSourceNodesMissingConfigAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer error
	}{
		{"pmxcfs 500", errGuardSiteConfigGone},
		{"404", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newMigratedHolderCase(t)
			if tc.answer != nil {
				c.answerOn("n1", tc.answer)
			}
			slot, landed, err := c.observeHolder()
			if err != nil || slot != "scsi0" || landed != c.volume {
				t.Fatalf("observeMigratedHolder = %q, %q, %v; want scsi0 and %s", slot, landed, err, c.volume)
			}
			volumes, err := c.guard.observeMigration(c.ctx, c.migration())
			if err != nil {
				t.Fatalf("observeMigration: %v", err)
			}
			if len(volumes) != 2 || volumes[0] != c.volume || volumes[1] != c.volume {
				t.Fatalf("observed volumes = %v, want the recorded and landed volume %s", volumes, c.volume)
			}
			if node := c.guard.lifecycle.disk.allocation.provenance.Node; node != "n2" {
				t.Fatalf("provenance node = %s after the migration, want n2", node)
			}
		})
	}
}

// TestObserveMigration_RefusesASourceThatIsNotProvenEmpty refuses when the
// source node still answers with the holder's config, and when it fails with
// a 500 that doesn't say the configuration file is gone.
func TestObserveMigration_RefusesASourceThatIsNotProvenEmpty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(c *migratedHolderCase)
	}{
		{"config still on the source", func(c *migratedHolderCase) { c.client.vmNodes[migratedHolderVMID] = "n1" }},
		{"another 500", func(c *migratedHolderCase) { c.answerOn("n1", errMigrateSourceTimeout) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newMigratedHolderCase(t)
			tc.setup(c)
			if _, err := c.guard.observeMigration(c.ctx, c.migration()); err == nil ||
				!strings.Contains(err.Error(), "migration source holder absence not observed") {
				t.Fatalf("observeMigration = %v, want the source absence refusal", err)
			}
		})
	}
}

// TestObserveMigration_RefusesWhenTheProbeDoesNotPlaceTheHolderOnTheTarget
// refuses a migration whose source answered empty when the config probe of
// every member finds the holder back on the source, finds it nowhere, or
// can't finish.
func TestObserveMigration_RefusesWhenTheProbeDoesNotPlaceTheHolderOnTheTarget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(c *migratedHolderCase)
		want  string
	}{
		{"on the source", func(c *migratedHolderCase) {
			// The first read, the guard's own, answers that the config is
			// gone, and every read after it finds the holder on n1.
			c.client.vmNodes[migratedHolderVMID] = "n1"
			reads := 0
			c.client.onNodeConfigRead = func(node string, vmid int) error {
				if node != "n1" || vmid != migratedHolderVMID {
					return nil
				}
				reads++
				if reads == 1 {
					return errGuardSiteConfigGone
				}
				return nil
			}
		}, "not found on node n2"},
		{"nowhere", func(c *migratedHolderCase) {
			delete(c.client.state.configs, migratedHolderVMID)
			delete(c.client.vmNodes, migratedHolderVMID)
		}, "not found on node n2"},
		{"probe error", func(c *migratedHolderCase) {
			c.answerOn("n1", errGuardSiteConfigGone)
			previous := c.client.onNodeConfigRead
			c.client.onNodeConfigRead = func(node string, vmid int) error {
				if node == "n2" && vmid == migratedHolderVMID {
					return errMigrateSourceTimeout
				}
				return previous(node, vmid)
			}
		}, "placement not observed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newMigratedHolderCase(t)
			tc.setup(c)
			if _, err := c.guard.observeMigration(c.ctx, c.migration()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("observeMigration = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// TestObserveMigration_RefusesAHeldMigrateLockAsRetriable refuses a target
// config that still carries PVE's migrate lock, with a retriable error.
func TestObserveMigration_RefusesAHeldMigrateLockAsRetriable(t *testing.T) {
	c := newMigratedHolderCase(t)
	c.answerOn("n1", errGuardSiteConfigGone)
	c.client.state.configs[migratedHolderVMID]["lock"] = "migrate"
	_, _, err := c.observeHolder()
	if err == nil || !strings.Contains(err.Error(), "migrate lock") {
		t.Fatalf("observeMigratedHolder = %v, want the migrate lock refusal", err)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("observeMigratedHolder = %v, want a retriable error", err)
	}
	if _, err := c.guard.observeMigration(c.ctx, c.migration()); err == nil || !strings.Contains(err.Error(), "migrate lock") {
		t.Fatalf("observeMigration = %v, want the migrate lock refusal", err)
	}
}

// TestObserveMigration_RefusesASerialOnTwoSlots refuses a target config that
// carries the disk's serial on two slots, because the landed volume is then
// ambiguous.
func TestObserveMigration_RefusesASerialOnTwoSlots(t *testing.T) {
	c := newMigratedHolderCase(t)
	c.answerOn("n1", errGuardSiteConfigGone)
	c.client.state.configs[migratedHolderVMID]["scsi1"] = c.volume + ",serial=" + c.token + ",size=5G"
	if _, err := c.guard.observeMigration(c.ctx, c.migration()); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("observeMigration = %v, want the ambiguous identity refusal", err)
	}
}

// deletion is the observation of the step that deleted the holder on n1.
func (c *migratedHolderCase) deletion() managedDiskMutationObservation {
	return managedDiskMutationObservation{node: "n1", vmid: migratedHolderVMID}
}

// deleteHolder removes the holder from every member.
func (c *migratedHolderCase) deleteHolder() {
	delete(c.client.state.configs, migratedHolderVMID)
	delete(c.client.vmNodes, migratedHolderVMID)
}

// TestObserveHolderDeletion_AcceptsTheMissingConfigAnswer observes a deleted
// holder whose recorded node answers the config read with PVE's real 500 for
// a configuration file that is gone, and then with a plain 404. The probe of
// every member finds the holder nowhere in both rows.
func TestObserveHolderDeletion_AcceptsTheMissingConfigAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer error
	}{
		{"pmxcfs 500", errGuardSiteConfigGone},
		{"404", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newMigratedHolderCase(t)
			c.deleteHolder()
			if tc.answer != nil {
				c.answerOn("n1", tc.answer)
			}
			volumes, err := c.guard.observeHolderDeletion(c.ctx, c.deletion())
			if err != nil {
				t.Fatalf("observeHolderDeletion: %v", err)
			}
			if len(volumes) != 1 || volumes[0] != c.volume {
				t.Fatalf("observed volumes = %v, want the recorded volume %s", volumes, c.volume)
			}
		})
	}
}

// TestObserveHolderDeletion_RefusesAHolderThatIsNotProvenGone refuses when
// the recorded node fails with a 500 that doesn't say the configuration file
// is gone, when the recorded node answers empty but the probe finds the holder
// on another member, and when the probe can't finish.
func TestObserveHolderDeletion_RefusesAHolderThatIsNotProvenGone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(c *migratedHolderCase)
	}{
		{"another 500", func(c *migratedHolderCase) {
			c.deleteHolder()
			c.answerOn("n1", errMigrateSourceTimeout)
		}},
		// The holder moved to n2 rather than being deleted.
		{"probe hit on another node after a 404", func(*migratedHolderCase) {}},
		{"probe hit on another node after a pmxcfs 500", func(c *migratedHolderCase) {
			c.answerOn("n1", errGuardSiteConfigGone)
		}},
		{"probe error", func(c *migratedHolderCase) {
			c.deleteHolder()
			c.answerOn("n1", errGuardSiteConfigGone)
			previous := c.client.onNodeConfigRead
			c.client.onNodeConfigRead = func(node string, vmid int) error {
				if node == "n2" && vmid == migratedHolderVMID {
					return errMigrateSourceTimeout
				}
				return previous(node, vmid)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newMigratedHolderCase(t)
			tc.setup(c)
			if _, err := c.guard.observeHolderDeletion(c.ctx, c.deletion()); err == nil ||
				!strings.Contains(err.Error(), "temporary holder deletion is not observed") {
				t.Fatalf("observeHolderDeletion = %v, want the deletion refusal", err)
			}
		})
	}
}
