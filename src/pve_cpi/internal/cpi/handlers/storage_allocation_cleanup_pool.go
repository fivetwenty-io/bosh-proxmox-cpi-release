package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

var cleanupSharedPoolName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// A pool is shared infrastructure, not an owned VM artifact. This permits only
// terminal disposition of a generation that stopped before any VM/storage write;
// it neither settles the original pool operation nor deletes the pool.
func cleanupOnlySharedPool(record aj.Record) (string, bool) {
	if record.Kind != "vm" || record.CID != "" || len(record.Steps) != 1 || len(record.Attempts) > 1 || record.ActiveAttempt() != 0 {
		return "", false
	}
	if len(record.Attempts) == 1 && record.Attempts[0].Completion != nil {
		return "", false
	}
	step := record.Steps[0]
	if step.Attempt != 0 || step.State != aj.Planned || step.Kind != "vm.Pool.CreatePool" || step.UPID != "" || len(step.VolIDs) != 0 || len(step.Charges) != 0 || len(step.Parameters) != 0 || step.Target.VMID <= 0 || step.Target.Node == "" {
		return "", false
	}
	if step.Target != (aj.Target{Node: step.Target.Node, VMID: step.Target.VMID}) {
		return "", false
	}
	plan, err := activeStorageAllocationPlan(record)
	if err != nil || plan.VMExecution == nil || plan.VMExecution.Version != 1 || plan.Node != step.Target.Node || plan.VMExecution.Node != step.Target.Node {
		return "", false
	}
	pool := plan.VMExecution.Pool
	if !cleanupSharedPoolName.MatchString(pool) || strings.HasPrefix(pool, "bosh-lock-") {
		return "", false
	}
	for i := range plan.Charges {
		charge := &plan.Charges[i]
		if charge.Submitted || charge.Acquired || charge.Reflected || charge.Retained || charge.VolumeID != "" {
			return "", false
		}
	}
	return pool, true
}

func observeCleanupPoolOnlyAbsence(ctx context.Context, deps Deps, record aj.Record, settlement *cleanupSettlement) (aj.Verification, error) {
	pool, ok := cleanupOnlySharedPool(record)
	if !ok || pool != settlement.SharedPoolOnly {
		return aj.Verification{}, fmt.Errorf("shared pool cleanup scope changed")
	}
	return observePlannedVMAbsence(ctx, deps, record, settlement.TaskObservation.Nodes, record.Steps[0].Target.VMID)
}

func observePlannedVMAbsence(ctx context.Context, deps Deps, record aj.Record, nodes []string, vmid int, allowed ...string) (aj.Verification, error) {
	if err := observePoolOnlyGuestsAbsent(ctx, deps, nodes, vmid); err != nil {
		return aj.Verification{}, err
	}
	if err := observePlannedVMStorageAbsence(ctx, deps, record, nodes, vmid, allowed...); err != nil {
		return aj.Verification{}, err
	}
	return aj.Verification{Complete: true, AbsenceVerified: true, VMAbsenceVerified: true, ArtifactDispositionVerified: true}, nil
}

func observePlannedVMStorageAbsence(ctx context.Context, deps Deps, record aj.Record, nodes []string, vmid int, allowed ...string) error {
	// Scan all applicable current and historical storage locations, including
	// unmarked plain VM files, before releasing this numeric VM reservation.
	report := StorageAllocationAudit{Complete: true}
	stores := auditStorageDefinitions(ctx, deps, []aj.Record{record}, &report)
	_, historical, _ := storageAuditRecordIndex([]aj.Record{record})
	targets := storageAuditTargets(ctx, deps, nodes, historical, stores, &report)
	if !report.Complete || len(report.Issues) != 0 {
		return fmt.Errorf("pool-only cleanup storage scope unavailable")
	}
	for _, target := range targets {
		if err := observePoolOnlyStorageAbsent(ctx, deps, target, vmid, allowed...); err != nil {
			return err
		}
	}
	return nil
}

func observePoolOnlyStorageAbsent(ctx context.Context, deps Deps, target storageAuditTarget, vmid int, allowed ...string) error {
	listing, err := deps.PVE.Nodes().ListStorageContent(ctx, target.node, target.storage, nil)
	if err != nil || listing == nil || *listing == nil {
		return fmt.Errorf("pool-only cleanup content unavailable")
	}
	seen := map[string]bool{}
	for _, raw := range *listing {
		var item struct {
			VolID string `json:"volid"`
			VMID  int    `json:"vmid"`
		}
		if json.Unmarshal(raw, &item) != nil || item.VolID == "" || seen[item.VolID] {
			return fmt.Errorf("pool-only cleanup content malformed")
		}
		storage, bare, err := pve.ParseDiskCID(item.VolID)
		if err != nil || storage != target.storage {
			return fmt.Errorf("pool-only cleanup content target differs")
		}
		seen[item.VolID] = true
		if len(allowed) == 1 && item.VolID == allowed[0] {
			continue
		}
		owner, _ := pve.EmbeddedDiskVMID(item.VolID)
		name := bare[strings.LastIndex(bare, "/")+1:]
		if item.VMID == vmid || owner == vmid || strings.HasPrefix(bare, strconv.Itoa(vmid)+"/") || strings.HasPrefix(name, "vm-"+strconv.Itoa(vmid)+"-") {
			return fmt.Errorf("pool-only cleanup found artifact for planned VM identity")
		}
	}
	return nil
}

func observePoolOnlyGuestsAbsent(ctx context.Context, deps Deps, nodes []string, vmid int) error {
	seen := map[int]bool{}
	for _, node := range nodes {
		qemu, err := deps.PVE.Nodes().ListQemu(ctx, node, nil)
		if err != nil || qemu == nil || *qemu == nil {
			return fmt.Errorf("pool-only cleanup QEMU inventory unavailable")
		}
		if err := validatePoolOnlyGuestRows(*qemu, vmid, seen); err != nil {
			return err
		}
		lxc, err := deps.PVE.Nodes().ListLxc(ctx, node)
		if err != nil || lxc == nil || *lxc == nil {
			return fmt.Errorf("pool-only cleanup LXC inventory unavailable")
		}
		if err := validatePoolOnlyGuestRows(*lxc, vmid, seen); err != nil {
			return err
		}
	}
	return nil
}
func validatePoolOnlyGuestRows(rows []json.RawMessage, vmid int, seen map[int]bool) error {
	for _, raw := range rows {
		var item struct {
			VMID json.Number `json:"vmid"`
		}
		if json.Unmarshal(raw, &item) != nil {
			return fmt.Errorf("pool-only cleanup guest identity malformed")
		}
		id, err := strconv.Atoi(string(item.VMID))
		if err != nil || id <= 0 || strconv.Itoa(id) != string(item.VMID) || seen[id] {
			return fmt.Errorf("pool-only cleanup guest identity missing or duplicated")
		}
		if id == vmid {
			return fmt.Errorf("pool-only cleanup found planned guest identity")
		}
		seen[id] = true
	}
	return nil
}
