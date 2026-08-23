// list_guests_internal_test.go: white-box tests for the authoritative
// cluster-wide guest enumeration. The behaviors pinned here are the ones the
// stale /cluster/resources index cannot provide: per-node listings union into
// one fleet, an unlistable node fails the enumeration loudly and retriably,
// and the PVE integer-boolean template flag decodes.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

type lgCluster struct {
	sdkcluster.Service
	nodes []string
	// statusFn overrides ListStatus; nil means "no offline members".
	statusFn func() (*sdkcluster.ListStatusResponse, error)
}

func (s *lgCluster) ListConfigNodes(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	resp := make(sdkcluster.ListConfigNodesResponse, 0, len(s.nodes))
	for _, n := range s.nodes {
		resp = append(resp, json.RawMessage(`{"name": "`+n+`"}`))
	}
	return &resp, nil
}

type lgNodes struct {
	sdknodes.Service
	// listings maps node name to raw JSON list entries; a node mapped to nil
	// with a presence in failNodes errors instead.
	listings  map[string][]string
	failNodes map[string]error
	calls     map[string]int
}

func (s *lgNodes) ListQemu(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[node]++
	if err, ok := s.failNodes[node]; ok {
		return nil, err
	}
	resp := make(sdknodes.ListQemuResponse, 0)
	for _, raw := range s.listings[node] {
		resp = append(resp, json.RawMessage(raw))
	}
	return &resp, nil
}

type lgClient struct {
	Client
	cluster *lgCluster
	nodes   *lgNodes
}

func (c *lgClient) Cluster() sdkcluster.Service { return c.cluster }
func (c *lgClient) Nodes() sdknodes.Service     { return c.nodes }

func lgCtx() context.Context {
	return WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
}

func TestListGuestsAuthoritative_UnionsPerNodeListings(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{nodes: []string{"pve1", "pve2"}},
		nodes: &lgNodes{listings: map[string][]string{
			// template serialized as the JSON number 1, the shape PVE's
			// Perl-backed API actually emits.
			"pve1": {
				`{"vmid": 596, "name": "bosh-vm", "tags": "bosh-cpi;bosh-dir-x", "status": "running"}`,
				`{"vmid": 30500, "name": "tpl", "tags": "bosh-stemcell-sha-feedbeef", "template": 1}`,
			},
			"pve2": {
				`{"vmid": 597}`,
			},
		}},
	}
	guests, err := ListGuestsAuthoritative(lgCtx(), c, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(guests) != 3 {
		t.Fatalf("expected 3 guests across both nodes, got %d: %+v", len(guests), guests)
	}
	byVMID := map[int]GuestRef{}
	for _, g := range guests {
		byVMID[g.VMID] = g
	}
	if g := byVMID[30500]; !g.Template || g.Node != "pve1" {
		t.Fatalf("integer-boolean template flag must decode and carry its node, got %+v", g)
	}
	if g := byVMID[596]; g.Template || g.Name != "bosh-vm" || len(g.SplitTags()) != 2 || g.Status != "running" {
		t.Fatalf("plain VM fields (including status) must decode, got %+v", g)
	}
	if g := byVMID[597]; g.Node != "pve2" || g.Name != "" || g.Tags != "" {
		t.Fatalf("minimal entry must decode with zero-value optionals, got %+v", g)
	}
}

func TestListGuestsAuthoritative_UnlistableNodeFailsRetriably(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{nodes: []string{"pve1", "pve2"}},
		nodes: &lgNodes{
			listings:  map[string][]string{"pve1": {`{"vmid": 596}`}},
			failNodes: map[string]error{"pve2": errors.New("connection refused")},
		},
	}
	guests, err := ListGuestsAuthoritative(lgCtx(), c, nil)
	if err == nil {
		t.Fatalf("an unlistable node must fail the enumeration, got %d guests", len(guests))
	}
	if !strings.Contains(err.Error(), "pve2") {
		t.Fatalf("the error must name the unlistable node, got: %v", err)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("a partial fleet must classify retriable, got: %v", err)
	}
}

