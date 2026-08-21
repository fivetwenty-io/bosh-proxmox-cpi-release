// disk_migrate_internal_test.go — white-box tests for the A2 mover
// primitives: the mover tag check, DestroyEmptyMover's fail-closed guards,
// and MigrateDiskViaMover's validation and re-entry windows. The end-to-end
// mover flow (isolation, migration, final attach) is exercised at the
// handler level in internal/cpi/handlers/attach_disk_migrate_internal_test.go.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

const dmToken = "bpd-feedface00112233"

// dmFakeClient is a node-aware two-service fake: config reads addressed to a
// node the VM is not on answer 404, matching the probe MigrateDiskViaMover
// uses to tell "still on the source" from "already migrated".
type dmFakeClient struct {
	parkerLockClient

	mu         sync.Mutex
	configs    map[int]map[string]any
	nodes      map[int]string
	deletedVMs []int
	protWrites []bool
}

func newDMFakeClient(configs map[int]map[string]any, nodes map[int]string) *dmFakeClient {
	return &dmFakeClient{configs: configs, nodes: nodes}
}

func (c *dmFakeClient) QEMU() qemu.Service      { return &dmFakeQEMU{c: c} }
func (c *dmFakeClient) Nodes() sdknodes.Service { return &dmFakeNodes{c: c} }

type dmFakeQEMU struct {
	qemu.Service
	c *dmFakeClient
}

func (q *dmFakeQEMU) Config(_ context.Context, node string, vmid int) (map[string]any, error) {
	q.c.mu.Lock()
	defer q.c.mu.Unlock()
	cfg, ok := q.c.configs[vmid]
	if !ok || q.c.nodes[vmid] != node {
		return nil, fmt.Errorf("dmFake: no config for vmid %d on node %s: %w", vmid, node, sdkerrors.ErrNotFound)
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out, nil
}

type dmFakeNodes struct {
	sdknodes.Service
	c *dmFakeClient
}

func (n *dmFakeNodes) UpdateQemuConfig(_ context.Context, node string, vmidStr string, params *sdknodes.UpdateQemuConfigParams) error {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	vmid, _ := strconv.Atoi(vmidStr)
	cfg, ok := n.c.configs[vmid]
	if !ok || n.c.nodes[vmid] != node {
		return fmt.Errorf("dmFake: no config for vmid %s on node %s: %w", vmidStr, node, sdkerrors.ErrNotFound)
	}
	if params.Protection != nil {
		cfg[paramProtection] = *params.Protection
		n.c.protWrites = append(n.c.protWrites, *params.Protection)
	}
	if params.Description != nil {
		cfg["description"] = *params.Description
	}
	return nil
}

func (n *dmFakeNodes) DeleteQemu(_ context.Context, node string, vmidStr string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	vmid, _ := strconv.Atoi(vmidStr)
	if _, ok := n.c.configs[vmid]; !ok || n.c.nodes[vmid] != node {
		return nil, fmt.Errorf("dmFake: no VM %d on node %s: %w", vmid, node, sdkerrors.ErrNotFound)
	}
	delete(n.c.configs, vmid)
	delete(n.c.nodes, vmid)
	n.c.deletedVMs = append(n.c.deletedVMs, vmid)
	resp := json.RawMessage(`""`)
	return &resp, nil
}

func dmMoverHolder(vmid int) DiskHolder {
	return DiskHolder{
		Found: true, VMID: vmid, Node: "pve1", IsParker: true, Slot: "scsi0",
		Tags: CpiOwnershipTag + ";" + ParkerTag + ";" + DiskMoverTag,
	}
}

func dmBand() ParkerConfig {
	return ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999, DiskStorage: "data"}
}

func TestTagsMarkDiskMover(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tags string
		want bool
	}{
		{"", false},
		{"bosh-cpi;bosh-parker", false},
		{"bosh-cpi;bosh-parker;bosh-disk-mover", true},
		{"BOSH-DISK-MOVER", true},
		{"bosh-disk-mover-extra", false},
		{"bosh-cpi,bosh-disk-mover", true},
		{"bosh-cpi bosh-disk-mover", true},
	}
	for _, tc := range cases {
		if got := TagsMarkDiskMover(tc.tags); got != tc.want {
			t.Errorf("TagsMarkDiskMover(%q) = %v, want %v", tc.tags, got, tc.want)
		}
	}
}

