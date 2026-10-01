package handlers

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// retainRerunPVE runs whole delete_vm calls over the legacy retention flow
// fixture. It adds the stop and destroy the fixture leaves out: a stop that
// succeeds at once, and a destroy that frees the volumes the VM owns.
type retainRerunPVE struct {
	*lifecycleFlowPVE
	destroyed map[int]bool
}

func (c *retainRerunPVE) QEMU() qemu.Service {
	return retainRerunQEMU{Service: c.lifecycleFlowPVE.QEMU()}
}

func (c *retainRerunPVE) Nodes() nodes.Service {
	return retainRerunNodes{Service: c.lifecycleFlowPVE.Nodes(), c: c}
}

func (c *retainRerunPVE) Cluster() cluster.Service {
	return retainRerunCluster{Service: c.lifecycleFlowPVE.Cluster()}
}

// retainRerunCluster answers the node-affinity pin removal with the
// not-found a VM without a pin gets.
type retainRerunCluster struct{ cluster.Service }

func (retainRerunCluster) DeleteHaRules(context.Context, string) error {
	return &sdkerrors.APIError{HTTPCode: 404}
}

func (retainRerunCluster) DeleteHaResources(context.Context, string, *cluster.DeleteHaResourcesParams) error {
	return &sdkerrors.APIError{HTTPCode: 404}
}

type retainRerunQEMU struct{ qemu.Service }

func (retainRerunQEMU) Stop(context.Context, string, int) (string, error) { return "", nil }

type retainRerunNodes struct {
	nodes.Service
	c *retainRerunPVE
}

func (n retainRerunNodes) DeleteQemu(_ context.Context, _ string, vmidText string, _ *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
	id, err := strconv.Atoi(vmidText)
	if err != nil {
		return nil, err
	}
	cfg, ok := n.c.state.configs[id]
	if !ok {
		return nil, &sdkerrors.APIError{HTTPCode: 404}
	}
	owner := ownerVolumePrefix(id)
	for _, value := range qemu.ParseDisks(cfg) {
		volid, _, _ := strings.Cut(value, ",")
		if strings.HasPrefix(volid[strings.LastIndexAny(volid, ":/")+1:], owner) {
			delete(n.c.state.volumes, volid)
		}
	}
	delete(n.c.state.configs, id)
	n.c.destroyed[id] = true
	resp := nodes.DeleteQemuResponse{}
	return &resp, nil
}

// UpdateQemuConfig applies tag writes, which the fixture's config update
// leaves out, so the fast path's bosh-deleting stamp lands.
func (n retainRerunNodes) UpdateQemuConfig(ctx context.Context, node, vmidText string, p *nodes.UpdateQemuConfigParams) error {
	if p != nil && p.Tags != nil {
		id, err := strconv.Atoi(vmidText)
		if err != nil {
			return err
		}
		if cfg, ok := n.c.state.configs[id]; ok {
			cfg[jsonKeyTags] = *p.Tags
		}
	}
	return n.Service.UpdateQemuConfig(ctx, node, vmidText, p)
}

func ownerVolumePrefix(vmid int) string { return "vm-" + strconv.Itoa(vmid) + "-" }

type retainRerunAgent struct{}

func (retainRerunAgent) Configure(context.Context, string, int, agent.AgentConfig) error { return nil }
func (retainRerunAgent) Remove(context.Context, string, int) error                       { return nil }

// retainRerunFixture is legacyRetainFlowFixture with the ephemeral volume
// named by the caller, so the file-backed form can be covered as well as the
// block form.
func retainRerunFixture(t *testing.T, volume string, fastPath bool) (Deps, *retainRerunPVE) {
	t.Helper()
	deps, client, _, _, _ := lifecycleFlowFixture(t)
	original := strings.Split(client.state.configs[777]["scsi1"].(string), ",")[0]
	client.state.volumes[volume] = client.state.volumes[original]
	delete(client.state.volumes, original)
	client.state.configs[777]["scsi1"] = volume + ",size=5G"
	client.state.configs[777]["tags"] = tagRetainEphemeral
	deps.Config.StoragePlacementNamespace = ""
	deps.Config.StorageAllocationJournalDir = ""
	deps.Config.FastPathDelete = &fastPath
	rerun := &retainRerunPVE{lifecycleFlowPVE: client, destroyed: map[int]bool{}}
	deps.PVE = rerun
	deps.Agent = retainRerunAgent{}
	deps.Logger = log.NewNopLogger()
	return deps, rerun
}

func deleteVMCall(t *testing.T, ctx context.Context, deps Deps, vmCID string) error {
	t.Helper()
	_, err := HandleDeleteVM(deps).Handle(ctx, []json.RawMessage{json.RawMessage(strconv.Quote(vmCID))}, jsonrpc.Context{})
	return err
}

