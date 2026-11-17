package handlers

import (
	"context"
	stderrors "errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// liveDiskBackend is a local backend whose node sweep answers from a script,
// the way the replica-aware sweep answers once it has asked every node and
// settled several copies by the node of the volume's owner (see
// nodeForReplicatedVolume, whose rules the pve package's tests cover). It
// records every volume it was asked about.
type liveDiskBackend struct {
	storageType string
	// nodes maps a volume to the node the sweep settles on.
	nodes map[string]string
	// err, when set, is the sweep's answer for every volume.
	err error

	mu    sync.Mutex
	asked []string
}

func (b *liveDiskBackend) Kind() pve.BackendKind { return pve.BackendLocal }

func (b *liveDiskBackend) StorageInfo() pve.StorageInfo {
	return pve.StorageInfo{Name: "local-zfs", Type: b.storageType}
}

func (b *liveDiskBackend) NodeForCreate(context.Context, string, string) (string, error) {
	panic("liveDiskBackend.NodeForCreate: create_vm placement never creates a persistent disk")
}

func (b *liveDiskBackend) NodeForExisting(_ context.Context, volume string) (string, error) {
	b.mu.Lock()
	b.asked = append(b.asked, volume)
	b.mu.Unlock()
	if b.err != nil {
		return "", b.err
	}
	if node, ok := b.nodes[volume]; ok {
		return node, nil
	}
	return "", cpierrors.DiskNotFound(volume)
}

func (b *liveDiskBackend) askedVolumes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.asked)
}

type liveDiskResolver struct{ b *liveDiskBackend }

func (r liveDiskResolver) Resolve(context.Context, string) (pve.Backend, error) { return r.b, nil }

// encodeZFSDiskCID encodes a persistent disk CID on local-zfs whose metadata
// records node as the node create_disk put it on.
func encodeZFSDiskCID(t *testing.T, bare, stableID, node string) string {
	t.Helper()
	cid, err := pve.EncodeDiskCID(bare, &pve.DiskCIDMeta{ID: stableID, Pool: "local-zfs", Node: node})
	if err != nil {
		t.Fatalf("EncodeDiskCID(%q): %v", bare, err)
	}
	return cid
}

