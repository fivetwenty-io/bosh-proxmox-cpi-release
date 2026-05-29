package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
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

// strPtr returns a pointer to the supplied string value.
func strPtr(s string) *string { return &s }

// listQemuItem is the in-test struct used to build ListQemu fake responses.
// It mirrors the subset of fields consumed by FindTemplateByName and
// FindTemplateBySHATag — the remainder of the per-VM JSON is irrelevant.
type listQemuItem struct {
	Vmid     int64   `json:"vmid"`
	Name     *string `json:"name,omitempty"`
	Tags     *string `json:"tags,omitempty"`
	Template *bool   `json:"template,omitempty"`
}

// makeListQemuResponse serialises a slice of listQemuItem into the
// sdknodes.ListQemuResponse shape ([]json.RawMessage).
func makeListQemuResponse(items []listQemuItem) *sdknodes.ListQemuResponse {
	resp := make(sdknodes.ListQemuResponse, 0, len(items))
	for _, item := range items {
		raw, err := json.Marshal(item)
		if err != nil {
			panic("makeListQemuResponse: marshal: " + err.Error())
		}
		resp = append(resp, raw)
	}
	return &resp
}

// newFindTemplateClient builds a mockClient whose Nodes().ListQemu is served
// by fn and all other node methods use the default templateNodesService
// (which panics on unimplemented methods).
func newFindTemplateClient(fn func(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error)) *mockClient {
	return newTemplateClient(&templateNodesService{listQemuFn: fn})
}

// ---- FindTemplateByName tests ----

func TestFindTemplateByName_Found(t *testing.T) {
	t.Parallel()
	const wantVMID = int64(6042)
	const wantName = "bosh-stemcell-ubuntu-jammy-1.234"

	items := []listQemuItem{
		{Vmid: 101, Name: strPtr("some-other-vm"), Template: boolPtr(false)},
		{Vmid: wantVMID, Name: strPtr(wantName), Template: boolPtr(true)},
	}
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponse(items), nil
	})

	vmid, found, err := pve.FindTemplateByName(context.Background(), c, "pve1", wantName)
	if err != nil {
		t.Fatalf("FindTemplateByName: unexpected error: %v", err)
	}
	if !found {
		t.Fatal("FindTemplateByName: expected found=true, got false")
	}
	if vmid != wantVMID {
		t.Errorf("FindTemplateByName: vmid = %d, want %d", vmid, wantVMID)
	}
}

func TestFindTemplateByName_NotFound(t *testing.T) {
	t.Parallel()

	items := []listQemuItem{
		{Vmid: 100, Name: strPtr("bosh-stemcell-other-name-1.0"), Template: boolPtr(true)},
	}
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponse(items), nil
	})

	vmid, found, err := pve.FindTemplateByName(context.Background(), c, "pve1", "bosh-stemcell-ubuntu-jammy-1.234")
	if err != nil {
		t.Fatalf("FindTemplateByName: unexpected error: %v", err)
	}
	if found {
		t.Errorf("FindTemplateByName: expected found=false, got vmid=%d", vmid)
	}
	if vmid != 0 {
		t.Errorf("FindTemplateByName: expected vmid=0, got %d", vmid)
	}
}

func TestFindTemplateByName_NotFound_NonTemplateVMMatchesName(t *testing.T) {
	t.Parallel()
	// A VM whose name matches but Template==false must NOT be returned.

	items := []listQemuItem{
		{Vmid: 200, Name: strPtr("bosh-stemcell-ubuntu-jammy-1.234"), Template: boolPtr(false)},
	}
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponse(items), nil
	})

	vmid, found, err := pve.FindTemplateByName(context.Background(), c, "pve1", "bosh-stemcell-ubuntu-jammy-1.234")
	if err != nil {
		t.Fatalf("FindTemplateByName: unexpected error: %v", err)
	}
	if found {
		t.Errorf("FindTemplateByName: non-template VM with matching name must not match; vmid=%d", vmid)
	}
}

