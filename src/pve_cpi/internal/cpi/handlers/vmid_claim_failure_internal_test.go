package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// claimDeniedErr is PVE's answer to a lock create from a token without
// Pool.Allocate on the lock pool's path.
func claimDeniedErr() error {
	return sdkerrors.ParseAPIError(403,
		[]byte(`{"data":null,"message":"Permission check failed (/pool/bosh-lock-vm-100, Pool.Allocate)\n"}`))
}

// claimReleaseFailure is a PVE answer that fails a sentinel delete without
// saying the pool is gone.
func claimReleaseFailure() error {
	return sdkerrors.ParseAPIError(500,
		[]byte(`{"data":null,"message":"cfs-lock 'file-user_cfg' error: got lock request timeout\n"}`))
}

// claimRefusalWarnings counts the warnings that say PVE refused a claim.
func claimRefusalWarnings(obs *log.Observer) int {
	n := 0
	for _, e := range obs.All() {
		if e.Level == log.LevelWarn && strings.HasPrefix(e.Message, "VMID claim: PVE refused") {
			n++
		}
	}
	return n
}

// TestWithVMIDClaim_PermissionRefusalRunsADisposalUnclaimed shows a token that
// cannot create the lock pool still deletes and rolls back, unclaimed, and
// that the request warns once however many claims it is refused.
func TestWithVMIDClaim_PermissionRefusalRunsADisposalUnclaimed(t *testing.T) {
	pools := newExclusiveLockPools()
	pools.createErr = claimDeniedErr()
	deps := claimDeps(pools)
	logger, obs := log.NewObservedLogger(log.LevelWarn)

	outer, inner := false, false
	err := withVMIDClaim(crhCtx(), deps, 100, "delete_vm/100", claimForDisposal, logger, func(ctx context.Context) error {
		outer = true
		if _, held := heldVMIDClaimWindow(ctx, 100); held {
			t.Error("a refused claim must not be recorded as held")
		}
		return withVMIDClaim(ctx, deps, 100, "create_vm-rollback/100", claimForDisposal, logger,
			func(context.Context) error {
				inner = true
				return nil
			})
	})
	if err != nil || !outer || !inner {
		t.Fatalf("a refused claim must let the disposal run unclaimed; outer %v, inner %v, err %v", outer, inner, err)
	}
	if got := claimRefusalWarnings(obs); got != 1 {
		t.Fatalf("the refusal must be warned about once per request, got %d warnings", got)
	}
	if pools.deletes != 0 {
		t.Fatalf("an unclaimed disposal has no sentinel to release; %d deletes", pools.deletes)
	}
}

// TestWithVMIDClaim_SweepFailsClosedOnAPermissionRefusal shows the sweep
// never runs its teardown without the claim that proves a VM abandoned.
func TestWithVMIDClaim_SweepFailsClosedOnAPermissionRefusal(t *testing.T) {
	pools := newExclusiveLockPools()
	pools.createErr = claimDeniedErr()

	ran := false
	err := withVMIDClaim(crhCtx(), claimDeps(pools), 100, "deferred-teardown-sweep/100", claimForSweep,
		log.NewNopLogger(), func(context.Context) error {
			ran = true
			return nil
		})
	if ran {
		t.Fatal("the sweep must not tear a VM down without its claim")
	}
	if !pve.IsClusterLockPermissionRefused(err) {
		t.Fatalf("want the permission refusal back, got %v", err)
	}
}

// TestWithVMIDClaim_OnlyAPermissionRefusalRunsUnclaimed shows that a refused
// create other than a 403, and an unknown outcome, keep failing the work.
func TestWithVMIDClaim_OnlyAPermissionRefusalRunsUnclaimed(t *testing.T) {
	cases := map[string]error{
		"400 bad request": sdkerrors.ParseAPIError(400, []byte(`{"data":null,"message":"invalid pool name\n"}`)),
		"401 rejected":    sdkerrors.ParseAPIError(401, []byte(`{"data":null,"message":"authentication failure\n"}`)),
		"unknown outcome": claimReleaseFailure(),
	}
	for name, createErr := range cases {
		t.Run(name, func(t *testing.T) {
			pools := newExclusiveLockPools()
			pools.createErr = createErr
			ran := false
			err := withVMIDClaim(crhCtx(), claimDeps(pools), 100, "delete_vm/100", claimForDisposal, log.NewNopLogger(),
				func(context.Context) error {
					ran = true
					return nil
				})
			if ran || err == nil {
				t.Fatalf("only a permission refusal may run the work unclaimed; ran %v, err %v", ran, err)
			}
			if !okToRetryCPIError(err) {
				t.Fatalf("the claim failure must stay retriable, got %v", err)
			}
		})
	}
}

