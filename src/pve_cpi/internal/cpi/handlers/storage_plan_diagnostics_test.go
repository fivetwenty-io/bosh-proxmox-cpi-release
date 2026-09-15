package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdk "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

func TestStoragePlanDiagnosticUsesProductionDiskPlanWithoutMutation(t *testing.T) {
	m, _, state := managedDiskFixture(t, "spread", true)
	before := diagnosticFiles(t, m.deps.Config.StorageAllocationJournalDir)
	request := StoragePlanDiagnosticRequest{Method: "create_disk", Arguments: []json.RawMessage{json.RawMessage(`1025`), json.RawMessage(`{"tags":{"secret":"DO-NOT-PRINT"}}`)}}
	report, err := ObserveStoragePlanDiagnostics(t.Context(), m.deps, request)
	if err != nil {
		t.Fatal(err)
	}
	if report.Node != "n1" || len(report.Targets) != 1 || report.Targets[0].ChargeBytes != m.plan.Targets[0].ChargeBytes || len(report.CandidateRanks) != 2 || len(report.Seed) != 64 || !report.ObservationOnly {
		t.Fatalf("invalid report: %+v", report)
	}
	if report.Targets[0].StorageID != report.CandidateRanks[0].Ranking.Candidate.StorageID {
		t.Fatal("chosen target does not match production rank")
	}
	if !reflect.DeepEqual(report.FrozenMembership["P"], []string{"a", "b"}) || len(report.Capacities) != 2 {
		t.Fatalf("missing observations: %+v", report)
	}
	if len(state.created) != 0 || state.parkMutations != 0 || len(state.volumes) != 0 {
		t.Fatal("diagnostics mutated PVE")
	}
	if !reflect.DeepEqual(before, diagnosticFiles(t, m.deps.Config.StorageAllocationJournalDir)) {
		t.Fatal("diagnostics changed private journal")
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "DO-NOT-PRINT") {
		t.Fatal("request secret leaked")
	}
}
func diagnosticFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			b, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			out[path] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestStoragePlanDiagnosticNoJournalConfiguredAndCapacityFailure(t *testing.T) {
	state := &managedDiskTestState{volumes: map[string]*nodes.GetStorageContentResponse{}}
	cfg := &config.CPIConfig{Node: "n1", StoragePlacementNamespace: "namespace", PersistentStorageSet: "P", StorageSets: map[string]config.StorageSet{"P": {Names: []string{"a"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}}
	deps := Deps{Config: cfg, PVE: managedDiskTestPVE{state: state}, Logger: log.NewNopLogger()}
	for _, size := range []string{"1", "1024000"} {
		report, err := ObserveStoragePlanDiagnostics(t.Context(), deps, StoragePlanDiagnosticRequest{Method: "create_disk", Arguments: []json.RawMessage{json.RawMessage(size), json.RawMessage(`{}`)}})
		if err != nil {
			t.Fatal(err)
		}
		if size == "1" && (report.Node != "n1" || len(report.Findings) == 0) {
			t.Fatalf("journal-less observation failed: %+v", report)
		}
		if size != "1" && (len(report.Rejections) == 0 || len(report.Capacities) == 0) {
			t.Fatalf("capacity failure lost observation: %+v", report)
		}
	}
	if len(state.created) != 0 || state.parkMutations != 0 {
		t.Fatal("mutation")
	}
}
func TestStoragePlanDiagnosticRedactsInvalidVMRequest(t *testing.T) {
	m, _, _ := managedDiskFixture(t, "spread", false)
	report, err := ObserveStoragePlanDiagnostics(t.Context(), m.deps, StoragePlanDiagnosticRequest{Method: "create_vm", Arguments: []json.RawMessage{json.RawMessage(`"agent-secret"`), json.RawMessage(`"SECRET-STEMCELL"`), json.RawMessage(`{}`), json.RawMessage(`{}`), json.RawMessage(`[]`), json.RawMessage(`{"password":"SECRET-ENV"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "SECRET") || len(report.Rejections) == 0 {
		t.Fatalf("unsafe response: %s", b)
	}
}

func (n managedDiskTestNodes) ListCertificatesInfo(context.Context, string) (*nodes.ListCertificatesInfoResponse, error) {
	return nil, errors.New("test certificate unavailable")
}

type diagnosticVMClient struct{ managedDiskTestPVE }

func (c diagnosticVMClient) Cluster() cluster.Service {
	return diagnosticVMCluster{managedDiskTestCluster{state: c.state}}
}

type diagnosticVMCluster struct{ managedDiskTestCluster }

func (c diagnosticVMCluster) ListStatus(context.Context) (*cluster.ListStatusResponse, error) {
	r := cluster.ListStatusResponse{json.RawMessage(`{"type":"node","name":"n1","online":1,"maxcpu":8,"maxmem":17179869184,"mem":1073741824,"cpu":0.1}`)}
	return &r, nil
}
func TestStoragePlanDiagnosticVMImportUsesProductionPlanner(t *testing.T) {
	no := false
	cfg := &config.CPIConfig{Node: "n1", EphemeralStorageSet: "E", StoragePlacementNamespace: "namespace", AgentMode: config.AgentModeNoAgent, StemcellStrategy: config.StemcellStrategyImport, Placement: &config.PlacementConfig{ExcludeMaintenanceNodes: &no}, StorageSets: map[string]config.StorageSet{"E": {Names: []string{"a", "b"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}}
	state := &managedDiskTestState{volumes: map[string]*nodes.GetStorageContentResponse{"a:import/stemcell.qcow2": {Size: sdk.PVEInt(1 << 30), Format: "qcow2"}}}
	deps := Deps{Config: cfg, PVE: diagnosticVMClient{managedDiskTestPVE{state: state}}, Logger: log.NewNopLogger()}
	args := []json.RawMessage{json.RawMessage(`"agent"`), json.RawMessage(`":heavy:a:import/stemcell.qcow2"`), json.RawMessage(`{"cpu":1,"ram":1024,"ephemeral_disk_size_mb":1024}`), json.RawMessage(`{}`), json.RawMessage(`[]`), json.RawMessage(`{}`)}
	report, err := ObserveStoragePlanDiagnostics(t.Context(), deps, StoragePlanDiagnosticRequest{Method: "create_vm", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if report.Node != "n1" || len(report.Targets) == 0 || report.Targets[0].Mechanism != "import" || len(report.CandidateRanks) != 2 {
		parsed, e := parseCreateVMArgs(args)
		if e != nil {
			t.Fatal(e)
		}
		sel, e := ResolveStoragePlacementSelectors(cfg, "create_vm", parsed.cloudPropsMap, parsed.cloudProps.EphemeralDiskSizeMB > 0)
		if e != nil {
			t.Fatal(e)
		}
		_, e = prepareManagedVMPlan(t.Context(), deps, parsed, sel, nil, "")
		t.Fatalf("VM plan missing: %v %+v", e, report)
	}
	cfg.AgentMode = ""
	cfg.ISOStorage = "local"
	withISO, err := ObserveStoragePlanDiagnostics(t.Context(), deps, StoragePlanDiagnosticRequest{Method: "create_vm", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	foundISO := false
	for _, target := range withISO.Targets {
		if target.Role == "iso" {
			foundISO = true
			if target.StorageID != withISO.Targets[0].StorageID || target.Mechanism != "upload" {
				t.Fatalf("wrong ISO choice: %+v", target)
			}
		}
	}
	if !foundISO {
		t.Fatalf("missing ISO plan: %+v", withISO)
	}
	if len(state.created) != 0 || state.parkMutations != 0 || len(state.volumes) != 1 {
		t.Fatal("VM diagnostic changed PVE")
	}
}

type diagnosticVMDefinitions struct{ managedDiskTestDefinitions }

func (c diagnosticVMClient) ClusterStorage() clusterstorage.Service { return diagnosticVMDefinitions{} }
func (diagnosticVMDefinitions) ListStorage(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	r := clusterstorage.ListStorageResponse{json.RawMessage(`{"storage":"a","type":"nfs","server":"nas","export":"/a","shared":1,"content":"images,import,iso"}`), json.RawMessage(`{"storage":"b","type":"nfs","server":"nas","export":"/b","shared":1,"content":"images,import,iso"}`)}
	return &r, nil
}

func TestStoragePlanDiagnosticRetainedCIDWithoutLiveContinuity(t *testing.T) {
	m, h, state := managedDiskFixture(t, "spread", false)
	cid, err := m.execute(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	before := diagnosticFiles(t, m.deps.Config.StorageAllocationJournalDir)
	created := len(state.created)
	report, err := ObserveStoragePlanDiagnostics(t.Context(), m.deps, StoragePlanDiagnosticRequest{Method: "create_disk", Arguments: []json.RawMessage{json.RawMessage(`1025`), json.RawMessage(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range report.Journal {
		if record.AllocationID == m.id && record.CID == cid {
			found = true
		}
	}
	if !found {
		t.Fatalf("retained exact CID missing: %+v", report.Journal)
	}
	if len(report.Findings) == 0 || !strings.Contains(report.Findings[0], "not checked against live cluster") {
		t.Fatalf("continuity claim missing: %+v", report.Findings)
	}
	if len(state.created) != created || !reflect.DeepEqual(before, diagnosticFiles(t, m.deps.Config.StorageAllocationJournalDir)) {
		t.Fatal("retained-record observation mutated state")
	}
}

func TestStoragePlanDiagnosticMissingIndexDoesNotHideRecordsOrRepair(t *testing.T) {
	m, _, _ := managedDiskFixture(t, "spread", false)
	removed := false
	for path := range diagnosticFiles(t, m.deps.Config.StorageAllocationJournalDir) {
		if filepath.Base(path) == "index.json" {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			removed = true
		}
	}
	if !removed {
		t.Fatal("fixture index absent")
	}
	before := diagnosticFiles(t, m.deps.Config.StorageAllocationJournalDir)
	report, err := ObserveStoragePlanDiagnostics(t.Context(), m.deps, StoragePlanDiagnosticRequest{Method: "create_disk", Arguments: []json.RawMessage{json.RawMessage(`1025`), json.RawMessage(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Journal) != 1 || !strings.Contains(strings.Join(report.Findings, ";"), "generation index invalid") {
		t.Fatalf("invalid index not reported: %+v", report)
	}
	if !reflect.DeepEqual(before, diagnosticFiles(t, m.deps.Config.StorageAllocationJournalDir)) {
		t.Fatal("diagnostics repaired missing index")
	}
}
