package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdk "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

type managedVMEntryClient struct {
	diagnosticVMClient
	journal     *aj.Journal
	creates     int
	running     bool
	failStatus  bool
	pauseReplan bool
	destroys    int
}

func (c *managedVMEntryClient) QEMU() qemu.Service {
	return &managedVMEntryQEMU{managedDiskTestQEMU{state: c.state}, c}
}
func (c *managedVMEntryClient) Nodes() nodes.Service {
	return &managedVMEntryNodes{managedDiskTestNodes: managedDiskTestNodes{state: c.state}, client: c}
}
func (c *managedVMEntryClient) Cluster() cluster.Service {
	return &managedVMEntryCluster{diagnosticVMCluster{managedDiskTestCluster{state: c.state}}}
}

type managedVMEntryCluster struct{ diagnosticVMCluster }

func (c *managedVMEntryCluster) ListSdnVnets(context.Context, *cluster.ListSdnVnetsParams) (*cluster.ListSdnVnetsResponse, error) {
	rows := cluster.ListSdnVnetsResponse{}
	return &rows, nil
}

type managedVMEntryNodes struct {
	managedDiskTestNodes
	client *managedVMEntryClient
}

func (n *managedVMEntryNodes) ListCertificatesInfo(ctx context.Context, node string) (*nodes.ListCertificatesInfoResponse, error) {
	return (&allocationAuditNodes{}).ListCertificatesInfo(ctx, node)
}

type managedVMEntryQEMU struct {
	managedDiskTestQEMU
	client *managedVMEntryClient
}

func (q *managedVMEntryQEMU) Create(ctx context.Context, node string, params map[string]any) (string, error) {
	vmid, ok := params["vmid"].(int)
	if !ok {
		return "", fmt.Errorf("missing VMID")
	}
	records, err := q.client.journal.List()
	if err != nil {
		return "", err
	}
	recorded := false
	for i := range records {
		for si := range records[i].Steps {
			step := &records[i].Steps[si]
			if step.Target.VMID == vmid && step.Kind == managedVMStepCreate && step.State == aj.Planned {
				recorded = true
			}
		}
	}
	if !recorded {
		return "", fmt.Errorf("remote creation preceded durable VM target intent")
	}
	upid, err := q.managedDiskTestQEMU.Create(ctx, node, params)
	if err != nil {
		return "", err
	}
	q.client.creates++
	drive, ok := params["virtio0"].(string)
	if !ok {
		return "", fmt.Errorf("missing import root")
	}
	pool := strings.Split(drive, ":")[0]
	volume := fmt.Sprintf("%s:%d/vm-%d-disk-0.qcow2", pool, vmid, vmid)
	q.state.configs[vmid]["virtio0"] = volume + ",size=1G"
	q.state.volumes[volume] = &nodes.GetStorageContentResponse{Size: sdk.PVEInt(1 << 30), Format: "qcow2"}
	return upid, nil
}
func (q *managedVMEntryQEMU) Status(context.Context, string, int) (map[string]any, error) {
	if q.client.failStatus {
		q.client.failStatus = false
		return nil, fmt.Errorf("temporary status outage after observed root creation")
	}
	state := "stopped"
	if q.client.running {
		state = "running"
	}
	return map[string]any{"status": state}, nil
}
func (q *managedVMEntryQEMU) Start(context.Context, string, int) (string, error) {
	q.client.running = true
	return "UPID:n1:start", nil
}

func TestManagedVMEntryJournalsBeforeCreateAndResumesWithoutCurrentSets(t *testing.T) {
	for _, tc := range []struct{ retry, pause bool }{{}, {retry: true}, {retry: true, pause: true}} {
		t.Run(fmt.Sprintf("retry=%t,pause=%t", tc.retry, tc.pause), func(t *testing.T) { runManagedVMEntryCase(t, tc.retry, tc.pause) })
	}
}

