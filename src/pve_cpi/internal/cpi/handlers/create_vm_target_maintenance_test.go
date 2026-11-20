package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// maintenanceTargetCluster wraps the shared cluster mock so a test can script
// the node tags in cluster status and the lrm text of status/current, and make
// a later status read, or the reads failOn names by their order, fail.
type maintenanceTargetCluster struct {
	*vmMockCluster
	tags      map[string]string
	lrmText   map[string]string
	failAfter int32
	failOn    map[int32]bool
	reads     atomic.Int32
}

func (c *maintenanceTargetCluster) ListStatus(context.Context) (*sdkcluster.ListStatusResponse, error) {
	n := c.reads.Add(1)
	if c.failAfter > 0 && n > c.failAfter || c.failOn[n] {
		return nil, errors.New("cluster status unavailable")
	}
	rows := sdkcluster.ListStatusResponse{}
	for _, row := range []map[string]any{
		{"type": "cluster", "name": "lab", "quorate": 1, "nodes": 2},
		{"type": "node", "name": "pve1", "online": 1, "local": 1, "tags": c.tags["pve1"]},
		{"type": "node", "name": "pve2", "online": 1, "tags": c.tags["pve2"]},
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
	var created atomic.Int32
	q := &vmMockQEMU{
		createFn: func(context.Context, string, map[string]any) (string, error) {
			created.Add(1)
			return "UPID:pve1:ok", nil
		},
	}
	deps := buildVMDepsPlacement(q, &vmMockNodes{}, nil, emptyListResources, &vmMockAgent{},
		func(cfg *config.CPIConfig) { cfg.Node = "pve1" })
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

func TestCreateVM_RefusesATargetNodeInTagMaintenance(t *testing.T) {
	t.Parallel()
	c := &maintenanceTargetCluster{tags: map[string]string{"pve2": "maintenance"}}
	created, err := createOnPinnedNode(t, c)
	requireRetriableMaintenanceRefusal(t, err, created, "pve2")
}

func TestCreateVM_CreatesOnAHealthyTargetNode(t *testing.T) {
	t.Parallel()
	c := &maintenanceTargetCluster{
		tags:    map[string]string{"pve1": "maintenance"},
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
	var created atomic.Int32
	q := &vmMockQEMU{
		createFn: func(context.Context, string, map[string]any) (string, error) {
			created.Add(1)
			return "UPID:pve1:ok", nil
		},
	}
	deps := buildVMDepsPlacement(q, &vmMockNodes{}, nil, emptyListResources, &vmMockAgent{},
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
	return created.Load(), err
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
// fail-open side. With HA writes off and no maintenance tags named, a health
// read that fails twice leaves placement treating every node as healthy, as
// it did before, and the create goes ahead.
func TestCreateVM_ScoredPlacementFailsOpenWhenNodeHealthIsNotRequired(t *testing.T) {
	t.Parallel()
	c := &maintenanceTargetCluster{failOn: healthReadsFail}
	created, err := createOnScoredNode(t, c, nil)
	if err != nil {
		t.Fatalf("create_vm failed although node health is not required: %v", err)
	}
	if created != 1 {
		t.Fatalf("create_vm created %d VMs, want 1", created)
	}
}
