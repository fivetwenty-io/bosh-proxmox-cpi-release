package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// maintenanceTargetCluster wraps the shared cluster mock so a test can script
// the lrm text of status/current and each node's notes, and make a later
// status read, or the reads failOn names by their order, fail. The notes are
// answered through the node config read of the nodes service that nodes
// returns.
type maintenanceTargetCluster struct {
	*vmMockCluster
	notes     map[string]string
	lrmText   map[string]string
	failAfter int32
	failOn    map[int32]bool
	reads     atomic.Int32

	// notesFailures makes the first n notes reads of a node fail, and a
	// negative n makes every read of it fail.
	notesFailures map[string]int

	mu         sync.Mutex
	notesReads map[string]int
}

// nodes returns a nodes service that answers the node config read from the
// scripted notes and counts each node's reads.
func (c *maintenanceTargetCluster) nodes() *vmMockNodes {
	return &vmMockNodes{listConfigFn: func(_ context.Context, node string) (*sdknodes.ListConfigResponse, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.notesReads == nil {
			c.notesReads = map[string]int{}
		}
		c.notesReads[node]++
		if n := c.notesFailures[node]; n < 0 || c.notesReads[node] <= n {
			return nil, errors.New("node config unavailable")
		}
		resp := &sdknodes.ListConfigResponse{}
		if notes, ok := c.notes[node]; ok {
			resp.Description = &notes
		}
		return resp, nil
	}}
}

// readsOf returns how many times node's notes were read.
func (c *maintenanceTargetCluster) readsOf(node string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.notesReads[node]
}

func (c *maintenanceTargetCluster) ListStatus(context.Context) (*sdkcluster.ListStatusResponse, error) {
	n := c.reads.Add(1)
	if c.failAfter > 0 && n > c.failAfter || c.failOn[n] {
		return nil, errors.New("cluster status unavailable")
	}
	rows := sdkcluster.ListStatusResponse{}
	for _, row := range []map[string]any{
		{"type": "cluster", "name": "lab", "quorate": 1, "nodes": 2},
		{"type": "node", "name": "pve1", "online": 1, "local": 1},
		{"type": "node", "name": "pve2", "online": 1},
	} {
		raw, _ := json.Marshal(row)
		rows = append(rows, raw)
	}
	return &rows, nil
}

func (c *maintenanceTargetCluster) ListHaStatusCurrent(context.Context) (*sdkcluster.ListHaStatusCurrentResponse, error) {
	resp := make(sdkcluster.ListHaStatusCurrentResponse, 0, len(c.lrmText))
	for node, text := range c.lrmText {
		raw, _ := json.Marshal(map[string]any{"type": "lrm", "node": node, "status": node + " (" + text + ")"})
		resp = append(resp, raw)
	}
	return &resp, nil
}

// pinnedNode is the node every test in this file pins.
const pinnedNode = "pve2"

func createOnPinnedNode(t *testing.T, c *maintenanceTargetCluster) (int32, error) {
	t.Helper()
	return createOnPinnedNodeWith(t, c, nil)
}

// createOnPinnedNodeWith runs create_vm pinned to pinnedNode against c and a
// config that mutate edits.
func createOnPinnedNodeWith(t *testing.T, c *maintenanceTargetCluster, mutate func(*config.CPIConfig)) (int32, error) {
	t.Helper()
	var created atomic.Int32
	q := &vmMockQEMU{
		createFn: func(context.Context, string, map[string]any) (string, error) {
			created.Add(1)
			return "UPID:pve1:ok", nil
		},
	}
	deps := buildVMDepsPlacement(q, c.nodes(), nil, emptyListResources, &vmMockAgent{},
		func(cfg *config.CPIConfig) {
			cfg.Node = "pve1"
			if mutate != nil {
				mutate(cfg)
			}
		})
	c.vmMockCluster = &vmMockCluster{listResourcesFn: emptyListResources}
	deps.PVE.(*mockPVEClient).clusterSvc = c
	deps.Config.EnsureNoIPConflicts = placementDisabled
	args := mkArgs("agent-maintenance-target", testStemcellCID, map[string]any{"target_node": pinnedNode},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})
	_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("maintenance-target"))
	return created.Load(), err
}

