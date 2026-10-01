package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	cs "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	ns "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// The move fixtures define these storages. "a" is an NFS export, "local" is a
// node-local dir, and "d" is a dir that later tests flag shared.
var moveDefinitions = map[string]string{
	"a":     `{"storage":"a","type":"nfs","server":"nas","export":"/a","content":"images,iso","shared":1}`,
	"local": `{"storage":"local","type":"dir","path":"/var/lib/vz","content":"images,iso"}`,
	"d":     `{"storage":"d","type":"dir","path":"/mnt/d","content":"images"}`,
}

func moveDefinition(t *testing.T, raw string) pve.StorageInfo {
	t.Helper()
	def, err := pve.ParseStorageEntry(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func moveAllocationID(t *testing.T) string {
	t.Helper()
	id, err := aj.NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func moveStorage(volume string) string {
	storage, _, _ := strings.Cut(volume, ":")
	return storage
}

// movePlan freezes the definitions of exactly the storages a record uses, so
// an unused storage never raises a historical-definition issue.
func movePlan(t *testing.T, key, node string, frozen map[string]pve.StorageInfo, volumes ...string) aj.Intent {
	t.Helper()
	definitions := map[string]pve.StorageInfo{}
	for _, volume := range volumes {
		definitions[moveStorage(volume)] = frozen[moveStorage(volume)]
	}
	plan := StorageAllocationPlan{Version: 1, Namespace: "director", AllocationKey: key, PolicyFingerprint: strings.Repeat("a", 64), Node: node, Definitions: definitions}
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return aj.Intent{PolicyFingerprint: plan.PolicyFingerprint, PlanVersion: 1, Plan: payload}
}

// moveVMRecord is a returned VM allocation with one observed create step per
// volume on node.
func moveVMRecord(t *testing.T, agent, node string, vmid int, frozen map[string]pve.StorageInfo, volumes ...string) aj.Record {
	t.Helper()
	record := aj.Record{ID: moveAllocationID(t), Namespace: "director", Kind: "vm", AgentID: agent, State: aj.ReadyToReturn, CID: strconv.Itoa(vmid), Intent: movePlan(t, agent, node, frozen, volumes...)}
	for index, volume := range volumes {
		storage := moveStorage(volume)
		record.Steps = append(record.Steps, aj.Step{ID: fmt.Sprintf("create-%d", index), Kind: "vm.QEMU.Create", State: aj.Observed, Target: aj.Target{Node: node, VMID: vmid, Storage: storage, Backing: frozen[storage].BackingKey()}, VolIDs: []string{volume}})
	}
	return record
}

// moveDiskRecord is a returned persistent disk created on node.
func moveDiskRecord(t *testing.T, node, volume string, frozen map[string]pve.StorageInfo) aj.Record {
	t.Helper()
	storage := moveStorage(volume)
	return aj.Record{ID: moveAllocationID(t), Namespace: "director", Kind: allocationKindDisk, State: aj.ReadyToReturn, CID: volume, Intent: movePlan(t, "disk", node, frozen, volume), Steps: []aj.Step{{ID: "create", Kind: "create_persistent_volume", State: aj.Observed, Target: aj.Target{Node: node, Storage: storage, Backing: frozen[storage].BackingKey(), IntendedVolume: volume}, VolIDs: []string{volume}}}}
}

func moveMarker(t *testing.T, allocationID, agent string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(agent))
	marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Kind: "vm", Namespace: "director", AllocationID: allocationID, AgentSHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatal(err)
	}
	return marker
}

// moveDescription renders a VM description that carries the allocation
// marker and the given disk provenance, the way a managed VM holds both.
func moveDescription(t *testing.T, marker string, disks ...pve.DiskAllocationProvenance) string {
	t.Helper()
	if len(disks) == 0 {
		return marker
	}
	entries := map[string]pve.DiskAllocationProvenance{}
	for index, disk := range disks {
		entries[fmt.Sprintf("bpd-%d", index)] = disk
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	description, err := pve.RenderSentinel(marker, map[string]json.RawMessage{"bosh_disk_allocations": raw})
	if err != nil {
		t.Fatal(err)
	}
	return description
}

var moveNodes = []string{"pve1", "pve2", "pve3"}

// moveFixture is VM 123 of one returned VM allocation, recorded on pve1 and
// found on another node, with a persistent disk whose provenance names pve1.
// Each field describes one variation; build turns them into records, the VM
// configuration, and listings that agree with each other.
type moveFixture struct {
	t    *testing.T
	deps Deps
	c    *allocationAuditClient

	rootStorage, diskStorage string
	// iso, when set, names the storage of a config ISO in ide2.
	iso string
	// node is where the VM is found; recorded is where its record says.
	node  string
	state aj.State
	// markerAgent is the agent whose digest the VM's marker carries.
	markerAgent string
	// frozen and current map a storage to the definition the plans froze
	// and the one PVE reports now, as raw JSON.
	frozen, current map[string]string
	// noProvenance leaves the persistent disk's provenance off the VM.
	noProvenance bool
	// configExtra adds config slots that no record owns.
	configExtra map[string]string
	// vmstate sets a top-level vmstate volume.
	vmstate string
	// held lists volumes outside the config, such as snapshot volumes, that
	// the listings serve.
	held []string
	// unlisted drops volumes from every listing on the VM's node.
	unlisted map[string]bool
	// offline reports one node offline in /cluster/status.
	offline string
	// clone places a second VM with this VMID and the same marker on pve3.
	clone int
	// activeVMID, when set, opens a second attempt whose only step targets
	// that VMID, which closes every create step.
	activeVMID int

	vm, disk aj.Record
	root     string
	eph      string
	pdisk    string
	isoVol   string
}

func newMoveFixture(t *testing.T) *moveFixture {
	t.Helper()
	deps, _, c := auditFixture(t)
	return &moveFixture{t: t, deps: deps, c: c, rootStorage: "a", diskStorage: "a", node: "pve2", state: aj.ReadyToReturn, markerAgent: "agent", frozen: maps.Clone(moveDefinitions), current: map[string]string{"a": moveDefinitions["a"], "local": moveDefinitions["local"]}}
}

func (f *moveFixture) build() []aj.Record {
	t := f.t
	frozen := map[string]pve.StorageInfo{}
	for name, raw := range f.frozen {
		frozen[name] = moveDefinition(t, raw)
	}
	f.c.storageRead.definitions = cs.ListStorageResponse{}
	for _, name := range slices.Sorted(maps.Keys(f.current)) {
		f.c.storageRead.definitions = append(f.c.storageRead.definitions, json.RawMessage(f.current[name]))
	}
	f.root = f.rootStorage + ":123/vm-123-disk-0.qcow2"
	f.eph = "a:123/vm-123-disk-1.qcow2"
	f.pdisk = f.diskStorage + ":123/vm-123-disk-2.qcow2"
	owned := []string{f.root, f.eph}
	if f.iso != "" {
		f.isoVol = f.iso + ":iso/vm-123-config.iso"
		owned = append(owned, f.isoVol)
	}
	f.vm = moveVMRecord(t, "agent", "pve1", 123, frozen, owned...)
	f.vm.State = f.state
	if f.activeVMID > 0 {
		plan := aj.AttemptPlan{Version: aj.AttemptVersion, PolicyFingerprint: f.vm.Intent.PolicyFingerprint, PlanVersion: 1, Plan: f.vm.Intent.Plan}
		f.vm.Attempts = []aj.Attempt{{Number: 0, Plan: plan}, {Number: 1, Plan: plan}}
		f.vm.Steps = append(f.vm.Steps, aj.Step{ID: "active", Attempt: 1, Kind: "vm.QEMU.Create", State: aj.Observed, Target: aj.Target{Node: "pve1", VMID: f.activeVMID}})
	}
	f.disk = moveDiskRecord(t, "pve1", f.pdisk, frozen)

	config := map[string]any{"virtio0": f.root, "scsi1": f.eph, "scsi2": f.pdisk + ",serial=bpd-0"}
	if f.isoVol != "" {
		config["ide2"] = f.isoVol + ",media=cdrom"
	}
	maps.Copy(config, stringsToAny(f.configExtra))
	if f.vmstate != "" {
		config["vmstate"] = f.vmstate
	}
	var provenance []pve.DiskAllocationProvenance
	if !f.noProvenance {
		provenance = append(provenance, pve.DiskAllocationProvenance{Version: 1, AllocationID: f.disk.ID, AllocationNamespace: "director", Volid: f.pdisk, Node: "pve1", Backing: frozen[f.diskStorage].BackingKey()})
	}
	marker := moveMarker(t, f.vm.ID, f.markerAgent)
	config["description"] = moveDescription(t, marker, provenance...)
	f.c.configs[123] = config

	f.c.clusterRead = &allocationAuditCluster{Service: f.c.idFakeClient.Cluster(), members: moveNodes}
	if f.offline != "" {
		f.c.clusterRead.status = []map[string]any{{"type": "cluster", "quorate": 1}}
		for _, node := range moveNodes {
			f.c.clusterRead.status = append(f.c.clusterRead.status, map[string]any{"type": "node", "name": node, "online": map[bool]int{true: 0, false: 1}[node == f.offline]})
		}
	}
	f.c.nodesRead.nodeNames = moveNodes
	f.c.nodesRead.guestNodes = map[int]string{123: f.node}
	if f.clone > 0 {
		f.c.configs[f.clone] = map[string]any{"description": marker, "virtio0": fmt.Sprintf("a:%d/vm-%d-disk-0.qcow2", f.clone, f.clone)}
		f.c.nodesRead.guestNodes[f.clone] = "pve3"
	}

	// Shared volumes list on every node; a node-local volume lists only on
	// the node the VM now runs on, because it moved there with the VM.
	volumes := append([]string{f.root, f.eph, f.pdisk}, f.held...)
	if f.isoVol != "" {
		volumes = append(volumes, f.isoVol)
	}
	for _, volume := range f.configExtra {
		volumes = append(volumes, volume)
	}
	if f.vmstate != "" {
		volumes = append(volumes, f.vmstate)
	}
	f.c.nodesRead.volumesByNode = map[string][]string{}
	for _, node := range moveNodes {
		for _, volume := range volumes {
			current, known := f.current[moveStorage(volume)]
			local := !known || !moveDefinition(t, current).IsShared()
			if local && node != f.node || node == f.node && f.unlisted[volume] {
				continue
			}
			f.c.nodesRead.volumesByNode[node] = append(f.c.nodesRead.volumesByNode[node], volume)
		}
	}
	return []aj.Record{f.vm, f.disk}
}

func stringsToAny(in map[string]string) map[string]any {
	out := map[string]any{}
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (f *moveFixture) audit() StorageAllocationAudit {
	f.t.Helper()
	records := f.build()
	report, err := auditStorageAllocationRecords(context.Background(), f.deps, records, nil, []string{"pve1"})
	if err != nil {
		f.t.Fatal(err)
	}
	return report
}

func findingWith(findings []string, prefix, fragment string) bool {
	return slices.ContainsFunc(findings, func(finding string) bool {
		return strings.HasPrefix(finding, prefix) && strings.Contains(finding, fragment)
	})
}

// TestAllocationAuditAcceptsSharedStorageMoves is the field case. Root
// migrated VM 4626 from pvupvecf101 to pvupvecf102 and VM 7014 from
// pvupvecf102 to pvupvecf103 on shared NFS, and VM 4626 holds a persistent
// disk whose provenance still names pvupvecf101. Every create_vm froze on the
// three conflicts that raised; the audit now reports three moves instead.
func TestAllocationAuditAcceptsSharedStorageMoves(t *testing.T) {
	deps, _, c := auditFixture(t)
	nodes := []string{"pvupvecf101", "pvupvecf102", "pvupvecf103"}
	shared := moveDefinition(t, moveDefinitions["a"])
	frozen := map[string]pve.StorageInfo{"a": shared}
	c.storageRead.definitions = cs.ListStorageResponse{json.RawMessage(moveDefinitions["a"])}
	c.clusterRead = &allocationAuditCluster{Service: c.idFakeClient.Cluster(), members: nodes}
	c.nodesRead.nodeNames = nodes
	c.nodesRead.guestNodes = map[int]string{4626: "pvupvecf102", 7014: "pvupvecf103"}

	vm4626 := moveVMRecord(t, "agent-4626", "pvupvecf101", 4626, frozen, "a:4626/vm-4626-disk-0.qcow2", "a:4626/vm-4626-disk-2.qcow2")
	disk := moveDiskRecord(t, "pvupvecf101", "a:4626/vm-4626-disk-1.qcow2", frozen)
	vm7014 := moveVMRecord(t, "agent-7014", "pvupvecf102", 7014, frozen, "a:7014/vm-7014-disk-0.qcow2", "a:7014/vm-7014-disk-1.qcow2")
	c.configs[4626] = map[string]any{
		"description": moveDescription(t, moveMarker(t, vm4626.ID, "agent-4626"), pve.DiskAllocationProvenance{Version: 1, AllocationID: disk.ID, AllocationNamespace: "director", Volid: "a:4626/vm-4626-disk-1.qcow2", Node: "pvupvecf101", Backing: shared.BackingKey()}),
		"virtio0":     "a:4626/vm-4626-disk-0.qcow2", "scsi1": "a:4626/vm-4626-disk-2.qcow2", "scsi2": "a:4626/vm-4626-disk-1.qcow2,serial=bpd-0",
	}
	c.configs[7014] = map[string]any{"description": moveMarker(t, vm7014.ID, "agent-7014"), "virtio0": "a:7014/vm-7014-disk-0.qcow2", "scsi1": "a:7014/vm-7014-disk-1.qcow2"}
	c.nodesRead.volumesByNode = map[string][]string{}
	for _, node := range nodes {
		c.nodesRead.volumesByNode[node] = []string{"a:4626/vm-4626-disk-0.qcow2", "a:4626/vm-4626-disk-1.qcow2", "a:4626/vm-4626-disk-2.qcow2", "a:7014/vm-7014-disk-0.qcow2", "a:7014/vm-7014-disk-1.qcow2"}
	}

	report, err := auditStorageAllocationRecords(context.Background(), deps, []aj.Record{vm4626, disk, vm7014}, nil, []string{"pvupvecf101"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conflicts) != 0 || len(report.Issues) != 0 || !report.Complete || !report.VMScanComplete {
		t.Fatalf("shared-storage moves still refused:\nconflicts: %s\nissues: %s", strings.Join(report.Conflicts, "\n"), strings.Join(report.Issues, "\n"))
	}
	want := []StorageAllocationMove{
		{AllocationID: vm4626.ID, Kind: "vm", VMID: 4626, RecordedNodes: []string{"pvupvecf101"}, ObservedNode: "pvupvecf102", Volumes: []string{"a:4626/vm-4626-disk-0.qcow2", "a:4626/vm-4626-disk-1.qcow2", "a:4626/vm-4626-disk-2.qcow2"}},
		{AllocationID: disk.ID, Kind: allocationKindDisk, VMID: 4626, RecordedNodes: []string{"pvupvecf101"}, ObservedNode: "pvupvecf102", Volumes: []string{"a:4626/vm-4626-disk-1.qcow2"}},
		{AllocationID: vm7014.ID, Kind: "vm", VMID: 7014, RecordedNodes: []string{"pvupvecf102"}, ObservedNode: "pvupvecf103", Volumes: []string{"a:7014/vm-7014-disk-0.qcow2", "a:7014/vm-7014-disk-1.qcow2"}},
	}
	for _, move := range want {
		if !slices.ContainsFunc(report.ObservedMoves, func(got StorageAllocationMove) bool { return reflect.DeepEqual(got, move) }) {
			t.Fatalf("move %+v missing from %+v", move, report.ObservedMoves)
		}
	}
	if len(report.ObservedMoves) != len(want) {
		t.Fatalf("unexpected moves: %+v", report.ObservedMoves)
	}
	if !report.observedMove("vm", vm4626.ID, 4626, "pvupvecf102") || !report.observedMove(allocationKindDisk, disk.ID, 4626, "pvupvecf102") || !report.observedMove("vm", vm7014.ID, 7014, "pvupvecf103") {
		t.Fatal("observedMove missed an accepted move")
	}
	if report.observedMove("vm", vm4626.ID, 4626, "pvupvecf101") || report.observedMove("vm", vm7014.ID, 7014, "pvupvecf102") {
		t.Fatal("observedMove accepted a node the VM is not on")
	}
	if err := storageAuditGateError(context.Background(), deps, "create_vm", report, storageAuditGateVMScan|storageAuditGateConflicts); err != nil {
		t.Fatalf("create_vm still refused: %v", err)
	}
	proof, err := storageAllocationVerification(report, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(proof.EvidenceJSON, `"observed_moves":[`) || !strings.Contains(proof.EvidenceJSON, `"observed_node":"pvupvecf103"`) {
		t.Fatalf("verification evidence lost the moves: %s", proof.EvidenceJSON)
	}
}

// TestAllocationAuditMoveRulesRefuseUnsafeMoves pins each condition of the
// VM and disk rules. A refused move keeps its conflict, and the conflict
// says why the move was not accepted.
func TestAllocationAuditMoveRulesRefuseUnsafeMoves(t *testing.T) {
	const vmConflict = "remote allocation "
	const diskConflict = "disk ownership provenance disagrees with actual holder node"
	for _, tc := range []struct {
		name   string
		mutate func(f *moveFixture)
		// vmReason and diskReason are the refusal fragments; an empty one
		// means that move must be accepted.
		vmReason, diskReason string
		// noDiskMove marks a variation that leaves no disk provenance to move.
		noDiskMove bool
	}{
		{name: "shared baseline is accepted", mutate: func(*moveFixture) {}},
		{name: "shared snapshots are accepted", mutate: func(f *moveFixture) {
			f.held = []string{"a:123/vm-123-state-pre.raw"}
			f.c.nodesRead.snapshots = map[int]map[string]map[string]any{123: {"pre": {"virtio0": "a:123/vm-123-disk-0.qcow2", "vmstate": "a:123/vm-123-state-pre.raw"}}}
		}},
		{name: "node-local root disk", mutate: func(f *moveFixture) { f.rootStorage = "local" },
			vmReason: "volume local:123/vm-123-disk-0.qcow2 is node-local"},
		{name: "node-local persistent disk", mutate: func(f *moveFixture) { f.diskStorage = "local" },
			vmReason: "volume local:123/vm-123-disk-2.qcow2 is node-local", diskReason: "volume local:123/vm-123-disk-2.qcow2 is node-local"},
		{name: "node-local config ISO", mutate: func(f *moveFixture) { f.iso = "local" },
			vmReason: "volume local:iso/vm-123-config.iso is node-local"},
		{name: "node-local vmstate of a hibernated VM", mutate: func(f *moveFixture) { f.vmstate = "local:123/vm-123-state-suspend.raw" },
			vmReason: "volume local:123/vm-123-state-suspend.raw is node-local"},
		{name: "local vmstate in a snapshot", mutate: func(f *moveFixture) {
			f.held = []string{"local:123/vm-123-state-pre.raw"}
			f.c.nodesRead.snapshots = map[int]map[string]map[string]any{123: {"pre": {"virtio0": "a:123/vm-123-disk-0.qcow2", "vmstate": "local:123/vm-123-state-pre.raw"}}}
		}, vmReason: "volume local:123/vm-123-state-pre.raw is node-local"},
		{name: "local volume only in a snapshot", mutate: func(f *moveFixture) {
			f.held = []string{"local:123/vm-123-disk-7.qcow2"}
			f.c.nodesRead.snapshots = map[int]map[string]map[string]any{123: {"pre": {"scsi3": "local:123/vm-123-disk-7.qcow2"}}}
		}, vmReason: "volume local:123/vm-123-disk-7.qcow2 is node-local"},
		{name: "failed snapshot read", mutate: func(f *moveFixture) {
			f.c.nodesRead.snapshots = map[int]map[string]map[string]any{123: {"pre": {}}}
			f.c.nodesRead.snapshotFailure = map[string]error{"pre": sdkerrors.ParseAPIError(500, []byte(`{"message":"snapshot config unreadable"}`))}
		}, vmReason: `snapshot "pre" of VM 123 could not be read: HTTP 500: snapshot config unreadable`},
		{name: "failed snapshot listing", mutate: func(f *moveFixture) {
			f.c.nodesRead.snapshotListFailure = map[int]error{123: errors.New("transport secret-password")}
		}, vmReason: "the snapshots of VM 123 could not be listed: unclassified error"},
		{name: "node list excludes the new node", mutate: func(f *moveFixture) {
			f.current["a"] = strings.Replace(moveDefinitions["a"], `"shared":1`, `"shared":1,"nodes":"pve1,pve3"`, 1)
		}, vmReason: `storage "a" of volume a:123/vm-123-disk-0.qcow2 is not available on pve2`, diskReason: `storage "a" of volume a:123/vm-123-disk-2.qcow2 is not available on pve2`},
		{name: "volume missing from the new node's listing", mutate: func(f *moveFixture) {
			f.unlisted = map[string]bool{"a:123/vm-123-disk-2.qcow2": true}
		}, vmReason: `volume a:123/vm-123-disk-2.qcow2 was not listed on storage "a" on pve2`, diskReason: `volume a:123/vm-123-disk-2.qcow2 was not listed on storage "a" on pve2`},
		{name: "storage flipped to shared after the plan froze", mutate: func(f *moveFixture) {
			f.rootStorage = "d"
			f.current["d"] = strings.Replace(moveDefinitions["d"], `"content":"images"`, `"content":"images","shared":1`, 1)
		}, vmReason: `storage "d" of volume d:123/vm-123-disk-0.qcow2 was node-local when its plan was frozen`},
		{name: "backing changed since the plan froze", mutate: func(f *moveFixture) {
			f.frozen["a"] = strings.Replace(moveDefinitions["a"], `"export":"/a"`, `"export":"/old"`, 1)
		}, vmReason: `the backing of storage "a" changed since the plan for volume a:123/vm-123-disk-0.qcow2 was frozen`, diskReason: `the backing of storage "a" changed since the plan for volume a:123/vm-123-disk-2.qcow2 was frozen`},
		{name: "VMID outside the active attempt", mutate: func(f *moveFixture) { f.activeVMID = 124 },
			vmReason: "VM 123 is not a target of the record's active attempt"},
		{name: "agent digest differs", mutate: func(f *moveFixture) { f.markerAgent = "other-agent" },
			vmReason: "the VM carries a different agent digest"},
		{name: "duplicate sighting", mutate: func(f *moveFixture) { f.clone = 124 },
			vmReason: "the allocation marker was sighted 2 times"},
		{name: "incomplete VM scan", mutate: func(f *moveFixture) { f.offline = "pve3" },
			vmReason: "the VM scan is incomplete", diskReason: "the VM scan is incomplete"},
		{name: "record still submitted", mutate: func(f *moveFixture) { f.state = aj.Submitted },
			vmReason: "the record is in state submitted, not ready_to_return or adopted"},
		{name: "volume owned by no record", mutate: func(f *moveFixture) { f.configExtra = map[string]string{"scsi5": "a:123/vm-123-disk-9.qcow2"} },
			vmReason: "volume a:123/vm-123-disk-9.qcow2 is owned by no journal record of this VM"},
		{name: "persistent disk without provenance on the VM", mutate: func(f *moveFixture) { f.noProvenance = true },
			vmReason: "volume a:123/vm-123-disk-2.qcow2 is owned by no journal record of this VM", noDiskMove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMoveFixture(t)
			tc.mutate(f)
			report := f.audit()
			vmPrefix := vmConflict + f.vm.ID + " (VM 123) is outside recorded mutation targets"
			if tc.vmReason != "" {
				if !findingWith(report.Conflicts, vmPrefix, "; not accepted as a move because "+tc.vmReason) {
					t.Fatalf("VM move not refused with %q:\n%s", tc.vmReason, strings.Join(report.Conflicts, "\n"))
				}
				if report.observedMove("vm", f.vm.ID, 123, "pve2") {
					t.Fatal("refused VM move still reported as observed")
				}
			} else if !report.observedMove("vm", f.vm.ID, 123, "pve2") {
				t.Fatalf("VM move refused:\n%s", strings.Join(report.Conflicts, "\n"))
			}
			if tc.diskReason != "" {
				if !findingWith(report.Conflicts, diskConflict, "; not accepted as a move because "+tc.diskReason) {
					t.Fatalf("disk move not refused with %q:\n%s", tc.diskReason, strings.Join(report.Conflicts, "\n"))
				}
				if report.observedMove(allocationKindDisk, f.disk.ID, 123, "pve2") {
					t.Fatal("refused disk move still reported as observed")
				}
			} else if !tc.noDiskMove && !report.observedMove(allocationKindDisk, f.disk.ID, 123, "pve2") {
				t.Fatalf("disk move refused:\n%s", strings.Join(report.Conflicts, "\n"))
			}
			gate := storageAuditGateError(context.Background(), f.deps, "create_vm", report, storageAuditGateVMScan|storageAuditGateConflicts)
			switch {
			case tc.vmReason == "" && tc.diskReason == "" && (gate != nil || len(report.Conflicts) != 0):
				t.Fatalf("safe move refused: %v", gate)
			case (tc.vmReason != "" || tc.diskReason != "") && gate == nil:
				t.Fatal("create_vm admitted an unsafe move")
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "secret-password") {
				t.Fatal("transport error text reached the report")
			}
		})
	}
}

// moveParkerFixture parks disk allocation D on parker VM 90001, recorded on
// pve1 and found on pve2 after a bulk migrate, with the volume on storage.
func moveParkerFixture(t *testing.T, storage string) (Deps, []aj.Record, aj.Record, string) {
	t.Helper()
	deps, _, c := auditFixture(t)
	frozen := map[string]pve.StorageInfo{"a": moveDefinition(t, moveDefinitions["a"]), "local": moveDefinition(t, moveDefinitions["local"])}
	c.storageRead.definitions = cs.ListStorageResponse{json.RawMessage(moveDefinitions["a"]), json.RawMessage(moveDefinitions["local"])}
	c.clusterRead = &allocationAuditCluster{Service: c.idFakeClient.Cluster(), members: moveNodes}
	c.nodesRead.nodeNames = moveNodes
	c.nodesRead.guestNodes = map[int]string{90001: "pve2"}
	volume := storage + ":90001/vm-90001-disk-0.qcow2"
	disk := moveDiskRecord(t, "pve1", volume, frozen)
	disk.Steps = append(disk.Steps, aj.Step{ID: "park", Kind: "park_QEMU_AttachDisk", State: aj.Observed, Target: aj.Target{Node: "pve1", VMID: 90001, Storage: storage, Backing: frozen[storage].BackingKey(), IntendedVolume: volume}})
	parked, err := json.Marshal(map[string]any{"bpd-p": map[string]any{"allocation_id": disk.ID, "allocation_namespace": "director", "allocation_backing": frozen[storage].BackingKey(), "disk_cid": volume, "parked_at": "2026-09-18T02:00:00Z", "node": "pve1", "volid": volume, "slot": "scsi1"}})
	if err != nil {
		t.Fatal(err)
	}
	description, err := pve.RenderSentinel("bosh parker", map[string]json.RawMessage{"bosh_parked_disks": parked})
	if err != nil {
		t.Fatal(err)
	}
	c.configs[90001] = map[string]any{"description": description, "scsi1": volume + ",serial=bpd-p"}
	c.nodesRead.volumesByNode = map[string][]string{"pve2": {volume}}
	if storage == "a" {
		c.nodesRead.volumesByNode["pve1"] = []string{volume}
		c.nodesRead.volumesByNode["pve3"] = []string{volume}
	}
	return deps, []aj.Record{disk}, disk, volume
}

// TestAllocationAuditAcceptsParkerMovedOnSharedStorage covers a bulk migrate
// during a patch cycle, which moves stopped parker VMs along with the rest.
func TestAllocationAuditAcceptsParkerMovedOnSharedStorage(t *testing.T) {
	deps, records, disk, volume := moveParkerFixture(t, "a")
	report, err := auditStorageAllocationRecords(context.Background(), deps, records, nil, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conflicts) != 0 || !report.VMScanComplete {
		t.Fatalf("parker move refused:\n%s", strings.Join(report.Conflicts, "\n"))
	}
	want := []StorageAllocationMove{{AllocationID: disk.ID, Kind: storageMoveKindParker, VMID: 90001, RecordedNodes: []string{"pve1"}, ObservedNode: "pve2", Volumes: []string{volume}}}
	if !reflect.DeepEqual(report.ObservedMoves, want) {
		t.Fatalf("moves = %+v, want %+v", report.ObservedMoves, want)
	}
}

func TestAllocationAuditRefusesParkerMovedWithLocalStorage(t *testing.T) {
	deps, records, disk, volume := moveParkerFixture(t, "local")
	report, err := auditStorageAllocationRecords(context.Background(), deps, records, nil, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if !findingWith(report.Conflicts, "disk ownership provenance disagrees with actual holder node: disk allocation "+disk.ID, "; not accepted as a move because volume "+volume+" is node-local") {
		t.Fatalf("local parker move not refused:\n%s", strings.Join(report.Conflicts, "\n"))
	}
	if len(report.ObservedMoves) != 0 {
		t.Fatalf("local parker move accepted: %+v", report.ObservedMoves)
	}
}

// TestAllocationAuditRefusedMoveBriefNamesTheReason pins the short form a
// gate error shows, so an operator reading bosh tasks learns why too.
func TestAllocationAuditRefusedMoveBriefNamesTheReason(t *testing.T) {
	f := newMoveFixture(t)
	f.rootStorage = "local"
	report := f.audit()
	err := storageAuditGateError(context.Background(), f.deps, "create_vm", report, storageAuditGateVMScan|storageAuditGateConflicts)
	if err == nil || !strings.Contains(err.Error(), "VM 123 on pve2, recorded pve1 (node_mismatch); not a move: volume local:123/vm-123-disk-0.qcow2 is node-local") {
		t.Fatalf("gate error lacks the refusal brief: %v", err)
	}
}

// TestStorageAuditGateErrorLogsObservedMoves pins the observed_moves field of
// the gate's Error log, so a refusal on other grounds still shows the moves
// the same audit accepted.
func TestStorageAuditGateErrorLogsObservedMoves(t *testing.T) {
	logger, observed := log.NewObservedLogger(slog.LevelDebug)
	report := gateTestReport()
	report.ObservedMoves = []StorageAllocationMove{{AllocationID: "180f7d1e-08e3-437d-8163-7c9bdfe00dc9", Kind: "vm", VMID: 4626, RecordedNodes: []string{"pvupvecf101"}, ObservedNode: "pvupvecf102"}}
	if err := storageAuditGateError(context.Background(), Deps{Logger: logger}, "create_vm", report, storageAuditGateConflicts); err == nil {
		t.Fatal("failing report admitted")
	}
	entries := observed.All()
	if len(entries) != 1 {
		t.Fatalf("want one log entry, got %+v", entries)
	}
	if got, _ := entries[0].Attrs["observed_moves"].(string); got != "vm allocation 180f7d1e-08e3-437d-8163-7c9bdfe00dc9 (VM 4626) moved from pvupvecf101 to pvupvecf102" {
		t.Fatalf("observed_moves = %q", got)
	}
}

// TestAllocationAuditObservedMovesSerializeWithoutOmitempty pins the JSON tag
// and that a clean report still names the field, like issues and conflicts.
func TestAllocationAuditObservedMovesSerializeWithoutOmitempty(t *testing.T) {
	deps, j, _ := auditFixture(t)
	report, err := AuditStorageAllocations(context.Background(), deps, j, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"observed_moves":null`) {
		t.Fatalf("observed_moves missing from a clean report: %s", encoded)
	}
	proof, err := storageAllocationVerification(report, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(proof.EvidenceJSON, "observed_moves") {
		t.Fatalf("evidence without moves changed shape: %s", proof.EvidenceJSON)
	}
}

// TestAllocationAuditMoveRulesNeverWriteTheJournal runs an accepted move and
// a refused one through a real journal and checks that no record changed.
func TestAllocationAuditMoveRulesNeverWriteTheJournal(t *testing.T) {
	deps, j, c, record, _ := resumeVMFixture(t)
	twoNodeCluster(c.allocationAuditClient)
	c.nodesRead.guestNodes[123] = "pve2"
	// resumeVMNodes answers GET /nodes with pve1 alone, so the audit learns
	// of pve2 from the nodes it is asked to inspect.
	nodes := []string{"pve1", "pve2"}
	before, err := j.List()
	if err != nil {
		t.Fatal(err)
	}
	c.nodesRead.volumesByNode = map[string][]string{"pve1": {"a:123/vm-123-disk-0.qcow2", "a:123/vm-123-disk-1.qcow2"}, "pve2": {"a:123/vm-123-disk-0.qcow2", "a:123/vm-123-disk-1.qcow2"}}
	accepted, err := AuditStorageAllocations(context.Background(), deps, j, nodes)
	if err != nil {
		t.Fatal(err)
	}
	if !accepted.observedMove("vm", record.ID, 123, "pve2") || len(accepted.Conflicts) != 0 {
		t.Fatalf("journal-backed move refused: %v", accepted.Conflicts)
	}
	c.nodesRead.volumesByNode["pve2"] = nil
	refused, err := AuditStorageAllocations(context.Background(), deps, j, nodes)
	if err != nil {
		t.Fatal(err)
	}
	if refused.observedMove("vm", record.ID, 123, "pve2") || len(refused.Conflicts) == 0 {
		t.Fatal("unlisted move accepted")
	}
	after, err := j.List()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("the audit changed journal records")
	}
	if len(c.descWrites) != 0 || len(c.destroyed) != 0 {
		t.Fatal("the audit mutated PVE")
	}
}

// TestAllocationAuditKeepsListedVolumes pins the listing evidence: each
// successful (node, storage) listing keeps its volids, and a failed or
// malformed listing keeps nothing, because it proves nothing.
func TestAllocationAuditKeepsListedVolumes(t *testing.T) {
	deps, j, c := auditFixture(t)
	c.storageRead.definitions = cs.ListStorageResponse{json.RawMessage(moveDefinitions["a"]), json.RawMessage(moveDefinitions["local"])}
	c.nodesRead.nodeNames = moveNodes
	c.nodesRead.volumesByNode = map[string][]string{"pve1": {"a:1/vm-1-disk-0.raw", "local:1/vm-1-disk-1.raw"}, "pve3": {"a:1/vm-1-disk-0.raw"}}
	c.nodesRead.failureByNode = map[string]error{"pve2": errors.New("listing failed")}
	c.nodesRead.malformedListing = map[string]bool{"pve3/a": true}
	report, err := AuditStorageAllocations(context.Background(), deps, j, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[storageAuditTarget]map[string]bool{
		{node: "pve1", storage: "a"}:     {"a:1/vm-1-disk-0.raw": true},
		{node: "pve1", storage: "local"}: {"local:1/vm-1-disk-1.raw": true},
		{node: "pve3", storage: "local"}: {},
	}
	if !reflect.DeepEqual(report.listed, want) {
		t.Fatalf("listed = %v, want %v", report.listed, want)
	}
	// The listing evidence is unserialized; the JSON shape is unchanged.
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "listed") {
		t.Fatalf("listing evidence was serialized: %s", encoded)
	}
}

// TestAllocationAuditKeepsVMInventory pins what the VM scan keeps of each
// configuration for the move rules.
func TestAllocationAuditKeepsVMInventory(t *testing.T) {
	deps, j, c := auditFixture(t)
	id := moveAllocationID(t)
	description := moveDescription(t, moveMarker(t, id, "agent"), pve.DiskAllocationProvenance{Version: 1, AllocationID: findingAllocationID, AllocationNamespace: "director", Volid: "a:123/vm-123-disk-2.qcow2", Node: "pve1", Backing: "nfs://nas/a"})
	c.configs[123] = map[string]any{"description": description, "virtio0": "a:123/vm-123-disk-0.qcow2,size=10G", "ide2": "none,media=cdrom", "scsi2": "a:123/vm-123-disk-2.qcow2", "vmstate": "a:123/vm-123-state-suspend.raw"}
	c.configs[124] = map[string]any{"virtio0": "not-a-volume"}
	report, err := AuditStorageAllocations(context.Background(), deps, j, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	byVMID := map[int]storageAuditVM{}
	for _, vm := range report.vms {
		byVMID[vm.vmid] = vm
	}
	want := storageAuditVM{node: "pve1", vmid: 123, volumes: map[string]string{"virtio0": "a:123/vm-123-disk-0.qcow2", "scsi2": "a:123/vm-123-disk-2.qcow2"}, hasVMState: true, vmstate: "a:123/vm-123-state-suspend.raw", disks: map[string]string{findingAllocationID: "a:123/vm-123-disk-2.qcow2"}}
	if !reflect.DeepEqual(byVMID[123], want) {
		t.Fatalf("inventory = %+v, want %+v", byVMID[123], want)
	}
	if unrecognized := byVMID[124]; unrecognized.vmid != 124 || unrecognized.volumes != nil {
		t.Fatalf("an unrecognized volume reference was inventoried as volumes: %+v", unrecognized)
	}
	if len(report.pending) != 0 {
		t.Fatalf("pending mismatches survived the audit: %+v", report.pending)
	}
}

// Compile-time guard that the listing fake still satisfies the SDK service.
var _ ns.Service = (*allocationAuditNodes)(nil)

// TestObservedMoveMatchesKindAndVMID pins that a parker's move of an
// allocation never stands in for its holder's move, and that a move answers
// only for the VMID it was accepted for.
func TestObservedMoveMatchesKindAndVMID(t *testing.T) {
	report := StorageAllocationAudit{ObservedMoves: []StorageAllocationMove{{AllocationID: "disk-1", Kind: storageMoveKindParker, VMID: 90881, RecordedNodes: []string{"n1"}, ObservedNode: "n2"}}}
	if !report.observedMove(storageMoveKindParker, "disk-1", 90881, "n2") {
		t.Fatal("accepted parker move not found")
	}
	for _, miss := range []struct {
		kind, id string
		vmid     int
		node     string
	}{
		{allocationKindDisk, "disk-1", 90881, "n2"},
		{allocationKindDisk, "disk-1", 777, "n2"},
		{storageMoveKindParker, "disk-1", 777, "n2"},
		{storageMoveKindParker, "disk-1", 90881, "n1"},
		{storageMoveKindParker, "disk-2", 90881, "n2"},
	} {
		if report.observedMove(miss.kind, miss.id, miss.vmid, miss.node) {
			t.Fatalf("move answered for %+v", miss)
		}
	}
}

// TestStorageAuditMoveRefusalNamesTheReason pins the reason a refused move
// carries into refusals that consult the audit's verdict.
func TestStorageAuditMoveRefusalNamesTheReason(t *testing.T) {
	var report StorageAllocationAudit
	report.VMScanComplete = true
	report.addConflict("disk ownership provenance disagrees with actual holder node: disk allocation disk-1 (volume a:1/vm-1-disk-0.raw) held by VM 1 on n2, provenance names n1; not accepted as a move because volume a:1/vm-1-disk-0.raw is node-local", "disk allocation disk-1 on n2, recorded n1; not a move: volume a:1/vm-1-disk-0.raw is node-local")
	if got := storageAuditMoveRefusal(report, "disk-1"); got != "disk allocation disk-1 on n2, recorded n1; not a move: volume a:1/vm-1-disk-0.raw is node-local" {
		t.Fatalf("refusal = %q, want the conflict's brief", got)
	}
	if got := storageAuditMoveRefusal(report, "disk-2"); got != "no audit finding names the allocation" {
		t.Fatalf("refusal = %q for an allocation no finding names", got)
	}
	report.VMScanComplete = false
	if got := storageAuditMoveRefusal(report, "disk-2"); got != "the VM scan is incomplete" {
		t.Fatalf("refusal = %q, want the incomplete VM scan", got)
	}
}

// TestAuditRefusesADiskMoveTheVMNoLongerHolds pins that the audit does not
// accept a disk move when the VM that provenance names has dropped the volume
// from its configuration.
func TestAuditRefusesADiskMoveTheVMNoLongerHolds(t *testing.T) {
	f := newMoveFixture(t)
	records := f.build()
	delete(f.c.configs[123], "scsi2")
	report, err := auditStorageAllocationRecords(context.Background(), f.deps, records, nil, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if report.observedMove(allocationKindDisk, f.disk.ID, 123, "pve2") {
		t.Fatal("the move was accepted for a VM that no longer holds the volume")
	}
	want := "not accepted as a move because VM 123 does not hold volume " + f.pdisk + " in its configuration"
	if conflicts := strings.Join(report.Conflicts, "\n"); !strings.Contains(conflicts, want) {
		t.Fatalf("conflicts do not name the missing volume, want %q in:\n%s", want, conflicts)
	}
}
