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
// The rule reads PVE HA in every mode, mode off included, because a parker
// registered while HA was on keeps its HA resource after the mode changes,
// and reading the resource makes the park safer, never riskier. A read that
// fails leaves the answer unknown, so the park fails with a retriable error
// and writes nothing.

// parkerAdmitFor returns the parker selection rule for deps, which reads PVE
// HA through the client no managed allocation guard wraps, because the guard
// admits only the writes a park plans.
func parkerAdmitFor(deps Deps) func(ctx context.Context, node string, parkerVMID int, bareVolid string) (bool, error) {
	return func(ctx context.Context, node string, parkerVMID int, bareVolid string) (bool, error) {
		return admitDiskIntoParker(ctx, deps, node, parkerVMID, bareVolid)
	}
}

// admitDiskIntoParker answers the parker selection rule for one disk and one
// parker on node. It answers true for a parker PVE HA does not know. For a
// registered parker, including one staged as ignored and one another owner
// registered, it answers true only when the disk is on shared storage whose
// nodes cover the parker's reach (parkerReach). Every read that fails, and a
// storage the HA profile does not list, returns a retriable error.
func admitDiskIntoParker(ctx context.Context, deps Deps, node string, parkerVMID int, bareVolid string) (bool, error) {
	client := unguardedPVE(deps.PVE)
	if client == nil || client.Cluster() == nil {
		// No HA client means nothing could have registered the parker through
		// it either.
		return true, nil
	}
	svc := client.Cluster()
	sid := haResourceSid(parkerVMID)
	res, err := svc.GetHaResources(ctx, sid)
	switch {
	case err != nil && (isHaNotFound(err) || pve.IsNotFound(err)):
		return true, nil
	case err != nil:
		return false, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"parker selection: read the HA resource of parker vmid %d before parking %s in it", parkerVMID, bareVolid))
	case res == nil:
		return false, cpierrors.Retriable(
			"parker selection: the read of the HA resource of parker vmid %d returned nothing", parkerVMID)
	}

	profile, err := resolveHAProfile(ctx, deps)
	if err != nil {
		return false, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"parker selection: resolve the HA profile to check whether registered parker vmid %d may take %s",
			parkerVMID, bareVolid))
	}
	index, members, err := parkerStorageIndex(profile, deps.Config)
	if err != nil {
		return false, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"parker selection: registered parker vmid %d", parkerVMID))
	}
	logger := deps.Log(ctx)

	info, err := index.Get(ctx, volumeStorage(bareVolid))
	if err != nil {
		return false, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"parker selection: classify the storage of %s for registered parker vmid %d", bareVolid, parkerVMID))
	}
	if !info.IsShared() {
		logger.Info("parker selection: the disk is on node-local storage, so it stays out of the parker that PVE HA "+
			"manages", log.Int("parker_vmid", parkerVMID), log.String("node", node), log.String("volid", bareVolid))
		return false, nil
	}
	diskReach, err := pve.VolumeAccessSet(ctx, index, members, []pve.AccessVolume{{Volume: bareVolid, HomeNode: node}})
	if err != nil {
		return false, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"parker selection: find the nodes that reach %s", bareVolid))
	}
	reach, err := parkerReach(ctx, client, index, members, node, parkerVMID)
	if err != nil {
		return false, err
	}
	if missing := nodesOutside(reach, diskReach); len(missing) > 0 {
		logger.Info("parker selection: the disk's storage does not reach every node PVE HA may move the parker to, "+
			"so the disk goes to another parker", log.Int("parker_vmid", parkerVMID), log.String("node", node),
			log.String("volid", bareVolid), log.String("parker_reach", strings.Join(reach, ",")),
			log.String("disk_reach", strings.Join(diskReach, ",")), log.String("missing", strings.Join(missing, ",")))
		return false, nil
	}
	return true, nil
}

// parkerReach returns the nodes a disk must reach before it may go into the
// registered parker: the nodes of every enabled strict node-affinity rule
// that names the parker, or every configured node when none does, together
// with the nodes that reach every volume the parker holds now. The second
// half keeps the parker's access set as it is, even where disks that left it
// widened it past its pin.
func parkerReach(
	ctx context.Context, client pve.Client, index pve.StorageInfoGetter, members []string, node string, parkerVMID int,
) ([]string, error) {
	sid := haResourceSid(parkerVMID)
	resp, err := client.Cluster().ListHaRules(ctx, &cluster.ListHaRulesParams{})
	if err != nil {
		return nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"parker selection: read the HA rules of registered parker vmid %d", parkerVMID))
	}
	set, err := ha.ParseRuleList(resp)
	if err != nil {
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"parker selection: parse the HA rules of registered parker vmid %d", parkerVMID))
	}
	reach := map[string]bool{}
	pinned := false
	for i := range set.Rules {
		r := &set.Rules[i]
		if r.Type != ha.RuleTypeNodeAffinity || r.Disabled || !r.Strict || !r.HasResource(sid) {
			continue
		}
		pinned = true
		for _, n := range r.NodeNames() {
			reach[n] = true
		}
	}
	if !pinned {
		for _, n := range members {
			reach[n] = true
		}
	}
	views, err := pve.ReadQemuViews(ctx, client, node, parkerVMID)
	if err != nil {
		return nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"parker selection: read the config of registered parker vmid %d on node %s", parkerVMID, node))
	}
	held, err := pve.VolumeAccessSet(ctx, index, members, parkerAccessVolumes(views, node))
	if err != nil {
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"parker selection: find the nodes that reach the volumes of registered parker vmid %d", parkerVMID))
	}
	for _, n := range held {
		reach[n] = true
	}
	out := make([]string, 0, len(reach))
	for n := range reach {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
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