func requireRetriableMaintenanceRefusal(t *testing.T, err error, created int32, node string) {
	t.Helper()
	if err == nil {
		t.Fatal("create_vm succeeded on a target_node in maintenance")
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("want a retriable error, got %T %v", err, err)
	}
	for _, want := range []string{`"` + node + `"`, "in maintenance", "retry once the node leaves maintenance"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if created != 0 {
		t.Fatal("create_vm created a VM on a node in maintenance")
	}
}

func TestCreateVM_RefusesATargetNodeInHAMaintenance(t *testing.T) {
	t.Parallel()
	c := &maintenanceTargetCluster{lrmText: map[string]string{"pve1": "active", "pve2": "maintenance"}}
	created, err := createOnPinnedNode(t, c)
	requireRetriableMaintenanceRefusal(t, err, created, "pve2")
}

// TestCreateVM_RefusesATargetNodeMarkedInItsNotes pins the refusal of a pinned
// node whose notes carry the maintenance marker, with the default
// maintenance_node_tags and no HA maintenance anywhere.
func TestCreateVM_RefusesATargetNodeMarkedInItsNotes(t *testing.T) {
	t.Parallel()
	c := &maintenanceTargetCluster{notes: map[string]string{"pve2": "Rack 4, row B\nTags: Maintenance"}}
	created, err := createOnPinnedNode(t, c)
	requireRetriableMaintenanceRefusal(t, err, created, "pve2")
	if !strings.Contains(err.Error(), `its notes carry the maintenance tag "Maintenance"`) {
		t.Fatalf("error %q does not say the node's notes carry the tag", err)
	}
}

// TestCreateVM_RefusesATargetNodeWhoseNotesCannotBeRead pins the fail-closed
// side of the pinned path. When the pinned node's notes cannot be read, the
// CPI cannot tell whether the node is marked for maintenance, so the create
// is refused with a retriable error and no clone.
func TestCreateVM_RefusesATargetNodeWhoseNotesCannotBeRead(t *testing.T) {
	t.Parallel()
	c := &maintenanceTargetCluster{notesFailures: map[string]int{"pve2": -1}}
	created, err := createOnPinnedNode(t, c)
	if err == nil {
		t.Fatal("create_vm succeeded although the target_node's notes were unreadable")
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("want a retriable error, got %T %v", err, err)
	}
	for _, want := range []string{`"pve2"`, "cannot tell whether", "node config unavailable", "retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if created != 0 {
		t.Fatal("create_vm created a VM although the target_node's notes were unreadable")
	}
}

// TestCreateVM_IgnoresNotesWithoutMaintenanceTags pins that an explicit empty
// maintenance_node_tags turns the marker off, so a pinned node whose notes
// carry it takes the VM and no notes are read.
func TestCreateVM_IgnoresNotesWithoutMaintenanceTags(t *testing.T) {
	t.Parallel()
	c := &maintenanceTargetCluster{notes: map[string]string{"pve2": "tags: maintenance"}}
	created, err := createOnPinnedNodeWith(t, c, func(cfg *config.CPIConfig) {
		cfg.Placement = &config.PlacementConfig{MaintenanceNodeTags: []string{}}
	})
	if err != nil {
		t.Fatalf("a marked target_node was refused with no maintenance tags configured: %v", err)
	}
	if created != 1 {
		t.Fatalf("create_vm created %d VMs, want 1", created)
	}
	if n := c.readsOf("pve2"); n != 0 {
		t.Fatalf("pve2's notes were read %d times with no maintenance tags configured, want 0", n)
	}
}

func TestCreateVM_CreatesOnAHealthyTargetNode(t *testing.T) {
	t.Parallel()
	c := &maintenanceTargetCluster{
		notes:   map[string]string{"pve1": "tags: maintenance", "pve2": "Rack 4\nmaintenance window on Fridays"},
		lrmText: map[string]string{"pve1": "maintenance", "pve2": "active"},
	}
	created, err := createOnPinnedNode(t, c)
	if err != nil {
		t.Fatalf("a healthy target_node was refused: %v", err)
	}
	if created != 1 {
		t.Fatalf("create_vm created %d VMs, want 1", created)
	}
}

func TestCreateVM_RefusesATargetNodeWhenTheHealthReadFails(t *testing.T) {
	t.Parallel()
	c := &maintenanceTargetCluster{failAfter: 1}
	created, err := createOnPinnedNode(t, c)
	if err == nil {
		t.Fatal("create_vm succeeded although the node health read failed")
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("want a retriable error, got %T %v", err, err)
	}
	for _, want := range []string{`"pve2"`, "cannot tell whether", "retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if created != 0 {
		t.Fatal("create_vm created a VM although node health was unreadable")
	}
}

// TestCreateVM_RefusesATargetNodeThatCannotTakeWork pins the refusal of a
// pinned node that health calls not placeable although it is not in
// maintenance, here because its LRM reports an old timestamp. The error must
// say the node cannot take a new VM and why, not that it is in maintenance,
// and it must be retriable with no clone.
func TestCreateVM_RefusesATargetNodeThatCannotTakeWork(t *testing.T) {
	t.Parallel()
	c := &maintenanceTargetCluster{lrmText: map[string]string{"pve1": "active", "pve2": "old timestamp - dead?"}}
	created, err := createOnPinnedNode(t, c)
	if err == nil {
		t.Fatal("create_vm succeeded on a target_node that cannot take work")
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("want a retriable error, got %T %v", err, err)
	}
	for _, want := range []string{`"pve2"`, "cannot take a new VM", "lrm status text reports an old timestamp",
		"retry once the node can take new work"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "in maintenance") {
		t.Fatalf("error %q calls a node that is not in maintenance in maintenance", err)
	}
	if created != 0 {
		t.Fatal("create_vm created a VM on a node that cannot take work")
	}
}

// healthReadsFail fails the third and fourth cluster status reads of a
// scored create. The create reads cluster status once before placement, and
// the gather reads the node list before node health, so these are the node
// health read and its retry.
var healthReadsFail = map[int32]bool{3: true, 4: true}

// createOnScoredNode runs create_vm with no target_node, so the scored
// placement path picks the node, against c and a config that mutate edits.
func createOnScoredNode(t *testing.T, c *maintenanceTargetCluster, mutate func(*config.CPIConfig)) (int32, error) {
	t.Helper()
	created, _, err := createOnScoredNodeAt(t, c, mutate)
	return created, err
}

// createOnScoredNodeAt is createOnScoredNode that also returns the node the
// VM was created on, or "" when none was.
func createOnScoredNodeAt(t *testing.T, c *maintenanceTargetCluster, mutate func(*config.CPIConfig)) (int32, string, error) {
	t.Helper()
	var (
		created atomic.Int32
		mu      sync.Mutex
		on      string
	)
	q := &vmMockQEMU{
		createFn: func(_ context.Context, node string, _ map[string]any) (string, error) {
			created.Add(1)
			mu.Lock()
			on = node
			mu.Unlock()
			return "UPID:" + node + ":ok", nil
		},
	}
	deps := buildVMDepsPlacement(q, c.nodes(), nil, emptyListResources, &vmMockAgent{},
		func(cfg *config.CPIConfig) {
			cfg.Node = "pve1"
			if mutate != nil {
				mutate(cfg)
			}
		})
	c.vmMockCluster = &vmMockCluster{listResourcesFn: emptyListResources}
	deps.PVE.(*mockPVEClient).clusterSvc = c
	deps.Config.EnsureNoIPConflicts = placementDisabled
	args := mkArgs("agent-scored-health", testStemcellCID, map[string]any{},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})
	_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("scored-health"))
	mu.Lock()
	defer mu.Unlock()
	return created.Load(), on, err
}

