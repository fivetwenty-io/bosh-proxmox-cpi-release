package pve

import (
	"context"
	"encoding/json"
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
	// jobs are the rows GET /cluster/replication returns; jobsErr fails it.
	jobs    []string
	jobsErr error
}

// newReplicaRig scripts a cluster where guest 100 has a replication job, so
// its volumes take the replica-aware sweep.
func newReplicaRig(holders ...string) *replicaRig {
	r := &replicaRig{
		holders: map[string]bool{}, failing: map[string]bool{},
		jobs: []string{`{"id":"100-0","guest":100,"target":"pve-02","type":"local"}`},
	}
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
		clusterSvc: &fakeCluster{replicationFn: func(context.Context) (*sdkcluster.ListReplicationResponse, error) {
			if r.jobsErr != nil {
				return nil, r.jobsErr
			}
			resp := sdkcluster.ListReplicationResponse{}
			for _, j := range r.jobs {
				resp = append(resp, json.RawMessage(j))
			}
			return &resp, nil
		}, listFn: func(
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
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("%s: want a retriable ambiguity error, got %v", name, err)
		}
		if !strings.Contains(err.Error(), "pve-01,pve-02") {
			t.Fatalf("%s: error does not name the nodes: %v", name, err)
		}
	}
}

// legacyAnswer runs the same lookup with replica-aware lookup off, which is
// the behavior every volume no replication job covers must keep.
func legacyAnswer(rig *replicaRig, volume string) (string, error) {
	off := newLocalBackend(rig.client(), StorageInfo{Name: "local-zfs", Type: StorageTypeZFSPool}, "", nil)
	return off.NodeForExisting(context.Background(), volume)
}

func sameAnswer(t *testing.T, label, gotNode string, gotErr error, wantNode string, wantErr error) {
	t.Helper()
	if gotNode != wantNode || (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("%s: got %q, %v; the lookup-off answer is %q, %v", label, gotNode, gotErr, wantNode, wantErr)
	}
	if gotErr == nil {
		return
	}
	if gotErr.Error() != wantErr.Error() {
		t.Fatalf("%s: got error %q; the lookup-off error is %q", label, gotErr, wantErr)
	}
	for _, typ := range []cpierrors.Type{cpierrors.TypeRetriableCloud, cpierrors.TypeDiskNotFound} {
		if cpierrors.IsType(gotErr, typ) != cpierrors.IsType(wantErr, typ) {
			t.Fatalf("%s: error type differs from lookup off: %v vs %v", label, gotErr, wantErr)
		}
	}
}

// A volume no replication job covers keeps the first-hit sweep exactly with
// lookup on. That holds for a CPI persistent disk, which a disk ID owns and no
// guest job replicates, and for a name PVE gives no owner.
func TestReplicaLookup_UncoveredVolumeKeepsTheLegacySweep(t *testing.T) {
	t.Parallel()
	scenarios := map[string]func() *replicaRig{
		"two copies": func() *replicaRig { return newReplicaRig("pve-01", "pve-02").ownerOn("pve-02") },
		"one copy":   func() *replicaRig { return newReplicaRig("pve-03") },
		"no copy":    func() *replicaRig { return newReplicaRig() },
		"first node down": func() *replicaRig {
			r := newReplicaRig("pve-02")
			r.failing["pve-01"] = true
			return r
		},
		"copy and a node down": func() *replicaRig {
			r := newReplicaRig("pve-01")
			r.failing["pve-02"] = true
			return r
		},
		"every node down": func() *replicaRig {
			r := newReplicaRig()
			r.failing = map[string]bool{"pve-01": true, "pve-02": true, "pve-03": true}
			return r
		},
		"owner VM read fails": func() *replicaRig {
			r := newReplicaRig("pve-01", "pve-02")
			r.vmErr = errors.New("connection reset")
			return r
		},
	}
	jobSets := map[string][]string{
		"no job at all":        nil,
		"a job of another VM":  {`{"id":"100-0","guest":100,"target":"pve-02"}`},
		"a disabled other job": {`{"id":"100-1","guest":100,"target":"pve-03","disable":1}`},
	}
	for _, volume := range []string{"vm-9001-disk-0", "custom-volume", "base-9001-disk-0"} {
		for sname, build := range scenarios {
			for jname, jobs := range jobSets {
				label := volume + ", " + sname + ", " + jname
				on := build()
				on.jobs = jobs
				got, err := on.backend(StorageTypeZFSPool).NodeForExisting(context.Background(), volume)
				ref := build()
				ref.jobs = jobs
				want, wantErr := legacyAnswer(ref, volume)
				sameAnswer(t, label, got, err, want, wantErr)
				if len(on.probed) != len(ref.probed) {
					t.Fatalf("%s: probed %v, lookup off probed %v", label, on.probed, ref.probed)
				}
			}
		}
	}
}

