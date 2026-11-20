package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// aaMetadataGuest is the config of the one VM a set_vm_metadata call writes,
// which the config read serves and the config write changes.
type aaMetadataGuest struct {
	mu  sync.Mutex
	cfg map[string]any
}

func (g *aaMetadataGuest) config(context.Context, string, int) (map[string]any, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]any, len(g.cfg))
	for k, v := range g.cfg {
		out[k] = v
	}
	return out, nil
}

// aaMetadataNodes adds the config write to the listing fake, so the handler's
// write lands in the guest's config.
type aaMetadataNodes struct {
	*icNodesService
	guest *aaMetadataGuest
}

func (n *aaMetadataNodes) UpdateQemuConfig(_ context.Context, _, _ string, p *sdknodes.UpdateQemuConfigParams) error {
	n.guest.mu.Lock()
	defer n.guest.mu.Unlock()
	if p.Description != nil {
		n.guest.cfg["description"] = *p.Description
	}
	if p.Tags != nil {
		n.guest.cfg[jsonKeyTags] = *p.Tags
	}
	if p.Name != nil {
		n.guest.cfg["name"] = *p.Name
	}
	return nil
}

// aaMetadataCluster adds an idle CRM and a cluster with every member online
// to the anti-affinity stub, which is what the settle wait reads.
type aaMetadataCluster struct {
	*aaClusterStub
}

func (c aaMetadataCluster) ListHaStatusManagerStatus(context.Context) (*cluster.ListHaStatusManagerStatusResponse,
	error,
) {
	return AbsenceIdleCRM(), nil
}

func (c aaMetadataCluster) ListHaStatusCurrent(context.Context) (*cluster.ListHaStatusCurrentResponse, error) {
	return AbsenceNoHAStatus(), nil
}

// aaMetadataClient serves the cluster stub, the lock pools, and the one
// guest's config to the handler.
type aaMetadataClient struct {
	*icPVEClient
	nodes *aaMetadataNodes
}

func (c *aaMetadataClient) Nodes() sdknodes.Service { return c.nodes }

// aaMetadataHandlerDeps returns deps for a set_vm_metadata call on VM 103 of
// the "web" group, on a cluster of four nodes where a rule holds at most
// three members. The members 100, 101, and 102 hold indexes 1, 2, and 3 and
// fill the rule, and VM 103 is staged as a member with no index tag yet.
func aaMetadataHandlerDeps(stub *aaClusterStub) (Deps, *aaMetadataGuest) {
	guestTags := ownerTagFor(aaDirector) + ";" + aaTag
	rows := make([]json.RawMessage, 0, 4)
	for i, v := range []int{100, 101, 102} {
		rows = append(rows, aaMetadataRow(v, aaTag+";index--"+strconv.Itoa(i+1)))
	}
	rows = append(rows, aaMetadataRow(103, guestTags))
	stub.tagged(rows...)
	stub.memberNodes = []string{"pve1", "pve2", "pve3", "pve4"}
	stub.statusFn = func() (*cluster.ListStatusResponse, error) { return AbsenceOnlineStatus(stub.memberNodes...), nil }
	markWebMembers(stub, 100, 101, 102, 103)
	stub.seedRule(aaRule, "vm:100,vm:101,vm:102")

	guest := &aaMetadataGuest{cfg: map[string]any{jsonKeyTags: guestTags, "name": "vm-103"}}
	qemuSvc := &icQEMUService{configFn: guest.config}
	listing := &icNodesService{listFn: stub.ListResources, qemu: func() qemu.Service { return qemuSvc }}
	deps := aaReplanDeps(stub, true)
	deps.PVE = &aaMetadataClient{
		icPVEClient: &icPVEClient{qemuSvc: qemuSvc, clusterSvc: aaMetadataCluster{stub}, poolsSvc: newAALockPools(nil)},
		nodes:       &aaMetadataNodes{icNodesService: listing, guest: guest},
	}
	return deps, guest
}

// aaMetadataRow builds a running guest's row on node pve1.
func aaMetadataRow(vmid int, tags string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"type": "qemu", "vmid": vmid, "tags": tags, "node": "pve1", "status": "running",
		"id": "qemu/" + strconv.Itoa(vmid), "name": "vm-" + strconv.Itoa(vmid),
	})
	return raw
}

// callSetVMMetadata drives the set_vm_metadata handler for VM 103 with the
// given instance index, the way the Director calls it.
func callSetVMMetadata(t *testing.T, deps Deps, index string) error {
	t.Helper()
	md, err := json.Marshal(map[string]any{
		"director": "bosh", "deployment": aaDeployment, "job": "web", "instance_group": "web",
		"index": index, "name": "web/6a0b0e5c-6f43-4f6e-8a0e-2b1f0c9d7e11",
	})
	if err != nil {
		t.Fatal(err)
	}
	args := []json.RawMessage{json.RawMessage(`"103"`), md}
	_, err = HandleSetVMMetadata(deps).Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: aaDirector})
	return err
}

// TestHandleSetVMMetadata_JoinsTheGroupByIndex pins that set_vm_metadata puts
// the VM into its group's rules by the index the Director names. The rule is
// full with indexes 1, 2, and 3, and the VM takes index 0, so it takes the
// place of index 3.
func TestHandleSetVMMetadata_JoinsTheGroupByIndex(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	deps, guest := aaMetadataHandlerDeps(stub)
	if err := callSetVMMetadata(t, deps, "0"); err != nil {
		t.Fatalf("set_vm_metadata = %v; want success", err)
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:103" {
		t.Fatalf("rule = %q; want vm:103 in by its index 0 and vm:102 (index 3) out", got)
	}
	if tags, _ := guest.config(context.Background(), "pve1", 103); !strings.Contains(tags[jsonKeyTags].(string),
		"index--0") {
		t.Fatalf("tags = %q; want the index tag written", tags[jsonKeyTags])
	}
}

// TestHandleSetVMMetadata_AFailedJoinOnlyWarns pins that set_vm_metadata
// succeeds when the rule write of the join fails, and that the failure is
// logged as a warning.
func TestHandleSetVMMetadata_AFailedJoinOnlyWarns(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	deps, _ := aaMetadataHandlerDeps(stub)
	stub.failUpdateRule = true
	logger, obs := log.NewObservedLogger(slog.LevelDebug)
	deps.Logger = logger
	if err := callSetVMMetadata(t, deps, "0"); err != nil {
		t.Fatalf("set_vm_metadata = %v; want success", err)
	}
	if stub.updateRuleCalls == 0 {
		t.Fatal("the call never tried to write the group's rule")
	}
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:102" {
		t.Fatalf("rule = %q; want it unchanged after the failed write", got)
	}
	for _, e := range obs.All() {
		if e.Level == slog.LevelWarn && strings.Contains(e.Message, "could not join") {
			return
		}
	}
	t.Fatalf("no warning about the failed join in %+v", obs.All())
}
