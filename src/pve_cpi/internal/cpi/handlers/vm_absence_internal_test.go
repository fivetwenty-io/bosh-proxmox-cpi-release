package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestJournalRecordOnOfflineMember(t *testing.T) {
	t.Parallel()

	step := func(node string, vmid int, external bool) aj.Step {
		return aj.Step{ID: "s", Kind: "vm.create", Target: aj.Target{Node: node, VMID: vmid, External: external}}
	}
	record := func(state aj.State, steps ...aj.Step) aj.Record {
		return aj.Record{ID: "rec-1", Kind: "vm", State: state, Steps: steps}
	}
	offline := []string{"pve1", "pve3"}

	cases := []struct {
		name       string
		records    []aj.Record
		wantMember string
	}{
		{name: "no records"},
		{name: "planned step on an offline member", records: []aj.Record{record(aj.Planned, step("pve1", 123, false))}, wantMember: "pve1"},
		{name: "open record on the second offline member", records: []aj.Record{record(aj.ReadyToReturn, step("pve3", 123, false))}, wantMember: "pve3"},
		{name: "deleted record", records: []aj.Record{record(aj.Deleted, step("pve1", 123, false))}},
		{name: "cleaned record", records: []aj.Record{record(aj.Cleaned, step("pve1", 123, false))}},
		{name: "retained after the VM's delete", records: []aj.Record{record(aj.VMDeletedRetained, step("pve1", 123, false))}},
		{name: "external step", records: []aj.Record{record(aj.Observed, step("pve1", 123, true))}},
		{name: "another VMID", records: []aj.Record{record(aj.Observed, step("pve1", 124, false))}},
		{name: "an online member", records: []aj.Record{record(aj.Observed, step("pve2", 123, false))}},
		{
			name: "match after records that do not tie",
			records: []aj.Record{
				record(aj.Deleted, step("pve1", 123, false)),
				record(aj.Observed, step("pve2", 123, false), step("pve1", 123, true), step("pve3", 123, false)),
			},
			wantMember: "pve3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			member, detail := journalRecordOnOfflineMember(tc.records, 123, offline)
			if member != tc.wantMember {
				t.Fatalf("member = %q, want %q", member, tc.wantMember)
			}
			if member == "" && detail != "" {
				t.Errorf("detail = %q with no member", detail)
			}
			if member != "" && !strings.Contains(detail, "allocation journal record rec-1") {
				t.Errorf("detail = %q, want it to name the record", detail)
			}
		})
	}
}

func TestJournalOfflineMemberTie_Unconfigured(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]*config.CPIConfig{
		"no config":    nil,
		"no directory": {StoragePlacementNamespace: "director"},
		"no namespace": {StorageAllocationJournalDir: t.TempDir()},
		"blank values": {StorageAllocationJournalDir: "  ", StoragePlacementNamespace: " "},
	} {
		if journalOfflineMemberTie(cfg) != nil {
			t.Errorf("%s: want no tie source", name)
		}
	}
}

func TestJournalOfflineMemberTie_NotEnrolledTiesNothing(t *testing.T) {
	t.Parallel()

	tie := journalOfflineMemberTie(&config.CPIConfig{StorageAllocationJournalDir: t.TempDir(), StoragePlacementNamespace: "director"})
	if tie == nil {
		t.Fatal("want a tie source for a configured journal")
	}
	member, detail, err := tie(context.Background(), 123, []string{"pve1"})
	if err != nil || member != "" || detail != "" {
		t.Fatalf("tie = (%q, %q, %v), want nothing for a journal that is not enrolled", member, detail, err)
	}
}

