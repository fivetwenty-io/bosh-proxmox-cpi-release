// delete_disk_parked_owned_test.go — the lab-ceph re-park deletion failure.
// After a reassignment the volume is named for the parker holding it, so
// delete_disk deallocates it through the parker's detach rather than through
// storage. These tests pin what must follow: no second delete of a volume
// that is already gone, an imgdel that races one anyway reported as success,
// and an absent volume never diagnosed as a parker deleted out-of-band.
package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdkclusterapi "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const (
	// reparkParkerVMID is the parker the disk ended up on after the last of
	// the report's seven park/unpark reassignments.
	reparkParkerVMID = 90842
	// reparkVolid is the parker-named volume that reassignment produced.
	reparkVolid = "rbd:vm-90842-disk-0"
	// reparkStableID is the disk identity riding the parker's drive entry.
	reparkStableID = "bpd-00112233aabbccdd"
)

// reparkDiskCID encodes the CID as create_disk emitted it under the parked
// strategy: a stable identity plus the parker anchor promise.
func reparkDiskCID(t *testing.T) string {
	t.Helper()
	return mustEncodeDiskCID(t, "rbd:vm-604-disk-0", &pve.DiskCIDMeta{ID: reparkStableID, Anchor: true})
}

// reparkDeps builds delete_disk Deps for a shared-RBD cluster whose parker
// sits on a different node than the configured one, as on lab-ceph.
func reparkDeps(client *mockPVEClient) handlers.Deps {
	return handlers.Deps{
		Config: &config.CPIConfig{
			Node:                     "lab-ceph-0",
			DiskStorage:              "rbd",
			DetachedDiskStrategy:     "parked",
			DiskDeleteStateGuard:     "off",
			ParkedDiskVMIDRangeStart: 90000,
			ParkedDiskVMIDRangeEnd:   90999,
		},
		PVE:    client,
		Logger: log.NewNopLogger(),
	}
}

// reparkClusterSvc places the parker on lab-ceph-2.
func reparkClusterSvc() *mockClusterSvc {
	return &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
			raw, _ := json.Marshal(map[string]any{"vmid": reparkParkerVMID, "node": "lab-ceph-2", "type": "qemu"})
			resp := sdkclusterapi.ListResourcesResponse{raw}
			return &resp, nil
		},
	}
}

// TestHandleDeleteDisk_ParkedOwned_SkipsRedundantStorageDelete is the primary
// defect. The parker's detach IS the deallocation for an owner-named volume,
// so issuing imgdel afterwards deletes nothing and can only fail.
func TestHandleDeleteDisk_ParkedOwned_SkipsRedundantStorageDelete(t *testing.T) {
	t.Parallel()

	var deleteCalls int
	storageSvc := &mockStorageService{
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			deleteCalls++
			return "", nil
		},
	}
	detached := false
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid != reparkParkerVMID {
				return map[string]any{}, nil
			}
			cfg := map[string]any{"tags": "bosh-cpi;bosh-parker", "protection": true}
			if !detached {
				cfg["scsi0"] = reparkVolid + ",serial=" + reparkStableID + ",size=10G"
			}
			return cfg, nil
		},
		detachDiskFn: func(_ context.Context, _ string, _ int, _ string) error {
			detached = true
			return nil
		},
	}

	deps := reparkDeps(&mockPVEClient{storageSvc: storageSvc, qemuSvc: qemuSvc, clusterSvc: reparkClusterSvc()})
	h := handlers.HandleDeleteDisk(deps)
	if _, err := h.Handle(context.Background(),
		[]json.RawMessage{marshal(reparkDiskCID(t))}, jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !detached {
		t.Fatal("the parked-owned deallocation must run")
	}
	if deleteCalls != 0 {
		t.Errorf("delete_disk must not issue imgdel for a volume the parker detach already deallocated, got %d calls", deleteCalls)
	}
}

// TestHandleDeleteDisk_ImgdelOnAbsentRbdImage_Idempotent is the defense in
// depth: whatever removed the image first, an imgdel that reports the image
// absent has nothing left to do, and must not surface as a delete failure.
func TestHandleDeleteDisk_ImgdelOnAbsentRbdImage_Idempotent(t *testing.T) {
	t.Parallel()

	storageSvc := &mockStorageService{
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			return "", errors.New(`rbd: error opening image vm-90842-disk-0: (2) No such file or directory`)
		},
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node: "lab-ceph-0", DiskStorage: "rbd",
			DetachedDiskStrategy: "free", DiskDeleteStateGuard: "off",
		},
		PVE:    &mockPVEClient{storageSvc: storageSvc, clusterSvc: &mockClusterSvc{}},
		Logger: log.NewNopLogger(),
	}
	h := handlers.HandleDeleteDisk(deps)
	if _, err := h.Handle(context.Background(),
		[]json.RawMessage{marshal(mustEncodeDiskCID(t, "rbd:vm-90842-disk-0", nil))}, jsonrpc.Context{}); err != nil {
		t.Fatalf("an imgdel reporting the image already absent must be idempotent success, got %v", err)
	}
}

