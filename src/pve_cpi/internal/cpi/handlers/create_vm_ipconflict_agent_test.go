// Package handlers — tests for the guest-agent IP fan-out conflict probe.
// Uses package handlers (internal test) to access private functions directly.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

// agentIPFace builds one interface JSON object for the guest-agent response.
func agentIPFace(name, ipType, ip string, prefix int) map[string]any {
	return map[string]any{
		"name": name,
		"ip-addresses": []map[string]any{
			{
				"ip-address-type": ipType,
				"ip-address":      ip,
				"prefix":          prefix,
			},
		},
	}
}

// agentResp serializes interfaces wrapped in {"result": [...]} envelope.
func agentResp(ifaces ...map[string]any) *nodes.ListQemuAgentNetworkGetInterfacesResponse {
	wrapped := map[string]any{"result": ifaces}
	b, _ := json.Marshal(wrapped)
	raw := nodes.ListQemuAgentNetworkGetInterfacesResponse(b)
	return &raw
}

// agentRespArray serializes directly as a JSON array (no envelope).
func agentRespArray(ifaces ...map[string]any) *nodes.ListQemuAgentNetworkGetInterfacesResponse {
	b, _ := json.Marshal(ifaces)
	raw := nodes.ListQemuAgentNetworkGetInterfacesResponse(b)
	return &raw
}

// agentRunningResource builds a cluster ListResources entry for a QEMU VM with given status.
// Uses "pve1" as the node (the only node used in these tests).
func agentRunningResource(vmid int, status string) json.RawMessage {
	m := map[string]any{
		"vmid":   vmid,
		"node":   "pve1",
		"type":   "qemu",
		"status": status,
	}
	b, _ := json.Marshal(m)
	return b
}

// agentListResp wraps raw JSON entries into a *cluster.ListResourcesResponse.
func agentListResp(entries ...json.RawMessage) *sdkcluster.ListResourcesResponse {
	r := sdkcluster.ListResourcesResponse(entries)
	return &r
}

// --------------------------------------------------------------------------
// agentDeps builds a Deps for probeGuestAgentIPConflict tests.
// --------------------------------------------------------------------------

func agentDeps(
	listFn func(context.Context, *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error),
	agentFn func(ctx context.Context, node string, vmid string) (*nodes.ListQemuAgentNetworkGetInterfacesResponse, error),
) Deps {
	return Deps{
		Config: icMinConfig(),
		PVE: &icPVEClient{
			clusterSvc: &icClusterService{listFn: listFn},
			nodesSvc:   &icNodesService{listFn: listFn, agentFn: agentFn},
		},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
}

// --------------------------------------------------------------------------
// probeGuestAgentIPConflict tests
// --------------------------------------------------------------------------

// TestProbeGuestAgentIPConflict_EmptyTargets verifies nil/empty targetIPs returns nil immediately.
func TestProbeGuestAgentIPConflict_EmptyTargets(t *testing.T) {
	t.Parallel()
	called := false
	deps := agentDeps(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		called = true
		return agentListResp(), nil
	}, nil)
	err := probeGuestAgentIPConflict(context.Background(), deps, log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("empty targetIPs: expected nil, got %v", err)
	}
	if called {
		t.Error("ListResources must not be called for nil targetIPs")
	}
}

// TestProbeGuestAgentIPConflict_EmptyCluster verifies no error when cluster has no VMs.
func TestProbeGuestAgentIPConflict_EmptyCluster(t *testing.T) {
	t.Parallel()
	deps := agentDeps(func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return agentListResp(), nil
	}, nil)
	err := probeGuestAgentIPConflict(context.Background(), deps, log.NewNopLogger(), []string{"10.0.0.5"})
	if err != nil {
		t.Fatalf("empty cluster: expected nil, got %v", err)
	}
}

// TestProbeGuestAgentIPConflict_NoRunningVMs verifies stopped VMs are skipped.
func TestProbeGuestAgentIPConflict_NoRunningVMs(t *testing.T) {
	t.Parallel()
	agentCalled := false
	deps := agentDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return agentListResp(agentRunningResource(100, "stopped")), nil
		},
		func(_ context.Context, _ string, _ string) (*nodes.ListQemuAgentNetworkGetInterfacesResponse, error) {
			agentCalled = true
			return agentResp(), nil
		},
	)
	err := probeGuestAgentIPConflict(context.Background(), deps, log.NewNopLogger(), []string{"10.0.0.5"})
	if err != nil {
		t.Fatalf("stopped VMs: expected nil, got %v", err)
	}
	if agentCalled {
		t.Error("agent must not be called for stopped VMs")
	}
}

