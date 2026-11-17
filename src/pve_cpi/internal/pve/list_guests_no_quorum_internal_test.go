package pve

import (
	"errors"
	"slices"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// lgNoQuorumCluster is a three-member cluster that has lost two members and
// with them its quorum. The survivor, az2-1, answers; the listings of az2-0 and
// az2-2 answer the 595 of an unreachable node on every attempt. status scripts
// /cluster/status, and reads counts its calls.
func lgNoQuorumCluster(
	status func() (*sdkcluster.ListStatusResponse, error), reads *int,
) (*lgClient, *lgNodes) {
	nodesSvc := &lgNodes{
		listings: map[string][]string{"lab-pve-cpi-az2-1": {`{"vmid": 210534}`}},
		failNodes: map[string]error{
			"lab-pve-cpi-az2-0": lgProxyUnreachable(),
			"lab-pve-cpi-az2-2": lgProxyUnreachable(),
		},
	}
	return &lgClient{
		cluster: &lgCluster{
			nodes: []string{"lab-pve-cpi-az2-0", "lab-pve-cpi-az2-1", "lab-pve-cpi-az2-2"},
			statusFn: func() (*sdkcluster.ListStatusResponse, error) {
				*reads++
				return status()
			},
		},
		nodes: nodesSvc,
	}, nodesSvc
}

// lgSurvivorOnly is the status the survivor reports once quorum is gone.
func lgSurvivorOnly(quorate bool) func() (*sdkcluster.ListStatusResponse, error) {
	return lgStatusQuorate(quorate, map[string]bool{
		"lab-pve-cpi-az2-0": false, "lab-pve-cpi-az2-1": true, "lab-pve-cpi-az2-2": false,
	})
}

// assertNoQuorumRefusal checks the refusal of an enumeration that stopped
// early on members the non-quorate entry node lists offline.
func assertNoQuorumRefusal(t *testing.T, err error) {
	t.Helper()
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("the refusal must stay retriable, got: %v", err)
	}
	for _, want := range []string{
		"could not list guests on node(s) lab-pve-cpi-az2-0,lab-pve-cpi-az2-2; refusing to decide from a partial fleet",
		"the entry node does not report the cluster quorate and lists node(s) lab-pve-cpi-az2-0,lab-pve-cpi-az2-2 offline",
		"retry once 'pvecm status' reports the cluster quorate",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must contain %q, got: %v", want, err)
		}
	}
	var enumeration *GuestEnumerationError
	if !errors.As(err, &enumeration) ||
		!slices.Equal(enumeration.NoQuorum, []string{"lab-pve-cpi-az2-0", "lab-pve-cpi-az2-2"}) {
		t.Fatalf("the enumeration error must name both dead members, got: %+v", enumeration)
	}
}

// TestListGuestsAuthoritative_StopsRetryingOfflineMembersWithoutQuorum: the
// strict form tries each dead member once when the entry node does not report
// the cluster quorate and lists the member offline, instead of spending the
// whole retry budget on a member that cannot come back while quorum is gone.
// It still fails, so an absence proof never rests on the partial fleet.
func TestListGuestsAuthoritative_StopsRetryingOfflineMembersWithoutQuorum(t *testing.T) {
	t.Parallel()
	reads := 0
	c, nodesSvc := lgNoQuorumCluster(lgSurvivorOnly(false), &reads)
	_, err := ListGuestsAuthoritative(lgCtx(), c, nil)
	assertNoQuorumRefusal(t, err)
	for _, node := range []string{"lab-pve-cpi-az2-0", "lab-pve-cpi-az2-2"} {
		if got := nodesSvc.calls[node]; got != 1 {
			t.Errorf("%s was listed %d times, want 1", node, got)
		}
	}
	// One read for the quorum check, shared by both members, and one for the
	// offline names in the refusal.
	if reads != 2 {
		t.Errorf("cluster status was read %d times, want 2", reads)
	}
}

// TestListGuestsAuthoritativeTolerant_StopsRetryingOfflineMembersWithoutQuorum:
// the tolerant form withholds its offline tolerance without quorum, and it
// now also stops retrying the members the entry node lists offline.
func TestListGuestsAuthoritativeTolerant_StopsRetryingOfflineMembersWithoutQuorum(t *testing.T) {
	t.Parallel()
	reads := 0
	c, nodesSvc := lgNoQuorumCluster(lgSurvivorOnly(false), &reads)
	_, _, err := ListGuestsAuthoritativeTolerant(lgCtx(), c, nil)
	assertNoQuorumRefusal(t, err)
	for _, node := range []string{"lab-pve-cpi-az2-0", "lab-pve-cpi-az2-2"} {
		if got := nodesSvc.calls[node]; got != 1 {
			t.Errorf("%s was listed %d times, want 1", node, got)
		}
	}
}

