package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"testing"
)

type haMembershipCluster struct {
	cluster.Service
	rows    *cluster.ListHaResourcesResponse
	readErr error
	gets    int
}

func (c *haMembershipCluster) ListHaResources(context.Context, *cluster.ListHaResourcesParams) (*cluster.ListHaResourcesResponse, error) {
	return c.rows, c.readErr
}
func (c *haMembershipCluster) GetHaResources(context.Context, string) (*cluster.GetHaResourcesResponse, error) {
	c.gets++
	return nil, errors.New("no such resource (HTTP500)")
}

type haMembershipClient struct {
	pve.Client
	c *haMembershipCluster
}

func (c *haMembershipClient) Cluster() cluster.Service { return c.c }
func TestManagedHAAbsentSkipsAmbiguousExactGet(t *testing.T) {
	deps, j, base, r := deleteManagedFixture(t)
	rows := cluster.ListHaResourcesResponse{json.RawMessage(`{"sid":"vm:999"}`)}
	c := &haMembershipCluster{Service: base.Cluster(), rows: &rows}
	deps.PVE = &haMembershipClient{Client: base, c: c}
	h, err := j.Acquire(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := managedVMDeleteHA(t.Context(), deps, h, "pve1", 123); err != nil {
		t.Fatal(err)
	}
	if c.gets != 0 {
		t.Fatal("absence used ambiguous exact GET")
	}
}
func TestManagedHAMembershipRequiresCompleteValidList(t *testing.T) {
	for _, body := range []string{"null", `[null]`, `[{"sid":""}]`, `[{"sid":"vm:123"},{"sid":"vm:123"}]`, `[{"sid":"vm:123"},{"sid":"malformed"}]`, `{"sid":"vm:123"}`} {
		t.Run(body, func(t *testing.T) {
			var rows cluster.ListHaResourcesResponse
			if err := json.Unmarshal([]byte(body), &rows); err != nil {
				rows = nil
			}
			c := &haMembershipCluster{rows: &rows}
			deps := Deps{PVE: &haMembershipClient{c: c}}
			if _, err := managedVMHAResourcePresent(t.Context(), deps, "vm:123"); err == nil {
				t.Fatal("invalid membership accepted")
			}
		})
	}
}
func TestCleanupDiagnosticDoesNotExposeSource(t *testing.T) {
	err := storageCleanupFailure("vm_ha", errors.New("password=private-backend-response"))
	if got := StorageAllocationDecisionFailure(storageDecisionSourceError(err)); got != "cleanup_vm_ha" {
		t.Fatalf("classification %s", got)
	}
	if got := StorageAllocationDecisionFailure(errors.New("password=private")); got != "identity_or_audit_evidence" {
		t.Fatalf("unclassified %s", got)
	}
}

func TestManagedHARulesRejectMalformedNonmatchingRows(t *testing.T) {
	cases := []map[string]any{
		{"rule": "other"},
		{"rule": "other", "resources": nil},
		{"rule": "other", "resources": 42},
		{"rule": "other", "resources": ""},
		{"rule": "other", "resources": "vm:999,"},
		{"rule": "other", "resources": "vm:999,vm:999"},
		{"resources": "vm:999"},
	}
	for _, row := range cases {
		service := &infrastructureHACluster{rows: []map[string]any{row}}
		deps := Deps{PVE: &infrastructureHAClient{ha: service}}
		if _, err := managedVMHARulesForResource(t.Context(), deps, 123); err == nil {
			t.Fatalf("malformed row proved absent membership: %#v", row)
		}
		if _, err := readManagedVMHARule(t.Context(), deps, "wanted"); err == nil {
			t.Fatalf("malformed row proved absent rule: %#v", row)
		}
	}
	service := &infrastructureHACluster{rows: []map[string]any{{"rule": "other", "resources": "vm:999"}, {"rule": "other", "resources": "ct:998"}}}
	if _, err := managedVMHARulesForResource(t.Context(), Deps{PVE: &infrastructureHAClient{ha: service}}, 123); err == nil {
		t.Fatal("duplicate nonmatching rule accepted")
	}
}
