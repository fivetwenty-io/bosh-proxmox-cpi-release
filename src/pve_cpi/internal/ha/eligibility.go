package ha

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// Outcome is what the CPI does with HA for one VM.
type Outcome string

const (
	// OutcomeRegister means the CPI registers the VM as an HA resource and
	// writes the pin that VMDecision.PinSpec describes.
	OutcomeRegister Outcome = "register"

	// OutcomeSkip means the VM gets no HA and runs unregistered. Reason says
	// why, and the caller logs it.
	OutcomeSkip Outcome = "skip"

	// OutcomeFail means the call fails. VMDecision.Err carries the error.
	OutcomeFail Outcome = "fail"
)

// Policy is the kind of HA a registered VM gets.
type Policy string

const (
	// PolicyNone means the VM is not HA-managed.
	PolicyNone Policy = "none"

	// PolicyShared means every volume is reachable from at least two placeable
	// nodes, so HA may move the VM inside its access set.
	PolicyShared Policy = "shared"

	// PolicyHold means the VM has a node-local volume and is pinned strictly to
	// its own node.
	PolicyHold Policy = "hold"

	// PolicyReplicate means the VM has node-local ZFS volumes that a
	// replication job copies to a partner node. Until the first sync is
	// verified the VM is pinned like a hold VM.
	PolicyReplicate Policy = "replicate"
)

// VolumeKind tells the config-drive ISO apart from the volumes that hold data.
type VolumeKind string

const (
	// VolumeDisk is every volume that is not an ISO: root, ephemeral, and
	// persistent disks, cloud-init, EFI and TPM state, unused volumes, and the
	// volumes of snapshots and saved VM state.
	VolumeDisk VolumeKind = "disk"

	// VolumeISO is a config-drive or other ISO image. Replication never copies
	// one, and a node-local ISO makes a live migration fail.
	VolumeISO VolumeKind = "iso"
)

// VMVolume is one volume the VM config references, or will reference.
type VMVolume struct {
	// Volume is the storage-qualified volume ID, such as "nfs1:100/vm-100-disk-0".
	Volume string

	// HomeNode is the node that holds the volume when its storage is node-local.
	// It defaults to the VM's node.
	HomeNode string

	// Kind is VolumeDisk when empty.
	Kind VolumeKind

	// Legacy marks a disk with no stable ID, which BOSH finds only by its
	// volume ID. A rename on shared storage would make it look missing.
	Legacy bool

	// NoReplicate marks a drive whose replicate flag is off, which PVE
	// silently skips when it replicates the guest.
	NoReplicate bool
}

func (v VMVolume) kind() VolumeKind {
	if v.Kind == "" {
		return VolumeDisk
	}
	return v.Kind
}

// NodeConstraint is a hardware or network requirement that only some nodes
// meet, such as a PCI device or a bridge. The caller computes it from the same
// checks placement already enforces.
type NodeConstraint struct {
	// Name says what the constraint is, such as "bridge vmbr1".
	Name string

	// Nodes lists the nodes that meet it.
	Nodes []string
}

// VMInput is everything DecideVM needs to know about one VM.
type VMInput struct {
	// Config supplies the pve.ha keys and the config-drive ISO setting.
	Config *config.CPIConfig

	// VMID and Node name the VM and the node it is placed on. Node anchors the
	// CPU migration domain and is the home of node-local volumes.
	VMID int
	Node string

	// Volumes lists every volume the VM config will reference, including the
	// ISO, cloud-init, EFI and TPM state, unused volumes, and the volumes of its
	// snapshots and saved VM state.
	Volumes []VMVolume

	// CPUType is the CPU type the VM runs with, as EffectiveCPUType resolves it.
	CPUType string

	// Constraints holds the hardware and network requirements of the VM.
	Constraints []NodeConstraint

	// AZ and AZNodes name the availability zone the VM was placed in and its
	// nodes. They are empty for a VM with no resolved AZ. The AZ is a hard
	// constraint while pve.ha.pin_az_strict is true, and only a preference
	// otherwise.
	AZ      string
	AZNodes []string

	// InDLBAZ says the VM named the DLB sentinel AZ.
	InDLBAZ bool

	// LinkedClone says the VM's disks are linked clones of a base template,
	// which replication cannot copy on its own.
	LinkedClone bool

	// State is the normalized HA state read for this decision. Its CRM uid
	// becomes VMDecision.DecisionUID. It may be nil when no read was made.
	State *pve.HAServiceState
}

