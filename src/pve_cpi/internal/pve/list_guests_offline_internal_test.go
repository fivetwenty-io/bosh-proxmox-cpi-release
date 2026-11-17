package pve

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// lgProxyUnreachable is the 595 a PVE API node answers when it can't reach the
// node a /nodes/<n>/... request is proxied to, which is what a rebooting
// member's listing returns. It classifies as a transient transport fault, so
// the listing retries it.
func lgProxyUnreachable() error {
	return sdkerrors.ParseAPIError(595, []byte(`{"message":"no route to host","code":595}`))
}

// lgStatusFlipsAfterFirstListing reports node online until its listing has
// been tried once, and offline from then on, the way /cluster/status reads for
// a member that stops answering its API before corosync drops it.
func lgStatusFlipsAfterFirstListing(nodesSvc *lgNodes, node string, members ...string) func() (*sdkcluster.ListStatusResponse, error) {
	return func() (*sdkcluster.ListStatusResponse, error) {
		online := map[string]bool{}
		for _, m := range members {
			online[m] = true
		}
		online[node] = nodesSvc.calls[node] == 0
		return lgStatus(online)()
	}
}

// TestListGuestsAuthoritativeTolerant_MemberGoesOfflineDuringListing: a member
// that the status read before the fan-out still reported online, and that the
// quorate cluster reports offline once its first listing attempt has failed,
// is excluded after that one attempt instead of being retried through the
// whole budget and failing the enumeration.
func TestListGuestsAuthoritativeTolerant_MemberGoesOfflineDuringListing(t *testing.T) {
	t.Parallel()
	nodesSvc := &lgNodes{
		listings:  map[string][]string{"pve1": {`{"vmid": 596}`}, "pve3": {`{"vmid": 700}`}},
		failNodes: map[string]error{"pve2": lgProxyUnreachable()},
	}
	c := &lgClient{
		cluster: &lgCluster{
			nodes:    []string{"pve1", "pve2", "pve3"},
			statusFn: lgStatusFlipsAfterFirstListing(nodesSvc, "pve2", "pve1", "pve3"),
		},
		nodes: nodesSvc,
	}
	guests, excluded, err := ListGuestsAuthoritativeTolerant(lgCtx(), c, nil)
	if err != nil {
		t.Fatalf("a member that went offline during its listing must be tolerated, got: %v", err)
	}
	if len(guests) != 2 {
		t.Fatalf("expected the two online nodes' guests, got %+v", guests)
	}
	if len(excluded) != 1 || excluded[0] != "pve2" {
		t.Fatalf("the member that went offline must come back as excluded, got %v", excluded)
	}
	if got := nodesSvc.calls["pve2"]; got != 1 {
		t.Fatalf("the member must stop being retried once the cluster reports it offline, got %d listing attempts", got)
	}
}

// TestListGuestsAuthoritativeTolerant_OnlineMemberKeepsRetryBudget: a member
// the cluster still reports online after a failed attempt keeps every retry,
// because a blip on a live member is what the retries are for, and the
// enumeration still fails retriable once they run out.
func TestListGuestsAuthoritativeTolerant_OnlineMemberKeepsRetryBudget(t *testing.T) {
	t.Parallel()
	nodesSvc := &lgNodes{
		listings:  map[string][]string{"pve1": {`{"vmid": 596}`}},
		failNodes: map[string]error{"pve2": lgProxyUnreachable()},
	}
	c := &lgClient{
		cluster: &lgCluster{
			nodes:    []string{"pve1", "pve2"},
			statusFn: lgStatus(map[string]bool{"pve1": true, "pve2": true}),
		},
		nodes: nodesSvc,
	}
	_, _, err := ListGuestsAuthoritativeTolerant(lgCtx(), c, nil)
	if err == nil {
		t.Fatal("an online member that can't be listed must still fail the enumeration")
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("a partial fleet must classify retriable, got: %v", err)
	}
	if got, want := nodesSvc.calls["pve2"], transientMaxAttemptsDefault(); got != want {
		t.Fatalf("an online member must keep the full retry budget, got %d attempts, want %d", got, want)
	}
}

// TestListGuestsAuthoritative_StrictIgnoresMemberGoingOffline: the strict form
// never consults the status between attempts, so an absence proof still rides
// out an outage through the whole retry budget and fails rather than drop the
// member.
func TestListGuestsAuthoritative_StrictIgnoresMemberGoingOffline(t *testing.T) {
	t.Parallel()
	nodesSvc := &lgNodes{
		listings:  map[string][]string{"pve1": {`{"vmid": 596}`}},
		failNodes: map[string]error{"pve2": lgProxyUnreachable()},
	}
	c := &lgClient{
		cluster: &lgCluster{
			nodes:    []string{"pve1", "pve2"},
			statusFn: lgStatusFlipsAfterFirstListing(nodesSvc, "pve2", "pve1"),
		},
		nodes: nodesSvc,
	}
	_, err := ListGuestsAuthoritative(lgCtx(), c, nil)
	if err == nil || !strings.Contains(err.Error(), "pve2") {
		t.Fatalf("the strict form must fail naming the member, got: %v", err)
	}
	if got, want := nodesSvc.calls["pve2"], transientMaxAttemptsDefault(); got != want {
		t.Fatalf("the strict form must use the full retry budget, got %d attempts, want %d", got, want)
	}
}

