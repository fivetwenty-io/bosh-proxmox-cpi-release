// Package handlers internal tests for §7.55 advertised_routes SDN injection.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	pveerr "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// sdkConflictErr is the SDK conflict sentinel that isSDNConflict recognizes via
// errors.Is. Wrap it with fmt.Errorf to simulate the real PVE SDK error chain.
var sdkConflictErr = pveerr.ErrConflict

// --------------------------------------------------------------------------
// routeClusterStub records CreateSdnVnetsSubnets and DeleteSdnVnetsSubnets
// calls. UpdateSdn (applySDN) is also recorded. Other methods panic via the
// nil embed.
// --------------------------------------------------------------------------

type routeClusterStub struct {
	sdkcluster.Service
	// createCalls is a list of (vnet, subnet) pairs in order.
	createCalls []routeCreateCall
	// deleteCalls is a list of (vnet, subnet) pairs in order.
	deleteCalls []routeDeleteCall
	// updateSdnCalls counts applySDN calls.
	updateSdnCalls int

	// inject errors
	createErr    error
	createErrAt  int // -1 = all calls fail; ≥0 = fail at that index
	deleteErr    error
	updateSdnErr error
}

type routeCreateCall struct {
	vnet   string
	subnet string
}

type routeDeleteCall struct {
	vnet   string
	subnet string
}

func (s *routeClusterStub) CreateSdnVnetsSubnets(_ context.Context, vnet string, p *sdkcluster.CreateSdnVnetsSubnetsParams) error {
	idx := len(s.createCalls)
	s.createCalls = append(s.createCalls, routeCreateCall{vnet: vnet, subnet: p.Subnet})
	if s.createErr != nil {
		if s.createErrAt < 0 || s.createErrAt == idx {
			return s.createErr
		}
	}
	return nil
}

func (s *routeClusterStub) DeleteSdnVnetsSubnets(_ context.Context, vnet, subnet string, _ *sdkcluster.DeleteSdnVnetsSubnetsParams) error {
	s.deleteCalls = append(s.deleteCalls, routeDeleteCall{vnet: vnet, subnet: subnet})
	return s.deleteErr
}

func (s *routeClusterStub) UpdateSdn(_ context.Context, _ *sdkcluster.UpdateSdnParams) (*sdkcluster.UpdateSdnResponse, error) {
	s.updateSdnCalls++
	if s.updateSdnErr != nil {
		return nil, s.updateSdnErr
	}
	// Return a nil body (synchronous apply, no UPID).
	return nil, nil //nolint:nilnil // stub: nil response + nil error = sync apply success
}

