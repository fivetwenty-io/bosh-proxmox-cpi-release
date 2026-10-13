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
// FindVMByConfigProbe absence proof
// ---------------------------------------------------------------------------

// TestFindVMByConfigProbe_OfflineMemberLeavesAbsenceUnproven is the fixture
// where FindVMAuthoritative excludes the offline member and proves absence.
// FindVMByConfigProbe has no index row to cover that member's guests, so the
// same answers leave absence unproven, and the error names the member.
func TestFindVMByConfigProbe_OfflineMemberLeavesAbsenceUnproven(t *testing.T) {
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
	loc, err := FindVMByConfigProbe(lgCtx(), c, 4113)
	if err == nil {
		t.Fatalf("an offline member proves nothing on the probe path; want an error, got loc=%+v", loc)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("error = %v; want retriable", err)
	}
	if !strings.Contains(err.Error(), "pve2") || !strings.Contains(err.Error(), "FindVMByConfigProbe") {
		t.Fatalf("the error must name the offline member and the function, got: %v", err)
	}
	for _, n := range q.probed {
		if n == "pve2" {
			t.Fatalf("probed %v; an offline member's config endpoint cannot answer and must not be probed", q.probed)
		}
	}
}

// TestFindVMByConfigProbe_OfflineMemberDoesNotHideAHit pins that the strict
// rule only withholds absence. A config on an online member is still a hit,
// because VMIDs are cluster-unique.
func TestFindVMByConfigProbe_OfflineMemberDoesNotHideAHit(t *testing.T) {
	t.Parallel()
	q := &mnQEMU{configFn: func(node string, _ int) (map[string]any, error) {
		if node == "pve2" {
			return map[string]any{"tags": "bosh-cpi"}, nil
		}
		return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
	}}
	c := &lgClient{
		cluster: &lgCluster{
			nodes:    []string{"pve1", "pve2", "pve3"},
			statusFn: lgStatus(map[string]bool{"pve1": false, "pve2": true, "pve3": true}),
		},
		nodes: &lgNodes{},
		qemu:  q,
	}
	loc, err := FindVMByConfigProbe(lgCtx(), c, 4114)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !loc.Found || loc.Node != "pve2" || !loc.FromConfig || loc.Tags != "bosh-cpi" {
		t.Fatalf("loc = %+v; want a FromConfig hit on pve2", loc)
	}
}

// TestFindVMByConfigProbe_SecondSweepFindsAMigratedVM covers a migration
// whose config rename lands inside the first sweep: the first sweep sees the
// VM on no member, and the second finds it on the target, whose location is
// the answer.
func TestFindVMByConfigProbe_SecondSweepFindsAMigratedVM(t *testing.T) {
	t.Parallel()
	q := &mnQEMU{}
	q.configFn = func(node string, _ int) (map[string]any, error) {
		// The first sweep probes pve1 and pve2; the config lands on pve1
		// after the first sweep has passed it.
		if node == "pve1" && len(q.probed) > 2 {
			return map[string]any{"tags": "bosh-cpi"}, nil
		}
		return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
	}
	c := &lgClient{
		cluster: &lgCluster{nodes: []string{"pve1", "pve2"}},
		nodes:   &lgNodes{},
		qemu:    q,
	}
	loc, err := FindVMByConfigProbe(lgCtx(), c, 4115)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !loc.Found || loc.Node != "pve1" || !loc.FromConfig {
		t.Fatalf("loc = %+v; want the second sweep's hit on pve1", loc)
	}
	if got := strings.Join(q.probed, ","); got != "pve1,pve2,pve1" {
		t.Fatalf("probed %s; want a full first sweep and a second sweep that stops at the hit", got)
	}
}

