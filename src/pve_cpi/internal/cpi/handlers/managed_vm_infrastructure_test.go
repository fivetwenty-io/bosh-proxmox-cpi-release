package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

func infrastructureJournal(t *testing.T) (*aj.Journal, aj.Intent) {
	t.Helper()
	_, plan, intent := runtimePlanFixture(t)
	plan.VMExecution = &StorageVMExecution{Version: 1, Tags: advertisedRouteTag("router", "10.60.0.0/16")}
	var err error
	intent.Plan, err = json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	journal, err := aj.Initialize(t.Context(), dir, plan.Namespace, aj.Enrollment{ClusterID: "cluster", AuthorityID: "authority", AuditID: "audit", CompleteHistoricalAudit: true, PreviousWriterFenced: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	})
	return journal, intent
}
func infrastructureHandle(t *testing.T, journal *aj.Journal, agent string, intent aj.Intent) *aj.Handle {
	t.Helper()
	handle, err := journal.AcquireVM(t.Context(), agent, intent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	})
	return handle
}
func routeIdentityFixture() managedVMRouteIdentity {
	return managedVMRouteIdentity{Version: 1, Kind: managedVMRouteKind, VNet: "router", Subnet: "evpn-10.60.0.0-16", CIDR: "10.60.0.0/16"}
}

func TestManagedVMRouteCleanupWaitsForRunningAbsence(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "lost-apply-response"}[unknown], func(t *testing.T) {
			testManagedVMRouteCleanupCase(t, unknown)
		})
	}
}

func testManagedVMRouteCleanupCase(t *testing.T, unknown bool) {
	t.Helper()
	journal, intent := infrastructureJournal(t)
	handle := infrastructureHandle(t, journal, "route", intent)
	service := &managedVMRouteCluster{created: true, running: true, unknown: unknown}
	deps := Deps{PVE: &managedVMRouteClient{service: service}, Logger: log.NewNopLogger()}
	err := deleteManagedVMRoute(t.Context(), deps, handle, aj.Target{Node: "pve1", VMID: 123}, routeIdentityFixture())
	if (err != nil) != unknown {
		t.Fatalf("unexpected outcome: %v", err)
	}
	if service.deletes != 1 || service.applies != 1 {
		t.Fatalf("writes delete=%d apply=%d", service.deletes, service.applies)
	}
	steps := handle.Record().Steps
	if len(steps) != 2 || steps[0].State != aj.Observed {
		t.Fatal("delete observation lost")
	}
	if unknown && steps[1].State == aj.Observed {
		t.Fatal("unknown apply marked observed")
	}
	if unknown {
		if err := deleteManagedVMRoute(t.Context(), deps, handle, aj.Target{Node: "pve1", VMID: 123}, routeIdentityFixture()); err == nil {
			t.Fatal("unknown route apply reissued")
		}
		if service.deletes != 1 || service.applies != 1 {
			t.Fatal("unknown route caused another mutation")
		}
	}
	if !unknown {
		if steps[1].State != aj.Observed {
			t.Fatal("running absence not recorded")
		}
		if err := deleteManagedVMRoute(t.Context(), deps, handle, aj.Target{Node: "pve1", VMID: 123}, routeIdentityFixture()); err != nil {
			t.Fatal(err)
		}
		if service.deletes != 1 || service.applies != 1 {
			t.Fatal("completed route cleanup repeated writes")
		}
	}
}

func TestManagedVMRouteHandoffRequiresOriginalCreation(t *testing.T) {
	journal, intent := infrastructureJournal(t)
	origin := infrastructureHandle(t, journal, "original", intent)
	next := infrastructureHandle(t, journal, "next", intent)
	identity := routeIdentityFixture()
	params, err := aj.MutationParameters(identity)
	if err != nil {
		t.Fatal(err)
	}
	step, err := storageMutationIntent(origin, "vm.Cluster.CreateSdnVnetsSubnets", aj.Target{Node: "pve1", VMID: 123}, nil, params)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(origin, step, nil, false); err != nil {
		t.Fatal(err)
	}
	if routes, err := managedVMRouteCleanupIdentities(journal, next.Record()); err != nil || len(routes) != 0 {
		t.Fatalf("unowned preexisting route adopted: %v %v", routes, err)
	}
	identity.SourceAllocation = origin.Record().ID
	identity.SourceStep = step
	if err := recordManagedVMSharedRoute(origin, aj.Target{Node: "pve1", VMID: 123}, identity, []string{"allocation:" + next.Record().ID}); err != nil {
		t.Fatal(err)
	}
	routes, err := managedVMRouteCleanupIdentities(journal, next.Record())
	if err != nil || len(routes) != 1 {
		t.Fatalf("handoff absent: %v %v", routes, err)
	}
	if routes[0].SourceAllocation != origin.Record().ID || routes[0].Subnet != identity.Subnet {
		t.Fatal("handoff changed original identity")
	}
	third := infrastructureHandle(t, journal, "third", intent)
	if err := recordManagedVMSharedRoute(next, aj.Target{Node: "pve1", VMID: 124}, routes[0], []string{"allocation:" + third.Record().ID}); err != nil {
		t.Fatal(err)
	}
	thirdRoutes, err := managedVMRouteCleanupIdentities(journal, third.Record())
	if err != nil || len(thirdRoutes) != 1 || thirdRoutes[0].SourceStep != step {
		t.Fatalf("transitive handoff lost immutable origin: %v %v", thirdRoutes, err)
	}
	invalid := identity
	invalid.Subnet = "another-subnet"
	if err := verifyManagedVMRouteOrigin(journal, invalid); err == nil {
		t.Fatal("rebound subnet accepted")
	}
}

