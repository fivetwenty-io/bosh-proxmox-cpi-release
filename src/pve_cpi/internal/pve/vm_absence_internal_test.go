// vm_absence_internal_test.go: white-box tests for the absence proof behind
// FindVMAuthoritative's miss path and FindVMByConfigProbe. A clean config
// sweep is necessary but not sufficient: the HA resource configuration, the
// CRM's service state, and any record tying the guest to an offline member
// each veto the proof, and any of them failing to answer leaves it unproven.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// absNotFound answers every config probe with a clean not-found.
func absNotFound() *mnQEMU {
	return &mnQEMU{configFn: func(string, int) (map[string]any, error) {
		return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
	}}
}

func absHAResources(rows ...string) func() (*sdkcluster.ListHaResourcesResponse, error) {
	return func() (*sdkcluster.ListHaResourcesResponse, error) {
		resp := make(sdkcluster.ListHaResourcesResponse, 0, len(rows))
		for _, r := range rows {
			resp = append(resp, json.RawMessage(r))
		}
		return &resp, nil
	}
}

func absManager(body string) func() (*sdkcluster.ListHaStatusManagerStatusResponse, error) {
	return func() (*sdkcluster.ListHaStatusManagerStatusResponse, error) {
		resp := sdkcluster.ListHaStatusManagerStatusResponse(body)
		return &resp, nil
	}
}

// absLookups runs both public entry points against one client, so each veto is
// pinned on the index-miss path and the contradicted-index path alike.
func absLookups() map[string]func(context.Context, Client, int) (VMLocation, error) {
	return map[string]func(context.Context, Client, int) (VMLocation, error){
		"FindVMAuthoritative": func(ctx context.Context, c Client, vmid int) (VMLocation, error) {
			return FindVMAuthoritative(ctx, c, vmid)
		},
		"FindVMByConfigProbe": FindVMByConfigProbe,
	}
}

func requireRetriableContaining(t *testing.T, loc VMLocation, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("loc = %+v; want a retriable error, absence must stay unproven", loc)
	}
	if loc.Found {
		t.Fatalf("loc = %+v; an error answer must not carry a location", loc)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v; want retriable", err)
	}
	if want != "" && !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v; want it to mention %q", err, want)
	}
}

func TestProveVMAbsent_HAResourceConfigured_Unproven(t *testing.T) {
	t.Parallel()
	for name, lookup := range absLookups() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &lgClient{
				cluster: &lgCluster{
					nodes:         []string{"pve1", "pve2"},
					haResourcesFn: absHAResources(`{"sid": "vm:999"}`, `{"sid": "vm:4120", "state": "started"}`),
				},
				nodes: &lgNodes{},
				qemu:  absNotFound(),
			}
			loc, err := lookup(lgCtx(), c, 4120)
			requireRetriableContaining(t, loc, err, "ha-manager remove vm:4120")
		})
	}
}

func TestProveVMAbsent_ContainerSIDHoldsTheVMID_Unproven(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{nodes: []string{"pve1"}, haResourcesFn: absHAResources(`{"sid": "ct:4121"}`)},
		nodes:   &lgNodes{},
		qemu:    absNotFound(),
	}
	loc, err := FindVMAuthoritative(lgCtx(), c, 4121)
	requireRetriableContaining(t, loc, err, "ct:4121")
}

func TestProveVMAbsent_OtherHAResourcesDoNotBlock(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{
			nodes:         []string{"pve1"},
			haResourcesFn: absHAResources(`{"sid": "vm:41220"}`, `{"sid": "ct:412"}`, `{"sid": "4122"}`),
			haManagerFn:   absManager(`{"service_status": {"vm:41220": {"state": "started", "node": "pve1"}}}`),
		},
		nodes: &lgNodes{},
		qemu:  absNotFound(),
	}
	loc, err := FindVMAuthoritative(lgCtx(), c, 4122)
	if err != nil || loc.Found {
		t.Fatalf("loc=%+v err=%v; want proven absence, no HA source names VM 4122", loc, err)
	}
}