// assertCutShortRetention checks the shape a cut-short retention leaves: the
// ephemeral volume is still on the VM's active slot, and the VM's
// bosh_attached_disks record names it under its bare volid.
func assertCutShortRetention(t *testing.T, client *retainRerunPVE, volume string) {
	t.Helper()
	cfg := client.state.configs[777]
	if cfg == nil {
		t.Fatal("the first delete destroyed the VM")
	}
	if slot, _ := pve.ConfigString(cfg, "scsi1"); !strings.HasPrefix(slot, volume) {
		t.Fatalf("the ephemeral volume must still be on scsi1; got %q", slot)
	}
	if pve.GetAttachedDiskCIDs(pve.DescriptionFromConfig(cfg))[volume] == "" {
		t.Fatal("the first retention must have recorded the ephemeral volume's CID")
	}
	if client.state.volumes[volume] == nil {
		t.Fatal("the cut-short retention lost the ephemeral volume")
	}
}

// assertRetentionFinished checks the rerun moved the ephemeral volume to a
// parker exactly once, with its contents, and destroyed the VM.
func assertRetentionFinished(t *testing.T, client *retainRerunPVE, volume string) {
	t.Helper()
	if !client.destroyed[777] {
		t.Error("the rerun must destroy the VM")
	}
	if client.moves != 1 {
		t.Errorf("the ephemeral volume must move to a parker exactly once; got %d moves", client.moves)
	}
	if client.state.volumes[volume] != nil {
		t.Error("the ephemeral volume must have moved off the VM's name")
	}
	preserved := false
	for _, info := range client.state.volumes {
		if info != nil && info.Size == 5<<30 {
			preserved = true
		}
	}
	if !preserved {
		t.Error("the ephemeral volume's contents were not preserved")
	}
}

// cutShortRetention brings VM 777 to the state a legacy retention leaves
// when its parker lock times out. The first delete_vm records the ephemeral
// volume's CID under its bare volid, then times out on the parker lock and
// returns an error, so the VM keeps the volume on scsi1 with the record in
// place. Both the block and the file-backed form get there for real.
func cutShortRetention(t *testing.T, deps Deps, client *retainRerunPVE, volume string, fastPath bool) {
	t.Helper()
	if err := deleteVMCall(t, lockTimeoutContext(t), deps, "777"); err == nil {
		t.Fatal("the first delete must fail when its parker lock times out")
	}
	assertCutShortRetention(t, client, volume)
	if fastPath {
		if tags, _ := pve.ConfigString(client.state.configs[777], jsonKeyTags); !strings.Contains(tags, tagDeletingVM) {
			t.Fatalf("the fast path must have queued the VM for the sweep; tags %q", tags)
		}
	}
}

var retainRerunVolumes = map[string]string{
	"block":       "a:vm-777-ephemeral-0",
	"file-backed": "a:777/vm-777-ephemeral-0.raw",
}

// lockTimeoutContext makes every cluster lock the request takes time out at
// once, with no wait and before it creates anything. Its deadline leaves less
// than the margin an acquire keeps for its release, so the acquire gives up
// up front. A parker lock that times out this way leaves exactly what a lock
// held by another request until the deadline leaves.
func lockTimeoutContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestDeleteVM_RetentionRerunAfterCutShortTransfer covers delete_vm reruns
// after a legacy retention recorded the ephemeral volume's CID and then had
// its parker lock time out, which leaves the VM holding its own ephemeral
// volume with a bosh_attached_disks record naming it. The owned legacy disk
// refusal must not mistake that volume for a persistent disk, or the VM
// could never be deleted.
func TestDeleteVM_RetentionRerunAfterCutShortTransfer(t *testing.T) {
	for form, volume := range retainRerunVolumes {
		for _, fastPath := range []bool{false, true} {
			name := form + "/sync"
			if fastPath {
				name = form + "/fast"
			}
			t.Run(name, func(t *testing.T) {
				deps, client := retainRerunFixture(t, volume, fastPath)
				cutShortRetention(t, deps, client, volume, fastPath)

				if err := deleteVMCall(t, context.Background(), deps, "777"); err != nil {
					t.Fatalf("the rerun must finish the retention and delete the VM: %v", err)
				}
				assertRetentionFinished(t, client, volume)
			})
		}
	}
}

// TestDeleteVM_StragglerSweepFinishesCutShortRetention covers the third path:
// the first fast-path delete stamps bosh-deleting and then has its retention
// cut short, and a later fast-path delete of another VM sweeps it up.
func TestDeleteVM_StragglerSweepFinishesCutShortRetention(t *testing.T) {
	for form, volume := range retainRerunVolumes {
		t.Run(form, func(t *testing.T) {
			deps, client := retainRerunFixture(t, volume, true)
			client.state.configs[778] = map[string]any{"name": "other", "digest": "1"}
			cutShortRetention(t, deps, client, volume, true)

			if err := deleteVMCall(t, context.Background(), deps, "778"); err != nil {
				t.Fatalf("the later delete must succeed: %v", err)
			}
			if !client.destroyed[778] {
				t.Error("the later delete must destroy its own VM")
			}
			assertRetentionFinished(t, client, volume)
		})
	}
}
