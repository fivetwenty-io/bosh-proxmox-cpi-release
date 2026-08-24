package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// ---- templateNodesService ----
// Embeds sdknodes.Service (panics on unimplemented methods) and overrides
// CreateQemuClone, CreateQemuTemplate, and ListQemu to enable targeted testing.

type templateNodesService struct {
	sdknodes.Service
	createQemuCloneFn    func(ctx context.Context, node, vmid string, params *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error)
	createQemuTemplateFn func(ctx context.Context, node, vmid string, params *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error)
	listQemuFn           func(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error)
}

func (s *templateNodesService) CreateQemuClone(ctx context.Context, node, vmid string, params *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
	if s.createQemuCloneFn != nil {
		return s.createQemuCloneFn(ctx, node, vmid, params)
	}
	// Default: return an empty raw message (no UPID).
	raw := sdknodes.CreateQemuCloneResponse{}
	return &raw, nil
}

func (s *templateNodesService) CreateQemuTemplate(ctx context.Context, node, vmid string, params *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
	if s.createQemuTemplateFn != nil {
		return s.createQemuTemplateFn(ctx, node, vmid, params)
	}
	// Default: return an empty raw message (no UPID).
	raw := sdknodes.CreateQemuTemplateResponse{}
	return &raw, nil
}

func (s *templateNodesService) ListQemu(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	if s.listQemuFn != nil {
		return s.listQemuFn(ctx, node, params)
	}
	// Default: empty list.
	out := sdknodes.ListQemuResponse{}
	return &out, nil
}

// newTemplateClient wires a templateNodesService into a mockClient.
func newTemplateClient(nodesSvc sdknodes.Service) *mockClient {
	return &mockClient{
		tasksSvc: &mockTasksService{},
		nodesSvc: nodesSvc,
	}
}

// marshalUPID encodes a UPID string as a JSON string, mirroring PVE's bare-string response.
func marshalUPID(upid string) json.RawMessage {
	b, _ := json.Marshal(upid)
	return b
}

// ---- BuildTemplateName tests ----

func TestBuildTemplateName_Basic(t *testing.T) {
	t.Parallel()
	// PVE VM names must be DNS-valid: '_' and '.' are replaced with '-'.
	got := pve.BuildTemplateName("bosh-proxmox-kvm-ubuntu-jammy-go_agent", "1.234")
	want := "bosh-stemcell-bosh-proxmox-kvm-ubuntu-jammy-go-agent-1-234"
	if got != want {
		t.Errorf("BuildTemplateName: got %q, want %q", got, want)
	}
}

func TestBuildTemplateName_UnderscoreReplaced(t *testing.T) {
	t.Parallel()
	// Underscores are invalid in a PVE/DNS name; runs collapse to a single '-'.
	got := pve.BuildTemplateName("ubuntu__jammy", "2.0")
	want := "bosh-stemcell-ubuntu-jammy-2-0"
	if got != want {
		t.Errorf("BuildTemplateName: got %q, want %q", got, want)
	}
}

func TestBuildTemplateName_UppercaseNormalized(t *testing.T) {
	t.Parallel()
	got := pve.BuildTemplateName("Ubuntu-Jammy", "1.0")
	want := "bosh-stemcell-ubuntu-jammy-1-0"
	if got != want {
		t.Errorf("BuildTemplateName: got %q, want %q", got, want)
	}
}

func TestBuildTemplateName_LengthCap(t *testing.T) {
	t.Parallel()
	// Build a name+version that would produce a result exceeding 200 chars.
	// prefix "bosh-stemcell-" = 14 chars; need remainder > 186.
	longName := strings.Repeat("a", 100)
	longVersion := strings.Repeat("b", 100)
	got := pve.BuildTemplateName(longName, longVersion)
	if len(got) > 200 {
		t.Errorf("BuildTemplateName: length %d exceeds 200-char cap", len(got))
	}
	// Must not end with a dash after truncation trimming.
	if strings.HasSuffix(got, "-") {
		t.Errorf("BuildTemplateName: result ends with trailing dash: %q", got)
	}
	// Must start with the expected prefix.
	if !strings.HasPrefix(got, "bosh-stemcell-") {
		t.Errorf("BuildTemplateName: missing prefix in %q", got)
	}
}

func TestBuildTemplateName_EmptyInputs(t *testing.T) {
	t.Parallel()
	// Both empty → sanitizeStemcellPart returns "", Trim strips nothing.
	got := pve.BuildTemplateName("", "")
	// Result: "bosh-stemcell--" → but sanitize trims leading/trailing dashes,
	// so each part is "". Joined: "bosh-stemcell--".
	// Trim on each part separately: name="" version="" → "bosh-stemcell--".
	if !strings.HasPrefix(got, "bosh-stemcell-") {
		t.Errorf("BuildTemplateName(empty,empty): got %q, want prefix bosh-stemcell-", got)
	}
}

func TestBuildTemplateName_SpecialCharsInVersion(t *testing.T) {
	t.Parallel()
	got := pve.BuildTemplateName("ubuntu-jammy", "1.0+build.3")
	// '+', '.', and spaces → '-'; consecutive replacements collapse to one '-'.
	want := "bosh-stemcell-ubuntu-jammy-1-0-build-3"
	if got != want {
		t.Errorf("BuildTemplateName: got %q, want %q", got, want)
	}
}

// ---- CloneQemuVM tests ----

