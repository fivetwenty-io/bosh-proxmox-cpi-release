package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const settleMigrationKind = "lifecycle_attach_disk_Nodes_CreateQemuMigrate"

// releaseNumber matches a dotted release number such as 1.2.3.
var releaseNumber = regexp.MustCompile(`\b\d+\.\d+\.\d+\b`)

// settleMigrationPVE is the flow fake with the migration task readback the
// SDK client serves. It answers every task read with task and taskErr, records
// the UPIDs it was asked for, and then runs afterTask, so a row can change the
// cluster between the task readback and the observation of the move.
type settleMigrationPVE struct {
	*lifecycleFlowPVE
	task      pve.StorageSettledTask
	taskErr   error
	taskReads []string
	afterTask func()
}

func (c *settleMigrationPVE) ObserveMigrationTaskSettlement(
	_ context.Context, upid string,
) (pve.StorageSettledTask, error) {
	c.taskReads = append(c.taskReads, upid)
	if c.afterTask != nil {
		c.afterTask()
	}
	return c.task, c.taskErr
}

// settleMigrationCase is the incident's record: the adopted mover's attach
// sent a migration from n2 to n1 that landed, but n2 answered the guard's
// readback of the mover's source config with a 500 that doesn't say the
// config is gone. The record requires reconciliation, and its migrate step is
// submitted with the task's UPID.
type settleMigrationCase struct {
	*adoptedMoverCase
	pve          *settleMigrationPVE
	step         aj.Step
	nodes        []string
	configWrites int
}

