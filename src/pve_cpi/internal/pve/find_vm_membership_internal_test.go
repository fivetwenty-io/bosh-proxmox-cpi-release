// find_vm_membership_internal_test.go: white-box tests for the cluster
// membership contract behind every authoritative fan-out. Pinned here:
// ListClusterMemberNames prefers corosync and falls back to GET /nodes only
// on an empty or resolved-API answer (never on a transport fault), the
// standalone-host topology enumerates through that fallback, and
// FindVMAuthoritative's probe loop honors the quorum-gated offline-member
// tolerance without ever weakening its absence proof.
package pve

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// mnQEMU records config probes and answers through configFn.
type mnQEMU struct {
	sdkqemu.Service
	configFn func(node string, vmid int) (map[string]any, error)
	probed   []string
}

func (q *mnQEMU) Config(_ context.Context, node string, vmid int) (map[string]any, error) {
	q.probed = append(q.probed, node)
	return q.configFn(node, vmid)
}

// mnSoloNodesList scripts GET /nodes to name a single standalone host.
func mnSoloNodesList() (*sdknodes.ListNodesResponse, error) {
	resp := sdknodes.ListNodesResponse{[]byte(`{"node": "solo", "status": "online"}`)}
	return &resp, nil
}

// ---------------------------------------------------------------------------
// ListClusterMemberNames
// ---------------------------------------------------------------------------

func TestListClusterMemberNames_CorosyncPrimary_NoFallback(t *testing.T) {
	t.Parallel()
	nodesSvc := &lgNodes{listNodesFn: mnSoloNodesList}
	c := &lgClient{
		cluster: &lgCluster{nodes: []string{"pve1", "pve2"}},
		nodes:   nodesSvc,
	}
	names, err := ListClusterMemberNames(lgCtx(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) != 2 || names[0] != "pve1" || names[1] != "pve2" {
		t.Fatalf("names = %v; want the corosync membership", names)
	}
	if nodesSvc.listNodesCalls != 0 {
		t.Fatalf("a non-empty corosync answer must not consult GET /nodes, got %d calls", nodesSvc.listNodesCalls)
	}
}

func TestListClusterMemberNames_EmptyCorosync_FallsBackToNodes(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{nodes: nil},
		nodes:   &lgNodes{listNodesFn: mnSoloNodesList},
	}
	names, err := ListClusterMemberNames(lgCtx(), c)
	if err != nil {
		t.Fatalf("a standalone host (empty corosync config) must enumerate via GET /nodes, got: %v", err)
	}
	if len(names) != 1 || names[0] != "solo" {
		t.Fatalf("names = %v; want the standalone host itself", names)
	}
}

func TestListClusterMemberNames_APIVerdictError_FallsBackToNodes(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{configNodesFn: func() (*sdkcluster.ListConfigNodesResponse, error) {
			// The resolved-API shape some PVE versions answer on a host with
			// no corosync configuration.
			return nil, sdkerrors.ParseAPIError(400, []byte(`{"message":"no cluster configuration"}`))
		}},
		nodes: &lgNodes{listNodesFn: mnSoloNodesList},
	}
	names, err := ListClusterMemberNames(lgCtx(), c)
	if err != nil {
		t.Fatalf("a resolved API verdict from corosync must fall back to GET /nodes, got: %v", err)
	}
	if len(names) != 1 || names[0] != "solo" {
		t.Fatalf("names = %v; want the standalone host itself", names)
	}
}

func TestListClusterMemberNames_PermissionVerdict_NoFallback(t *testing.T) {
	t.Parallel()
	nodesSvc := &lgNodes{listNodesFn: mnSoloNodesList}
	c := &lgClient{
		cluster: &lgCluster{configNodesFn: func() (*sdkcluster.ListConfigNodesResponse, error) {
			return nil, sdkerrors.ParseAPIError(403, []byte(`{"message":"permission denied"}`))
		}},
		nodes: nodesSvc,
	}
	_, err := ListClusterMemberNames(lgCtx(), c)
	if err == nil {
		t.Fatal("a permission verdict never means \"no corosync configuration\"; want an error, got success")
	}
	if nodesSvc.listNodesCalls != 0 {
		t.Fatalf("a permission verdict must not consult the GET /nodes fallback (an asymmetric ACL could answer with a subset of a real cluster), got %d calls",
			nodesSvc.listNodesCalls)
	}
}

