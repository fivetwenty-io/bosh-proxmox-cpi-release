package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// infoResolver resolves every storage to the static backend of deps and adds
// the storage definition info, the way the production resolver's backends
// carry the classified storage they were built from.
type infoResolver struct {
	base pve.BackendResolver
	info pve.StorageInfo
}

type infoBackend struct {
	pve.Backend
	info pve.StorageInfo
}

func (b infoBackend) StorageInfo() pve.StorageInfo { return b.info }

func (r infoResolver) Resolve(ctx context.Context, storage string) (pve.Backend, error) {
	b, err := r.base.Resolve(ctx, storage)
	if err != nil {
		return nil, err
	}
	info := r.info
	info.Name = storage
	return infoBackend{Backend: b, info: info}, nil
}

// parkReplicatedFixture parks the flow fixture's managed disk by config edit
// under its birth name, which its parker doesn't own, and marks VM 777 as
// replicated, the shape of a persistent disk on NFS that returns to a VM the
// replicate policy set up. It returns the volume and the parker.
func parkReplicatedFixture(t *testing.T, deps Deps, client *lifecycleFlowPVE, cid string) (string, int) {
	t.Helper()
	value, _ := pve.ConfigString(client.state.configs[777], "scsi1")
	volume := strings.Split(value, ",")[0]
	if err := digestDetach(t, deps, cid); err != nil {
		t.Fatalf("park the disk by config edit: %v", err)
	}
	parker := 0
	for vmid, cfg := range client.state.configs {
		for key, held := range cfg {
			if text, ok := held.(string); ok && vmid != 777 && strings.HasPrefix(text, volume+",") &&
				!strings.HasPrefix(key, "unused") {
				parker = vmid
			}
		}
	}
	if parker == 0 || fakeVolumeOwnedBy(volume, parker) {
		t.Fatalf("want %s parked by config edit under a name its parker doesn't own, parker %d", volume, parker)
	}
	client.replicatedVMs = map[int]bool{777: true}
	client.moveCalls = 0
	return volume, parker
}

// assertAttachedByConfigEdit checks that volume left the parker and entered
// VM 777 under its own name with the disk's serial, and that the journal record
// closed rather than asking for reconciliation.
func assertAttachedByConfigEdit(
	t *testing.T, client *lifecycleFlowPVE, jr *aj.Journal, id, volume string, parker int,
) {
	t.Helper()
	held := ""
	for key, value := range client.state.configs[777] {
		if text, ok := value.(string); ok && strings.HasPrefix(text, volume+",") && !strings.HasPrefix(key, "unused") {
			held = text
		}
	}
	if held == "" || !strings.Contains(held, "serial=") {
		t.Fatalf("VM 777 does not hold %s with its serial: %v", volume, client.state.configs[777])
	}
	for key, value := range client.state.configs[parker] {
		if text, ok := value.(string); ok && strings.HasPrefix(text, volume+",") {
			t.Fatalf("parker %d still holds %s at %s", parker, volume, key)
		}
	}
	record, err := jr.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State == aj.ReconciliationRequired {
		t.Fatalf("the attach left the record asking for reconciliation: %q", record.Reason)
	}
}

// TestManagedAttachIntoReplicatedVMFallsBackToConfigEdit reproduces PVE's
// refusal to reassign a volume whose storage cannot replicate into a VM that
// has a replication job. The resolver cannot classify the storage, so the
// attach tries the reassignment, and PVE's answer sends it to the config edit,
// which keeps the birth-named volume's name. The attach lands with one move
// POST and no retries of the refusal.
func TestManagedAttachIntoReplicatedVMFallsBackToConfigEdit(t *testing.T) {
	deps, client, jr, id, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	volume, parker := parkReplicatedFixture(t, deps, client, cid)
	if err := digestAttach(t, deps, cid); err != nil {
		t.Fatalf("attach into the replicated VM: %v", err)
	}
	if client.moveCalls != 1 || client.replicationAnswers != 1 {
		t.Fatalf("%d move POSTs, %d refusals; the refusal is permanent and answered once", client.moveCalls,
			client.replicationAnswers)
	}
	assertAttachedByConfigEdit(t, client, jr, id, volume, parker)
}