func newSettleMigrationCase(t *testing.T, pmxcfs bool) *settleMigrationCase {
	t.Helper()
	c := newAdoptedMoverCase(t)
	c.client.onNodeConfigRead = func(node string, vmid int) error {
		if node == "n2" && vmid == c.mover && c.client.vmNode(c.mover) == "n1" {
			return &sdkerrors.APIError{HTTPCode: 500, Message: "got timeout"}
		}
		return nil
	}
	_, attachErr := HandleAttachDisk(c.deps).Handle(c.ctx,
		[]json.RawMessage{planJSON(t, "777"), planJSON(t, c.cid)}, jsonrpc.Context{})
	if attachErr == nil || !strings.Contains(attachErr.Error(), "requires reconciliation") {
		t.Fatalf("attach_disk = %v, want the reconciliation error", attachErr)
	}
	c.client.onNodeConfigRead = nil
	c.client.pmxcfsConfigMissing = pmxcfs
	record, err := c.journal.Inspect(c.id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.ReconciliationRequired {
		t.Fatalf("the unobserved migration left the record %s, want %s", record.State, aj.ReconciliationRequired)
	}
	var step aj.Step
	for i := range record.Steps {
		if record.Steps[i].Kind == settleMigrationKind {
			step = record.Steps[i]
		}
	}
	if step.State != aj.Submitted || step.UPID != lifecycleFlowMigrateUPID("n2", c.mover) ||
		step.Target.Node != "n1" || step.Target.VMID != c.mover {
		t.Fatalf("migrate step = %+v, want the submitted migration of mover %d from n2 to n1", step, c.mover)
	}
	wrapper := &settleMigrationPVE{
		lifecycleFlowPVE: c.client,
		task:             pve.StorageSettledTask{UPID: step.UPID, Node: "n2", Status: "stopped", ExitStatus: "OK"},
	}
	c.deps.PVE = wrapper
	c.logged.Reset()
	s := &settleMigrationCase{adoptedMoverCase: c, pve: wrapper, step: step, nodes: []string{"n1", "n2"}}
	c.client.afterConfigWrite = func(int) { s.configWrites++ }
	return s
}

func (c *settleMigrationCase) decision() StorageAllocationDecision {
	return StorageAllocationDecision{
		Action: decisionActionSettleMigration, AllocationID: c.id, ExpectedCID: c.cid, DecisionID: "shield-settle",
		PreviousWriterFenced: true, RemoteTasksSettled: true,
		MigrationStepID: c.step.ID, MigrationTaskUPID: c.step.UPID,
	}
}

func (c *settleMigrationCase) settle(decision StorageAllocationDecision) (aj.Record, error) {
	return SettleStorageAllocationMigration(c.ctx, c.deps, c.journal, c.nodes, decision)
}

func (c *settleMigrationCase) record(t *testing.T) aj.Record {
	t.Helper()
	record, err := c.journal.Inspect(c.id)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// otherVolume is a volume of the mover other than the one the migrate step
// recorded, on storage, or on the step's own storage when storage is empty.
func (c *settleMigrationCase) otherVolume(storage string) string {
	if storage == "" {
		storage = c.step.Target.Storage
	}
	return fmt.Sprintf("%s:%d/vm-%d-disk-9.raw", storage, c.mover, c.mover)
}

// mutations counts every change the fake applied to PVE.
func (c *settleMigrationCase) mutations() [7]int {
	return [7]int{
		c.client.migrations, c.client.deletes, c.client.moveCalls, len(c.client.protectionWrites),
		len(c.client.destroyed), c.configWrites, len(c.client.state.pools),
	}
}

// appendSubmittedStep records a step of kind that the active attempt planned
// and PVE answered with upid, keeping the record in reconciliation_required,
// the way the journal holds a step a guard could not observe.
func (c *settleMigrationCase) appendSubmittedStep(
	t *testing.T, kind string, target aj.Target, charges []inv.ChargeRecord, upid string,
) aj.Step {
	t.Helper()
	handle, err := c.journal.Acquire(c.ctx, c.id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	}()
	id, err := storageMutationIntent(handle, kind, target, charges)
	if err != nil {
		t.Fatal(err)
	}
	if upid == "" {
		step, _ := recordStep(handle.Record(), id)
		return step
	}
	record := handle.Record()
	for i := range record.Steps {
		if record.Steps[i].ID == id {
			record.Steps[i].State = aj.Submitted
			record.Steps[i].UPID = upid
		}
	}
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	step, _ := recordStep(handle.Record(), id)
	return step
}

// settleMigrationWarnings returns the WARN lines in which settle-migration
// says it settled a migration step.
func settleMigrationWarnings(logged string) []string {
	var lines []string
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, `"level":"WARN"`) && strings.Contains(line, "settle-migration settled") {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestSettleMigrationSettlesTheIncidentStep settles the incident's submitted
// migrate step from a readback of its finished task and of the mover where
// the migration left it. The step becomes observed with the volume it moved,
// the record stays in reconciliation_required with a reason that names the
// decision, nothing in PVE changes, and the Director's next attach finishes
// the operation and returns the disk.
func TestSettleMigrationSettlesTheIncidentStep(t *testing.T) {
	for _, mode := range lifecycleFlowConfigMissingModes {
		t.Run(mode.name, func(t *testing.T) {
			c := newSettleMigrationCase(t, mode.pmxcfs)
			before := c.record(t)
			mutations := c.mutations()

			settled, err := c.settle(c.decision())
			if err != nil {
				t.Fatalf("settle-migration = %s", StorageAllocationDecisionFailure(err))
			}
			if c.mutations() != mutations {
				t.Fatalf("settle-migration changed PVE: %v, was %v", c.mutations(), mutations)
			}
			if !reflect.DeepEqual(c.pve.taskReads, []string{c.step.UPID}) {
				t.Fatalf("task reads = %v, want one read of %s", c.pve.taskReads, c.step.UPID)
			}
			record := c.record(t)
			if !reflect.DeepEqual(record, settled) {
				t.Fatalf("returned record differs from the journal's")
			}
			assertMigrationSettled(t, c, before, record)

			c.deps.PVE = c.client
			_, attachErr := HandleAttachDisk(c.deps).Handle(c.ctx,
				[]json.RawMessage{planJSON(t, "777"), planJSON(t, c.cid)}, jsonrpc.Context{})
			if attachErr != nil {
				t.Fatalf("the attach after settle-migration = %s", log.ScrubMessage(attachErr.Error()))
			}
			assertReturnedRecord(t, "attach after settle-migration", c.record(t))
			if slots := parkerSlotsCarrying(c.client, 777, record.DiskToken); len(slots) != 1 {
				t.Fatalf("VM 777 carries the disk's serial on %v, want exactly one slot", slots)
			}
		})
	}
}

// assertMigrationSettled checks that settle-migration marked only the
// case's migrate step observed, with the volume the step recorded, kept the
// record in reconciliation_required with the decision's reason, and logged
// one WARN that carries the evidence.
func assertMigrationSettled(t *testing.T, c *settleMigrationCase, before, record aj.Record) {
	t.Helper()
	if record.State != aj.ReconciliationRequired || record.Reason != "migration settled by decision shield-settle" {
		t.Fatalf("record = %s (%q), want reconciliation_required with the decision's reason",
			record.State, record.Reason)
	}
	if len(record.Steps) != len(before.Steps) {
		t.Fatalf("settle-migration added steps")
	}
	assertMigrationSettlementEvidence(t, c, before, record)
	for i := range record.Steps {
		got, was := record.Steps[i], before.Steps[i]
		if got.ID != c.step.ID {
			if !reflect.DeepEqual(got, was) {
				t.Errorf("step %s changed: %+v, was %+v", got.ID, got, was)
			}
			continue
		}
		if got.State != aj.Observed || !reflect.DeepEqual(got.VolIDs, []string{c.step.Target.IntendedVolume}) {
			t.Errorf("migrate step = %s with %v, want observed with %s",
				got.State, got.VolIDs, c.step.Target.IntendedVolume)
		}
		if got.UPID != was.UPID || !reflect.DeepEqual(got.Target, was.Target) {
			t.Errorf("migrate step identity changed: %+v, was %+v", got, was)
		}
	}
	lines := settleMigrationWarnings(c.logged.String())
	if len(lines) != 1 {
		t.Fatalf("settle-migration logged %d WARNs, want one: %s", len(lines), c.logged.String())
	}
	for _, want := range []string{
		`"allocation_id":"` + c.id + `"`, `"decision_id":"shield-settle"`, `"step_id":"` + c.step.ID + `"`,
		`"kind":"` + settleMigrationKind + `"`, `"source_node":"n2"`, `"target_node":"n1"`, `"slot":"scsi0"`,
		`"volume":"` + c.step.Target.IntendedVolume + `"`, `"serial":"` + record.DiskToken + `"`,
		`"upid":"` + c.step.UPID + `"`, `"exit_status":"OK"`,
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the WARN lacks %s: %s", want, lines[0])
		}
	}
}

// TestSettleMigrationRefusesWithoutProof refuses each way the decision, the
// record, the task, or the cluster can fail to prove that the step's
// migration finished and left the disk where the step says. Every refusal
// names its reason and leaves the journal and PVE as they were.
func TestSettleMigrationRefusesWithoutProof(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, c *settleMigrationCase, d *StorageAllocationDecision)
		want  string
	}{
		{name: "writer not fenced", want: "--previous-writer-fenced",
			setup: func(_ *testing.T, _ *settleMigrationCase, d *StorageAllocationDecision) {
				d.PreviousWriterFenced = false
			}},
		{name: "tasks not settled", want: "--remote-tasks-settled",
			setup: func(_ *testing.T, _ *settleMigrationCase, d *StorageAllocationDecision) {
				d.RemoteTasksSettled = false
			}},
		{name: "other action", want: "unsupported allocation decision",
			setup: func(_ *testing.T, _ *settleMigrationCase, d *StorageAllocationDecision) { d.Action = "adopt" }},
		{name: "no decision reference", want: "decision reference",
			setup: func(_ *testing.T, _ *settleMigrationCase, d *StorageAllocationDecision) { d.DecisionID = " " }},
		{name: "no step", want: "--step-id",
			setup: func(_ *testing.T, _ *settleMigrationCase, d *StorageAllocationDecision) { d.MigrationStepID = "" }},
		{name: "step with a control character", want: "--step-id",
			setup: func(_ *testing.T, _ *settleMigrationCase, d *StorageAllocationDecision) {
				d.MigrationStepID += "\n"
			}},
		{name: "unknown step", want: "has no step attempt-0-step-999",
			setup: func(_ *testing.T, _ *settleMigrationCase, d *StorageAllocationDecision) {
				d.MigrationStepID = "attempt-0-step-999"
			}},
		{name: "no expected CID", want: "--expected-cid",
			setup: func(_ *testing.T, _ *settleMigrationCase, d *StorageAllocationDecision) { d.ExpectedCID = "" }},
		{name: "other CID", want: "with the exact CID",
			setup: func(_ *testing.T, _ *settleMigrationCase, d *StorageAllocationDecision) {
				d.ExpectedCID = "pvd-other"
			}},
		{name: "malformed UPID", want: "malformed",
			setup: func(_ *testing.T, _ *settleMigrationCase, d *StorageAllocationDecision) {
				d.MigrationTaskUPID = "UPID:n2:qmigrate"
			}},
		{name: "UPID of another step", want: "does not match the UPID",
			setup: func(_ *testing.T, c *settleMigrationCase, d *StorageAllocationDecision) {
				d.MigrationTaskUPID = strings.Replace(c.step.UPID, "0000A1B2", "0000FFFF", 1)
			}},
		{name: "not a migrate step", want: "is not a cross-node migration step",
			setup: func(t *testing.T, c *settleMigrationCase, d *StorageAllocationDecision) {
				d.MigrationStepID = c.record(t).Steps[0].ID
			}},
		{name: "planned migrate step", want: "is planned, not submitted",
			setup: func(t *testing.T, c *settleMigrationCase, d *StorageAllocationDecision) {
				d.MigrationStepID = c.appendSubmittedStep(t, settleMigrationKind, c.step.Target, nil, "").ID
			}},
		{name: "not a qmigrate task", want: "not a qmigrate task",
			setup: func(t *testing.T, c *settleMigrationCase, d *StorageAllocationDecision) {
				upid := strings.Replace(c.step.UPID, "qmigrate", "qmclone", 1)
				d.MigrationStepID = c.appendSubmittedStep(t, settleMigrationKind, c.step.Target, nil, upid).ID
				d.MigrationTaskUPID = upid
			}},
		{name: "task of another VM", want: "but step",
			setup: func(t *testing.T, c *settleMigrationCase, d *StorageAllocationDecision) {
				upid := lifecycleFlowMigrateUPID("n2", c.mover+1)
				d.MigrationStepID = c.appendSubmittedStep(t, settleMigrationKind, c.step.Target, nil, upid).ID
				d.MigrationTaskUPID = upid
			}},
		{name: "task ran on the target", want: "ran on node n1, the step's target",
			setup: func(t *testing.T, c *settleMigrationCase, d *StorageAllocationDecision) {
				upid := lifecycleFlowMigrateUPID("n1", c.mover)
				d.MigrationStepID = c.appendSubmittedStep(t, settleMigrationKind, c.step.Target, nil, upid).ID
				d.MigrationTaskUPID = upid
			}},
		{name: "copy-local migration", want: "settle-migration does not settle a copy-local migration",
			setup: func(t *testing.T, c *settleMigrationCase, d *StorageAllocationDecision) {
				charge := inv.ChargeRecord{
					Charge:      inv.Charge{ID: "migrate", Node: "n1", StorageID: "b", Bytes: 1 << 30},
					CapacityKey: c.step.Target.Backing,
				}
				upid := strings.Replace(c.step.UPID, "0000A1B2", "0000A1B3", 1)
				step := c.appendSubmittedStep(t, settleMigrationKind, c.step.Target, []inv.ChargeRecord{charge}, upid)
				d.MigrationStepID, d.MigrationTaskUPID = step.ID, upid
			}},
		{name: "another unsettled step", want: "is planned",
			setup: func(t *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				target := c.step.Target
				target.VMID, target.Node = 777, "n1"
				c.appendSubmittedStep(t, "lifecycle_attach_disk_Nodes_UpdateQemuConfig", target, nil, "")
			}},
		{name: "source node outside the cluster nodes", want: "nodes this decision may read",
			setup: func(_ *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				c.nodes = []string{"n1"}
			}},
		{name: "cluster identity unread", want: "verified live cluster continuity",
			setup: func(_ *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				c.client.certificateErr = map[string]error{"n2": errors.New("certificate read failed")}
			}},
		{name: "another namespace", want: "namespace",
			setup: func(_ *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				config := *c.deps.Config
				config.StoragePlacementNamespace = "other"
				c.deps.Config = &config
			}},
		{name: "a second holder of the disk", want: "audit conflicts",
			setup: func(_ *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				c.client.state.configs[778] = map[string]any{
					"name": "other", "digest": "1", "scsi0": c.step.Target.IntendedVolume + ",size=5G",
				}
			}},
		{name: "task still running", want: "not proven stopped with exit status OK",
			setup: func(_ *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				c.pve.task.Status = "running"
			}},
		{name: "task failed", want: "not proven stopped with exit status OK",
			setup: func(_ *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				c.pve.task.ExitStatus = "migration aborted"
			}},
		{name: "task unreadable", want: "status read failed",
			setup: func(_ *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				c.pve.taskErr = errors.New("status read failed")
			}},
		{name: "mover still on the source", want: "migration source holder absence not observed",
			setup: func(_ *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				c.pve.afterTask = func() { c.client.vmNodes[c.mover] = "n2" }
			}},
		{name: "probe places the mover on the source", want: "not found on node n1",
			setup: func(_ *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				reads := 0
				c.pve.afterTask = func() {
					c.client.vmNodes[c.mover] = "n2"
					c.client.onNodeConfigRead = func(node string, vmid int) error {
						if node == "n2" && vmid == c.mover {
							reads++
							if reads == 1 {
								return lifecycleConfigMissingAnswer(node, vmid)
							}
						}
						return nil
					}
				}
			}},
		{name: "source read times out", want: "migration source holder absence not observed",
			setup: func(_ *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				c.pve.afterTask = func() { c.client.onNodeConfigRead = flowConfigTimeout(c.mover) }
			}},
		{name: "mover deleted", want: "not found on node n1",
			setup: func(_ *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				c.pve.afterTask = func() { delete(c.client.state.configs, c.mover) }
			}},
		{name: "target holds the migrate lock", want: "still holds the migrate lock",
			setup: func(_ *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				c.pve.afterTask = func() { c.client.state.configs[c.mover]["lock"] = "migrate" }
			}},
		{name: "serial on no slot", want: "migration target disk identity missing",
			setup: func(_ *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				c.pve.afterTask = func() { c.client.state.configs[c.mover]["scsi0"] = c.step.Target.IntendedVolume }
			}},
		{name: "serial on two slots", want: "migration target disk identity ambiguous",
			setup: func(t *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				serial := c.record(t).DiskToken
				c.pve.afterTask = func() {
					c.client.state.configs[c.mover]["scsi1"] = c.otherVolume("") + ",serial=" + serial
				}
			}},
		{name: "slot on another backing", want: "migration target backing differs",
			setup: func(t *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				serial := c.record(t).DiskToken
				other := map[string]string{"a": "b", "b": "a"}[c.step.Target.Storage]
				c.pve.afterTask = func() {
					c.client.state.configs[c.mover]["scsi0"] = c.otherVolume(other) + ",serial=" + serial
				}
			}},
		{name: "slot names another volume", want: "carries volume",
			setup: func(t *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				serial := c.record(t).DiskToken
				c.pve.afterTask = func() {
					c.client.state.configs[c.mover]["scsi0"] = c.otherVolume("") + ",serial=" + serial
				}
			}},
		{name: "volume missing from its storage", want: "is not in its storage's content listing on node n1",
			setup: func(_ *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				c.pve.afterTask = func() { delete(c.client.state.volumes, c.step.Target.IntendedVolume) }
			}},
		{name: "storage listing fails", want: "failed with listing_http_403",
			setup: func(_ *testing.T, c *settleMigrationCase, _ *StorageAllocationDecision) {
				c.pve.afterTask = func() {
					c.client.state.existsErr = &sdkerrors.APIError{HTTPCode: 403, Message: "permission denied"}
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newSettleMigrationCase(t, true)
			decision := c.decision()
			tc.setup(t, c, &decision)
			before := c.record(t)
			mutations := c.mutations()

			settled, err := c.settle(decision)
			if err == nil {
				t.Fatalf("settle-migration settled the step: %+v", settled)
			}
			text := StorageAllocationDecisionFailure(err)
			if !strings.Contains(text, tc.want) {
				t.Fatalf("settle-migration refusal = %q (%v), want it to name %q", text, err, tc.want)
			}
			if releaseNumber.MatchString(text) {
				t.Fatalf("settle-migration refusal = %q, want no release number in it", text)
			}
			if !reflect.DeepEqual(c.record(t), before) {
				t.Fatalf("the refusal changed the journal")
			}
			if c.mutations() != mutations {
				t.Fatalf("the refusal changed PVE: %v, was %v", c.mutations(), mutations)
			}
			if lines := settleMigrationWarnings(c.logged.String()); len(lines) != 0 {
				t.Fatalf("a refusal logged a settlement: %v", lines)
			}
		})
	}
}

// flowConfigTimeout fails every read of vmid's config on n2 with a 500 that
// doesn't say the config is gone.
func flowConfigTimeout(vmid int) func(string, int) error {
	return func(node string, id int) error {
		if node == "n2" && id == vmid {
			return &sdkerrors.APIError{HTTPCode: 500, Message: "got timeout"}
		}
		return nil
	}
}

// TestSettleMigrationRefusesARecordThatIsNotAnUnsettledDisk refuses a disk
// record that doesn't require reconciliation and a record that isn't a disk.
func TestSettleMigrationRefusesARecordThatIsNotAnUnsettledDisk(t *testing.T) {
	c := newAdoptedMoverCase(t)
	record, err := c.journal.Inspect(c.id)
	if err != nil {
		t.Fatal(err)
	}
	upid := lifecycleFlowMigrateUPID("n2", c.mover)
	decision := StorageAllocationDecision{
		Action: decisionActionSettleMigration, AllocationID: c.id, ExpectedCID: c.cid, DecisionID: "settle",
		PreviousWriterFenced: true, RemoteTasksSettled: true, MigrationStepID: record.Steps[0].ID,
		MigrationTaskUPID: upid,
	}
	nodes := []string{"n1", "n2"}
	if _, err := SettleStorageAllocationMigration(c.ctx, c.deps, c.journal, nodes, decision); err == nil ||
		!strings.Contains(StorageAllocationDecisionFailure(err), "reconciliation_required") {
		t.Fatalf("settling a returned disk = %v, want a refusal naming reconciliation_required", err)
	}
	if after, _ := c.journal.Inspect(c.id); !reflect.DeepEqual(after, record) {
		t.Fatal("the refusal changed the returned record")
	}

	handle, err := c.journal.AcquireVM(c.ctx, "vm-agent", record.Intent)
	if err != nil {
		t.Fatal(err)
	}
	vmID := handle.Record().ID
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	decision.AllocationID = vmID
	if _, err := SettleStorageAllocationMigration(c.ctx, c.deps, c.journal, nodes, decision); err == nil ||
		!strings.Contains(StorageAllocationDecisionFailure(err), "disk record") {
		t.Fatalf("settling a VM record = %v, want a refusal naming the disk record", err)
	}
}

// TestSettleMigrationRefusesAfterTheGuardDeniedAWrite runs settlement with a
// guard that has already denied a PVE write, the way it would if anything on
// the settlement's path tried to change PVE. The settlement refuses and saves
// nothing.
func TestSettleMigrationRefusesAfterTheGuardDeniedAWrite(t *testing.T) {
	c := newSettleMigrationCase(t, true)
	before := c.record(t)
	mutations := c.mutations()
	raw := c.deps.PVE
	guard, denied, err := newDecisionDenyGuard(raw)
	if err != nil {
		t.Fatal(err)
	}
	deps := c.deps
	deps.PVE = guard.Client()
	_, migrateErr := deps.PVE.Nodes().CreateQemuMigrate(c.ctx, "n1", strconv.Itoa(c.mover),
		&nodes.CreateQemuMigrateParams{Target: "n2"})
	if migrateErr == nil {
		t.Fatal("the decision guard let a migration through")
	}
	_, err = settleStorageAllocationMigration(c.ctx, deps, raw, guard, denied, c.journal, c.nodes, c.decision())
	if err == nil || !strings.Contains(StorageAllocationDecisionFailure(err), "cannot mutate PVE") {
		t.Fatalf("settlement after a denied write = %v, want the guard's refusal", err)
	}
	if !reflect.DeepEqual(c.record(t), before) || c.mutations() != mutations {
		t.Fatal("settlement after a denied write changed the journal or PVE")
	}
}

// assertMigrationSettlementEvidence checks that settle-migration appended one
// verification to the record, which proves nothing about ownership or absence
// and carries the settlement's evidence, and that the evidence reads back.
func assertMigrationSettlementEvidence(t *testing.T, c *settleMigrationCase, before, record aj.Record) {
	t.Helper()
	n := len(before.Verifications)
	if len(record.Verifications) != n+1 || !reflect.DeepEqual(record.Verifications[:n], before.Verifications) {
		t.Fatalf("verifications = %+v, want the earlier ones and one more", record.Verifications)
	}
	v := record.Verifications[n]
	if !v.Complete || v.OwnershipVerified || v.AbsenceVerified || v.VMAbsenceVerified ||
		v.ArtifactDispositionVerified || v.EvidenceID == "" || v.EvidenceJSON == "" {
		t.Fatalf("settlement verification = %+v, want complete evidence that verifies nothing else", v)
	}
	evidence, err := MigrationSettlementEvidenceOf(record, c.step.ID)
	if err != nil {
		t.Fatalf("MigrationSettlementEvidenceOf: %v", err)
	}
	want := MigrationSettlementEvidence{
		Scope: migrationSettlementScope, AllocationID: c.id, DecisionID: "shield-settle", StepID: c.step.ID,
		Kind: settleMigrationKind, UPID: c.step.UPID, TaskStatus: "stopped", ExitStatus: "OK", SourceNode: "n2",
		TargetNode: "n1", VMID: c.mover, Slot: "scsi0", Volume: c.step.Target.IntendedVolume,
		Serial: record.DiskToken, ObservedAt: evidence.ObservedAt,
	}
	if evidence.ObservedAt == "" || !reflect.DeepEqual(evidence, want) {
		t.Fatalf("settlement evidence = %+v, want %+v", evidence, want)
	}
	if _, err := MigrationSettlementEvidenceOf(record, c.step.ID+"-other"); err == nil {
		t.Fatal("MigrationSettlementEvidenceOf found evidence for a step the record never settled")
	}
	if _, err := MigrationSettlementEvidenceOf(before, c.step.ID); err == nil {
		t.Fatal("MigrationSettlementEvidenceOf found evidence before the settlement")
	}
}

// TestSettleMigrationRecordsItsOutcome counts a settlement as settled and a
// refused one as rejected, among the storage reconciliation outcomes.
func TestSettleMigrationRecordsItsOutcome(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	metrics, err := NewStoragePlacementMetrics(provider.Meter("test"))
	if err != nil {
		t.Fatal(err)
	}
	refused := newSettleMigrationCase(t, true)
	refused.deps.StorageMetrics = metrics
	decision := refused.decision()
	decision.PreviousWriterFenced = false
	if _, err := refused.settle(decision); err == nil {
		t.Fatal("settle-migration settled without the fencing attestation")
	}
	settled := newSettleMigrationCase(t, true)
	settled.deps.StorageMetrics = metrics
	if _, err := settled.settle(settled.decision()); err != nil {
		t.Fatalf("settle-migration = %s", StorageAllocationDecisionFailure(err))
	}

	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || m.Name != "cpi.storage.reconciliations" {
				continue
			}
			for _, point := range sum.DataPoints {
				outcome, _ := point.Attributes.Value("storage.reconciliation.outcome")
				counts[outcome.AsString()] += point.Value
			}
		}
	}
	if want := map[string]int64{"rejected": 1, "settled": 1}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("reconciliation outcomes = %v, want %v", counts, want)
	}
}