func TestCloneQemuVM_Success(t *testing.T) {
	t.Parallel()
	const wantUPID = "UPID:pve1:00001234:clone:6042:"
	var capturedNode, capturedVMID string
	var capturedParams *sdknodes.CreateQemuCloneParams

	nodesSvc := &templateNodesService{
		createQemuCloneFn: func(_ context.Context, node, vmid string, params *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			capturedNode = node
			capturedVMID = vmid
			capturedParams = params
			raw := marshalUPID(wantUPID)
			return &raw, nil
		},
	}
	c := newTemplateClient(nodesSvc)

	params := &sdknodes.CreateQemuCloneParams{Newid: 7001}
	upid, err := pve.CloneQemuVM(context.Background(), c, "pve1", 6042, params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if upid != wantUPID {
		t.Errorf("CloneQemuVM: upid = %q, want %q", upid, wantUPID)
	}
	if capturedNode != "pve1" {
		t.Errorf("CloneQemuVM: forwarded node = %q, want pve1", capturedNode)
	}
	if capturedVMID != "6042" {
		t.Errorf("CloneQemuVM: forwarded vmid = %q, want 6042", capturedVMID)
	}
	if capturedParams == nil || capturedParams.Newid != 7001 {
		t.Errorf("CloneQemuVM: params not forwarded correctly, got %+v", capturedParams)
	}
}

func TestCloneQemuVM_SDKError(t *testing.T) {
	t.Parallel()
	nodesSvc := &templateNodesService{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			return nil, errors.New("connection refused")
		},
	}
	c := newTemplateClient(nodesSvc)

	_, err := pve.CloneQemuVM(context.Background(), c, "pve1", 6042, &sdknodes.CreateQemuCloneParams{Newid: 7001})
	if err == nil {
		t.Fatal("CloneQemuVM: expected error from SDK, got nil")
	}
	if !strings.Contains(err.Error(), "CloneQemuVM") {
		t.Errorf("CloneQemuVM: error missing context label: %v", err)
	}
}

func TestCloneQemuVM_NilContext(t *testing.T) {
	t.Parallel()
	c := newTemplateClient(&templateNodesService{})
	//nolint:staticcheck // intentional nil ctx for validation test
	//lint:ignore SA1012 intentional nil ctx for validation test
	_, err := pve.CloneQemuVM(nil, c, "pve1", 6042, &sdknodes.CreateQemuCloneParams{Newid: 7001})
	if err == nil {
		t.Fatal("CloneQemuVM: expected error for nil ctx")
	}
}

func TestCloneQemuVM_NilClient(t *testing.T) {
	t.Parallel()
	_, err := pve.CloneQemuVM(context.Background(), nil, "pve1", 6042, &sdknodes.CreateQemuCloneParams{Newid: 7001})
	if err == nil {
		t.Fatal("CloneQemuVM: expected error for nil client")
	}
}

func TestCloneQemuVM_EmptyNode(t *testing.T) {
	t.Parallel()
	c := newTemplateClient(&templateNodesService{})
	_, err := pve.CloneQemuVM(context.Background(), c, "", 6042, &sdknodes.CreateQemuCloneParams{Newid: 7001})
	if err == nil {
		t.Fatal("CloneQemuVM: expected error for empty node")
	}
}

func TestCloneQemuVM_InvalidVMID(t *testing.T) {
	t.Parallel()
	c := newTemplateClient(&templateNodesService{})

	for _, vmid := range []int64{0, -1, -999} {
		_, err := pve.CloneQemuVM(context.Background(), c, "pve1", vmid, &sdknodes.CreateQemuCloneParams{Newid: 7001})
		if err == nil {
			t.Errorf("CloneQemuVM: expected error for vmid=%d, got nil", vmid)
		}
	}
}

func TestCloneQemuVM_NilParams(t *testing.T) {
	t.Parallel()
	c := newTemplateClient(&templateNodesService{})
	_, err := pve.CloneQemuVM(context.Background(), c, "pve1", 6042, nil)
	if err == nil {
		t.Fatal("CloneQemuVM: expected error for nil params")
	}
}

func TestCloneQemuVM_NilResponseReturnsEmpty(t *testing.T) {
	t.Parallel()
	// SDK returns nil *CreateQemuCloneResponse (no-UPID synchronous completion).
	nodesSvc := &templateNodesService{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			return nil, nil
		},
	}
	c := newTemplateClient(nodesSvc)
	upid, err := pve.CloneQemuVM(context.Background(), c, "pve1", 6042, &sdknodes.CreateQemuCloneParams{Newid: 7001})
	if err != nil {
		t.Fatalf("CloneQemuVM: unexpected error for nil response: %v", err)
	}
	if upid != "" {
		t.Errorf("CloneQemuVM: expected empty upid for nil response, got %q", upid)
	}
}

// ---- MakeTemplate tests ----

func TestMakeTemplate_Success(t *testing.T) {
	t.Parallel()
	const wantUPID = "UPID:pve1:00005678:template:6042:"
	var capturedNode, capturedVMID string

	nodesSvc := &templateNodesService{
		createQemuTemplateFn: func(_ context.Context, node, vmid string, params *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			capturedNode = node
			capturedVMID = vmid
			// Verify Disk is nil (all-disks mode).
			if params == nil {
				return nil, errors.New("MakeTemplate: params must not be nil")
			}
			if params.Disk != nil {
				return nil, errors.New("MakeTemplate: expected nil Disk for all-disks conversion")
			}
			raw := marshalUPID(wantUPID)
			return &raw, nil
		},
	}
	c := newTemplateClient(nodesSvc)

	upid, err := pve.MakeTemplate(context.Background(), c, "pve1", 6042)
	if err != nil {
		t.Fatalf("MakeTemplate: unexpected error: %v", err)
	}
	if upid != wantUPID {
		t.Errorf("MakeTemplate: upid = %q, want %q", upid, wantUPID)
	}
	if capturedNode != "pve1" {
		t.Errorf("MakeTemplate: forwarded node = %q, want pve1", capturedNode)
	}
	if capturedVMID != "6042" {
		t.Errorf("MakeTemplate: forwarded vmid = %q, want 6042", capturedVMID)
	}
}

func TestMakeTemplate_SDKError(t *testing.T) {
	t.Parallel()
	nodesSvc := &templateNodesService{
		createQemuTemplateFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			return nil, errors.New("disk busy")
		},
	}
	c := newTemplateClient(nodesSvc)

	_, err := pve.MakeTemplate(context.Background(), c, "pve1", 6042)
	if err == nil {
		t.Fatal("MakeTemplate: expected error from SDK, got nil")
	}
	if !strings.Contains(err.Error(), "MakeTemplate") {
		t.Errorf("MakeTemplate: error missing context label: %v", err)
	}
}

func TestMakeTemplate_NilContext(t *testing.T) {
	t.Parallel()
	c := newTemplateClient(&templateNodesService{})
	//nolint:staticcheck // intentional nil ctx for validation test
	//lint:ignore SA1012 intentional nil ctx for validation test
	_, err := pve.MakeTemplate(nil, c, "pve1", 6042)
	if err == nil {
		t.Fatal("MakeTemplate: expected error for nil ctx")
	}
}

