package handlers_test

import (
	"context"
	"errors"
	"slices"
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

	h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}))
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

	h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}))
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

	h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}))
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

	h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}))
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

	h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}))
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

	h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("101", metadata), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotTags) > 255 {
		t.Errorf("tags length %d exceeds 255; got: %q", len(gotTags), gotTags)
	}
	// Truncation must occur at a ";" boundary — no partial "<key>--<value>".
	for part := range strings.SplitSeq(gotTags, ";") {
		if part == "" || !strings.Contains(part, "--") {
			t.Errorf("malformed tag %q in %q", part, gotTags)
		}
	}
}

// TestHandleSetVMMetadata_PreservesCustomTags verifies operator-supplied tags
// already on the VM (env--prod, owner--alice) survive a director re-sync.
func TestHandleSetVMMetadata_PreservesCustomTags(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"tags": "env--prod;owner--alice",
			}, nil
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
		"director":   "bosh",
		"deployment": "cf",
		"job":        "diego_cell",
	}), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"env--prod", "owner--alice", "director--bosh", "deployment--cf", "job--diego-cell"} {
		if !strings.Contains(gotTags, want) {
			t.Errorf("tags missing %q; got: %q", want, gotTags)
		}
	}
}

// TestHandleSetVMMetadata_ReplacesStaleBoshTags verifies that pre-existing
// director--/deployment--/job-- entries are dropped and rebuilt from this
// call's metadata, so stale values cannot accumulate.
func TestHandleSetVMMetadata_ReplacesStaleBoshTags(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"tags": "director--old-uuid;deployment--old;job--old;env--prod",
			}, nil
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
		"director":   "new-uuid",
		"deployment": "cf",
		"job":        "diego",
	}), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(gotTags, "director--old-uuid") || strings.Contains(gotTags, "deployment--old") || strings.Contains(gotTags, "job--old") {
		t.Errorf("stale BOSH-managed tags survived; got: %q", gotTags)
	}
	for _, want := range []string{"director--new-uuid", "deployment--cf", "job--diego", "env--prod"} {
		if !strings.Contains(gotTags, want) {
			t.Errorf("tags missing %q; got: %q", want, gotTags)
		}
	}
}

