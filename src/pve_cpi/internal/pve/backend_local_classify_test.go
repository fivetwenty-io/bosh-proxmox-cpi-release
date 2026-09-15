// Tests for the local backend's live storage classification: the answer it
// gives ProveVolumeAbsent when the point probe has failed and the proof is
// about to read what an empty content listing means.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// classifyStorageName is the storage every case below classifies, and
// classifyVolid is a volume on it. The volid carries the storage in its first
// segment because the content listing parses the storage out of the volume.
const (
	classifyStorageName = "dir-images"
	classifyVolid       = "dir-images:vm-100-disk-0.qcow2"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeContentNodes is a nodes.Service fake exposing only ListStorageContent,
// the one call the absence proof makes on that service. It counts its calls so
// a sweep can assert how many nodes it actually listed.
type fakeContentNodes struct {
	nodes.Service
	calls  atomic.Int32
	volids []string
}

func (f *fakeContentNodes) ListStorageContent(
	_ context.Context, _, _ string, _ *nodes.ListStorageContentParams,
) (*nodes.ListStorageContentResponse, error) {
	f.calls.Add(1)
	out := make(nodes.ListStorageContentResponse, 0, len(f.volids))
	for _, volid := range f.volids {
		raw, err := json.Marshal(map[string]string{"volid": volid})
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return &out, nil
}

// countingStorageIndex serves one /storage index and counts the reads, which
// is how the memoization test sees a single live classification behind a
// multi-node sweep. It wraps fakeClusterStorageService (storage_info_test.go)
// rather than reimplementing clusterstorage.Service a second time.
type countingStorageIndex struct {
	svc     *fakeClusterStorageService
	calls   atomic.Int32
	entries []string
	err     error
}

func newCountingStorageIndex(err error, entries ...string) *countingStorageIndex {
	idx := &countingStorageIndex{entries: entries, err: err}
	idx.svc = &fakeClusterStorageService{
		listFn: func(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
			idx.calls.Add(1)
			if idx.err != nil {
				return nil, idx.err
			}
			out := make(clusterstorage.ListStorageResponse, 0, len(idx.entries))
			for _, entry := range idx.entries {
				out = append(out, json.RawMessage(entry))
			}
			return &out, nil
		},
	}
	return idx
}

// visibleBackendClient answers the audit-visibility proof that the content
// listing demands before it will call a volume absent. backendTestClient does
// not implement it, so a case that needs a provable absence wraps it here.
type visibleBackendClient struct {
	*backendTestClient
}

func (c *visibleBackendClient) StorageAuditVisibility(context.Context) error { return nil }

// noFormatProbeErr is PVE's reply to a volume stat that did not work on file
// storage. ExistsTolerant cannot fold it either way, so the proof falls
// through to the content listing, which is the only path that consults the
// classifier.
func noFormatProbeErr(volid string) error {
	body := []byte(`{"message":"volume_size_info on '` + volid + `' failed - no format","code":500}`)
	return sdkerrors.ParseAPIError(500, body)
}

// classifySweep assembles a backend whose probes all fail with the no-format
// reply and whose content listing answers with listed, over the named nodes.
// It returns the backend and the two fakes the assertions read.
func classifySweep(
	t *testing.T, captured StorageInfo, index *countingStorageIndex, listed []string, nodeNames ...string,
) (Backend, *fakeContentNodes) {
	t.Helper()
	content := &fakeContentNodes{volids: listed}
	rows := make([]map[string]any, 0, len(nodeNames))
	for _, name := range nodeNames {
		rows = append(rows, map[string]any{"node": name})
	}
	base := &backendTestClient{
		storageSvc: &fakeStorage{
			existsFn: func(_ context.Context, _, _, volume string) (bool, error) {
				return false, noFormatProbeErr(volume)
			},
		},
		clusterSvc: &fakeCluster{
			listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
				return clusterResp(rows...), nil
			},
		},
		nodesSvc: content,
	}
	if index != nil {
		base.clusterStorageSvc = index.svc
	}
	return newLocalBackend(&visibleBackendClient{backendTestClient: base}, captured, "", nil), content
}

// ---------------------------------------------------------------------------
// the live read wins over the captured info
// ---------------------------------------------------------------------------

// TestLocalBackend_Classify_LiveReadBeatsCapturedInfo is the pair this change
// exists for. The captured StorageInfo says plain dir, on which an empty
// listing proves nothing, while the live index says the same storage carries
// is_mountpoint, on which PVE refuses to list at all once the mount drops. The
// live answer is the one the proof gets, so the empty listing becomes an
// absence and the sweep reports the disk missing rather than unproven.
func TestLocalBackend_Classify_LiveReadBeatsCapturedInfo(t *testing.T) {
	t.Parallel()

	captured := StorageInfo{Name: classifyStorageName, Type: StorageTypeDir}
	index := newCountingStorageIndex(nil,
		`{"storage":"other-store","type":"nfs"}`,
		`{"storage":"dir-images","type":"dir","is_mountpoint":"1"}`,
	)
	b, content := classifySweep(t, captured, index, nil, "pve-01")

	_, err := b.NodeForExisting(context.Background(), classifyVolid)
	if err == nil {
		t.Fatalf("expected DiskNotFound once the live classification proved the absence")
	}
	if !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Fatalf("got %v, want DiskNotFound (the live is_mountpoint makes the empty listing a proof)", err)
	}
	if got := index.calls.Load(); got != 1 {
		t.Fatalf("live /storage reads = %d, want 1", got)
	}
	if got := content.calls.Load(); got != 1 {
		t.Fatalf("content listings = %d, want 1", got)
	}
}