// TestRunCreateAttempt_PermissionRefusalCreatesUnclaimed shows a create on a
// token that cannot create the lock pool goes ahead without the claim.
func TestRunCreateAttempt_PermissionRefusalCreatesUnclaimed(t *testing.T) {
	pools := newExclusiveLockPools()
	pools.createErr = claimDeniedErr()
	deps := claimDeps(pools)
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	ctx, releaseClaims := bindCreateVMIDClaims(context.Background(), time.Hour, logger)

	attempts := 0
	for _, candidate := range []int{200, 201} {
		if err := runCreateAttempt(ctx, deps, candidate, logger, func() error {
			attempts++
			return nil
		}); err != nil {
			t.Fatalf("candidate %d: a refused claim must not fail the create: %v", candidate, err)
		}
		if _, held := heldVMIDClaimWindow(ctx, candidate); held {
			t.Fatalf("candidate %d: a refused claim must not be recorded as held", candidate)
		}
	}
	if attempts != 2 {
		t.Fatalf("every candidate must run; %d ran", attempts)
	}
	if got := claimRefusalWarnings(obs); got != 1 {
		t.Fatalf("the refusal must be warned about once per create, got %d warnings", got)
	}
	if err := releaseClaims(nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestWithVMIDClaim_RetriesAFailedRelease shows a release that PVE fails once
// is sent again and frees the claim.
func TestWithVMIDClaim_RetriesAFailedRelease(t *testing.T) {
	pools := newExclusiveLockPools()
	pools.deleteErrs = []error{claimReleaseFailure()}

	if err := withVMIDClaim(crhCtx(), claimDeps(pools), 100, "delete_vm/100", claimForDisposal, log.NewNopLogger(),
		func(context.Context) error { return nil }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pools.held(100) {
		t.Fatal("the retried release must free the claim")
	}
	if pools.deletes != 2 {
		t.Fatalf("want the failed delete and one retry, got %d deletes", pools.deletes)
	}
}

// TestVMIDClaimSet_KeepsAClaimWhoseReleaseKeepsFailing shows a claim whose
// every release attempt fails stays in the set, so the create's final
// releaseAll tries it again, and a later candidate on that VMID reads as held.
func TestVMIDClaimSet_KeepsAClaimWhoseReleaseKeepsFailing(t *testing.T) {
	pools := newExclusiveLockPools()
	deps := claimDeps(pools)
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	ctx, releaseClaims := bindCreateVMIDClaims(context.Background(), time.Hour, logger)

	failures := make([]error, vmidClaimReleaseAttempts)
	for i := range failures {
		failures[i] = claimReleaseFailure()
	}
	pools.deleteErrs = failures
	err := runCreateAttempt(ctx, deps, 200, logger, func() error { return errors.New("clone failed") })
	if err == nil {
		t.Fatal("the attempt's own error must come back")
	}
	if pools.deletes != vmidClaimReleaseAttempts {
		t.Fatalf("want %d release attempts, got %d", vmidClaimReleaseAttempts, pools.deletes)
	}
	if !pools.held(200) {
		t.Fatal("the sentinel must still stand after every release failed")
	}
	if _, held := heldVMIDClaimWindow(ctx, 200); !held {
		t.Fatal("a claim whose release failed must stay in the set")
	}
	warned := false
	for _, e := range obs.All() {
		if e.Level == log.LevelWarn && e.Attrs[metadataKeyVMID] != nil {
			warned = true
		}
	}
	if !warned {
		t.Fatal("a failed release must be logged")
	}

	retry := runCreateAttempt(ctx, deps, 200, logger, func() error {
		t.Error("a VMID whose claim this request still holds must not be created on again")
		return nil
	})
	if !isVMIDClaimHeld(retry) {
		t.Fatalf("a VMID the set still holds must read as held so the create draws a fresh one, got %v", retry)
	}

	if err := releaseClaims(nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pools.held(200) {
		t.Fatal("the create's final release must retry the claim and free it")
	}
}

// TestWithVMIDClaim_ReleasesAfterTheRequestIsCancelled shows a request that
// is cancelled while it holds the claim still deletes the sentinel.
func TestWithVMIDClaim_ReleasesAfterTheRequestIsCancelled(t *testing.T) {
	pools := newExclusiveLockPools()
	ctx, cancel := context.WithCancel(crhCtx())
	defer cancel()

	err := withVMIDClaim(ctx, claimDeps(pools), 100, "delete_vm/100", claimForDisposal, log.NewNopLogger(),
		func(context.Context) error {
			cancel()
			return nil
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pools.held(100) {
		t.Fatal("a cancelled request must still release its claim")
	}
}

// TestRunInVMIDClaimWindow_ACutOffIsRetriable shows that work the claim's
// window cuts off reaches the Director as retriable, whatever the interrupted
// call returned, while a request that was itself cancelled keeps its error.
func TestRunInVMIDClaimWindow_ACutOffIsRetriable(t *testing.T) {
	cut := func(ctx context.Context) error {
		<-ctx.Done()
		return cpierrors.Cloud("delete_vm: read config: %v", ctx.Err())
	}
	err := runInVMIDClaimWindow(context.Background(), 100, time.Now().Add(20*time.Millisecond), cut)
	if !okToRetryCPIError(err) {
		t.Fatalf("a cut by the claim's window must be retriable, got %v", err)
	}

	parent, cancel := context.WithCancel(context.Background())
	cancel()
	err = runInVMIDClaimWindow(parent, 100, time.Now().Add(time.Hour), cut)
	if okToRetryCPIError(err) {
		t.Fatalf("a cancelled request is not a window cut, got %v", err)
	}

	if err := runInVMIDClaimWindow(context.Background(), 100, time.Now().Add(time.Hour),
		func(context.Context) error { return nil }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestBindCreateVMIDClaims_ACutOffCreateIsRetriable shows a create that its
// claim's bound cuts off fails retriably, and that a create under a request
// deadline keeps its own error for the dispatcher to judge.
func TestBindCreateVMIDClaims_ACutOffCreateIsRetriable(t *testing.T) {
	ctx, finish := bindCreateVMIDClaims(context.Background(), 20*time.Millisecond, log.NewNopLogger())
	<-ctx.Done()
	if err := finish(cpierrors.Cloud("create_vm: clone: %v", ctx.Err())); !okToRetryCPIError(err) {
		t.Fatalf("a create its bound cut off must fail retriably, got %v", err)
	}

	parent, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	ctx, finish = bindCreateVMIDClaims(parent, time.Hour, log.NewNopLogger())
	<-ctx.Done()
	if err := finish(cpierrors.Cloud("create_vm: clone: %v", ctx.Err())); okToRetryCPIError(err) {
		t.Fatalf("a request deadline is the dispatcher's to rewrite, got %v", err)
	}
}
