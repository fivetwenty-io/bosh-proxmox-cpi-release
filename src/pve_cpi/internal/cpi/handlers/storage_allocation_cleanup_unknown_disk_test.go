package handlers

import (
	"context"
	"errors"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"os"
	"reflect"
	"testing"
)

func unknownDiskCleanupFixture(t *testing.T) (Deps, *lifecycleFlowPVE, *aj.Journal, aj.Record) {
	t.Helper()
	deps, client, original, id, _ := lifecycleFlowFixtureState(t, false)
	old, err := original.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := activeStorageAllocationPlan(old)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	journal, err := aj.Initialize(t.Context(), dir, old.Namespace, aj.Enrollment{ClusterID: old.ClusterID, AuthorityID: "authority", AuditID: "audit", PreviousWriterFenced: true, CompleteHistoricalAudit: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	})
	h, err := journal.CreateDisk(t.Context(), id, old.Intent)
	if err != nil {
		t.Fatal(err)
	}
	parameters, e := aj.MutationParameters(map[string]any{"version": 1, "kind": "persistent_birth", "absence_verified": true})
	if e != nil {
		t.Fatal(e)
	}
	if _, err = storageMutationIntent(h, "create_persistent_volume", old.Steps[0].Target, plan.Charges, parameters); err != nil {
		t.Fatal(err)
	}
	record := h.Record()
	record.State = aj.ReconciliationRequired
	record.Reason = "submission response lost"
	if err = h.Save(record); err != nil {
		t.Fatal(err)
	}
	record = h.Record()
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	delete(client.state.configs, 777)
	copied := *deps.Config
	copied.StorageAllocationJournalDir = dir
	deps.Config = &copied
	return deps, client, journal, record
}

func TestCleanupUnknownPersistentSubmissionDisposesObservedOutcome(t *testing.T) {
	for _, absent := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned volume", true: "complete absence"}[absent], func(t *testing.T) {
			deps, client, journal, before := unknownDiskCleanupFixture(t)
			if absent {
				delete(client.state.volumes, before.Steps[0].Target.IntendedVolume)
			}
			tasks := &cleanupTaskClient{Client: client}
			deps.PVE = tasks
			got, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"n1"}, cleanupAttestedDecision(before.ID))
			if err != nil {
				t.Fatal(err)
			}
			if got.State != aj.Cleaned || len(client.state.volumes) != 0 {
				t.Fatal("cleanup did not prove absence")
			}
			if !reflect.DeepEqual(got.Steps[0], before.Steps[0]) {
				t.Fatal("original unknown submission rewritten")
			}
			if tasks.taskCalls != 1 {
				t.Fatal("independent task settlement missing")
			}
		})
	}
}

type interruptedUnknownCleanupClient struct {
	pve.Client
	base         *lifecycleFlowPVE
	interrupted  bool
	dropResponse bool
	retainVolume bool
}

func (c *interruptedUnknownCleanupClient) Storage() storage.Service {
	return interruptedUnknownCleanupStorage{Service: c.base.Storage(), client: c}
}
func (c *interruptedUnknownCleanupClient) StorageAuditVisibility(context.Context) error {
	if c.interrupted {
		return errors.New("post-delete visibility lost")
	}
	return nil
}

type interruptedUnknownCleanupStorage struct {
	storage.Service
	client *interruptedUnknownCleanupClient
}

func (s interruptedUnknownCleanupStorage) DeleteVolumeAsync(ctx context.Context, node, pool, volume string) (string, error) {
	saved := s.client.base.state.volumes[volume]
	result, err := s.Service.DeleteVolumeAsync(ctx, node, pool, volume)
	if s.client.retainVolume {
		s.client.base.state.volumes[volume] = saved
	}
	if s.client.dropResponse {
		return "", errors.New("delete response lost")
	}
	s.client.interrupted = true
	return result, err
}
func TestCleanupUnknownPersistentSubmissionCanFinishInterruptedDeletion(t *testing.T) {
	deps, client, journal, before := unknownDiskCleanupFixture(t)
	interrupted := &interruptedUnknownCleanupClient{Client: client, base: client}
	tasks := &cleanupTaskClient{Client: interrupted}
	deps.PVE = tasks
	if _, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"n1"}, cleanupAttestedDecision(before.ID)); err == nil {
		t.Fatal("interruption accepted")
	}
	if client.deletes != 1 {
		t.Fatal("first cleanup did not submit exactly one delete")
	}
	interrupted.interrupted = false
	got, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"n1"}, cleanupAttestedDecision(before.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got.State != aj.Cleaned || client.deletes != 1 || !reflect.DeepEqual(got.Steps[0], before.Steps[0]) {
		t.Fatal("cleanup replayed deletion or lost unknown history")
	}
}

