package handlers

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// The parker selection rule.
//
// A parker that PVE HA manages is relocated with its config when its node
// fails, so every disk it holds must be reachable from every node HA may
// recover it onto. A park therefore puts a disk into a registered parker only
// when the disk's storage reaches every node of that parker's reach, which is
// the nodes of its strict pin, or every configured node when it has none,
// together with the nodes that reach every volume it holds now. That keeps
// the parker's access set as it was, so its pin never has to be narrowed
// while it holds disks. A disk on node-local storage never goes into a
// registered parker. Every other parker, one PVE HA does not know, takes any
// disk, as it always has.
//
// pve.ParkerConfig.Admit carries the rule into every park, transfer, and
// resumed transfer, and pve calls it inside the parker's protection window.
// That window holds the parker's VMID lock, which the parker registration
// holds too, so a parker cannot be registered between the answer and the
// attach it allows. A refusal makes the selection loop try the next parker on
// the node, or create one, and a parker the loop creates is unregistered, so
// it takes the disk.
//
// The rule runs only when pve.ha.mode is auto or managed. In off mode it
// reads no HA state, and a park behaves as it did before parkers were ever
// registered. A parker registered while HA was on keeps its HA resource after
// a change to off, and nothing then keeps a disk out of it.
//
// Every answer the rule cannot prove refuses the parker rather than failing
// the park, because a refusal is always safe: the loop moves on, and the
// fresh parker it ends at is unregistered. Only an HA read that fails is an
// error. A refusal by PVE for a missing grant (HTTP 403) never comes right on
// its own: auto mode then parks as if PVE HA managed no parker, with a
// warning that names the privilege, and managed mode fails the park with a
// non-retriable error that names it. Any other failed read is retriable when
// it may pass, which is a server error or a transport fault, and permanent
// when it is PVE's verdict about the request.

// parkerHAReadPrivilege is the privilege PVE checks for the HA reads of the
// parker rules, GET /cluster/ha/resources/{sid} and GET /cluster/ha/rules.
const parkerHAReadPrivilege = "Sys.Audit on /"

// parkerAdmitFor returns the parker selection rule for deps, which reads PVE
// HA through the client no managed allocation guard wraps, because the guard
// admits only the writes a park plans. It returns nil in off mode, which
// leaves every park as it was before the rule existed.
func parkerAdmitFor(deps Deps) func(ctx context.Context, node string, parkerVMID int, bareVolid string) (bool, error) {
	if deps.Config == nil || deps.Config.HAModeValue() == config.HAModeOff {
		return nil
	}
	// A park may ask about several parkers; they share one HA profile.
	deps = withRequestHAProfile(deps)
	return func(ctx context.Context, node string, parkerVMID int, bareVolid string) (bool, error) {
		return admitDiskIntoParker(ctx, deps, node, parkerVMID, bareVolid)
	}
}

