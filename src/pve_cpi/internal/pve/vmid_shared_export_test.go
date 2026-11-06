package pve_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// isoScanClient serves one storage listing per storage name and an images
// storage list holding imagesStorages.
func isoScanClient(contents map[string][]string, imagesStorages ...string) (*mockClient, *imageScanNodes) {
	nodesSvc := &imageScanNodes{
		listStorageFn: func(context.Context, string, *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error) {
			rows := make([]map[string]any, 0, len(imagesStorages))
			for _, name := range imagesStorages {
				rows = append(rows, imagesRow(name))
			}
			return nodeStorageRows(rows...), nil
		},
	}
	nodesSvc.listStorageContentFn = func(_ context.Context, _, storage string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		return buildStorageContent(contents[storage]...), nil
	}
	return newImageScanClient(nodesSvc), nodesSvc
}

func TestNextVMID_ConfigDriveISOOnVMStorageReservesItsVMID(t *testing.T) {
	t.Parallel()
	client, _ := isoScanClient(map[string][]string{"nfs": {"nfs:iso/vm-300-config.iso"}})
	for range 20 {
		got, err := pve.NextVMID(context.Background(), client, pve.WithRange(300, 301), pve.WithStorageScan("pve1", "nfs"))
		if err != nil {
			t.Fatalf("NextVMID: %v", err)
		}
		if got != 301 {
			t.Fatalf("NextVMID = %d, want 301: nfs:iso/vm-300-config.iso reserves 300", got)
		}
	}
}

func TestNextVMID_ConfigDriveISOOnDistinctISOStorageReservesItsVMID(t *testing.T) {
	t.Parallel()
	client, nodesSvc := isoScanClient(map[string][]string{
		"data":    {},
		"isopool": {"isopool:iso/vm-300-config.iso"},
	}, "data")
	for range 20 {
		got, err := pve.NextVMID(context.Background(), client, pve.WithRange(300, 301),
			pve.WithNodeImageStorageScan("pve1"), pve.WithExtraStorageScan("pve1", "isopool"))
		if err != nil {
			t.Fatalf("NextVMID: %v", err)
		}
		if got != 301 {
			t.Fatalf("NextVMID = %d, want 301: the ISO pool's vm-300-config.iso reserves 300", got)
		}
	}
	if nodesSvc.listed["isopool"] == 0 {
		t.Error("the distinct ISO pool was never listed")
	}
}

func TestNextVMID_ConfigDriveISOOutsideTheScanIsNotRead(t *testing.T) {
	t.Parallel()
	client, nodesSvc := isoScanClient(map[string][]string{
		"data":    {},
		"isopool": {"isopool:iso/vm-300-config.iso"},
	}, "data")
	seen := map[int]bool{}
	for range 60 {
		got, err := pve.NextVMID(context.Background(), client, pve.WithRange(300, 301), pve.WithNodeImageStorageScan("pve1"))
		if err != nil {
			t.Fatalf("NextVMID: %v", err)
		}
		seen[got] = true
	}
	if !seen[300] {
		t.Error("300 was never handed out, though no scanned storage names it")
	}
	if nodesSvc.listed["isopool"] != 0 {
		t.Error("a storage outside the scan was listed")
	}
}

