package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func claimDeps(pools pve.PoolService) Deps {
	return Deps{
		Config: lrTokenConfig(),
		PVE:    claimClient{lrClient: &lrClient{nodes: &lrNodesStub{}, qemu: &lrQEMUStub{}}, pools: pools},
		Logger: log.NewNopLogger(),
	}
}

func TestWithVMIDClaim_HoldsTheClaimForTheWorkAndReleasesIt(t *testing.T) {
	pools := newExclusiveLockPools()
	deps := claimDeps(pools)
	logger := log.NewNopLogger()

	ran := false
	err := withVMIDClaim(crhCtx(), deps, 100, "delete_vm/100", true, logger, func(ctx context.Context) error {
		ran = true
		if !pools.held(100) {
			t.Error("the claim must be held while the work runs")
		}
		exp, ok := pools.expiry(100)
		if !ok {
			t.Fatal("the claim must record its expiry")
		}
		dl, ok := ctx.Deadline()
		if !ok {
			t.Fatal("the work must run under a deadline")
		}
		if !dl.Before(exp) {
			t.Errorf("the work's deadline %v must end before the claim's expiry %v, so an expired claim "+
				"belongs to a process that has stopped working", dl, exp)
		}
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("ran %v, err %v", ran, err)
	}
	if pools.held(100) {
		t.Fatal("the claim must be released once the work returns")
	}
}

func TestWithVMIDClaim_NestedClaimAndLockReuseTheOuterClaim(t *testing.T) {
	pools := newExclusiveLockPools()
	deps := claimDeps(pools)
	logger := log.NewNopLogger()

	nestedRan, lockRan := false, false
	err := withVMIDClaim(crhCtx(), deps, 100, "create_vm/100", true, logger, func(ctx context.Context) error {
		if err := withVMIDClaim(ctx, deps, 100, "create_vm-rollback/100", true, logger, func(context.Context) error {
			nestedRan = true
			return nil
		}); err != nil {
			return err
		}
		return withVMIDLock(ctx, pools, 100, "tag write", logger, func() error {
			lockRan = true
			return nil
		})
	})
	if err != nil || !nestedRan || !lockRan {
		t.Fatalf("nested %v, lock %v, err %v", nestedRan, lockRan, err)
	}
	if pools.creates != 1 {
		t.Fatalf("the nested claim and lock must reuse the outer claim; %d sentinel creates", pools.creates)
	}
	if pools.held(100) {
		t.Fatal("the outer claim must be released")
	}
}

func TestWithVMIDClaim_TryOnceLeavesALiveHolderAlone(t *testing.T) {
	pools := newExclusiveLockPools()
	pools.hold(100, "create_vm/100", time.Now().Add(time.Hour))
	deps := claimDeps(pools)

	ran := false
	err := withVMIDClaim(crhCtx(), deps, 100, "deferred-teardown-sweep/100", false, log.NewNopLogger(),
		func(context.Context) error {
			ran = true
			return nil
		})
	if ran {
		t.Fatal("the work must not run while another process holds the claim")
	}
	if !isVMIDClaimHeld(err) || !okToRetryCPIError(err) {
		t.Fatalf("want a retriable error carrying errVMIDClaimHeld, got %v", err)
	}
	if comment, _ := pools.comment(100); comment == "" || !pools.held(100) {
		t.Fatal("the live holder's claim must stay in place")
	}
}

func TestWithVMIDClaim_TryOnceStealsAnExpiredClaim(t *testing.T) {
	pools := newExclusiveLockPools()
	pools.hold(100, "create_vm/100", time.Now().Add(-time.Minute))
	deps := claimDeps(pools)

	ran := false
	err := withVMIDClaim(crhCtx(), deps, 100, "deferred-teardown-sweep/100", false, log.NewNopLogger(),
		func(context.Context) error {
			ran = true
			return nil
		})
	if err != nil || !ran {
		t.Fatalf("an expired claim belongs to a stopped process and must be taken; ran %v, err %v", ran, err)
	}
}

func TestWithVMIDClaim_ClosedWindowRunsNothing(t *testing.T) {
	pools := newExclusiveLockPools()
	deps := claimDeps(pools)
	ctx := vmidClaimContextWithWindow(crhCtx(), 100, time.Now().Add(-time.Second))

	ran := false
	err := withVMIDClaim(ctx, deps, 100, "delete_vm/100", true, log.NewNopLogger(), func(context.Context) error {
		ran = true
		return nil
	})
	if ran || !okToRetryCPIError(err) {
		t.Fatalf("a closed window must refuse the work with a retriable error; ran %v, err %v", ran, err)
	}
	err = withVMIDLock(ctx, pools, 100, "tag write", log.NewNopLogger(), func() error {
		ran = true
		return nil
	})
	if ran || !okToRetryCPIError(err) {
		t.Fatalf("withVMIDLock under a closed claim must refuse; ran %v, err %v", ran, err)
	}
	if pools.creates != 0 {
		t.Fatalf("a held claim must never be acquired again; %d sentinel creates", pools.creates)
	}
}

func TestWithVMIDClaim_NoPoolServiceRunsUnclaimed(t *testing.T) {
	ran := false
	err := withVMIDClaim(crhCtx(), claimDeps(nil), 100, "delete_vm/100", true, log.NewNopLogger(),
		func(context.Context) error {
			ran = true
			return nil
		})
	if err != nil || !ran {
		t.Fatalf("ran %v, err %v", ran, err)
	}
}

func TestRunCreateAttempt_ClaimsEachCandidateAndReleasesOnFailure(t *testing.T) {
	pools := newExclusiveLockPools()
	deps := claimDeps(pools)
	logger := log.NewNopLogger()
	ctx, releaseClaims := bindCreateVMIDClaims(context.Background(), logger)

	dl, ok := ctx.Deadline()
	if !ok || time.Until(dl) > createVMIDClaimBodyBudget || time.Until(dl) < createVMIDClaimBodyBudget-time.Minute {
		t.Fatalf("a create with no deadline must be bounded by the body budget; deadline %v, ok %v", dl, ok)
	}

	err := runCreateAttempt(ctx, deps, 200, logger, func() error {
		if !pools.held(200) {
			t.Error("the candidate must be claimed before its clone")
		}
		return errors.New("clone failed")
	})
	if err == nil || pools.held(200) {
		t.Fatalf("a failed attempt must return its error and release its claim; err %v, held %v", err, pools.held(200))
	}

	pools.hold(201, "create_vm/201", time.Now().Add(time.Hour))
	called := false
	err = runCreateAttempt(ctx, deps, 201, logger, func() error {
		called = true
		return nil
	})
	if called || !isVMIDClaimHeld(err) {
		t.Fatalf("a candidate another process claims must be skipped for a fresh one; called %v, err %v", called, err)
	}

	if err := runCreateAttempt(ctx, deps, 202, logger, func() error { return nil }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pools.held(202) {
		t.Fatal("a successful attempt keeps its claim for the rest of the create")
	}
	if exp, ok := pools.expiry(202); !ok || exp.Before(dl.Add(rollbackCleanupTimeout)) {
		t.Fatalf("the create's claim must outlast its deadline plus the rollback; expiry %v, deadline %v", exp, dl)
	}

	releaseClaims()
	if pools.held(202) {
		t.Fatal("the create's release must give up every claim it holds")
	}
	if ctx.Err() == nil {
		t.Fatal("the release must end the create's bound")
	}
}

func TestVMIDClaimTTLs(t *testing.T) {
	if got := disposalClaimTTL(context.Background()); got != vmidLockTTLNow() {
		t.Fatalf("a disposal without a deadline gets the lock TTL, got %v", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	if got := disposalClaimTTL(ctx); got < 2*time.Hour {
		t.Fatalf("a disposal's claim must cover its request, got %v", got)
	}
	if got := createClaimTTL(ctx); got < 2*time.Hour+rollbackCleanupTimeout {
		t.Fatalf("a create's claim must cover its request and the rollback, got %v", got)
	}
	if got := createClaimTTL(context.Background()); got < createVMIDClaimBodyBudget+rollbackCleanupTimeout {
		t.Fatalf("a create without a deadline must be covered for the body budget, got %v", got)
	}
}

// identityQEMU answers each node's config, and not found for a node with none.
type identityQEMU struct {
	qemu.Service
	configs map[string]map[string]any
	readErr error
}

func (q identityQEMU) Config(_ context.Context, node string, _ int) (map[string]any, error) {
	if q.readErr != nil {
		return nil, q.readErr
	}
	cfg, ok := q.configs[node]
	if !ok {
		return nil, &sdkerrors.APIError{HTTPCode: 404}
	}
	return cfg, nil
}

type identityCluster struct {
	cluster.Service
	rows cluster.ListResourcesResponse
}

func (c identityCluster) ListResources(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
	rows := c.rows
	return &rows, nil
}

type identityClient struct {
	pve.Client
	qemu    identityQEMU
	cluster identityCluster
}

func (c identityClient) QEMU() qemu.Service { return c.qemu }
func (c identityClient) Nodes() sdknodes.Service {
	return &configPendingNodes{config: c.qemu.Config}
}
func (c identityClient) Cluster() cluster.Service { return c.cluster }

func identityConfig(uuid, ctime, vmgenid string) map[string]any {
	cfg := map[string]any{}
	if uuid != "" {
		cfg["smbios1"] = "uuid=" + uuid + ",manufacturer=bosh"
	}
	if ctime != "" {
		cfg["meta"] = "creation-qemu=9.0.2,ctime=" + ctime
	}
	if vmgenid != "" {
		cfg["vmgenid"] = vmgenid
	}
	return cfg
}

func identityIndexRow(t *testing.T, vmid int, node string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"vmid": vmid, "node": node, "type": "qemu"})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCleanupTargetStillOurs(t *testing.T) {
	destroyed := captureVMIdentity("pve01", 100, identityConfig("AAAA-1111", "1700000000", "gen-1"))
	other := identityConfig("BBBB-2222", "1800000000", "gen-2")
	same := identityConfig("aaaa-1111", "1700000000", "gen-1")

	cases := map[string]struct {
		configs map[string]map[string]any
		index   []json.RawMessage
		readErr error
		want    bool
	}{
		"the VMID is free":                      {want: true},
		"the destroyed VM is still at the VMID": {configs: map[string]map[string]any{"pve01": same}, want: true},
		"a different VM took the VMID":          {configs: map[string]map[string]any{"pve01": other}},
		"the config read fails":                 {readErr: errors.New("500 internal error")},
		"only the stale index row is left": {
			index: []json.RawMessage{identityIndexRow(t, 100, "pve01")}, want: true,
		},
		"a different VM took the VMID on another node": {
			configs: map[string]map[string]any{"pve02": other},
			index:   []json.RawMessage{identityIndexRow(t, 100, "pve02")},
		},
		"the destroyed VM moved to another node": {
			configs: map[string]map[string]any{"pve02": same},
			index:   []json.RawMessage{identityIndexRow(t, 100, "pve02")},
			want:    true,
		},
		"the VMID holds a VM with no identity marks": {configs: map[string]map[string]any{"pve01": {}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			deps := Deps{Config: lrTokenConfig(), Logger: log.NewNopLogger(), PVE: identityClient{
				qemu:    identityQEMU{configs: tc.configs, readErr: tc.readErr},
				cluster: identityCluster{rows: tc.index},
			}}
			if got := cleanupTargetStillOurs(crhCtx(), deps, destroyed, "agent", log.NewNopLogger()); got != tc.want {
				t.Fatalf("cleanupTargetStillOurs = %v, want %v", got, tc.want)
			}
			ran := false
			runPostDestroyCleanup(crhCtx(), deps, destroyed, "agent", log.NewNopLogger(), func() { ran = true })
			if ran != tc.want {
				t.Fatalf("the cleanup ran %v, want %v", ran, tc.want)
			}
		})
	}
}

func TestSameVMIdentity_TheFirstSharedMarkDecides(t *testing.T) {
	id := captureVMIdentity("pve01", 100, identityConfig("AAAA-1111", "", "gen-1"))
	if !id.sameVMIdentity(identityConfig("AAAA-1111", "", "gen-after-rollback")) {
		t.Fatal("a snapshot rollback changes only vmgenid; the SMBIOS UUID must decide")
	}
	if id.sameVMIdentity(identityConfig("CCCC-3333", "", "gen-1")) {
		t.Fatal("a different SMBIOS UUID is a different VM")
	}
	genOnly := captureVMIdentity("pve01", 100, identityConfig("", "", "gen-1"))
	if !genOnly.sameVMIdentity(identityConfig("", "", "GEN-1")) || genOnly.sameVMIdentity(identityConfig("", "", "gen-2")) {
		t.Fatal("vmgenid decides when it is the only shared mark")
	}
	if captureVMIdentity("pve01", 100, identityConfig("", "", "1")).sameVMIdentity(identityConfig("", "", "1")) {
		t.Fatal("a vmgenid of 1 is the auto request, not an identity")
	}
}

const lockedMigrateStopMsg = "500 unable to stop VM 100: VM is locked (migrate)\n"

func TestRetryStopWithSkiplock_RefusesAMigrateLock(t *testing.T) {
	nodes := &lrNodesStub{}
	deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: nodes, qemu: &lrQEMUStub{}}, Logger: log.NewNopLogger()}

	_, err := retryStopWithSkiplock(crhCtx(), deps, "create_vm: rollback", "pve01", 100,
		errors.New(lockedMigrateStopMsg), log.NewNopLogger())
	if !errors.Is(err, errSkiplockStopDuringMigration) || !okToRetryCPIError(err) {
		t.Fatalf("want a retriable migration deferral, got %v", err)
	}
	if len(nodes.stopCalls) != 0 {
		t.Fatalf("no skiplock stop may be sent during a migration, got %d", len(nodes.stopCalls))
	}
}

func TestRetryStopWithSkiplock_ReadsTheLockWhenTheErrorDoesNotNameIt(t *testing.T) {
	origErr := errors.New("500 unable to stop VM 100: VM is locked")
	cases := map[string]struct {
		config    func(int) (map[string]any, error)
		destroyed bool
		wantStop  bool
		wantErr   func(error) bool
	}{
		"migrate lock": {
			config:  func(int) (map[string]any, error) { return map[string]any{"lock": "migrate"}, nil },
			wantErr: func(err error) bool { return errors.Is(err, errSkiplockStopDuringMigration) && okToRetryCPIError(err) },
		},
		"unreadable lock": {
			config: func(int) (map[string]any, error) { return nil, errors.New("500 internal error") },
			wantErr: func(err error) bool {
				return errors.Is(err, errSkiplockStopDuringMigration) && errors.Is(err, origErr)
			},
		},
		"VM already gone": {
			destroyed: true,
			wantErr:   func(err error) bool { return errors.Is(err, origErr) },
		},
		"backup lock": {
			config:   func(int) (map[string]any, error) { return map[string]any{"lock": "backup"}, nil },
			wantStop: true,
			wantErr:  func(err error) bool { return err == nil },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			nodes := &lrNodesStub{destroyed: tc.destroyed}
			q := &lrQEMUStub{configFn: tc.config}
			deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

			_, err := retryStopWithSkiplock(crhCtx(), deps, "delete_vm", "pve01", 100, origErr, log.NewNopLogger())
			if !tc.wantErr(err) {
				t.Fatalf("unexpected error %v", err)
			}
			if stopped := len(nodes.stopCalls) == 1; stopped != tc.wantStop {
				t.Fatalf("skiplock stop sent %v, want %v", stopped, tc.wantStop)
			}
		})
	}
}

