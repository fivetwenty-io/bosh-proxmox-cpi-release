package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// haLifecycleFixture parks the flow fixture's journal-managed disk, and puts
// VM 777 under PVE HA in mode auto: its HA resource and its pin over n1 and n2
// carry aaDirector's marker, and the pin records storages. The disk's storage
// is shared NFS that both nodes reach. It returns the fixture, the HA store,
// the journal, and the disk's record ID and CID.
func haLifecycleFixture(t *testing.T, storages ...string) haLifecycle {
	t.Helper()
	deps, client, jr, id, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	if err := digestDetach(t, deps, cid); err != nil {
		t.Fatalf("park the disk: %v", err)
	}
	deps.Config.HA = &config.HAConfig{Mode: haChoiceOf(config.HAModeAuto)}
	profile := stagingProfile(config.HAModeAuto)
	profile.Nodes = []ha.NodeFacts{{Name: "n1", Online: true, Version: "9.2.4"},
		{Name: "n2", Online: true, Version: "9.2.4"}}
	profile.Storages = []pve.StorageInfo{{Name: deps.Config.DiskStorage, Type: pve.StorageTypeNFS, Shared: true,
		Content: "images"}}
	deps.HAProfile = func(context.Context) (*ha.Profile, error) { return profile, nil }
	deps.Resolver = infoResolver{base: backendResolverOrDefault(deps),
		info: pve.StorageInfo{Type: pve.StorageTypeNFS, Shared: true, Content: "images"}}
	store := newLifecycleFlowHA()
	marker := haIdentity{Owner: aaDirector}.marker()
	store.resources["vm:777"] = true
	store.resComments["vm:777"] = marker
	store.states["vm:777"] = "started"
	nodes, strict := "n1,n2", false
	comment := ha.WithVolumeRecord(marker, storages)
	store.rules["bosh-na-777"] = cluster.CreateHaRulesParams{Rule: "bosh-na-777", Type: naRuleType,
		Resources: "vm:777", Comment: &comment, Nodes: &nodes, Strict: &strict}
	client.haStore = store
	repl := newFakeReplica()
	repl.source = "n1"
	deps.Replication, client.replication = repl, repl
	client.state.configs[777]["tags"] = "bosh-cpi;" + haManagedTag
	return haLifecycle{deps: deps, client: client, store: store, jr: jr, id: id, cid: cid}
}

// haLifecycle is what haLifecycleFixture builds.
type haLifecycle struct {
	deps   Deps
	client *lifecycleFlowPVE
	store  *lifecycleFlowHA
	jr     *aj.Journal
	id     string
	cid    string
}

// TestManagedAttachWritesTheHAPinInsideTheDiskLifecycle attaches a
// journal-managed disk to a VM under PVE HA whose pin does not record the
// disk's storage yet. The HA decision keeps the VM on both nodes, so the attach
// lands first and the pin that records the new storage is written after it,
// inside the disk's lifecycle. The pin write carries no VM, and the lifecycle
// guard judges it by its rule name and this Director's marker, so the attach
// succeeds, the pin records the storage, and the disk's record does not ask for
// reconciliation.
func TestManagedAttachWritesTheHAPinInsideTheDiskLifecycle(t *testing.T) {
	f := haLifecycleFixture(t)
	deps, client, store, jr, id, cid := f.deps, f.client, f.store, f.jr, f.id, f.cid
	_, err := HandleAttachDisk(deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)},
		jsonrpc.Context{DirectorUUID: aaDirector})
	if err != nil {
		t.Fatalf("attach into the VM under HA: %v", err)
	}
	pin, ok := store.rules["bosh-na-777"]
	if !ok || pin.Comment == nil {
		t.Fatalf("rules %v; the pin is gone", store.rules)
	}
	recorded, ok := ha.VolumeRecord(*pin.Comment)
	if !ok || len(recorded) != 1 || recorded[0] != deps.Config.DiskStorage {
		t.Fatalf("pin comment %q records %v, want the disk's storage %s", *pin.Comment, recorded,
			deps.Config.DiskStorage)
	}
	if haOwnershipOf(*pin.Comment, aaDirector) != haOwnedByUs {
		t.Fatalf("pin comment %q lost the ownership marker", *pin.Comment)
	}
	found := false
	for _, w := range store.writes {
		found = found || w == "update-rule:bosh-na-777"
	}
	if !found {
		t.Fatalf("HA writes %v; the pin update went through no guarded write", store.writes)
	}
	if !store.resources["vm:777"] {
		t.Fatal("the attach removed the VM from HA")
	}
	held := false
	for key, value := range client.state.configs[777] {
		text, _ := value.(string)
		held = held || (strings.HasPrefix(key, "scsi") && strings.Contains(text, "serial="))
	}
	if !held {
		t.Fatalf("VM 777 config %v holds no disk", client.state.configs[777])
	}
	record, err := jr.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State == aj.ReconciliationRequired {
		t.Fatalf("the attach left the record asking for reconciliation: %q", record.Reason)
	}
}