func TestNextVMID_LogsStorageOnlyReservations(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := log.NewLogger("info", &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	ctx := log.IntoContext(context.Background(), logger)
	client, _ := isoScanClient(map[string][]string{"nfs": {
		"nfs:vm-307-disk-0", "nfs:vm-301-disk-0", "nfs:iso/vm-306-config.iso", "nfs:vm-303-disk-0",
		"nfs:vm-302-disk-0", "nfs:vm-305-disk-0", "nfs:vm-304-disk-0",
		// Outside the range: reserved, but not counted in the line.
		"nfs:vm-999-disk-0",
	}})
	// 300 is a guest of this cluster, so the cluster listing already holds it.
	client.clusterSvc = &stubClusterService{listResourcesFn: func(context.Context, *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(300), nil
	}}
	got, err := pve.NextVMID(ctx, client, pve.WithRange(300, 308), pve.WithStorageScan("pve1", "nfs"))
	if err != nil {
		t.Fatalf("NextVMID: %v", err)
	}
	if got != 308 {
		t.Fatalf("NextVMID = %d, want 308", got)
	}
	out := buf.String()
	if n := strings.Count(out, "VMID allocation skipped IDs that only storage holds"); n != 1 {
		t.Fatalf("info line logged %d times, want once:\n%s", n, out)
	}
	for _, want := range []string{`"count":7`, `"ids":"301,302,303,304,305"`, `"range":"300-308"`} {
		if !strings.Contains(out, want) {
			t.Errorf("info line lacks %s:\n%s", want, out)
		}
	}
}

func TestNextVMID_NoInfoLineWhenStorageAddsNothing(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := log.NewLogger("info", &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	ctx := log.IntoContext(context.Background(), logger)
	client, _ := isoScanClient(map[string][]string{"nfs": {"nfs:vm-300-disk-0"}})
	client.clusterSvc = &stubClusterService{listResourcesFn: func(context.Context, *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(300), nil
	}}
	if _, err := pve.NextVMID(ctx, client, pve.WithRange(300, 308), pve.WithStorageScan("pve1", "nfs")); err != nil {
		t.Fatalf("NextVMID: %v", err)
	}
	if strings.Contains(buf.String(), "only storage holds") {
		t.Errorf("info line logged though the storage scan added no ID:\n%s", buf.String())
	}
}

func TestNextDiskVMID_LogsExistingDisksAtDebugOnly(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := log.NewLogger("info", &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	ctx := log.IntoContext(context.Background(), logger)
	client, _ := isoScanClient(map[string][]string{"nfs": {"nfs:vm-9000-disk-0"}})
	if _, err := pve.NextDiskVMID(ctx, client, "pve1", "nfs", pve.WithRange(9000, 9001)); err != nil {
		t.Fatalf("NextDiskVMID: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("the disk allocator logged at info level for its own persistent disks:\n%s", buf.String())
	}
}

func TestNextDiskVMID_LogsStorageOnlyReservations(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := log.NewLogger("debug", &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	ctx := log.IntoContext(context.Background(), logger)
	client, _ := isoScanClient(map[string][]string{"nfs": {"nfs:vm-9000-disk-0"}})
	got, err := pve.NextDiskVMID(ctx, client, "pve1", "nfs", pve.WithRange(9000, 9001))
	if err != nil {
		t.Fatalf("NextDiskVMID: %v", err)
	}
	if got != 9001 {
		t.Fatalf("NextDiskVMID = %d, want 9001", got)
	}
	for _, want := range []string{`"ids":"9000"`, "existing persistent disks"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("disk allocator debug line lacks %s:\n%s", want, buf.String())
		}
	}
	if strings.Contains(buf.String(), "another cluster") {
		t.Errorf("disk allocator line blames another cluster:\n%s", buf.String())
	}
}

func TestAllocateWithRetry_FailedContentListingSendsNoCreate(t *testing.T) {
	t.Parallel()
	nodesSvc := &imageScanNodes{
		listStorageFn: func(context.Context, string, *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error) {
			return nodeStorageRows(imagesRow("data")), nil
		},
	}
	nodesSvc.listStorageContentFn = func(context.Context, string, string, *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		return nil, errors.New("storage 'isopool' is not online")
	}
	client := newImageScanClient(nodesSvc)
	creates := 0
	_, err := pve.AllocateWithRetry(context.Background(), client,
		func(int) error {
			creates++
			return nil
		},
		func(error) bool { return true },
		3,
		pve.WithRange(300, 301), pve.WithNoBackoff(),
		pve.WithNodeImageStorageScan("pve1"), pve.WithExtraStorageScan("pve1", "isopool"))
	if err == nil {
		t.Fatal("allocation succeeded although a storage content listing failed")
	}
	if creates != 0 {
		t.Errorf("create sent %d times after a failed content listing, want none", creates)
	}
}
