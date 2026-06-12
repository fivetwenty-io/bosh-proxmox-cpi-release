package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcloudinit "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// ---------------------------------------------------------------------------
// local stubs (package-internal tests cannot use handlers_test mocks)
// ---------------------------------------------------------------------------

// pciNodesStub is a minimal nodes.Service stub that overrides only the methods
// exercised by PCI passthrough helpers.
type pciNodesStub struct {
	sdknodes.Service   // nil embed — panics on any other method call
	updateQemuConfigFn func(ctx context.Context, node string, vmid string, params *sdknodes.UpdateQemuConfigParams) error
	listHardwarePciFn  func(ctx context.Context, node string, params *sdknodes.ListHardwarePciParams) (*sdknodes.ListHardwarePciResponse, error)
}

func (s *pciNodesStub) UpdateQemuConfig(ctx context.Context, node string, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
	if s.updateQemuConfigFn != nil {
		return s.updateQemuConfigFn(ctx, node, vmid, params)
	}
	return nil
}

func (s *pciNodesStub) ListHardwarePci(ctx context.Context, node string, params *sdknodes.ListHardwarePciParams) (*sdknodes.ListHardwarePciResponse, error) {
	if s.listHardwarePciFn != nil {
		return s.listHardwarePciFn(ctx, node, params)
	}
	empty := sdknodes.ListHardwarePciResponse{}
	return &empty, nil
}

// pciPVEClient wraps a pciNodesStub into a minimal pve.Client.
type pciPVEClient struct {
	nodesSvc sdknodes.Service
}

var _ pve.Client = (*pciPVEClient)(nil)

func (c *pciPVEClient) QEMU() sdkqemu.Service                     { return nil }
func (c *pciPVEClient) Cluster() sdkcluster.Service               { return nil }
func (c *pciPVEClient) Storage() sdkstorage.Service               { return nil }
func (c *pciPVEClient) CloudInit() sdkcloudinit.Service           { return nil }
func (c *pciPVEClient) Tasks() sdktasks.Service                   { return nil }
func (c *pciPVEClient) Nodes() sdknodes.Service                   { return c.nodesSvc }
func (c *pciPVEClient) ClusterStorage() sdkclusterstorage.Service { return nil }
func (c *pciPVEClient) Pools() pve.PoolService                    { return nil }

func pciDeps(nodesSvc sdknodes.Service) Deps {
	return Deps{
		PVE:    &pciPVEClient{nodesSvc: nodesSvc},
		Logger: log.NewNopLogger(),
	}
}

// ---------------------------------------------------------------------------
// validatePCIPassthroughs
// ---------------------------------------------------------------------------

func TestValidatePCIPassthroughs_ValidAddresses(t *testing.T) {
	t.Parallel()

	pts := []PCIPassthrough{
		{Address: "0000:01:00.0"},
		{Address: "0000:02:00.1"},
	}
	if err := validatePCIPassthroughs(pts); err != nil {
		t.Fatalf("expected no error for valid addresses; got %v", err)
	}
}

func TestValidatePCIPassthroughs_EmptySlice_NoError(t *testing.T) {
	t.Parallel()

	if err := validatePCIPassthroughs(nil); err != nil {
		t.Fatalf("expected no error for nil slice; got %v", err)
	}
	if err := validatePCIPassthroughs([]PCIPassthrough{}); err != nil {
		t.Fatalf("expected no error for empty slice; got %v", err)
	}
}

func TestValidatePCIPassthroughs_EmptyAddress_Error(t *testing.T) {
	t.Parallel()

	pts := []PCIPassthrough{{Address: ""}}
	err := validatePCIPassthroughs(pts)
	if err == nil {
		t.Fatal("expected error for empty address; got nil")
	}
}