func TestMigrateDiskViaMover_Validation(t *testing.T) {
	t.Parallel()
	c := newDMFakeClient(nil, nil)
	holder := DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true}

	t.Run("legacy disk refused", func(t *testing.T) {
		t.Parallel()
		_, _, err := MigrateDiskViaMover(context.Background(), c, nil, DiskMigrationSpec{
			Holder: holder, TargetNode: "pve2", Volid: "data:vm-9001-disk-0",
		}, dmBand())
		if err == nil || !strings.Contains(err.Error(), "stable ID") {
			t.Fatalf("err = %v, want the stable-ID requirement", err)
		}
	})

	t.Run("same-node holder refused", func(t *testing.T) {
		t.Parallel()
		_, _, err := MigrateDiskViaMover(context.Background(), c, nil, DiskMigrationSpec{
			Holder: holder, TargetNode: "pve1", Volid: "data:vm-9001-disk-0", StableID: dmToken,
		}, dmBand())
		if err == nil || !strings.Contains(err.Error(), "same-node") {
			t.Fatalf("err = %v, want the same-node rejection", err)
		}
	})

	t.Run("non-parker holder refused", func(t *testing.T) {
		t.Parallel()
		_, _, err := MigrateDiskViaMover(context.Background(), c, nil, DiskMigrationSpec{
			Holder: DiskHolder{Found: true, VMID: 700, Node: "pve1"}, TargetNode: "pve2",
			Volid: "data:vm-9001-disk-0", StableID: dmToken,
		}, dmBand())
		if err == nil || !strings.Contains(err.Error(), "not a parker") {
			t.Fatalf("err = %v, want the non-parker rejection", err)
		}
	})
}

// TestMigrateDiskViaMover_MidMigrationLockIsRetriable covers the exhausted
// await budget window: the retried attach finds the adopted mover still on
// the source node under PVE's migrate lock and must get a retriable error,
// never a second migrate request or a permanent failure.
func TestMigrateDiskViaMover_MidMigrationLockIsRetriable(t *testing.T) {
	t.Parallel()
	c := newDMFakeClient(map[int]map[string]any{
		90007: {
			"tags":  "bosh-cpi;bosh-parker;bosh-disk-mover",
			"lock":  "migrate",
			"scsi0": "data:vm-90007-disk-0,serial=" + dmToken,
		},
	}, map[int]string{90007: "pve1"})

	_, _, err := MigrateDiskViaMover(context.Background(), c, nil, DiskMigrationSpec{
		Holder: dmMoverHolder(90007), TargetNode: "pve2",
		Volid: "data:vm-90007-disk-0", StableID: dmToken,
	}, dmBand())
	if err == nil || !strings.Contains(err.Error(), "mid-migration") {
		t.Fatalf("err = %v, want the mid-migration deferral", err)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("err = %v, want a retriable class", err)
	}
}

// TestMigrateDiskViaMover_ConvergesWhenAlreadyMigrated covers the window
// where a previous run's migration completed after its await budget ran out:
// the mover's config is gone from the source node and present on the target.
// The flow must converge without a new migrate call — protection re-asserted,
// slot and renamed volid re-derived from the serial, provenance rewritten for
// the new node.
func TestMigrateDiskViaMover_ConvergesWhenAlreadyMigrated(t *testing.T) {
	t.Parallel()
	c := newDMFakeClient(map[int]map[string]any{
		90007: {
			"tags":          "bosh-cpi;bosh-parker;bosh-disk-mover",
			paramProtection: false, // the interrupted run dropped it for the migrate
			"scsi2":         "data:vm-90007-disk-3,serial=" + dmToken + ",size=10G",
		},
	}, map[int]string{90007: "pve2"})

	mover, landed, err := MigrateDiskViaMover(context.Background(), c, nil, DiskMigrationSpec{
		Holder: dmMoverHolder(90007), TargetNode: "pve2",
		Volid: "data:vm-90007-disk-0", StableID: dmToken, DiskCID: "pvd-x",
	}, dmBand())
	if err != nil {
		t.Fatalf("MigrateDiskViaMover: %v", err)
	}
	if mover.VMID != 90007 || mover.Node != "pve2" || mover.Slot != "scsi2" {
		t.Errorf("mover = %+v, want vmid 90007 on pve2 slot scsi2", mover)
	}
	if landed != "data:vm-90007-disk-3" {
		t.Errorf("landed = %q, want the serial-located volid", landed)
	}
	if prot, _ := c.configs[90007][paramProtection].(bool); !prot {
		t.Error("protection not re-asserted on the migrated mover")
	}
	// The provenance record was rewritten for the new node and volid.
	desc, _ := c.configs[90007]["description"].(string)
	if !strings.Contains(desc, "data:vm-90007-disk-3") || !strings.Contains(desc, "pve2") {
		t.Errorf("provenance description %q does not carry the renamed volid and new node", desc)
	}
}