func TestMakeTemplate_NilClient(t *testing.T) {
	t.Parallel()
	_, err := pve.MakeTemplate(context.Background(), nil, "pve1", 6042)
	if err == nil {
		t.Fatal("MakeTemplate: expected error for nil client")
	}
}

func TestMakeTemplate_EmptyNode(t *testing.T) {
	t.Parallel()
	c := newTemplateClient(&templateNodesService{})
	_, err := pve.MakeTemplate(context.Background(), c, "", 6042)
	if err == nil {
		t.Fatal("MakeTemplate: expected error for empty node")
	}
}

func TestMakeTemplate_InvalidVMID(t *testing.T) {
	t.Parallel()
	c := newTemplateClient(&templateNodesService{})

	for _, vmid := range []int64{0, -1, -999} {
		_, err := pve.MakeTemplate(context.Background(), c, "pve1", vmid)
		if err == nil {
			t.Errorf("MakeTemplate: expected error for vmid=%d, got nil", vmid)
		}
	}
}

func TestMakeTemplate_NilResponseReturnsEmpty(t *testing.T) {
	t.Parallel()
	// SDK returns nil (synchronous, no task).
	nodesSvc := &templateNodesService{
		createQemuTemplateFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			return nil, nil
		},
	}
	c := newTemplateClient(nodesSvc)
	upid, err := pve.MakeTemplate(context.Background(), c, "pve1", 6042)
	if err != nil {
		t.Fatalf("MakeTemplate: unexpected error for nil response: %v", err)
	}
	if upid != "" {
		t.Errorf("MakeTemplate: expected empty upid for nil response, got %q", upid)
	}
}

func TestMakeTemplate_UPIDFromObjectField(t *testing.T) {
	t.Parallel()
	// Some PVE versions return {"upid":"..."} instead of a bare string.
	const wantUPID = "UPID:pve1:00005678:template:6042:"
	objRaw, _ := json.Marshal(map[string]string{"upid": wantUPID})

	nodesSvc := &templateNodesService{
		createQemuTemplateFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			raw := sdknodes.CreateQemuTemplateResponse(objRaw)
			return &raw, nil
		},
	}
	c := newTemplateClient(nodesSvc)

	upid, err := pve.MakeTemplate(context.Background(), c, "pve1", 6042)
	if err != nil {
		t.Fatalf("MakeTemplate: unexpected error: %v", err)
	}
	if upid != wantUPID {
		t.Errorf("MakeTemplate: upid = %q, want %q", upid, wantUPID)
	}
}

// ---- helpers for FindTemplate tests ----

// ---- FindTemplateByName tests ----

// ---- FindTemplateBySHATag tests ----

// ---- regression: PVE serialises booleans as integers (0/1) ----
//
// The real PVE API (Perl-backed) returns "template":1 for frozen templates,
// not the JSON boolean "template":true. These tests build the list response
// from raw JSON with the integer shape to guard the dedup lookups against
// regressing to a *bool decode that skips every template.

// makeListQemuResponseRaw builds a ListQemuResponse directly from raw JSON
// object strings, bypassing the *bool-typed listQemuItem helper so tests can
// reproduce PVE's integer-boolean wire shape verbatim.
func makeListQemuResponseRaw(objs ...string) *sdknodes.ListQemuResponse {
	resp := make(sdknodes.ListQemuResponse, 0, len(objs))
	for _, o := range objs {
		resp = append(resp, json.RawMessage(o))
	}
	return &resp
}

// ============================================================
// ResolveTemplateVMIDForNode tests
// ============================================================

// resolveTemplateClient builds a mockClient wired with a single listQemuFn.
func resolveTemplateClient(fn func(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error)) *mockClient {
	return newTemplateClient(&templateNodesService{listQemuFn: fn})
}

// TestResolveTemplateVMIDForNode_PrimaryTemplate verifies a primary template (sha
// tag, no node tag) is found.
func TestResolveTemplateVMIDForNode_PrimaryTemplate(t *testing.T) {
	t.Parallel()
	sha8 := "abc12345"
	c := resolveTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		// Primary: sha tag present, no node tag.
		return makeListQemuResponseRaw(
			`{"vmid":30001,"template":1,"tags":"bosh-stemcell-cache;bosh-stemcell-sha-` + sha8 + `"}`,
		), nil
	})

	vmid, found, err := pve.ResolveTemplateVMIDForNode(context.Background(), c, "pve1", sha8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for primary template")
	}
	if vmid != 30001 {
		t.Errorf("vmid = %d; want 30001", vmid)
	}
}

// TestResolveTemplateVMIDForNode_Replica verifies a per-node replica (sha + node tag) is found.
func TestResolveTemplateVMIDForNode_Replica(t *testing.T) {
	t.Parallel()
	sha8 := "def45678"
	node := "pve2"
	nodeTag := pve.ReplicaNodeTagForNode(node)
	c := resolveTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponseRaw(
			`{"vmid":30002,"template":1,"tags":"bosh-stemcell-cache;bosh-stemcell-sha-` + sha8 + `;` + nodeTag + `"}`,
		), nil
	})

	vmid, found, err := pve.ResolveTemplateVMIDForNode(context.Background(), c, node, sha8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for replica template")
	}
	if vmid != 30002 {
		t.Errorf("vmid = %d; want 30002", vmid)
	}
}

// TestResolveTemplateVMIDForNode_OtherNodeReplicaNotMatched verifies a replica
// tagged for a different node is NOT returned.
func TestResolveTemplateVMIDForNode_OtherNodeReplicaNotMatched(t *testing.T) {
	t.Parallel()
	sha8 := "ghi90abc"
	node := "pve2"
	otherTag := pve.ReplicaNodeTagForNode("pve3")
	c := resolveTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		// This replica is for pve3, not pve2.
		return makeListQemuResponseRaw(
			`{"vmid":30003,"template":1,"tags":"bosh-stemcell-cache;bosh-stemcell-sha-` + sha8 + `;` + otherTag + `"}`,
		), nil
	})

	_, found, err := pve.ResolveTemplateVMIDForNode(context.Background(), c, node, sha8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("should NOT find pve3 replica when searching for pve2")
	}
}