func TestValidatePCIPassthroughs_BadFormat_Error(t *testing.T) {
	t.Parallel()

	cases := []string{
		"01:00.0",        // missing domain
		"0000:01:00",     // missing .func
		"0000:01:00.0.0", // extra segment
		"ZZZZ:01:00.0",   // non-hex domain
		"0000:01:0.0",    // slot too short
	}
	for _, addr := range cases {
		pts := []PCIPassthrough{{Address: addr}}
		err := validatePCIPassthroughs(pts)
		if err == nil {
			t.Errorf("expected error for address %q; got nil", addr)
		}
	}
}

// ---------------------------------------------------------------------------
// buildPCIChecker
// ---------------------------------------------------------------------------

// stubPCINodeSvc implements nodesServiceForPCI for tests.
type stubPCINodeSvc struct {
	fn func(ctx context.Context, node string, params *sdknodes.ListHardwarePciParams) (*sdknodes.ListHardwarePciResponse, error)
}

func (s *stubPCINodeSvc) ListHardwarePci(ctx context.Context, node string, params *sdknodes.ListHardwarePciParams) (*sdknodes.ListHardwarePciResponse, error) {
	return s.fn(ctx, node, params)
}

func rawPCIEntry(id string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"id": id, "class": "0x030200", "device_name": "Tesla T4"})
	return b
}

func TestBuildPCIChecker_DevicePresent_ReturnsTrue(t *testing.T) {
	t.Parallel()

	svc := &stubPCINodeSvc{
		fn: func(_ context.Context, _ string, _ *sdknodes.ListHardwarePciParams) (*sdknodes.ListHardwarePciResponse, error) {
			resp := sdknodes.ListHardwarePciResponse{rawPCIEntry("0000:01:00.0")}
			return &resp, nil
		},
	}
	checker := buildPCIChecker(context.Background(), svc, []string{"0000:01:00.0"})
	ok, err := checker("pve1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected checker to return true; got false")
	}
}

func TestBuildPCIChecker_DeviceAbsent_ReturnsFalse(t *testing.T) {
	t.Parallel()

	svc := &stubPCINodeSvc{
		fn: func(_ context.Context, _ string, _ *sdknodes.ListHardwarePciParams) (*sdknodes.ListHardwarePciResponse, error) {
			resp := sdknodes.ListHardwarePciResponse{rawPCIEntry("0000:02:00.0")}
			return &resp, nil
		},
	}
	checker := buildPCIChecker(context.Background(), svc, []string{"0000:01:00.0"})
	ok, err := checker("pve1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected checker to return false when device absent")
	}
}

func TestBuildPCIChecker_APIError_ReturnsError(t *testing.T) {
	t.Parallel()

	apiErr := errors.New("PVE API error")
	svc := &stubPCINodeSvc{
		fn: func(_ context.Context, _ string, _ *sdknodes.ListHardwarePciParams) (*sdknodes.ListHardwarePciResponse, error) {
			return nil, apiErr
		},
	}
	checker := buildPCIChecker(context.Background(), svc, []string{"0000:01:00.0"})
	ok, err := checker("pve1")
	if err == nil {
		t.Fatal("expected error from checker on API failure; got nil")
	}
	if ok {
		t.Error("expected ok=false when API error")
	}
}

func TestBuildPCIChecker_MultipleDevicesAllPresent_ReturnsTrue(t *testing.T) {
	t.Parallel()

	svc := &stubPCINodeSvc{
		fn: func(_ context.Context, _ string, _ *sdknodes.ListHardwarePciParams) (*sdknodes.ListHardwarePciResponse, error) {
			resp := sdknodes.ListHardwarePciResponse{
				rawPCIEntry("0000:01:00.0"),
				rawPCIEntry("0000:02:00.0"),
			}
			return &resp, nil
		},
	}
	checker := buildPCIChecker(context.Background(), svc, []string{"0000:01:00.0", "0000:02:00.0"})
	ok, err := checker("pve1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected true when all devices present")
	}
}

