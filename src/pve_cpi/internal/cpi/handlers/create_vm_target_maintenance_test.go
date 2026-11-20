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
// a later status read fail.
type maintenanceTargetCluster struct {
	*vmMockCluster
	tags      map[string]string
	lrmText   map[string]string
	failAfter int32
	reads     atomic.Int32
}

func (c *maintenanceTargetCluster) ListStatus(context.Context) (*sdkcluster.ListStatusResponse, error) {
	n := c.reads.Add(1)
	if c.failAfter > 0 && n > c.failAfter {
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
	resp := sdkcluster.ListHaStatusCurrentResponse{}
	for node, text := range c.lrmText {
		raw, _ := json.Marshal(map[string]any{"type": "lrm", "node": node, "status": node + " (" + text + ")"})
		resp = append(resp, raw)
	}
	return &resp, nil
}

func createOnPinnedNode(t *testing.T, c *maintenanceTargetCluster, target string) (error, int32) {
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
	args := mkArgs("agent-maintenance-target", testStemcellCID, map[string]any{"target_node": target},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})
	_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("maintenance-target"))
	return err, created.Load()
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
	err, created := createOnPinnedNode(t, c, "pve2")
	requireRetriableMaintenanceRefusal(t, err, created, "pve2")
}

func TestCreateVM_RefusesATargetNodeInTagMaintenance(t *testing.T) {
	t.Parallel()
	c := &maintenanceTargetCluster{tags: map[string]string{"pve2": "maintenance"}}
	err, created := createOnPinnedNode(t, c, "pve2")
	requireRetriableMaintenanceRefusal(t, err, created, "pve2")
}

func TestCreateVM_CreatesOnAHealthyTargetNode(t *testing.T) {
	t.Parallel()
	c := &maintenanceTargetCluster{
		tags:    map[string]string{"pve1": "maintenance"},
		lrmText: map[string]string{"pve1": "maintenance", "pve2": "active"},
	}
	err, created := createOnPinnedNode(t, c, "pve2")
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
	err, created := createOnPinnedNode(t, c, "pve2")
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