// TestResolveTemplateVMIDForNode_NotFound verifies false when no matching template.
func TestResolveTemplateVMIDForNode_NotFound(t *testing.T) {
	t.Parallel()
	c := resolveTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		empty := sdknodes.ListQemuResponse{}
		return &empty, nil
	})
	_, found, err := pve.ResolveTemplateVMIDForNode(context.Background(), c, "pve1", "sha8xxxxx")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("expected not-found on empty list")
	}
}

// TestResolveTemplateVMIDForNode_EmptySHA8_ReturnsNotFound verifies empty sha8 returns false.
func TestResolveTemplateVMIDForNode_EmptySHA8_ReturnsNotFound(t *testing.T) {
	t.Parallel()
	c := resolveTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponseRaw(`{"vmid":30004,"template":1,"tags":"bosh-stemcell-cache;bosh-stemcell-sha-"}`), nil
	})
	_, found, err := pve.ResolveTemplateVMIDForNode(context.Background(), c, "pve1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("expected not-found when sha8 is empty")
	}
}

// TestResolveTemplateVMIDForNode_APIError propagates ListQemu errors.
func TestResolveTemplateVMIDForNode_APIError(t *testing.T) {
	t.Parallel()
	c := resolveTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return nil, errors.New("connection refused")
	})
	_, _, err := pve.ResolveTemplateVMIDForNode(context.Background(), c, "pve1", "sha8test")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestResolveTemplateVMIDForNode_TransientThenSuccess_Retries pins the F6
// fix: a single transient ListQemu fault (a pvedaemon worker recycle mid-scan)
// must not fail the placement scorer outright — it is exactly the kind of
// blip a single in-process retry absorbs, the same tolerance every sibling
// per-node listing in this package already has (listNodeGuests,
// FindTemplatesBySHATagNode).
func TestResolveTemplateVMIDForNode_TransientThenSuccess_Retries(t *testing.T) {
	t.Parallel()
	sha8 := "abc12345"
	var calls int
	c := resolveTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		calls++
		if calls == 1 {
			// Transient shape: "(code: 596)" is detected by IsTransientTransport.
			return nil, errors.New("pveproxy backend gone (code: 596)")
		}
		return makeListQemuResponseRaw(
			`{"vmid":30001,"template":1,"tags":"bosh-stemcell-cache;bosh-stemcell-sha-` + sha8 + `"}`,
		), nil
	})

	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	vmid, found, err := pve.ResolveTemplateVMIDForNode(ctx, c, "pve1", sha8)
	if err != nil {
		t.Fatalf("expected success after transient retry, got: %v", err)
	}
	if !found || vmid != 30001 {
		t.Errorf("vmid/found = %d/%v; want 30001/true", vmid, found)
	}
	if calls < 2 {
		t.Errorf("expected at least 2 ListQemu calls (1 transient + 1 success), got %d", calls)
	}
}

// TestResolveTemplateVMIDForNode_Exhausted5xx_ClassifiesRetriable pins the
// classification half of the F6 fix. A persistent 5xx (a pvedaemon that
// never recovers within the retry budget) exhausts RetryOnTransient and
// returns the raw SDK error; cpierrors.Wrap(listErr, ...) alone would flatten
// that to a permanent CloudError (Wrap defaults anything that is not already
// a *cpierrors.Error to non-retriable), stranding the Director on a fault a
// later re-drive would have cleared. WrapErrorKeepingClass must run first so
// the SDK's own 5xx classification (retriable) survives the wrap.
func TestResolveTemplateVMIDForNode_Exhausted5xx_ClassifiesRetriable(t *testing.T) {
	t.Parallel()
	c := resolveTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return nil, sdkerrors.ParseAPIError(500, []byte(`{"message":"pveproxy backend gone"}`))
	})
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	_, _, err := pve.ResolveTemplateVMIDForNode(ctx, c, "pve1", "sha8test")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("an exhausted 5xx must still classify TypeRetriableCloud; got: %v", err)
	}
}

// TestResolveTemplateVMIDForNode_LowestVMIDWins verifies tie-break by VMID.
func TestResolveTemplateVMIDForNode_LowestVMIDWins(t *testing.T) {
	t.Parallel()
	sha8 := "jkl12345"
	c := resolveTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		item1 := json.RawMessage(`{"vmid":30010,"template":1,"tags":"bosh-stemcell-cache;bosh-stemcell-sha-` + sha8 + `"}`)
		item2 := json.RawMessage(`{"vmid":30005,"template":1,"tags":"bosh-stemcell-cache;bosh-stemcell-sha-` + sha8 + `"}`)
		resp := sdknodes.ListQemuResponse{item1, item2}
		return &resp, nil
	})

	vmid, found, err := pve.ResolveTemplateVMIDForNode(context.Background(), c, "pve1", sha8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if vmid != 30005 {
		t.Errorf("vmid = %d; want 30005 (lowest)", vmid)
	}
}

// TestReplicaNodeTagForNode_Format checks the tag format is correct.
func TestReplicaNodeTagForNode_Format(t *testing.T) {
	t.Parallel()
	cases := []struct {
		node string
		want string
	}{
		{"pve1", "bosh-stemcell-node-pve1"},
		{"pve-node-2", "bosh-stemcell-node-pve-node-2"},
		{"PVE_HOST_3", "bosh-stemcell-node-pve-host-3"},
	}
	for _, tc := range cases {
		t.Run(tc.node, func(t *testing.T) {
			t.Parallel()
			got := pve.ReplicaNodeTagForNode(tc.node)
			if got != tc.want {
				t.Errorf("ReplicaNodeTagForNode(%q) = %q; want %q", tc.node, got, tc.want)
			}
		})
	}
}

// ============================================================================
// Cluster-scoped template lookup: FindTemplatesBySHATagCluster,
// FindTemplateByNameCluster, BuildTemplateNameWithSHA.
// ============================================================================

// cacheTag is the generation marker every cache template this CPI builds
// carries. Fixtures must include it (or a director-- ref tag) or the
// generation gate in listClusterQemuTemplates hides them, exactly as it hides
// a template left behind by a previous CPI generation.
const cacheTag = "bosh-stemcell-cache"