// TestProbeGuestAgentIPConflict_DHCPIPConflict verifies a running VM's DHCP-assigned IP triggers conflict.
func TestProbeGuestAgentIPConflict_DHCPIPConflict(t *testing.T) {
	t.Parallel()
	const targetIP = "10.0.0.5"
	deps := agentDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return agentListResp(agentRunningResource(200, "running")), nil
		},
		func(_ context.Context, _ string, _ string) (*nodes.ListQemuAgentNetworkGetInterfacesResponse, error) {
			return agentResp(agentIPFace("eth0", "ipv4", targetIP, 24)), nil
		},
	)
	err := probeGuestAgentIPConflict(context.Background(), deps, log.NewNopLogger(), []string{targetIP})
	if err == nil {
		t.Fatal("expected IPConflictCloudError, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("expected TypeCloud error, got %T: %v", err, err)
	}
	msg := err.Error()
	if !agentContains(msg, targetIP) {
		t.Errorf("error %q missing target IP %q", msg, targetIP)
	}
	if !agentContains(msg, "200") {
		t.Errorf("error %q missing conflicting VMID", msg)
	}
}

// TestProbeGuestAgentIPConflict_NoConflict verifies no error when no guest holds a target IP.
func TestProbeGuestAgentIPConflict_NoConflict(t *testing.T) {
	t.Parallel()
	deps := agentDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return agentListResp(agentRunningResource(300, "running")), nil
		},
		func(_ context.Context, _ string, _ string) (*nodes.ListQemuAgentNetworkGetInterfacesResponse, error) {
			return agentResp(agentIPFace("eth0", "ipv4", "10.0.0.99", 24)), nil
		},
	)
	err := probeGuestAgentIPConflict(context.Background(), deps, log.NewNopLogger(), []string{"10.0.0.5"})
	if err != nil {
		t.Fatalf("no-conflict: expected nil, got %v", err)
	}
}

// TestProbeGuestAgentIPConflict_AgentError_FailOpen verifies agent error is skipped (fail-open).
func TestProbeGuestAgentIPConflict_AgentError_FailOpen(t *testing.T) {
	t.Parallel()
	deps := agentDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return agentListResp(agentRunningResource(400, "running")), nil
		},
		func(_ context.Context, _ string, _ string) (*nodes.ListQemuAgentNetworkGetInterfacesResponse, error) {
			return nil, errors.New("guest agent not running")
		},
	)
	err := probeGuestAgentIPConflict(context.Background(), deps, log.NewNopLogger(), []string{"10.0.0.5"})
	if err != nil {
		t.Fatalf("agent error must be fail-open, got %v", err)
	}
}

// TestProbeGuestAgentIPConflict_LoopbackIgnored verifies loopback addresses in guest are skipped.
func TestProbeGuestAgentIPConflict_LoopbackIgnored(t *testing.T) {
	t.Parallel()
	deps := agentDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return agentListResp(agentRunningResource(500, "running")), nil
		},
		func(_ context.Context, _ string, _ string) (*nodes.ListQemuAgentNetworkGetInterfacesResponse, error) {
			return agentResp(
				agentIPFace("lo", "ipv4", "127.0.0.1", 8),
				agentIPFace("lo", "ipv6", "::1", 128),
			), nil
		},
	)
	err := probeGuestAgentIPConflict(context.Background(), deps, log.NewNopLogger(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("loopback address must be ignored, got %v", err)
	}
}

// TestProbeGuestAgentIPConflict_LinkLocalIgnored verifies link-local addresses in guest are skipped.
func TestProbeGuestAgentIPConflict_LinkLocalIgnored(t *testing.T) {
	t.Parallel()
	deps := agentDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return agentListResp(agentRunningResource(600, "running")), nil
		},
		func(_ context.Context, _ string, _ string) (*nodes.ListQemuAgentNetworkGetInterfacesResponse, error) {
			return agentResp(
				agentIPFace("eth0", "ipv4", "169.254.0.1", 16),
				agentIPFace("eth0", "ipv6", "fe80::1", 64),
			), nil
		},
	)
	err := probeGuestAgentIPConflict(context.Background(), deps, log.NewNopLogger(), []string{"169.254.0.1", "fe80::1"})
	if err != nil {
		t.Fatalf("link-local address must be ignored, got %v", err)
	}
}