// TestHandleSetVMMetadata_EmitsNameTag verifies the BOSH instance name
// "<job>/<id>" is emitted as a "<job>--<id>" tag (PVE tags reject "/", so
// the slash is rewritten to the "--" separator used elsewhere in tags).
func TestHandleSetVMMetadata_EmitsNameTag(t *testing.T) {
	t.Parallel()

	metadata := map[string]any{
		"director":   "bosh",
		"deployment": "cf",
		"job":        "diego-cell",
		"id":         "2844c990-aef3-4de7-8bf3-d936fc2201be",
		"name":       "diego-cell/2844c990-aef3-4de7-8bf3-d936fc2201be",
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

	h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("101", metadata), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "diego-cell--2844c990-aef3-4de7-8bf3-d936fc2201be"
	if !strings.Contains(gotTags, want) {
		t.Errorf("expected name tag %q in tags; got: %q", want, gotTags)
	}

	// Exact entry match — splitting on ";" should yield the literal string.
	if !slices.Contains(strings.Split(gotTags, ";"), want) {
		t.Errorf("name tag %q not present as a standalone entry; got: %q", want, gotTags)
	}
}

// TestHandleSetVMMetadata_NameTagSanitizesInvalidBytes verifies that any
// non-[A-Za-z0-9-] byte in the BOSH name (other than the "/" between job
// and id, which is rewritten to "--") is replaced with "-".
func TestHandleSetVMMetadata_NameTagSanitizesInvalidBytes(t *testing.T) {
	t.Parallel()

	// Underscore is invalid in PVE tag values → must become "-".
	metadata := map[string]any{
		"name": "diego_cell/abc_123",
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

	h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("101", metadata), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "diego-cell--abc-123"
	if !strings.Contains(gotTags, want) {
		t.Errorf("expected sanitized name tag %q; got: %q", want, gotTags)
	}
	if strings.ContainsRune(gotTags, '/') {
		t.Errorf("tags must not contain '/'; got: %q", gotTags)
	}
}

// TestHandleSetVMMetadata_NameTagMissingOmitted verifies that omitting the
// "name" key from metadata simply skips the name tag (no empty entry, no error).
func TestHandleSetVMMetadata_NameTagMissingOmitted(t *testing.T) {
	t.Parallel()

	metadata := map[string]any{
		"director":   "bosh",
		"deployment": "cf",
		"job":        "diego-cell",
		// no "name" key
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

	h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("101", metadata), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for p := range strings.SplitSeq(gotTags, ";") {
		if p == "" {
			t.Errorf("empty tag entry in: %q", gotTags)
		}
	}
}

// TestHandleSetVMMetadata_SetsVMName verifies that metadata["name"] is
// rewritten to a DNS label and passed as UpdateQemuConfigParams.Name so
// the PVE UI shows the BOSH instance identifier instead of "vm-<vmid>".
func TestHandleSetVMMetadata_SetsVMName(t *testing.T) {
	t.Parallel()

	var gotName *string
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _, _ string, params *nodes.UpdateQemuConfigParams) error {
			gotName = params.Name
			return nil
		},
	}

	h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("101", map[string]any{
		"name": "diego-cell/2844c990-aef3-4de7-8bf3-d936fc2201be",
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotName == nil {
		t.Fatal("expected Name to be set, got nil")
	}
	want := "diego-cell-2844c990-aef3-4de7-8bf3-d936fc2201be"
	if *gotName != want {
		t.Errorf("Name = %q, want %q", *gotName, want)
	}
}

// TestHandleSetVMMetadata_VMNameFromJobIndex verifies the primary VM name
// derivation: "<job>-<index>" so the PVE UI shows "diego-cell-0" rather
// than the UUID-bearing "<job>-<id>" form. The UUID name metadata is only
// a fallback.
func TestHandleSetVMMetadata_VMNameFromJobIndex(t *testing.T) {
	t.Parallel()

	cases := []struct {
		desc string
		md   map[string]any
		want string
	}{
		{
			desc: "diego-cell job with numeric index",
			md: map[string]any{
				"job":   "diego-cell",
				"index": 0,
				"name":  "diego-cell/2844c990-aef3-4de7-8bf3-d936fc2201be",
			},
			want: "diego-cell-0",
		},
		{
			desc: "bosh job with string index",
			md: map[string]any{
				"job":   "bosh",
				"index": "0",
				"name":  "bosh/abcd1234",
			},
			want: "bosh-0",
		},
		{
			desc: "job with underscores sanitized",
			md: map[string]any{
				"job":   "diego_cell",
				"index": 2,
			},
			want: "diego-cell-2",
		},
	}

	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			t.Parallel()
			var gotName *string
			nodesSvc := &mockNodesService{
				updateQemuConfigFn: func(_ context.Context, _, _ string, params *nodes.UpdateQemuConfigParams) error {
					gotName = params.Name
					return nil
				},
			}
			h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}))
			_, err := h.Handle(context.Background(), marshalArgs("101", c.md), jsonrpc.Context{})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotName == nil {
				t.Fatalf("expected Name=%q, got nil", c.want)
			}
			if *gotName != c.want {
				t.Errorf("Name = %q, want %q", *gotName, c.want)
			}
		})
	}
}