func runManagedVMEntryCase(t *testing.T, retry, pause bool) {
	t.Helper()
	no := false
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.CPIConfig{Node: "n1", VMStorage: "a", EphemeralStorageSet: "E", StoragePlacementNamespace: "namespace", StorageAllocationJournalDir: directory, AgentMode: config.AgentModeNoAgent, StemcellStrategy: config.StemcellStrategyImport, Placement: &config.PlacementConfig{ExcludeMaintenanceNodes: &no}, StorageSets: map[string]config.StorageSet{"E": {Names: []string{"a", "b"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}}
	if retry {
		limit := 1
		cfg.Placement.FallbackMax = &limit
	}
	journal, err := aj.Initialize(t.Context(), directory, cfg.StoragePlacementNamespace, aj.Enrollment{ClusterID: "pve-root-ca-sha256:" + strings.Repeat("ab", 32), AuthorityID: "authority", AuditID: "audit", CompleteHistoricalAudit: true, PreviousWriterFenced: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Error(err)
		}
	})
	state := &managedDiskTestState{volumes: map[string]*nodes.GetStorageContentResponse{"a:import/stemcell.qcow2": {Size: sdk.PVEInt(1 << 30), Format: "qcow2"}}}
	client := &managedVMEntryClient{diagnosticVMClient: diagnosticVMClient{managedDiskTestPVE{state: state}}, journal: journal, failStatus: retry, pauseReplan: pause}
	deps := Deps{Config: cfg, PVE: client, Logger: log.NewNopLogger()}
	args := []json.RawMessage{json.RawMessage(`"entry-agent"`), json.RawMessage(`":heavy:a:import/stemcell.qcow2"`), json.RawMessage(`{"cpu":1,"ram":1024,"root_disk_size":1024}`), json.RawMessage(`{}`), json.RawMessage(`[]`), json.RawMessage(`{}`)}
	result, err := createVM(t.Context(), deps, args)
	if pause {
		if err == nil || client.creates != 1 || client.destroys != 1 {
			t.Fatalf("replan outage did not stop after exact cleanup: %v", err)
		}
		closed, found, inspectErr := journal.InspectVM("entry-agent")
		if inspectErr != nil || !found || !managedVMAttemptClosed(closed) || len(state.configs) != 0 {
			t.Fatalf("cleanup checkpoint not durable: %v", inspectErr)
		}
		result, err = createVM(t.Context(), deps, args)
	}
	if err != nil {
		t.Fatal(err)
	}
	values, ok := result.([]any)
	if !ok || len(values) == 0 {
		t.Fatalf("unexpected VM result %v", result)
	}
	cid, ok := values[0].(string)
	if !ok {
		t.Fatal("missing CID")
	}
	vmid, err := strconv.Atoi(cid)
	if err != nil {
		t.Fatal(err)
	}
	expectedCreates := 1
	if retry {
		expectedCreates = 2
	}
	if client.creates != expectedCreates || len(state.configs) != 1 || state.configs[vmid] == nil {
		t.Fatal("fresh managed request did not create exactly one VM")
	}
	record, found, err := journal.InspectVM("entry-agent")
	if err != nil || !found || record.State != aj.ReadyToReturn {
		t.Fatalf("ready generation not durable: %+v %v", record, err)
	}
	if retry && (client.destroys != 1 || len(record.Attempts) != 2 || record.Attempts[0].Completion == nil) {
		t.Fatalf("cleanup/retry lost bounded same-generation history: destroys=%d attempts=%d", client.destroys, len(record.Attempts))
	}
	cfg.EphemeralStorageSet = ""
	cfg.StorageSets = nil
	resumed, err := createVM(t.Context(), deps, args)
	if err != nil {
		t.Fatal(err)
	}
	ready, ok := resumed.([]any)
	if !ok || ready[0] != cid || client.creates != expectedCreates {
		t.Fatalf("removed policy duplicated generation: %v", resumed)
	}
}

func (c *managedVMEntryCluster) ListHaRules(context.Context, *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	rows := cluster.ListHaRulesResponse{}
	return &rows, nil
}
func (c *managedVMEntryCluster) GetHaResources(context.Context, string) (*cluster.GetHaResourcesResponse, error) {
	return nil, cpierrors.VMNotFound("ha")
}
func (n *managedVMEntryNodes) DeleteQemu(_ context.Context, _ string, id string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
	vmid, err := strconv.Atoi(id)
	if err != nil {
		return nil, err
	}
	if params == nil || params.DestroyUnreferencedDisks == nil || *params.DestroyUnreferencedDisks {
		return nil, fmt.Errorf("cleanup used unbounded volume destruction")
	}
	for key, value := range n.state.configs[vmid] {
		if !managedVMVolumeDevice(key) {
			continue
		}
		drive, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("malformed owned volume")
		}
		delete(n.state.volumes, strings.Split(drive, ",")[0])
	}
	delete(n.state.configs, vmid)
	n.client.destroys++
	raw := json.RawMessage(`"UPID:n1:destroy"`)
	return &raw, nil
}

func (q *managedVMEntryQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if q.state.configs[vmid] == nil {
		return nil, cpierrors.VMNotFound(strconv.Itoa(vmid))
	}
	return q.managedDiskTestQEMU.Config(ctx, node, vmid)
}

func (n *managedVMEntryNodes) GetStorageContent(ctx context.Context, node, pool, volume string) (*nodes.GetStorageContentResponse, error) {
	if n.client.destroys > 0 && n.client.pauseReplan && strings.Contains(volume, "import/") {
		n.client.pauseReplan = false
		return nil, fmt.Errorf("source observation unavailable during fresh planning after cleanup")
	}
	return n.managedDiskTestNodes.GetStorageContent(ctx, node, pool, volume)
}

func (c *managedVMEntryCluster) ListHaResources(context.Context, *cluster.ListHaResourcesParams) (*cluster.ListHaResourcesResponse, error) {
	rows := cluster.ListHaResourcesResponse{}
	return &rows, nil
}
