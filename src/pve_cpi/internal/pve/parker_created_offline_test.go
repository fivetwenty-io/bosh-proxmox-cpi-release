package pve_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// offlineParkVolid is the disk every test below parks or looks up. No guest
// on the online node holds it.
const offlineParkVolid = "local-lvm:vm-9001-disk-0"

// offlineParkCluster builds a two-node cluster for the park tests. pve1 is
// online and runs parker 90000, which has scsi1 free, and workload VM 501,
// which holds an unrelated disk. pve2 is reported offline by the quorate
// cluster, and its listing fails as a powered-off member's does. The
// returned counter tracks AttachDisk calls.
func offlineParkCluster() (pve.Client, *int) {
	attaches := 0
	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			if vmid == 90000 {
				return map[string]any{"tags": "bosh-parker", "scsi0": "local-lvm:vm-9999-disk-0"}, nil
			}
			return map[string]any{"scsi0": "local-lvm:vm-501-disk-0"}, nil
		},
		attachFn: func(_ string, _ int, _, _ string, _ *qemu.AttachOpts) (string, error) {
			attaches++
			return "", nil
		},
	}
	fc := &diskFakeCluster{
		listFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return parkerClusterResp(
				map[string]any{"vmid": int64(90000), "node": "pve1"},
				map[string]any{"vmid": int64(501), "node": "pve1"},
				map[string]any{"vmid": int64(602), "node": "pve2"},
			), nil
		},
		listStatusFn: func(context.Context) (*cluster.ListStatusResponse, error) {
			return diskQuorateStatusWithOffline("pve2"), nil
		},
	}
	c := &diskClusterClient{
		qemuSvc:    qemuSvc,
		clusterSvc: fc,
		nodesOverride: &diskFakeNodesFromCluster{
			f:        fc,
			failNode: "pve2",
			failErr:  errors.New("pve2: no route to host (member is powered off)"),
		},
	}
	return c, &attaches
}

// requireOfflineRefusal fails the test unless err is the holder scan's
// retriable refusal for offlineParkVolid, naming pve2 and telling the
// operator to retry once it is back.
func requireOfflineRefusal(t *testing.T, err error, where string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: want the offline refusal, got nil", where)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("%s: want a retriable error, got: %v", where, err)
	}
	var refusal *pve.DiskHolderOfflineRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("%s: want a DiskHolderOfflineRefusal in the chain, got: %v", where, err)
	}
	if refusal.Volid != offlineParkVolid || !slices.Equal(refusal.Nodes, []string{"pve2"}) {
		t.Errorf("%s: refusal = %+v; want volid %s and nodes [pve2]", where, *refusal, offlineParkVolid)
	}
	text := err.Error()
	if !strings.Contains(text, "node(s) pve2 were excluded as offline") ||
		!strings.Contains(text, "retry the operation once node(s) pve2 are back in the cluster") {
		t.Errorf("%s: the error must name pve2 and say to retry once it is back, got: %v", where, err)
	}
	if errors.Is(err, pve.ErrDiskNotAttachedToAnyVM) {
		t.Errorf("%s: the refusal must not read as not attached, got: %v", where, err)
	}
}

// TestParkDisk_ExistingDiskStillRefusesWhileANodeIsOffline: a park whose
// holder scan can't read an offline node refuses, names the offline node, and tells
// the operator to retry once it is back. Nothing is attached.
func TestParkDisk_ExistingDiskStillRefusesWhileANodeIsOffline(t *testing.T) {
	t.Parallel()
	c, attaches := offlineParkCluster()

	err := pve.ParkDisk(context.Background(), c, nopLogger(), "pve1", offlineParkVolid, parkerTestCfg(), pve.ParkContext{})
	requireOfflineRefusal(t, err, "ParkDisk")
	if *attaches != 0 {
		t.Fatalf("AttachDisk calls = %d; want none after the refusal", *attaches)
	}
}

// TestHolderScanCallersStillRefuseWhileANodeIsOffline: every other entry
// point to the holder scan keeps failing closed while a node is offline, and
// each names the node and says to retry once it is back. These are the reads
// attach, detach, delete, the orphan sweeps, and the parker moves rest on.
func TestHolderScanCallersStillRefuseWhileANodeIsOffline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	callers := map[string]func(pve.Client) error{
		"FindVMByDiskVolid": func(c pve.Client) error {
			_, _, err := pve.FindVMByDiskVolid(ctx, c, offlineParkVolid)
			return err
		},
		"FindVMByDiskVolidOrNone": func(c pve.Client) error {
			_, _, _, err := pve.FindVMByDiskVolidOrNone(ctx, c, offlineParkVolid)
			return err
		},
		"ResolveDiskHolder": func(c pve.Client) error {
			_, err := pve.ResolveDiskHolder(ctx, c, nopLogger(), offlineParkVolid, parkerTestCfg())
			return err
		},
		"IsDiskParked": func(c pve.Client) error {
			_, _, _, _, err := pve.IsDiskParked(ctx, c, nopLogger(), offlineParkVolid, parkerTestCfg())
			return err
		},
		"DiskHeldByRealVM": func(c pve.Client) error {
			_, _, _, err := pve.DiskHeldByRealVM(ctx, c, nopLogger(), offlineParkVolid, parkerTestCfg())
			return err
		},
	}
	for name, call := range callers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, _ := offlineParkCluster()
			requireOfflineRefusal(t, call(c), name)
		})
	}
}
