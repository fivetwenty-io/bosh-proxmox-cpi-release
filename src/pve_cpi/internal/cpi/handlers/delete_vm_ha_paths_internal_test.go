package handlers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// --- HA deferral marking ---

func TestDeferHAMarksOnceAndKeepsTheError(t *testing.T) {
	if deferHA(nil) != nil {
		t.Fatal("deferHA(nil) must stay nil")
	}
	inner := cpierrors.Retriable("delete_vm: wait")
	marked := deferHA(inner)
	if !isWholeHADeferral(marked) || !okToRetryCPIError(marked) || marked.Error() != inner.Error() {
		t.Fatalf("the deferral must keep the retriable error and its text: %v", marked)
	}
	var outer *haDeferral
	if again := deferHA(marked); !errors.As(again, &outer) || errors.As(outer.err, new(*haDeferral)) {
		t.Fatal("deferHA must not wrap a deferral twice")
	}
	if !isWholeHADeferral(storageCleanupFailure("vm_ha", marked)) {
		t.Fatal("a cleanup stage around a deferral is still a whole deferral")
	}
	if isWholeHADeferral(errors.Join(marked, errors.New("other"))) {
		t.Fatal("a deferral joined with another failure is not a whole deferral")
	}
	if isWholeHADeferral(inner) {
		t.Fatal("an unmarked error is not a deferral")
	}
}

// --- the gate under a short deadline, an unmarked entry node, and an unreadable config ---

// TestGateHADeleteKeepsTheStopAndDestroyReserveOfTheDeadline ends the motion
// wait early enough that the stop and the destroy still have their time.
func TestGateHADeleteKeepsTheStopAndDestroyReserveOfTheDeadline(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "freeze")
	f := newHADeleteFixture(t, scene, 600)
	reserve := haDeleteStopReserve + haDeleteDestroyReserve()
	ctx, cancel := context.WithTimeout(t.Context(), reserve+2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := gateHADelete(ctx, f.deps, "delete_vm", 101, "pve-node1", f.logger)
	requireHADeferral(t, err, "the delete's deadline leaves no more time to wait for the HA motion")
	if !isWholeHADeferral(err) {
		t.Fatalf("the gate's refusal must be an HA deferral: %v", err)
	}
	if waited := time.Since(start); waited > 30*time.Second {
		t.Fatalf("the gate waited %s into the stop and destroy reserve", waited)
	}
	if deadline, _ := ctx.Deadline(); time.Until(deadline) < reserve-5*time.Second {
		t.Fatal("the gate spent the reserve the stop and the destroy need")
	}
}

func TestHaDeleteStopBudgetLeavesTheDestroyReserve(t *testing.T) {
	deps := Deps{Config: &config.CPIConfig{}}
	if budget, err := haDeleteStopBudget(context.Background(), deps, "delete_vm", 101); err != nil ||
		budget < haMotionWait(deps)-time.Second || budget > haMotionWait(deps) {
		t.Fatalf("without a deadline the stop gets the motion wait, got %s, %v", budget, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), haDeleteDestroyReserve()/2)
	defer cancel()
	if _, err := haDeleteStopBudget(ctx, deps, "delete_vm", 101); err == nil || !okToRetryCPIError(err) {
		t.Fatalf("a deadline inside the destroy reserve must defer the stop, got %v", err)
	}
}

// TestGateHADeleteDefersWhenClusterStatusMarksNoLocalNode names the missing
// local flag and writes nothing.
func TestGateHADeleteDefersWhenClusterStatusMarksNoLocalNode(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	scene.NoLocal = true
	f := newHADeleteFixture(t, scene, 1)
	_, err := f.gate(t, "pve-node1")
	requireHADeferral(t, err, "cluster status marks no local node")
	if events := scene.EventsSnapshot(); len(events) != 0 {
		t.Fatalf("an unmarked entry node must write nothing, got %v", events)
	}
}

