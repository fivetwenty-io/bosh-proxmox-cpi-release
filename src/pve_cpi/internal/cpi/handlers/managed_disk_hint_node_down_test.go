package handlers

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	pveerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// hintNodeDownCtx disables retry backoff, so a lookup that does retry the
// down member fails fast instead of sleeping out the curve.
func hintNodeDownCtx() context.Context {
	return pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
}

// TestManagedDiskHintNode_DownNodeDoesNotBlockHintElsewhere: create_disk
// reads the hinted VM's node on every planning and recheck step. With another
// member offline, the read still answers the VM's node on the online member,
// and it never lists the offline one, so it neither stalls on the listing's
// retries nor fails create_disk.
func TestManagedDiskHintNode_DownNodeDoesNotBlockHintElsewhere(t *testing.T) {
	m, _, state := managedDiskFixture(t, "spread", false)
	state.configs = map[int]map[string]any{321: {"name": "vm-321"}}
	state.downNode = "n2"

	node, err := managedDiskHintNode(hintNodeDownCtx(), m.deps, "321")
	if err != nil {
		t.Fatalf("a down member must not fail the hint lookup for a VM elsewhere, got: %v", err)
	}
	if node != "n1" {
		t.Fatalf("expected the hinted VM's node n1, got %q", node)
	}
	if state.downLists != 0 {
		t.Fatalf("the member the cluster reports offline must not be listed, got %d listing attempts", state.downLists)
	}
}

// TestManagedDiskHintNode_HintOnDownNodeFailsRetriable: a hinted VM found on
// no online member may live on the offline one, where no disk can be created
// for it. The lookup lists that member once more with the strict retry
// budget, and when it still doesn't answer, the lookup fails retriable and
// names the member rather than reporting the VM as gone.
func TestManagedDiskHintNode_HintOnDownNodeFailsRetriable(t *testing.T) {
	m, _, state := managedDiskFixture(t, "spread", false)
	state.configs = map[int]map[string]any{}
	state.downNode = "n2"

	_, err := managedDiskHintNode(hintNodeDownCtx(), m.deps, "321")
	if err == nil {
		t.Fatal("a hinted VM that may be on the offline member must fail the lookup")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("the down-target error must be retriable, got: %v", err)
	}
	if !strings.Contains(err.Error(), "n2") || strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("the error must name the offline member and not call the VM gone, got: %v", err)
	}
	if strings.Contains(err.Error(), "no allocation submitted") {
		t.Fatalf("the lookup can't know whether an allocation was submitted, got: %v", err)
	}
	if state.downLists != pve.DefaultTransientMaxAttempts {
		t.Fatalf("the offline member must get one strict listing with the full budget, got %d listing attempts", state.downLists)
	}
}

// TestManagedDiskHintNode_HintOnBlippingNodeFoundAfterRelist: a corosync blip
// reports the hinted VM's node offline while the VM keeps running. Listing that
// node again with the strict retry budget rides out the blip and finds the VM.
func TestManagedDiskHintNode_HintOnBlippingNodeFoundAfterRelist(t *testing.T) {
	m, _, state := managedDiskFixture(t, "spread", false)
	state.configs = map[int]map[string]any{}
	state.downNode = "n2"
	state.downFailures = 2
	state.downGuests = []int{321}

	node, err := managedDiskHintNode(hintNodeDownCtx(), m.deps, "321")
	if err != nil {
		t.Fatalf("a blip the strict listing rides out must not fail the lookup, got: %v", err)
	}
	if node != "n2" {
		t.Fatalf("expected the hinted VM's node n2, got %q", node)
	}
	if state.downLists != 3 {
		t.Fatalf("the member must be listed again until it answers, got %d listing attempts, want 3", state.downLists)
	}
}

// TestManagedDiskHintNode_HintMissingAfterRelistStaysRetriable: when the
// left-out member answers its second listing and the hinted VM isn't there
// either, the lookup still doesn't call the VM gone, because PVE HA may have
// moved it between the two listings. It fails retriable instead.
func TestManagedDiskHintNode_HintMissingAfterRelistStaysRetriable(t *testing.T) {
	m, _, state := managedDiskFixture(t, "spread", false)
	state.configs = map[int]map[string]any{}
	state.downNode = "n2"
	state.downFailures = 1

	_, err := managedDiskHintNode(hintNodeDownCtx(), m.deps, "321")
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("want a retriable error that doesn't call the VM gone, got: %v", err)
	}
}

