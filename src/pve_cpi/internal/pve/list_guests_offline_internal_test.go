package pve

import (
	"errors"
	"strings"
	"testing"

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