type infrastructureHACluster struct {
	cluster.Service
	rows          []map[string]any
	failure       error
	removed       bool
	deletes       int
	beforeDelete  func()
	unknown       bool
	nilResource   bool
	wrongResource bool
}

func (c *infrastructureHACluster) ListHaRules(context.Context, *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	if c.failure != nil {
		return nil, c.failure
	}
	rows := cluster.ListHaRulesResponse{}
	for _, fields := range c.rows {
		raw, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		rows = append(rows, raw)
	}
	return &rows, nil
}

type infrastructureHAClient struct {
	*managedVMRouteClient
	ha *infrastructureHACluster
}

func (c *infrastructureHAClient) Cluster() cluster.Service { return c.ha }
func TestManagedVMHAPurgePreservesSharedMembers(t *testing.T) {
	before := map[string]map[string]any{"bosh-aa-web": {"rule": "bosh-aa-web", "type": "resource-affinity", "resources": "vm:123,vm:124,vm:125", "affinity": "negative"}, "bosh-na-123": {"rule": "bosh-na-123", "type": "node-affinity", "resources": "vm:123"}}
	cases := []struct {
		name, resources, affinity      string
		residue, unreadable, wantError bool
	}{{name: "exact", resources: "vm:124,vm:125", affinity: "negative"}, {name: "lost-other-member", resources: "vm:124", affinity: "negative", wantError: true}, {name: "wrong-policy", resources: "vm:124,vm:125", affinity: "positive", wantError: true}, {name: "target-remains", resources: "vm:123,vm:124,vm:125", affinity: "negative", wantError: true}, {name: "sole-rule-remains", resources: "vm:124,vm:125", affinity: "negative", residue: true, wantError: true}, {name: "unreadable", unreadable: true, wantError: true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &infrastructureHACluster{rows: []map[string]any{{"rule": "bosh-aa-web", "type": "resource-affinity", "resources": tc.resources, "affinity": tc.affinity}}}
			if tc.residue {
				svc.rows = append(svc.rows, map[string]any{"rule": "bosh-na-123", "resources": "vm:999"})
			}
			if tc.unreadable {
				svc.failure = errors.New("unavailable")
			}
			deps := Deps{PVE: &infrastructureHAClient{ha: svc}}
			err := observeManagedVMHAPurge(t.Context(), deps, 123, before)
			if (err != nil) != tc.wantError {
				t.Fatalf("purge proof: %v", err)
			}
		})
	}
}

func TestManagedVMRouteMutationRecordsResolvedSubnet(t *testing.T) {
	journal, intent := infrastructureJournal(t)
	handle := infrastructureHandle(t, journal, "route", intent)
	svc := &managedVMRouteCluster{}
	runtime := &managedVMAllocation{deps: Deps{PVE: &managedVMRouteClient{service: svc}}, handle: handle}
	call := ManagedAllocationMutation{Service: "Cluster", Method: "CreateSdnVnetsSubnets", Args: map[string]any{"vnet": "router", "params": &cluster.CreateSdnVnetsSubnetsParams{Subnet: "10.60.0.0/16"}}}
	params, err := runtime.infrastructureMutationParameters(t.Context(), call)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(params), "evpn-10.60.0.0-16") {
		t.Fatal("concrete zone-qualified subnet absent")
	}
	step, err := storageMutationIntent(handle, "vm.Cluster.CreateSdnVnetsSubnets", aj.Target{Node: "pve1", VMID: 123}, nil, params)
	if err != nil {
		t.Fatal(err)
	}
	svc.created = true
	if err := runtime.observeSDNMutation(t.Context(), call, step, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.infrastructureMutationParameters(t.Context(), call); err == nil {
		t.Fatal("preexisting subnet claimed as new creation")
	}
}