func TestListClusterMemberNames_TransportError_NoFallback(t *testing.T) {
	t.Parallel()
	nodesSvc := &lgNodes{listNodesFn: mnSoloNodesList}
	c := &lgClient{
		cluster: &lgCluster{configNodesFn: func() (*sdkcluster.ListConfigNodesResponse, error) {
			return nil, errors.New("connection refused")
		}},
		nodes: nodesSvc,
	}
	_, err := ListClusterMemberNames(lgCtx(), c)
	if err == nil {
		t.Fatal("a transport fault leaves membership unknown, not empty; want an error, got success")
	}
	if nodesSvc.listNodesCalls != 0 {
		t.Fatalf("a transport fault must not consult the GET /nodes fallback (membership is unknown, not \"no cluster\"), got %d calls",
			nodesSvc.listNodesCalls)
	}
}

func TestListClusterMemberNames_BothSourcesEmpty_Retriable(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{nodes: nil},
		nodes:   &lgNodes{},
	}
	_, err := ListClusterMemberNames(lgCtx(), c)
	if err == nil {
		t.Fatal("no source names any member; want an error, got success")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("error = %v; want retriable", err)
	}
}

// TestListGuestsAuthoritative_StandaloneHostFallsBackToNodes pins the wiring:
// the enumeration itself works on a never-clustered host, where corosync
// membership answers empty and GET /nodes names exactly the host.
func TestListGuestsAuthoritative_StandaloneHostFallsBackToNodes(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{nodes: nil},
		nodes: &lgNodes{
			listNodesFn: mnSoloNodesList,
			listings:    map[string][]string{"solo": {`{"vmid": 596, "name": "bosh-vm"}`}},
		},
	}
	guests, err := ListGuestsAuthoritative(lgCtx(), c, nil)
	if err != nil {
		t.Fatalf("standalone-host enumeration must work, got: %v", err)
	}
	if len(guests) != 1 || guests[0].VMID != 596 || guests[0].Node != "solo" {
		t.Fatalf("guests = %+v; want the standalone host's single guest", guests)
	}
}

// ---------------------------------------------------------------------------
// FindVMAuthoritative offline-member tolerance
// ---------------------------------------------------------------------------

func TestFindVMAuthoritative_OfflineMemberExcluded_AbsenceProven(t *testing.T) {
	t.Parallel()
	q := &mnQEMU{configFn: func(node string, _ int) (map[string]any, error) {
		if node == "pve2" {
			return nil, errors.New("offline member must not be probed")
		}
		return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
	}}
	c := &lgClient{
		cluster: &lgCluster{
			nodes:    []string{"pve1", "pve2"},
			statusFn: lgStatus(map[string]bool{"pve1": true, "pve2": false}),
		},
		nodes: &lgNodes{},
		qemu:  q,
	}
	loc, err := FindVMAuthoritative(lgCtx(), c, 4110)
	if err != nil {
		t.Fatalf("a quorate-reported-offline member must be excluded, not block the lookup; got: %v", err)
	}
	if loc.Found {
		t.Fatalf("loc = %+v; want Found=false (absence proven on every online member)", loc)
	}
	if len(q.probed) != 1 || q.probed[0] != "pve1" {
		t.Fatalf("probed %v; want only the online member probed", q.probed)
	}
}