// TestProbeGuestAgentIPConflict_IPv6Conflict verifies IPv6 conflict detection.
func TestProbeGuestAgentIPConflict_IPv6Conflict(t *testing.T) {
	t.Parallel()
	const targetIP = "2001:db8::1"
	deps := agentDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return agentListResp(agentRunningResource(700, "running")), nil
		},
		func(_ context.Context, _ string, _ string) (*nodes.ListQemuAgentNetworkGetInterfacesResponse, error) {
			return agentResp(agentIPFace("eth0", "ipv6", targetIP, 64)), nil
		},
	)
	err := probeGuestAgentIPConflict(context.Background(), deps, log.NewNopLogger(), []string{targetIP})
	if err == nil {
		t.Fatal("expected IPConflictCloudError for IPv6, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("expected TypeCloud, got %T", err)
	}
}

// TestProbeGuestAgentIPConflict_ArrayResponseFormat verifies bare-array response format works.
func TestProbeGuestAgentIPConflict_ArrayResponseFormat(t *testing.T) {
	t.Parallel()
	const targetIP = "192.168.1.50"
	deps := agentDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return agentListResp(agentRunningResource(800, "running")), nil
		},
		func(_ context.Context, _ string, _ string) (*nodes.ListQemuAgentNetworkGetInterfacesResponse, error) {
			return agentRespArray(agentIPFace("eth0", "ipv4", targetIP, 24)), nil
		},
	)
	err := probeGuestAgentIPConflict(context.Background(), deps, log.NewNopLogger(), []string{targetIP})
	if err == nil {
		t.Fatal("expected conflict for array-format response, got nil")
	}
}

// TestProbeGuestAgentIPConflict_ContextCancellation verifies cancelled context does not hang or panic.
func TestProbeGuestAgentIPConflict_ContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	entries := make([]json.RawMessage, 10)
	for i := range entries {
		entries[i] = agentRunningResource(1000+i, "running")
	}
	deps := agentDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return agentListResp(entries...), nil
		},
		nil,
	)
	// Must not hang or panic; result does not matter.
	_ = probeGuestAgentIPConflict(ctx, deps, log.NewNopLogger(), []string{"10.0.0.1"})
}

// TestProbeGuestAgentIPConflict_NilResponseFailOpen verifies nil response is skip (fail-open).
func TestProbeGuestAgentIPConflict_NilResponseFailOpen(t *testing.T) {
	t.Parallel()
	deps := agentDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return agentListResp(agentRunningResource(900, "running")), nil
		},
		func(_ context.Context, _ string, _ string) (*nodes.ListQemuAgentNetworkGetInterfacesResponse, error) {
			return nil, nil // nil response, nil error
		},
	)
	err := probeGuestAgentIPConflict(context.Background(), deps, log.NewNopLogger(), []string{"10.0.0.5"})
	if err != nil {
		t.Fatalf("nil agent response must be fail-open, got %v", err)
	}
}

// --------------------------------------------------------------------------
// --------------------------------------------------------------------------
// CRITICAL-1 + HIGH-1: ListResources failure and g.Wait failure must fail-open.
// --------------------------------------------------------------------------

// TestProbeGuestAgentIPConflict_ListResourcesError_FailOpen verifies that when
// ListResources returns an error, probeGuestAgentIPConflict returns nil (fail-open).
func TestProbeGuestAgentIPConflict_ListResourcesError_FailOpen(t *testing.T) {
	t.Parallel()
	deps := agentDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return nil, errors.New("cluster API unavailable")
		},
		nil,
	)
	err := probeGuestAgentIPConflict(context.Background(), deps, log.NewNopLogger(), []string{"10.0.0.5"})
	if err != nil {
		t.Fatalf("ListResources error must be fail-open (nil), got %v", err)
	}
}

// TestProbeGuestAgentIPConflict_RealConflictStillReturnsError confirms that
// a genuine IP conflict still returns a non-nil Cloud error even after the
// fail-open changes (regression guard).
func TestProbeGuestAgentIPConflict_RealConflictStillReturnsError(t *testing.T) {
	t.Parallel()
	const targetIP = "10.1.2.3"
	deps := agentDeps(
		func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return agentListResp(agentRunningResource(1100, "running")), nil
		},
		func(_ context.Context, _ string, _ string) (*nodes.ListQemuAgentNetworkGetInterfacesResponse, error) {
			return agentResp(agentIPFace("eth0", "ipv4", targetIP, 24)), nil
		},
	)
	err := probeGuestAgentIPConflict(context.Background(), deps, log.NewNopLogger(), []string{targetIP})
	if err == nil {
		t.Fatal("real conflict must return non-nil error, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("real conflict must return TypeCloud, got %T: %v", err, err)
	}
}

// --------------------------------------------------------------------------
// MEDIUM-1: CIDR and zone-suffixed addresses must parse correctly.
// --------------------------------------------------------------------------