// clusterResourceItem is the in-test struct used to build ListResources fake
// responses for cluster-scoped template lookups. Mirrors the subset of
// /cluster/resources fields FindTemplatesBySHATagCluster and
// FindTemplateByNameCluster consume.
type clusterResourceItem struct {
	Type     string `json:"type"`
	Vmid     int64  `json:"vmid,omitempty"`
	Node     string `json:"node,omitempty"`
	Name     string `json:"name,omitempty"`
	Tags     string `json:"tags,omitempty"`
	Template *bool  `json:"template,omitempty"`
}

// makeClusterResourcesResponse serialises a slice of clusterResourceItem into
// the sdkcluster.ListResourcesResponse shape ([]json.RawMessage).
func makeClusterResourcesResponse(items []clusterResourceItem) *sdkcluster.ListResourcesResponse {
	resp := make(sdkcluster.ListResourcesResponse, 0, len(items))
	for _, item := range items {
		raw, err := json.Marshal(item)
		if err != nil {
			panic("makeClusterResourcesResponse: marshal: " + err.Error())
		}
		resp = append(resp, raw)
	}
	return &resp
}

// authFixtureAdapter translates the ListResources-shaped fixture fn into the
// authoritative per-node listing surface the cluster-scoped template lookups
// now read (ListClusterConfigNodes + per-node ListQemu). Cluster membership
// is the distinct node names of the fixture rows ("pve1" when the fixture
// yields none, so an empty fixture reads as an empty cluster rather than a
// failed enumeration); each node's qemu listing carries the fixture's qemu
// rows for that node. An fn error surfaces from the membership listing.
type authFixtureAdapter struct {
	fn func(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error)
}

func (a *authFixtureAdapter) rows(ctx context.Context) ([]clusterResourceItem, error) {
	if a.fn == nil {
		return nil, nil
	}
	resp, err := a.fn(ctx, &sdkcluster.ListResourcesParams{})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}
	var out []clusterResourceItem
	for _, raw := range *resp {
		var item clusterResourceItem
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if item.Node == "" {
			item.Node = "pve1"
		}
		out = append(out, item)
	}
	return out, nil
}

type authFixtureCluster struct {
	sdkcluster.Service
	ad *authFixtureAdapter
}

func (s *authFixtureCluster) ListConfigNodes(ctx context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	rows, err := s.ad.rows(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var nodes []string
	for _, r := range rows {
		if !seen[r.Node] {
			seen[r.Node] = true
			nodes = append(nodes, r.Node)
		}
	}
	if len(nodes) == 0 {
		nodes = []string{"pve1"}
	}
	resp := make(sdkcluster.ListConfigNodesResponse, 0, len(nodes))
	for _, n := range nodes {
		resp = append(resp, json.RawMessage(`{"name": "`+n+`"}`))
	}
	return &resp, nil
}

type authFixtureNodes struct {
	sdknodes.Service
	ad *authFixtureAdapter
}

func (s *authFixtureNodes) ListQemu(ctx context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	rows, err := s.ad.rows(ctx)
	if err != nil {
		return nil, err
	}
	resp := make(sdknodes.ListQemuResponse, 0)
	for _, r := range rows {
		// A per-node qemu listing carries QEMU guests only; lxc containers
		// and non-VM resource rows never appear in it by construction.
		if r.Type != "qemu" || r.Node != node {
			continue
		}
		raw, mErr := json.Marshal(r)
		if mErr != nil {
			continue
		}
		resp = append(resp, raw)
	}
	return &resp, nil
}

// newClusterTemplateClient builds a mockClient serving the fixture fn through
// the authoritative per-node listing surface (see authFixtureAdapter).
func newClusterTemplateClient(fn func(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error)) *mockClient {
	ad := &authFixtureAdapter{fn: fn}
	return &mockClient{clusterSvc: &authFixtureCluster{ad: ad}, nodesSvc: &authFixtureNodes{ad: ad}}
}

func TestFindTemplatesBySHATagCluster_MatchesAcrossNodes(t *testing.T) {
	t.Parallel()
	const sha8 = "ab12cd34"

	items := []clusterResourceItem{
		// Non-template VM on pve1: excluded (Template nil).
		{Type: "qemu", Vmid: 100, Node: "pve1", Tags: "bosh-stemcell-cache;bosh-stemcell-sha-" + sha8},
		// Primary cache template on pve1.
		{Type: "qemu", Vmid: 6042, Node: "pve1", Name: "bosh-stemcell-ubuntu-jammy-1.0-" + sha8, Tags: "bosh-stemcell-cache;bosh-stemcell-sha-" + sha8, Template: boolPtr(true)},
		// Per-node replica on pve2 — matched too (both are cache templates for the same sha).
		{Type: "qemu", Vmid: 6099, Node: "pve2", Name: "bosh-stemcell-ubuntu-jammy-1.0-" + sha8, Tags: "bosh-stemcell-cache;bosh-stemcell-sha-" + sha8 + ";bosh-stemcell-node-pve2", Template: boolPtr(true)},
		// LXC container carrying the same sha tag: excluded by type.
		{Type: "lxc", Vmid: 7000, Node: "pve1", Tags: "bosh-stemcell-cache;bosh-stemcell-sha-" + sha8, Template: boolPtr(true)},
		// Non-VM resource row (node/storage/pool): excluded by type + vmid==0.
		{Type: "node", Node: "pve3"},
		// Template on a different sha tag: excluded.
		{Type: "qemu", Vmid: 6100, Node: "pve1", Tags: "bosh-stemcell-cache;bosh-stemcell-sha-ffffffff", Template: boolPtr(true)},
	}
	c := newClusterTemplateClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return makeClusterResourcesResponse(items), nil
	})

	refs, err := pve.FindTemplatesBySHATagCluster(context.Background(), c, sha8)
	if err != nil {
		t.Fatalf("FindTemplatesBySHATagCluster: unexpected error: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("FindTemplatesBySHATagCluster: got %d refs, want 2: %+v", len(refs), refs)
	}
	if refs[0].VMID != 6042 || refs[0].Node != "pve1" {
		t.Errorf("FindTemplatesBySHATagCluster: refs[0] = %+v, want vmid=6042 node=pve1", refs[0])
	}
	if refs[1].VMID != 6099 || refs[1].Node != "pve2" {
		t.Errorf("FindTemplatesBySHATagCluster: refs[1] = %+v, want vmid=6099 node=pve2", refs[1])
	}
}