// TestHandleDeleteDisk_AnchorMissing_VolumeGone_Idempotent covers the
// follow-on the report calls the worse half. After the deallocation the
// parker carries no scsi0 and the volume is gone, so the holder scan finds
// nothing — which the anchor guard read as "the parker was deleted
// out-of-band" and answered with advice to relax pve.parked_anchor_strict.
// A volume that is not on storage has already been deleted; that is what
// delete_disk was asked for.
func TestHandleDeleteDisk_AnchorMissing_VolumeGone_Idempotent(t *testing.T) {
	t.Parallel()

	var deleteCalls int
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) { return false, nil },
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			deleteCalls++
			return "", nil
		},
	}
	// The parker survives but holds nothing; no VM in the cluster references
	// the volume.
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"tags": "bosh-cpi;bosh-parker", "protection": true}, nil
		},
	}
	deps := reparkDeps(&mockPVEClient{storageSvc: storageSvc, qemuSvc: qemuSvc, clusterSvc: reparkClusterSvc()})
	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(reparkDiskCID(t))}, jsonrpc.Context{})
	if err != nil {
		t.Fatalf("a disk whose volume is already off storage must be idempotent success, got %v", err)
	}
	if deleteCalls != 0 {
		t.Errorf("nothing to delete, want 0 imgdel calls, got %d", deleteCalls)
	}
}

// TestHandleDeleteDisk_AnchorMissing_VolumePresent_StillRefuses keeps the
// guard that the idempotency shortcut must not weaken: a volume that IS on
// storage with no VM referencing it is the state the anchor refusal exists
// for, and it must still be refused with its recovery advice.
func TestHandleDeleteDisk_AnchorMissing_VolumePresent_StillRefuses(t *testing.T) {
	t.Parallel()

	var deleteCalls int
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) { return true, nil },
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			deleteCalls++
			return "", nil
		},
	}
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"tags": "bosh-cpi;bosh-parker", "protection": true}, nil
		},
	}
	deps := reparkDeps(&mockPVEClient{storageSvc: storageSvc, qemuSvc: qemuSvc, clusterSvc: reparkClusterSvc()})
	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(reparkDiskCID(t))}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("a promised-anchor volume still on storage with no holder must be refused")
	}
	if !strings.Contains(err.Error(), "parked_anchor_strict") {
		t.Errorf("the refusal must keep its recovery advice, got %v", err)
	}
	if deleteCalls != 0 {
		t.Errorf("a refused delete must not reach storage, got %d imgdel calls", deleteCalls)
	}
}

// TestHandleDeleteDisk_AnchorMissing_ExistenceUnprovable_Refuses is the
// fail-safe direction: when the existence probe itself fails, absence is not
// established, and the refusal stands rather than a delete proceeding on a
// guess.
func TestHandleDeleteDisk_AnchorMissing_ExistenceUnprovable_Refuses(t *testing.T) {
	t.Parallel()

	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, _ string) (bool, error) {
			return false, errors.New("ceph mon unreachable")
		},
	}
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"tags": "bosh-cpi;bosh-parker", "protection": true}, nil
		},
	}
	deps := reparkDeps(&mockPVEClient{storageSvc: storageSvc, qemuSvc: qemuSvc, clusterSvc: reparkClusterSvc()})
	h := handlers.HandleDeleteDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(reparkDiskCID(t))}, jsonrpc.Context{})
	if err == nil {
		t.Fatal("an unprovable absence must not be treated as a completed delete")
	}
}

// ---------------------------------------------------------------------------
// The file-storage twins of the three cases above. On dir, NFS, and CIFS
// storage PVE answers a single-volume GET for a file it cannot stat with an
// HTTP 500 naming volume_size_info rather than a 404, so the point probe
// settles nothing and the storage content listing is what proves absence.
// ---------------------------------------------------------------------------