// TestFindConflictingIP_CIDRSuffix verifies "192.168.1.5/24" guest address
// matches target "192.168.1.5" (strips CIDR prefix before ParseIP).
func TestFindConflictingIP_CIDRSuffix(t *testing.T) {
	t.Parallel()
	ifaces := []agentNetworkEntry{
		{
			Name: "eth0",
			IPAddresses: []agentIPAddress{
				{Type: "ipv4", Address: "10.0.0.5/24", Prefix: 24},
			},
		},
	}
	targetSet := map[string]string{"10.0.0.5": "10.0.0.5"}
	key, orig := findConflictingIP(ifaces, targetSet)
	if key == "" {
		t.Fatal("findConflictingIP: expected conflict for CIDR-suffixed address, got none")
	}
	if orig != "10.0.0.5" {
		t.Errorf("findConflictingIP: orig = %q; want %q", orig, "10.0.0.5")
	}
}

// TestFindConflictingIP_ZoneSuffix_LinkLocal verifies "fe80::1%eth0" is still
// skipped (link-local) but is parsed correctly rather than failing ParseIP.
func TestFindConflictingIP_ZoneSuffix_LinkLocal(t *testing.T) {
	t.Parallel()
	ifaces := []agentNetworkEntry{
		{
			Name: "eth0",
			IPAddresses: []agentIPAddress{
				{Type: "ipv6", Address: "fe80::1%eth0", Prefix: 64},
			},
		},
	}
	targetSet := map[string]string{"fe80::1": "fe80::1"}
	key, _ := findConflictingIP(ifaces, targetSet)
	if key != "" {
		t.Errorf("link-local address must be skipped; got conflict key %q", key)
	}
}

// TestFindConflictingIP_GlobalV6WithZone verifies a global IPv6 with zone
// suffix (e.g. "2001:db8::1%eth0") matches target "2001:db8::1".
func TestFindConflictingIP_GlobalV6WithZone(t *testing.T) {
	t.Parallel()
	ifaces := []agentNetworkEntry{
		{
			Name: "eth0",
			IPAddresses: []agentIPAddress{
				{Type: "ipv6", Address: "2001:db8::1%eth0", Prefix: 64},
			},
		},
	}
	targetSet := map[string]string{"2001:db8::1": "2001:db8::1"}
	key, orig := findConflictingIP(ifaces, targetSet)
	if key == "" {
		t.Fatal("findConflictingIP: expected conflict for global IPv6 with zone suffix, got none")
	}
	if orig != "2001:db8::1" {
		t.Errorf("findConflictingIP: orig = %q; want %q", orig, "2001:db8::1")
	}
}

// --------------------------------------------------------------------------
// MEDIUM-2: parseAgentNetworkInterfaces shape verification.
// The SDK type is json.RawMessage — it passes whatever PVE returns through
// resp.Data marshal/unmarshal unchanged. PVE wraps agent data in the "data"
// envelope stripped by the SDK client, so this function receives either:
//   - bare array:  [{"name":"eth0","ip-addresses":[...]}]
//   - result wrap: {"result":[{"name":"eth0","ip-addresses":[...]}]}
//
// The previously documented nested {"result":{"result":[...]}} shape is NOT
// real — it was an incorrect note. This test asserts both real shapes parse.
// --------------------------------------------------------------------------

// TestParseAgentNetworkInterfaces_BareArray verifies bare-array shape parses.
func TestParseAgentNetworkInterfaces_BareArray(t *testing.T) {
	t.Parallel()
	raw := []byte(`[{"name":"eth0","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"10.0.0.1","prefix":24}]}]`)
	ifaces := parseAgentNetworkInterfaces(raw)
	if len(ifaces) != 1 {
		t.Fatalf("bare array: expected 1 interface, got %d", len(ifaces))
	}
	if ifaces[0].Name != "eth0" {
		t.Errorf("bare array: ifaces[0].Name = %q; want %q", ifaces[0].Name, "eth0")
	}
}

// TestParseAgentNetworkInterfaces_ResultWrap verifies {"result":[...]} shape parses.
func TestParseAgentNetworkInterfaces_ResultWrap(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"result":[{"name":"eth0","ip-addresses":[{"ip-address-type":"ipv4","ip-address":"10.0.0.1","prefix":24}]}]}`)
	ifaces := parseAgentNetworkInterfaces(raw)
	if len(ifaces) != 1 {
		t.Fatalf("result-wrap: expected 1 interface, got %d", len(ifaces))
	}
	if ifaces[0].Name != "eth0" {
		t.Errorf("result-wrap: ifaces[0].Name = %q; want %q", ifaces[0].Name, "eth0")
	}
}

// --------------------------------------------------------------------------
// agentContains: helper — avoids dependency on strings package
// --------------------------------------------------------------------------

func agentContains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