func TestJournalOfflineMemberTie_EnrolledRecord(t *testing.T) {
	deps, _, _, record, _ := resumeVMFixture(t)
	tie := journalOfflineMemberTie(deps.Config)
	if tie == nil {
		t.Fatal("want a tie source for a configured journal")
	}

	member, detail, err := tie(context.Background(), 123, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if member != "pve1" || !strings.Contains(detail, record.ID) {
		t.Fatalf("tie = (%q, %q), want pve1 and record %s", member, detail, record.ID)
	}
	for _, probe := range []struct {
		vmid    int
		offline []string
	}{{123, []string{"pve2"}}, {124, []string{"pve1"}}} {
		member, _, err := tie(context.Background(), probe.vmid, probe.offline)
		if err != nil || member != "" {
			t.Errorf("tie(%d, %v) = (%q, %v), want no tie", probe.vmid, probe.offline, member, err)
		}
	}
}

// offlineMemberHasVMFixture is a two-member cluster where pve1 has gone dark
// and VM 123 is in neither the index nor pve2's configs, while the journal
// still holds the open record of its create on pve1.
func offlineMemberHasVMFixture(t *testing.T) Deps {
	t.Helper()
	deps, _, c, _, _ := resumeVMFixture(t)
	c.mu.Lock()
	delete(c.configs, 123)
	c.mu.Unlock()
	c.configFailure = map[int]error{123: &sdkerrors.APIError{HTTPCode: 404}}
	c.clusterRead = &allocationAuditCluster{
		Service: c.idFakeClient.Cluster(),
		members: []string{"pve1", "pve2"},
		status: []map[string]any{
			{"type": "cluster", "name": "fixture", "quorate": 1},
			{"type": "node", "name": "pve1", "online": 0},
			{"type": "node", "name": "pve2", "online": 1},
		},
	}
	return deps
}

func TestHasVM_OfflineMemberTiedByTheJournalIsRetriable(t *testing.T) {
	deps := offlineMemberHasVMFixture(t)

	result, err := HandleHasVM(deps).Handle(context.Background(), []json.RawMessage{json.RawMessage(`"123"`)}, jsonrpc.Context{})

	if err == nil {
		t.Fatalf("has_vm = %v, want a retriable error while the journal ties VM 123 to offline pve1", result)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want retriable", err)
	}
	for _, want := range []string{"allocation journal record", "pve1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err.Error(), want)
		}
	}
}

func TestHasVM_OfflineMemberWithNoTieAnswersFalse(t *testing.T) {
	deps := offlineMemberHasVMFixture(t)
	untied := *deps.Config
	untied.StorageAllocationJournalDir = ""
	deps.Config = &untied

	result, err := HandleHasVM(deps).Handle(context.Background(), []json.RawMessage{json.RawMessage(`"123"`)}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("has_vm: %v, want false when nothing ties VM 123 to the offline member", err)
	}
	if result != false {
		t.Fatalf("has_vm = %v, want false", result)
	}
}

// rbfdMovedClient is rbfdClient on a two-member cluster where VM 6031's config
// has already moved from pve01 to pve02.
type rbfdMovedClient struct {
	*rbfdClient
}

func (c *rbfdMovedClient) config(_ context.Context, node string, _ int) (map[string]any, error) {
	if node != "pve02" {
		return nil, &sdkerrors.APIError{HTTPCode: 404}
	}
	return map[string]any{}, nil
}

func (c *rbfdMovedClient) QEMU() qemu.Service { return &rbfdMovedQEMU{rbfdQEMUStub: c.qemu, client: c} }

func (c *rbfdMovedClient) Nodes() sdknodes.Service {
	return &SlotDeleteNodes{Service: c.nodes, Config: c.config, Delete: c.qemu.deleteConfigKey}
}

func (c *rbfdMovedClient) Cluster() sdkcluster.Service {
	s := newNAStub()
	s.members = []string{"pve01", "pve02"}
	return s
}

type rbfdMovedQEMU struct {
	*rbfdQEMUStub
	client *rbfdMovedClient
}

func (q *rbfdMovedQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	return q.client.config(ctx, node, vmid)
}

func TestProtectForeignDisksOnRollback_NilConfigMovedVMIsRetriable(t *testing.T) {
	callLog := []string{}
	nodesStub := &rbfdNodesStub{callLog: &callLog}
	client := &rbfdMovedClient{rbfdClient: &rbfdClient{nodes: nodesStub, qemu: &rbfdQEMUStub{callLog: &callLog}}}
	deps := Deps{PVE: client, Logger: log.NewNopLogger()}

	err := protectForeignDisksOnRollback(context.Background(), deps, "pve01", "6031", 6031, log.NewNopLogger())
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !strings.Contains(err.Error(), "pve02") {
		t.Fatalf("err = %v, want retriable naming pve02, where the VM's config is now", err)
	}

	cleanupVM(context.Background(), deps, "pve01", 6031, nil, log.NewNopLogger())
	if nodesStub.deleteCalls != 0 {
		t.Fatalf("DeleteQemu calls = %d, want none for a VM that moved before the purge", nodesStub.deleteCalls)
	}
}