const (
	// fileAnchorNFSStorage is an NFS storage, whose listing PVE refuses to
	// serve at all when the export is down, so a listing that omits the
	// volume proves the volume is gone.
	fileAnchorNFSStorage = "nfs-images"
	fileAnchorNFSVolid   = "nfs-images:604/vm-604-disk-0.qcow2"
	// fileAnchorDirStorage is a plain dir storage with no is_mountpoint,
	// which lists an empty array when its mount drops instead of failing.
	fileAnchorDirStorage = "dir-images"
	fileAnchorDirVolid   = "dir-images:604/vm-604-disk-0.qcow2"
	fileAnchorNode       = "lab-file-0"
)

// fileAnchorProof wires one delete_disk call against file storage: the parker
// survives holding nothing, the point probe answers PVE's "no format" 500, and
// the content listing and the audit-visibility proof are whatever the case
// under test needs them to be.
type fileAnchorProof struct {
	deps        handlers.Deps
	observer    *log.Observer
	deleteCalls *int
}

func newFileAnchorProof(
	storageName, storageType, isMountpoint string,
	listing func() (*sdknodes.ListStorageContentResponse, error),
	visibilityErr error,
) fileAnchorProof {
	// The storage type alone decides shared versus local, exactly as it does
	// in PVE: nfs is cluster-visible and dir is node-local. Nothing here sets
	// a shared flag by hand, because a dir storage flagged shared is a shape
	// production never produces, and testing against it would hide the node
	// sweep a local backend really runs before delete_disk sees the disk.
	deleteCalls := 0
	storageSvc := &mockStorageService{
		existsFn: func(_ context.Context, _, _, volume string) (bool, error) {
			return false, nfsNoFormat(volume)
		},
		deleteVolumeAsyncFn: func(_ context.Context, _, _, _ string) (string, error) {
			deleteCalls++
			return "", nil
		},
	}
	// The parker is still there and holds nothing, so the holder scan finds
	// no VM referencing the volume.
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"tags": "bosh-cpi;bosh-parker", "protection": true}, nil
		},
	}
	nodesSvc := &mockNodesService{
		listStorageContentFn: func(
			_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams,
		) (*sdknodes.ListStorageContentResponse, error) {
			return listing()
		},
	}
	client := &visiblePVEClient{
		mockPVEClient: &mockPVEClient{
			storageSvc: storageSvc,
			qemuSvc:    qemuSvc,
			nodesSvc:   nodesSvc,
			clusterSvc: reparkClusterSvc(),
			clusterStorageSvc: &mockClusterStorage{
				storageName:  storageName,
				storageType:  storageType,
				isMountpoint: isMountpoint,
				// A local storage restricted to the one node the fixture
				// runs on keeps the node sweep off the cluster membership
				// listing, which this suite does not script.
				nodes: fileAnchorNode,
			},
		},
		visibilityErr: visibilityErr,
	}
	logger, observer := log.NewObservedLogger(log.LevelWarn)
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:                     fileAnchorNode,
			DiskStorage:              storageName,
			DetachedDiskStrategy:     "parked",
			DiskDeleteStateGuard:     "off",
			ParkedDiskVMIDRangeStart: 90000,
			ParkedDiskVMIDRangeEnd:   90999,
		},
		PVE:      client,
		Resolver: liveBackendResolver(client, fileAnchorNode),
		Logger:   logger,
	}
	return fileAnchorProof{deps: deps, observer: observer, deleteCalls: &deleteCalls}
}

// deleteFileAnchorDisk runs delete_disk against a promised-anchor CID naming
// the given volume.
func deleteFileAnchorDisk(t *testing.T, fixture fileAnchorProof, volid string) error {
	t.Helper()
	cid := mustEncodeDiskCID(t, volid, &pve.DiskCIDMeta{ID: reparkStableID, Anchor: true})
	h := handlers.HandleDeleteDisk(fixture.deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{marshal(cid)}, jsonrpc.Context{})
	return err
}

// emptyStorageListing is the reply PVE gives for a storage holding nothing,
// and also the reply a dir storage gives once its mount has dropped.
func emptyStorageListing() (*sdknodes.ListStorageContentResponse, error) {
	return storageContentListing(), nil
}

// warnsAbsenceUnproven reports whether the run logged the warning that says
// the refusal is standing on an absence nobody could establish.
func warnsAbsenceUnproven(observer *log.Observer) bool {
	for _, entry := range observer.All() {
		if entry.Level == log.LevelWarn && strings.Contains(entry.Message, "absence could not be proven") {
			return true
		}
	}
	return false
}