// TestHandleSetVMMetadata_VMNameFallsBackToName verifies that when job or
// index is missing, the handler falls back to sanitizing metadata["name"]
// ("<job>/<id>") so existing deployments don't regress.
func TestHandleSetVMMetadata_VMNameFallsBackToName(t *testing.T) {
	t.Parallel()

	var gotName *string
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _, _ string, params *nodes.UpdateQemuConfigParams) error {
			gotName = params.Name
			return nil
		},
	}

	h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}))
	// job present, index absent → fall back to name.
	_, err := h.Handle(context.Background(), marshalArgs("101", map[string]any{
		"job":  "diego-cell",
		"name": "diego-cell/2844c990-aef3-4de7-8bf3-d936fc2201be",
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotName == nil {
		t.Fatal("expected fallback Name, got nil")
	}
	want := "diego-cell-2844c990-aef3-4de7-8bf3-d936fc2201be"
	if *gotName != want {
		t.Errorf("Name = %q, want %q", *gotName, want)
	}
}

// TestHandleSetVMMetadata_LeavesVMNameUnchangedWhenAbsent verifies that
// when metadata has no "name" key the Name pointer is nil — preserving the
// existing PVE name instead of clobbering it with an empty string.
func TestHandleSetVMMetadata_LeavesVMNameUnchangedWhenAbsent(t *testing.T) {
	t.Parallel()

	var gotName *string
	called := false
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _, _ string, params *nodes.UpdateQemuConfigParams) error {
			called = true
			gotName = params.Name
			return nil
		},
	}

	h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("101", map[string]any{
		"director":   "bosh",
		"deployment": "cf",
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("UpdateQemuConfig was not called")
	}
	if gotName != nil {
		t.Errorf("Name should be nil when metadata lacks 'name'; got %q", *gotName)
	}
}

// TestHandleSetVMMetadata_VMNameIncludesDeployment verifies that when the
// director provides a "deployment" key the VM name includes it as the
// segment between any prefix and the job, so VMs from different deployments
// are distinguishable in the PVE UI.
func TestHandleSetVMMetadata_VMNameIncludesDeployment(t *testing.T) {
	t.Parallel()

	var gotName *string
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _, _ string, params *nodes.UpdateQemuConfigParams) error {
			gotName = params.Name
			return nil
		},
	}

	h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}))
	_, err := h.Handle(context.Background(), marshalArgs("101", map[string]any{
		"deployment": "cf",
		"job":        "api",
		"index":      0,
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotName == nil {
		t.Fatal("expected Name to be set")
	}
	if *gotName != "cf-api-0" {
		t.Errorf("Name = %q, want %q", *gotName, "cf-api-0")
	}
}

// TestHandleSetVMMetadata_VMNameWithPrefix verifies that Config.VMPrefix is
// prepended to the assembled name, yielding "<prefix>-<deployment>-<job>-<index>".
func TestHandleSetVMMetadata_VMNameWithPrefix(t *testing.T) {
	t.Parallel()

	var gotName *string
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _, _ string, params *nodes.UpdateQemuConfigParams) error {
			gotName = params.Name
			return nil
		},
	}

	deps := testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{})
	deps.Config.VMPrefix = "cpi"

	h := handlers.HandleSetVMMetadata(deps)
	_, err := h.Handle(context.Background(), marshalArgs("101", map[string]any{
		"deployment": "cf",
		"job":        "diego-cell",
		"index":      2,
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotName == nil {
		t.Fatal("expected Name to be set")
	}
	if *gotName != "cpi-cf-diego-cell-2" {
		t.Errorf("Name = %q, want %q", *gotName, "cpi-cf-diego-cell-2")
	}
}

// TestHandleSetVMMetadata_EmitsIndexTag verifies that metadata["index"] is
// emitted as an "index--<n>" PVE tag alongside the director/deployment/job
// triple, and that stale "index--" tags from a prior sync are replaced.
func TestHandleSetVMMetadata_EmitsIndexTag(t *testing.T) {
	t.Parallel()

	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"tags": "index--7;env--prod",
			}, nil
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
		"director":   "bosh",
		"deployment": "cf",
		"job":        "api",
		"index":      0,
	}), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(gotTags, "index--7") {
		t.Errorf("stale index tag survived; got: %q", gotTags)
	}
	for _, want := range []string{"index--0", "director--bosh", "deployment--cf", "job--api", "env--prod"} {
		if !strings.Contains(gotTags, want) {
			t.Errorf("tags missing %q; got: %q", want, gotTags)
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

	h := handlers.HandleSetVMMetadata(testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}))
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
	if posA >= posM || posM >= posZ {
		t.Errorf("description keys not sorted: a=%d m=%d z=%d in %q", posA, posM, posZ, gotDescription)
	}
}