// ownDownCluster is ownTestCluster with one member listed offline, or with a
// /cluster/status that cannot be read.
type ownDownCluster struct {
	ownTestCluster
	down       string
	statusErr  error
	notQuorate bool
}

func (c ownDownCluster) ListStatus(context.Context) (*sdkcluster.ListStatusResponse, error) {
	if c.statusErr != nil {
		return nil, c.statusErr
	}
	resp := make(sdkcluster.ListStatusResponse, 0, 1+len(c.nodes))
	quorate := 1
	if c.notQuorate {
		quorate = 0
	}
	resp = append(resp, json.RawMessage(fmt.Sprintf(`{"type":"cluster","name":"fixture","quorate":%d}`, quorate)))
	for _, n := range c.nodes {
		online := 1
		if n == c.down {
			online = 0
		}
		row, _ := json.Marshal(map[string]any{"type": "node", "name": n, "online": online})
		resp = append(resp, row)
	}
	return &resp, nil
}

// TestReadAtVMNode_ConfigMissingNeedsANodeThatCanAnswer pins the proxy-target
// rule on the read every ownership check makes: a config-missing answer from
// a node the cluster does not report able to answer, or from a cluster whose
// status cannot be read, is retriable and never errVMAbsent.
func TestReadAtVMNode_ConfigMissingNeedsANodeThatCanAnswer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cluster sdkcluster.Service
		want    string
	}{
		{name: "owner offline", cluster: ownDownCluster{ownTestCluster: ownTestCluster{nodes: []string{"n1", "n2"}}, down: "n1"},
			want: "does not report that node able to answer"},
		{name: "cluster not quorate",
			cluster: ownDownCluster{ownTestCluster: ownTestCluster{nodes: []string{"n1", "n2"}}, notQuorate: true},
			want:    "does not report that node able to answer"},
		{name: "status unreadable",
			cluster: ownDownCluster{ownTestCluster: ownTestCluster{nodes: []string{"n1", "n2"}}, statusErr: sdkerrors.ErrNotFound},
			want:    "could not be read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &ownTestQEMU{err: sdkerrors.ErrNotFound}
			deps := ownTestDeps(q)
			deps.PVE = ownTestClient{qemu: q, cluster: tc.cluster}

			_, _, err := readAtVMNode(context.Background(), deps, "test", 101, "n1", func(node string) (map[string]any, error) {
				return q.Config(context.Background(), node, 101)
			})

			if err == nil || errors.Is(err, errVMAbsent) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("err = %v, want retriable and not errVMAbsent", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q", err.Error(), tc.want)
			}
			if q.reads != 1 {
				t.Errorf("config reads = %d, want only the first read before the cluster refused the answer", q.reads)
			}
		})
	}
}

// TestReadAtVMNode_StaleIndexRelocates covers the index that still names the
// old owner after a migration: the read moves to the member that holds the
// VM now instead of reporting it gone.
func TestReadAtVMNode_StaleIndexRelocates(t *testing.T) {
	t.Parallel()

	q := &ownTestQEMU{byNode: map[string]ownTestAnswer{
		"n1": {err: sdkerrors.ErrNotFound},
		"n2": {cfg: map[string]any{"tags": "moved"}},
	}}
	deps := ownTestDepsWithMembers(q, "n1", "n2")

	got, node, err := readAtVMNode(context.Background(), deps, "test", 101, "n1", func(node string) (map[string]any, error) {
		return q.Config(context.Background(), node, 101)
	})
	if err != nil {
		t.Fatalf("readAtVMNode: %v", err)
	}
	if node != "n2" || got["tags"] != "moved" {
		t.Fatalf("read (%v) on %s, want the config on n2", got, node)
	}
}