// TestManagedDiskHintNode_PartialFleetFailureStaysRetriable: a member the
// cluster still reports online that never answers its listing gets the full
// retry budget, and the lookup then fails with the listing's own retriable
// error, which names the member, rather than a permanent one.
func TestManagedDiskHintNode_PartialFleetFailureStaysRetriable(t *testing.T) {
	m, _, state := managedDiskFixture(t, "spread", false)
	state.configs = map[int]map[string]any{321: {"name": "vm-321"}}
	state.guestErr = pveerrors.ParseAPIError(595, []byte(`{"message":"no route to host","code":595}`))

	_, err := managedDiskHintNode(hintNodeDownCtx(), m.deps, "321")
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("a partial-fleet listing failure must stay retriable, got: %v", err)
	}
	if !strings.Contains(err.Error(), "n1") {
		t.Fatalf("the error must name the member that didn't answer, got: %v", err)
	}
}

// TestManagedDiskHintNode_MissingHintWithEveryMemberListed: with nothing left
// out of the scan, a hinted VM found nowhere still reads as gone.
func TestManagedDiskHintNode_MissingHintWithEveryMemberListed(t *testing.T) {
	m, _, state := managedDiskFixture(t, "spread", false)
	state.configs = map[int]map[string]any{}

	_, err := managedDiskHintNode(hintNodeDownCtx(), m.deps, "321")
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a hinted VM found on no member must read as gone, got: %v", err)
	}
	if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("a proven-missing hinted VM must stay permanent, got: %v", err)
	}
}

// TestHandleCreateDisk_HintOnDownNodeSubmitsNothing: create_disk with a vm_cid
// whose VM may be on the offline member fails retriable before any allocation.
// It creates no volume and writes no journal record, and the error says so.
func TestHandleCreateDisk_HintOnDownNodeSubmitsNothing(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.CPIConfig{Node: "n1", DiskStorage: "legacy-forbidden", DetachedDiskStrategy: "free", StoragePlacementNamespace: "namespace", StorageAllocationJournalDir: dir, PersistentStorageSet: "P", StorageSets: map[string]config.StorageSet{"P": {Names: []string{"a"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}}
	journal, err := aj.Initialize(t.Context(), dir, "namespace", aj.Enrollment{ClusterID: "pve-root-ca-sha256:" + strings.Repeat("ab", 32), AuthorityID: "authority", AuditID: "audit", CompleteHistoricalAudit: true, PreviousWriterFenced: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	})
	state := &managedDiskTestState{volumes: map[string]*nodes.GetStorageContentResponse{}, configs: map[int]map[string]any{}, downNode: "n2"}
	client := hintDownClient{&managedVMEntryClient{diagnosticVMClient: diagnosticVMClient{managedDiskTestPVE{state: state}}, journal: journal}}
	deps := Deps{Config: cfg, PVE: client, Logger: log.NewNopLogger()}

	_, err = HandleCreateDisk(deps).Handle(hintNodeDownCtx(), []json.RawMessage{json.RawMessage(`1024`), json.RawMessage(`{}`), json.RawMessage(`"321"`)}, jsonrpc.Context{})
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want a retriable error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "reports n2 offline") || !strings.Contains(err.Error(), "no allocation submitted") {
		t.Fatalf("the error must name the offline member and say nothing was submitted, got: %v", err)
	}
	if len(state.volumes) != 0 {
		t.Fatalf("no volume may be created, got %v", state.volumes)
	}
	records, err := journal.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("no journal record may be written, got %d", len(records))
	}
}

// hintDownClient is the create_disk entry fixture with a quorate cluster whose
// status reports the fixture's down member offline.
type hintDownClient struct{ *managedVMEntryClient }

func (c hintDownClient) Cluster() cluster.Service {
	return hintDownCluster{Service: c.managedVMEntryClient.Cluster(), down: c.state.downNode}
}

type hintDownCluster struct {
	cluster.Service
	down string
}

func (c hintDownCluster) ListStatus(ctx context.Context) (*cluster.ListStatusResponse, error) {
	r, err := c.Service.ListStatus(ctx)
	if err != nil {
		return nil, err
	}
	out := append(cluster.ListStatusResponse{json.RawMessage(`{"type":"cluster","quorate":1}`)}, *r...)
	out = append(out, json.RawMessage(`{"type":"node","name":"`+c.down+`","online":0}`))
	return &out, nil
}