// TestLocalBackend_Classify_CapturedInfoAloneLeavesItUnproven is the control
// for the case above: the same captured dir storage with no live read
// available keeps the conservative answer, so the empty listing proves nothing
// and the sweep comes back retriable rather than DiskNotFound.
func TestLocalBackend_Classify_CapturedInfoAloneLeavesItUnproven(t *testing.T) {
	t.Parallel()

	captured := StorageInfo{Name: classifyStorageName, Type: StorageTypeDir}
	b, _ := classifySweep(t, captured, nil, nil, "pve-01")

	_, err := b.NodeForExisting(context.Background(), classifyVolid)
	if err == nil {
		t.Fatalf("expected an unproven absence to surface as an error")
	}
	if cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Fatalf("got DiskNotFound, want a retriable unproven answer; err=%v", err)
	}
	if !strings.Contains(err.Error(), "content listing came back empty") {
		t.Fatalf("error does not name the empty listing: %v", err)
	}
}

// TestLocalBackend_Classify_LiveReadMadeOnceAcrossSweep pins the memoization.
// Three nodes each fail their point probe and each read a content listing, and
// all three classifier calls are served by one live /storage read.
func TestLocalBackend_Classify_LiveReadMadeOnceAcrossSweep(t *testing.T) {
	t.Parallel()

	captured := StorageInfo{Name: classifyStorageName, Type: StorageTypeDir}
	index := newCountingStorageIndex(nil, `{"storage":"dir-images","type":"dir","is_mountpoint":"1"}`)
	b, content := classifySweep(t, captured, index, nil, "pve-01", "pve-02", "pve-03")

	_, err := b.NodeForExisting(context.Background(), classifyVolid)
	if !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Fatalf("got %v, want DiskNotFound after a complete sweep", err)
	}
	if got := content.calls.Load(); got != 3 {
		t.Fatalf("content listings = %d, want 3 (one per candidate node)", got)
	}
	if got := index.calls.Load(); got != 1 {
		t.Fatalf("live /storage reads = %d, want 1 for the whole sweep", got)
	}
}

// TestLocalBackend_Classify_NoClusterStorageFallsBackToCapturedType covers the
// client that wires no cluster storage service at all, which is the shape most
// fakes take. The captured nfs classification still stands, so an empty
// listing on a type PVE refuses to serve when its backing is gone is still an
// absence, and today's behavior on those clients is unchanged.
func TestLocalBackend_Classify_NoClusterStorageFallsBackToCapturedType(t *testing.T) {
	t.Parallel()

	captured := StorageInfo{Name: classifyStorageName, Type: StorageTypeNFS}
	b, content := classifySweep(t, captured, nil, nil, "pve-01")

	_, err := b.NodeForExisting(context.Background(), classifyVolid)
	if !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Fatalf("got %v, want DiskNotFound from the captured nfs classification", err)
	}
	if got := content.calls.Load(); got != 1 {
		t.Fatalf("content listings = %d, want 1", got)
	}
}