// TestManagedAttachIntoReplicatedVMTakesConfigEditFirst is the same attach
// with a resolver that knows the storage is NFS and a job list that names
// the VM's job. The attach never sends the reassignment PVE would refuse.
func TestManagedAttachIntoReplicatedVMTakesConfigEditFirst(t *testing.T) {
	deps, client, jr, id, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	volume, parker := parkReplicatedFixture(t, deps, client, cid)
	deps.Resolver = infoResolver{base: backendResolverOrDefault(deps),
		info: pve.StorageInfo{Type: pve.StorageTypeNFS, Shared: true, Content: "images"}}
	repl := newFakeReplica()
	repl.addJob("777-0", "n1", "n2", "")
	deps.Replication = repl
	if err := digestAttach(t, deps, cid); err != nil {
		t.Fatalf("attach into the replicated VM: %v", err)
	}
	if client.moveCalls != 0 || repl.listCalls == 0 {
		t.Fatalf("%d move POSTs, %d job reads; a known refusal is never sent", client.moveCalls, repl.listCalls)
	}
	assertAttachedByConfigEdit(t, client, jr, id, volume, parker)
}

// TestManagedAttachIntoReplicatedVMWithoutJobReassigns checks that a VM with
// no replication job keeps the reassignment.
func TestManagedAttachIntoReplicatedVMWithoutJobReassigns(t *testing.T) {
	deps, client, _, _, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	parkReplicatedFixture(t, deps, client, cid)
	client.replicatedVMs = nil
	deps.Resolver = infoResolver{base: backendResolverOrDefault(deps),
		info: pve.StorageInfo{Type: pve.StorageTypeNFS, Shared: true, Content: "images"}}
	deps.Replication = newFakeReplica()
	if err := digestAttach(t, deps, cid); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if client.moveCalls != 1 {
		t.Fatalf("%d move POSTs; a VM with no job takes the disk by reassignment", client.moveCalls)
	}
}

func TestReplicaReassignVerdict(t *testing.T) {
	nfs := pve.StorageInfo{Name: "nfs", Type: pve.StorageTypeNFS, Shared: true}
	parker := pve.DiskHolder{Found: true, IsParker: true, VMID: 90000, Node: "n1"}
	mover := parker
	mover.Tags = pve.DiskMoverTag
	birth := resolvedDisk{diskCID: "d", volid: "nfs:700/vm-700-disk-0.qcow2", stableID: "s"}
	parkerNamed := resolvedDisk{diskCID: "d", volid: "nfs:90000/vm-90000-disk-0.qcow2", stableID: "s"}
	cases := []struct {
		name       string
		rd         resolvedDisk
		holder     pve.DiskHolder
		info       pve.StorageInfo
		jobs       string
		configEdit bool
		refused    string
	}{
		{name: "no job", rd: parkerNamed, holder: parker, info: nfs},
		{name: "zfspool replicates", rd: parkerNamed, holder: parker, jobs: "j",
			info: pve.StorageInfo{Name: "local-zfs", Type: pve.StorageTypeZFSPool}},
		{name: "birth named on shared", rd: birth, holder: parker, info: nfs, jobs: "j", configEdit: true},
		{name: "parker named", rd: parkerNamed, holder: parker, info: nfs, jobs: "j", refused: "named for parker"},
		{name: "mover holder", rd: birth, holder: mover, info: nfs, jobs: "j", refused: "named for parker"},
		{name: "node-local", rd: birth, holder: parker, jobs: "j", refused: "node-local",
			info: pve.StorageInfo{Name: "local-lvm", Type: pve.StorageTypeLVMThin}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			configEdit, err := replicaReassignVerdict("attach_disk", c.rd, c.holder, c.info, 710, c.jobs)
			if configEdit != c.configEdit {
				t.Fatalf("config edit %v, want %v", configEdit, c.configEdit)
			}
			if c.refused == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			var ce *cpierrors.Error
			if err == nil || !errors.As(err, &ce) || ce.Type() != cpierrors.TypeCloud ||
				!strings.Contains(err.Error(), c.refused) || !strings.Contains(err.Error(), "Recreate the VM") {
				t.Fatalf("err %v; want a Cloud refusal naming %q and the remedy", err, c.refused)
			}
		})
	}
}