// TestDeriveDiskFaultConstraints_FollowsAReplicatedDiskAfterAFailover is a
// persistent disk created on pve0 and attached to a replicate VM, which PVE HA
// failed over to pve1. The Director then recreated the VM, so a parker on pve1
// holds the disk under its own name, and pve0 still holds a stale replica. The
// CID names pve0, and the VM must land on pve1, where the live copy is.
func TestDeriveDiskFaultConstraints_FollowsAReplicatedDiskAfterAFailover(t *testing.T) {
	t.Parallel()
	const birth = "local-zfs:vm-9001-disk-0"
	const live = "local-zfs:vm-90000-disk-0"
	deps := idTestDeps(newIDFakeClient(map[int]map[string]any{
		90000: {"tags": "bosh-cpi;bosh-parker", diskKeyScsi0: live + ",serial=" + idTestToken},
	}))
	deps.Logger = log.NewNopLogger()
	backend := &liveDiskBackend{storageType: pve.StorageTypeZFSPool, nodes: map[string]string{
		live:  "pve1",
		birth: "pve0",
	}}
	deps.Resolver = liveDiskResolver{b: backend}

	c, err := deriveDiskFaultConstraints(context.Background(), deps,
		[]string{encodeZFSDiskCID(t, birth, idTestToken, "pve0")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.requiredLocalNode != "pve1" {
		t.Errorf("requiredLocalNode = %q, want pve1, the node that holds the live copy", c.requiredLocalNode)
	}
	if got := backend.askedVolumes(); !slices.Equal(got, []string{live}) {
		t.Errorf("swept volumes %v, want only the disk's current volume %s", got, live)
	}
}

// TestResolveTargetNode_PlacesAReplicatedDiskWhenItsOldNodeIsDown is the same
// failover with the old node still down. The CID names pve1, which is offline,
// and the live search finds the disk on pve2, so create_vm places the VM there
// instead of failing on the old node.
func TestResolveTargetNode_PlacesAReplicatedDiskWhenItsOldNodeIsDown(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(
		[]map[string]any{
			{
				"type": "node", "name": "pve1", "online": 0, "maxcpu": int64(4),
				"maxmem": int64(8 * 1024 * 1024 * 1024), "mem": int64(0), "cpu": 0.0,
			},
			onlineNode("pve2"),
		},
		nil,
		func(c *config.CPIConfig) {
			c.Placement = &config.PlacementConfig{AZMap: map[string][]string{"zone-a": {"pve1", "pve2"}}}
		},
	)
	const bare = "local-zfs:vm-9001-disk-0"
	backend := &liveDiskBackend{storageType: pve.StorageTypeZFSPool, nodes: map[string]string{bare: "pve2"}}
	deps.Resolver = liveDiskResolver{b: backend}

	node, err := resolveTargetNodeWithRNG(context.Background(), deps, createVMCloudProps{},
		[]string{encodeZFSDiskCID(t, bare, "", "pve1")}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if node != "pve2" {
		t.Errorf("node = %q, want pve2, the online node that holds the disk", node)
	}
}

// TestDeriveDiskFaultConstraints_KeepsTheCIDNodeForStorageThatCannotReplicate
// is a persistent disk on LVM-thin, which no replication job copies, so the
// node in its CID stays a hard constraint and no sweep runs. The backend would
// name another node if it were asked, so a search would show.
func TestDeriveDiskFaultConstraints_KeepsTheCIDNodeForStorageThatCannotReplicate(t *testing.T) {
	t.Parallel()
	const bare = "local-zfs:vm-9001-disk-0"
	for _, storageType := range []string{pve.StorageTypeLVMThin, pve.StorageTypeLVM, pve.StorageTypeDir} {
		deps := buildFaultDomainDeps(nil, nil, nil)
		backend := &liveDiskBackend{storageType: storageType, nodes: map[string]string{bare: "pve2"}}
		deps.Resolver = liveDiskResolver{b: backend}
		c, err := deriveDiskFaultConstraints(context.Background(), deps,
			[]string{encodeZFSDiskCID(t, bare, "", "pve1")})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", storageType, err)
		}
		if c.requiredLocalNode != "pve1" {
			t.Errorf("%s: requiredLocalNode = %q, want the CID's node pve1", storageType, c.requiredLocalNode)
		}
		if got := backend.askedVolumes(); len(got) != 0 {
			t.Errorf("%s: swept %v, want no sweep for storage that cannot replicate", storageType, got)
		}
	}
}

// TestDeriveDiskFaultConstraints_RefusesToPickBetweenUnprovenCopies is a disk
// whose volume sits on pve1 and pve2 while its owner runs on neither, so the
// sweep cannot tell which copy is live. create_vm fails with the sweep's
// retriable error, which names both nodes, rather than trust the CID's node,
// whose copy may be the stale one.
func TestDeriveDiskFaultConstraints_RefusesToPickBetweenUnprovenCopies(t *testing.T) {
	t.Parallel()
	const bare = "local-zfs:vm-100-disk-1"
	deps := buildFaultDomainDeps(nil, nil, nil)
	backend := &liveDiskBackend{storageType: pve.StorageTypeZFSPool, err: cpierrors.Retriable(
		"NodeForExisting: volume %s is on nodes pve1,pve2, and its owner VM runs on none of them, so the CPI "+
			"cannot tell which copy is live and acts on neither", bare)}
	deps.Resolver = liveDiskResolver{b: backend}

	c, err := deriveDiskFaultConstraints(context.Background(), deps,
		[]string{encodeZFSDiskCID(t, bare, "", "pve1")})
	if err == nil {
		t.Fatalf("requiredLocalNode = %q with no error, want a refusal", c.requiredLocalNode)
	}
	var cpiErr *cpierrors.Error
	if !stderrors.As(err, &cpiErr) || !cpiErr.OkToRetry() {
		t.Errorf("err = %v, want a retriable error", err)
	}
	if !strings.Contains(err.Error(), "pve1,pve2") || !strings.Contains(err.Error(), bare) {
		t.Errorf("err = %v, want it to name the disk and both nodes", err)
	}
}

// TestDeriveDiskFaultConstraints_KeepsTheCIDNodeForADiskTheSweepFindsNowhere
// leaves a missing disk for attach_disk to report, as before.
func TestDeriveDiskFaultConstraints_KeepsTheCIDNodeForADiskTheSweepFindsNowhere(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(nil, nil, nil)
	deps.Resolver = liveDiskResolver{b: &liveDiskBackend{storageType: pve.StorageTypeZFSPool}}
	c, err := deriveDiskFaultConstraints(context.Background(), deps,
		[]string{encodeZFSDiskCID(t, "local-zfs:vm-9001-disk-0", "", "pve1")})
	if err != nil || c.requiredLocalNode != "pve1" {
		t.Fatalf("requiredLocalNode = %q, err %v; want the CID's node pve1", c.requiredLocalNode, err)
	}
}

// TestResolveTargetNode_PinnedReplicatedDiskSkipsTheHeadroomCheck places
// the VM of a replicated disk whose live copy is on pve02 while its CID names
// pve01, with headroom enforce and the other two nodes full. The live copy
// pins the VM to pve02, and a pin leaves no node to choose, so placement makes
// no headroom read and gives no estimate. A replicate VM never reaches the
// estimate in any case, because the estimate also decides the linked-clone
// reading, and replicate refuses a linked clone.
func TestResolveTargetNode_PinnedReplicatedDiskSkipsTheHeadroomCheck(t *testing.T) {
	t.Parallel()
	const bare = "local-zfs:vm-9001-disk-0"
	rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce, fullNonHAGuests("pve01", "pve03"),
		func(c *config.CPIConfig) {
			c.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyReplicate)
		})
	profile := stagingProfile(config.HAModeAuto)
	profile.Storages = append(profile.Storages, pve.StorageInfo{
		Name: "local-zfs", Type: pve.StorageTypeZFSPool, Content: "images", Nodes: []string{"pve02", "pve03"},
	})
	rig.deps.HAProfile = func(context.Context) (*ha.Profile, error) { return profile, nil }
	backend := &liveDiskBackend{storageType: pve.StorageTypeZFSPool, nodes: map[string]string{bare: "pve02"}}
	rig.deps.Resolver = liveDiskResolver{b: backend}
	cids := []string{encodeZFSDiskCID(t, bare, "", "pve01")}

	node, alternates, err := rig.placeWith(createVMCloudProps{}, cids, nil, bridgePlan{})
	if err != nil {
		t.Fatalf("placement: %v", err)
	}
	if node != "pve02" || len(alternates) != 0 {
		t.Fatalf("placement = %q %v; want pve02, the node the live copy pins, with no alternates", node, alternates)
	}
	if got := rig.cluster.reads(); got != 0 {
		t.Errorf("%d headroom reads; want none for a pinned VM", got)
	}
	if _, ok := hasWarn(rig.logs, "headroom estimate"); ok {
		t.Errorf("a pinned VM got a headroom warning; logs: %v", rig.logs.All())
	}
}