// TestFindTemplatesBySHATagCluster_PreGenerationTemplateInvisible is the
// cross-generation adoption guard: a template built by a PREVIOUS CPI
// generation carries the content sha tag (and bosh-cpi) but neither
// bosh-stemcell-cache nor any director-- ref tag. Adopting it would register a
// ref this CPI could then drop to zero and destroy a template a live older
// director still clones from, so it must be invisible to the lookup entirely.
func TestFindTemplatesBySHATagCluster_PreGenerationTemplateInvisible(t *testing.T) {
	t.Parallel()
	const sha8 = "cbc4cf34"

	items := []clusterResourceItem{
		// Previous-generation template: sha tag + bosh-cpi, no cache tag, no ref tag.
		{Type: "qemu", Vmid: 30169, Node: "pve1", Name: "bosh-stemcell-ubuntu-noble-1-383",
			Tags: "bosh-cpi;bosh-stemcell-sha-" + sha8, Template: boolPtr(true)},
	}
	c := newClusterTemplateClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return makeClusterResourcesResponse(items), nil
	})

	refs, err := pve.FindTemplatesBySHATagCluster(context.Background(), c, sha8)
	if err != nil {
		t.Fatalf("FindTemplatesBySHATagCluster: unexpected error: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("FindTemplatesBySHATagCluster: pre-generation template must be invisible, got %+v", refs)
	}
}

// TestFindTemplatesBySHATagCluster_RefTaggedOnlyStillVisible covers the second
// eligibility arm: a template this CPI has already adopted carries a
// director-- ref tag. It must stay visible even without the cache tag, or its
// ref set becomes unreachable — refcounting would never converge and the
// template could never be cleaned up.
func TestFindTemplatesBySHATagCluster_RefTaggedOnlyStillVisible(t *testing.T) {
	t.Parallel()
	const sha8 = "cbc4cf34"

	items := []clusterResourceItem{
		{Type: "qemu", Vmid: 30169, Node: "pve1", Name: "bosh-stemcell-ubuntu-noble-1-383",
			Tags: "bosh-cpi;bosh-stemcell-sha-" + sha8 + ";director--abc123", Template: boolPtr(true)},
	}
	c := newClusterTemplateClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return makeClusterResourcesResponse(items), nil
	})

	refs, err := pve.FindTemplatesBySHATagCluster(context.Background(), c, sha8)
	if err != nil {
		t.Fatalf("FindTemplatesBySHATagCluster: unexpected error: %v", err)
	}
	if len(refs) != 1 || refs[0].VMID != 30169 {
		t.Fatalf("FindTemplatesBySHATagCluster: adopted (ref-tagged) template must stay visible, got %+v", refs)
	}
}

// TestFindTemplatesBySHATagCluster_BareDirectorPrefixNotAMarker guards the
// eligibility predicate against a degenerate "director--" token with no UUID:
// an empty ref names no director and must not confer eligibility.
func TestFindTemplatesBySHATagCluster_BareDirectorPrefixNotAMarker(t *testing.T) {
	t.Parallel()
	const sha8 = "cbc4cf34"

	items := []clusterResourceItem{
		{Type: "qemu", Vmid: 30169, Node: "pve1", Tags: "bosh-stemcell-sha-" + sha8 + ";director--", Template: boolPtr(true)},
	}
	c := newClusterTemplateClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return makeClusterResourcesResponse(items), nil
	})

	refs, err := pve.FindTemplatesBySHATagCluster(context.Background(), c, sha8)
	if err != nil {
		t.Fatalf("FindTemplatesBySHATagCluster: unexpected error: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("FindTemplatesBySHATagCluster: bare director-- prefix must not confer eligibility, got %+v", refs)
	}
}

// TestResolveTemplateVMIDForNode_PreGenerationTemplateInvisible applies the
// same guard to the node-scoped resolver, which reads GET /nodes/<node>/qemu
// rather than the cluster index and would otherwise re-expose a
// pre-generation template to the placement scorer and the create_vm clone path.
func TestResolveTemplateVMIDForNode_PreGenerationTemplateInvisible(t *testing.T) {
	t.Parallel()
	const sha8 = "cbc4cf34"
	c := resolveTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponseRaw(
			`{"vmid":30169,"template":1,"tags":"bosh-cpi;bosh-stemcell-sha-` + sha8 + `"}`,
		), nil
	})

	_, found, err := pve.ResolveTemplateVMIDForNode(context.Background(), c, "pve1", sha8)
	if err != nil {
		t.Fatalf("ResolveTemplateVMIDForNode: unexpected error: %v", err)
	}
	if found {
		t.Fatal("ResolveTemplateVMIDForNode: pre-generation template must be invisible")
	}
}

func TestFindTemplatesBySHATagCluster_NoMatch(t *testing.T) {
	t.Parallel()
	items := []clusterResourceItem{
		{Type: "qemu", Vmid: 100, Node: "pve1", Tags: "bosh-stemcell-cache;bosh-stemcell-sha-ffffffff", Template: boolPtr(true)},
	}
	c := newClusterTemplateClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return makeClusterResourcesResponse(items), nil
	})

	refs, err := pve.FindTemplatesBySHATagCluster(context.Background(), c, "ab12cd34")
	if err != nil {
		t.Fatalf("FindTemplatesBySHATagCluster: unexpected error: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("FindTemplatesBySHATagCluster: got %d refs, want 0: %+v", len(refs), refs)
	}
}

func TestFindTemplatesBySHATagCluster_EmptySHA8_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	c := newClusterTemplateClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		t.Fatal("ListResources must not be called for an empty sha8")
		return nil, nil
	})

	refs, err := pve.FindTemplatesBySHATagCluster(context.Background(), c, "")
	if err != nil {
		t.Fatalf("FindTemplatesBySHATagCluster: unexpected error: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("FindTemplatesBySHATagCluster: got %d refs, want 0", len(refs))
	}
}