// admitDiskIntoParker answers the parker selection rule for one disk and one
// parker on node. It answers true for a parker PVE HA does not know, and in
// off mode for every parker. For a registered parker, including one staged as
// ignored and one another owner registered, it answers true only when the
// disk is on shared storage whose nodes cover the parker's reach
// (parkerReach). A fact it cannot prove, such as a storage the HA profile
// does not list, refuses the parker. A failed HA read is an error, classified
// by parkerHAReadFailure.
func admitDiskIntoParker(ctx context.Context, deps Deps, node string, parkerVMID int, bareVolid string) (bool, error) {
	if deps.Config == nil || deps.Config.HAModeValue() == config.HAModeOff {
		return true, nil
	}
	client := unguardedPVE(deps.PVE)
	if client == nil || client.Cluster() == nil {
		// No HA client means nothing could have registered the parker through
		// it either.
		return true, nil
	}
	logger := deps.Log(ctx).With(log.Int("parker_vmid", parkerVMID), log.String("node", node),
		log.String("volid", bareVolid))
	refuse := func(reason string, err error) (bool, error) {
		logger.Info("parker selection: "+reason+", so the disk goes to another parker", log.Err(err))
		return false, nil
	}
	sid := haResourceSid(parkerVMID)
	res, err := client.Cluster().GetHaResources(ctx, sid)
	switch {
	case err != nil && (isHaNotFound(err) || pve.IsNotFound(err)):
		return true, nil
	case err != nil:
		return parkerHAReadFailure(deps, logger, err, fmt.Sprintf(
			"parker selection: read the HA resource of parker vmid %d before parking %s in it", parkerVMID, bareVolid))
	case res == nil:
		return false, cpierrors.Retriable(
			"parker selection: the read of the HA resource of parker vmid %d returned nothing", parkerVMID)
	}

	profile, err := resolveHAProfile(ctx, deps)
	if err != nil {
		if pve.IsPermissionDenied(err) {
			return parkerHAReadFailure(deps, logger, err, fmt.Sprintf(
				"parker selection: resolve the HA profile to check whether registered parker vmid %d may take %s",
				parkerVMID, bareVolid))
		}
		return false, cpierrors.Wrap(pve.WrapErrorKeepingClass(err), fmt.Sprintf(
			"parker selection: resolve the HA profile to check whether registered parker vmid %d may take %s",
			parkerVMID, bareVolid))
	}
	index, members, err := parkerStorageIndex(profile, deps.Config)
	if err != nil {
		return refuse("the storages or nodes PVE HA may move the registered parker to are unknown", err)
	}
	info, err := index.Get(ctx, volumeStorage(bareVolid))
	if err != nil {
		return refuse("the disk's storage is not one the HA profile lists", err)
	}
	if !info.IsShared() {
		return refuse("the disk is on node-local storage, which a parker that PVE HA manages never takes", nil)
	}
	diskReach, err := pve.VolumeAccessSet(ctx, index, members, []pve.AccessVolume{{Volume: bareVolid, HomeNode: node}})
	if err != nil {
		return refuse("the nodes that reach the disk are unknown", err)
	}
	reach, answer, err := parkerReach(ctx, deps, logger, profile, index, members, node, parkerVMID)
	switch {
	case err != nil:
		return false, err
	case answer == parkerReachUnknown:
		return refuse("the nodes PVE HA may move the registered parker to are unknown", nil)
	case answer == parkerReachUnguarded:
		return true, nil
	}
	if missing := nodesOutside(reach, diskReach); len(missing) > 0 {
		logger.Info("parker selection: the disk's storage does not reach every node PVE HA may move the parker to, "+
			"so the disk goes to another parker", log.String("parker_reach", strings.Join(reach, ",")),
			log.String("disk_reach", strings.Join(diskReach, ",")), log.String("missing", strings.Join(missing, ",")))
		return false, nil
	}
	return true, nil
}

// parkerHAReadFailure answers a failed HA read of the parker selection rule.
// A refusal for a missing grant never comes right on its own: auto mode
// admits the disk, as a park did before parkers were registered, and warns
// with the privilege to grant, and managed mode fails the park with a
// non-retriable error that names it. Every other failure keeps the class its
// cause gives it, so a server error or a transport fault is retriable and a
// verdict about the request is not.
func parkerHAReadFailure(deps Deps, logger *log.Logger, err error, msg string) (bool, error) {
	if !pve.IsPermissionDenied(err) {
		return false, parkerHAReadError(err, msg)
	}
	if deps.Config != nil && deps.Config.HAModeValue() == config.HAModeManaged {
		return false, cpierrors.Wrap(pve.WrapError(err), fmt.Sprintf(
			"%s: PVE refused the read, because the token lacks %s, which pve.ha.mode managed needs to keep a disk "+
				"out of a parker PVE HA could move to a node without the disk's storage; grant %s and retry",
			msg, parkerHAReadPrivilege, parkerHAReadPrivilege))
	}
	logger.Warn("parker selection: PVE refused an HA read, because the token lacks "+parkerHAReadPrivilege+
		"; the disk is parked as if PVE HA managed no parker, until the privilege is granted", log.Err(err))
	return true, nil
}

