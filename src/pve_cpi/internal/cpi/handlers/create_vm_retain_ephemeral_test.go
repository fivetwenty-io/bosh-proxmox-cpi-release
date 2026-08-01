package handlers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
)

// ---------------------------------------------------------------------------
// cloud_properties.retain_ephemeral_on_delete flows into the initial tag set
// stamped on the VM at create time (create_vm.go's initialTags construction).
// delete_vm reads this tag (handlers.TagRetainEphemeral, i.e.
// tagRetainEphemeral) to decide whether the ephemeral disk survives delete —
// see detachRetainedEphemeralDisk — so a VM created without the tag has its
// ephemeral disk destroyed regardless of what the operator asked for. These
// tests drive HandleCreateVM end to end and assert on the tags param of the
// captured QEMU.Create call, rather than reconstructing the tag-merge logic
// inside the test body.
// ---------------------------------------------------------------------------

func TestCreateVM_RetainEphemeralOnDelete_True_TagAppliedOnCreate(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-retain-ephemeral-true", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512, "retain_ephemeral_on_delete": true},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("retain-ephemeral-true")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	p := q.createCalls[0].params
	tags, _ := p["tags"].(string)
	if !strings.Contains(tags, handlers.TagRetainEphemeral) {
		t.Errorf("createParams[\"tags\"] = %q; want it to contain %q when cloud_properties.retain_ephemeral_on_delete is true",
			tags, handlers.TagRetainEphemeral)
	}
}

func TestCreateVM_RetainEphemeralOnDelete_Nil_NoTagOnCreate(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	// retain_ephemeral_on_delete omitted entirely (nil in the decoded struct).
	args := mkArgs("agent-retain-ephemeral-nil", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("retain-ephemeral-nil")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	p := q.createCalls[0].params
	tags, _ := p["tags"].(string)
	if strings.Contains(tags, handlers.TagRetainEphemeral) {
		t.Errorf("createParams[\"tags\"] = %q; must NOT contain %q when retain_ephemeral_on_delete is unset",
			tags, handlers.TagRetainEphemeral)
	}
}