func TestHADeferralForUnreadableConfigNamesTheHAState(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(101, "pve-node2", "started", "fence")
	deadNode(scene)
	f := newHADeleteFixture(t, scene, 1)
	err := haDeferralForUnreadableConfig(t.Context(), f.deps, "delete_vm", 101, "pve-node2", f.logger)
	requireHADeferral(t, err, "fenced")
	if !isWholeHADeferral(err) {
		t.Fatalf("the refusal must be an HA deferral: %v", err)
	}

	healthy := NewHADeleteScene()
	healthy.Manage(101, "pve-node1", "started", "started")
	g := newHADeleteFixture(t, healthy, 1)
	if err := haDeferralForUnreadableConfig(t.Context(), g.deps, "delete_vm", 101, "pve-node1", g.logger); err != nil {
		t.Fatalf("an HA state that lets the delete proceed leaves the config error to the caller, got %v", err)
	}
}

// --- skiplock and the migrate lock ---

func TestDestroyVMWithRecovery_MigrateLockNeverSkiplocks(t *testing.T) {
	errs := make([]error, 0, rollbackDestroyMaxAttempts)
	for range rollbackDestroyMaxAttempts {
		errs = append(errs, lock500("migrate"))
	}
	nodes := &lrNodesStub{deleteErrs: errs}
	deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: nodes, qemu: &lrQEMUStub{}}, Logger: log.NewNopLogger()}
	_, _, err := destroyStepOnlyBypassingGates(crhCtx(), deps)
	requireHADeferral(t, err, "holds a migrate lock", "never destroys a VM with skiplock")
	for i, call := range nodes.deleteCalls {
		if call.Skiplock != nil && *call.Skiplock {
			t.Fatalf("DeleteQemu call %d carried skiplock while the VM held a migrate lock", i)
		}
	}
}

// TestRetryDestroyWithSkiplock_ReadsTheLockTheRefusalOmits reads the config
// when the refusal does not name the lock, and sends no skiplock for a
// migrate lock or a config it cannot read.
func TestRetryDestroyWithSkiplock_ReadsTheLockTheRefusalOmits(t *testing.T) {
	refusal := errors.New("unable to destroy VM 100: VM is locked ()")
	for name, tc := range map[string]struct {
		config    func(int) (map[string]any, error)
		migration bool
		skiplock  bool
	}{
		"migrate":    {config: func(int) (map[string]any, error) { return map[string]any{"lock": "migrate"}, nil }, migration: true},
		"unreadable": {config: func(int) (map[string]any, error) { return nil, errors.New("read failed") }},
		"backup":     {config: func(int) (map[string]any, error) { return map[string]any{"lock": "backup"}, nil }, skiplock: true},
	} {
		t.Run(name, func(t *testing.T) {
			nodes := &lrNodesStub{deleteErrs: []error{nil}}
			q := &lrQEMUStub{configFn: tc.config}
			deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}
			_, err := retryDestroyWithSkiplock(crhCtx(), deps, "pve01", 100, true, false, refusal, log.NewNopLogger())
			var migrating *skiplockMigrationRefusal
			if got := errors.As(err, &migrating); got != tc.migration {
				t.Fatalf("migration refusal = %v, want %v (err %v)", got, tc.migration, err)
			}
			if tc.migration && err.Error() != refusal.Error() {
				t.Fatalf("the refusal must keep PVE's text for the lock-clear branch, got %q", err)
			}
			if sent := len(nodes.deleteCalls) == 1; sent != tc.skiplock {
				t.Fatalf("skiplock destroy sent = %v, want %v", sent, tc.skiplock)
			}
		})
	}
}

func TestRefuseSkiplockDuringMigration(t *testing.T) {
	for name, tc := range map[string]struct {
		config func(int) (map[string]any, error)
		want   string
	}{
		"migrate":    {config: func(int) (map[string]any, error) { return map[string]any{"lock": "migrate"}, nil }, want: "migrate lock"},
		"unreadable": {config: func(int) (map[string]any, error) { return nil, errors.New("read failed") }, want: "read the config lock"},
		"gone": {config: func(int) (map[string]any, error) {
			return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "Configuration file does not exist"}
		}},
		"unlocked": {config: func(int) (map[string]any, error) { return map[string]any{}, nil }},
	} {
		t.Run(name, func(t *testing.T) {
			deps := Deps{PVE: &lrClient{nodes: &lrNodesStub{}, qemu: &lrQEMUStub{configFn: tc.config}}}
			err := refuseSkiplockDuringMigration(crhCtx(), deps, "delete_vm: fast-path", "pve01", 100, log.NewNopLogger())
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want no refusal, got %v", err)
				}
				return
			}
			requireHADeferral(t, err, tc.want)
		})
	}
}