func TestStopVMForRollback_DefersAMigratingVM(t *testing.T) {
	nodes := &lrNodesStub{}
	q := &lrQEMUStub{stopErr: errors.New(lockedMigrateStopMsg)}
	deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

	err := stopVMForRollback(crhCtx(), deps, "pve01", 100, log.NewNopLogger())
	if !errors.Is(err, errSkiplockStopDuringMigration) {
		t.Fatalf("want the migration deferral, got %v", err)
	}
	if len(nodes.stopCalls) != 0 {
		t.Fatal("no skiplock stop may be sent during a migration")
	}
}

func TestStopVMForRollback_OtherStopFailuresAreBestEffort(t *testing.T) {
	q := &lrQEMUStub{stopErr: errors.New("500 unexpected failure")}
	deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: &lrNodesStub{}, qemu: q}, Logger: log.NewNopLogger()}
	if err := stopVMForRollback(crhCtx(), deps, "pve01", 100, log.NewNopLogger()); err != nil {
		t.Fatalf("a stop failure other than the migration refusal is left to the halt wait, got %v", err)
	}
}

func TestCleanupVM_MigratingVMIsNotStoppedOrDestroyed(t *testing.T) {
	nodes := &lrNodesStub{}
	q := &lrQEMUStub{stopErr: errors.New(lockedMigrateStopMsg), status: "running"}
	deps := Deps{Config: lrRootPamConfig(), PVE: &lrClient{nodes: nodes, qemu: q}, Logger: log.NewNopLogger()}

	cleanupVM(crhCtx(), deps, "pve01", 100, nil, log.NewNopLogger())

	if len(nodes.stopCalls) != 0 || len(nodes.deleteCalls) != 0 {
		t.Fatalf("a migrating VM must be left for the sweep; skiplock stops %d, destroys %d",
			len(nodes.stopCalls), len(nodes.deleteCalls))
	}
}

