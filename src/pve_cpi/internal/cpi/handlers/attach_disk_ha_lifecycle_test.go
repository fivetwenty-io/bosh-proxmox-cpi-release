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
// journal-managed disk for an attach to VM 777 and sends HA writes through its
// guarded client. A write of VM 777's pin or HA resource that carries this
// Director's marker goes through without a journal step, and a write PVE
// fails leaves the guard usable. Every other write is refused before it is
// sent and leaves the guard usable too. That covers a rule or resource of
// another Director, a rule that is not a CPI rule, a pin or resource of
// another VM even when it is this Director's, a pin that names another
// resource, a pin creation while the pin exists, a resource update that sets
// another field, requests disabled other than as the barrier's restore of a
// resource it held from disabled, or deletes a setting however PVE would split
// the list, a removal that does not purge, and any write from a lifecycle that
// serves no VM. The barrier's restore of disabled goes through. A registration
// still locks the guard as before.
func TestDiskLifecycleGuardJudgesHAWritesByOwner(t *testing.T) {
	f := haLifecycleFixture(t)
	deps, store, jr, id, cid := f.deps, f.store, f.jr, f.id, f.cid
	ctx := context.Background()
	deps.RequestDirectorUUID = aaDirector
	other := haIdentity{Owner: aaOtherDirector}.marker()
	marker := haIdentity{Owner: aaDirector}.marker()
	foreign := "n1,n2"
	store.rules["bosh-aa-other"] = cluster.CreateHaRulesParams{Rule: "bosh-aa-other", Type: "resource-affinity",
		Resources: "vm:888", Comment: &other, Nodes: &foreign}
	store.resources["vm:888"] = true
	store.resComments["vm:888"] = other
	store.resources["vm:555"] = true
	store.resComments["vm:555"] = marker
	store.states["vm:555"] = "started"
	store.rules["bosh-na-555"] = cluster.CreateHaRulesParams{Rule: "bosh-na-555", Type: naRuleType,
		Resources: "vm:555", Comment: &marker, Nodes: &foreign}
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
	lifecycle.haVMID = 777
	steps := func() int {
		record, err := jr.Inspect(id)
		if err != nil {
			t.Fatal(err)
		}
		return len(record.Steps)
	}
	before := steps()
	svc := local.PVE.Cluster()
	nodes := "n1"
	ignored, disabled := "ignored", "disabled"
	purge, keep := true, false
	// withResourceComment and withPinComment run write while VM 777's resource
	// or pin carries comment, and put the marker back afterwards.
	withResourceComment := func(comment string, write func() error) func() error {
		return func() error {
			store.resComments["vm:777"] = comment
			defer func() { store.resComments["vm:777"] = marker }()
			return write()
		}
	}
	withPinComment := func(comment string, write func() error) func() error {
		return func() error {
			pin := store.rules["bosh-na-777"]
			was := pin.Comment
			pin.Comment = &comment
			store.rules["bosh-na-777"] = pin
			defer func() { pin.Comment = was; store.rules["bosh-na-777"] = pin }()
			return write()
		}
	}
	// withHeld runs write while VM 777's resource requests state and carries
	// comment, the way the barrier leaves it while it holds the VM, and puts the
	// started resource with the marker back afterwards.
	withHeld := func(state, comment string, write func() error) func() error {
		return func() error {
			store.states["vm:777"], store.resComments["vm:777"] = state, comment
			defer func() { store.states["vm:777"], store.resComments["vm:777"] = "started", marker }()
			return write()
		}
	}
	heldFromDisabled := marker + " bosh-narrow-from=disabled"
	heldFromStarted := marker + " bosh-narrow-from=started"
	requestDisabled := func(comment string) func() error {
		return func() error {
			return svc.UpdateHaResources(ctx, "vm:777", &cluster.UpdateHaResourcesParams{State: &disabled,
				Comment: &comment})
		}
	}
	refused := []struct {
		name  string
		write func() error
	}{
		{"a lifecycle that serves no VM", func() error {
			// A resource vm:0 of this Director, so only the check that the
			// lifecycle serves a VM refuses the write.
			lifecycle.haVMID = 0
			store.resources["vm:0"], store.states["vm:0"], store.resComments["vm:0"] = true, "started", marker
			defer func() {
				lifecycle.haVMID = 777
				delete(store.resources, "vm:0")
				delete(store.states, "vm:0")
				delete(store.resComments, "vm:0")
			}()
			return svc.UpdateHaResources(ctx, "vm:0", &cluster.UpdateHaResourcesParams{State: &ignored,
				Comment: &marker})
		}},
		{"another Director's rule", func() error {
			return svc.UpdateHaRules(ctx, "bosh-aa-other", &cluster.UpdateHaRulesParams{Comment: &marker})
		}},
		{"the pin while another Director's marker is on it", withPinComment(other, func() error {
			return svc.UpdateHaRules(ctx, "bosh-na-777", &cluster.UpdateHaRulesParams{Comment: &marker,
				Nodes: &nodes})
		})},
		{"a rule that is not a CPI rule", func() error { return svc.DeleteHaRules(ctx, "operator-rule") }},
		{"a pin update without the marker", func() error {
			plain := "no marker"
			return svc.UpdateHaRules(ctx, "bosh-na-777", &cluster.UpdateHaRulesParams{Comment: &plain})
		}},
		{"a pin creation without the marker", func() error {
			return svc.CreateHaRules(ctx, &cluster.CreateHaRulesParams{Rule: "bosh-na-777", Type: naRuleType,
				Resources: "vm:777", Nodes: &nodes})
		}},
		{"a pin creation while the pin exists", func() error {
			return svc.CreateHaRules(ctx, &cluster.CreateHaRulesParams{Rule: "bosh-na-777", Type: naRuleType,
				Resources: "vm:777", Comment: &marker, Nodes: &nodes})
		}},
		{"another VM's pin of this Director", func() error {
			return svc.UpdateHaRules(ctx, "bosh-na-555", &cluster.UpdateHaRulesParams{Comment: &marker,
				Nodes: &nodes})
		}},
		{"a creation of another VM's pin", func() error {
			return svc.CreateHaRules(ctx, &cluster.CreateHaRulesParams{Rule: "bosh-na-999", Type: naRuleType,
				Resources: "vm:999", Comment: &marker, Nodes: &nodes})
		}},
		{"a pin creation that names another resource too", func() error {
			// The pin is absent, so only the check of the new rule's resources
			// refuses the creation.
			pin := store.rules["bosh-na-777"]
			delete(store.rules, "bosh-na-777")
			defer func() { store.rules["bosh-na-777"] = pin }()
			return svc.CreateHaRules(ctx, &cluster.CreateHaRulesParams{Rule: "bosh-na-777", Type: naRuleType,
				Resources: "vm:777;vm:555", Comment: &marker, Nodes: &nodes})
		}},
		{"a pin update that names another resource", func() error {
			resources := "vm:555"
			return svc.UpdateHaRules(ctx, "bosh-na-777", &cluster.UpdateHaRulesParams{Comment: &marker,
				Nodes: &nodes, Resources: &resources})
		}},
		{"a removal of another VM's pin", func() error { return svc.DeleteHaRules(ctx, "bosh-na-555") }},
		{"another Director's resource", func() error {
			return svc.UpdateHaResources(ctx, "vm:888", &cluster.UpdateHaResourcesParams{State: &ignored})
		}},
		{"the resource while another Director's marker is on it", withResourceComment(other, func() error {
			return svc.UpdateHaResources(ctx, "vm:777", &cluster.UpdateHaResourcesParams{State: &ignored})
		})},
		{"another VM's resource of this Director", func() error {
			return svc.UpdateHaResources(ctx, "vm:555", &cluster.UpdateHaResourcesParams{State: &ignored,
				Comment: &marker})
		}},
		{"a resource update that requests disabled", func() error {
			return svc.UpdateHaResources(ctx, "vm:777", &cluster.UpdateHaResourcesParams{State: &disabled,
				Comment: &marker})
		}},
		{"a request for disabled while the barrier's token names started",
			withHeld("ignored", heldFromStarted, requestDisabled(marker))},
		{"a request for disabled on a resource that is not ignored",
			withHeld("started", heldFromDisabled, requestDisabled(marker))},
		{"a request for disabled that keeps the barrier's token",
			withHeld("ignored", heldFromDisabled, requestDisabled(heldFromDisabled))},
		{"a request for disabled that writes no comment", withHeld("ignored", heldFromDisabled, func() error {
			return svc.UpdateHaResources(ctx, "vm:777", &cluster.UpdateHaResourcesParams{State: &disabled})
		})},
		{"a resource update that sets another field", func() error {
			restarts := int64(5)
			return svc.UpdateHaResources(ctx, "vm:777", &cluster.UpdateHaResourcesParams{State: &ignored,
				MaxRestart: &restarts})
		}},
		{"a resource update that drops the marker", func() error {
			del := "comment"
			return svc.UpdateHaResources(ctx, "vm:777", &cluster.UpdateHaResourcesParams{Delete: &del})
		}},
		{"a resource update that drops the marker in a semicolon list", func() error {
			del := "state;comment"
			return svc.UpdateHaResources(ctx, "vm:777", &cluster.UpdateHaResourcesParams{Delete: &del})
		}},
		{"a resource update that drops the marker in a spaced list", func() error {
			del := "state comment"
			return svc.UpdateHaResources(ctx, "vm:777", &cluster.UpdateHaResourcesParams{Delete: &del})
		}},
		{"a resource update that deletes another setting", func() error {
			del := "max_restart"
			return svc.UpdateHaResources(ctx, "vm:777", &cluster.UpdateHaResourcesParams{State: &ignored,
				Delete: &del})
		}},
		{"a removal of another VM's resource", func() error {
			return svc.DeleteHaResources(ctx, "vm:555", &cluster.DeleteHaResourcesParams{Purge: &purge})
		}},
		{"a removal that does not purge", func() error {
			return svc.DeleteHaResources(ctx, "vm:777", &cluster.DeleteHaResourcesParams{Purge: &keep})
		}},
		{"a removal without parameters", func() error {
			return svc.DeleteHaResources(ctx, "vm:777", &cluster.DeleteHaResourcesParams{})
		}},
	}
	// reasons holds the refusal that rows whose write another check could
	// also refuse must give.
	reasons := map[string]string{
		"a lifecycle that serves no VM":                  "serves no VM",
		"a pin creation that names another resource too": "creation names resources",
	}
	for _, row := range refused {
		writes := len(store.writes)
		err := row.write()
		if err == nil || !errors.Is(err, pve.ErrMutationNotAttempted) {
			t.Fatalf("%s: err %v, want a refusal before the write was sent", row.name, err)
		}
		if reason, ok := reasons[row.name]; ok && !strings.Contains(err.Error(), reason) {
			t.Fatalf("%s: err %v, want the refusal that says %q", row.name, err, reason)
		}
		if !errors.Is(err, errManagedHAWriteRefused) {
			t.Fatalf("%s: err %v, want the HA write refusal", row.name, err)
		}
		if len(store.writes) != writes {
			t.Fatalf("%s: the refused write reached PVE: %v", row.name, store.writes)
		}
		if gerr := lifecycle.guard.Err(); gerr != nil {
			t.Fatalf("%s: the refusal locked the guard: %v", row.name, gerr)
		}
	}
	if store.states["vm:555"] != "started" || store.rules["bosh-na-555"].Nodes == nil {
		t.Fatal("a refused write changed VM 555's resource or pin")
	}

	if err := svc.UpdateHaResources(ctx, "vm:777", &cluster.UpdateHaResourcesParams{State: &ignored,
		Comment: &marker}); err != nil {
		t.Fatalf("request ignored on this Director's resource: %v", err)
	}
	if store.states["vm:777"] != "ignored" {
		t.Fatalf("state %q, want ignored", store.states["vm:777"])
	}
	// The barrier's restore of a resource that was disabled before it held
	// the VM puts disabled back and drops the token.
	store.resComments["vm:777"] = heldFromDisabled
	if err := requestDisabled(marker)(); err != nil {
		t.Fatalf("restore disabled on the resource the barrier held: %v", err)
	}
	if store.states["vm:777"] != "disabled" || store.resComments["vm:777"] != marker {
		t.Fatalf("state %q comment %q, want disabled with the token dropped", store.states["vm:777"],
			store.resComments["vm:777"])
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
	if err := svc.DeleteHaResources(ctx, "vm:777", &cluster.DeleteHaResourcesParams{Purge: &purge}); err != nil {
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

// TestSplitPVEListSplitsAsPVEDoes pins the list split PVE applies to a delete
// parameter, so a check for a key sees every key PVE would delete.
func TestSplitPVEListSplitsAsPVEDoes(t *testing.T) {
	for _, row := range []struct {
		list string
		want []string
	}{
		{"", nil},
		{" , ; ", nil},
		{"comment", []string{"comment"}},
		{"state,comment", []string{"state", "comment"}},
		{"state;comment", []string{"state", "comment"}},
		{" state  comment\t", []string{"state", "comment"}},
		{"state, ;comment", []string{"state", "comment"}},
		{"state\x00comment,x", []string{"state", "comment,x"}},
	} {
		if got := splitPVEList(row.list); !slices.Equal(got, row.want) {
			t.Errorf("splitPVEList(%q) = %q, want %q", row.list, got, row.want)
		}
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
	// The CRM runs the VM on n1 and acts on no request for it, so the VM is
	// settled there when the rebuild removes its job. Its last round and the
	// LRM's report after it are always newer than the entry node's clock at the
	// read before, so the rebuild sees a round after its pin at once.
	store.manager = func() *cluster.ListHaStatusManagerStatusResponse {
		return HAFreshManagerStatus(time.Now().Unix()+6, "n1", []string{"n2"},
			map[string]HASceneService{"vm:777": {State: "started", Node: "n1", UID: "u1"}})
	}
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

// TestManagedAttachPutsBackADisabledHAResourceAfterTheBarrier attaches a
// journal-managed disk on node-local storage into VM 777, whose HA resource an
// operator disabled. Under the hold policy the pin narrows to n1, so the
// attach runs inside the barrier: it requests ignored with the token that
// names disabled, the pin narrows, the disk lands, and the barrier puts
// disabled back. The disk lifecycle's guard admits that restore, so the attach
// succeeds, the resource is disabled again without the token, and the disk's
// record does not ask for reconciliation.
func TestManagedAttachPutsBackADisabledHAResourceAfterTheBarrier(t *testing.T) {
	f := haLifecycleFixture(t, "nfs")
	deps, store, jr, id, cid := f.deps, f.store, f.jr, f.id, f.cid
	deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyHold)
	local := pve.StorageInfo{Name: deps.Config.DiskStorage, Type: pve.StorageTypeLVMThin, Content: "images,rootdir"}
	profile, err := deps.HAProfile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	profile.Storages = []pve.StorageInfo{local, {Name: "nfs", Type: pve.StorageTypeNFS, Shared: true,
		Content: "images"}}
	deps.Resolver = infoResolver{base: backendResolverOrDefault(deps), info: local}
	store.states["vm:777"] = "disabled"
	// The CRM reports the disabled VM stopped on n1 until the barrier's
	// ignored request lands, and lists it no more after that. Each read is a
	// round newer than the request, so the drain sees the CRM let go at once.
	store.manager = func() *cluster.ListHaStatusManagerStatusResponse {
		services := map[string]HASceneService{}
		if store.states["vm:777"] != "ignored" {
			services["vm:777"] = HASceneService{State: "stopped", Node: "n1", UID: "u1"}
		}
		return HAFreshManagerStatus(time.Now().Unix()+6, "n1", []string{"n2"}, services)
	}
	_, err = HandleAttachDisk(deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)},
		jsonrpc.Context{DirectorUUID: aaDirector})
	if err != nil {
		t.Fatalf("attach into the disabled VM under HA: %v (writes %v)", err, store.writes)
	}
	if !slices.Contains(store.writes, "update-resource:vm:777") {
		t.Fatalf("HA writes %v; the attach did not hold the VM inside the barrier", store.writes)
	}
	if store.states["vm:777"] != "disabled" {
		t.Fatalf("HA resource state %q, want disabled put back", store.states["vm:777"])
	}
	if comment := store.resComments["vm:777"]; strings.Contains(comment, "bosh-narrow-from=") ||
		haOwnershipOf(comment, aaDirector) != haOwnedByUs {
		t.Fatalf("HA resource comment %q, want the marker without the barrier's token", comment)
	}
	if pin := store.rules["bosh-na-777"]; pin.Nodes == nil || *pin.Nodes != "n1" {
		t.Fatalf("pin %+v, want it narrowed to n1", pin)
	}
	record, err := jr.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State == aj.ReconciliationRequired {
		t.Fatalf("the attach left the record asking for reconciliation: %q", record.Reason)
	}
}