// TestDiskLifecycleGuardJudgesHAWritesByOwner opens the lifecycle of a
// journal-managed disk and sends HA writes through its guarded client. A write
// of this Director's pin or HA resource goes through without a journal step,
// and a write PVE fails leaves the guard usable. A write of another
// Director's rule or resource, of a rule that is not a CPI rule, or of a pin
// update that drops the marker, is refused before it is sent and leaves the
// guard usable too. A write that targets a VM still needs the VM's identity,
// and one without it locks the guard as before.
func TestDiskLifecycleGuardJudgesHAWritesByOwner(t *testing.T) {
	f := haLifecycleFixture(t)
	deps, store, jr, id, cid := f.deps, f.store, f.jr, f.id, f.cid
	ctx := context.Background()
	deps.RequestDirectorUUID = aaDirector
	other := haIdentity{Owner: aaOtherDirector}.marker()
	foreign := "n1,n2"
	store.rules["bosh-aa-other"] = cluster.CreateHaRulesParams{Rule: "bosh-aa-other", Type: "resource-affinity",
		Resources: "vm:888", Comment: &other, Nodes: &foreign}
	store.resources["vm:888"] = true
	store.resComments["vm:888"] = other
	bare, meta, err := decodeDiskCID(ctx, deps, "attach_disk", cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(ctx, deps, "attach_disk", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	local, lifecycle, err := managedDiskOperation(ctx, deps, rd, "attach_disk")
	if err != nil || lifecycle == nil {
		t.Fatalf("open the disk's lifecycle: %v", err)
	}
	defer func() { _ = lifecycle.finish(ctx, errors.New("the test ends the operation"), false) }()
	steps := func() int {
		record, err := jr.Inspect(id)
		if err != nil {
			t.Fatal(err)
		}
		return len(record.Steps)
	}
	before := steps()
	svc := local.PVE.Cluster()
	marker := haIdentity{Owner: aaDirector}.marker()
	nodes := "n1"
	refused := []struct {
		name  string
		write func() error
	}{
		{"another Director's rule", func() error {
			return svc.UpdateHaRules(ctx, "bosh-aa-other", &cluster.UpdateHaRulesParams{Comment: &marker})
		}},
		{"a rule that is not a CPI rule", func() error { return svc.DeleteHaRules(ctx, "operator-rule") }},
		{"a pin update without the marker", func() error {
			plain := "no marker"
			return svc.UpdateHaRules(ctx, "bosh-na-777", &cluster.UpdateHaRulesParams{Comment: &plain})
		}},
		{"a pin creation without the marker", func() error {
			return svc.CreateHaRules(ctx, &cluster.CreateHaRulesParams{Rule: "bosh-na-999", Type: naRuleType,
				Resources: "vm:999", Nodes: &nodes})
		}},
		{"another Director's resource", func() error {
			state := "ignored"
			return svc.UpdateHaResources(ctx, "vm:888", &cluster.UpdateHaResourcesParams{State: &state})
		}},
		{"a resource update that drops the marker", func() error {
			del := "comment"
			return svc.UpdateHaResources(ctx, "vm:777", &cluster.UpdateHaResourcesParams{Delete: &del})
		}},
	}
	for _, row := range refused {
		writes := len(store.writes)
		err := row.write()
		if err == nil || !errors.Is(err, pve.ErrMutationNotAttempted) {
			t.Fatalf("%s: err %v, want a refusal before the write was sent", row.name, err)
		}
		if len(store.writes) != writes {
			t.Fatalf("%s: the refused write reached PVE: %v", row.name, store.writes)
		}
		if gerr := lifecycle.guard.Err(); gerr != nil {
			t.Fatalf("%s: the refusal locked the guard: %v", row.name, gerr)
		}
	}

	state := "ignored"
	if err := svc.UpdateHaResources(ctx, "vm:777", &cluster.UpdateHaResourcesParams{State: &state,
		Comment: &marker}); err != nil {
		t.Fatalf("request ignored on this Director's resource: %v", err)
	}
	if store.states["vm:777"] != "ignored" {
		t.Fatalf("state %q, want ignored", store.states["vm:777"])
	}
	stale := "stale"
	if err := svc.UpdateHaRules(ctx, "bosh-na-777", &cluster.UpdateHaRulesParams{Comment: &marker,
		Nodes: &nodes, Digest: &stale}); err == nil {
		t.Fatal("a pin update with a stale digest succeeded")
	}
	if err := lifecycle.guard.Err(); err != nil {
		t.Fatalf("a failed pin write locked the guard: %v", err)
	}
	if err := svc.UpdateHaRules(ctx, "bosh-na-777", &cluster.UpdateHaRulesParams{Comment: &marker,
		Nodes: &nodes}); err != nil {
		t.Fatalf("update this Director's pin: %v", err)
	}
	if got := store.rules["bosh-na-777"].Nodes; got == nil || *got != "n1" {
		t.Fatalf("pin nodes %v, want n1", got)
	}
	if err := svc.DeleteHaRules(ctx, "bosh-na-777"); err != nil {
		t.Fatalf("delete this Director's pin: %v", err)
	}
	if err := svc.DeleteHaResources(ctx, "vm:777", &cluster.DeleteHaResourcesParams{}); err != nil {
		t.Fatalf("remove this Director's resource: %v", err)
	}
	if after := steps(); after != before {
		t.Fatalf("the HA writes journaled %d steps, want none", after-before)
	}
	if err := lifecycle.guard.Err(); err != nil {
		t.Fatalf("the admitted HA writes locked the guard: %v", err)
	}

	// A disk lifecycle never registers a VM in HA, and that refusal locks the
	// guard as it always has, so it runs last.
	if err := svc.CreateHaResources(ctx, &cluster.CreateHaResourcesParams{Sid: "vm:777",
		Comment: &marker}); err == nil {
		t.Fatal("the lifecycle registered the VM in HA")
	}
	if err := lifecycle.guard.Err(); err == nil {
		t.Fatal("an HA registration inside the lifecycle left the guard usable")
	}
}

// TestDiskLifecycleGuardStillNeedsTheVMOfAVMWrite sends a configuration write
// that names no positive VM through a disk lifecycle's guarded client. The HA
// admission does not reach it, so it is refused for its missing target and
// the guard locks, as it did before.
func TestDiskLifecycleGuardStillNeedsTheVMOfAVMWrite(t *testing.T) {
	f := haLifecycleFixture(t)
	deps, cid := f.deps, f.cid
	ctx := context.Background()
	deps.RequestDirectorUUID = aaDirector
	bare, meta, err := decodeDiskCID(ctx, deps, "attach_disk", cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(ctx, deps, "attach_disk", cid, bare, meta)
	if err != nil {
		t.Fatal(err)
	}
	local, lifecycle, err := managedDiskOperation(ctx, deps, rd, "attach_disk")
	if err != nil || lifecycle == nil {
		t.Fatalf("open the disk's lifecycle: %v", err)
	}
	defer func() { _ = lifecycle.finish(ctx, errors.New("the test ends the operation"), false) }()
	name := "renamed"
	if err := local.PVE.Nodes().UpdateQemuConfig(ctx, "n1", "0", &sdknodes.UpdateQemuConfigParams{
		Name: &name}); err == nil {
		t.Fatal("a configuration write that names no VM was admitted")
	}
	if err := lifecycle.guard.Err(); err == nil {
		t.Fatal("a configuration write that names no VM left the guard usable")
	}
}

// TestManagedAttachRebuildsTheReplicaOfAVMUnderHA attaches a journal-managed
// disk that a parker holds under its own name, on NFS that cannot replicate,
// into a replicate VM under this Director's PVE HA, whose pin names its
// partner n2 and whose job copies it there. PVE refuses that reassignment into
// a VM with a job, so the attach rebuilds the replica: it narrows the pin to n1
// with the rebuild token, removes the job, lets the disk enter under VM 777's
// own name, and sets the job up again to n2. The pin the attach's HA decision
// writes after the disk lands goes through the disk's lifecycle guard, so the
// record does not ask for reconciliation, and the token is gone at the end.
func TestManagedAttachRebuildsTheReplicaOfAVMUnderHA(t *testing.T) {
	deps, client, jr, id, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	_, serial, _ := strings.Cut(client.state.configs[777]["scsi1"].(string), "serial=")
	serial, _, _ = strings.Cut(serial, ",")
	for step, call := range []func(*testing.T, Deps, string) error{digestDetach, digestAttach, digestDetach} {
		if err := call(t, deps, cid); err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
	}
	parker, parked := diskHolderBySerial(t, client, serial)
	if parker == 777 || !fakeVolumeOwnedBy(parked, parker) {
		t.Fatalf("VM %d holds %s, want a parker under its own name", parker, parked)
	}
	deps.Config.HA = &config.HAConfig{Mode: haChoiceOf(config.HAModeAuto),
		LocalPolicy: haChoiceOf(config.HALocalPolicyReplicate)}
	profile := stagingProfile(config.HAModeAuto)
	profile.Nodes = []ha.NodeFacts{{Name: "n1", Online: true, Version: "9.2.4"},
		{Name: "n2", Online: true, Version: "9.2.4"}}
	profile.Storages = append(profile.Storages, pve.StorageInfo{Name: deps.Config.DiskStorage,
		Type: pve.StorageTypeNFS, Shared: true, Content: "images"})
	deps.HAProfile = func(context.Context) (*ha.Profile, error) { return profile, nil }
	deps.Resolver = infoResolver{base: backendResolverOrDefault(deps),
		info: pve.StorageInfo{Type: pve.StorageTypeNFS, Shared: true, Content: "images"}}
	client.state.configs[777]["tags"] = "bosh-cpi;" + haManagedTag + ";" + haReplicaTag
	// The VM boots from a node-local ZFS volume, the disk its job copies to n2.
	client.state.configs[777]["scsi0"] = "local-zfs:vm-777-disk-0,size=10G"
	profile.Storages = append(profile.Storages, pve.StorageInfo{Name: "local-zfs", Type: pve.StorageTypeZFSPool,
		Content: "images,rootdir"})

	store := newLifecycleFlowHA()
	marker := haIdentity{Owner: aaDirector}.marker()
	store.resources["vm:777"] = true
	store.resComments["vm:777"] = marker
	store.states["vm:777"] = "started"
	comment := ha.WithReplicaPartner(ha.WithReplicaChanged(ha.WithVolumeRecord(marker, nil),
		time.Now().Add(-time.Hour)), "n2")
	nodes, strict := "n1,n2", true
	store.rules["bosh-na-777"] = cluster.CreateHaRulesParams{Rule: "bosh-na-777", Type: naRuleType,
		Resources: "vm:777", Comment: &comment, Nodes: &nodes, Strict: &strict}
	client.haStore = store

	repl := newFakeReplica()
	repl.source = "n1"
	repl.removeOnSchedule = true
	repl.addJob("777-0", "n1", "n2", renderReplicaJobMarker(aaDirector, 1))
	deps.Replication, client.replication = repl, repl
	client.moveCalls = 0
	_, err := HandleAttachDisk(deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)},
		jsonrpc.Context{DirectorUUID: aaDirector})
	if err != nil {
		t.Fatalf("attach into the replicate VM under HA: %v", err)
	}
	if !repl.called("delete:777-0:force=false") || repl.called("delete:777-0:force=true") {
		t.Fatalf("calls %v; the job is removed with cleanup and never forced", repl.calls)
	}
	if !repl.called("create:777-0") || len(repl.jobs) != 1 || repl.jobs[0].Target != "n2" {
		t.Fatalf("calls %v jobs %+v; the job is set up again to n2", repl.calls, repl.jobs)
	}
	if holder, volume := diskHolderBySerial(t, client, serial); holder != 777 ||
		!strings.Contains(volume, "vm-777-") || client.replicationAnswers != 0 {
		t.Fatalf("VM %d holds %s after %d refusals, want VM 777 under its own name by reassignment", holder,
			volume, client.replicationAnswers)
	}
	pin, ok := store.rules["bosh-na-777"]
	if !ok || pin.Comment == nil {
		t.Fatalf("rules %v; the pin is gone", store.rules)
	}
	if _, rebuilding := ha.ReplicaRebuild(*pin.Comment); rebuilding {
		t.Fatalf("pin comment %q keeps the rebuild token", *pin.Comment)
	}
	if recorded, ok := ha.VolumeRecord(*pin.Comment); !ok || !slices.Contains(recorded, deps.Config.DiskStorage) {
		t.Fatalf("pin comment %q records %v, want the disk's storage", *pin.Comment, recorded)
	}
	if !store.resources["vm:777"] || store.states["vm:777"] != "started" {
		t.Fatalf("HA resource present %v state %q, want the VM still started under HA",
			store.resources["vm:777"], store.states["vm:777"])
	}
	record, err := jr.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State == aj.ReconciliationRequired {
		t.Fatalf("the attach left the record asking for reconciliation: %q", record.Reason)
	}
}