// VMDecision is the one answer rule writes, headroom, and the landing check
// read for a VM.
type VMDecision struct {
	VMID int
	SID  string

	// Node is the node the decision was made for.
	Node string

	// Outcome says whether to register, skip, or fail, and Policy says what
	// kind of HA a registered VM gets. Reason always says why, and Err is set
	// only when Outcome is OutcomeFail.
	Outcome Outcome
	Policy  Policy
	Reason  string
	Err     error

	// Warnings lists the lines the caller logs: legacy disks, a downgrade to
	// some-local, and so on.
	Warnings []string

	// Access is the VM's access set: the nodes that reach every volume, narrowed
	// by the hardware and network constraints. It is sorted and may hold nodes
	// that are not placeable now.
	Access []string

	// CPUDomain is the CPU migration domain of the placed node when the CPU type
	// is host or max, and nil otherwise.
	CPUDomain []string

	// Hard is the hard admissible set in the form PinSpec takes it. nil means
	// the VM has no hard constraint. Admissible is the same set as a concrete
	// list, which is every configured node when Hard is nil.
	Hard       []string
	Admissible []string

	// Placed, Preferred, and DLB are the soft inputs of the pin. Priorities
	// holds the resulting priority of each admissible node.
	Placed     string
	Preferred  []string
	DLB        bool
	Priorities map[string]int

	// Partner is the replication partner, and Widened is the node set the pin
	// grows to once the replica is recoverable. Both are empty unless Policy is
	// PolicyReplicate. Hard names the source alone until then.
	Partner string
	Widened []string

	// DecisionUID is the CRM uid the decision's state read saw, empty when it
	// saw none. StagedIgnored is true once the call has staged the resource as
	// ignored and never activated it. The rule writer reads both when a repair
	// fails and it must decide whether it may remove the resource.
	DecisionUID   string
	StagedIgnored bool
}

// Registers reports whether the VM is to be registered.
func (d *VMDecision) Registers() bool { return d != nil && d.Outcome == OutcomeRegister }

// Failure returns the error of a failed decision, or nil.
func (d *VMDecision) Failure() error {
	if d == nil || d.Outcome != OutcomeFail {
		return nil
	}
	return d.Err
}

// MarkStaged records that this call staged the VM's resource as ignored and has
// not activated it, so a failed repair may remove it directly.
func (d *VMDecision) MarkStaged() { d.StagedIgnored = true }

// MarkActivated records that the resource left the ignored state.
func (d *VMDecision) MarkActivated() { d.StagedIgnored = false }

// RepairDeps wires the decision into the rule writer's repair. The writer asks
// about a service ID, and the decision answers only for its own VM. Without a
// state read or a removal path the writer never removes a resource.
func (d *VMDecision) RepairDeps(
	read func(ctx context.Context, vmid int) (*pve.HAServiceState, error),
	remove func(ctx context.Context, sid string) error,
) RepairDeps {
	deps := RepairDeps{ReadState: read, RemoveResource: remove}
	if d == nil {
		return deps
	}
	deps.StagedIgnored = func(sid string) bool { return sid == d.SID && d.StagedIgnored }
	deps.DecisionUID = func(sid string) (string, bool) {
		if sid != d.SID || d.DecisionUID == "" {
			return "", false
		}
		return d.DecisionUID, true
	}
	return deps
}

// PinSpec returns the spec of the VM's merged node-affinity pin. configured is
// the cluster's configured node set.
func (d *VMDecision) PinSpec(id, comment string, configured []string, rebalanceOnStart bool) PinSpec {
	return PinSpec{
		ID: id, Comment: comment, SID: d.SID,
		Hard: d.Hard, Configured: configured,
		Placed: d.Placed, Preferred: d.Preferred,
		RebalanceOnStart: rebalanceOnStart, DLB: d.DLB,
	}
}