func TestFindTemplateByName_NotFound_TemplateFlagAbsent(t *testing.T) {
	t.Parallel()
	// Template field omitted entirely (not-yet-frozen VM) — must NOT match.

	items := []listQemuItem{
		// No Template field set → nil pointer in struct → omitempty drops it.
		{Vmid: 300, Name: strPtr("bosh-stemcell-ubuntu-jammy-1.234")},
	}
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponse(items), nil
	})

	_, found, err := pve.FindTemplateByName(context.Background(), c, "pve1", "bosh-stemcell-ubuntu-jammy-1.234")
	if err != nil {
		t.Fatalf("FindTemplateByName: unexpected error: %v", err)
	}
	if found {
		t.Error("FindTemplateByName: VM with absent Template field must not match")
	}
}

func TestFindTemplateByName_MultiMatch_ReturnsLowestVMID(t *testing.T) {
	t.Parallel()
	// Two templates with same name → return the lower VMID.
	// This pins the deterministic multi-match behavior.
	const matchName = "bosh-stemcell-ubuntu-jammy-1.234"

	items := []listQemuItem{
		{Vmid: 6100, Name: strPtr(matchName), Template: boolPtr(true)},
		{Vmid: 6050, Name: strPtr(matchName), Template: boolPtr(true)},
		{Vmid: 6200, Name: strPtr(matchName), Template: boolPtr(true)},
	}
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponse(items), nil
	})

	vmid, found, err := pve.FindTemplateByName(context.Background(), c, "pve1", matchName)
	if err != nil {
		t.Fatalf("FindTemplateByName: unexpected error: %v", err)
	}
	if !found {
		t.Fatal("FindTemplateByName: expected found=true")
	}
	if vmid != 6050 {
		t.Errorf("FindTemplateByName: multi-match: expected lowest vmid=6050, got %d", vmid)
	}
}

func TestFindTemplateByName_NilResponse(t *testing.T) {
	t.Parallel()
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return nil, nil
	})

	vmid, found, err := pve.FindTemplateByName(context.Background(), c, "pve1", "bosh-stemcell-ubuntu-jammy-1.234")
	if err != nil {
		t.Fatalf("FindTemplateByName: nil response must not error: %v", err)
	}
	if found || vmid != 0 {
		t.Errorf("FindTemplateByName: nil response: expected (0,false), got (%d,%v)", vmid, found)
	}
}

func TestFindTemplateByName_EmptyList(t *testing.T) {
	t.Parallel()
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponse(nil), nil
	})

	vmid, found, err := pve.FindTemplateByName(context.Background(), c, "pve1", "bosh-stemcell-ubuntu-jammy-1.234")
	if err != nil {
		t.Fatalf("FindTemplateByName: empty list must not error: %v", err)
	}
	if found || vmid != 0 {
		t.Errorf("FindTemplateByName: empty list: expected (0,false), got (%d,%v)", vmid, found)
	}
}

func TestFindTemplateByName_APIError(t *testing.T) {
	t.Parallel()
	apiErr := errors.New("PVE connection refused")
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return nil, apiErr
	})

	_, _, err := pve.FindTemplateByName(context.Background(), c, "pve1", "bosh-stemcell-ubuntu-jammy-1.234")
	if err == nil {
		t.Fatal("FindTemplateByName: expected error from API, got nil")
	}
	if !strings.Contains(err.Error(), "FindTemplateByName") {
		t.Errorf("FindTemplateByName: error missing context label: %v", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("FindTemplateByName: error missing original message: %v", err)
	}
}

func TestFindTemplateByName_NilContext(t *testing.T) {
	t.Parallel()
	c := newFindTemplateClient(nil)
	//nolint:staticcheck // intentional nil ctx for validation test
	_, _, err := pve.FindTemplateByName(nil, c, "pve1", "bosh-stemcell-ubuntu-jammy-1.234")
	if err == nil {
		t.Fatal("FindTemplateByName: expected error for nil ctx")
	}
}