// --- the straggler sweep ---

// TestSweepTeardownGateReadsHAOnceWithoutWaiting pins the sweep's HA gate:
// one read, no wait, and only a state the table lets proceed on the located
// node goes on. An HA-managed VM that may proceed goes on too, because the
// teardown then stops it through PVE HA.
func TestSweepTeardownGateReadsHAOnceWithoutWaiting(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(*HADeleteScene)
		node  string
		want  bool
	}{
		"not HA-managed": {setup: func(s *HADeleteScene) { s.Place(101, "pve-node1") }, node: "pve-node1", want: true},
		"HA-managed":     {setup: func(s *HADeleteScene) { s.Manage(101, "pve-node1", "started", "started") }, node: "pve-node1", want: true},
		"in fence":       {setup: func(s *HADeleteScene) { s.Manage(101, "pve-node2", "started", "fence"); deadNode(s) }, node: "pve-node2"},
		"unreadable":     {setup: func(s *HADeleteScene) { s.Place(101, "pve-node1"); s.NoLocal = true }, node: "pve-node1"},
		"HA in motion":   {setup: func(s *HADeleteScene) { s.Manage(101, "pve-node1", "started", "migrate") }, node: "pve-node1"},
		"ignored by HA":  {setup: func(s *HADeleteScene) { s.Manage(101, "pve-node1", "ignored", "") }, node: "pve-node1", want: true},
		"stopped by HA":  {setup: func(s *HADeleteScene) { s.Manage(101, "pve-node1", "stopped", "stopped") }, node: "pve-node1", want: true},
		"offline member": {setup: func(s *HADeleteScene) { s.Place(101, "pve-node2"); deadNode(s) }, node: "pve-node2"},
		"another node":   {setup: func(s *HADeleteScene) { s.Manage(101, "pve-node1", "started", "started") }, node: "pve-node2"},
	} {
		t.Run(name, func(t *testing.T) {
			scene := NewHADeleteScene()
			tc.setup(scene)
			f := newHADeleteFixture(t, scene, 1)
			teardown := &vmTeardown{caller: "deferred teardown sweep", vmid: 101, node: tc.node, logger: f.logger, gate: teardownGateNoWait}
			err := teardown.settleHAGate(t.Context(), f.deps)
			if got := err == nil; got != tc.want {
				t.Fatalf("sweep gate allowed = %v (err %v), want %v", got, err, tc.want)
			}
			if err != nil && !isWholeHADeferral(err) {
				t.Fatalf("a sweep gate refusal must be an HA deferral, got %T %v", err, err)
			}
			if events := scene.EventsSnapshot(); len(events) != 0 {
				t.Fatalf("the sweep's HA gate must write nothing, got %v", events)
			}
		})
	}
}

// --- the create rollback ---

// rollbackHAClient serves the rollback's HA reads, stop, and permissions from
// a scene and its config, tag, and destroy calls from the keep-failed fakes.
type rollbackHAClient struct {
	*kfClient
	scene *HADeleteScene
}

func (c *rollbackHAClient) Cluster() cluster.Service {
	return &HASceneCluster{Scene: c.scene, UseIndex: true}
}
func (c *rollbackHAClient) Nodes() sdknodes.Service {
	return &HASceneNodes{Service: c.kfClient.Nodes(), Scene: c.scene}
}
func (c *rollbackHAClient) QEMU() qemu.Service {
	return &HASceneQEMU{Service: c.kfClient.QEMU(), Scene: c.scene}
}
func (c *rollbackHAClient) Tasks() tasks.Service { return haSceneTasks{} }
func (c *rollbackHAClient) EffectivePermissions(context.Context, string) (map[string]bool, error) {
	return c.scene.perms()
}