func TestListGuestsAuthoritative_EmptyMembershipFailsRetriably(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{nodes: nil},
		nodes:   &lgNodes{},
	}
	_, err := ListGuestsAuthoritative(lgCtx(), c, nil)
	if err == nil {
		t.Fatal("empty corosync membership must fail the enumeration, not report an empty fleet")
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("empty membership must classify retriable, got: %v", err)
	}
}

func TestListGuestsAuthoritative_MalformedEntrySkipped(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{nodes: []string{"pve1"}},
		nodes: &lgNodes{listings: map[string][]string{
			"pve1": {`not-json`, `{"vmid": 0}`, `{"vmid": 596}`},
		}},
	}
	guests, err := ListGuestsAuthoritative(lgCtx(), c, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(guests) != 1 || guests[0].VMID != 596 {
		t.Fatalf("malformed and zero-vmid entries must be skipped, got %+v", guests)
	}
}

// ListStatus delegates to statusFn when scripted; otherwise it reports no
// offline members (fully online fixture cluster).
func (s *lgCluster) ListStatus(context.Context) (*sdkcluster.ListStatusResponse, error) {
	if s.statusFn != nil {
		return s.statusFn()
	}
	empty := sdkcluster.ListStatusResponse{}
	return &empty, nil
}

// lgStatus builds a /cluster/status fixture from name -> online pairs, in the
// integer-boolean shape PVE's Perl-backed API actually emits. The fixture
// cluster reports quorate; use lgStatusQuorate to script otherwise.
func lgStatus(online map[string]bool) func() (*sdkcluster.ListStatusResponse, error) {
	return lgStatusQuorate(true, online)
}

// lgStatusQuorate is lgStatus with an explicit quorate flag on the cluster
// row.
func lgStatusQuorate(quorate bool, online map[string]bool) func() (*sdkcluster.ListStatusResponse, error) {
	return func() (*sdkcluster.ListStatusResponse, error) {
		qFlag := "0"
		if quorate {
			qFlag = "1"
		}
		resp := make(sdkcluster.ListStatusResponse, 0, len(online)+1)
		resp = append(resp, json.RawMessage(`{"type": "cluster", "name": "lab", "quorate": `+qFlag+`}`))
		for name, up := range online {
			flag := "0"
			if up {
				flag = "1"
			}
			resp = append(resp, json.RawMessage(`{"type": "node", "name": "`+name+`", "online": `+flag+`}`))
		}
		return &resp, nil
	}
}

// TestListGuestsAuthoritativeTolerant_OfflineMemberExcluded: a member
// /cluster/status reports offline is excluded from the fan-out instead of
// failing the enumeration, even though its listing would error, and its name
// comes back in the excluded list.
func TestListGuestsAuthoritativeTolerant_OfflineMemberExcluded(t *testing.T) {
	t.Parallel()
	nodesSvc := &lgNodes{
		listings:  map[string][]string{"pve1": {`{"vmid": 596}`}},
		failNodes: map[string]error{"pve2": errors.New("connection refused")},
	}
	c := &lgClient{
		cluster: &lgCluster{
			nodes:    []string{"pve1", "pve2"},
			statusFn: lgStatus(map[string]bool{"pve1": true, "pve2": false}),
		},
		nodes: nodesSvc,
	}
	guests, excluded, err := ListGuestsAuthoritativeTolerant(lgCtx(), c, nil)
	if err != nil {
		t.Fatalf("an offline member must be tolerated, got: %v", err)
	}
	if len(guests) != 1 || guests[0].VMID != 596 {
		t.Fatalf("expected the online node's guests only, got %+v", guests)
	}
	if len(excluded) != 1 || excluded[0] != "pve2" {
		t.Fatalf("the excluded member must be reported to the caller, got %v", excluded)
	}
	if nodesSvc.calls["pve2"] != 0 {
		t.Fatalf("the offline member must not be listed at all, got %d calls", nodesSvc.calls["pve2"])
	}
}