// DecideVM decides what the CPI does with HA for one VM. It reads the profile
// and the input and nothing else, so the same inputs always give the same
// answer. The rows of the decision table are:
//
//   - Mode off registers nothing, and a VM that names the DLB sentinel AZ fails.
//   - A profile that cannot write HA gives no registration in auto and an error
//     in managed.
//   - A VM whose volumes are all shared registers with full HA when at least two
//     placeable nodes reach every volume and, for a CPU type of host or max, its
//     CPU migration domain.
//   - A VM with a node-local volume, or whose access set is too small, follows
//     pve.ha.local_policy: none registers nothing, hold pins the VM to its own
//     node, and replicate pins it to its own node until a verified replica
//     exists. A replicate VM that misses a prerequisite gets no HA in auto and
//     an error in managed.
//
// DecideVM returns an error only for a caller mistake. A decision that fails the
// call is returned with Outcome OutcomeFail and Err set.
func (p *Profile) DecideVM(in VMInput) (*VMDecision, error) {
	switch {
	case p == nil:
		return nil, cpierrors.Cloud("DecideVM: profile must not be nil")
	case in.Config == nil:
		return nil, cpierrors.Cloud("DecideVM: config must not be nil")
	case in.VMID <= 0:
		return nil, cpierrors.Cloud("DecideVM: VM ID must be positive, got %d", in.VMID)
	case strings.TrimSpace(in.Node) == "":
		return nil, cpierrors.Cloud("DecideVM: the VM's node must not be empty")
	}
	e := &eligibility{p: p, in: in, cfg: in.Config, managed: p.Mode == config.HAModeManaged}
	e.d = &VMDecision{
		VMID: in.VMID, SID: pve.HAServiceSID(in.VMID), Node: strings.TrimSpace(in.Node),
		Outcome: OutcomeSkip, Policy: PolicyNone, Placed: strings.TrimSpace(in.Node),
	}
	if st := in.State; st != nil && st.SID == e.d.SID && st.Known && st.InCRM {
		e.d.DecisionUID = st.UID
	}
	e.decide()
	return e.d, nil
}

// eligibility carries one DecideVM call.
type eligibility struct {
	p       *Profile
	in      VMInput
	cfg     *config.CPIConfig
	d       *VMDecision
	managed bool

	members   []string
	placeable map[string]bool
	infos     map[string]pve.StorageInfo
}

func (e *eligibility) decide() {
	if e.p.Mode != config.HAModeAuto && !e.managed {
		if e.in.InDLBAZ && e.cfg.DLBAZName() != "" {
			e.fail(cpierrors.Cloud("create_vm: availability_zone %q is the DLB sentinel AZ, which needs "+
				"pve.ha.mode set to \"auto\" or \"managed\"; set pve.ha.mode, or set pve.ha.dlb.az_name to "+
				"another name, or to an empty string to disable the sentinel", e.cfg.DLBAZName()))
			return
		}
		e.skip("pve.ha.mode is off, so the CPI registers nothing")
		return
	}
	if !e.p.WritesAllowed() {
		if err := e.p.PrerequisiteError(); err != nil {
			e.fail(err)
			return
		}
		e.skip("HA is observe-only on this cluster: " + e.p.Reason)
		return
	}
	e.members = e.p.nodeNames()
	if len(e.members) == 0 {
		e.refuse(cpierrors.Cloud("the HA profile lists no cluster node"))
		return
	}
	e.placeable = e.p.placeableSet()
	if !e.resolveDLB() {
		return
	}
	e.classify()
}

// skip ends the decision with no HA for the VM.
func (e *eligibility) skip(reason string) {
	e.d.Outcome, e.d.Policy, e.d.Reason = OutcomeSkip, PolicyNone, reason
}

// fail ends the decision with an error.
func (e *eligibility) fail(err error) {
	e.d.Outcome, e.d.Policy, e.d.Err, e.d.Reason = OutcomeFail, PolicyNone, err, err.Error()
}

// refuse ends the decision the way a missing prerequisite does: no HA for the VM
// in auto, an error in managed.
func (e *eligibility) refuse(err error) {
	if e.managed {
		e.fail(err)
		return
	}
	e.skip(err.Error())
	e.warn("%s", err.Error())
}

func (e *eligibility) warn(format string, args ...any) {
	e.d.Warnings = append(e.d.Warnings, fmt.Sprintf(format, args...))
}

// resolveDLB decides whether the VM is a DLB VM, and reports false when the
// decision has ended.
func (e *eligibility) resolveDLB() bool {
	sentinel := e.in.InDLBAZ && e.cfg.DLBAZName() != ""
	global := e.cfg.HADLBEnabledValue()
	if !sentinel && !global {
		return true
	}
	if e.p.DLBSupported {
		e.d.DLB = true
		return true
	}
	err := cpierrors.Cloud("VM %d asks for the dynamic load balancer, which needs PVE 9.2 or later on every "+
		"online node (lowest version %q)", e.in.VMID, e.p.MinVersion)
	switch {
	case e.managed:
		e.fail(err)
		return false
	case sentinel:
		e.skip(err.Error())
		e.warn("%s", err.Error())
		return false
	}
	e.warn("pve.ha.dlb.enabled is ignored for VM %d: the dynamic load balancer needs PVE 9.2 or later "+
		"on every online node (lowest version %q)", e.in.VMID, e.p.MinVersion)
	return true
}

