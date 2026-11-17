package pve

import (
	"context"
	"slices"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// midSweepFlipClient builds a two-member cluster with an empty index, so every
// lookup takes the config-probe path. pve1 comes first in membership, the
// status reports it online until its config has been probed once, and every
// probe of it answers the 595 of a node that has stopped answering. pve2
// holds the VM when holder is set, and answers a clean not-found otherwise.
// The returned function counts the probes pve1 got.
func midSweepFlipClient(holder bool) (*lgClient, func() int) {
	q := &mnQEMU{}
	pve1Probes := func() int {
		return len(slices.DeleteFunc(slices.Clone(q.probed), func(n string) bool { return n != "pve1" }))
	}
	q.configFn = func(node string, _ int) (map[string]any, error) {
		if node == "pve1" {
			return nil, lgProxyUnreachable()
		}
		if holder {
			return map[string]any{"name": "young", "tags": "bosh"}, nil
		}
		return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
	}
	c := &lgClient{
		cluster: &lgCluster{
			nodes: []string{"pve1", "pve2"},
			statusFn: func() (*sdkcluster.ListStatusResponse, error) {
				return lgStatus(map[string]bool{"pve1": pve1Probes() == 0, "pve2": true})()
			},
		},
		nodes: &lgNodes{},
		qemu:  q,
	}
	return c, pve1Probes
}

// TestFindVMAuthoritative_HitBehindMemberGoingOffline: a VM the index has not
// caught up with sits on pve2, and pve1, which the probe tries first, stops
// answering and is then reported offline. pve1 gets one probe, not the whole
// retry budget, and the lookup finds the VM on pve2.
func TestFindVMAuthoritative_HitBehindMemberGoingOffline(t *testing.T) {
	t.Parallel()
	c, pve1Probes := midSweepFlipClient(true)
	loc, err := FindVMAuthoritative(lgCtx(), c, 4120)
	if err != nil {
		t.Fatalf("a member going offline must not fail a lookup whose VM is on another member, got: %v", err)
	}
	if !loc.Found || loc.Node != "pve2" || !loc.FromConfig {
		t.Fatalf("loc = %+v; want the VM found on pve2 by its config", loc)
	}
	if got := pve1Probes(); got != 1 {
		t.Fatalf("pve1 got %d probes; want one before the status shows it offline", got)
	}
}

// TestFindVMAuthoritative_AbsentBehindMemberGoingOffline: with the VM on no
// member, the member that went offline during the first sweep is excluded the
// way a member reported offline before the sweep is, so the absence proof
// completes after one probe of it.
func TestFindVMAuthoritative_AbsentBehindMemberGoingOffline(t *testing.T) {
	t.Parallel()
	c, pve1Probes := midSweepFlipClient(false)
	loc, err := FindVMAuthoritative(lgCtx(), c, 4121)
	if err != nil || loc.Found {
		t.Fatalf("loc=%+v err=%v; want absence proven with the offline member excluded", loc, err)
	}
	if got := pve1Probes(); got != 1 {
		t.Fatalf("pve1 got %d probes; want one before the status shows it offline", got)
	}
}

// TestFindVMAuthoritative_OfflineTieVetoesMemberGoneOfflineMidSweep: a member
// excluded during the sweep is still subject to the offline-member ties, so a
// record that ties the VM to it keeps absence unproven, exactly as it does for
// a member excluded before the sweep.
func TestFindVMAuthoritative_OfflineTieVetoesMemberGoneOfflineMidSweep(t *testing.T) {
	t.Parallel()
	c, _ := midSweepFlipClient(false)
	var tiedTo []string
	tie := func(_ context.Context, _ int, offline []string) (string, string, error) {
		tiedTo = offline
		if slices.Contains(offline, "pve1") {
			return "pve1", "a live allocation-journal record", nil
		}
		return "", "", nil
	}
	loc, err := FindVMAuthoritative(lgCtx(), c, 4122, WithOfflineMemberTie(tie))
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || loc.Found {
		t.Fatalf("loc=%+v err=%v; want a retriable error from the tie", loc, err)
	}
	if !strings.Contains(err.Error(), "pve1") || !strings.Contains(err.Error(), "journal") {
		t.Fatalf("the error must name the tie and the member, got: %v", err)
	}
	if !slices.Equal(tiedTo, []string{"pve1"}) {
		t.Fatalf("the tie was asked about %v; want the member excluded during the sweep", tiedTo)
	}
}

// TestFindVMByConfigProbe_AbsentBehindMemberGoingOffline: the config probe
// excludes a member the cluster begins reporting offline during a sweep, as
// FindVMAuthoritative does, so the absence proof completes after one probe of
// it once the index lists the VM nowhere.
func TestFindVMByConfigProbe_AbsentBehindMemberGoingOffline(t *testing.T) {
	t.Parallel()
	c, pve1Probes := midSweepFlipClient(false)
	loc, err := FindVMByConfigProbe(lgCtx(), c, 4123)
	if err != nil || loc.Found {
		t.Fatalf("loc=%+v err=%v; want absence proven with the offline member excluded", loc, err)
	}
	if got := pve1Probes(); got != 1 {
		t.Fatalf("pve1 got %d probes; want one before the status shows it offline", got)
	}
}

// TestFindVMByConfigProbe_MemberGoingOfflineWithIndexRowFailsFast: when the
// index still lists the VM, the member that went offline leaves absence
// unproven, and the call refuses after one probe of it instead of spending the
// retry budget first.
func TestFindVMByConfigProbe_MemberGoingOfflineWithIndexRowFailsFast(t *testing.T) {
	t.Parallel()
	c, pve1Probes := midSweepFlipClient(false)
	c.cluster.resourcesFn = lgIndex(map[int]string{4123: "pve1"})
	loc, err := FindVMByConfigProbe(lgCtx(), c, 4123)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || loc.Found {
		t.Fatalf("loc=%+v err=%v; want a retriable error and Found=false", loc, err)
	}
	if !strings.Contains(err.Error(), "pve1") || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("the error must name the offline member, got: %v", err)
	}
	if got := pve1Probes(); got != 1 {
		t.Fatalf("pve1 got %d probes; want one before the status shows it offline", got)
	}
}