func TestCleanupUnknownPersistentSubmissionRefusesUnsafeDisposition(t *testing.T) {
	for _, mode := range []string{"unfenced", "unsettled synchronous request", "active task", "incomplete task visibility", "attached disk", "incomplete storage visibility"} {
		t.Run(mode, func(t *testing.T) {
			deps, client, journal, record := unknownDiskCleanupFixture(t)
			tasks := &cleanupTaskClient{Client: client}
			deps.PVE = tasks
			decision := cleanupAttestedDecision(record.ID)
			switch mode {
			case "unfenced":
				decision.PreviousWriterFenced = false
			case "unsettled synchronous request":
				decision.RemoteTasksSettled = false
			case "active task":
				tasks.taskErr = errors.New("task running")
			case "incomplete task visibility":
				tasks.incomplete = true
			case "attached disk":
				client.state.configs[777] = map[string]any{"name": "workload", "scsi1": record.Steps[0].Target.IntendedVolume + ",serial=" + record.DiskToken + ",size=5G"}
			case "incomplete storage visibility":
				client.visibilityErr = errors.New("restricted visibility")
			}
			before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
			if _, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"n1"}, decision); err == nil {
				t.Fatal("unsafe disposition accepted")
			}
			if client.deletes != 0 || !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
				t.Fatal("refusal changed resources or authority")
			}
		})
	}
}
func TestCleanupUnknownPersistentSubmissionRejectsAlteredBirthIntent(t *testing.T) {
	_, _, _, record := unknownDiskCleanupFixture(t)
	for _, mode := range []string{"missing absence", "false absence", "wrong UUID", "wrong namespace", "wrong VMID", "wrong backing", "wrong storage", "wrong charge", "acquired charge", "second allocation", "non-first allocation", "foreign mutation", "external target", "unexpected task"} {
		t.Run(mode, func(t *testing.T) {
			changed := record
			changed.Steps = append([]aj.Step(nil), record.Steps...)
			step := changed.Steps[0]
			step.Charges = append([]aj.Charge(nil), step.Charges...)
			switch mode {
			case "missing absence":
				step.Parameters = nil
			case "false absence":
				step.Parameters = []byte(`{"version":1,"kind":"persistent_birth","absence_verified":false}`)
			case "wrong UUID":
				changed.ID = "12345678-1234-4234-8234-123456789abc"
			case "wrong namespace":
				changed.Namespace = "another namespace"
			case "wrong VMID":
				step.Target.VMID++
			case "wrong backing":
				step.Target.Backing = "other"
			case "wrong storage":
				step.Target.Storage = "other"
			case "wrong charge":
				step.Charges[0].PlannedBytes++
			case "acquired charge":
				step.Charges[0].AcquiredBytes++
			case "second allocation":
				other := step
				other.ID = "second"
				changed.Steps = append(changed.Steps, other)
			case "non-first allocation":
				other := step
				other.ID = "first"
				other.Kind = "other"
				changed.Steps = append([]aj.Step{other}, changed.Steps...)
			case "foreign mutation":
				step.Kind = "resize"
			case "external target":
				step.Target.External = true
			case "unexpected task":
				step.UPID = "UPID:n1:unknown"
			}
			if cleanupUnknownDiskAllocation(step, changed) {
				t.Fatal("altered birth intent accepted")
			}
		})
	}
}

func TestCleanupUnknownPersistentSubmissionCanFinishLostDeleteResponse(t *testing.T) {
	deps, client, journal, before := unknownDiskCleanupFixture(t)
	interrupted := &interruptedUnknownCleanupClient{Client: client, base: client, dropResponse: true}
	deps.PVE = &cleanupTaskClient{Client: interrupted}
	if _, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"n1"}, cleanupAttestedDecision(before.ID)); err == nil {
		t.Fatal("lost response accepted")
	}
	if client.deletes != 1 {
		t.Fatal("first cleanup did not submit exactly one delete")
	}
	got, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"n1"}, cleanupAttestedDecision(before.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got.State != aj.Cleaned || client.deletes != 1 || !reflect.DeepEqual(got.Steps[0], before.Steps[0]) {
		t.Fatal("cleanup replayed deletion or lost unknown history")
	}
}

func TestCleanupLostDeleteResponseCannotReplayWhileVolumeRemains(t *testing.T) {
	deps, client, journal, before := unknownDiskCleanupFixture(t)
	interrupted := &interruptedUnknownCleanupClient{Client: client, base: client, dropResponse: true, retainVolume: true}
	deps.PVE = &cleanupTaskClient{Client: interrupted}
	for i := 0; i < 2; i++ {
		if _, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"n1"}, cleanupAttestedDecision(before.ID)); err == nil {
			t.Fatal("remaining volume accepted as absent")
		}
	}
	if client.deletes != 1 || len(client.state.volumes) != 1 {
		t.Fatal("uncertain deletion replayed")
	}
	got, err := journal.Inspect(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == aj.Cleaned || !reflect.DeepEqual(got.Steps[0], before.Steps[0]) {
		t.Fatal("uncertain allocation history lost")
	}
}