func rollbackHADeps(scene *HADeleteScene, nodes *kfNodes, q *kfQEMU) Deps {
	motion := 1
	cfg := naPinConfig(map[string][]string{"z1": {"pve-node1"}})
	cfg.HA = &config.HAConfig{MotionWaitSec: &motion}
	return Deps{
		Config: cfg,
		PVE:    &rollbackHAClient{kfClient: &kfClient{nodes: nodes, qemu: q}, scene: scene},
		Logger: log.NewNopLogger(),
	}
}

// TestCleanupVM_RemovesNodeAffinityPinWhenEnabled stops an HA-managed VM
// through PVE HA, destroys it, and then removes its pin and HA resource.
func TestCleanupVM_RemovesNodeAffinityPinWhenEnabled(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Manage(100, "pve-node1", "started", "started")
	scene.Rules["bosh-na-100"] = map[string]any{"type": "node-affinity", "resources": "vm:100", "nodes": "pve-node1"}
	nodes, q := &kfNodes{}, &kfQEMU{}
	deps := rollbackHADeps(scene, nodes, q)

	cleanupVM(context.Background(), deps, "pve-node1", 100, nil, log.NewNopLogger())

	events := scene.EventsSnapshot()
	if len(events) == 0 || events[0] != "UpdateHaResources:vm:100=stopped" {
		t.Fatalf("the rollback must stop the HA-managed VM through PVE HA first, got %v", events)
	}
	if slices.Contains(events, "Stop:pve-node1/100") {
		t.Fatalf("the rollback must not stop an HA-managed VM directly, got %v", events)
	}
	if nodes.deleteCalls != 1 {
		t.Fatalf("expected 1 DeleteQemu call, got %d", nodes.deleteCalls)
	}
	if _, ok := scene.Rules["bosh-na-100"]; ok {
		t.Error("cleanupVM must remove the node-affinity pin rule on rollback")
	}
	if !slices.Contains(events, "DeleteHaResources:vm:100") {
		t.Errorf("cleanupVM must deregister the HA resource on rollback, got %v", events)
	}
}

// TestCleanupVM_PreservesAnHAManagedVMTheGateDefers tags and leaves a VM whose
// HA state does not allow the destroy, and writes nothing to HA.
func TestCleanupVM_PreservesAnHAManagedVMTheGateDefers(t *testing.T) {
	for name, setup := range map[string]func(*HADeleteScene){
		"fence":       func(s *HADeleteScene) { s.Manage(100, "pve-node2", "started", "fence"); deadNode(s) },
		"recovery":    func(s *HADeleteScene) { s.Manage(100, "pve-node2", "started", "recovery"); deadNode(s) },
		"motion":      func(s *HADeleteScene) { s.Manage(100, "pve-node2", "started", "migrate") },
		"no local":    func(s *HADeleteScene) { s.Manage(100, "pve-node2", "started", "started"); s.NoLocal = true },
		"perms unset": func(s *HADeleteScene) { s.Manage(100, "pve-node2", "started", "started"); s.Perms = nil },
	} {
		t.Run(name, func(t *testing.T) {
			scene := NewHADeleteScene()
			setup(scene)
			scene.Rules["bosh-na-100"] = map[string]any{"type": "node-affinity", "resources": "vm:100", "nodes": "pve-node2"}
			nodes, q := &kfNodes{}, &kfQEMU{}
			deps := rollbackHADeps(scene, nodes, q)

			cleanupVM(context.Background(), deps, "pve-node2", 100, kfEnv(), log.NewNopLogger())

			if nodes.deleteCalls != 0 {
				t.Fatalf("a deferred rollback must not destroy the VM, got %d DeleteQemu calls", nodes.deleteCalls)
			}
			if nodes.failedTagCalls != 1 || nodes.updatedTags == nil ||
				!strings.Contains(*nodes.updatedTags, "bosh-create-failed") {
				t.Fatalf("a deferred rollback must tag the VM it leaves, got %d writes, tags %v", nodes.failedTagCalls,
					nodes.updatedTags)
			}
			if events := scene.EventsSnapshot(); len(events) != 0 {
				t.Fatalf("a deferred rollback must write nothing to HA or stop the VM, got %v", events)
			}
			if _, ok := scene.Rules["bosh-na-100"]; !ok {
				t.Fatal("a deferred rollback must keep the pin rule")
			}
		})
	}
}

