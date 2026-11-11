package pve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// mmMoverVMID is the mover each test here migrates from mmSource to mmTarget.
const (
	mmMoverVMID = 90000
	mmSource    = "pve1"
	mmTarget    = "pve2"
	mmVolid     = "shared:vm-90000-disk-0"
)

// mmProtectionWrite is one protection write the fake accepted.
type mmProtectionWrite struct {
	node string
	on   bool
}

// mmClient is a two-node fake that answers the way real PVE does. A config
// read or write addressed to a node the VM isn't on fails with the 500 that
// says the configuration file does not exist, and the cluster serves both
// members to the config probe. migrateFn decides what the migrate call does to
// the fake's state and what it returns, and waitErr is the migrate task's
// outcome. readErr fails every config read addressed to a node until a
// protection-on write lands there, which models a node that stops answering
// reads and is answering again by the time the restore reads back.
type mmClient struct {
	parkerLockClient

	mu        sync.Mutex
	configs   map[int]map[string]any
	nodes     map[int]string
	migrateFn func(c *mmClient) (*sdknodes.CreateQemuMigrateResponse, error)
	waitErr   error
	readErr   map[string]error
	writes    []mmProtectionWrite
	// attempts records every protection write the fake received, in order,
	// whether it applied or PVE refused it.
	attempts []mmProtectionWrite
}

func newMMClient() *mmClient {
	return &mmClient{
		configs: map[int]map[string]any{mmMoverVMID: {
			"name": parkerVMName("", mmMoverVMID), "protection": true,
			"tags":  CpiOwnershipTag + ";" + ParkerTag + ";" + DiskMoverTag,
			"scsi0": mmVolid + ",serial=" + dmToken + ",size=1G",
		}},
		nodes: map[int]string{mmMoverVMID: mmSource},
	}
}

// mmConfigMissing is PVE's answer for a VM whose config isn't on node.
func mmConfigMissing(node string, vmid int) error {
	body, _ := json.Marshal(map[string]any{
		"data":    nil,
		"message": fmt.Sprintf("Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist\n", node, vmid),
	})
	return sdkerrors.ParseAPIError(500, body)
}

// moveTo puts the mover's config on node, or removes it from every node when
// node is empty. The caller holds c.mu.
func (c *mmClient) moveTo(node string) {
	if node == "" {
		delete(c.configs, mmMoverVMID)
		delete(c.nodes, mmMoverVMID)
		return
	}
	c.nodes[mmMoverVMID] = node
}

func (c *mmClient) QEMU() qemu.Service          { return &mmQEMU{c: c} }
func (c *mmClient) Nodes() sdknodes.Service     { return &mmNodes{c: c} }
func (c *mmClient) Cluster() sdkcluster.Service { return &mmCluster{} }
func (c *mmClient) Tasks() sdktasks.Service     { return &mmTasks{c: c} }

type mmQEMU struct {
	qemu.Service
	c *mmClient
}