func (c *infrastructureHACluster) GetHaResources(context.Context, string) (*cluster.GetHaResourcesResponse, error) {
	if c.removed {
		return nil, &sdkerrors.APIError{HTTPCode: 500, Message: "no such resource"}
	}
	if c.nilResource {
		return nil, nil
	}
	if c.wrongResource {
		return &cluster.GetHaResourcesResponse{Sid: "vm:999"}, nil
	}
	return &cluster.GetHaResourcesResponse{Sid: "vm:123"}, nil
}
func (c *infrastructureHACluster) DeleteHaResources(_ context.Context, sid string, params *cluster.DeleteHaResourcesParams) error {
	c.deletes++
	if c.beforeDelete != nil {
		c.beforeDelete()
	}
	if sid != "vm:123" || params == nil || params.Purge == nil || !*params.Purge {
		return errors.New("wrong purge identity")
	}
	if c.unknown {
		return errors.New("lost response")
	}
	c.removed = true
	c.rows = []map[string]any{{"rule": "shared", "type": "resource-affinity", "resources": "vm:124", "affinity": "negative"}}
	return nil
}

func TestManagedVMHAPurgeJournalsBeforeMutationAndNeverReissuesUnknown(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "observed", true: "unknown"}[unknown], func(t *testing.T) {
			testManagedVMHAPurgeJournalCase(t, unknown)
		})
	}
}

func testManagedVMHAPurgeJournalCase(t *testing.T, unknown bool) {
	t.Helper()
	journal, intent := infrastructureJournal(t)
	handle := infrastructureHandle(t, journal, "ha", intent)
	service := &infrastructureHACluster{unknown: unknown, rows: []map[string]any{{"rule": "shared", "type": "resource-affinity", "resources": "vm:123,vm:124", "affinity": "negative"}}}
	service.beforeDelete = func() { assertManagedVMHAPurgeIntents(t, journal, handle.Record().ID) }
	deps := Deps{PVE: &infrastructureHAClient{ha: service}}
	err := managedVMDeleteHA(t.Context(), deps, handle, "pve1", 123)
	if (err != nil) != unknown {
		t.Fatalf("purge outcome: %v", err)
	}
	steps := handle.Record().Steps
	for i := range steps {
		step := &steps[i]
		if (step.State == aj.Observed) == unknown {
			t.Fatal("wrong observed state")
		}
	}
	if unknown {
		if err := managedVMDeleteHA(t.Context(), deps, handle, "pve1", 123); err == nil {
			t.Fatal("unknown purge reissued")
		}
	}
	if service.deletes != 1 {
		t.Fatalf("purge calls %d", service.deletes)
	}
}