func TestRemoveBirthTag_RetriesADigestRefusal(t *testing.T) {
	removed := false
	q := &lrQEMUStub{configFn: func(int) (map[string]any, error) {
		if removed {
			return map[string]any{"tags": "bosh-cpi", "digest": "d3"}, nil
		}
		return map[string]any{"tags": "bosh-cpi;bosh-creating-123", "digest": "d2"}, nil
	}}
	nodes := &lrNodesStub{updateErrs: []error{digestRefusal(), nil}, onUpdate: func() { removed = true }}

	if err := removeBirthTag(crhCtx(), birthTagRemovalDeps(nodes, q), "pve01", 100, log.NewNopLogger()); err != nil {
		t.Fatalf("a digest refusal must be retried with a fresh read, got %v", err)
	}
	if nodes.updateCalls != 2 {
		t.Fatalf("want the refused write and its retry, got %d writes", nodes.updateCalls)
	}
}

func TestRemoveBirthTag_FailsAfterRepeatedDigestRefusals(t *testing.T) {
	q := &lrQEMUStub{configFn: func(int) (map[string]any, error) {
		return map[string]any{"tags": "bosh-cpi;bosh-creating-123", "digest": "d2"}, nil
	}}
	nodes := &lrNodesStub{updateErrs: []error{digestRefusal()}}

	err := removeBirthTag(crhCtx(), birthTagRemovalDeps(nodes, q), "pve01", 100, log.NewNopLogger())
	if err == nil {
		t.Fatal("a birth tag that cannot be removed must fail the create, or the sweep would reap the VM")
	}
	if nodes.updateCalls < birthTagDigestAttempts {
		t.Fatalf("want at least %d attempts, got %d", birthTagDigestAttempts, nodes.updateCalls)
	}
}

func TestHasCPIOwnedPrefix_ReservesTheTeardownQueueTags(t *testing.T) {
	for _, tag := range []string{"bosh-creating-123", "bosh-deleting", "BOSH-DELETING"} {
		if !hasCPIOwnedPrefix(tag) {
			t.Errorf("%q queues a VM for teardown and must be reserved to the CPI", tag)
		}
	}
	if hasCPIOwnedPrefix("custom-tag") {
		t.Error("an ordinary tag must not be reserved")
	}
}