// volumeClass is the classification of one volume.
type volumeClass struct {
	vol   VMVolume
	info  pve.StorageInfo
	local bool
}

// classify reads the volumes and ends the decision.
func (e *eligibility) classify() {
	topology := e.cfg.HAStorageTopologyValue()
	classes, err := e.classifyVolumes(topology)
	if err != nil {
		e.refuse(err)
		return
	}
	var diskLocal, isoLocal []string
	var disks, isos []pve.AccessVolume
	for _, c := range classes {
		av := pve.AccessVolume{Volume: c.vol.Volume, HomeNode: c.vol.HomeNode}
		if c.vol.kind() == VolumeISO {
			isos = append(isos, av)
			if c.local {
				isoLocal = append(isoLocal, c.vol.Volume)
			}
			continue
		}
		disks = append(disks, av)
		if c.local {
			diskLocal = append(diskLocal, c.vol.Volume)
		}
	}
	if topology == config.HAStorageTopologyShared && len(diskLocal)+len(isoLocal) > 0 {
		e.fail(cpierrors.Cloud("pve.ha.storage_topology is shared, so every volume must be on shared storage, "+
			"and VM %d holds node-local volumes %s", e.in.VMID, strings.Join(append(diskLocal, isoLocal...), ", ")))
		return
	}
	getter := e.storageGetter(topology)
	diskAccess, err := e.narrow(getter, disks)
	if err != nil {
		e.refuse(err)
		return
	}
	allAccess, err := e.narrow(getter, append(append([]pve.AccessVolume{}, disks...), isos...))
	if err != nil {
		e.refuse(err)
		return
	}
	e.d.Access = allAccess

	domain, domainErr := e.cpuDomain()
	e.d.CPUDomain = domain

	// A node-local ISO downgrades a VM that would otherwise get full HA.
	wouldBeShared := len(diskLocal) == 0 && e.countPlaceable(intersect(diskAccess, domainOrAll(domain, e.members))) >= 2
	if len(isoLocal) > 0 && wouldBeShared && domainErr == nil {
		if e.managed || e.cfg.RequireSharedISOForHAEnabled() {
			e.fail(cpierrors.Cloud("VM %d holds the node-local ISO %s and would otherwise get full HA; "+
				"point pve.iso_storage at shared storage, or clear pve.require_shared_iso_for_ha and use "+
				"auto mode to accept the downgrade", e.in.VMID, strings.Join(isoLocal, ", ")))
			return
		}
		e.warn("VM %d holds the node-local ISO %s, which makes it a some-local VM and takes it out of full HA; "+
			"point pve.iso_storage at shared storage to restore it", e.in.VMID, strings.Join(isoLocal, ", "))
	}

	someLocal := len(diskLocal)+len(isoLocal) > 0
	if !someLocal && e.countPlaceable(allAccess) >= 2 {
		e.decideShared(allAccess, domain, domainErr)
	} else {
		reason := "its access set holds fewer than two placeable nodes"
		if someLocal {
			reason = "it holds node-local volumes " + strings.Join(append(append([]string{}, diskLocal...), isoLocal...), ", ")
		}
		e.decideLocal(classes, allAccess, domain, domainErr, len(isoLocal) > 0, topology, reason)
	}
	if e.d.Outcome == OutcomeRegister {
		e.checkLegacy(classes)
	}
}

// classifyVolumes pairs every volume with its storage. A volume whose storage
// the profile does not carry cannot be placed, so it ends the decision.
func (e *eligibility) classifyVolumes(topology string) ([]*volumeClass, error) {
	if len(e.in.Volumes) > 0 && e.p.StoragesErr != "" {
		return nil, cpierrors.Cloud("the storage definitions could not be read (%s), so the volumes of VM %d "+
			"cannot be classified", e.p.StoragesErr, e.in.VMID)
	}
	e.infos = make(map[string]pve.StorageInfo, len(e.p.Storages))
	for i := range e.p.Storages {
		e.infos[e.p.Storages[i].Name] = e.p.Storages[i]
	}
	out := make([]*volumeClass, 0, len(e.in.Volumes))
	for i := range e.in.Volumes {
		v := e.in.Volumes[i]
		storage, name, ok := strings.Cut(v.Volume, ":")
		if !ok || storage == "" || name == "" {
			return nil, cpierrors.Cloud("%q is not a storage-qualified volume ID", v.Volume)
		}
		info, known := e.infos[storage]
		if !known {
			return nil, cpierrors.Cloud("volume %s is on storage %q, which the storage definitions do not list",
				v.Volume, storage)
		}
		if strings.TrimSpace(v.HomeNode) == "" {
			v.HomeNode = e.d.Node
		}
		local := topology == config.HAStorageTopologyLocal || !info.IsShared()
		out = append(out, &volumeClass{vol: v, info: info, local: local})
	}
	return out, nil
}