// TestReplicaRefusalVerdictOutranksStaleIndex checks that PVE's refusal wins
// over a storage index that calls the storage a ZFS pool, and that the
// verdict wraps the refusal for a parker-named volume.
func TestReplicaRefusalVerdictOutranksStaleIndex(t *testing.T) {
	repl := newFakeReplica()
	repl.addJob("710-0", "n1", "n2", "operator job")
	deps := Deps{Config: &config.CPIConfig{Node: "n1"}, Logger: log.NewNopLogger(), Replication: repl}
	deps.Resolver = infoResolver{base: backendResolverOrDefault(deps),
		info: pve.StorageInfo{Type: pve.StorageTypeZFSPool, Shared: true}}
	parker := pve.DiskHolder{Found: true, IsParker: true, VMID: 90000, Node: "n1"}
	refusal := pve.ErrMoveDiskReplicationRefused
	rd := resolvedDisk{diskCID: "d", stableID: "s"}
	err := replicaRefusalVerdict(context.Background(), deps, "attach_disk", rd, "nfs:90000/vm-90000-disk-0.qcow2",
		parker, 710, refusal)
	if err == nil || !errors.Is(err, refusal) || cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err %v; the parker-named volume is refused with PVE's answer kept", err)
	}
	if err := replicaRefusalVerdict(context.Background(), deps, "attach_disk", rd, "nfs:700/vm-700-disk-0.qcow2",
		parker, 710, refusal); err != nil {
		t.Fatalf("birth-named volume refused: %v", err)
	}
}

// TestReplicaRefusalVerdictRetriesARefusalThatPasses checks that a refusal a
// retry would not meet, because the VM's job is marked for removal or gone
// since, is retriable, and so is one whose jobs cannot be read to tell, and
// one whose storage cannot be classified for a failed lookup.
func TestReplicaRefusalVerdictRetriesARefusalThatPasses(t *testing.T) {
	parker := pve.DiskHolder{Found: true, IsParker: true, VMID: 90000, Node: "n1"}
	rd := resolvedDisk{diskCID: "d", stableID: "s"}
	cases := map[string]func(repl *fakeReplica, deps *Deps){
		"job marked for removal": func(repl *fakeReplica, _ *Deps) { repl.jobs[0].RemoveJob = "full" },
		"job gone since":         func(repl *fakeReplica, _ *Deps) { repl.jobs = nil },
		"job list unreadable":    func(repl *fakeReplica, _ *Deps) { repl.listErr = errors.New("connection reset") },
		"storage lookup failed": func(_ *fakeReplica, deps *Deps) {
			deps.Resolver = failingResolver{err: cpierrors.Retriable("storage index: connection reset")}
		},
	}
	for name, mutate := range cases {
		repl := newFakeReplica()
		repl.addJob("710-0", "n1", "n2", "operator job")
		deps := Deps{Config: &config.CPIConfig{Node: "n1"}, Logger: log.NewNopLogger(), Replication: repl}
		deps.Resolver = infoResolver{base: backendResolverOrDefault(deps),
			info: pve.StorageInfo{Type: pve.StorageTypeNFS, Shared: true}}
		mutate(repl, &deps)
		err := replicaRefusalVerdict(context.Background(), deps, "attach_disk", rd,
			"nfs:90000/vm-90000-disk-0.qcow2", parker, 710, pve.ErrMoveDiskReplicationRefused)
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("%s: err %v, want a retriable error", name, err)
		}
	}
}

// failingResolver is a backend resolver whose every lookup fails with err.
type failingResolver struct{ err error }

func (f failingResolver) Resolve(context.Context, string) (pve.Backend, error) { return nil, f.err }

