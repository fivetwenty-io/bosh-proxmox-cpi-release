package pve_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// admitRig is a parker fixture whose parkers start empty, record every
// attach, and answer the parker selection rule through admit.
type admitRig struct {
	mu       sync.Mutex
	created  []int
	attaches []admitAttach
	asked    []int
}

type admitAttach struct {
	vmid  int
	node  string
	volid string
}

func (r *admitRig) qemu(full map[int]bool) *parkerQEMU {
	return &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			if full[vmid] {
				return fullParkerDisks(), nil
			}
			return map[string]any{"tags": "bosh-parker"}, nil
		},
		createFn: func(_ string, params map[string]any) (string, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			vmid, _ := params["vmid"].(int)
			r.created = append(r.created, vmid)
			return "", nil
		},
		attachFn: func(node string, vmid int, volid, _ string, opts *qemu.AttachOpts) (string, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.attaches = append(r.attaches, admitAttach{vmid: vmid, node: node, volid: volid})
			if opts != nil {
				return opts.DiskID, nil
			}
			return "", nil
		},
	}
}

func (r *admitRig) attachedTo() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, 0, len(r.attaches))
	for _, a := range r.attaches {
		out = append(out, a.vmid)
	}
	return out
}

func (r *admitRig) record(vmid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, vmid)
}

func admitCtx() context.Context {
	return pve.WithTestBackoff(context.Background(), func(_ int) time.Duration { return 0 })
}

func listRows(rows ...map[string]any) func(context.Context, *cluster.ListResourcesParams) (
	*cluster.ListResourcesResponse, error,
) {
	return func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(rows...), nil
	}
}

// TestParkDisk_AdmitRefusalSkipsToNextParker proves a parker the selection
// rule refuses is passed over like a full one, and the disk lands in the next
// parker that the rule admits.
func TestParkDisk_AdmitRefusalSkipsToNextParker(t *testing.T) {
	t.Parallel()
	rig := &admitRig{}
	c := buildParkerClient(rig.qemu(nil), listRows(
		map[string]any{"vmid": int64(90000), "node": "pve1"},
		map[string]any{"vmid": int64(90001), "node": "pve1"},
	))
	cfg := parkerTestCfg()
	cfg.Admit = func(_ context.Context, node string, vmid int, volid string) (bool, error) {
		rig.record(vmid)
		if node != "pve1" || volid != "nfs:vm-700-disk-1" {
			t.Errorf("Admit got node %q volid %q", node, volid)
		}
		return vmid != 90000, nil
	}

	err := pve.ParkDisk(admitCtx(), c, nopLogger(), "pve1", "nfs:vm-700-disk-1", cfg, pve.ParkContext{})
	if err != nil {
		t.Fatalf("ParkDisk: %v", err)
	}
	if got := rig.attachedTo(); len(got) != 1 || got[0] != 90001 {
		t.Fatalf("the disk must land only in parker 90001, which the rule admits; attaches %v", got)
	}
	if len(rig.created) != 0 {
		t.Fatalf("no parker may be created while an admitted one has room; created %v", rig.created)
	}
	if len(rig.asked) < 2 || rig.asked[0] != 90000 {
		t.Fatalf("the rule must be asked about 90000 first, then 90001; asked %v", rig.asked)
	}
}

// TestParkDisk_AdmitRefusesEveryParkerCreatesFresh proves that when the rule
// refuses every existing parker, the park creates a fresh one, which nothing
// has registered, and lands the disk there.
func TestParkDisk_AdmitRefusesEveryParkerCreatesFresh(t *testing.T) {
	t.Parallel()
	rig := &admitRig{}
	c := buildParkerClient(rig.qemu(nil), listRows(
		map[string]any{"vmid": int64(90000), "node": "pve1"},
		map[string]any{"vmid": int64(90001), "node": "pve1"},
	))
	cfg := parkerTestCfg()
	cfg.Admit = func(_ context.Context, _ string, vmid int, _ string) (bool, error) {
		return vmid != 90000 && vmid != 90001, nil
	}

	err := pve.ParkDisk(admitCtx(), c, nopLogger(), "pve1", "nfs:vm-700-disk-1", cfg, pve.ParkContext{})
	if err != nil {
		t.Fatalf("ParkDisk: %v", err)
	}
	if len(rig.created) != 1 {
		t.Fatalf("exactly one fresh parker must be created; created %v", rig.created)
	}
	got := rig.attachedTo()
	if len(got) != 1 || got[0] != rig.created[0] {
		t.Fatalf("the disk must land in the fresh parker %d; attaches %v", rig.created[0], got)
	}
}

// TestParkDisk_AdmitErrorFailsWithItsTypeAndWritesNothing proves an
// unanswered rule fails the park with the rule's own retriable type, before
// any attach, and never moves on to another parker.
func TestParkDisk_AdmitErrorFailsWithItsTypeAndWritesNothing(t *testing.T) {
	t.Parallel()
	rig := &admitRig{}
	c := buildParkerClient(rig.qemu(nil), listRows(
		map[string]any{"vmid": int64(90000), "node": "pve1"},
		map[string]any{"vmid": int64(90001), "node": "pve1"},
	))
	cfg := parkerTestCfg()
	cfg.Admit = func(_ context.Context, _ string, vmid int, _ string) (bool, error) {
		rig.record(vmid)
		return false, cpierrors.Retriable("the HA resource read failed")
	}

	err := pve.ParkDisk(admitCtx(), c, nopLogger(), "pve1", "nfs:vm-700-disk-1", cfg, pve.ParkContext{})
	if err == nil {
		t.Fatal("ParkDisk must fail when the selection rule cannot answer")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("the failure must keep the rule's retriable type; got %v", err)
	}
	if errors.Is(err, pve.ErrParkerNotAdmitted) {
		t.Fatalf("an unanswered rule is not a refusal; got %v", err)
	}
	if got := rig.attachedTo(); len(got) != 0 {
		t.Fatalf("nothing may be attached when the rule cannot answer; attaches %v", got)
	}
	if len(rig.created) != 0 || len(rig.asked) != 1 {
		t.Fatalf("the park must stop at the first unanswered parker; created %v asked %v", rig.created, rig.asked)
	}
}