// storageGetter answers VolumeAccessSet from the profile's storage definitions.
// Under storage_topology local, no volume follows its VM, so every storage
// reads as node-local.
func (e *eligibility) storageGetter(topology string) pve.StorageInfoGetter {
	return storageIndex{infos: e.infos, forceLocal: topology == config.HAStorageTopologyLocal}
}

type storageIndex struct {
	infos      map[string]pve.StorageInfo
	forceLocal bool
}

func (s storageIndex) Get(_ context.Context, name string) (pve.StorageInfo, error) {
	info, ok := s.infos[name]
	if !ok {
		return pve.StorageInfo{}, fmt.Errorf("storage %q is not defined", name)
	}
	if s.forceLocal {
		return pve.StorageInfo{Name: info.Name, Disabled: info.Disabled, Nodes: info.Nodes}, nil
	}
	return info, nil
}

// narrow returns the access set of volumes, narrowed by the node constraints.
// VolumeAccessSet reads an in-memory storage index and never uses its context,
// so the background context never waits, and DecideVM stays a pure function
// with no context parameter.
func (e *eligibility) narrow(getter pve.StorageInfoGetter, volumes []pve.AccessVolume) ([]string, error) {
	access, err := pve.VolumeAccessSet(context.Background(), getter, e.members, volumes)
	if err != nil {
		return nil, err
	}
	for _, c := range e.in.Constraints {
		access = intersect(access, c.Nodes)
	}
	return access, nil
}

// cpuDomain returns the CPU migration domain of the placed node when the CPU
// type is host or max, and nil when the VM can migrate across CPU models. An
// error means the domain cannot be stated, which counts as an empty domain.
func (e *eligibility) cpuDomain() ([]string, error) {
	if !CPUTypeNeedsDomain(e.in.CPUType) {
		return nil, nil
	}
	dom, ok := e.p.DomainOf(e.d.Node)
	if !ok {
		return nil, fmt.Errorf("the CPU of node %s is unknown", e.d.Node)
	}
	return append([]string{}, dom.Nodes...), nil
}

// decideShared handles a VM whose volumes are all shared and whose access set
// holds two placeable nodes. The CPU migration domain may still shrink it.
func (e *eligibility) decideShared(access, domain []string, domainErr error) {
	allowed := access
	if CPUTypeNeedsDomain(e.in.CPUType) {
		if domainErr != nil {
			e.refuse(e.cpuError(domainErr.Error(), false))
			return
		}
		allowed = intersect(access, domain)
		if n := e.countPlaceable(allowed); n < 2 {
			e.refuse(e.cpuError(fmt.Sprintf("its CPU migration domain %s holds %d placeable node(s)",
				listOrNone(allowed), n), len(allowed) >= 2))
			return
		}
	}
	e.d.Access = allowed
	e.register(PolicyShared, allowed, "every volume is on shared storage that at least two placeable nodes reach")
}

// cpuError builds the refusal for a host or max CPU type whose migration
// domain is too small. transient is true when the domain holds enough nodes
// and only their health is missing.
func (e *eligibility) cpuError(detail string, transient bool) error {
	text := fmt.Sprintf("VM %d gets no HA: its CPU type %q can live-migrate only between nodes with the same CPU "+
		"model and flags, and %s. Set pve.cpu_type to the newest CPU model that every node of the cluster "+
		"supports, such as x86-64-v3 or x86-64-v2-AES, or use a cluster whose nodes share one CPU model",
		e.in.VMID, cpuTypeModel(e.in.CPUType), detail)
	if transient {
		return cpierrors.Retriable("%s", text)
	}
	return cpierrors.Cloud("%s", text)
}