// TestCreateVM_ScoredPlacementRefusesWhenNodeHealthStaysUnreadable pins the
// fail-closed side of scored placement. When the operator names maintenance
// tags, placement relies on node health to keep new VMs off a node in
// maintenance, so a health read that fails twice must stop the create with a
// retriable error and no clone.
func TestCreateVM_ScoredPlacementRefusesWhenNodeHealthStaysUnreadable(t *testing.T) {
	t.Parallel()
	c := &maintenanceTargetCluster{failOn: healthReadsFail}
	created, err := createOnScoredNode(t, c, func(cfg *config.CPIConfig) {
		cfg.Placement = &config.PlacementConfig{MaintenanceNodeTags: []string{"maintenance"}}
	})
	if err == nil {
		t.Fatal("create_vm placed a VM although node health was unreadable")
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("want a retriable error, got %T %v", err, err)
	}
	for _, want := range []string{"cannot tell which nodes are in maintenance", "failed twice", "retry once the cluster answers"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if created != 0 {
		t.Fatal("create_vm created a VM although node health was unreadable")
	}
}

// TestCreateVM_ScoredPlacementFailsOpenWhenNodeHealthIsNotRequired pins the
// fail-open side. With HA writes off and maintenance_node_tags set to an
// explicit [], a health read that fails twice leaves placement treating every node as healthy, as
// it did before, and the create goes ahead.
func TestCreateVM_ScoredPlacementFailsOpenWhenNodeHealthIsNotRequired(t *testing.T) {
	t.Parallel()
	c := &maintenanceTargetCluster{failOn: healthReadsFail}
	created, err := createOnScoredNode(t, c, func(cfg *config.CPIConfig) {
		cfg.Placement = &config.PlacementConfig{MaintenanceNodeTags: []string{}}
	})
	if err != nil {
		t.Fatalf("create_vm failed although node health is not required: %v", err)
	}
	if created != 1 {
		t.Fatalf("create_vm created %d VMs, want 1", created)
	}
}

// TestCreateVM_ScoredPlacementLeavesOutANodeMarkedInItsNotes pins that scored
// placement never picks a node whose notes carry the maintenance marker, with
// the default maintenance_node_tags, whichever node is marked.
func TestCreateVM_ScoredPlacementLeavesOutANodeMarkedInItsNotes(t *testing.T) {
	t.Parallel()
	for marked, want := range map[string]string{"pve1": "pve2", "pve2": "pve1"} {
		t.Run(marked, func(t *testing.T) {
			t.Parallel()
			c := &maintenanceTargetCluster{notes: map[string]string{marked: "Owner: platform team\n  tags: drain, maintenance  "}}
			created, on, err := createOnScoredNodeAt(t, c, nil)
			if err != nil {
				t.Fatalf("create_vm failed with one node left to place on: %v", err)
			}
			if created != 1 || on != want {
				t.Fatalf("create_vm created %d VMs on %q, want 1 on %q", created, on, want)
			}
		})
	}
}

// TestCreateVM_ScoredPlacementRefusesWhenNotesStayUnreadable pins that a notes
// read that fails on the health read and again on its retry counts as a
// failed node health read, so the create stops with the retriable error and
// no clone.
func TestCreateVM_ScoredPlacementRefusesWhenNotesStayUnreadable(t *testing.T) {
	t.Parallel()
	c := &maintenanceTargetCluster{notesFailures: map[string]int{"pve2": -1}}
	created, err := createOnScoredNode(t, c, nil)
	if err == nil {
		t.Fatal("create_vm placed a VM although a node's notes stayed unreadable")
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("want a retriable error, got %T %v", err, err)
	}
	for _, want := range []string{"cannot tell which nodes are in maintenance", "failed twice", "node config unavailable"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if created != 0 {
		t.Fatal("create_vm created a VM although a node's notes stayed unreadable")
	}
}

// TestCreateVM_ReadsEachNodesNotesOncePerCall pins that one create_vm reads
// each node's notes once, however many node health reads it makes, and that a
// failed read is asked again on the retry. pve2's first read fails, so the
// health read is retried, pve1's answer comes from the first read, and pve2
// is read a second time and found marked.
func TestCreateVM_ReadsEachNodesNotesOncePerCall(t *testing.T) {
	t.Parallel()
	c := &maintenanceTargetCluster{
		notes:         map[string]string{"pve2": "tags: maintenance"},
		notesFailures: map[string]int{"pve2": 1},
	}
	created, on, err := createOnScoredNodeAt(t, c, nil)
	if err != nil {
		t.Fatalf("create_vm failed although the notes read succeeded on the retry: %v", err)
	}
	if created != 1 || on != "pve1" {
		t.Fatalf("create_vm created %d VMs on %q, want 1 on pve1", created, on)
	}
	if n := c.readsOf("pve1"); n != 1 {
		t.Fatalf("pve1's notes were read %d times in one create_vm, want 1", n)
	}
	if n := c.readsOf("pve2"); n != 2 {
		t.Fatalf("pve2's notes were read %d times, want 2 (the failed read and its retry)", n)
	}
}