// parkerHAReadError wraps a failed HA read with the class its cause gives it:
// a server error, a pushback, or a transport fault may pass and is retriable,
// while PVE's verdict about the request, any other 4xx, is not. An error
// that carries no status and is not a known transport fault is treated as one
// that may pass, because no verdict was given.
func parkerHAReadError(err error, msg string) error {
	if pve.IsRequestVerdict(err) {
		return cpierrors.Wrap(pve.WrapError(err), msg)
	}
	return cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, msg)
}

// parkerReachAnswer says how far parkerReach got.
type parkerReachAnswer int

const (
	// parkerReachKnown: the reach is proven.
	parkerReachKnown parkerReachAnswer = iota
	// parkerReachUnknown: a fact the reach needs cannot be proven, such as a
	// held volume on a storage the profile does not list, or the parker is
	// gone. The parker is refused.
	parkerReachUnknown
	// parkerReachUnguarded: auto mode, and PVE refused the rules read for a
	// missing grant. The park goes ahead as without HA.
	parkerReachUnguarded
)

// parkerReach returns the nodes a disk must reach before it may go into the
// registered parker: the nodes of every enabled strict node-affinity rule
// that names the parker, or every configured node when none does, together
// with the nodes that reach every volume the parker holds now. The second
// half keeps the parker's access set as it is, even where disks that left it
// widened it past its pin.
//
// Every configured node is the widest reach a pin can give, so a cluster
// whose profile says PVE does not answer the rules read, and a rules answer
// that does not parse, count as no strict pin, which is never less strict.
func parkerReach(
	ctx context.Context, deps Deps, logger *log.Logger, profile *ha.Profile, index pve.StorageInfoGetter,
	members []string, node string, parkerVMID int,
) ([]string, parkerReachAnswer, error) {
	client := unguardedPVE(deps.PVE)
	set := map[string]bool{}
	pinned := false
	if profile.RulesSupported {
		resp, err := client.Cluster().ListHaRules(ctx, &cluster.ListHaRulesParams{})
		if err != nil {
			admit, failErr := parkerHAReadFailure(deps, logger, err, fmt.Sprintf(
				"parker selection: read the HA rules of registered parker vmid %d", parkerVMID))
			if failErr != nil || !admit {
				return nil, parkerReachUnknown, failErr
			}
			return nil, parkerReachUnguarded, nil
		}
		pinned = addStrictPinNodes(set, resp, haResourceSid(parkerVMID), logger)
	}
	if !pinned {
		for _, n := range members {
			set[n] = true
		}
	}
	views, err := pve.ReadQemuViews(ctx, client, node, parkerVMID)
	if err != nil {
		if pve.IsNotFound(err) || pve.IsPmxcfsConfigMissing(err) {
			// The parker is gone, so it takes nothing, and the loop moves on.
			return nil, parkerReachUnknown, nil
		}
		return nil, parkerReachUnknown, cpierrors.Wrap(pve.WrapConfigReadError(err), fmt.Sprintf(
			"parker selection: read the config of registered parker vmid %d on node %s", parkerVMID, node))
	}
	held, err := pve.VolumeAccessSet(ctx, index, members, parkerAccessVolumes(views, node))
	if err != nil {
		logger.Info("parker selection: the nodes that reach the registered parker's volumes are unknown",
			log.Err(err))
		return nil, parkerReachUnknown, nil
	}
	for _, n := range held {
		set[n] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, parkerReachKnown, nil
}

// addStrictPinNodes adds to set the nodes of every enabled strict
// node-affinity rule in resp that names sid, and reports whether any did. A
// rules answer that does not parse adds nothing, so the caller counts every
// configured node.
func addStrictPinNodes(set map[string]bool, resp *cluster.ListHaRulesResponse, sid string, logger *log.Logger) bool {
	rules, err := ha.ParseRuleList(resp)
	if err != nil {
		logger.Info("parker selection: the HA rules did not parse, so the registered parker's reach counts every "+
			"configured node", log.Err(err))
		return false
	}
	pinned := false
	for i := range rules.Rules {
		r := &rules.Rules[i]
		if r.Type != ha.RuleTypeNodeAffinity || r.Disabled || !r.Strict || !r.HasResource(sid) {
			continue
		}
		pinned = true
		for _, n := range r.NodeNames() {
			set[n] = true
		}
	}
	return pinned
}

// parkerAccessVolumes lists every volume a parker holds or is receiving: the
// volumes of its config with its pending changes, which counts a disk whose
// detach is still pending, and the volumes its provenance records name, which
// counts a disk an unfinished transfer is still moving in. Volumes on
// node-local storage are homed on node.
func parkerAccessVolumes(views pve.QemuViews, node string) []pve.AccessVolume {
	seen := map[string]bool{}
	var out []pve.AccessVolume
	add := func(volume string) {
		volume = strings.TrimSpace(volume)
		if volume == "" || seen[volume] {
			return
		}
		seen[volume] = true
		out = append(out, pve.AccessVolume{Volume: volume, HomeNode: node})
	}
	holding := views.Holding()
	for _, v := range pve.ConfigAccessVolumes(holding, node) {
		add(v.Volume)
	}
	for _, v := range pve.ConfigAccessVolumes(views.PendingReplacements(), node) {
		add(v.Volume)
	}
	for _, v := range pve.ParkerRecordedVolumes(holding) {
		add(v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Volume < out[j].Volume })
	return out
}

// parkerStorageIndex returns the storage index and the configured node set
// the parker rules read, both from the HA profile, so the selection rule and
// the registration decision judge a storage the same way. Under
// pve.ha.storage_topology local every storage reads as node-local. A profile
// whose storages could not be read, or that lists no node, is an error.
func parkerStorageIndex(profile *ha.Profile, cfg *config.CPIConfig) (pve.StorageInfoGetter, []string, error) {
	switch {
	case profile == nil:
		return nil, nil, cpierrors.Cloud("the HA profile is missing")
	case profile.StoragesErr != "":
		return nil, nil, cpierrors.Cloud("the storage definitions could not be read: %s", profile.StoragesErr)
	}
	members := profileNodeNames(profile)
	if len(members) == 0 {
		return nil, nil, cpierrors.Cloud("the HA profile lists no cluster node")
	}
	sort.Strings(members)
	forceLocal := cfg != nil && cfg.HAStorageTopologyValue() == config.HAStorageTopologyLocal
	infos := make(map[string]pve.StorageInfo, len(profile.Storages))
	for i := range profile.Storages {
		infos[profile.Storages[i].Name] = profile.Storages[i]
	}
	return parkerStorages{infos: infos, forceLocal: forceLocal}, members, nil
}

// parkerStorages answers pve.VolumeAccessSet from the HA profile's storage
// definitions.
type parkerStorages struct {
	infos      map[string]pve.StorageInfo
	forceLocal bool
}

func (s parkerStorages) Get(_ context.Context, name string) (pve.StorageInfo, error) {
	info, ok := s.infos[name]
	if !ok {
		return pve.StorageInfo{}, cpierrors.Cloud("storage %q is not in the storage definitions", name)
	}
	if s.forceLocal {
		return pve.StorageInfo{Name: info.Name, Disabled: info.Disabled, Nodes: info.Nodes}, nil
	}
	return info, nil
}

// volumeStorage returns the storage of a storage-qualified volume ID, or the
// whole ID when it names none, which no storage index carries.
func volumeStorage(volid string) string {
	storage, _, ok := strings.Cut(strings.TrimSpace(volid), ":")
	if !ok {
		return strings.TrimSpace(volid)
	}
	return strings.TrimSpace(storage)
}

// nodesOutside returns the nodes of want that have does not hold, sorted.
func nodesOutside(want, have []string) []string {
	var out []string
	for _, n := range want {
		if !slices.Contains(have, n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
