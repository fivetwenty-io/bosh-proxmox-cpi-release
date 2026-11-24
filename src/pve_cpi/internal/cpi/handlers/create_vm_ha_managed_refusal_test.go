package handlers_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
)

// managedRefusalRig is a create_vm call under pve.ha.mode managed on a
// single-node cluster whose HA profile the test supplies. It counts every
// create and clone that reaches PVE.
type managedRefusalRig struct {
	deps     handlers.Deps
	created  atomic.Int32
	cloned   atomic.Int32
	resolved atomic.Int32
}

func newManagedRefusalRig(t *testing.T, profile *ha.Profile, profileErr error, mutate func(*config.CPIConfig)) *managedRefusalRig {
	t.Helper()
	r := &managedRefusalRig{}
	q := &vmMockQEMU{
		createFn: func(context.Context, string, map[string]any) (string, error) {
			r.created.Add(1)
			return "UPID:pve:create:ok", nil
		},
		cloneFn: func(context.Context, string, int, map[string]any) (string, error) {
			r.cloned.Add(1)
			return "UPID:pve:clone:ok", nil
		},
	}
	r.deps = buildVMDepsPlacement(q, &vmMockNodes{}, listStatusSingleNode(), emptyListResources, &vmMockAgent{},
		func(c *config.CPIConfig) {
			c.Node = "pve"
			c.HA = &config.HAConfig{Mode: haChoiceOf(config.HAModeManaged)}
			c.EnsureNoIPConflicts = placementDisabled
			if mutate != nil {
				mutate(c)
			}
		})
	r.deps.HAProfile = func(context.Context) (*ha.Profile, error) {
		r.resolved.Add(1)
		return profile, profileErr
	}
	return r
}

func (r *managedRefusalRig) create(t *testing.T, directorUUID string, cloudProps map[string]any) error {
	t.Helper()
	args := mkArgs("agent-managed-refusal", testStemcellCID, cloudProps,
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})
	reqCtx := jsonrpc.Context{RequestID: "managed-refusal", DirectorUUID: directorUUID}
	_, err := handlers.HandleCreateVM(r.deps).Handle(context.Background(), args, reqCtx)
	return err
}

func (r *managedRefusalRig) wantNothingCreated(t *testing.T) {
	t.Helper()
	if n := r.created.Load(); n != 0 {
		t.Errorf("create_vm sent %d create calls to PVE before it refused, want 0", n)
	}
	if n := r.cloned.Load(); n != 0 {
		t.Errorf("create_vm sent %d clone calls to PVE before it refused, want 0", n)
	}
}

// refusingProfile is a managed profile on a cluster that misses a
// prerequisite, so the CPI may not write HA state there.
func refusingProfile() *ha.Profile {
	gaps := []string{
		"2 configured votes fall short of the CPI's HA rule, which needs a majority of them to remain after " +
			"any one node fails and does not count corosync's two_node flag, because 2 minus the 1 vote of pve " +
			"leaves 1, which is not a majority of 2 (2 from the nodelist, QDevice no)",
		"missing Sys.Console on /",
	}
	return &ha.Profile{
		Mode: config.HAModeManaged, Effective: ha.EffectiveObserveOnly, Reason: gaps[0], Gaps: gaps,
		Topology: config.HAStorageTopologyShared, TopologySource: ha.TopologyFromConfig,
	}
}

// TestCreateVM_ManagedPrerequisiteRefusalComesBeforeTheClone runs create_vm
// in managed mode on a cluster whose HA profile misses a prerequisite. The
// profile is known before the VM exists, so the call must fail with the
// profile's own non-retriable error before any create or clone reaches PVE.
func TestCreateVM_ManagedPrerequisiteRefusalComesBeforeTheClone(t *testing.T) {
	t.Parallel()
	profile := refusingProfile()
	r := newManagedRefusalRig(t, profile, nil, nil)

	err := r.create(t, "director-uuid-1", map[string]any{})
	if err == nil {
		t.Fatal("create_vm succeeded in managed mode on a cluster that misses an HA prerequisite")
	}
	want := profile.PrerequisiteError().Error()
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.OkToRetry() {
		t.Fatalf("want a non-retriable cloud error, got %T %v", err, err)
	}
	r.wantNothingCreated(t)
}

// TestCreateVM_ManagedRequestWithoutDirectorUUIDRefusesBeforeTheClone runs a
// managed create_vm whose request carries no director_uuid. The CPI could not
// mark the VM's HA resource as its own, which it knows before it creates
// anything.
func TestCreateVM_ManagedRequestWithoutDirectorUUIDRefusesBeforeTheClone(t *testing.T) {
	t.Parallel()
	r := newManagedRefusalRig(t, refusingProfile(), nil, nil)

	err := r.create(t, "", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "carries no director_uuid") {
		t.Fatalf("want the director_uuid refusal, got %v", err)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.OkToRetry() {
		t.Fatalf("want a non-retriable cloud error, got %T %v", err, err)
	}
	r.wantNothingCreated(t)
}

// TestCreateVM_ManagedUnresolvableProfileRefusesBeforeTheClone runs a managed
// create_vm whose HA profile cannot be resolved. The call fails retriable, as
// it did after the clone, and creates nothing.
func TestCreateVM_ManagedUnresolvableProfileRefusesBeforeTheClone(t *testing.T) {
	t.Parallel()
	r := newManagedRefusalRig(t, nil, errors.New("cluster status read failed"), nil)

	err := r.create(t, "director-uuid-1", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "resolve the HA profile") {
		t.Fatalf("want the profile resolution error, got %v", err)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("want a retriable error, got %T %v", err, err)
	}
	r.wantNothingCreated(t)
}

// TestCreateVM_ManagedDLBBelowPVE92RefusesBeforeTheClone runs a managed
// create_vm with the dynamic load balancer on, on a cluster that meets every
// prerequisite but runs a node below PVE 9.2. The refusal depends on the
// profile and the configuration alone, so it comes before the clone.
func TestCreateVM_ManagedDLBBelowPVE92RefusesBeforeTheClone(t *testing.T) {
	t.Parallel()
	profile := &ha.Profile{
		Mode: config.HAModeManaged, Effective: ha.EffectiveFull, MinVersion: "9.1.4", HAVersionOK: true,
		RulesSupported: true, Clustered: true, Quorate: true, EntryNode: "pve",
		Nodes: []ha.NodeFacts{
			{Name: "pve", Online: true, Votes: 1, VotesKnown: true, Version: "9.1.4"},
			{Name: "pve-2", Online: true, Votes: 1, VotesKnown: true, Version: "9.1.4"},
		},
		Topology: config.HAStorageTopologyShared, TopologySource: ha.TopologyFromConfig,
	}
	enabled := true
	r := newManagedRefusalRig(t, profile, nil, func(c *config.CPIConfig) {
		c.HA.DLB = &config.HADLBConfig{Enabled: &enabled}
	})

	err := r.create(t, "director-uuid-1", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "asks for the dynamic load balancer") {
		t.Fatalf("want the dynamic load balancer refusal, got %v", err)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.OkToRetry() {
		t.Fatalf("want a non-retriable cloud error, got %T %v", err, err)
	}
	r.wantNothingCreated(t)
}