// decideLocal applies pve.ha.local_policy to a VM that cannot be HA-managed as
// shared.
func (e *eligibility) decideLocal(
	classes []*volumeClass, access, domain []string, domainErr error, isoLocal bool, topology, why string,
) {
	policy := e.cfg.HALocalPolicyValue()
	if topology == config.HAStorageTopologyShared {
		e.refuse(cpierrors.Cloud("pve.ha.storage_topology is shared, and VM %d cannot be HA-managed: %s", e.in.VMID, why))
		return
	}
	switch policy {
	case config.HALocalPolicyHold:
		if !contains(access, e.d.Node) {
			e.refuse(cpierrors.Cloud("VM %d cannot be pinned to node %s, because its volumes are not all reachable "+
				"from it (access set %s)", e.in.VMID, e.d.Node, listOrNone(access)))
			return
		}
		e.register(PolicyHold, []string{e.d.Node},
			"it cannot be HA-managed as shared ("+why+"), so local_policy hold pins it to node "+e.d.Node)
	case config.HALocalPolicyReplicate:
		e.decideReplicate(classes, domain, domainErr, isoLocal, why)
	default:
		e.skip("local_policy is none and the VM cannot be HA-managed as shared (" + why + ")")
	}
}

// register ends the decision with a registration whose hard constraints are
// hard intersected with the strict AZ, the node constraints, and the placed
// node.
func (e *eligibility) register(policy Policy, hard []string, reason string) {
	set := append([]string{}, hard...)
	if az := e.strictAZ(); az != nil {
		set = intersect(set, az)
	}
	if len(set) == 0 {
		e.refuse(cpierrors.Cloud("VM %d has an empty hard admissible set: its volumes, hardware, CPU model, and "+
			"availability zone %q leave no node that satisfies all of them", e.in.VMID, e.in.AZ))
		return
	}
	if !contains(set, e.d.Node) {
		e.refuse(cpierrors.Cloud("VM %d is placed on node %s, which lies outside its hard admissible set %s",
			e.in.VMID, e.d.Node, strings.Join(set, ",")))
		return
	}
	e.d.Outcome, e.d.Policy, e.d.Reason = OutcomeRegister, policy, reason
	e.d.Admissible = set
	if !sameSet(set, e.members) {
		e.d.Hard = set
	}
	if az := e.preferredAZ(); az != nil {
		e.d.Preferred = az
	}
	e.d.Priorities = e.priorities()
}

// strictAZ returns the nodes of a strict AZ, or nil when the AZ is not a hard
// constraint.
func (e *eligibility) strictAZ() []string {
	if e.cfg.HAPinAZValue() == config.HARuleOff || len(e.in.AZNodes) == 0 || !e.cfg.HAPinAZStrictValue() {
		return nil
	}
	return normalizeNodes(e.in.AZNodes)
}

// preferredAZ returns the nodes of a non-strict AZ, which only raise priority.
func (e *eligibility) preferredAZ() []string {
	if e.cfg.HAPinAZValue() == config.HARuleOff || len(e.in.AZNodes) == 0 || e.cfg.HAPinAZStrictValue() {
		return nil
	}
	return normalizeNodes(e.in.AZNodes)
}

// priorities returns the priority of each admissible node: the placed node
// first, the nodes of a non-strict AZ next, and the rest last. A DLB VM has no
// placed priority, because PVE chooses its node.
func (e *eligibility) priorities() map[string]int {
	preferred := toSet(e.d.Preferred)
	out := make(map[string]int, len(e.d.Admissible))
	for _, n := range e.d.Admissible {
		switch {
		case n == e.d.Placed && !e.d.DLB:
			out[n] = PinPriorityPlaced
		case preferred[n]:
			out[n] = PinPriorityPreferred
		default:
			out[n] = PinPriorityRest
		}
	}
	return out
}

// checkLegacy applies the legacy-disk rule to a VM that is to be registered.
// A rename of a volume under HA would make a disk without a stable ID look
// missing, so auto warns once per disk and managed refuses.
func (e *eligibility) checkLegacy(classes []*volumeClass) {
	var legacy []string
	for _, c := range classes {
		if c.vol.Legacy && c.vol.kind() == VolumeDisk {
			legacy = append(legacy, c.vol.Volume)
		}
	}
	if len(legacy) == 0 {
		return
	}
	if e.managed {
		e.fail(cpierrors.Cloud("VM %d holds legacy disks %s with no stable ID, and managed mode does not register "+
			"a VM whose disk a volume rename would make look missing", e.in.VMID, strings.Join(legacy, ", ")))
		return
	}
	for _, v := range legacy {
		e.warn("disk %s of VM %d has no stable ID, so a volume rename under HA would make it look missing", v, e.in.VMID)
	}
}