// TestFindVMByConfigProbe_OnlineMemberKeepsProbeRetryBudget: a member the
// cluster still reports online is merely unreachable, and it may still be
// running the VM. It keeps the whole retry budget and is never excluded.
func TestFindVMByConfigProbe_OnlineMemberKeepsProbeRetryBudget(t *testing.T) {
	t.Parallel()
	c, pve1Probes := midSweepFlipClient(false)
	c.cluster.statusFn = lgStatus(map[string]bool{"pve1": true, "pve2": true})
	loc, err := FindVMByConfigProbe(lgCtx(), c, 4126)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || loc.Found {
		t.Fatalf("loc=%+v err=%v; want a retriable probe failure", loc, err)
	}
	if got := pve1Probes(); got != DefaultTransientMaxAttempts {
		t.Fatalf("pve1 got %d probes; want the full budget of %d", got, DefaultTransientMaxAttempts)
	}
}

// TestFindVMAuthoritative_OnlineMemberKeepsProbeRetryBudget: a member the
// cluster still reports online keeps the whole retry budget, because a brief
// blip on a live node is what the retries exist for.
func TestFindVMAuthoritative_OnlineMemberKeepsProbeRetryBudget(t *testing.T) {
	t.Parallel()
	c, pve1Probes := midSweepFlipClient(false)
	c.cluster.statusFn = lgStatus(map[string]bool{"pve1": true, "pve2": true})
	loc, err := FindVMAuthoritative(lgCtx(), c, 4124)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || loc.Found {
		t.Fatalf("loc=%+v err=%v; want a retriable probe failure", loc, err)
	}
	if got := pve1Probes(); got != DefaultTransientMaxAttempts {
		t.Fatalf("pve1 got %d probes; want the full budget of %d", got, DefaultTransientMaxAttempts)
	}
}

// TestFindVMAuthoritative_ProbeStatusRecheckTakesOneAttempt: the status read
// after a failed probe gets a single attempt, so a status endpoint that blips
// adds one read per failed probe rather than a retry budget of its own.
func TestFindVMAuthoritative_ProbeStatusRecheckTakesOneAttempt(t *testing.T) {
	t.Parallel()
	c, pve1Probes := midSweepFlipClient(false)
	statusCalls := 0
	c.cluster.statusFn = func() (*sdkcluster.ListStatusResponse, error) {
		statusCalls++
		if statusCalls == 1 {
			return lgStatus(map[string]bool{"pve1": true, "pve2": true})()
		}
		return nil, lgProxyUnreachable()
	}
	if _, err := FindVMAuthoritative(lgCtx(), c, 4125); err == nil {
		t.Fatal("a member that never answers and is still reported online must leave absence unproven")
	}
	probes := pve1Probes()
	if probes != DefaultTransientMaxAttempts {
		t.Fatalf("pve1 got %d probes; want the full budget", probes)
	}
	if want := 1 + probes; statusCalls != want {
		t.Fatalf("status read %d times; want %d, one before the sweep and one per failed probe", statusCalls, want)
	}
}