// TestListGuestsAuthoritative_StrictNeverTolerates: the strict form must fail
// loudly on an unlistable member even when /cluster/status would excuse it as
// offline; absence proofs cannot conclude from a reduced fleet.
func TestListGuestsAuthoritative_StrictNeverTolerates(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{
			nodes:    []string{"pve1", "pve2"},
			statusFn: lgStatus(map[string]bool{"pve1": true, "pve2": false}),
		},
		nodes: &lgNodes{
			listings:  map[string][]string{"pve1": {`{"vmid": 596}`}},
			failNodes: map[string]error{"pve2": errors.New("connection refused")},
		},
	}
	_, err := ListGuestsAuthoritative(lgCtx(), c, nil)
	if err == nil {
		t.Fatal("the strict form must fail on an unlistable member even when status reports it offline")
	}
	if !strings.Contains(err.Error(), "pve2") {
		t.Fatalf("the error must name the unlistable node, got: %v", err)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("a partial fleet must classify retriable, got: %v", err)
	}
}

// TestListGuestsAuthoritativeTolerant_NotQuorateWithholdsTolerance: a cluster
// that is not quorate has no authority to declare members down (a minority
// partition sees every majority member as offline), so the tolerance must not
// fire and the unlistable member fails the enumeration.
func TestListGuestsAuthoritativeTolerant_NotQuorateWithholdsTolerance(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{
			nodes:    []string{"pve1", "pve2"},
			statusFn: lgStatusQuorate(false, map[string]bool{"pve1": true, "pve2": false}),
		},
		nodes: &lgNodes{
			listings:  map[string][]string{"pve1": {`{"vmid": 596}`}},
			failNodes: map[string]error{"pve2": errors.New("connection refused")},
		},
	}
	_, _, err := ListGuestsAuthoritativeTolerant(lgCtx(), c, nil)
	if err == nil {
		t.Fatal("without quorum, an offline member must not be tolerated")
	}
	if !strings.Contains(err.Error(), "pve2") {
		t.Fatalf("the error must name the unlistable node, got: %v", err)
	}
}

// TestListGuestsAuthoritativeTolerant_AllMembersOfflineFailsRetriably:
// excluding every member must not read as an empty fleet; a destructive
// caller would conclude "no guests" from a fully-dark cluster.
func TestListGuestsAuthoritativeTolerant_AllMembersOfflineFailsRetriably(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{
			nodes:    []string{"pve1", "pve2"},
			statusFn: lgStatus(map[string]bool{"pve1": false, "pve2": false}),
		},
		nodes: &lgNodes{},
	}
	_, _, err := ListGuestsAuthoritativeTolerant(lgCtx(), c, nil)
	if err == nil {
		t.Fatal("a fully-offline membership must fail the enumeration, not report an empty fleet")
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("fully-offline membership must classify retriable, got: %v", err)
	}
}

// TestListGuestsAuthoritativeTolerant_StatusUnavailableKeepsFailLoud: when
// the status read itself fails, no tolerance is granted; an unlistable member
// fails the enumeration exactly as it would without the status consult.
func TestListGuestsAuthoritativeTolerant_StatusUnavailableKeepsFailLoud(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{
			nodes: []string{"pve1", "pve2"},
			statusFn: func() (*sdkcluster.ListStatusResponse, error) {
				return nil, errors.New("status endpoint unavailable")
			},
		},
		nodes: &lgNodes{
			listings:  map[string][]string{"pve1": {`{"vmid": 596}`}},
			failNodes: map[string]error{"pve2": errors.New("connection refused")},
		},
	}
	_, _, err := ListGuestsAuthoritativeTolerant(lgCtx(), c, nil)
	if err == nil {
		t.Fatal("with the status read failed, an unlistable member must fail the enumeration")
	}
	if !strings.Contains(err.Error(), "pve2") {
		t.Fatalf("the error must name the unlistable node, got: %v", err)
	}
}