// decideReplicate checks the replication prerequisites and picks the partner.
func (e *eligibility) decideReplicate(
	classes []*volumeClass, domain []string, domainErr error, isoLocal bool, why string,
) {
	var gaps []string
	if isoLocal {
		if e.managed || e.cfg.RequireSharedISOForHAEnabled() {
			e.fail(cpierrors.Cloud("VM %d holds a node-local ISO, and replication does not copy it; point "+
				"pve.iso_storage at shared storage", e.in.VMID))
			return
		}
		gaps = append(gaps, "its config-drive ISO is node-local and replication does not copy it")
	}
	if !e.p.VMReplicate {
		gaps = append(gaps, "the token lacks VM.Replicate on /vms")
	}
	if e.in.LinkedClone {
		gaps = append(gaps, "its disks are linked clones whose base may live only on the source")
	}
	if domainErr != nil {
		gaps = append(gaps, domainErr.Error())
	}
	candidates := e.replicaCandidates(domain)
	var sharedVols []pve.AccessVolume
	for _, c := range classes {
		if !c.local {
			sharedVols = append(sharedVols, pve.AccessVolume{Volume: c.vol.Volume, HomeNode: c.vol.HomeNode})
		}
	}
	if len(sharedVols) > 0 {
		sa, err := e.narrow(storageIndex{infos: e.infos}, sharedVols)
		if err != nil {
			gaps = append(gaps, err.Error())
		} else {
			candidates = intersect(candidates, sa)
		}
	}
	localDisks := 0
	for _, c := range classes {
		if c.local && c.vol.kind() == VolumeDisk {
			localDisks++
			gaps = append(gaps, e.zfsGaps(c)...)
			candidates = intersect(candidates, e.zfsNodes(c.info))
		}
	}
	if localDisks == 0 {
		gaps = append(gaps, "it holds no node-local disk for a replication job to copy")
	}
	candidates = remove(candidates, e.d.Node)
	var partners []string
	for _, n := range candidates {
		if e.placeable[n] {
			partners = append(partners, n)
		}
	}
	if len(partners) == 0 {
		gaps = append(gaps, "no placeable partner node has the same ZFS storage and reaches the shared volumes")
	}
	if len(gaps) > 0 {
		e.refuse(cpierrors.Cloud("VM %d cannot use local_policy replicate (%s): %s", e.in.VMID, why, strings.Join(gaps, "; ")))
		return
	}
	partner := partners[0]
	for _, j := range e.p.Replication {
		if int(j.Guest) == e.in.VMID && contains(partners, j.Target) {
			partner = j.Target
			break
		}
	}
	e.register(PolicyReplicate, []string{e.d.Node},
		"local ZFS volumes replicate from node "+e.d.Node+" to node "+partner+", and the pin names the source until a sync is verified")
	if e.d.Outcome != OutcomeRegister {
		return
	}
	e.d.Partner = partner
	e.d.Widened = normalizeNodes([]string{e.d.Node, partner})
	if az := e.strictAZ(); az != nil {
		e.d.Widened = intersect(e.d.Widened, az)
	}
}

// replicaCandidates returns the nodes a replica may live on before the volumes
// narrow them: every node the constraints and the CPU domain allow, restricted
// to the strict AZ.
func (e *eligibility) replicaCandidates(domain []string) []string {
	out := append([]string{}, e.members...)
	for _, c := range e.in.Constraints {
		out = intersect(out, c.Nodes)
	}
	if domain != nil {
		out = intersect(out, domain)
	}
	if az := e.strictAZ(); az != nil {
		out = intersect(out, az)
	}
	sort.Strings(out)
	return out
}

// zfsGaps lists why a node-local volume cannot be replicated.
func (e *eligibility) zfsGaps(c *volumeClass) []string {
	var gaps []string
	switch {
	case !strings.EqualFold(c.info.Type, pve.StorageTypeZFSPool):
		gaps = append(gaps, fmt.Sprintf("volume %s is on %s storage %s, which is not a ZFS pool",
			c.vol.Volume, c.info.Type, c.info.Name))
	case c.info.Disabled:
		gaps = append(gaps, fmt.Sprintf("storage %s is disabled", c.info.Name))
	case c.info.Shared:
		gaps = append(gaps, fmt.Sprintf("storage %s is shared", c.info.Name))
	}
	if c.vol.NoReplicate {
		gaps = append(gaps, fmt.Sprintf("volume %s has its replicate flag off", c.vol.Volume))
	}
	if c.info.Content != "" && !containsField(c.info.Content, "images") {
		gaps = append(gaps, fmt.Sprintf("storage %s does not hold images", c.info.Name))
	}
	if len(c.info.Nodes) > 0 && !contains(c.info.Nodes, c.vol.HomeNode) {
		gaps = append(gaps, fmt.Sprintf("storage %s is not defined on node %s", c.info.Name, c.vol.HomeNode))
	}
	return gaps
}