func TestProveVMAbsent_HAResourcesUnreadable_Unproven(t *testing.T) {
	t.Parallel()
	for name, lookup := range absLookups() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &lgClient{
				cluster: &lgCluster{
					nodes: []string{"pve1"},
					haResourcesFn: func() (*sdkcluster.ListHaResourcesResponse, error) {
						return nil, &sdkerrors.APIError{HTTPCode: 403, Message: "Permission check failed (/, Sys.Audit)"}
					},
				},
				nodes: &lgNodes{},
				qemu:  absNotFound(),
			}
			loc, err := lookup(lgCtx(), c, 4123)
			requireRetriableContaining(t, loc, err, "HA resources could not be read")
		})
	}
}

func TestProveVMAbsent_HAResourcesNoBody_Unproven(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{
			nodes:         []string{"pve1"},
			haResourcesFn: func() (*sdkcluster.ListHaResourcesResponse, error) { return nil, nil },
		},
		nodes: &lgNodes{},
		qemu:  absNotFound(),
	}
	loc, err := FindVMAuthoritative(lgCtx(), c, 4124)
	requireRetriableContaining(t, loc, err, "no body")
}

func TestProveVMAbsent_HAResourceRowUnparseable_Unproven(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{nodes: []string{"pve1"}, haResourcesFn: absHAResources(`"vm:4125"`)},
		nodes:   &lgNodes{},
		qemu:    absNotFound(),
	}
	loc, err := FindVMAuthoritative(lgCtx(), c, 4125)
	requireRetriableContaining(t, loc, err, "does not parse")
}

func TestProveVMAbsent_CRMServiceWithNoConfigAnywhere_Unproven(t *testing.T) {
	t.Parallel()
	for name, lookup := range absLookups() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &lgClient{
				cluster: &lgCluster{
					nodes: []string{"pve1", "pve2"},
					haManagerFn: absManager(
						`{"manager_status": {"service_status": {"vm:4126": {"state": "recovery", "node": "pve2"}}}}`),
				},
				nodes: &lgNodes{},
				qemu:  absNotFound(),
			}
			loc, err := lookup(lgCtx(), c, 4126)
			requireRetriableContaining(t, loc, err, "HA manager still holds service vm:4126")
		})
	}
}

func TestProveVMAbsent_CRMUnreadable_Unproven(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{
			nodes: []string{"pve1"},
			haManagerFn: func() (*sdkcluster.ListHaStatusManagerStatusResponse, error) {
				return nil, &sdkerrors.APIError{HTTPCode: 403, Message: "permission denied"}
			},
		},
		nodes: &lgNodes{},
		qemu:  absNotFound(),
	}
	loc, err := FindVMAuthoritative(lgCtx(), c, 4127)
	requireRetriableContaining(t, loc, err, "HA manager status could not be read")
}

func TestProveVMAbsent_SweepHitSkipsHAReads(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{
			nodes: []string{"pve1"},
			haResourcesFn: func() (*sdkcluster.ListHaResourcesResponse, error) {
				return nil, errors.New("HA resources must not be read once a sweep finds the guest")
			},
		},
		nodes: &lgNodes{},
		qemu: &mnQEMU{configFn: func(string, int) (map[string]any, error) {
			return map[string]any{"tags": "bosh-cpi"}, nil
		}},
	}
	loc, err := FindVMByConfigProbe(lgCtx(), c, 4128)
	if err != nil || !loc.Found || loc.Node != "pve1" {
		t.Fatalf("loc=%+v err=%v; want the sweep's hit on pve1", loc, err)
	}
}

// offlineClient is a quorate two-member cluster whose pve2 is reported
// offline, so FindVMAuthoritative's sweeps exclude it.
func offlineClient(cluster *lgCluster) *lgClient {
	cluster.nodes = []string{"pve1", "pve2"}
	cluster.statusFn = lgStatus(map[string]bool{"pve1": true, "pve2": false})
	return &lgClient{cluster: cluster, nodes: &lgNodes{}, qemu: &mnQEMU{configFn: func(node string, _ int) (map[string]any, error) {
		if node == "pve2" {
			return nil, errors.New("offline member must not be probed")
		}
		return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
	}}}
}

