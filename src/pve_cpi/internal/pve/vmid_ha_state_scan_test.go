package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// haListsCluster answers the HA resource and rule lists from fixed rows, or
// fails them with err, and counts the reads.
type haListsCluster struct {
	sdkcluster.Service
	resources []map[string]any
	rules     []map[string]any
	err       error
	// resourcesErr and rulesErr fail one list alone.
	resourcesErr error
	rulesErr     error

	mu         sync.Mutex
	reads      int
	rulesReads int
}

func (c *haListsCluster) ListHaResources(
	context.Context, *sdkcluster.ListHaResourcesParams,
) (*sdkcluster.ListHaResourcesResponse, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if c.resourcesErr != nil {
		return nil, c.resourcesErr
	}
	out := make(sdkcluster.ListHaResourcesResponse, 0, len(c.resources))
	for _, row := range c.resources {
		raw, _ := json.Marshal(row)
		out = append(out, raw)
	}
	return &out, nil
}

func (c *haListsCluster) ListHaRules(
	context.Context, *sdkcluster.ListHaRulesParams,
) (*sdkcluster.ListHaRulesResponse, error) {
	c.mu.Lock()
	c.reads++
	c.rulesReads++
	c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if c.rulesErr != nil {
		return nil, c.rulesErr
	}
	out := make(sdkcluster.ListHaRulesResponse, 0, len(c.rules))
	for _, row := range c.rules {
		raw, _ := json.Marshal(row)
		out = append(out, raw)
	}
	return &out, nil
}

func (c *haListsCluster) readCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

func (c *haListsCluster) rulesReadCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rulesReads
}

// haScanClient serves the cluster API through an haListsCluster wrapped
// around the base client's own cluster service.
type haScanClient struct {
	pve.Client
	lists *haListsCluster
}

func (c *haScanClient) Cluster() sdkcluster.Service {
	c.lists.Service = c.Client.Cluster()
	return c.lists
}

func newHAScanVMIDClient(lists *haListsCluster) *haScanClient {
	base := newVMIDClient(func(context.Context, *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return buildResources(), nil
	})
	return &haScanClient{Client: base, lists: lists}
}

// TestNextVMID_HAStateScan_SkipsVMIDsTheHAStateNames proves the HA state scan
// keeps out of the allocation every VMID an HA resource names, every VMID a
// rule names as a resource, and every VMID a pin's name carries, whether the
// rule lists its resources as a string or as an array.
func TestNextVMID_HAStateScan_SkipsVMIDsTheHAStateNames(t *testing.T) {
	t.Parallel()
	lists := &haListsCluster{
		resources: []map[string]any{{"sid": "vm:200", "state": "stopped"}, {"sid": "ct:201"}},
		rules: []map[string]any{
			{"rule": "keep-apart", "type": "resource-affinity", "resources": "vm:202,ct:203"},
			{"rule": "pin-array", "type": "node-affinity", "resources": []string{"vm:204"}},
			{"rule": "bosh-na-205", "type": "node-affinity", "resources": ""},
		},
	}
	c := newHAScanVMIDClient(lists)
	id, err := pve.NextVMID(context.Background(), c, pve.WithRange(200, 206), pve.WithHAStateScan(pve.HAStateScanRequiringGrant))
	if err != nil {
		t.Fatalf("NextVMID: %v", err)
	}
	if id != 206 {
		t.Fatalf("NextVMID = %d, want 206, the one VMID the HA state does not name", id)
	}
}

// TestNextVMID_HAStateScan_ExhaustsTheRangeTheHAStateFills proves a range
// whose every VMID the HA state names allocates nothing.
func TestNextVMID_HAStateScan_ExhaustsTheRangeTheHAStateFills(t *testing.T) {
	t.Parallel()
	lists := &haListsCluster{
		resources: []map[string]any{{"sid": "vm:200"}},
		rules:     []map[string]any{{"rule": "bosh-na-201", "type": "node-affinity", "resources": "vm:201"}},
	}
	c := newHAScanVMIDClient(lists)
	if id, err := pve.NextVMID(context.Background(), c, pve.WithRange(200, 201), pve.WithHAStateScan(pve.HAStateScanRequiringGrant)); err == nil {
		t.Fatalf("NextVMID = %d, want an exhausted range", id)
	}
}

// TestNextVMID_HAStateScan_FailsClosed proves a failed HA read fails the
// allocation instead of allocating blind to the HA state.
func TestNextVMID_HAStateScan_FailsClosed(t *testing.T) {
	t.Parallel()
	lists := &haListsCluster{err: errors.New("connection reset by peer")}
	c := newHAScanVMIDClient(lists)
	if id, err := pve.NextVMID(context.Background(), c, pve.WithRange(200, 210), pve.WithHAStateScan(pve.HAStateScanRequiringGrant)); err == nil {
		t.Fatalf("NextVMID = %d, want the HA read's failure", id)
	}
}

// TestNextVMID_WithoutHAStateScanReadsNoHAState proves an allocation that
// does not ask for the scan reads no HA state.
func TestNextVMID_WithoutHAStateScanReadsNoHAState(t *testing.T) {
	t.Parallel()
	lists := &haListsCluster{resources: []map[string]any{{"sid": "vm:200"}}}
	c := newHAScanVMIDClient(lists)
	if _, err := pve.NextVMID(context.Background(), c, pve.WithRange(200, 200)); err != nil {
		t.Fatalf("NextVMID: %v", err)
	}
	if n := lists.readCount(); n != 0 {
		t.Fatalf("HA reads = %d, want none", n)
	}
}

// haScanStatus builds the error the SDK returns for an HTTP status.
func haScanStatus(code int, body string) error {
	return sdkerrors.ParseAPIError(code, []byte(body))
}