// TestAttachPreflightRefusesReplicatedReassign checks that the HA preflight
// refuses, from profile facts alone, an attach PVE would refuse, so the VM
// never leaves HA for it.
func TestAttachPreflightRefusesReplicatedReassign(t *testing.T) {
	profile := attachProfile(config.HAModeAuto)
	profile.Replication = []ha.ReplicationJob{{ID: "100-0", Guest: 100, Source: "pve01", Target: "pve02"}}
	parker := pve.DiskHolder{Found: true, IsParker: true, VMID: 90000, Node: "pve01"}
	h := &attachHA{profile: profile, vmid: 100,
		disk: resolvedDisk{diskCID: "d", volid: "nfs2:90000/vm-90000-disk-0.qcow2", stableID: "s"}}
	err := h.refuseReplicatedAttach(context.Background(), parker)
	if err == nil || !strings.Contains(err.Error(), "the replication job 100-0") {
		t.Fatalf("err %v; want the refusal naming the job", err)
	}
	h.disk.volid = "nfs2:700/vm-700-disk-0.qcow2"
	if err := h.refuseReplicatedAttach(context.Background(), parker); err != nil {
		t.Fatalf("birth-named disk refused: %v", err)
	}
	h.disk.volid = "nfs2:90000/vm-90000-disk-0.qcow2"
	profile.ReplicationErr = "read failed"
	if err := h.refuseReplicatedAttach(context.Background(), parker); err != nil {
		t.Fatalf("an unread job list refused: %v", err)
	}
	profile.ReplicationErr = ""
	if err := h.refuseReplicatedAttach(context.Background(), pve.DiskHolder{}); err != nil {
		t.Fatalf("a free-floating disk refused: %v", err)
	}
}

// TestAttachToAReplicateVMWithSourceOnlyPinIsGuarded is the lab shape. The
// VM's pin names its source alone because no sync has been verified yet, and
// an NFS disk attaches. The source-only pin no longer bounds the replica
// partner, so the decision keeps the VM in HA behind the barrier rather than
// taking it out of HA.
func TestAttachToAReplicateVMWithSourceOnlyPinIsGuarded(t *testing.T) {
	deps, stub, profile := attachRig(config.HAModeAuto)
	wait := 60
	deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyReplicate)
	deps.Config.HA.MotionWaitSec = &wait
	profile.Storages = append(profile.Storages,
		pve.StorageInfo{Name: "local-zfs", Type: pve.StorageTypeZFSPool, Content: "images"})
	profile.Replication = []ha.ReplicationJob{{ID: "100-0", Guest: 100, Source: "pve01", Target: "pve02"}}
	repl := newFakeReplica()
	repl.addJob("100-0", "pve01", "pve02", renderReplicaJobMarker(aaDirector, 1))
	deps.Replication = repl
	cfg := map[string]any{"scsi0": replicaRootVolume + ",size=10G", "cpu": "x86-64-v3"}
	deps.PVE.(*icPVEClient).qemuSvc = &icQEMUService{configFn: func(context.Context, string, int) (map[string]any,
		error,
	) {
		return cfg, nil
	}}
	comment := ha.WithVolumeRecord(haIdentity{Owner: aaDirector}.marker(), []string{"local-zfs"})
	nodes, strict := "pve01", true
	stub.rules["bosh-na-100"] = cluster.CreateHaRulesParams{Rule: "bosh-na-100", Type: naRuleType,
		Resources: "vm:100", Comment: &comment, Nodes: &nodes, Strict: &strict}
	h, err := decideAttachHA(context.Background(), deps, "100", sharedDisk())
	if err != nil || h == nil {
		t.Fatalf("decision: h %+v err %v", h, err)
	}
	if h.action == attachHALeave {
		t.Fatalf("decision takes the VM out of HA for an NFS attach: %+v", h)
	}
	if h.action == attachHAGuarded && !slices.Contains(h.newPin.NodeNames(), "pve01") {
		t.Fatalf("new pin %v drops the source", h.newPin.NodeNames())
	}
}

// TestManagedAttachOfAParkerNamedDiskWaitsForALeavingJob is the attach of a
// disk a parker holds under its own name into a VM whose only job is marked
// for removal. PVE refuses the reassignment while the job is configured, and
// the refusal is retriable because pvesr removes the job on its next run. Once
// the job is gone, the same attach lands by reassignment.
func TestManagedAttachOfAParkerNamedDiskWaitsForALeavingJob(t *testing.T) {
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
	deps.Resolver = infoResolver{base: backendResolverOrDefault(deps),
		info: pve.StorageInfo{Type: pve.StorageTypeNFS, Shared: true, Content: "images"}}
	repl := newFakeReplica()
	repl.addJob("777-0", "n1", "n2", "operator job")
	repl.jobs[0].RemoveJob = "full"
	deps.Replication, client.replication = repl, repl
	client.moveCalls = 0
	err := digestAttach(t, deps, cid)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !strings.Contains(err.Error(), "777-0") {
		t.Fatalf("err %v; a refusal from a job on its way out is retriable", err)
	}
	if holder, _ := diskHolderBySerial(t, client, serial); holder != parker || len(client.destroyed) != 0 {
		t.Fatalf("VM %d holds the disk, destroyed %v; the refused attach leaves it on parker %d", holder,
			client.destroyed, parker)
	}
	repl.jobs = nil
	if err := digestAttach(t, deps, cid); err != nil {
		t.Fatalf("attach once the job is gone: %v", err)
	}
	if holder, volume := diskHolderBySerial(t, client, serial); holder != 777 ||
		!strings.Contains(volume, "vm-777-") {
		t.Fatalf("VM %d holds %s, want VM 777 under its own name", holder, volume)
	}
	record, err := jr.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State == aj.ReconciliationRequired {
		t.Fatalf("the retried attach left the record asking for reconciliation: %q", record.Reason)
	}
}