func TestBuildPCIChecker_MultipleDevicesOneMissing_ReturnsFalse(t *testing.T) {
	t.Parallel()

	svc := &stubPCINodeSvc{
		fn: func(_ context.Context, _ string, _ *sdknodes.ListHardwarePciParams) (*sdknodes.ListHardwarePciResponse, error) {
			resp := sdknodes.ListHardwarePciResponse{rawPCIEntry("0000:01:00.0")}
			return &resp, nil
		},
	}
	checker := buildPCIChecker(context.Background(), svc, []string{"0000:01:00.0", "0000:02:00.0"})
	ok, err := checker("pve1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected false when one of two devices is absent")
	}
}

func TestBuildPCIChecker_MalformedEntry_Skipped(t *testing.T) {
	t.Parallel()

	svc := &stubPCINodeSvc{
		fn: func(_ context.Context, _ string, _ *sdknodes.ListHardwarePciParams) (*sdknodes.ListHardwarePciResponse, error) {
			// First entry is malformed; second is valid.
			resp := sdknodes.ListHardwarePciResponse{
				json.RawMessage(`not-valid-json`),
				rawPCIEntry("0000:01:00.0"),
			}
			return &resp, nil
		},
	}
	checker := buildPCIChecker(context.Background(), svc, []string{"0000:01:00.0"})
	ok, err := checker("pve1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected true: malformed entry skipped, valid entry present")
	}
}

// ---------------------------------------------------------------------------
// applyPCIPassthrough
// ---------------------------------------------------------------------------

func TestApplyPCIPassthrough_Empty_NoAPICall(t *testing.T) {
	t.Parallel()

	called := false
	nodesSvc := &pciNodesStub{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			called = true
			return nil
		},
	}
	deps := pciDeps(nodesSvc)
	err := applyPCIPassthrough(context.Background(), deps, "pve1", 100, nil, deps.Logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("UpdateQemuConfig must not be called when pts is empty")
	}
}

func TestApplyPCIPassthrough_SingleDevice_SetsHostpci0(t *testing.T) {
	t.Parallel()

	var capturedParams *sdknodes.UpdateQemuConfigParams
	nodesSvc := &pciNodesStub{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			capturedParams = params
			return nil
		},
	}
	deps := pciDeps(nodesSvc)
	pts := []PCIPassthrough{{Address: "0000:01:00.0"}}
	if err := applyPCIPassthrough(context.Background(), deps, "pve1", 100, pts, deps.Logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedParams == nil {
		t.Fatal("UpdateQemuConfig was not called")
	}
	if capturedParams.Hostpci[0] != "0000:01:00.0" {
		t.Errorf("hostpci0 = %q; want %q", capturedParams.Hostpci[0], "0000:01:00.0")
	}
}

func TestApplyPCIPassthrough_TwoDevices_SetsHostpci0And1(t *testing.T) {
	t.Parallel()

	var capturedParams *sdknodes.UpdateQemuConfigParams
	nodesSvc := &pciNodesStub{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			capturedParams = params
			return nil
		},
	}
	deps := pciDeps(nodesSvc)
	pts := []PCIPassthrough{
		{Address: "0000:01:00.0"},
		{Address: "0000:02:00.0"},
	}
	if err := applyPCIPassthrough(context.Background(), deps, "pve1", 100, pts, deps.Logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedParams == nil {
		t.Fatal("UpdateQemuConfig was not called")
	}
	if capturedParams.Hostpci[0] != "0000:01:00.0" {
		t.Errorf("hostpci0 = %q; want %q", capturedParams.Hostpci[0], "0000:01:00.0")
	}
	if capturedParams.Hostpci[1] != "0000:02:00.0" {
		t.Errorf("hostpci1 = %q; want %q", capturedParams.Hostpci[1], "0000:02:00.0")
	}
}

// ---------------------------------------------------------------------------
// normalizePCIAddress / short-form id matching
// ---------------------------------------------------------------------------