// TestHandleDeleteDisk_AnchorMissing_NFSVolumeGone_Idempotent is the live
// defect. The point probe cannot answer on NFS, and reading its failure as
// "unproven" left delete_disk refusing a disk that was already gone.
func TestHandleDeleteDisk_AnchorMissing_NFSVolumeGone_Idempotent(t *testing.T) {
	t.Parallel()

	fixture := newFileAnchorProof(fileAnchorNFSStorage, "nfs", "", emptyStorageListing, nil)
	if err := deleteFileAnchorDisk(t, fixture, fileAnchorNFSVolid); err != nil {
		t.Fatalf("a volume an NFS listing proves gone must be idempotent success, got %v", err)
	}
	if *fixture.deleteCalls != 0 {
		t.Errorf("nothing to delete, want 0 imgdel calls, got %d", *fixture.deleteCalls)
	}
}

// TestHandleDeleteDisk_AnchorMissing_NFSVolumePresent_StillRefuses keeps the
// guard the idempotency shortcut must not weaken: the listing carries the
// volume, so it is there, and the anchor refusal stands with its recovery.
func TestHandleDeleteDisk_AnchorMissing_NFSVolumePresent_StillRefuses(t *testing.T) {
	t.Parallel()

	listing := func() (*sdknodes.ListStorageContentResponse, error) {
		return storageContentListing(fileAnchorNFSVolid), nil
	}
	fixture := newFileAnchorProof(fileAnchorNFSStorage, "nfs", "", listing, nil)
	err := deleteFileAnchorDisk(t, fixture, fileAnchorNFSVolid)
	if err == nil {
		t.Fatal("a promised-anchor volume the listing still carries must be refused")
	}
	if !strings.Contains(err.Error(), "parked_anchor_strict") {
		t.Errorf("the refusal must keep its recovery advice, got %v", err)
	}
	if *fixture.deleteCalls != 0 {
		t.Errorf("a refused delete must not reach storage, got %d imgdel calls", *fixture.deleteCalls)
	}
}

// TestHandleDeleteDisk_AnchorMissing_NFSListingFails_Refuses is the fail-safe
// direction: the one observation that could have settled the question did not
// land, so the refusal stands and the log says why.
func TestHandleDeleteDisk_AnchorMissing_NFSListingFails_Refuses(t *testing.T) {
	t.Parallel()

	listing := func() (*sdknodes.ListStorageContentResponse, error) {
		return nil, errors.New("storage 'nfs-images' is not online")
	}
	fixture := newFileAnchorProof(fileAnchorNFSStorage, "nfs", "", listing, nil)
	if err := deleteFileAnchorDisk(t, fixture, fileAnchorNFSVolid); err == nil {
		t.Fatal("an unprovable absence must not be treated as a completed delete")
	}
	if !warnsAbsenceUnproven(fixture.observer) {
		t.Error("a refusal standing on an unproven absence must say so in the log")
	}
	if *fixture.deleteCalls != 0 {
		t.Errorf("a refused delete must not reach storage, got %d imgdel calls", *fixture.deleteCalls)
	}
}

// TestHandleDeleteDisk_AnchorMissing_NFSVisibilityUnproven_Refuses covers the
// reduced-ACL token. Without Sys.Audit at /access the listing may be
// permission filtered, so a volume missing from it proves nothing.
func TestHandleDeleteDisk_AnchorMissing_NFSVisibilityUnproven_Refuses(t *testing.T) {
	t.Parallel()

	fixture := newFileAnchorProof(fileAnchorNFSStorage, "nfs", "", emptyStorageListing,
		errors.New("permission check failed (/access, Sys.Audit)"))
	if err := deleteFileAnchorDisk(t, fixture, fileAnchorNFSVolid); err == nil {
		t.Fatal("a listing that may be permission filtered must not prove absence")
	}
	if !warnsAbsenceUnproven(fixture.observer) {
		t.Error("a refusal standing on an unproven absence must say so in the log")
	}
	if *fixture.deleteCalls != 0 {
		t.Errorf("a refused delete must not reach storage, got %d imgdel calls", *fixture.deleteCalls)
	}
}