type unreadableHAResourcesCluster struct{ *naClusterStub }

func (unreadableHAResourcesCluster) ListHaResources(
	context.Context, *cluster.ListHaResourcesParams,
) (*cluster.ListHaResourcesResponse, error) {
	return nil, errors.New("HA resource list unavailable")
}

type unreadableHAKFClient struct{ *kfClient }

func (c *unreadableHAKFClient) Cluster() cluster.Service {
	return unreadableHAResourcesCluster{naClusterStub: c.cluster}
}

func TestCleanupVM_PreservesTheVMWhenHAMembershipIsUnreadable(t *testing.T) {
	cl := newNAStub()
	cl.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100"}
	nodes, q := &kfNodes{}, &kfQEMU{}
	deps := Deps{
		Config: icMinConfig(),
		PVE:    &unreadableHAKFClient{kfClient: &kfClient{nodes: nodes, qemu: q, cluster: cl}},
		Logger: log.NewNopLogger(),
	}

	cleanupVM(context.Background(), deps, "pve01", 100, kfEnv(), log.NewNopLogger())

	if nodes.deleteCalls != 0 || q.stopCalls != 0 {
		t.Fatalf("an unreadable HA membership must leave the VM, got %d destroys and %d stops", nodes.deleteCalls,
			q.stopCalls)
	}
	if nodes.failedTagCalls != 1 {
		t.Fatalf("the preserved VM must be tagged, got %d tag writes", nodes.failedTagCalls)
	}
	if _, ok := cl.rules["bosh-na-100"]; !ok {
		t.Fatal("the preserved VM must keep its pin rule")
	}
}

// --- the journal-managed delete ---

// managedHADeleteFixture is a journal-managed VM whose HA resource the CRM
// holds in state, with a one second motion wait.
func managedHADeleteFixture(t *testing.T, state string) (Deps, *aj.Journal, *deleteManagedClient, *infrastructureHACluster, aj.Record) {
	t.Helper()
	deps, j, base, record := deleteManagedFixture(t)
	motion := 1
	deps.Config.HA = &config.HAConfig{MotionWaitSec: &motion}
	ha := &infrastructureHACluster{Service: base.Cluster(), haState: state}
	deps.PVE = &cleanupTaskClient{Client: &unknownHACleanupClient{deleteManagedClient: base, ha: ha}}
	return deps, j, base, ha, record
}

