package pve

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// replicaRig scripts a three-node cluster where a volume may sit on several
// nodes at once, the way storage replication leaves it.
type replicaRig struct {
	mu      sync.Mutex
	holders map[string]bool
	failing map[string]bool
	owner   map[string]any
	vmErr   error
	probed  []string
}

func newReplicaRig(holders ...string) *replicaRig {
	r := &replicaRig{holders: map[string]bool{}, failing: map[string]bool{}}
	for _, h := range holders {
		r.holders[h] = true
	}
	return r
}

func (r *replicaRig) client() *backendTestClient {
	return &backendTestClient{
		storageSvc: &fakeStorage{existsFn: func(_ context.Context, node, _, _ string) (bool, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.probed = append(r.probed, node)
			if r.failing[node] {
				return false, errors.New("connection refused")
			}
			return r.holders[node], nil
		}},
		clusterSvc: &fakeCluster{listFn: func(
			_ context.Context, params *sdkcluster.ListResourcesParams,
		) (*sdkcluster.ListResourcesResponse, error) {
			if params != nil && params.Type != nil && *params.Type == "vm" {
				if r.vmErr != nil {
					return nil, r.vmErr
				}
				if r.owner == nil {
					return clusterResp(), nil
				}
				return clusterResp(r.owner), nil
			}
			return clusterResp(
				map[string]any{"node": "pve-01"},
				map[string]any{"node": "pve-02"},
				map[string]any{"node": "pve-03"},
			), nil
		}},
	}
}

func (r *replicaRig) backend(storageType string) Backend {
	res := &resolver{client: r.client()}
	WithReplicaAwareLookup(true)(res)
	return res.local(StorageInfo{Name: "local-zfs", Type: storageType})
}

func (r *replicaRig) ownerOn(node string) *replicaRig {
	r.owner = map[string]any{"vmid": 100, "node": node, "type": "qemu"}
	return r
}

func TestReplicaLookup_SingleCopyIsTheVolume(t *testing.T) {
	t.Parallel()
	rig := newReplicaRig("pve-03")
	got, err := rig.backend(StorageTypeZFSPool).NodeForExisting(context.Background(), "vm-100-disk-1")
	if err != nil || got != "pve-03" {
		t.Fatalf("got %q, %v; want pve-03", got, err)
	}
	if len(rig.probed) != 3 {
		t.Fatalf("probed %v, want every node", rig.probed)
	}
}

func TestReplicaLookup_TwoCopiesResolveToTheOwnerNode(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{StorageTypeZFSPool, ""} {
		rig := newReplicaRig("pve-01", "pve-02").ownerOn("pve-02")
		got, err := rig.backend(typ).NodeForExisting(context.Background(), "vm-100-disk-1")
		if err != nil || got != "pve-02" {
			t.Fatalf("type %q: got %q, %v; want the owner's node pve-02, never the first copy", typ, got, err)
		}
	}
}

func TestReplicaLookup_TwoCopiesWithTheOwnerElsewhereRefusesToPick(t *testing.T) {
	t.Parallel()
	cases := map[string]*replicaRig{
		"owner on a third node": newReplicaRig("pve-01", "pve-02").ownerOn("pve-03"),
		"owner gone":            newReplicaRig("pve-01", "pve-02"),
	}
	for name, rig := range cases {
		_, err := rig.backend(StorageTypeZFSPool).NodeForExisting(context.Background(), "vm-100-disk-1")
		if err == nil {
			t.Fatalf("%s: picked a copy", name)
		}
		if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
			t.Fatalf("%s: want an ambiguity Cloud error, got %v", name, err)
		}
		if !strings.Contains(err.Error(), "pve-01,pve-02") {
			t.Fatalf("%s: error does not name the nodes: %v", name, err)
		}
	}
}

func TestReplicaLookup_UnownedNameOnTwoNodesRefusesToPick(t *testing.T) {
	t.Parallel()
	rig := newReplicaRig("pve-01", "pve-02").ownerOn("pve-01")
	_, err := rig.backend(StorageTypeZFSPool).NodeForExisting(context.Background(), "custom-volume")
	if err == nil || cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want an ambiguity error, got %v", err)
	}
}

func TestReplicaLookup_OwnerReadErrorIsRetriable(t *testing.T) {
	t.Parallel()
	rig := newReplicaRig("pve-01", "pve-02")
	rig.vmErr = errors.New("connection reset")
	_, err := rig.backend(StorageTypeZFSPool).NodeForExisting(context.Background(), "vm-100-disk-1")
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want retriable, got %v", err)
	}
}

func TestReplicaLookup_OneCopyWithANodeUnreachable(t *testing.T) {
	t.Parallel()
	rig := newReplicaRig("pve-01").ownerOn("pve-01")
	rig.failing["pve-02"] = true
	got, err := rig.backend(StorageTypeZFSPool).NodeForExisting(context.Background(), "vm-100-disk-1")
	if err != nil || got != "pve-01" {
		t.Fatalf("owner on the copy: got %q, %v; want pve-01", got, err)
	}

	rig = newReplicaRig("pve-01").ownerOn("pve-02")
	rig.failing["pve-02"] = true
	_, err = rig.backend(StorageTypeZFSPool).NodeForExisting(context.Background(), "vm-100-disk-1")
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("owner on the unreachable node: want retriable, got %v", err)
	}
}

func TestReplicaLookup_NoCopy(t *testing.T) {
	t.Parallel()
	rig := newReplicaRig()
	_, err := rig.backend(StorageTypeZFSPool).NodeForExisting(context.Background(), "vm-100-disk-1")
	if !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Fatalf("clean sweep: want DiskNotFound, got %v", err)
	}

	rig = newReplicaRig()
	rig.failing["pve-03"] = true
	_, err = rig.backend(StorageTypeZFSPool).NodeForExisting(context.Background(), "vm-100-disk-1")
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("sweep with a probe error: want retriable, got %v", err)
	}
}

func TestReplicaLookup_OffOrOtherStorageKeepsTheFirstHit(t *testing.T) {
	t.Parallel()
	rig := newReplicaRig("pve-01", "pve-02").ownerOn("pve-02")
	off := newLocalBackend(rig.client(), StorageInfo{Name: "local-zfs", Type: StorageTypeZFSPool}, "", nil)
	got, err := off.NodeForExisting(context.Background(), "vm-100-disk-1")
	if err != nil || got != "pve-01" {
		t.Fatalf("lookup off: got %q, %v; want the first hit pve-01", got, err)
	}

	got, err = rig.backend("lvmthin").NodeForExisting(context.Background(), "vm-100-disk-1")
	if err != nil || got != "pve-01" {
		t.Fatalf("lvmthin: got %q, %v; want the first hit pve-01", got, err)
	}
}

func TestWithReplicaAwareLookupSetsTheBackendField(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{true, false} {
		res := &resolver{}
		WithReplicaAwareLookup(enabled)(res)
		b, ok := res.local(StorageInfo{Name: "local-zfs"}).(*localBackend)
		if !ok || b.replicaAware != enabled {
			t.Fatalf("enabled %v: backend %#v", enabled, b)
		}
	}
}
