package pve

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// lgStrictDownCluster is a two-member cluster whose pve2 listing answers the
// 595 of an unreachable node on every attempt, with /cluster/status scripted
// by status.
func lgStrictDownCluster(status func() (*sdkcluster.ListStatusResponse, error)) (*lgClient, *lgNodes) {
	nodesSvc := &lgNodes{
		listings:  map[string][]string{"pve1": {`{"vmid": 596}`}},
		failNodes: map[string]error{"pve2": lgProxyUnreachable()},
	}
	return &lgClient{
		cluster: &lgCluster{nodes: []string{"pve1", "pve2"}, statusFn: status},
		nodes:   nodesSvc,
	}, nodesSvc
}

// TestListGuestsAuthoritative_StrictNamesMemberReportedOfflineAndTheRetry: the
// strict form still gives a member the quorate cluster reports offline the
// whole retry budget, because its callers need every member's guests. Once the
// budget runs out, the refusal names the member as offline and says to retry
// once it is back, and it stays retriable.
func TestListGuestsAuthoritative_StrictNamesMemberReportedOfflineAndTheRetry(t *testing.T) {
	t.Parallel()
	c, nodesSvc := lgStrictDownCluster(lgStatus(map[string]bool{"pve1": true, "pve2": false}))
	_, err := ListGuestsAuthoritative(lgCtx(), c, nil)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("the strict refusal must stay retriable, got: %v", err)
	}
	for _, want := range []string{
		"could not list guests on node(s) pve2; refusing to decide from a partial fleet",
		"the cluster reports node(s) pve2 offline",
		"retry once node(s) pve2 are back in the cluster",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must contain %q, got: %v", want, err)
		}
	}
	var enumeration *GuestEnumerationError
	if !errors.As(err, &enumeration) || !slices.Equal(enumeration.Offline, []string{"pve2"}) {
		t.Fatalf("the enumeration error must name pve2 as offline, got: %+v", enumeration)
	}
	if got, want := nodesSvc.calls["pve2"], transientMaxAttemptsDefault(); got != want {
		t.Fatalf("the strict form must use the full retry budget on an offline member, got %d attempts, want %d", got, want)
	}
}

// TestListGuestsAuthoritative_StrictNamesNoOfflineMemberWithoutQuorum: a
// cluster without quorum has no authority to call a member offline, so the
// refusal reads exactly as it did before.
func TestListGuestsAuthoritative_StrictNamesNoOfflineMemberWithoutQuorum(t *testing.T) {
	t.Parallel()
	c, _ := lgStrictDownCluster(lgStatusQuorate(false, map[string]bool{"pve1": true, "pve2": false}))
	_, err := ListGuestsAuthoritative(lgCtx(), c, nil)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("the strict refusal must stay retriable, got: %v", err)
	}
	if got, want := err.Error(), "ListGuestsAuthoritative: could not list guests on node(s) pve2; refusing to decide from a partial fleet"; got != want {
		t.Fatalf("a cluster without quorum must not change the refusal, got %q, want %q", got, want)
	}
}

// TestListGuestsAuthoritative_StrictNamesNoOfflineMemberWhileItReadsOnline: a
// member that is unreachable while the cluster still reports it online isn't
// called offline, because it may be running its guests.
func TestListGuestsAuthoritative_StrictNamesNoOfflineMemberWhileItReadsOnline(t *testing.T) {
	t.Parallel()
	c, _ := lgStrictDownCluster(lgStatus(map[string]bool{"pve1": true, "pve2": true}))
	_, err := ListGuestsAuthoritative(lgCtx(), c, nil)
	if err == nil || strings.Contains(err.Error(), "offline") {
		t.Fatalf("an unreachable member reported online must not be called offline, got: %v", err)
	}
}