// TestManagedAttachRetiresTheJobOfAVMOutOfHA is the attach of a disk a parker
// holds under its own name into a VM that left HA under a release that kept
// its replicate tag and this Director's job. The attach retires the job with
// cleanup and removes the tag before the disk enters, so the reassignment
// lands rather than meeting PVE's refusal.
func TestManagedAttachRetiresTheJobOfAVMOutOfHA(t *testing.T) {
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
	deps.HAProfile = func(context.Context) (*ha.Profile, error) { return profile, nil }
	deps.Resolver = infoResolver{base: backendResolverOrDefault(deps),
		info: pve.StorageInfo{Type: pve.StorageTypeNFS, Shared: true, Content: "images"}}
	client.state.configs[777]["tags"] = "bosh-cpi;" + haReplicaTag
	repl := newFakeReplica()
	repl.source = "n1"
	repl.removeOnSchedule = true
	repl.addJob("777-0", "n1", "n2", renderReplicaJobMarker(aaDirector, 1))
	deps.Replication, client.replication = repl, repl
	client.moveCalls = 0
	_, err := HandleAttachDisk(deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, "777"), planJSON(t, cid)},
		jsonrpc.Context{DirectorUUID: aaDirector})
	if err != nil {
		t.Fatalf("attach into the VM out of HA: %v", err)
	}
	if !repl.called("delete:777-0:force=false") || repl.called("delete:777-0:force=true") || len(repl.jobs) != 0 {
		t.Fatalf("calls %v jobs %v; the job goes with cleanup and is never forced", repl.calls, repl.jobs)
	}
	if tags, _ := pve.ConfigString(client.state.configs[777], "tags"); hasReplicaTag(tags) {
		t.Fatalf("tags %q keep the replicate tag", tags)
	}
	if holder, volume := diskHolderBySerial(t, client, serial); holder != 777 ||
		!strings.Contains(volume, "vm-777-") || client.replicationAnswers != 0 {
		t.Fatalf("VM %d holds %s after %d refusals, want VM 777 under its own name by reassignment", holder,
			volume, client.replicationAnswers)
	}
	record, err := jr.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State == aj.ReconciliationRequired {
		t.Fatalf("the attach left the record asking for reconciliation: %q", record.Reason)
	}
}

// TestManagedAttachSettlesAMoveTaskThatMeetsAJobSetUpAfterTheRequest has
// PVE accept the reassignment and the task exit with the replication
// refusal, because a job was set up between the request and the task's own
// check under the configuration locks. Nothing moved, so the move step
// settles, the attach takes the config edit, and the record does not ask for
// reconciliation.
func TestManagedAttachSettlesAMoveTaskThatMeetsAJobSetUpAfterTheRequest(t *testing.T) {
	deps, client, jr, id, cid := lifecycleFlowFixture(t)
	deps.Config.DetachedDiskStrategy = "parked"
	deps.Logger = log.NewNopLogger()
	deps.Agent = legacyDestroyAgent{}
	volume, parker := parkReplicatedFixture(t, deps, client, cid)
	client.replicatedVMs = nil
	client.moveTaskExit = "Cannot move disk to a replicated VM. Storage does not support replication!"
	if err := digestAttach(t, deps, cid); err != nil {
		t.Fatalf("attach after the task's replication refusal: %v", err)
	}
	if client.moveCalls != 1 {
		t.Fatalf("%d move POSTs; the refused task is not sent again", client.moveCalls)
	}
	assertAttachedByConfigEdit(t, client, jr, id, volume, parker)
}