func TestFindTemplateByName_NilClient(t *testing.T) {
	t.Parallel()
	_, _, err := pve.FindTemplateByName(context.Background(), nil, "pve1", "bosh-stemcell-ubuntu-jammy-1.234")
	if err == nil {
		t.Fatal("FindTemplateByName: expected error for nil client")
	}
}

func TestFindTemplateByName_EmptyNode(t *testing.T) {
	t.Parallel()
	c := newFindTemplateClient(nil)
	_, _, err := pve.FindTemplateByName(context.Background(), c, "", "bosh-stemcell-ubuntu-jammy-1.234")
	if err == nil {
		t.Fatal("FindTemplateByName: expected error for empty node")
	}
}

func TestFindTemplateByName_EmptyName(t *testing.T) {
	t.Parallel()
	c := newFindTemplateClient(nil)
	_, _, err := pve.FindTemplateByName(context.Background(), c, "pve1", "")
	if err == nil {
		t.Fatal("FindTemplateByName: expected error for empty name")
	}
}

// ---- FindTemplateBySHATag tests ----

func TestFindTemplateBySHATag_Found(t *testing.T) {
	t.Parallel()
	const sha8 = "ab12cd34"
	const wantVMID = int64(6042)

	items := []listQemuItem{
		{Vmid: 101, Tags: strPtr("some-tag"), Template: boolPtr(true)},
		{Vmid: wantVMID, Tags: strPtr("env--prod;bosh-stemcell-sha-" + sha8), Template: boolPtr(true)},
	}
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponse(items), nil
	})

	vmid, found, err := pve.FindTemplateBySHATag(context.Background(), c, "pve1", sha8)
	if err != nil {
		t.Fatalf("FindTemplateBySHATag: unexpected error: %v", err)
	}
	if !found {
		t.Fatal("FindTemplateBySHATag: expected found=true, got false")
	}
	if vmid != wantVMID {
		t.Errorf("FindTemplateBySHATag: vmid = %d, want %d", vmid, wantVMID)
	}
}

func TestFindTemplateBySHATag_NotFound(t *testing.T) {
	t.Parallel()
	items := []listQemuItem{
		{Vmid: 100, Tags: strPtr("bosh-stemcell-sha-xxxxxxxx"), Template: boolPtr(true)},
	}
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponse(items), nil
	})

	vmid, found, err := pve.FindTemplateBySHATag(context.Background(), c, "pve1", "ab12cd34")
	if err != nil {
		t.Fatalf("FindTemplateBySHATag: unexpected error: %v", err)
	}
	if found {
		t.Errorf("FindTemplateBySHATag: expected found=false, got vmid=%d", vmid)
	}
}

func TestFindTemplateBySHATag_WrongTagPrefix_NotMatched(t *testing.T) {
	t.Parallel()
	// sha8 "abc12345" must NOT match token "bosh-stemcell-sha-abc123456"
	// (longer token — not a prefix collision guard, but exact-token requirement).
	// This pins the token-exact (not substring) matching behavior.
	const sha8 = "abc12345"

	items := []listQemuItem{
		// Token "bosh-stemcell-sha-abc12345x" differs from "bosh-stemcell-sha-abc12345".
		{Vmid: 200, Tags: strPtr("bosh-stemcell-sha-abc12345x"), Template: boolPtr(true)},
	}
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponse(items), nil
	})

	vmid, found, err := pve.FindTemplateBySHATag(context.Background(), c, "pve1", sha8)
	if err != nil {
		t.Fatalf("FindTemplateBySHATag: unexpected error: %v", err)
	}
	if found {
		t.Errorf("FindTemplateBySHATag: wrong-prefix tag must not match; vmid=%d", vmid)
	}
}

