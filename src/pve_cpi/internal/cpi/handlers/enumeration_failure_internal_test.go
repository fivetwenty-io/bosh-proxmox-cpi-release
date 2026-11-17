// Package handlers internal tests pinning each caller's policy when the
// authoritative guest enumeration (pve.ListGuestsAuthoritative) fails: the
// disk-holder scan propagates a retriable error, the advertised-route
// refcount fails open by deleting nothing, and the guest-agent IP probe
// fails open by never blocking create_vm. Three call sites, three deliberate
// policies; these tests keep them from silently drifting.
package handlers

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// enumFailureDeps builds Deps whose cluster membership resolves (one node,
// "pve1") but whose per-node qemu listing always errors, so every
// ListGuestsAuthoritative call fails with the partial-fleet classification.
func enumFailureDeps() Deps {
	membership := func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		return ipListResp(ipVMResource(100, "existing-vm")), nil
	}
	return Deps{
		Config: icMinConfig(),
		PVE: &icPVEClient{
			qemuSvc:    &icQEMUService{},
			clusterSvc: &icClusterService{listFn: membership},
			nodesSvc: &icNodesService{
				listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
					return nil, stderrors.New("connection refused")
				},
			},
		},
		Agent:  &icAgentStub{},
		Logger: log.NewNopLogger(),
	}
}

// enumFailureCtx disables retry backoff so RetryOnTransient inside the
// enumeration spins instantly instead of sleeping out its curve.
func enumFailureCtx() context.Context {
	return pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
}

// TestFindVMsHostingDisk_EnumerationFailure_Retriable: the disk-holder scan
// must propagate the enumeration failure as a retriable error, never answer
// from a partial fleet (a false 0-match is silent metadata loss, a false
// 1-match masks multi-attach).
func TestFindVMsHostingDisk_EnumerationFailure_Retriable(t *testing.T) {
	t.Parallel()
	_, err := findVMsHostingDisk(enumFailureCtx(), enumFailureDeps(), "pvd-test-disk")
	if err == nil {
		t.Fatal("a failed enumeration must fail the holder scan, not report zero holders")
	}
	var typed *cpierrors.Error
	if !stderrors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("the holder scan's enumeration failure must classify retriable, got: %v", err)
	}
}

// TestAdvrtTagsHeldByOthers_EnumerationFailure_DeletesNothing: the
// advertised-route refcount fails open. nil is the "scan failed" sentinel
// its callers treat as "delete no subnet"; a partial answer here would turn
// "sole holder" into a wrong verdict that deletes a subnet still in use.
func TestAdvrtTagsHeldByOthers_EnumerationFailure_DeletesNothing(t *testing.T) {
	t.Parallel()
	shared := advrtTagsHeldByOthers(enumFailureCtx(), enumFailureDeps(), 100, log.NewNopLogger())
	if shared != nil {
		t.Fatalf("a failed enumeration must return the nil sentinel (delete nothing), got %v", shared)
	}
}

// TestProbeGuestAgentIPConflict_EnumerationFailure_FailsOpen: the guest-agent
// IP probe is advisory; an infra error enumerating guests must never block
// create_vm.
func TestProbeGuestAgentIPConflict_EnumerationFailure_FailsOpen(t *testing.T) {
	t.Parallel()
	err := probeGuestAgentIPConflict(enumFailureCtx(), enumFailureDeps(), log.NewNopLogger(), []string{"10.0.0.5"})
	if err != nil {
		t.Fatalf("the agent probe must fail open on enumeration failure, got: %v", err)
	}
}

// keep the nodes import anchored to the adapter types this file builds on.
var _ nodes.Service = (*icNodesService)(nil)