// lingeringCRMCluster serves an index with no VM, no HA resource, and a CRM
// whose service_status lists vm:<vmid> for the first lingering reads and then
// drops it.
type lingeringCRMCluster struct {
	ownTestCluster
	vmid      int
	lingering int32
	resources []string
	reads     atomic.Int32
}

func (c *lingeringCRMCluster) ListResources(
	context.Context, *sdkcluster.ListResourcesParams,
) (*sdkcluster.ListResourcesResponse, error) {
	resp := sdkcluster.ListResourcesResponse{}
	return &resp, nil
}

func (c *lingeringCRMCluster) ListHaResources(
	context.Context, *sdkcluster.ListHaResourcesParams,
) (*sdkcluster.ListHaResourcesResponse, error) {
	return AbsenceHAResources(c.resources...), nil
}

func (c *lingeringCRMCluster) ListHaStatusManagerStatus(
	context.Context,
) (*sdkcluster.ListHaStatusManagerStatusResponse, error) {
	if c.reads.Add(1) > c.lingering {
		return AbsenceIdleCRM(), nil
	}
	resp := sdkcluster.ListHaStatusManagerStatusResponse(
		fmt.Sprintf(`{"manager_status":{"service_status":{"vm:%d":{"state":"started","node":"n1"}}}}`, c.vmid))
	return &resp, nil
}

func lingeringDeps(c *lingeringCRMCluster) Deps {
	q := &ownTestQEMU{err: sdkerrors.ErrNotFound}
	deps := ownTestDeps(q)
	deps.PVE = ownTestClient{qemu: q, cluster: c}
	return deps
}

func lingeringFixture(lingering int32, resources ...string) *lingeringCRMCluster {
	return &lingeringCRMCluster{
		ownTestCluster: ownTestCluster{nodes: []string{"n1", "n2"}},
		vmid:           701,
		lingering:      lingering,
		resources:      resources,
	}
}

func TestPollManagedVMDestroyAbsence_WaitsForTheCRMToDropTheService(t *testing.T) {
	t.Parallel()
	c := lingeringFixture(2)
	deps := lingeringDeps(c)

	loc, err := pollManagedVMDestroyAbsence(context.Background(), 5*time.Second, time.Millisecond,
		func() (pve.VMLocation, error) { return findVMAuthoritative(context.Background(), deps, c.vmid) })

	if err != nil || loc.Found {
		t.Fatalf("loc = %+v, err = %v; want absence proven once the CRM drops the service", loc, err)
	}
	if got := c.reads.Load(); got != 3 {
		t.Errorf("CRM reads = %d, want 3 (two lingering answers, then the clear one)", got)
	}
}

func TestPollManagedVMDestroyAbsence_StaysBoundedAndRetriableWhenTheCRMNeverDrops(t *testing.T) {
	t.Parallel()
	c := lingeringFixture(1 << 30)
	deps := lingeringDeps(c)

	start := time.Now()
	_, err := pollManagedVMDestroyAbsence(context.Background(), 60*time.Millisecond, 5*time.Millisecond,
		func() (pve.VMLocation, error) { return findVMAuthoritative(context.Background(), deps, c.vmid) })

	if err == nil || !errors.Is(err, pve.ErrHAManagerHoldsService) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want the retriable CRM-still-holds error", err)
	}
	if c.reads.Load() < 2 {
		t.Errorf("CRM reads = %d, want the proof repeated before the wait ended", c.reads.Load())
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("poll took %s, want it bounded by the wait", elapsed)
	}
}

func TestPollManagedVMDestroyAbsence_OtherRefusalsReturnAtOnce(t *testing.T) {
	t.Parallel()
	c := lingeringFixture(0, "vm:701")
	deps := lingeringDeps(c)
	calls := 0

	_, err := pollManagedVMDestroyAbsence(context.Background(), 5*time.Second, time.Millisecond,
		func() (pve.VMLocation, error) {
			calls++
			return findVMAuthoritative(context.Background(), deps, c.vmid)
		})

	if err == nil || errors.Is(err, pve.ErrHAManagerHoldsService) || !strings.Contains(err.Error(), "is still configured") {
		t.Fatalf("err = %v, want the configured-resource refusal", err)
	}
	if calls != 1 {
		t.Errorf("proof ran %d times, want 1 because a configured resource is not cured by waiting", calls)
	}
}