func TestFindVMAuthoritative_NotQuorate_ToleranceWithheld(t *testing.T) {
	t.Parallel()
	q := &mnQEMU{configFn: func(node string, _ int) (map[string]any, error) {
		if node == "pve1" {
			return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
		}
		return nil, &sdkerrors.APIError{HTTPCode: 500, Message: "pvedaemon worker cycling"}
	}}
	c := &lgClient{
		cluster: &lgCluster{
			nodes:    []string{"pve1", "pve2"},
			statusFn: lgStatusQuorate(false, map[string]bool{"pve1": true, "pve2": false}),
		},
		nodes: &lgNodes{},
		qemu:  q,
	}
	loc, err := FindVMAuthoritative(lgCtx(), c, 4111)
	if err == nil {
		t.Fatalf("without quorum the offline report has no authority; the failed probe must surface, got loc=%+v", loc)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("error = %v; want retriable", err)
	}
	if !strings.Contains(err.Error(), "pve2") {
		t.Fatalf("the error must name the failing node, got: %v", err)
	}
	probedPve2 := false
	for _, n := range q.probed {
		if n == "pve2" {
			probedPve2 = true
		}
	}
	if !probedPve2 {
		t.Fatalf("probed %v; without quorum every member must still be probed", q.probed)
	}
}

func TestFindVMAuthoritative_AllMembersOffline_Retriable(t *testing.T) {
	t.Parallel()
	q := &mnQEMU{configFn: func(string, int) (map[string]any, error) {
		return nil, errors.New("no member may be probed")
	}}
	c := &lgClient{
		cluster: &lgCluster{
			nodes:    []string{"pve1", "pve2"},
			statusFn: lgStatus(map[string]bool{"pve1": false, "pve2": false}),
		},
		nodes: &lgNodes{},
		qemu:  q,
	}
	loc, err := FindVMAuthoritative(lgCtx(), c, 4112)
	if err == nil {
		t.Fatalf("a fully-dark cluster proves nothing; want an error, got loc=%+v", loc)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("error = %v; want retriable", err)
	}
	if len(q.probed) != 0 {
		t.Fatalf("probed %v; want no probes against offline members", q.probed)
	}
}

// ---------------------------------------------------------------------------
// FindTemplatesBySHATagClusterTolerant
// ---------------------------------------------------------------------------

const mnTemplateRow = `{"vmid": 30500, "name": "tpl", ` +
	`"tags": "bosh-stemcell-cache;bosh-stemcell-sha-feedbeef", "template": 1}`

func TestFindTemplatesBySHATagClusterTolerant_OfflineMemberExcluded(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{
			nodes:    []string{"pve1", "pve2"},
			statusFn: lgStatus(map[string]bool{"pve1": true, "pve2": false}),
		},
		nodes: &lgNodes{
			listings:  map[string][]string{"pve1": {mnTemplateRow}},
			failNodes: map[string]error{"pve2": errors.New("connection refused")},
		},
	}
	refs, err := FindTemplatesBySHATagClusterTolerant(lgCtx(), c, "feedbeef")
	if err != nil {
		t.Fatalf("the tolerant lookup must exclude the offline member, got: %v", err)
	}
	if len(refs) != 1 || refs[0].VMID != 30500 || refs[0].Node != "pve1" {
		t.Fatalf("refs = %+v; want the online member's template", refs)
	}
}

func TestFindTemplatesBySHATagCluster_StrictFailsOnOfflineMember(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{
			nodes:    []string{"pve1", "pve2"},
			statusFn: lgStatus(map[string]bool{"pve1": true, "pve2": false}),
		},
		nodes: &lgNodes{
			listings:  map[string][]string{"pve1": {mnTemplateRow}},
			failNodes: map[string]error{"pve2": errors.New("connection refused")},
		},
	}
	_, err := FindTemplatesBySHATagCluster(lgCtx(), c, "feedbeef")
	if err == nil {
		t.Fatal("the strict lookup feeds destructive sweeps and must fail on an unlistable member")
	}
	if !strings.Contains(err.Error(), "pve2") {
		t.Fatalf("the error must name the unlistable node, got: %v", err)
	}
}