func (q *mmQEMU) Config(_ context.Context, node string, vmid int) (map[string]any, error) {
	q.c.mu.Lock()
	defer q.c.mu.Unlock()
	if err := q.c.readErr[node]; err != nil {
		return nil, err
	}
	cfg, ok := q.c.configs[vmid]
	if !ok || q.c.nodes[vmid] != node {
		return nil, mmConfigMissing(node, vmid)
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return withAnsweredDigest(out), nil
}

type mmNodes struct {
	sdknodes.Service
	c *mmClient
}

func (n *mmNodes) UpdateQemuConfig(
	_ context.Context, node string, vmidStr string, params *sdknodes.UpdateQemuConfigParams,
) error {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	vmid, _ := strconv.Atoi(vmidStr)
	if params.Protection != nil {
		n.c.attempts = append(n.c.attempts, mmProtectionWrite{node: node, on: *params.Protection})
	}
	cfg, ok := n.c.configs[vmid]
	if !ok || n.c.nodes[vmid] != node {
		return mmConfigMissing(node, vmid)
	}
	if params.Protection != nil {
		cfg[paramProtection] = *params.Protection
		n.c.writes = append(n.c.writes, mmProtectionWrite{node: node, on: *params.Protection})
		if *params.Protection {
			delete(n.c.readErr, node)
		}
	}
	return nil
}

func (n *mmNodes) CreateQemuMigrate(
	_ context.Context, _ string, _ string, _ *sdknodes.CreateQemuMigrateParams,
) (*sdknodes.CreateQemuMigrateResponse, error) {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	return n.c.migrateFn(n.c)
}

type mmTasks struct {
	sdktasks.Service
	c *mmClient
}

func (t *mmTasks) Wait(_ context.Context, _, upid string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.c.waitErr != nil {
		return nil, t.c.waitErr
	}
	return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
}

func (t *mmTasks) GetStatus(_ context.Context, _, upid string) (*sdktasks.Status, error) {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.c.waitErr != nil {
		return &sdktasks.Status{Status: "stopped", ExitStatus: t.c.waitErr.Error(), UpID: upid}, nil
	}
	return &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
}

// mmCluster serves both members, online, with no HA resources.
type mmCluster struct{ sdkcluster.Service }

func (mmCluster) ListConfigNodes(context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	resp := make(sdkcluster.ListConfigNodesResponse, 0, 2)
	for _, node := range []string{mmSource, mmTarget} {
		raw, _ := json.Marshal(map[string]any{"name": node})
		resp = append(resp, raw)
	}
	return &resp, nil
}

func (mmCluster) ListStatus(context.Context) (*sdkcluster.ListStatusResponse, error) {
	resp := sdkcluster.ListStatusResponse{}
	return &resp, nil
}

func (mmCluster) ListHaResources(
	context.Context, *sdkcluster.ListHaResourcesParams,
) (*sdkcluster.ListHaResourcesResponse, error) {
	resp := sdkcluster.ListHaResourcesResponse{}
	return &resp, nil
}

func (mmCluster) ListHaStatusManagerStatus(context.Context) (*sdkcluster.ListHaStatusManagerStatusResponse, error) {
	resp := sdkcluster.ListHaStatusManagerStatusResponse{}
	return &resp, nil
}

// mmMigrate runs migrateMoverToNode against c and returns what it logged and
// its error.
func mmMigrate(t *testing.T, c *mmClient) (string, error) {
	t.Helper()
	var logged bytes.Buffer
	logger, err := log.NewLogger("debug", &logged)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	mover := DiskHolder{
		Found: true, VMID: mmMoverVMID, Node: mmSource, IsParker: true, Slot: "scsi0",
		Tags: CpiOwnershipTag + ";" + ParkerTag + ";" + DiskMoverTag,
	}
	spec := DiskMigrationSpec{
		Holder: mover, TargetNode: mmTarget, Volid: mmVolid, StableID: dmToken,
		MaxAttempts: 1, AwaitBudget: time.Second,
	}
	_, _, migErr := migrateMoverToNode(ctx, c, logger, mover, mmVolid, spec, dmBand(), ParkContext{})
	return logged.String(), migErr
}

// mmUPID is the migrate task's UPID on the source node.
func mmUPID() *sdknodes.CreateQemuMigrateResponse {
	resp := sdknodes.CreateQemuMigrateResponse(strconv.Quote(
		"UPID:" + mmSource + ":0000AB12:00512345:65D0AA11:qmigrate:" + strconv.Itoa(mmMoverVMID) + ":root@pam:"))
	return &resp
}

// mmWarnings returns the WARN lines in logged that mention a protection
// restore.
func mmWarnings(logged string) []string {
	var lines []string
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, `"level":"WARN"`) && strings.Contains(line, "protection") {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestMigrateMoverToNode_RestoresProtectionWhereTheMoverLanded fails the
// migrate call after PVE already moved the mover to the target, the way a
// response lost after the migration ran does. The restore has to find the
// mover on the target and put its protection back there.
func TestMigrateMoverToNode_RestoresProtectionWhereTheMoverLanded(t *testing.T) {
	c := newMMClient()
	c.migrateFn = func(c *mmClient) (*sdknodes.CreateQemuMigrateResponse, error) {
		c.moveTo(mmTarget)
		return nil, errors.New("migration response lost")
	}
	logged, migErr := mmMigrate(t, c)
	if migErr == nil || !strings.Contains(migErr.Error(), "migrate mover vmid") {
		t.Fatalf("migrateMoverToNode = %v, want the failed migrate call", migErr)
	}
	want := []mmProtectionWrite{{node: mmSource, on: false}, {node: mmTarget, on: true}}
	if len(c.writes) != len(want) || c.writes[0] != want[0] || c.writes[1] != want[1] {
		t.Fatalf("protection writes = %+v, want %+v", c.writes, want)
	}
	if !ConfigFlag(c.configs[mmMoverVMID], paramProtection) {
		t.Fatal("the mover's protection is off after the restore")
	}
	if lines := mmWarnings(logged); len(lines) != 0 {
		t.Fatalf("the restore warned: %v", lines)
	}
}

// mmReadTimeout is a 500 that doesn't say the config is gone.
var mmReadTimeout = &sdkerrors.APIError{HTTPCode: 500, Message: "got timeout"}

// TestMigrateMoverToNode_FallsBackWhenNoReadPlacesTheMover fails the migrate
// call in ways that leave no config read able to place the mover. The restore
// then tries the source node, moves on to the target only when the source
// answers that the config is missing, and stops at the first write whose
// readback shows protection on.
func TestMigrateMoverToNode_FallsBackWhenNoReadPlacesTheMover(t *testing.T) {
	for _, tc := range []struct {
		name    string
		migrate func(c *mmClient) (*sdknodes.CreateQemuMigrateResponse, error)
		want    mmProtectionWrite
	}{
		// The mover stayed on the source, which can't answer the locate read.
		{"source read fails", func(c *mmClient) (*sdknodes.CreateQemuMigrateResponse, error) {
			c.readErr = map[string]error{mmSource: mmReadTimeout}
			return nil, errors.New("migration response lost")
		}, mmProtectionWrite{node: mmSource, on: true}},
		// The mover reached the target, which can't answer the probe, so the
		// source write answers config-missing and the target write succeeds.
		{"source answers config-missing", func(c *mmClient) (*sdknodes.CreateQemuMigrateResponse, error) {
			c.moveTo(mmTarget)
			c.readErr = map[string]error{mmTarget: mmReadTimeout}
			return nil, errors.New("migration response lost")
		}, mmProtectionWrite{node: mmTarget, on: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newMMClient()
			c.migrateFn = tc.migrate
			logged, migErr := mmMigrate(t, c)
			if migErr == nil || !strings.Contains(migErr.Error(), "migrate mover vmid") {
				t.Fatalf("migrateMoverToNode = %v, want the failed migrate call", migErr)
			}
			want := []mmProtectionWrite{{node: mmSource, on: false}, tc.want}
			if len(c.writes) != len(want) || c.writes[0] != want[0] || c.writes[1] != want[1] {
				t.Fatalf("protection writes = %+v, want %+v", c.writes, want)
			}
			// A write that answers config-missing is PVE's verdict, so the
			// restore sends it once and moves on without a retry.
			attempts := []mmProtectionWrite{{node: mmSource, on: false}, {node: mmSource, on: true}}
			if tc.want.node == mmTarget {
				attempts = append(attempts, tc.want)
			}
			if !slices.Equal(c.attempts, attempts) {
				t.Fatalf("protection write attempts = %+v, want %+v", c.attempts, attempts)
			}
			if !ConfigFlag(c.configs[mmMoverVMID], paramProtection) {
				t.Fatal("the mover's protection is off after the restore")
			}
			if lines := mmWarnings(logged); len(lines) != 0 {
				t.Fatalf("the restore warned: %v", lines)
			}
		})
	}
}

// TestMigrateMoverToNode_WarnsWhenTheMoverIsOnNeitherNode fails the migrate
// call and leaves the mover's config on no member. Both restore writes answer
// that the config is missing, so nothing lands, and the WARN names the nodes
// the restore tried and the command that puts protection back.
func TestMigrateMoverToNode_WarnsWhenTheMoverIsOnNeitherNode(t *testing.T) {
	c := newMMClient()
	c.migrateFn = func(c *mmClient) (*sdknodes.CreateQemuMigrateResponse, error) {
		c.moveTo("")
		return nil, errors.New("migration response lost")
	}
	logged, migErr := mmMigrate(t, c)
	if migErr == nil || !strings.Contains(migErr.Error(), "migrate mover vmid") {
		t.Fatalf("migrateMoverToNode = %v, want the failed migrate call", migErr)
	}
	// The protection-off write before the migration is the only one that
	// landed.
	if len(c.writes) != 1 || c.writes[0].on {
		t.Fatalf("protection writes = %+v, want only the write that cleared it", c.writes)
	}
	// Each restore write answers config-missing once and isn't retried.
	attempts := []mmProtectionWrite{{node: mmSource, on: false}, {node: mmSource, on: true}, {node: mmTarget, on: true}}
	if !slices.Equal(c.attempts, attempts) {
		t.Fatalf("protection write attempts = %+v, want %+v", c.attempts, attempts)
	}
	lines := mmWarnings(logged)
	if len(lines) != 1 {
		t.Fatalf("logged %d protection warnings, want one: %s", len(lines), logged)
	}
	for _, want := range []string{
		"protection restore on mover 90000 did not succeed on node(s) " + mmSource + ", " + mmTarget,
		"qm set 90000 --protection 1",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the warning lacks %q: %s", want, lines[0])
		}
	}
}

// TestMigrateMoverToNode_RestoresProtectionOnTheSourceAfterAFailedTask runs a
// migrate task that fails, which leaves the mover on the source node. The
// restore finds it there and puts its protection back on the source.
func TestMigrateMoverToNode_RestoresProtectionOnTheSourceAfterAFailedTask(t *testing.T) {
	c := newMMClient()
	c.migrateFn = func(*mmClient) (*sdknodes.CreateQemuMigrateResponse, error) {
		return mmUPID(), nil
	}
	c.waitErr = errors.New("task failed: migration aborted")
	logged, migErr := mmMigrate(t, c)
	if migErr == nil || !strings.Contains(migErr.Error(), "migrate task for mover") {
		t.Fatalf("migrateMoverToNode = %v, want the failed migrate task", migErr)
	}
	want := []mmProtectionWrite{{node: mmSource, on: false}, {node: mmSource, on: true}}
	if len(c.writes) != len(want) || c.writes[0] != want[0] || c.writes[1] != want[1] {
		t.Fatalf("protection writes = %+v, want %+v", c.writes, want)
	}
	if lines := mmWarnings(logged); len(lines) != 0 {
		t.Fatalf("the restore warned: %v", lines)
	}
}