func routeDeps(cl *routeClusterStub) Deps {
	return Deps{
		Config: icMinConfig(),
		PVE:    &icPVEClient{clusterSvc: cl},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
}

// --------------------------------------------------------------------------
// validateAdvertisedRoutes
// --------------------------------------------------------------------------

func TestValidateAdvertisedRoutes_EmptyIsNil(t *testing.T) {
	if err := validateAdvertisedRoutes(nil); err != nil {
		t.Fatalf("expected nil for empty routes; got %v", err)
	}
	if err := validateAdvertisedRoutes([]AdvertisedRoute{}); err != nil {
		t.Fatalf("expected nil for empty slice; got %v", err)
	}
}

func TestValidateAdvertisedRoutes_ValidRoute(t *testing.T) {
	routes := []AdvertisedRoute{
		{VNet: "vnet1", Destination: "10.64.0.0/16"},
		{VNet: "vnet2", Destination: "192.168.100.0/24"},
	}
	if err := validateAdvertisedRoutes(routes); err != nil {
		t.Fatalf("unexpected error for valid routes: %v", err)
	}
}

func TestValidateAdvertisedRoutes_EmptyVNetErrors(t *testing.T) {
	routes := []AdvertisedRoute{{VNet: "", Destination: "10.0.0.0/8"}}
	err := validateAdvertisedRoutes(routes)
	if err == nil {
		t.Fatal("expected error for empty vnet")
	}
}

func TestValidateAdvertisedRoutes_InvalidVNetNameErrors(t *testing.T) {
	// Too long (>8 chars) and with uppercase — both invalid.
	cases := []string{"BIGNAME1", "toolongname", "vnet_x", "VN1"}
	for _, name := range cases {
		routes := []AdvertisedRoute{{VNet: name, Destination: "10.0.0.0/8"}}
		if err := validateAdvertisedRoutes(routes); err == nil {
			t.Errorf("expected error for vnet name %q; got nil", name)
		}
	}
}

func TestValidateAdvertisedRoutes_EmptyDestinationErrors(t *testing.T) {
	routes := []AdvertisedRoute{{VNet: "vnet1", Destination: ""}}
	err := validateAdvertisedRoutes(routes)
	if err == nil {
		t.Fatal("expected error for empty destination")
	}
}

func TestValidateAdvertisedRoutes_InvalidCIDRErrors(t *testing.T) {
	cases := []string{"10.0.0.0", "not-a-cidr", "10.0.0.0/33", "300.0.0.0/8"}
	for _, dest := range cases {
		routes := []AdvertisedRoute{{VNet: "vnet1", Destination: dest}}
		if err := validateAdvertisedRoutes(routes); err == nil {
			t.Errorf("expected error for destination %q; got nil", dest)
		}
	}
}

// --------------------------------------------------------------------------
// applyAdvertisedRoutes — byte-identical path
// --------------------------------------------------------------------------

func TestApplyAdvertisedRoutes_EmptyNoCalls(t *testing.T) {
	cl := &routeClusterStub{}
	if err := applyAdvertisedRoutes(context.Background(), routeDeps(cl), "pve1", 100, nil, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error for nil routes: %v", err)
	}
	if len(cl.createCalls) != 0 || cl.updateSdnCalls != 0 {
		t.Errorf("no API calls expected for nil routes; got create=%v updateSdn=%d",
			cl.createCalls, cl.updateSdnCalls)
	}

	cl2 := &routeClusterStub{}
	if err := applyAdvertisedRoutes(context.Background(), routeDeps(cl2), "pve1", 100, []AdvertisedRoute{}, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error for empty slice: %v", err)
	}
	if len(cl2.createCalls) != 0 || cl2.updateSdnCalls != 0 {
		t.Errorf("no API calls expected for empty slice; got create=%v updateSdn=%d",
			cl2.createCalls, cl2.updateSdnCalls)
	}
}

// --------------------------------------------------------------------------
// applyAdvertisedRoutes — happy path
// --------------------------------------------------------------------------

func TestApplyAdvertisedRoutes_CreatesSubnetsAndAppliesSDN(t *testing.T) {
	cl := &routeClusterStub{}
	routes := []AdvertisedRoute{
		{VNet: "vnet1", Destination: "10.64.0.0/16"},
		{VNet: "vnet2", Destination: "172.16.0.0/12"},
	}
	if err := applyAdvertisedRoutes(context.Background(), routeDeps(cl), "pve1", 200, routes, log.NewNopLogger()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Two CreateSdnVnetsSubnets calls.
	if len(cl.createCalls) != 2 {
		t.Fatalf("expected 2 create calls; got %d: %v", len(cl.createCalls), cl.createCalls)
	}
	if cl.createCalls[0].vnet != "vnet1" || cl.createCalls[0].subnet != "10.64.0.0/16" {
		t.Errorf("first create: got %+v; want {vnet1, 10.64.0.0/16}", cl.createCalls[0])
	}
	if cl.createCalls[1].vnet != "vnet2" || cl.createCalls[1].subnet != "172.16.0.0/12" {
		t.Errorf("second create: got %+v; want {vnet2, 172.16.0.0/12}", cl.createCalls[1])
	}

	// Exactly one applySDN call.
	if cl.updateSdnCalls != 1 {
		t.Errorf("expected 1 applySDN call; got %d", cl.updateSdnCalls)
	}

	// No rollback delete calls on success.
	if len(cl.deleteCalls) != 0 {
		t.Errorf("expected no delete calls on success; got %v", cl.deleteCalls)
	}
}

// --------------------------------------------------------------------------
// applyAdvertisedRoutes — create failure triggers rollback
// --------------------------------------------------------------------------

func TestApplyAdvertisedRoutes_CreateFailureRollsBackPrevious(t *testing.T) {
	// First create succeeds, second fails. The first must be rolled back.
	cl := &routeClusterStub{
		createErr:   errors.New("pve: subnet conflict"),
		createErrAt: 1, // fail on second call (index 1)
	}
	routes := []AdvertisedRoute{
		{VNet: "vnet1", Destination: "10.64.0.0/16"},
		{VNet: "vnet2", Destination: "10.65.0.0/16"},
	}
	err := applyAdvertisedRoutes(context.Background(), routeDeps(cl), "pve1", 201, routes, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error when second create fails")
	}

	// First subnet must be rolled back via DeleteSdnVnetsSubnets.
	if len(cl.deleteCalls) != 1 {
		t.Fatalf("expected 1 rollback delete call; got %d: %v", len(cl.deleteCalls), cl.deleteCalls)
	}
	if cl.deleteCalls[0].vnet != "vnet1" || cl.deleteCalls[0].subnet != "10.64.0.0/16" {
		t.Errorf("rollback delete: got %+v; want {vnet1, 10.64.0.0/16}", cl.deleteCalls[0])
	}

	// applySDN must NOT be called when create fails.
	if cl.updateSdnCalls != 0 {
		t.Errorf("applySDN must not be called after create failure; got %d calls", cl.updateSdnCalls)
	}
}

func TestApplyAdvertisedRoutes_AllCreateFail_NothingToRollback(t *testing.T) {
	// First create fails immediately — nothing was committed yet.
	cl := &routeClusterStub{
		createErr:   errors.New("pve: conflict"),
		createErrAt: 0,
	}
	routes := []AdvertisedRoute{{VNet: "vnet1", Destination: "10.0.0.0/8"}}
	if err := applyAdvertisedRoutes(context.Background(), routeDeps(cl), "pve1", 202, routes, log.NewNopLogger()); err == nil {
		t.Fatal("expected error")
	}
	if len(cl.deleteCalls) != 0 {
		t.Errorf("expected no delete calls when nothing was committed; got %v", cl.deleteCalls)
	}
}

// --------------------------------------------------------------------------
// applyAdvertisedRoutes — applySDN failure triggers rollback
// --------------------------------------------------------------------------

func TestApplyAdvertisedRoutes_ApplySDNFailureRollsBackAll(t *testing.T) {
	cl := &routeClusterStub{updateSdnErr: errors.New("pve: sdn apply failed")}
	routes := []AdvertisedRoute{
		{VNet: "vnet1", Destination: "10.64.0.0/16"},
		{VNet: "vnet2", Destination: "10.65.0.0/16"},
	}
	err := applyAdvertisedRoutes(context.Background(), routeDeps(cl), "pve1", 203, routes, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error when applySDN fails")
	}

	// Both subnets must be rolled back.
	if len(cl.deleteCalls) != 2 {
		t.Fatalf("expected 2 rollback delete calls; got %d: %v", len(cl.deleteCalls), cl.deleteCalls)
	}
}

// --------------------------------------------------------------------------
// applyAdvertisedRoutes — rollback-delete failure logs warning but returns
// original cause error (not the delete error)
// --------------------------------------------------------------------------

func TestApplyAdvertisedRoutes_RollbackDeleteFailWarnsButReturnsOriginal(t *testing.T) {
	cl := &routeClusterStub{
		createErr:   errors.New("create boom"),
		createErrAt: 1, // second call fails
		deleteErr:   errors.New("delete boom"),
	}
	routes := []AdvertisedRoute{
		{VNet: "vnet1", Destination: "10.64.0.0/16"},
		{VNet: "vnet2", Destination: "10.65.0.0/16"},
	}
	err := applyAdvertisedRoutes(context.Background(), routeDeps(cl), "pve1", 204, routes, log.NewNopLogger())
	if err == nil {
		t.Fatal("expected error")
	}
	// Error must contain the original create error text, not the delete error.
	if !strings.Contains(err.Error(), "create boom") {
		t.Errorf("error %q should contain original cause 'create boom'", err.Error())
	}
}

// --------------------------------------------------------------------------
// applyAdvertisedRoutes — idempotency: already-exists (409) is not an error
// --------------------------------------------------------------------------

func TestApplyAdvertisedRoutes_AlreadyExistsIsIdempotent(t *testing.T) {
	// PVE returns a 409 Conflict when the subnet already exists.
	// isSDNConflict recognizes fmt.Errorf-wrapped pveerr.ErrConflict.
	// Without this guard, a director retry after a partial-create+failed-rollback
	// would permanently wedge: every call finds the leftover subnet and fails.
	cl := &routeClusterStub{
		createErr:   fmt.Errorf("subnet conflict: %w", sdkConflictErr),
		createErrAt: -1, // all create calls return conflict
	}
	routes := []AdvertisedRoute{
		{VNet: "vnet1", Destination: "10.64.0.0/16"},
		{VNet: "vnet2", Destination: "10.65.0.0/16"},
	}
	err := applyAdvertisedRoutes(context.Background(), routeDeps(cl), "pve1", 205, routes, log.NewNopLogger())
	if err != nil {
		t.Fatalf("already-exists conflict must be idempotent; got error: %v", err)
	}
	// applySDN must still be called to commit the SDN state.
	if cl.updateSdnCalls != 1 {
		t.Errorf("expected 1 applySDN call even when subnets already exist; got %d", cl.updateSdnCalls)
	}
	// Pre-existing subnets must NOT be rolled back (we did not create them).
	if len(cl.deleteCalls) != 0 {
		t.Errorf("must not delete pre-existing subnets; got %v", cl.deleteCalls)
	}
}

// --------------------------------------------------------------------------
// validateCIDR
// --------------------------------------------------------------------------

func TestValidateCIDR(t *testing.T) {
	cases := []struct {
		input   string
		wantErr bool
	}{
		{"10.0.0.0/8", false},
		{"192.168.1.0/24", false},
		{"2001:db8::/32", false},
		{"", true},
		{"10.0.0.0", true},
		{"not-a-cidr", true},
		{"10.0.0.0/33", true},
	}
	for _, tc := range cases {
		err := validateCIDR(tc.input)
		if tc.wantErr && err == nil {
			t.Errorf("validateCIDR(%q): expected error; got nil", tc.input)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("validateCIDR(%q): unexpected error: %v", tc.input, err)
		}
	}
}