func TestNormalizePCIAddress(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"0000:01:00.0", "0000:01:00.0"},
		{"01:00.0", "0000:01:00.0"},      // short form gets the default domain
		{"0000:01:00.A", "0000:01:00.a"}, // lowercased
		{" 01:00.0 ", "0000:01:00.0"},    // trimmed then padded
		{"bogus", "bogus"},               // unrecognized shapes pass through
	}
	for _, c := range cases {
		if got := normalizePCIAddress(c.in); got != c.want {
			t.Errorf("normalizePCIAddress(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestBuildPCIChecker_ShortFormID_MatchesCanonicalAddress(t *testing.T) {
	t.Parallel()

	// Some PVE versions report /hardware/pci ids without the PCI domain.
	svc := &stubPCINodeSvc{
		fn: func(_ context.Context, _ string, _ *sdknodes.ListHardwarePciParams) (*sdknodes.ListHardwarePciResponse, error) {
			resp := sdknodes.ListHardwarePciResponse{rawPCIEntry("01:00.0")}
			return &resp, nil
		},
	}
	checker := buildPCIChecker(context.Background(), svc, []string{"0000:01:00.0"})
	ok, err := checker("pve1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected short-form PVE id to match canonical operator address")
	}
}

// ---------------------------------------------------------------------------
// verifyPCIOnNode (post-resolution guard covering filter-bypassing paths)
// ---------------------------------------------------------------------------

func TestVerifyPCIOnNode_Empty_NoAPICall(t *testing.T) {
	t.Parallel()

	called := false
	nodesSvc := &pciNodesStub{
		listHardwarePciFn: func(_ context.Context, _ string, _ *sdknodes.ListHardwarePciParams) (*sdknodes.ListHardwarePciResponse, error) {
			called = true
			empty := sdknodes.ListHardwarePciResponse{}
			return &empty, nil
		},
	}
	deps := pciDeps(nodesSvc)
	if err := verifyPCIOnNode(context.Background(), deps, "pve1", nil, deps.Logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("ListHardwarePci must not be called when pts is empty")
	}
}

func TestVerifyPCIOnNode_DevicePresent_NoError(t *testing.T) {
	t.Parallel()

	nodesSvc := &pciNodesStub{
		listHardwarePciFn: func(_ context.Context, node string, _ *sdknodes.ListHardwarePciParams) (*sdknodes.ListHardwarePciResponse, error) {
			if node != "pve1" {
				t.Errorf("checked node = %q; want pve1", node)
			}
			resp := sdknodes.ListHardwarePciResponse{rawPCIEntry("0000:01:00.0")}
			return &resp, nil
		},
	}
	deps := pciDeps(nodesSvc)
	pts := []PCIPassthrough{{Address: "0000:01:00.0"}}
	if err := verifyPCIOnNode(context.Background(), deps, "pve1", pts, deps.Logger); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVerifyPCIOnNode_DeviceAbsent_NonRetriableError(t *testing.T) {
	t.Parallel()

	nodesSvc := &pciNodesStub{
		listHardwarePciFn: func(_ context.Context, _ string, _ *sdknodes.ListHardwarePciParams) (*sdknodes.ListHardwarePciResponse, error) {
			resp := sdknodes.ListHardwarePciResponse{rawPCIEntry("0000:09:00.0")}
			return &resp, nil
		},
	}
	deps := pciDeps(nodesSvc)
	pts := []PCIPassthrough{{Address: "0000:01:00.0"}}
	err := verifyPCIOnNode(context.Background(), deps, "pve1", pts, deps.Logger)
	if err == nil {
		t.Fatal("expected error when device absent on resolved node; got nil")
	}
	if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Error("device-absent must be non-retriable (operator config error); got retriable")
	}
}

func TestVerifyPCIOnNode_APIError_RetriableError(t *testing.T) {
	t.Parallel()

	nodesSvc := &pciNodesStub{
		listHardwarePciFn: func(_ context.Context, _ string, _ *sdknodes.ListHardwarePciParams) (*sdknodes.ListHardwarePciResponse, error) {
			return nil, errors.New("pvedaemon restarting")
		},
	}
	deps := pciDeps(nodesSvc)
	pts := []PCIPassthrough{{Address: "0000:01:00.0"}}
	err := verifyPCIOnNode(context.Background(), deps, "pve1", pts, deps.Logger)
	if err == nil {
		t.Fatal("expected error when ListHardwarePci fails; got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("transient API failure must be retriable; got %v", err)
	}
}

// ---------------------------------------------------------------------------
// isTransientRejectionReason — PCI reason classification
// ---------------------------------------------------------------------------

func TestIsTransientRejectionReason_PCIReasons(t *testing.T) {
	t.Parallel()

	if !isTransientRejectionReason("PCI device check error: connection refused") {
		t.Error("a failed PCI device check must classify as transient (director may re-drive)")
	}
	if isTransientRejectionReason("missing required PCI device") {
		t.Error("a confirmed-absent PCI device must classify as permanent")
	}
}

// ---------------------------------------------------------------------------
// applyPCIPassthroughWithCleanup — failure destroys the candidate VM
// ---------------------------------------------------------------------------

func TestApplyPCIPassthroughWithCleanup_ErrorDestroysCandidate(t *testing.T) {
	t.Parallel()

	// kfNodes.updateErr makes the hostpci UpdateQemuConfig call fail; the
	// cleanup wrapper must then destroy the candidate VM (DeleteQemu) so a
	// failed PCI apply never orphans a cloned VM.
	nodesSvc := &kfNodes{updateErr: errors.New("hostpci rejected by PVE")}
	q := &kfQEMU{}
	deps := Deps{
		PVE:    &kfClient{nodes: nodesSvc, qemu: q, cluster: newNAStub()},
		Logger: log.NewNopLogger(),
	}

	pts := []PCIPassthrough{{Address: "0000:01:00.0"}}
	err := applyPCIPassthroughWithCleanup(context.Background(), deps, "pve1", 4242, pts, deps.Logger)
	if err == nil {
		t.Fatal("expected the UpdateQemuConfig error to propagate; got nil")
	}
	if nodesSvc.deleteCalls != 1 {
		t.Errorf("cleanupVM must destroy the candidate VM on PCI apply failure; DeleteQemu calls = %d, want 1", nodesSvc.deleteCalls)
	}
	if q.stopCalls != 1 {
		t.Errorf("cleanupVM must attempt to stop the candidate VM before destroy; Stop calls = %d, want 1", q.stopCalls)
	}
}

// ---------------------------------------------------------------------------
// cleanupVM — node-affinity pin removal is unconditional (no flag gate)
// ---------------------------------------------------------------------------

func TestCleanupVM_RemovesNodeAffinityPin_WithoutPinFlag(t *testing.T) {
	t.Parallel()

	// The PCI strict pin is written regardless of placement.pin_az_via_ha_rules,
	// so rollback must remove the pin unconditionally — a flag-gated removal
	// would orphan the bosh-na-<vmid> rule forever. nil Config mirrors a
	// default, flag-off deployment.
	cl := newNAStub()
	cl.rules["bosh-na-4242"] = sdkcluster.CreateHaRulesParams{Rule: "bosh-na-4242"}
	cl.resources["vm:4242"] = true
	deps := Deps{
		PVE:    &kfClient{nodes: &kfNodes{}, qemu: &kfQEMU{}, cluster: cl},
		Logger: log.NewNopLogger(),
	}

	cleanupVM(context.Background(), deps, "pve1", 4242, deps.Logger)

	if _, ok := cl.rules["bosh-na-4242"]; ok {
		t.Error("cleanupVM must delete the bosh-na-<vmid> HA rule even with the AZ-pin flag off")
	}
	if cl.resources["vm:4242"] {
		t.Error("cleanupVM must deregister the VM's HA resource on rollback")
	}
}