// TestListGuestsAuthoritativeTolerant_FailedOnlineMemberNamesNoOffline: the
// tolerant form leaves out a member reported offline, so a member it still
// failed on was reported online, and its refusal never calls it offline even
// when the status reads offline after the budget ran out.
func TestListGuestsAuthoritativeTolerant_FailedOnlineMemberNamesNoOffline(t *testing.T) {
	t.Parallel()
	c, _ := lgStrictDownCluster(nil)
	// One status read before the fan-out and one after each failed attempt
	// report pve2 online. Any read after that reports it offline.
	online := 1 + transientMaxAttemptsDefault()
	reads := 0
	c.cluster.statusFn = func() (*sdkcluster.ListStatusResponse, error) {
		reads++
		return lgStatus(map[string]bool{"pve1": true, "pve2": reads <= online})()
	}
	_, _, err := ListGuestsAuthoritativeTolerant(lgCtx(), c, nil)
	if err == nil {
		t.Fatal("a member that failed every attempt while reported online must fail the tolerant listing")
	}
	var enumeration *GuestEnumerationError
	if !errors.As(err, &enumeration) || len(enumeration.Offline) != 0 {
		t.Fatalf("the tolerant refusal must not name an offline member, got: %v", err)
	}
}

// TestListMemberGuests_NamesMemberReportedOffline: the strict listing of named
// members, which the disk calls use for a member they left out, names a member
// the quorate cluster still reports offline after the budget and says to
// retry once it is back.
func TestListMemberGuests_NamesMemberReportedOffline(t *testing.T) {
	t.Parallel()
	c, _ := lgStrictDownCluster(lgStatus(map[string]bool{"pve1": true, "pve2": false}))
	_, err := ListMemberGuests(lgCtx(), c, nil, []string{"pve2"})
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("the member listing refusal must stay retriable, got: %v", err)
	}
	if !strings.Contains(err.Error(), "retry once node(s) pve2 are back in the cluster") {
		t.Fatalf("the refusal must say to retry once pve2 is back, got: %v", err)
	}
}

// TestListIndexedGuests_ReadsGuestRows: the index read returns QEMU and LXC
// rows and rows with no type, with their node and tags, and it skips rows for
// other resources and rows with no VMID or node.
func TestListIndexedGuests_ReadsGuestRows(t *testing.T) {
	t.Parallel()
	rows := sdkcluster.ListResourcesResponse{
		json.RawMessage(`{"vmid": 101, "node": "pve2", "type": "qemu", "tags": "bosh-parker", "status": "unknown"}`),
		json.RawMessage(`{"vmid": 102, "node": "pve2", "type": "lxc"}`),
		json.RawMessage(`{"vmid": 103, "node": "pve1", "template": 1}`),
		json.RawMessage(`{"node": "pve1", "type": "node"}`),
		json.RawMessage(`{"vmid": 104, "type": "qemu"}`),
		json.RawMessage(`{"vmid": 0, "node": "pve1", "type": "qemu"}`),
		json.RawMessage(`not json`),
	}
	c := &lgClient{cluster: &lgCluster{resourcesFn: func() (*sdkcluster.ListResourcesResponse, error) { return &rows, nil }}}
	got, err := ListIndexedGuests(lgCtx(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want the three guest rows, got %+v", got)
	}
	if got[0].VMID != 101 || got[0].Node != "pve2" || got[0].Type != "qemu" || !TagsMarkParker(got[0].Tags) {
		t.Errorf("first row decoded wrong: %+v", got[0])
	}
	if got[1].VMID != 102 || got[1].Type != "lxc" {
		t.Errorf("second row decoded wrong: %+v", got[1])
	}
	if got[2].VMID != 103 || got[2].Type != "" || !got[2].Template {
		t.Errorf("third row decoded wrong: %+v", got[2])
	}
}

// TestListIndexedGuests_FailsClosed: a read that fails, and a client with no
// cluster service, return an error and no rows, so no caller can read an
// unread index as an empty one.
func TestListIndexedGuests_FailsClosed(t *testing.T) {
	t.Parallel()
	failing := &lgClient{cluster: &lgCluster{resourcesFn: func() (*sdkcluster.ListResourcesResponse, error) {
		return nil, lgProxyUnreachable()
	}}}
	if rows, err := ListIndexedGuests(lgCtx(), failing); err == nil || rows != nil {
		t.Fatalf("a failed index read must return an error and no rows, got rows=%v err=%v", rows, err)
	}
	if rows, err := ListIndexedGuests(lgCtx(), nil); err == nil || rows != nil {
		t.Fatalf("a nil client must return an error and no rows, got rows=%v err=%v", rows, err)
	}
}