// TestFindVMByConfigProbe_AbsenceTakesTwoSweeps pins the cost of the second
// sweep: it runs only when the first proves absence, and absence is answered
// only after both sweeps found every member not-found.
func TestFindVMByConfigProbe_AbsenceTakesTwoSweeps(t *testing.T) {
	t.Parallel()
	q := &mnQEMU{configFn: func(string, int) (map[string]any, error) {
		return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
	}}
	c := &lgClient{
		cluster: &lgCluster{nodes: []string{"pve1", "pve2"}},
		nodes:   &lgNodes{},
		qemu:    q,
	}
	loc, err := FindVMByConfigProbe(lgCtx(), c, 4116)
	if err != nil || loc.Found {
		t.Fatalf("loc=%+v err=%v; want proven absence", loc, err)
	}
	if got := strings.Join(q.probed, ","); got != "pve1,pve2,pve1,pve2" {
		t.Fatalf("probed %s; want two complete sweeps", got)
	}

	hit := &mnQEMU{configFn: func(string, int) (map[string]any, error) {
		return map[string]any{"tags": "bosh-cpi"}, nil
	}}
	c.qemu = hit
	if loc, err := FindVMByConfigProbe(lgCtx(), c, 4116); err != nil || !loc.Found {
		t.Fatalf("loc=%+v err=%v; want a hit", loc, err)
	}
	if len(hit.probed) != 1 {
		t.Fatalf("probed %v; a first-sweep hit must not run a second sweep", hit.probed)
	}
}

// TestFindVMByConfigProbe_SecondSweepFailureIsRetriable pins that the second
// sweep is held to the same rules as the first. Every member answers 404 in
// the first sweep, and the second sweep cannot finish: in one case pve2's
// probe fails, and in the other the quorate cluster now reports pve2
// offline. Neither may be answered as absence, because the second sweep is
// the one that would see a migration that landed during the first.
func TestFindVMByConfigProbe_SecondSweepFailureIsRetriable(t *testing.T) {
	t.Parallel()
	notFound := &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}

	t.Run("a probe error", func(t *testing.T) {
		t.Parallel()
		q := &mnQEMU{}
		q.configFn = func(node string, _ int) (map[string]any, error) {
			// The first sweep is the first two probes.
			if node == "pve2" && len(q.probed) > 2 {
				return nil, &sdkerrors.APIError{HTTPCode: 500, Message: "pvedaemon worker cycling"}
			}
			return nil, notFound
		}
		c := &lgClient{
			cluster: &lgCluster{nodes: []string{"pve1", "pve2"}},
			nodes:   &lgNodes{},
			qemu:    q,
		}
		loc, err := FindVMByConfigProbe(lgCtx(), c, 4117)
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || loc.Found {
			t.Fatalf("loc=%+v err=%v; want a retriable error and Found=false", loc, err)
		}
		if !strings.Contains(err.Error(), "pve2") {
			t.Fatalf("the error must name the failing member, got: %v", err)
		}
		if len(q.probed) < 4 {
			t.Fatalf("probed %v; want the failure to come from the second sweep", q.probed)
		}
	})

	t.Run("a member goes offline", func(t *testing.T) {
		t.Parallel()
		statusCalls := 0
		online := lgStatus(map[string]bool{"pve1": true, "pve2": true})
		offline := lgStatus(map[string]bool{"pve1": true, "pve2": false})
		probes := &mnQEMU{}
		probes.configFn = func(node string, _ int) (map[string]any, error) {
			if node == "pve2" && statusCalls > 1 {
				return nil, errors.New("an offline member must not be probed")
			}
			return nil, notFound
		}
		c := &lgClient{
			cluster: &lgCluster{
				nodes: []string{"pve1", "pve2"},
				statusFn: func() (*sdkcluster.ListStatusResponse, error) {
					statusCalls++
					if statusCalls == 1 {
						return online()
					}
					return offline()
				},
			},
			nodes: &lgNodes{},
			qemu:  probes,
		}
		loc, err := FindVMByConfigProbe(lgCtx(), c, 4118)
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || loc.Found {
			t.Fatalf("loc=%+v err=%v; want a retriable error and Found=false", loc, err)
		}
		if !strings.Contains(err.Error(), "offline") || !strings.Contains(err.Error(), "pve2") {
			t.Fatalf("the error must name the offline member, got: %v", err)
		}
		if statusCalls < 2 {
			t.Fatalf("status read %d time(s); want each sweep to read it", statusCalls)
		}
	})
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