// TestMigrateDiskViaMover_SerialMissingAfterMigration: a migrated mover whose
// drive entries carry no matching serial cannot be converged automatically —
// a permanent, human-readable error, never a silent guess at a volid.
func TestMigrateDiskViaMover_SerialMissingAfterMigration(t *testing.T) {
	t.Parallel()
	c := newDMFakeClient(map[int]map[string]any{
		90007: {
			"tags":  "bosh-cpi;bosh-parker;bosh-disk-mover",
			"scsi0": "data:vm-90007-disk-0", // serial lost
		},
	}, map[int]string{90007: "pve2"})

	_, _, err := MigrateDiskViaMover(context.Background(), c, nil, DiskMigrationSpec{
		Holder: dmMoverHolder(90007), TargetNode: "pve2",
		Volid: "data:vm-90007-disk-0", StableID: dmToken,
	}, dmBand())
	if err == nil || !strings.Contains(err.Error(), "no drive entry carries serial") {
		t.Fatalf("err = %v, want the serial-not-found refusal", err)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.OkToRetry() {
		t.Fatalf("err = %v, want a permanent class", err)
	}
}

func TestDestroyEmptyMover_Guards(t *testing.T) {
	t.Parallel()

	t.Run("refuses a holder without the mover tag", func(t *testing.T) {
		t.Parallel()
		c := newDMFakeClient(nil, nil)
		holder := DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Tags: "bosh-cpi;bosh-parker"}
		err := DestroyEmptyMover(context.Background(), c, nil, holder)
		if err == nil || !strings.Contains(err.Error(), "durable parker") {
			t.Fatalf("err = %v, want the durable-parker refusal", err)
		}
	})

	t.Run("refuses when the config's own tags lack the mover tag", func(t *testing.T) {
		t.Parallel()
		// The caller believed 90000 was a mover; the config says otherwise.
		c := newDMFakeClient(map[int]map[string]any{
			90000: {"tags": "bosh-cpi;bosh-parker"},
		}, map[int]string{90000: "pve1"})
		err := DestroyEmptyMover(context.Background(), c, nil, dmMoverHolder(90000))
		if err == nil || !strings.Contains(err.Error(), "do not mark a migration mover") {
			t.Fatalf("err = %v, want the config-tag refusal", err)
		}
		if len(c.deletedVMs) != 0 {
			t.Errorf("deletedVMs = %v, want none", c.deletedVMs)
		}
	})

	t.Run("refuses a mover still holding a disk", func(t *testing.T) {
		t.Parallel()
		c := newDMFakeClient(map[int]map[string]any{
			90007: {
				"tags":  "bosh-cpi;bosh-parker;bosh-disk-mover",
				"scsi0": "data:vm-90007-disk-0,serial=" + dmToken,
			},
		}, map[int]string{90007: "pve1"})
		err := DestroyEmptyMover(context.Background(), c, nil, dmMoverHolder(90007))
		if err == nil || !strings.Contains(err.Error(), "still references volumes") {
			t.Fatalf("err = %v, want the still-references refusal", err)
		}
		if len(c.deletedVMs) != 0 {
			t.Errorf("deletedVMs = %v, want none", c.deletedVMs)
		}
	})

	t.Run("refuses a mover with an unused entry", func(t *testing.T) {
		t.Parallel()
		c := newDMFakeClient(map[int]map[string]any{
			90007: {
				"tags":    "bosh-cpi;bosh-parker;bosh-disk-mover",
				"unused0": "data:vm-90007-disk-0",
			},
		}, map[int]string{90007: "pve1"})
		err := DestroyEmptyMover(context.Background(), c, nil, dmMoverHolder(90007))
		if err == nil || !strings.Contains(err.Error(), "unused entries") {
			t.Fatalf("err = %v, want the unused-entries refusal", err)
		}
	})

	t.Run("defers a mover under a destructive lock", func(t *testing.T) {
		t.Parallel()
		c := newDMFakeClient(map[int]map[string]any{
			90007: {
				"tags": "bosh-cpi;bosh-parker;bosh-disk-mover",
				"lock": "migrate",
			},
		}, map[int]string{90007: "pve1"})
		err := DestroyEmptyMover(context.Background(), c, nil, dmMoverHolder(90007))
		if err == nil {
			t.Fatal("err = nil, want a retriable deferral")
		}
		var typed *cpierrors.Error
		if !errors.As(err, &typed) || !typed.OkToRetry() {
			t.Fatalf("err = %v, want a retriable class", err)
		}
	})

	t.Run("gone mover is idempotent success", func(t *testing.T) {
		t.Parallel()
		c := newDMFakeClient(map[int]map[string]any{}, map[int]string{})
		if err := DestroyEmptyMover(context.Background(), c, nil, dmMoverHolder(90007)); err != nil {
			t.Fatalf("err = %v, want nil for a mover that is already gone", err)
		}
	})

	t.Run("destroys an empty mover, clearing protection first", func(t *testing.T) {
		t.Parallel()
		c := newDMFakeClient(map[int]map[string]any{
			90007: {
				"tags":          "bosh-cpi;bosh-parker;bosh-disk-mover",
				paramProtection: true,
			},
		}, map[int]string{90007: "pve1"})
		if err := DestroyEmptyMover(context.Background(), c, nil, dmMoverHolder(90007)); err != nil {
			t.Fatalf("DestroyEmptyMover: %v", err)
		}
		if len(c.deletedVMs) != 1 || c.deletedVMs[0] != 90007 {
			t.Errorf("deletedVMs = %v, want [90007]", c.deletedVMs)
		}
		if len(c.protWrites) != 1 || c.protWrites[0] {
			t.Errorf("protection writes = %v, want one clear before the delete", c.protWrites)
		}
	})
}