func TestFindTemplatesBySHATagCluster_APIError(t *testing.T) {
	t.Parallel()
	c := newClusterTemplateClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return nil, errors.New("connection refused")
	})

	_, err := pve.FindTemplatesBySHATagCluster(context.Background(), c, "ab12cd34")
	if err == nil {
		t.Fatal("FindTemplatesBySHATagCluster: expected error from API failure, got nil")
	}
	if !strings.Contains(err.Error(), "FindTemplatesBySHATagCluster") {
		t.Errorf("FindTemplatesBySHATagCluster: error missing context label: %v", err)
	}
}

func TestFindTemplatesBySHATagCluster_NilContext(t *testing.T) {
	t.Parallel()
	c := newClusterTemplateClient(nil)
	//nolint:staticcheck // intentional nil ctx for validation test
	//lint:ignore SA1012 intentional nil ctx for validation test
	_, err := pve.FindTemplatesBySHATagCluster(nil, c, "ab12cd34")
	if err == nil {
		t.Fatal("FindTemplatesBySHATagCluster: expected error for nil ctx, got nil")
	}
}

func TestFindTemplatesBySHATagCluster_NilClient(t *testing.T) {
	t.Parallel()
	_, err := pve.FindTemplatesBySHATagCluster(context.Background(), nil, "ab12cd34")
	if err == nil {
		t.Fatal("FindTemplatesBySHATagCluster: expected error for nil client, got nil")
	}
}

func TestFindTemplateByNameCluster_Found(t *testing.T) {
	t.Parallel()
	const wantName = "bosh-stemcell-ubuntu-jammy-1.0-ab12cd34"

	items := []clusterResourceItem{
		{Type: "qemu", Vmid: 100, Node: "pve1", Name: "unrelated", Tags: cacheTag, Template: boolPtr(true)},
		{Type: "qemu", Vmid: 6042, Node: "pve1", Name: wantName, Tags: cacheTag, Template: boolPtr(true)},
		// Same name but not a template: excluded.
		{Type: "qemu", Vmid: 6043, Node: "pve2", Name: wantName, Tags: cacheTag},
	}
	c := newClusterTemplateClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return makeClusterResourcesResponse(items), nil
	})

	refs, err := pve.FindTemplateByNameCluster(context.Background(), c, wantName)
	if err != nil {
		t.Fatalf("FindTemplateByNameCluster: unexpected error: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("FindTemplateByNameCluster: got %d refs, want 1: %+v", len(refs), refs)
	}
	if refs[0].VMID != 6042 {
		t.Errorf("FindTemplateByNameCluster: refs[0].VMID = %d, want 6042", refs[0].VMID)
	}
}

func TestFindTemplateByNameCluster_MultiMatch_SortedByVMID(t *testing.T) {
	t.Parallel()
	const name = "bosh-stemcell-ubuntu-jammy-1.0-ab12cd34"

	items := []clusterResourceItem{
		{Type: "qemu", Vmid: 6099, Node: "pve2", Name: name, Tags: cacheTag, Template: boolPtr(true)},
		{Type: "qemu", Vmid: 6042, Node: "pve1", Name: name, Tags: cacheTag, Template: boolPtr(true)},
	}
	c := newClusterTemplateClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return makeClusterResourcesResponse(items), nil
	})

	refs, err := pve.FindTemplateByNameCluster(context.Background(), c, name)
	if err != nil {
		t.Fatalf("FindTemplateByNameCluster: unexpected error: %v", err)
	}
	if len(refs) != 2 || refs[0].VMID != 6042 || refs[1].VMID != 6099 {
		t.Fatalf("FindTemplateByNameCluster: got %+v, want sorted [6042, 6099]", refs)
	}
}

func TestFindTemplateByNameCluster_NoMatch(t *testing.T) {
	t.Parallel()
	c := newClusterTemplateClient(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return makeClusterResourcesResponse(nil), nil
	})

	refs, err := pve.FindTemplateByNameCluster(context.Background(), c, "does-not-exist")
	if err != nil {
		t.Fatalf("FindTemplateByNameCluster: unexpected error: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("FindTemplateByNameCluster: got %d refs, want 0", len(refs))
	}
}

func TestFindTemplateByNameCluster_EmptyName(t *testing.T) {
	t.Parallel()
	c := newClusterTemplateClient(nil)
	_, err := pve.FindTemplateByNameCluster(context.Background(), c, "")
	if err == nil {
		t.Fatal("FindTemplateByNameCluster: expected error for empty name, got nil")
	}
}

func TestFindTemplateByNameCluster_NilContext(t *testing.T) {
	t.Parallel()
	c := newClusterTemplateClient(nil)
	//nolint:staticcheck // intentional nil ctx for validation test
	//lint:ignore SA1012 intentional nil ctx for validation test
	_, err := pve.FindTemplateByNameCluster(nil, c, "some-name")
	if err == nil {
		t.Fatal("FindTemplateByNameCluster: expected error for nil ctx, got nil")
	}
}

func TestFindTemplateByNameCluster_NilClient(t *testing.T) {
	t.Parallel()
	_, err := pve.FindTemplateByNameCluster(context.Background(), nil, "some-name")
	if err == nil {
		t.Fatal("FindTemplateByNameCluster: expected error for nil client, got nil")
	}
}

// ---- BuildTemplateNameWithSHA tests ----

func TestBuildTemplateNameWithSHA_AppendsAfterTruncation(t *testing.T) {
	t.Parallel()
	got := pve.BuildTemplateNameWithSHA("ubuntu-jammy", "1.0", "ab12cd34")
	want := "bosh-stemcell-ubuntu-jammy-1-0-ab12cd34"
	if got != want {
		t.Errorf("BuildTemplateNameWithSHA: got %q, want %q", got, want)
	}
}

func TestBuildTemplateNameWithSHA_EmptySHA8_UsesUnknownPlaceholder(t *testing.T) {
	t.Parallel()
	got := pve.BuildTemplateNameWithSHA("ubuntu-jammy", "1.0", "")
	want := "bosh-stemcell-ubuntu-jammy-1-0-00000000"
	if got != want {
		t.Errorf("BuildTemplateNameWithSHA: got %q, want %q", got, want)
	}
}