func TestReplicaLookup_UnownedNameOnTwoNodesKeepsTheFirstHit(t *testing.T) {
	t.Parallel()
	rig := newReplicaRig("pve-01", "pve-02").ownerOn("pve-02")
	got, err := rig.backend(StorageTypeZFSPool).NodeForExisting(context.Background(), "custom-volume")
	if err != nil || got != "pve-01" {
		t.Fatalf("got %q, %v; PVE never replicates an unowned name, want the first hit pve-01", got, err)
	}
}

// When the job list cannot be read, or holds a row whose guest cannot be
// told, a single clean copy is still the volume and anything else defers.
func TestReplicaLookup_UnknownCoverage(t *testing.T) {
	t.Parallel()
	rigs := map[string]func(holders ...string) *replicaRig{
		"list fails": func(h ...string) *replicaRig {
			r := newReplicaRig(h...)
			r.jobsErr = errors.New("permission denied")
			return r
		},
		"unreadable row": func(h ...string) *replicaRig {
			r := newReplicaRig(h...)
			r.jobs = []string{`{"id":"x","target":"pve-02"}`}
			return r
		},
		"malformed row": func(h ...string) *replicaRig {
			r := newReplicaRig(h...)
			r.jobs = []string{`[1,2]`}
			return r
		},
	}
	for name, build := range rigs {
		got, err := build("pve-03").backend(StorageTypeZFSPool).NodeForExisting(context.Background(), "vm-9001-disk-0")
		if err != nil || got != "pve-03" {
			t.Fatalf("%s, one copy: got %q, %v; want pve-03", name, got, err)
		}
		rig := build("pve-01", "pve-02").ownerOn("pve-02")
		_, err = rig.backend(StorageTypeZFSPool).NodeForExisting(context.Background(), "vm-100-disk-0")
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("%s, two copies: want retriable, got %v", name, err)
		}
		if !strings.Contains(err.Error(), "pve-01,pve-02") {
			t.Fatalf("%s: error does not name the nodes: %v", name, err)
		}
		rig = build("pve-01")
		rig.failing["pve-02"] = true
		_, err = rig.backend(StorageTypeZFSPool).NodeForExisting(context.Background(), "vm-100-disk-0")
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("%s, one copy and a node down: want retriable, got %v", name, err)
		}
	}
}

func TestReplicationRowGuest(t *testing.T) {
	t.Parallel()
	cases := map[string]int{
		`{"id":"100-0"}`:             100,
		`{"id":"x","guest":"205"}`:   205,
		`{"guest":7}`:                7,
		`{"id":"0-1"}`:               0,
		`{"id":"abc"}`:               0,
		`not json`:                   0,
		`{"id":"-1","guest":"abc"}`:  0,
		`{"id":"100-0","guest":101}`: 0,
		`{"id":"100-0","guest":100}`: 100,
	}
	for raw, want := range cases {
		got, ok := replicationRowGuest(json.RawMessage(raw))
		if got != want || ok != (want > 0) {
			t.Fatalf("%s: got %d, %v; want %d", raw, got, ok, want)
		}
	}
}

func TestVolumeOwnerVMID(t *testing.T) {
	t.Parallel()
	cases := map[string]int{
		"local-zfs:vm-100-disk-0": 100,
		"vm-9001-disk-1":          9001,
		"base-7-disk-0":           7,
		"subvol-100-disk-0":       0,
		"custom-volume":           0,
		"vm-0-disk-0":             0,
	}
	for volid, want := range cases {
		got, ok := VolumeOwnerVMID(volid)
		if got != want || ok != (want > 0) {
			t.Fatalf("%s: got %d, %v; want %d", volid, got, ok, want)
		}
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
