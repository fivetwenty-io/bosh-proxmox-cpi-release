package handlers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// TestHandleSetVMMetadata_Happy verifies description and tags are written correctly.
func TestHandleSetVMMetadata_Happy(t *testing.T) {
	t.Parallel()

	metadata := map[string]any{
		"director":   "bosh-director",
		"deployment": "cf",
		"job":        "diego_cell",
		"index":      "0",
		"id":         "vm-abc123",
	}

	var gotDescription string
	var gotTags string

	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, node string, vmid string, params *nodes.UpdateQemuConfigParams) error {
			if node != "pve-node1" || vmid != "101" {
				t.Errorf("UpdateQemuConfig: unexpected node=%q vmid=%q", node, vmid)
			}
			if params.Description != nil {
				gotDescription = *params.Description
			}
			if params.Tags != nil {
				gotTags = *params.Tags
			}
			return nil
		},
	}

	h := handlers.HandleSetVMMetadata(testDeps(nil, nodesSvc, nil, &mockAgentService{}))
	result, err := h.Handle(context.Background(), marshalArgs("101", metadata), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}

	// Description should contain all 5 keys sorted.
	for _, key := range []string{"deployment", "director", "id", "index", "job"} {
		if !strings.Contains(gotDescription, key+": ") {
			t.Errorf("description missing key %q; got:\n%s", key, gotDescription)
		}
	}

	// Tags must use "<key>--<value>" form with non-alphanumeric/non-"-" bytes
	// in the value replaced with "-" ("diego_cell" -> "diego-cell").
	expectedTagParts := []string{"director--bosh-director", "deployment--cf", "job--diego-cell"}
	for _, part := range expectedTagParts {
		if !strings.Contains(gotTags, part) {
			t.Errorf("tags missing %q; got: %q", part, gotTags)
		}
	}

	// Tags must not exceed maxTagLength (255).
	if len(gotTags) > 255 {
		t.Errorf("tags length %d exceeds 255; got: %q", len(gotTags), gotTags)
	}
}

// TestHandleSetVMMetadata_EmptyMetadata verifies empty metadata writes empty description/tags.
func TestHandleSetVMMetadata_EmptyMetadata(t *testing.T) {
	t.Parallel()

	var gotDescription string
	var gotTags string

	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _, _ string, params *nodes.UpdateQemuConfigParams) error {
			if params.Description != nil {
				gotDescription = *params.Description
			}
			if params.Tags != nil {
				gotTags = *params.Tags
			}
			return nil
		},
	}

	h := handlers.HandleSetVMMetadata(testDeps(nil, nodesSvc, nil, &mockAgentService{}))
	result, err := h.Handle(context.Background(), marshalArgs("101", map[string]any{}), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if gotDescription != "" {
		t.Errorf("expected empty description for empty metadata, got: %q", gotDescription)
	}
	if gotTags != "" {
		t.Errorf("expected empty tags for empty metadata, got: %q", gotTags)
	}
}

// TestHandleSetVMMetadata_NotFound verifies 404 from UpdateQemuConfig yields VMNotFound.
func TestHandleSetVMMetadata_NotFound(t *testing.T) {
	t.Parallel()

	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return notFoundAPIErr()
		},
	}

	h := handlers.HandleSetVMMetadata(testDeps(nil, nodesSvc, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("999", map[string]any{"director": "bosh"}), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected VMNotFound error, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Fatalf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() != cpierrors.TypeVMNotFound {
		t.Errorf("error type = %q; want %q", cpiErr.Type(), cpierrors.TypeVMNotFound)
	}
}

// TestHandleSetVMMetadata_SDKError verifies non-404 errors are propagated.
func TestHandleSetVMMetadata_SDKError(t *testing.T) {
	t.Parallel()

	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return errors.New("pve: config locked")
		},
	}

	h := handlers.HandleSetVMMetadata(testDeps(nil, nodesSvc, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("101", map[string]any{"index": "0"}), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error from SDK failure, got nil")
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) {
		t.Errorf("expected *cpierrors.Error, got %T: %v", err, err)
	}
	if cpiErr.Type() == cpierrors.TypeVMNotFound {
		t.Error("generic SDK error should not yield VMNotFound")
	}
}

// TestHandleSetVMMetadata_MissingVMCID verifies missing first argument returns error.
func TestHandleSetVMMetadata_MissingVMCID(t *testing.T) {
	t.Parallel()

	h := handlers.HandleSetVMMetadata(testDeps(nil, nil, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), nil, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected error for missing vm_cid")
	}
}

// TestHandleSetVMMetadata_NullMetadata verifies null metadata arg is treated as empty map.
func TestHandleSetVMMetadata_NullMetadata(t *testing.T) {
	t.Parallel()

	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _, _ string, _ *nodes.UpdateQemuConfigParams) error {
			return nil
		},
	}

	h := handlers.HandleSetVMMetadata(testDeps(nil, nodesSvc, nil, &mockAgentService{}))
	// args[1] = JSON null
	result, err := h.Handle(context.Background(), marshalArgs("101", nil), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error for null metadata: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
}

// TestHandleSetVMMetadata_TagTruncation verifies long tags are truncated to
// maxTagLength at a tag boundary, never emitting a partial "<key>--<value>".
func TestHandleSetVMMetadata_TagTruncation(t *testing.T) {
	t.Parallel()

	// Three values long enough that the joined "<key>--<value>" form exceeds
	// maxTagLength (255). Each prefixed tag is ~110 bytes; three joined ~330.
	long := strings.Repeat("a", 100)
	metadata := map[string]any{
		"director":   "d-" + long,
		"deployment": "d-" + long,
		"job":        "j-" + long,
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

	h := handlers.HandleSetVMMetadata(testDeps(nil, nodesSvc, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("101", metadata), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotTags) > 255 {
		t.Errorf("tags length %d exceeds 255; got: %q", len(gotTags), gotTags)
	}
	// Truncation must occur at a ";" boundary — no partial "<key>--<value>".
	for _, part := range strings.Split(gotTags, ";") {
		if part == "" || !strings.Contains(part, "--") {
			t.Errorf("malformed tag %q in %q", part, gotTags)
		}
	}
}

// TestHandleSetVMMetadata_DescriptionSorted verifies description lines are sorted by key.
func TestHandleSetVMMetadata_DescriptionSorted(t *testing.T) {
	t.Parallel()

	metadata := map[string]any{
		"z_last":  "zzz",
		"a_first": "aaa",
		"m_mid":   "mmm",
	}

	var gotDescription string
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _, _ string, params *nodes.UpdateQemuConfigParams) error {
			if params.Description != nil {
				gotDescription = *params.Description
			}
			return nil
		},
	}

	h := handlers.HandleSetVMMetadata(testDeps(nil, nodesSvc, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("101", metadata), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify alphabetical order: a_first before m_mid before z_last.
	posA := strings.Index(gotDescription, "a_first")
	posM := strings.Index(gotDescription, "m_mid")
	posZ := strings.Index(gotDescription, "z_last")
	if posA < 0 || posM < 0 || posZ < 0 {
		t.Fatalf("description missing expected keys: %q", gotDescription)
	}
	if !(posA < posM && posM < posZ) {
		t.Errorf("description keys not sorted: a=%d m=%d z=%d in %q", posA, posM, posZ, gotDescription)
	}
}