// TestManagedVMDeleteHADeferralLeavesTheRecord defers a journal-managed VM in
// fence, in recovery, or in motion with the gate's retriable error, before
// the admission, so the record is unchanged and nothing is stopped,
// destroyed, or purged.
func TestManagedVMDeleteHADeferralLeavesTheRecord(t *testing.T) {
	for state, want := range map[string]string{
		"fence":    "fenced",
		"recovery": "does not report node pve1 fenced",
		"migrate":  "motion budget",
	} {
		t.Run(state, func(t *testing.T) {
			deps, j, base, ha, record := managedHADeleteFixture(t, state)
			before, err := j.Inspect(record.ID)
			if err != nil {
				t.Fatal(err)
			}
			handled, err := deleteManagedVMIfRecorded(t.Context(), deps, "123", 123)
			if !handled {
				t.Fatal("the journal-managed VM was not handled")
			}
			requireHADeferral(t, err, want)
			if cpierrors.IsDefinite(err) {
				t.Fatalf("an HA deferral must not be definite: %v", err)
			}
			after, err := j.Inspect(record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.State != before.State || len(after.Steps) != len(before.Steps) ||
				len(after.Verifications) != len(before.Verifications) {
				t.Fatalf("the deferral changed the record: %s with %d steps, was %s with %d", after.State,
					len(after.Steps), before.State, len(before.Steps))
			}
			if base.stopCount != 0 || base.destroyCount != 0 || ha.deletes != 0 {
				t.Fatalf("the deferral acted: stop %d destroy %d purge %d", base.stopCount, base.destroyCount, ha.deletes)
			}
		})
	}
}

// TestManagedVMDeleteHAStopDeferralKeepsTheRecordRetriable hands a deferral
// of the HA stop, after the admission, back to the Director as the same
// retriable error without requiring reconciliation, and the retry completes
// the delete.
func TestManagedVMDeleteHAStopDeferralKeepsTheRecordRetriable(t *testing.T) {
	deps, j, base, ha, record := managedHADeleteFixture(t, "started")
	base.permsErr = errors.New("permission denied - (HTTP403)")

	handled, err := deleteManagedVMIfRecorded(t.Context(), deps, "123", 123)
	if !handled {
		t.Fatal("the journal-managed VM was not handled")
	}
	requireHADeferral(t, err, "permissions on / are unreadable")
	if cpierrors.IsDefinite(err) {
		t.Fatalf("an HA deferral must not be definite: %v", err)
	}
	after, err := j.Inspect(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State == aj.ReconciliationRequired {
		t.Fatalf("an HA deferral must not require reconciliation: %s", after.Reason)
	}
	if base.stopCount != 0 || base.destroyCount != 0 || ha.deletes != 0 {
		t.Fatalf("the deferral acted: stop %d destroy %d purge %d", base.stopCount, base.destroyCount, ha.deletes)
	}

	// PVE HA has since stopped the VM and the token's permissions read again.
	base.permsErr = nil
	ha.haState = ""
	base.stopped = true
	handled, err = deleteManagedVMIfRecorded(t.Context(), deps, "123", 123)
	if !handled || err != nil {
		t.Fatalf("the retry: handled %v, err %v", handled, err)
	}
	final, err := j.Inspect(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != aj.Deleted || base.destroyCount != 1 || ha.deletes != 1 {
		t.Fatalf("the retry did not complete the delete: %s, destroy %d, purge %d", final.State,
			base.destroyCount, ha.deletes)
	}
}

// requireManagedDeferralLeavesTheRecord runs the delete of the journal-managed
// VM and requires the HA deferral naming want, an unchanged record, and no
// stop, destroy, or purge.
func requireManagedDeferralLeavesTheRecord(
	t *testing.T, deps Deps, j *aj.Journal, base *deleteManagedClient, ha *infrastructureHACluster, record aj.Record,
	want string,
) {
	t.Helper()
	before, err := j.Inspect(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	handled, err := deleteManagedVMIfRecorded(t.Context(), deps, "123", 123)
	if !handled {
		t.Fatal("the journal-managed VM was not handled")
	}
	requireHADeferral(t, err, want)
	if !isWholeHADeferral(err) {
		t.Fatalf("the error is not an HA deferral: %v", err)
	}
	after, err := j.Inspect(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != before.State || len(after.Steps) != len(before.Steps) {
		t.Fatalf("the deferral changed the record: %s with %d steps, was %s with %d", after.State,
			len(after.Steps), before.State, len(before.Steps))
	}
	if base.stopCount != 0 || base.destroyCount != 0 || ha.deletes != 0 {
		t.Fatalf("the deferral acted: stop %d destroy %d purge %d", base.stopCount, base.destroyCount, ha.deletes)
	}
}

// TestManagedVMDeleteHADefersOnAnUnknownNodeOrAStaleLRM defers a
// journal-managed VM whose node the CRM reports unknown, or whose LRM
// timestamp is stale, before the admission.
func TestManagedVMDeleteHADefersOnAnUnknownNodeOrAStaleLRM(t *testing.T) {
	for name, tc := range map[string]struct {
		tune func(s *HADeleteScene)
		want string
	}{
		"node unknown": {tune: func(s *HADeleteScene) { s.NodeState["pve1"] = "unknown" },
			want: "CRM reports the node unknown"},
		"stale LRM": {tune: func(s *HADeleteScene) { s.LRMAge["pve1"] = 3600 }, want: "no usable state"},
	} {
		t.Run(name, func(t *testing.T) {
			deps, j, base, ha, record := managedHADeleteFixture(t, "started")
			ha.tune = tc.tune
			requireManagedDeferralLeavesTheRecord(t, deps, j, base, ha, record, tc.want)
		})
	}
}

// TestManagedVMDeleteDefersAnUnreadableConfigOnTheHAState answers a config
// read that fails before the gate, the shape of a VM on a dead node, with the
// HA deferral when the VM's HA state does not let the delete proceed, and with
// the read's own error when it does.
func TestManagedVMDeleteDefersAnUnreadableConfigOnTheHAState(t *testing.T) {
	readErr := errors.New("proxy loop detected - no route to host (HTTP595)")

	t.Run("fence", func(t *testing.T) {
		deps, j, base, ha, record := managedHADeleteFixture(t, "fence")
		base.configErr = readErr
		requireManagedDeferralLeavesTheRecord(t, deps, j, base, ha, record, "fence")
	})

	t.Run("proceed", func(t *testing.T) {
		deps, _, base, ha, _ := managedHADeleteFixture(t, "started")
		base.configErr = readErr
		handled, err := deleteManagedVMIfRecorded(t.Context(), deps, "123", 123)
		if !handled || !errors.Is(err, readErr) {
			t.Fatalf("want the config read's own error, got handled %v err %v", handled, err)
		}
		if base.stopCount != 0 || base.destroyCount != 0 || ha.deletes != 0 {
			t.Fatalf("the failed read acted: stop %d destroy %d purge %d", base.stopCount, base.destroyCount,
				ha.deletes)
		}
	})
}

// --- the scene's rule update ---

func TestHASceneClusterUpdateHaRules(t *testing.T) {
	scene := NewHADeleteScene()
	scene.Rules["shared"] = map[string]any{"type": "resource-affinity", "resources": "vm:1,vm:2", "comment": "c"}
	c := &HASceneCluster{Scene: scene}
	resources, del := "vm:2", "comment"
	if err := c.UpdateHaRules(t.Context(), "shared", &cluster.UpdateHaRulesParams{
		Type: "resource-affinity", Resources: &resources, Delete: &del,
	}); err != nil {
		t.Fatal(err)
	}
	if scene.Rules["shared"]["resources"] != "vm:2" || scene.Rules["shared"]["comment"] != nil {
		t.Fatalf("the update was not applied: %v", scene.Rules["shared"])
	}
	if err := c.UpdateHaRules(t.Context(), "missing", &cluster.UpdateHaRulesParams{Type: "resource-affinity"}); err == nil {
		t.Fatal("an update of a missing rule must fail")
	}
	if events := scene.EventsSnapshot(); !slices.Equal(events, []string{"UpdateHaRules:shared=vm:2", "UpdateHaRules:missing="}) {
		t.Fatalf("the updates were not recorded: %v", events)
	}
}

// TestGateHADeleteExpiredGraceWindowReadsAStaleSourceAsKnown keeps the HA
// state Known after the grace window expires, although the failed source's
// LRM timestamp is hours old, so the expiry is a decision on a read state.
func TestGateHADeleteExpiredGraceWindowReadsAStaleSourceAsKnown(t *testing.T) {
	scene := recoveryScene()
	f := newHADeleteFixture(t, scene, 600)
	if _, err := f.gate(t, "pve-node2"); err == nil {
		t.Fatal("the running grace window must defer")
	}
	f.clock.t = f.clock.t.Add(11 * time.Minute)
	_, err := f.gate(t, "pve-node2")
	requireHADeferral(t, err, "grace window expired")
	st, err := readHADeleteState(t.Context(), f.deps, "delete_vm", 101, "pve-node2")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Known || st.LRMFresh || st.Raw != "recovery" {
		t.Fatalf("state after the expiry: known=%v lrm_fresh=%v raw=%q, want a known recovery with a stale LRM",
			st.Known, st.LRMFresh, st.Raw)
	}
	if events := scene.EventsSnapshot(); len(events) != 0 {
		t.Fatalf("an expired grace window must write nothing, got %v", events)
	}
}