// TestListMemberGuests_StopsRetryingOfflineMembersWithoutQuorum: the strict
// listing of named members stops in the same way.
func TestListMemberGuests_StopsRetryingOfflineMembersWithoutQuorum(t *testing.T) {
	t.Parallel()
	reads := 0
	c, nodesSvc := lgNoQuorumCluster(lgSurvivorOnly(false), &reads)
	_, err := ListMemberGuests(lgCtx(), c, nil, []string{"lab-pve-cpi-az2-0", "lab-pve-cpi-az2-2"})
	assertNoQuorumRefusal(t, err)
	if got := nodesSvc.calls["lab-pve-cpi-az2-0"]; got != 1 {
		t.Errorf("lab-pve-cpi-az2-0 was listed %d times, want 1", got)
	}
}

// TestListGuestsAuthoritative_KeepsRetryBudgetWithQuorum: a quorate cluster
// keeps the full budget for the strict form, as before.
func TestListGuestsAuthoritative_KeepsRetryBudgetWithQuorum(t *testing.T) {
	t.Parallel()
	reads := 0
	c, nodesSvc := lgNoQuorumCluster(lgSurvivorOnly(true), &reads)
	_, err := ListGuestsAuthoritative(lgCtx(), c, nil)
	var enumeration *GuestEnumerationError
	if !errors.As(err, &enumeration) || len(enumeration.NoQuorum) != 0 {
		t.Fatalf("a quorate cluster must not stop a listing for lack of quorum, got: %v", err)
	}
	if got, want := nodesSvc.calls["lab-pve-cpi-az2-0"], transientMaxAttemptsDefault(); got != want {
		t.Errorf("lab-pve-cpi-az2-0 was listed %d times, want the full budget of %d", got, want)
	}
}

// TestListGuestsAuthoritative_KeepsRetryBudgetForMemberListedOnline: without
// quorum, a member the entry node still lists online keeps the full budget,
// because only a member listed offline cannot answer.
func TestListGuestsAuthoritative_KeepsRetryBudgetForMemberListedOnline(t *testing.T) {
	t.Parallel()
	reads := 0
	c, nodesSvc := lgNoQuorumCluster(lgStatusQuorate(false, map[string]bool{
		"lab-pve-cpi-az2-0": true, "lab-pve-cpi-az2-1": true, "lab-pve-cpi-az2-2": false,
	}), &reads)
	_, err := ListGuestsAuthoritative(lgCtx(), c, nil)
	var enumeration *GuestEnumerationError
	if !errors.As(err, &enumeration) || !slices.Equal(enumeration.NoQuorum, []string{"lab-pve-cpi-az2-2"}) {
		t.Fatalf("only lab-pve-cpi-az2-2 may stop for lack of quorum, got: %v", err)
	}
	if got, want := nodesSvc.calls["lab-pve-cpi-az2-0"], transientMaxAttemptsDefault(); got != want {
		t.Errorf("lab-pve-cpi-az2-0 was listed %d times, want the full budget of %d", got, want)
	}
}

// TestListGuestsAuthoritative_KeepsRetryBudgetWhenStatusFails: a status read
// that fails proves nothing, so the strict form keeps the full budget.
func TestListGuestsAuthoritative_KeepsRetryBudgetWhenStatusFails(t *testing.T) {
	t.Parallel()
	reads := 0
	c, nodesSvc := lgNoQuorumCluster(func() (*sdkcluster.ListStatusResponse, error) {
		return nil, lgProxyUnreachable()
	}, &reads)
	_, err := ListGuestsAuthoritative(lgCtx(), c, nil)
	if err == nil || strings.Contains(err.Error(), "quorate") {
		t.Fatalf("a failed status read must not name the quorum, got: %v", err)
	}
	if got, want := nodesSvc.calls["lab-pve-cpi-az2-0"], transientMaxAttemptsDefault(); got != want {
		t.Errorf("lab-pve-cpi-az2-0 was listed %d times, want the full budget of %d", got, want)
	}
}