// TestLocalBackend_Classify_LiveReadErrorFallsBackToCapturedType proves the
// fallback is about the answer being unavailable rather than about the service
// being absent: a live read that fails on the wire leaves the captured nfs
// classification in charge.
func TestLocalBackend_Classify_LiveReadErrorFallsBackToCapturedType(t *testing.T) {
	t.Parallel()

	captured := StorageInfo{Name: classifyStorageName, Type: StorageTypeNFS}
	index := newCountingStorageIndex(errors.New("api unreachable"))
	b, _ := classifySweep(t, captured, index, nil, "pve-01")

	_, err := b.NodeForExisting(context.Background(), classifyVolid)
	if !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Fatalf("got %v, want DiskNotFound from the captured nfs classification", err)
	}
	if got := index.calls.Load(); got != 1 {
		t.Fatalf("live /storage reads = %d, want 1 attempt", got)
	}
}

// TestLocalBackend_Classify_FabricatedInfoIsUnproven covers the resolver's
// cache-miss fallback, which builds a backend around a StorageInfo carrying
// nothing but the name. With no live read to replace it, that is not a
// classification, and the proof is told so rather than being handed an empty
// type it would read as an unrecognized storage.
func TestLocalBackend_Classify_FabricatedInfoIsUnproven(t *testing.T) {
	t.Parallel()

	captured := StorageInfo{Name: classifyStorageName}
	b, _ := classifySweep(t, captured, nil, nil, "pve-01")

	_, err := b.NodeForExisting(context.Background(), classifyVolid)
	if err == nil {
		t.Fatalf("expected an error when the storage was never classified")
	}
	if cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Fatalf("got DiskNotFound on an unclassified storage; err=%v", err)
	}
	if !strings.Contains(err.Error(), "could not be classified") {
		t.Fatalf("error does not say the storage went unclassified: %v", err)
	}
}

// TestLocalBackend_Classify_FabricatedInfoTakesTheLiveAnswer closes the loop on
// the fabricated shape: when the live index does carry the storage, the
// cache-miss backend classifies from it and the sweep proves the absence.
func TestLocalBackend_Classify_FabricatedInfoTakesTheLiveAnswer(t *testing.T) {
	t.Parallel()

	captured := StorageInfo{Name: classifyStorageName}
	index := newCountingStorageIndex(nil, `{"storage":"dir-images","type":"nfs"}`)
	b, _ := classifySweep(t, captured, index, nil, "pve-01")

	_, err := b.NodeForExisting(context.Background(), classifyVolid)
	if !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Fatalf("got %v, want DiskNotFound from the live nfs classification", err)
	}
}

// TestLocalBackend_Classify_PopulatedListingStillFindsTheVolume guards the
// ordinary hit: the classifier only decides what an empty listing means, and a
// listing that carries the volume answers the sweep before anything is
// classified at all.
func TestLocalBackend_Classify_PopulatedListingStillFindsTheVolume(t *testing.T) {
	t.Parallel()

	captured := StorageInfo{Name: classifyStorageName, Type: StorageTypeDir}
	index := newCountingStorageIndex(nil, `{"storage":"dir-images","type":"dir","is_mountpoint":"1"}`)
	b, _ := classifySweep(t, captured, index, []string{classifyVolid}, "pve-01", "pve-02")

	node, err := b.NodeForExisting(context.Background(), classifyVolid)
	if err != nil {
		t.Fatalf("NodeForExisting: %v", err)
	}
	if node != "pve-01" {
		t.Fatalf("got %q, want pve-01 (the first node whose listing carries the volume)", node)
	}
	if got := index.calls.Load(); got != 0 {
		t.Fatalf("live /storage reads = %d, want 0 (a listing that found the volume never classifies)", got)
	}
}

// ---------------------------------------------------------------------------
// LiveStorageInfo
// ---------------------------------------------------------------------------

// liveInfoClient serves a cluster storage service and nothing else, which is
// all LiveStorageInfo touches.
func liveInfoClient(index *countingStorageIndex) Client {
	c := &backendTestClient{}
	if index != nil {
		c.clusterStorageSvc = index.svc
	}
	return c
}