func TestManagedVMRouteDisposedOriginCannotAuthorizeRecreatedSubnet(t *testing.T) {
	journal, intent := infrastructureJournal(t)
	origin := infrastructureHandle(t, journal, "origin", intent)
	next := infrastructureHandle(t, journal, "next", intent)
	identity := routeIdentityFixture()
	params, err := aj.MutationParameters(identity)
	if err != nil {
		t.Fatal(err)
	}
	step, err := storageMutationIntent(origin, "vm.Cluster.CreateSdnVnetsSubnets", aj.Target{Node: "pve1", VMID: 123}, nil, params)
	if err != nil {
		t.Fatal(err)
	}
	if err := storageMutationObserved(origin, step, nil, false); err != nil {
		t.Fatal(err)
	}
	identity.SourceAllocation = origin.Record().ID
	identity.SourceStep = step
	if err := recordManagedVMSharedRoute(origin, aj.Target{Node: "pve1", VMID: 123}, identity, []string{"allocation:" + next.Record().ID}); err != nil {
		t.Fatal(err)
	}
	params, err = aj.MutationParameters(identity)
	if err != nil {
		t.Fatal(err)
	}
	_, err = storageMutationIntent(origin, "vm.route.delete", aj.Target{Node: "pve1", VMID: 123}, nil, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := managedVMRouteCleanupIdentities(journal, next.Record()); err == nil {
		t.Fatal("old creation proof survived deletion intent")
	}
}

func assertManagedVMHAPurgeIntents(t *testing.T, journal *aj.Journal, allocationID string) {
	t.Helper()

	saved, err := journal.Inspect(allocationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Steps) != 2 {
		t.Fatalf("purge preceded durable rule and resource intent: %+v", saved.Steps)
	}
	for i := range saved.Steps {
		step := &saved.Steps[i]
		if step.State != aj.Planned {
			t.Fatal("prewrite intent not planned")
		}
	}
	var fields map[string]any
	if err := json.Unmarshal(saved.Steps[0].Parameters, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["original_resources"] != "vm:123,vm:124" || fields["desired_resources"] != "vm:124" || fields["affinity"] != "negative" {
		t.Fatalf("exact purge contract missing: %v", fields)
	}

}

type routeUsersClient struct {
	pve.Client
	listing *routeUsersNodes
}

func (c *routeUsersClient) Nodes() nodes.Service { return c.listing }

type routeUsersNodes struct {
	nodes.Service
	tags string
}

func (n *routeUsersNodes) ListQemu(context.Context, string, *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	raw, err := json.Marshal(map[string]any{"vmid": 124, "tags": n.tags})
	if err != nil {
		return nil, err
	}
	rows := nodes.ListQemuResponse{raw}
	return &rows, nil
}
func TestManagedVMRouteUsersRequireFreshConfigurationTags(t *testing.T) {
	tag := advertisedRouteTag("router", "10.60.0.0/16")
	for _, current := range []bool{false, true} {
		t.Run(map[bool]string{false: "removed-since-listing", true: "added-since-listing"}[current], func(t *testing.T) {
			deps, journal, c := auditFixture(t)
			listed, live := tag, ""
			if current {
				listed, live = "", tag
			}
			c.configs[124] = map[string]any{"tags": live}
			deps.PVE = &routeUsersClient{Client: c, listing: &routeUsersNodes{Service: c.Nodes(), tags: listed}}
			refs, err := managedVMRouteUsers(t.Context(), deps, journal, "director", 123)
			if err != nil {
				t.Fatal(err)
			}
			if (len(refs[tag]) == 1) != current {
				t.Fatalf("stale listing governed shared reference: %v", refs)
			}
		})
	}
}

func TestManagedVMRouteUsersRejectCopiedAllocationMarker(t *testing.T) {
	for _, copied := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact-owner", true: "copied-marker"}[copied], func(t *testing.T) {
			journal, intent := infrastructureJournal(t)
			handle := infrastructureHandle(t, journal, "holder", intent)
			vmid := 124
			if copied {
				vmid = 125
			}
			step, err := storageMutationIntent(handle, "vm.QEMU.Create", aj.Target{Node: "pve1", VMID: vmid}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := storageMutationObserved(handle, step, nil, false); err != nil {
				t.Fatal(err)
			}
			record := handle.Record()
			marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Kind: "vm", Namespace: record.Namespace, AllocationID: record.ID, AgentSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(record.AgentID)))})
			if err != nil {
				t.Fatal(err)
			}
			deps, _, c := auditFixture(t)
			tag := advertisedRouteTag("router", "10.60.0.0/16")
			c.configs[124] = map[string]any{"tags": tag, "description": marker}
			deps.PVE = &routeUsersClient{Client: c, listing: &routeUsersNodes{Service: c.Nodes(), tags: tag}}
			refs, err := managedVMRouteUsers(t.Context(), deps, journal, record.Namespace, 123)
			if (err != nil) != copied {
				t.Fatalf("route holder authority: %v", err)
			}
			if !copied && (len(refs[tag]) != 1 || refs[tag][0] != "allocation:"+record.ID) {
				t.Fatalf("exact holder lost: %v", refs)
			}
		})
	}
}

func TestManagedVMHAPurgeRejectsUnreadableResourceIdentity(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong-vm", true: "nil-response"}[missing], func(t *testing.T) {
			journal, intent := infrastructureJournal(t)
			handle := infrastructureHandle(t, journal, "ha", intent)
			service := &infrastructureHACluster{nilResource: missing, wrongResource: !missing}
			if err := managedVMDeleteHA(t.Context(), Deps{PVE: &infrastructureHAClient{ha: service}}, handle, "pve1", 123); err == nil {
				t.Fatal("unproven HA identity accepted")
			}
			if service.deletes != 0 || len(handle.Record().Steps) != 0 {
				t.Fatal("unproven HA identity caused a write")
			}
		})
	}
}

func (c *infrastructureHACluster) ListHaResources(context.Context, *cluster.ListHaResourcesParams) (*cluster.ListHaResourcesResponse, error) {
	rows := cluster.ListHaResourcesResponse{}
	if !c.removed {
		rows = append(rows, json.RawMessage(`{"sid":"vm:123"}`))
	}
	return &rows, nil
}