// TestListGuestsAuthoritativeTolerant_EveryMemberExcludedFails: with one member
// reported offline before the fan-out and the only other one going offline
// during its listing, nothing was listed at all. The tolerance never turns a
// fully dark cluster into an empty guest list, so the call fails retriable.
func TestListGuestsAuthoritativeTolerant_EveryMemberExcludedFails(t *testing.T) {
	t.Parallel()
	nodesSvc := &lgNodes{failNodes: map[string]error{"pve2": lgProxyUnreachable()}}
	c := &lgClient{
		cluster: &lgCluster{
			nodes: []string{"pve1", "pve2"},
			statusFn: func() (*sdkcluster.ListStatusResponse, error) {
				return lgStatus(map[string]bool{"pve1": false, "pve2": nodesSvc.calls["pve2"] == 0})()
			},
		},
		nodes: nodesSvc,
	}
	guests, excluded, err := ListGuestsAuthoritativeTolerant(lgCtx(), c, nil)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("every member excluded must fail retriable, got guests=%v excluded=%v err=%v", guests, excluded, err)
	}
	if !strings.Contains(err.Error(), "every cluster member reports offline") {
		t.Fatalf("the error must say that every member is offline, got: %v", err)
	}
}

// TestListGuestsAuthoritativeTolerant_StatusRecheckTakesOneAttempt: the status
// read after a failed listing attempt gets a single attempt of its own. When
// the status endpoint itself blips, a full budget there would add up to eight
// status reads after every failed listing attempt.
func TestListGuestsAuthoritativeTolerant_StatusRecheckTakesOneAttempt(t *testing.T) {
	t.Parallel()
	nodesSvc := &lgNodes{
		listings:  map[string][]string{"pve1": {`{"vmid": 596}`}},
		failNodes: map[string]error{"pve2": lgProxyUnreachable()},
	}
	statusCalls := 0
	c := &lgClient{
		cluster: &lgCluster{
			nodes: []string{"pve1", "pve2"},
			statusFn: func() (*sdkcluster.ListStatusResponse, error) {
				statusCalls++
				if statusCalls == 1 {
					return lgStatus(map[string]bool{"pve1": true, "pve2": true})()
				}
				return nil, lgProxyUnreachable()
			},
		},
		nodes: nodesSvc,
	}
	if _, _, err := ListGuestsAuthoritativeTolerant(lgCtx(), c, nil); err == nil {
		t.Fatal("a member that never answers and is still reported online must fail the enumeration")
	}
	listAttempts := nodesSvc.calls["pve2"]
	if listAttempts != transientMaxAttemptsDefault() {
		t.Fatalf("the online member must keep the full retry budget, got %d attempts", listAttempts)
	}
	if want := 1 + listAttempts; statusCalls != want {
		t.Fatalf("status read %d times; want %d, one before the fan-out and one per failed listing attempt", statusCalls, want)
	}
}

// TestListMemberGuests_RidesOutBlipOnMemberReportedOffline: a member the
// cluster reports offline is listed anyway, with the full retry budget, so a
// corosync blip that ends within the budget yields the member's guests.
func TestListMemberGuests_RidesOutBlipOnMemberReportedOffline(t *testing.T) {
	t.Parallel()
	nodesSvc := &lgNodes{
		listings:  map[string][]string{"pve2": {`{"vmid": 700}`}},
		failNodes: map[string]error{"pve2": lgProxyUnreachable()},
	}
	c := &lgClient{
		cluster: &lgCluster{nodes: []string{"pve1", "pve2"}, statusFn: lgStatus(map[string]bool{"pve1": true, "pve2": false})},
		nodes:   nodesSvc,
	}
	// The backoff between attempts stands in for the clock: the blip ends
	// after pve2's third failed listing.
	ctx := WithTestBackoff(context.Background(), func(int) time.Duration {
		if nodesSvc.calls["pve2"] >= 3 {
			delete(nodesSvc.failNodes, "pve2")
		}
		return 0
	})
	guests, err := ListMemberGuests(ctx, c, nil, []string{"pve2"})
	if err != nil {
		t.Fatalf("a blip within the retry budget must be ridden out, got: %v", err)
	}
	if len(guests) != 1 || guests[0].VMID != 700 || guests[0].Node != "pve2" {
		t.Fatalf("guests = %+v; want pve2's guest 700", guests)
	}
	if got := nodesSvc.calls["pve2"]; got != 4 {
		t.Fatalf("pve2 was listed %d times; want 4", got)
	}
	if nodesSvc.calls["pve1"] != 0 {
		t.Fatal("only the named members may be listed")
	}
}

// TestListMemberGuests_FailsRetriableAfterBudget: a member that never answers
// fails the call after the full budget with a retriable error that names it.
func TestListMemberGuests_FailsRetriableAfterBudget(t *testing.T) {
	t.Parallel()
	nodesSvc := &lgNodes{failNodes: map[string]error{"pve2": lgProxyUnreachable()}}
	c := &lgClient{
		cluster: &lgCluster{nodes: []string{"pve1", "pve2"}, statusFn: lgStatus(map[string]bool{"pve1": true, "pve2": false})},
		nodes:   nodesSvc,
	}
	_, err := ListMemberGuests(lgCtx(), c, nil, []string{"pve2"})
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !strings.Contains(err.Error(), "pve2") {
		t.Fatalf("want a retriable error naming pve2, got: %v", err)
	}
	if got, want := nodesSvc.calls["pve2"], transientMaxAttemptsDefault(); got != want {
		t.Fatalf("pve2 was listed %d times; want the full budget of %d", got, want)
	}
}