// TestLiveStorageInfo_ReturnsTheParsedEntry checks the helper decodes through
// the cache's own parser, so the fields the absence proof reads arrive
// populated rather than defaulted.
func TestLiveStorageInfo_ReturnsTheParsedEntry(t *testing.T) {
	t.Parallel()

	index := newCountingStorageIndex(nil,
		`{"storage":"other-store","type":"lvmthin"}`,
		`{"storage":"dir-images","type":"dir","is_mountpoint":"/mnt/pve/dir-images","nodes":"pve-01,pve-02"}`,
	)
	info, err := LiveStorageInfo(context.Background(), liveInfoClient(index), classifyStorageName)
	if err != nil {
		t.Fatalf("LiveStorageInfo: %v", err)
	}
	if info.Type != StorageTypeDir {
		t.Fatalf("type = %q, want dir", info.Type)
	}
	if !info.IsMountpoint {
		t.Fatalf("is_mountpoint did not survive the decode: %+v", info)
	}
	if len(info.Nodes) != 2 || info.Nodes[0] != "pve-01" || info.Nodes[1] != "pve-02" {
		t.Fatalf("nodes = %v, want [pve-01 pve-02]", info.Nodes)
	}
}

// TestLiveStorageInfo_MissingEntryErrors is the case a stale cache and a
// deleted storage both produce. An absent name is an error, never a zero
// StorageInfo a caller could mistake for a classification.
func TestLiveStorageInfo_MissingEntryErrors(t *testing.T) {
	t.Parallel()

	index := newCountingStorageIndex(nil, `{"storage":"other-store","type":"nfs"}`)
	info, err := LiveStorageInfo(context.Background(), liveInfoClient(index), classifyStorageName)
	if err == nil {
		t.Fatalf("expected an error for a storage absent from the index, got %+v", info)
	}
	if !strings.Contains(err.Error(), "not in the PVE storage index") {
		t.Fatalf("error does not name the miss: %v", err)
	}
}

// TestLiveStorageInfo_SkipsMalformedEntries keeps one unparseable row from
// taking the whole lookup down, matching the cache refresh's rule.
func TestLiveStorageInfo_SkipsMalformedEntries(t *testing.T) {
	t.Parallel()

	index := newCountingStorageIndex(nil,
		`{"storage":"broken","disable":"maybe"}`,
		`{"storage":"dir-images","type":"nfs"}`,
	)
	info, err := LiveStorageInfo(context.Background(), liveInfoClient(index), classifyStorageName)
	if err != nil {
		t.Fatalf("LiveStorageInfo: %v", err)
	}
	if info.Type != StorageTypeNFS {
		t.Fatalf("type = %q, want nfs", info.Type)
	}
}

// TestLiveStorageInfo_RejectsUnusableInputs covers every way the lookup cannot
// be made. Each one is an error rather than a silent zero value, because the
// backend's fallback decides what to do about it and cannot if it is not told.
func TestLiveStorageInfo_RejectsUnusableInputs(t *testing.T) {
	t.Parallel()

	index := newCountingStorageIndex(nil, `{"storage":"dir-images","type":"nfs"}`)
	cases := []struct {
		name   string
		ctx    context.Context //nolint:containedctx // a nil ctx is one of the inputs under test
		client Client
		arg    string
		want   string
	}{
		{"nil context", nil, liveInfoClient(index), classifyStorageName, "needs a context and a client"},
		{"nil client", context.Background(), nil, classifyStorageName, "needs a context and a client"},
		{"empty name", context.Background(), liveInfoClient(index), "  ", "needs a storage name"},
		{"no cluster storage service", context.Background(), liveInfoClient(nil), classifyStorageName,
			"needs the cluster storage service"},
		{"transport failure", context.Background(), liveInfoClient(newCountingStorageIndex(errors.New("boom"))),
			classifyStorageName, "live lookup of dir-images"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := LiveStorageInfo(tc.ctx, tc.client, tc.arg)
			if err == nil {
				t.Fatalf("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestLiveStorageInfo_NilIndexErrors covers the response that came back with
// no body at all, which is not an empty index and must not read as one.
func TestLiveStorageInfo_NilIndexErrors(t *testing.T) {
	t.Parallel()

	c := &backendTestClient{clusterStorageSvc: &fakeClusterStorageService{
		listFn: func(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
			return nil, nil
		},
	}}
	_, err := LiveStorageInfo(context.Background(), c, classifyStorageName)
	if err == nil {
		t.Fatalf("expected an error for a nil storage index")
	}
	if !strings.Contains(err.Error(), "nil storage index") {
		t.Fatalf("error does not name the nil index: %v", err)
	}
}