func TestFindTemplateBySHATag_NonTemplateNotMatched(t *testing.T) {
	t.Parallel()
	const sha8 = "ab12cd34"

	items := []listQemuItem{
		// Matching tag but Template==false — must be skipped.
		{Vmid: 300, Tags: strPtr("bosh-stemcell-sha-" + sha8), Template: boolPtr(false)},
	}
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponse(items), nil
	})

	vmid, found, err := pve.FindTemplateBySHATag(context.Background(), c, "pve1", sha8)
	if err != nil {
		t.Fatalf("FindTemplateBySHATag: unexpected error: %v", err)
	}
	if found {
		t.Errorf("FindTemplateBySHATag: non-template must not match; vmid=%d", vmid)
	}
}

func TestFindTemplateBySHATag_CommaSeparatedTags(t *testing.T) {
	t.Parallel()
	// PVE also accepts comma-delimited tags; verify normalization works.
	const sha8 = "ab12cd34"
	const wantVMID = int64(6043)

	items := []listQemuItem{
		{Vmid: wantVMID, Tags: strPtr("env--prod,bosh-stemcell-sha-" + sha8), Template: boolPtr(true)},
	}
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponse(items), nil
	})

	vmid, found, err := pve.FindTemplateBySHATag(context.Background(), c, "pve1", sha8)
	if err != nil {
		t.Fatalf("FindTemplateBySHATag: unexpected error: %v", err)
	}
	if !found {
		t.Fatal("FindTemplateBySHATag: comma-delimited tags must match")
	}
	if vmid != wantVMID {
		t.Errorf("FindTemplateBySHATag: vmid = %d, want %d", vmid, wantVMID)
	}
}

func TestFindTemplateBySHATag_MultiMatch_ReturnsLowestVMID(t *testing.T) {
	t.Parallel()
	const sha8 = "ab12cd34"
	const tag = "bosh-stemcell-sha-" + sha8

	items := []listQemuItem{
		{Vmid: 6100, Tags: strPtr(tag), Template: boolPtr(true)},
		{Vmid: 6050, Tags: strPtr(tag), Template: boolPtr(true)},
		{Vmid: 6200, Tags: strPtr(tag), Template: boolPtr(true)},
	}
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponse(items), nil
	})

	vmid, found, err := pve.FindTemplateBySHATag(context.Background(), c, "pve1", sha8)
	if err != nil {
		t.Fatalf("FindTemplateBySHATag: unexpected error: %v", err)
	}
	if !found {
		t.Fatal("FindTemplateBySHATag: expected found=true")
	}
	if vmid != 6050 {
		t.Errorf("FindTemplateBySHATag: multi-match: expected lowest vmid=6050, got %d", vmid)
	}
}

func TestFindTemplateBySHATag_EmptySHA8_ReturnsFalse(t *testing.T) {
	t.Parallel()
	// Empty sha8 → (0,false,nil) without calling the API.
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		t.Error("FindTemplateBySHATag: ListQemu must not be called for empty sha8")
		return nil, nil
	})

	vmid, found, err := pve.FindTemplateBySHATag(context.Background(), c, "pve1", "")
	if err != nil {
		t.Fatalf("FindTemplateBySHATag: empty sha8 must not error: %v", err)
	}
	if found || vmid != 0 {
		t.Errorf("FindTemplateBySHATag: empty sha8: expected (0,false), got (%d,%v)", vmid, found)
	}
}

func TestFindTemplateBySHATag_NilResponse(t *testing.T) {
	t.Parallel()
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return nil, nil
	})

	vmid, found, err := pve.FindTemplateBySHATag(context.Background(), c, "pve1", "ab12cd34")
	if err != nil {
		t.Fatalf("FindTemplateBySHATag: nil response must not error: %v", err)
	}
	if found || vmid != 0 {
		t.Errorf("FindTemplateBySHATag: nil response: expected (0,false), got (%d,%v)", vmid, found)
	}
}

