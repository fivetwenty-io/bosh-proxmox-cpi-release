package handlers_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// TestHandleSetVMMetadata_MixedCaseGroupKeepsIdentityTag reproduces a VM whose
// instance group is "rM". PVE stores the name tag as "rm--<id>", so the sync
// has to treat the new "rM--<id>" as the same entry. The stored list is close
// to the cap, and a duplicate would push the vm-prefix identity tag off it.
func TestHandleSetVMMetadata_MixedCaseGroupKeepsIdentityTag(t *testing.T) {
	t.Parallel()

	const id = "1de28840-4a9e-4d4f-a421-50270360faac"
	stored := strings.Join([]string{
		"bosh-cpi",
		"bosh-owner--a1d3f809-511b-4ed5-a41f-ef891aa8b4a9",
		"deployment--p6-rm",
		"director--" + strings.Repeat("d", 40),
		"index--0",
		"instance-group--rm",
		"job--rm",
		"rm--" + id,
		"vm-prefix--halab-b",
	}, ";")

	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"tags": stored}, nil
		},
	}
	var gotTags string
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _, _ string, params *nodes.UpdateQemuConfigParams) error {
			if params.Tags != nil {
				gotTags = *params.Tags
			}
			return nil
		},
	}

	h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, qemuSvc, nodesSvc, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("101", map[string]any{
		"director":       strings.Repeat("d", 40),
		"deployment":     "p6-rM",
		"instance_group": "rM",
		"job":            "rM",
		"index":          "0",
		"name":           "rM/" + id,
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries := strings.Split(gotTags, ";")
	count := 0
	for _, e := range entries {
		if strings.HasSuffix(strings.ToLower(e), "--"+id) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("want exactly one name tag for %s, got %d in %q", id, count, gotTags)
	}
	if !slices.Contains(entries, "vm-prefix--bosh") && !slices.Contains(entries, "vm-prefix--halab-b") {
		t.Errorf("the vm-prefix identity tag was lost; got %q", gotTags)
	}
	if len(gotTags) > 350 {
		t.Errorf("tags are %d bytes, over the 350 cap: %q", len(gotTags), gotTags)
	}
}

// TestHandleSetVMMetadata_RenamedGroupLeavesNoStaleNameTag checks that the
// instance name tag is rebuilt on every sync, so a stored tag for the same
// instance id under an earlier group name does not stay beside the new one.
func TestHandleSetVMMetadata_RenamedGroupLeavesNoStaleNameTag(t *testing.T) {
	t.Parallel()

	const id = "1de28840-4a9e-4d4f-a421-50270360faac"
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"tags": "bosh-cpi;env--prod;oldgroup--" + id}, nil
		},
	}
	var gotTags string
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _, _ string, params *nodes.UpdateQemuConfigParams) error {
			if params.Tags != nil {
				gotTags = *params.Tags
			}
			return nil
		},
	}

	h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, qemuSvc, nodesSvc, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("101", map[string]any{
		"instance_group": "newGroup",
		"name":           "newGroup/" + id,
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entries := strings.Split(gotTags, ";")
	if slices.Contains(entries, "oldgroup--"+id) {
		t.Errorf("stale name tag survived; got %q", gotTags)
	}
	if !slices.Contains(entries, "newGroup--"+id) || !slices.Contains(entries, "env--prod") {
		t.Errorf("want the new name tag and the operator tag; got %q", gotTags)
	}
}