// TestBuildTemplateNameWithSHA_TruncationCollision_Disambiguated pins the
// bug fix: two stemcells whose sanitized name+version are IDENTICAL up to
// the 200-char BuildTemplateName truncation boundary (so BuildTemplateName
// alone would alias them to the same template name) must still produce
// distinct names once the differing sha8 is appended after truncation.
func TestBuildTemplateNameWithSHA_TruncationCollision_Disambiguated(t *testing.T) {
	t.Parallel()

	// A name long enough that BuildTemplateName truncates at 200 chars
	// regardless of what a short "version" suffix is — the two versions
	// below differ only in the region truncation discards.
	longName := strings.Repeat("a", 250)

	nameA := pve.BuildTemplateNameWithSHA(longName, "version-one", "11111111")
	nameB := pve.BuildTemplateNameWithSHA(longName, "version-two", "22222222")

	// Precondition the test is actually exercising the collision: the
	// pre-sha base (BuildTemplateName output) must be identical for both —
	// the truncation cap swallows the version difference entirely.
	baseA := pve.BuildTemplateName(longName, "version-one")
	baseB := pve.BuildTemplateName(longName, "version-two")
	if baseA != baseB {
		t.Fatalf("test precondition failed: BuildTemplateName bases differ (%q vs %q) — truncation collision not exercised", baseA, baseB)
	}

	if nameA == nameB {
		t.Errorf("BuildTemplateNameWithSHA: truncation-collision names must differ once sha8 is appended; both = %q", nameA)
	}
	if !strings.HasSuffix(nameA, "-11111111") {
		t.Errorf("BuildTemplateNameWithSHA: nameA = %q, want suffix -11111111", nameA)
	}
	if !strings.HasSuffix(nameB, "-22222222") {
		t.Errorf("BuildTemplateNameWithSHA: nameB = %q, want suffix -22222222", nameB)
	}
}

func TestBuildTemplateNameWithSHA_InvalidCharsSanitized(t *testing.T) {
	t.Parallel()
	// sha8 is expected to be lowercase hex; a wildly out-of-band value must
	// still yield a DNS-safe name via the same sanitizer BuildTemplateName uses.
	got := pve.BuildTemplateNameWithSHA("ubuntu-jammy", "1.0", "AB12_CD!34")
	if strings.ContainsAny(got, "_!") {
		t.Errorf("BuildTemplateNameWithSHA: result contains DNS-unsafe characters: %q", got)
	}
}

// ============================================================
// FindTemplatesBySHATagNode tests
// ============================================================

// TestFindTemplatesBySHATagNode_MatchesPrimariesAndReplicas verifies the
// node-scoped scan returns every generation-gated sha match on the node,
// primary and replica alike, in ascending-VMID order with Node stamped.
func TestFindTemplatesBySHATagNode_MatchesPrimariesAndReplicas(t *testing.T) {
	t.Parallel()
	sha8 := "abc12345"
	nodeTag := pve.ReplicaNodeTagForNode("pve1")
	c := resolveTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponseRaw(
			// Replica listed first: ordering must come from VMID sorting.
			`{"vmid":30020,"template":1,"tags":"bosh-stemcell-cache;bosh-stemcell-sha-`+sha8+`;`+nodeTag+`"}`,
			`{"vmid":30010,"template":1,"tags":"bosh-stemcell-cache;bosh-stemcell-sha-`+sha8+`"}`,
			// Different sha: excluded.
			`{"vmid":30030,"template":1,"tags":"bosh-stemcell-cache;bosh-stemcell-sha-ffffffff"}`,
			// Not a template: excluded.
			`{"vmid":30040,"tags":"bosh-stemcell-cache;bosh-stemcell-sha-`+sha8+`"}`,
			// No generation marker: a previous CPI generation's template, excluded.
			`{"vmid":30050,"template":1,"tags":"bosh-stemcell-sha-`+sha8+`"}`,
		), nil
	})

	refs, err := pve.FindTemplatesBySHATagNode(context.Background(), c, "pve1", sha8)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("len(refs) = %d; want 2 (primary + replica), got %+v", len(refs), refs)
	}
	if refs[0].VMID != 30010 || refs[1].VMID != 30020 {
		t.Errorf("refs not in ascending-VMID order: %+v", refs)
	}
	for _, r := range refs {
		if r.Node != "pve1" {
			t.Errorf("ref %d Node = %q; want %q (stamped from the probed node)", r.VMID, r.Node, "pve1")
		}
	}
	if refs[0].IsReplica() {
		t.Error("vmid 30010 carries no node tag and must not classify as a replica")
	}
	if !refs[1].IsReplica() {
		t.Error("vmid 30020 carries a node tag and must classify as a replica")
	}
}

// TestFindTemplatesBySHATagNode_EmptyInputs verifies the guard clauses.
func TestFindTemplatesBySHATagNode_EmptyInputs(t *testing.T) {
	t.Parallel()
	c := resolveTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		t.Fatal("ListQemu must not be called for an empty sha8")
		return nil, nil
	})

	refs, err := pve.FindTemplatesBySHATagNode(context.Background(), c, "pve1", "")
	if err != nil || refs != nil {
		t.Errorf("empty sha8: got (%v, %v); want (nil, nil)", refs, err)
	}
	if _, err := pve.FindTemplatesBySHATagNode(context.Background(), c, "", "abc12345"); err == nil {
		t.Error("empty node must error")
	}
}

// ListStatus reports no offline members; the fixture cluster is fully online.
func (s *authFixtureCluster) ListStatus(context.Context) (*sdkcluster.ListStatusResponse, error) {
	empty := sdkcluster.ListStatusResponse{}
	return &empty, nil
}

// ListNodes reports an empty node list; the standalone-membership
// fallback then surfaces the original corosync answer unchanged.
func (s *templateNodesService) ListNodes(context.Context) (*sdknodes.ListNodesResponse, error) {
	empty := sdknodes.ListNodesResponse{}
	return &empty, nil
}

// ListNodes reports an empty node list; the standalone-membership
// fallback then surfaces the original corosync answer unchanged.
func (s *authFixtureNodes) ListNodes(context.Context) (*sdknodes.ListNodesResponse, error) {
	empty := sdknodes.ListNodesResponse{}
	return &empty, nil
}