// haScanRetriable reports whether err tells the Director to retry.
func haScanRetriable(err error) bool {
	var cerr *cpierrors.Error
	return errors.As(err, &cerr) && cerr.OkToRetry()
}

// noBackoff makes the scan's transient retries run without waiting.
func noBackoff() context.Context {
	return pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
}

// TestNextVMID_HAStateScan_AnswersADeniedReadByPolicy proves a refusal for a
// missing grant allocates without the HA state under the best-effort and the
// denial-tolerating policies, and fails the allocation with a non-retriable
// error that names Sys.Audit on / under the policy that requires the grant.
func TestNextVMID_HAStateScan_AnswersADeniedReadByPolicy(t *testing.T) {
	t.Parallel()
	forbidden := haScanStatus(403, "Permission check failed (/, Sys.Audit)")
	for _, policy := range []pve.HAStateScanPolicy{
		pve.HAStateScanBestEffort, pve.HAStateScanToleratingDenial, pve.HAStateScanRequiringGrant,
	} {
		lists := &haListsCluster{err: forbidden}
		c := newHAScanVMIDClient(lists)
		id, err := pve.NextVMID(noBackoff(), c, pve.WithRange(200, 210), pve.WithHAStateScan(policy))
		if policy != pve.HAStateScanRequiringGrant {
			if err != nil {
				t.Fatalf("policy %v: a denied HA read must leave the allocation going: %v", policy, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("policy %v: NextVMID = %d, want the denial", policy, id)
		}
		if haScanRetriable(err) || !strings.Contains(err.Error(), "Sys.Audit on /") {
			t.Fatalf("policy %v: want a non-retriable error naming Sys.Audit on /, got %v", policy, err)
		}
	}
}

// TestNextVMID_HAStateScan_ScansTheResourcesWithoutTheRulesEndpoint proves a
// rules endpoint PVE does not implement, as on PVE 8, leaves the resources
// scanned under every policy, and that the scan reads it once rather than
// retrying a 501 as a server fault.
func TestNextVMID_HAStateScan_ScansTheResourcesWithoutTheRulesEndpoint(t *testing.T) {
	t.Parallel()
	for _, policy := range []pve.HAStateScanPolicy{
		pve.HAStateScanBestEffort, pve.HAStateScanToleratingDenial, pve.HAStateScanRequiringGrant,
	} {
		lists := &haListsCluster{
			resources: []map[string]any{{"sid": "vm:200"}},
			rulesErr:  haScanStatus(501, "Method 'GET /cluster/ha/rules' not implemented"),
		}
		c := newHAScanVMIDClient(lists)
		id, err := pve.NextVMID(noBackoff(), c, pve.WithRange(200, 201), pve.WithHAStateScan(policy))
		if err != nil {
			t.Fatalf("policy %v: NextVMID: %v", policy, err)
		}
		if id != 201 {
			t.Fatalf("policy %v: NextVMID = %d, want 201, the VMID no HA resource names", policy, id)
		}
		if n := lists.rulesReadCount(); n != 1 {
			t.Fatalf("policy %v: rules reads = %d, want 1", policy, n)
		}
	}
}

// TestNextVMID_HAStateScan_AnswersAServerErrorByPolicy proves a server error
// that outlasts the scan's retries fails the allocation retriably under the
// policies auto and managed mode use, and allocates without the HA state
// under the best-effort policy off mode uses.
func TestNextVMID_HAStateScan_AnswersAServerErrorByPolicy(t *testing.T) {
	t.Parallel()
	for _, policy := range []pve.HAStateScanPolicy{
		pve.HAStateScanBestEffort, pve.HAStateScanToleratingDenial, pve.HAStateScanRequiringGrant,
	} {
		lists := &haListsCluster{err: haScanStatus(503, "service unavailable")}
		c := newHAScanVMIDClient(lists)
		id, err := pve.NextVMID(noBackoff(), c, pve.WithRange(200, 210), pve.WithHAStateScan(policy))
		if policy == pve.HAStateScanBestEffort {
			if err != nil {
				t.Fatalf("best effort: a failed HA read must leave the allocation going: %v", err)
			}
			continue
		}
		if err == nil || !haScanRetriable(err) {
			t.Fatalf("policy %v: NextVMID = %d, want a retriable error, got %v", policy, id, err)
		}
	}
}

// TestNextVMID_HAStateScan_WarnsOnceAboutADeniedRead proves the warning for
// an allocation that went on without the HA state is logged once per process,
// so a call that allocates several parkers does not repeat it.
func TestNextVMID_HAStateScan_WarnsOnceAboutADeniedRead(t *testing.T) {
	pve.ResetHAStateScanWarnings()
	t.Cleanup(pve.ResetHAStateScanWarnings)
	logger, observer := log.NewObservedLogger(log.LevelWarn)
	ctx := log.IntoContext(noBackoff(), logger)
	lists := &haListsCluster{err: haScanStatus(403, "Permission check failed (/, Sys.Audit)")}
	c := newHAScanVMIDClient(lists)
	for range 3 {
		if _, err := pve.NextVMID(ctx, c, pve.WithRange(200, 210),
			pve.WithHAStateScan(pve.HAStateScanToleratingDenial)); err != nil {
			t.Fatalf("NextVMID: %v", err)
		}
	}
	warned := 0
	for _, entry := range observer.All() {
		if strings.Contains(entry.Message, "Sys.Audit on /") {
			warned++
		}
	}
	if warned != 1 {
		t.Fatalf("warnings naming Sys.Audit on / = %d, want 1; entries %+v", warned, observer.All())
	}
}