// zfsNodes returns the nodes on which a ZFS storage with the same ID exists.
func (e *eligibility) zfsNodes(info pve.StorageInfo) []string {
	if len(info.Nodes) == 0 {
		return append([]string{}, e.members...)
	}
	return intersect(e.members, info.Nodes)
}

func (e *eligibility) countPlaceable(nodes []string) int {
	n := 0
	for _, name := range nodes {
		if e.placeable[name] {
			n++
		}
	}
	return n
}

// nodeNames returns the configured node names, sorted.
func (p *Profile) nodeNames() []string {
	out := make([]string, 0, len(p.Nodes))
	for i := range p.Nodes {
		if p.Nodes[i].Name != "" {
			out = append(out, p.Nodes[i].Name)
		}
	}
	sort.Strings(out)
	return out
}

// placeableSet returns the nodes that can take a VM now: the health report's
// placeable nodes, or the online nodes when there is no report.
func (p *Profile) placeableSet() map[string]bool {
	out := map[string]bool{}
	if p.Health != nil {
		for _, e := range p.Health.Nodes {
			if e.Placeable {
				out[e.Name] = true
			}
		}
		return out
	}
	for _, n := range p.OnlineNodes() {
		out[n] = true
	}
	return out
}

// EffectiveCPUType resolves the CPU type a VM runs with, in the order the CPI
// writes it. A cloud_properties.pve_config.cpu passthrough is the last write,
// so it wins. Otherwise cloud_properties.cpu_type wins over pve.cpu_type, and
// the config layer is config.CPIConfig.CPUTypeValue, which is the read create_vm
// resolves the VM's shape with, so the two cannot drift. A config that went
// through ApplyDefaults has config.DefaultCPUType in pve.cpu_type, and an
// unset value in a config that never did gives "". The pve-default sentinel at
// either layer gives "", which leaves the choice to PVE's own default.
func EffectiveCPUType(cfg *config.CPIConfig, perVM *string, passthrough string) string {
	if v := strings.TrimSpace(passthrough); v != "" {
		return v
	}
	if perVM != nil {
		v := strings.TrimSpace(*perVM)
		if v == config.CPUTypePVEDefault {
			return ""
		}
		return v
	}
	return cfg.CPUTypeValue()
}

// cpuTypeModel returns the model name of a PVE cpu property, which is its first
// field and may carry a cputype= prefix. A property with no model field reads as
// empty, which PVE treats as kvm64.
func cpuTypeModel(cpu string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(cpu), ",")
	first = strings.TrimSpace(first)
	if rest, ok := strings.CutPrefix(first, "cputype="); ok {
		return strings.TrimSpace(rest)
	}
	if strings.Contains(first, "=") {
		return ""
	}
	return first
}

// CPUTypeNeedsDomain reports whether a VM with this CPU type can live-migrate
// only between nodes with the same CPU model and flags. That is the case for
// host and max, which pass the physical CPU through. A named model and the
// empty type, which PVE reads as kvm64, are portable.
func CPUTypeNeedsDomain(cpu string) bool {
	switch strings.ToLower(cpuTypeModel(cpu)) {
	case "host", "max":
		return true
	}
	return false
}

func normalizeNodes(in []string) []string {
	out := trimmed(in)
	sort.Strings(out)
	return out
}

// intersect returns the nodes in both lists, sorted.
func intersect(a, b []string) []string {
	in := toSet(b)
	out := []string{}
	for _, n := range a {
		if in[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// domainOrAll returns the CPU domain, or all when the VM has none.
func domainOrAll(domain, all []string) []string {
	if domain == nil {
		return all
	}
	return domain
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func remove(list []string, s string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

func containsField(csv, want string) bool {
	for _, f := range strings.Split(csv, ",") {
		if strings.TrimSpace(f) == want {
			return true
		}
	}
	return false
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := toSet(b)
	for _, v := range a {
		if !set[v] {
			return false
		}
	}
	return true
}

func listOrNone(nodes []string) string {
	if len(nodes) == 0 {
		return "(none)"
	}
	return "[" + strings.Join(nodes, ",") + "]"
}