// TestHandleDeleteDisk_AnchorMissing_PlainDirEmptyListing_Refuses is the
// dropped-mount case. PVE lists a dir storage with no is_mountpoint as an
// empty array once its mount goes away, exactly as it lists a storage that is
// genuinely empty, so an empty listing settles nothing there.
//
// A dir storage is node-local, so the refusal an operator actually meets
// comes from the node sweep rather than from the anchor guard: delete_disk
// asks the backend which node holds the volume, every candidate answers
// unproven, and a sweep that could not prove the volume absent anywhere is
// retriable by design. What matters is that the delete does not proceed and
// that the message carries the fix.
func TestHandleDeleteDisk_AnchorMissing_PlainDirEmptyListing_Refuses(t *testing.T) {
	t.Parallel()

	fixture := newFileAnchorProof(fileAnchorDirStorage, "dir", "", emptyStorageListing, nil)
	err := deleteFileAnchorDisk(t, fixture, fileAnchorDirVolid)
	if err == nil {
		t.Fatal("an empty listing on a plain dir storage must not prove absence")
	}
	if !strings.Contains(err.Error(), "is_mountpoint") {
		t.Errorf("the message must name the fix that turns a dropped mount into an honest failure, got %v", err)
	}
	if !strings.Contains(err.Error(), fileAnchorDirStorage) {
		t.Errorf("the message must name the storage to set it on, got %v", err)
	}
	if !okToRetry(err) {
		t.Errorf("a sweep that could not prove the volume absent anywhere is retriable, got %v", err)
	}
	if *fixture.deleteCalls != 0 {
		t.Errorf("a refused delete must not reach storage, got %d imgdel calls", *fixture.deleteCalls)
	}
}

// okToRetry reports whether the Director may re-drive the call that produced
// err.
func okToRetry(err error) bool {
	var typed *cpierrors.Error
	return errors.As(err, &typed) && typed.OkToRetry()
}

// TestHandleDeleteDisk_AnchorMissing_PlainDirPopulatedListing_Idempotent is
// the other half of that rule. Other volumes in the listing prove the tree is
// really mounted, which is the evidence an empty array cannot give. On a
// node-local dir storage the sweep reaches that conclusion for every node, so
// delete_disk settles the call as already done before the anchor guard runs.
func TestHandleDeleteDisk_AnchorMissing_PlainDirPopulatedListing_Idempotent(t *testing.T) {
	t.Parallel()

	listing := func() (*sdknodes.ListStorageContentResponse, error) {
		return storageContentListing(
			"dir-images:701/vm-701-disk-0.qcow2",
			"dir-images:702/vm-702-disk-0.qcow2",
		), nil
	}
	fixture := newFileAnchorProof(fileAnchorDirStorage, "dir", "", listing, nil)
	if err := deleteFileAnchorDisk(t, fixture, fileAnchorDirVolid); err != nil {
		t.Fatalf("a populated dir listing that omits the volume proves it gone, got %v", err)
	}
	if *fixture.deleteCalls != 0 {
		t.Errorf("nothing to delete, want 0 imgdel calls, got %d", *fixture.deleteCalls)
	}
}

// TestHandleDeleteDisk_AnchorMissing_DirWithIsMountpoint_Idempotent pins the
// fix the unproven message advises. With is_mountpoint set, PVE refuses to
// activate the storage when the mount is gone, so an empty listing from a
// storage that answered at all is proof, and the same delete that hung on the
// message above now completes.
func TestHandleDeleteDisk_AnchorMissing_DirWithIsMountpoint_Idempotent(t *testing.T) {
	t.Parallel()

	fixture := newFileAnchorProof(fileAnchorDirStorage, "dir", "1", emptyStorageListing, nil)
	if err := deleteFileAnchorDisk(t, fixture, fileAnchorDirVolid); err != nil {
		t.Fatalf("is_mountpoint makes an empty dir listing proof of absence, got %v", err)
	}
	if *fixture.deleteCalls != 0 {
		t.Errorf("nothing to delete, want 0 imgdel calls, got %d", *fixture.deleteCalls)
	}
}

// TestHandleDeleteDisk_AnchorMissing_UnclassifiableStorage_Refuses pins what a
// storage the CPI cannot look up means. A storage it cannot classify is one
// whose listing cannot carry a proof, so the absence stays unproven rather
// than falling open.
func TestHandleDeleteDisk_AnchorMissing_UnclassifiableStorage_Refuses(t *testing.T) {
	t.Parallel()

	fixture := newFileAnchorProof(fileAnchorNFSStorage, "nfs", "", emptyStorageListing, nil)
	// A client with no cluster-storage service is what a token that cannot
	// read the storage index sees, and it is what an unwired caller sees too.
	fixture.deps.PVE.(*visiblePVEClient).clusterStorageSvc = nil
	if err := deleteFileAnchorDisk(t, fixture, fileAnchorNFSVolid); err == nil {
		t.Fatal("a storage the CPI cannot classify must not yield a proven absence")
	}
	if !warnsAbsenceUnproven(fixture.observer) {
		t.Error("a refusal standing on an unproven absence must say so in the log")
	}
}