func TestFindTemplateBySHATag_APIError(t *testing.T) {
	t.Parallel()
	apiErr := errors.New("PVE timeout")
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return nil, apiErr
	})

	_, _, err := pve.FindTemplateBySHATag(context.Background(), c, "pve1", "ab12cd34")
	if err == nil {
		t.Fatal("FindTemplateBySHATag: expected error from API, got nil")
	}
	if !strings.Contains(err.Error(), "FindTemplateBySHATag") {
		t.Errorf("FindTemplateBySHATag: error missing context label: %v", err)
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("FindTemplateBySHATag: error missing original message: %v", err)
	}
}

func TestFindTemplateBySHATag_NilContext(t *testing.T) {
	t.Parallel()
	c := newFindTemplateClient(nil)
	//nolint:staticcheck // intentional nil ctx for validation test
	_, _, err := pve.FindTemplateBySHATag(nil, c, "pve1", "ab12cd34")
	if err == nil {
		t.Fatal("FindTemplateBySHATag: expected error for nil ctx")
	}
}

func TestFindTemplateBySHATag_NilClient(t *testing.T) {
	t.Parallel()
	_, _, err := pve.FindTemplateBySHATag(context.Background(), nil, "pve1", "ab12cd34")
	if err == nil {
		t.Fatal("FindTemplateBySHATag: expected error for nil client")
	}
}

func TestFindTemplateBySHATag_EmptyNode(t *testing.T) {
	t.Parallel()
	c := newFindTemplateClient(nil)
	_, _, err := pve.FindTemplateBySHATag(context.Background(), c, "", "ab12cd34")
	if err == nil {
		t.Fatal("FindTemplateBySHATag: expected error for empty node")
	}
}

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

func TestFindTemplateByName_MatchesIntegerTemplateFlag(t *testing.T) {
	t.Parallel()
	const wantVMID = int64(30231)
	const wantName = "bosh-stemcell-bosh-openstack-kvm-ubuntu-noble-1-364"

	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponseRaw(
			`{"vmid":101,"name":"some-vm"}`,
			`{"vmid":30231,"name":"`+wantName+`","template":1}`,
		), nil
	})

	vmid, found, err := pve.FindTemplateByName(context.Background(), c, "pve", wantName)
	if err != nil {
		t.Fatalf("FindTemplateByName: unexpected error: %v", err)
	}
	if !found {
		t.Fatal("FindTemplateByName: expected found=true for integer template flag, got false")
	}
	if vmid != wantVMID {
		t.Errorf("FindTemplateByName: vmid = %d, want %d", vmid, wantVMID)
	}
}

func TestFindTemplateBySHATag_MatchesIntegerTemplateFlag(t *testing.T) {
	t.Parallel()
	const sha8 = "891b3b74"
	const wantVMID = int64(30231)

	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponseRaw(
			`{"vmid":30231,"name":"bosh-stemcell-x","tags":"bosh-stemcell-sha-` + sha8 + `","template":1}`,
		), nil
	})

	vmid, found, err := pve.FindTemplateBySHATag(context.Background(), c, "pve", sha8)
	if err != nil {
		t.Fatalf("FindTemplateBySHATag: unexpected error: %v", err)
	}
	if !found {
		t.Fatal("FindTemplateBySHATag: expected found=true for integer template flag, got false")
	}
	if vmid != wantVMID {
		t.Errorf("FindTemplateBySHATag: vmid = %d, want %d", vmid, wantVMID)
	}
}

func TestFindTemplateByName_IntegerZeroTemplateFlagNotMatched(t *testing.T) {
	t.Parallel()
	const name = "bosh-stemcell-x"
	c := newFindTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		// template:0 is a regular VM that happens to share the name; must NOT match.
		return makeListQemuResponseRaw(
			`{"vmid":42,"name":"` + name + `","template":0}`,
		), nil
	})

	_, found, err := pve.FindTemplateByName(context.Background(), c, "pve", name)
	if err != nil {
		t.Fatalf("FindTemplateByName: unexpected error: %v", err)
	}
	if found {
		t.Fatal("FindTemplateByName: template:0 must not match")
	}
}