func TestFindVMAuthoritative_OfflineMember_TieNamesIt_Unproven(t *testing.T) {
	t.Parallel()
	var gotVMID int
	var gotOffline []string
	tie := func(_ context.Context, vmid int, offline []string) (string, string, error) {
		gotVMID, gotOffline = vmid, offline
		return "pve2", "allocation journal record agent-7", nil
	}
	loc, err := FindVMAuthoritative(lgCtx(), offlineClient(&lgCluster{}), 4130, WithOfflineMemberTie(tie))
	requireRetriableContaining(t, loc, err, "allocation journal record agent-7 ties it to member pve2")
	if gotVMID != 4130 || strings.Join(gotOffline, ",") != "pve2" {
		t.Fatalf("tie saw vmid=%d offline=%v; want 4130 and [pve2]", gotVMID, gotOffline)
	}
}

func TestFindVMAuthoritative_OfflineMember_TieUnreadable_Unproven(t *testing.T) {
	t.Parallel()
	tie := func(context.Context, int, []string) (string, string, error) {
		return "", "", errors.New("journal directory unreadable")
	}
	loc, err := FindVMAuthoritative(lgCtx(), offlineClient(&lgCluster{}), 4131, WithOfflineMemberTie(tie))
	requireRetriableContaining(t, loc, err, "could not be read")
}

func TestFindVMAuthoritative_OfflineMember_HAResourceNamesVM_Unproven(t *testing.T) {
	t.Parallel()
	tie := func(context.Context, int, []string) (string, string, error) {
		t.Error("the tie must not be consulted once HA already holds the guest")
		return "", "", nil
	}
	cluster := &lgCluster{haResourcesFn: absHAResources(`{"sid": "vm:4132", "state": "started"}`)}
	loc, err := FindVMAuthoritative(lgCtx(), offlineClient(cluster), 4132, WithOfflineMemberTie(tie))
	requireRetriableContaining(t, loc, err, "vm:4132")
}

func TestFindVMAuthoritative_OfflineMember_NoTie_AbsentWithWarning(t *testing.T) {
	t.Parallel()
	calls := 0
	tie := func(context.Context, int, []string) (string, string, error) {
		calls++
		return "", "", nil
	}
	loc, err := FindVMAuthoritative(lgCtx(), offlineClient(&lgCluster{}), 4133, WithOfflineMemberTie(nil),
		WithOfflineMemberTie(tie))
	if err != nil || loc.Found {
		t.Fatalf("loc=%+v err=%v; want today's tolerated absence when nothing ties the guest to pve2", loc, err)
	}
	if calls != 1 {
		t.Fatalf("tie called %d times; want once", calls)
	}
}

func TestFindVMAuthoritative_NoOfflineMember_TieNotConsulted(t *testing.T) {
	t.Parallel()
	tie := func(context.Context, int, []string) (string, string, error) {
		t.Error("the tie must only be consulted when an offline member was excluded")
		return "pve1", "record", nil
	}
	c := &lgClient{cluster: &lgCluster{nodes: []string{"pve1", "pve2"}}, nodes: &lgNodes{}, qemu: absNotFound()}
	loc, err := FindVMAuthoritative(lgCtx(), c, 4134, WithOfflineMemberTie(tie))
	if err != nil || loc.Found {
		t.Fatalf("loc=%+v err=%v; want proven absence", loc, err)
	}
}

func TestHASIDNamesVMID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		sid  string
		want bool
	}{
		{"vm:100", true}, {"ct:100", true}, {"\tct:100 ", true}, {"vm:1000", false}, {"vm:10", false},
		{"100", false}, {"lxc:100", false}, {"", false},
	}
	for _, tc := range cases {
		if got := haSIDNamesVMID(tc.sid, 100); got != tc.want {
			t.Errorf("haSIDNamesVMID(%q, 100) = %v; want %v", tc.sid, got, tc.want)
		}
	}
}