// TestParkDisk_AdmitKeepsRegisteredReach models the AB then AC case. Parker
// 90000 is registered and its reach is pve1 and pve2, and parker 90001 is
// not registered. A disk on storage ab, which reaches pve1 and pve2, lands in
// 90000, and a disk on storage ac, which reaches pve1 and pve3, must go to
// 90001, so 90000 never holds a disk that pve2 cannot reach.
func TestParkDisk_AdmitKeepsRegisteredReach(t *testing.T) {
	t.Parallel()
	rig := &admitRig{}
	c := buildParkerClient(rig.qemu(nil), listRows(
		map[string]any{"vmid": int64(90000), "node": "pve1"},
		map[string]any{"vmid": int64(90001), "node": "pve1"},
	))
	storageNodes := map[string][]string{"ab": {"pve1", "pve2"}, "ac": {"pve1", "pve3"}}
	registeredReach := map[int][]string{90000: {"pve1", "pve2"}}
	cfg := parkerTestCfg()
	cfg.Admit = func(_ context.Context, _ string, vmid int, volid string) (bool, error) {
		reach, registered := registeredReach[vmid]
		if !registered {
			return true, nil
		}
		storage, _, _ := strings.Cut(volid, ":")
		for _, n := range reach {
			found := false
			for _, s := range storageNodes[storage] {
				found = found || s == n
			}
			if !found {
				return false, nil
			}
		}
		return true, nil
	}

	ctx := admitCtx()
	if err := pve.ParkDisk(ctx, c, nopLogger(), "pve1", "ab:vm-700-disk-1", cfg, pve.ParkContext{}); err != nil {
		t.Fatalf("ParkDisk ab: %v", err)
	}
	if err := pve.ParkDisk(ctx, c, nopLogger(), "pve1", "ac:vm-701-disk-1", cfg, pve.ParkContext{}); err != nil {
		t.Fatalf("ParkDisk ac: %v", err)
	}
	rig.mu.Lock()
	defer rig.mu.Unlock()
	if len(rig.attaches) != 2 {
		t.Fatalf("two attaches expected; got %+v", rig.attaches)
	}
	if a := rig.attaches[0]; a.vmid != 90000 || a.volid != "ab:vm-700-disk-1" {
		t.Errorf("the ab disk must land in the registered parker 90000; got %+v", a)
	}
	if a := rig.attaches[1]; a.vmid != 90001 || a.volid != "ac:vm-701-disk-1" {
		t.Errorf("the ac disk must land in parker 90001, not the registered one; got %+v", a)
	}
}

// TestParkDisk_ParkerMovedToAnotherNodeIsNotReused proves a parker PVE HA
// moved to another node leaves the band accounting intact: the park on the
// original node never tries the moved parker, and a fresh parker takes a VMID
// of its own rather than the moved parker's.
func TestParkDisk_ParkerMovedToAnotherNodeIsNotReused(t *testing.T) {
	t.Parallel()
	rig := &admitRig{}
	var nodesRead sync.Map
	q := rig.qemu(map[int]bool{90001: true})
	base := q.configFn
	q.configFn = func(node string, vmid int) (map[string]any, error) {
		nodesRead.Store(vmid, node)
		return base(node, vmid)
	}
	// 90000 failed over to pve2 with its config, and 90001 on pve1 is full.
	c := buildParkerClient(q, listRows(
		map[string]any{"vmid": int64(90000), "node": "pve2"},
		map[string]any{"vmid": int64(90001), "node": "pve1"},
	))
	cfg := parkerTestCfg()
	cfg.Admit = func(_ context.Context, node string, vmid int, _ string) (bool, error) {
		if vmid == 90000 {
			t.Errorf("the moved parker 90000 must never be asked about from node %s", node)
		}
		return true, nil
	}

	err := pve.ParkDisk(admitCtx(), c, nopLogger(), "pve1", "nfs:vm-700-disk-1", cfg, pve.ParkContext{})
	if err != nil {
		t.Fatalf("ParkDisk: %v", err)
	}
	if len(rig.created) != 1 || rig.created[0] == 90000 || rig.created[0] == 90001 {
		t.Fatalf("a fresh parker with a VMID of its own must be created; created %v", rig.created)
	}
	rig.mu.Lock()
	defer rig.mu.Unlock()
	if len(rig.attaches) != 1 || rig.attaches[0].vmid != rig.created[0] || rig.attaches[0].node != "pve1" {
		t.Fatalf("the disk must land in the fresh parker on pve1; attaches %+v", rig.attaches)
	}
	if node, ok := nodesRead.Load(90000); ok && node == "pve1" {
		t.Fatal("the moved parker's config must never be read on pve1")
	}
}
