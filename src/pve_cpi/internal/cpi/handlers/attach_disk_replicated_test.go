package handlers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
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
	deps := Deps{Config: &config.CPIConfig{Node: "n1"}, Logger: log.NewNopLogger()}
	deps.Resolver = infoResolver{base: backendResolverOrDefault(deps),
		info: pve.StorageInfo{Type: pve.StorageTypeZFSPool, Shared: true}}
	parker := pve.DiskHolder{Found: true, IsParker: true, VMID: 90000, Node: "n1"}
	refusal := pve.ErrMoveDiskReplicationRefused
	rd := resolvedDisk{diskCID: "d", stableID: "s"}
	err := replicaRefusalVerdict(context.Background(), deps, "attach_disk", rd, "nfs:90000/vm-90000-disk-0.qcow2",
		parker, 710, refusal)
	if err == nil || !errors.Is(err, refusal) {
		t.Fatalf("err %v; the parker-named volume is refused with PVE's answer kept", err)
	}
	if err := replicaRefusalVerdict(context.Background(), deps, "attach_disk", rd, "nfs:700/vm-700-disk-0.qcow2",
		parker, 710, refusal); err != nil {
		t.Fatalf("birth-named volume refused: %v", err)
	}
}

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
