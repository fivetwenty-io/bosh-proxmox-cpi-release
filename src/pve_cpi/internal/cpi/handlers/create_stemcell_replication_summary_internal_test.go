package handlers

import (
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// TestLogReplicationSummaryAllSucceeded verifies the all-green shape: exactly
// one info line, no failed fields.
func TestLogReplicationSummaryAllSucceeded(t *testing.T) {
	t.Parallel()
	logger, obs := log.NewObservedLogger(log.LevelInfo)

	logReplicationSummary(logger, "create_stemcell: replication", []replicaOutcome{
		{Node: "pve2", Stage: "replicated"},
		{Node: "pve3", Stage: "already-exists"},
		{Node: "pve4", Stage: "adopted"},
	})

	entries := obs.All()
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 summary entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Level != log.LevelInfo {
		t.Fatalf("expected info level, got %v", e.Level)
	}
	if !strings.Contains(e.Message, "all replicas succeeded") {
		t.Fatalf("unexpected message %q", e.Message)
	}
	if got := e.Attrs["replica_nodes"]; got != int64(3) && got != 3 {
		t.Fatalf("expected replica_nodes=3, got %v", got)
	}
	if _, ok := e.Attrs["failed_nodes"]; ok {
		t.Fatalf("all-green summary must not carry failed_nodes: %v", e.Attrs)
	}
}

// TestLogReplicationSummaryPartialFailure verifies the degraded shape: one
// warn line naming each failed node with its terminal stage and error.
func TestLogReplicationSummaryPartialFailure(t *testing.T) {
	t.Parallel()
	logger, obs := log.NewObservedLogger(log.LevelInfo)

	logReplicationSummary(logger, "create_stemcell: replication", []replicaOutcome{
		{Node: "pve2", Stage: "replicated"},
		{Node: "pve3", Stage: "upload", Err: errors.New("disk full")},
		{Node: "pve4", Stage: "ensure-template", Err: errors.New("vmid conflict")},
	})

	entries := obs.All()
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 summary entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Level != log.LevelWarn {
		t.Fatalf("expected warn level, got %v", e.Level)
	}
	if !strings.Contains(e.Message, "some replicas failed") {
		t.Fatalf("unexpected message %q", e.Message)
	}
	if got, _ := e.Attrs["failed_nodes"].(string); got != "pve3,pve4" {
		t.Fatalf("expected failed_nodes=pve3,pve4, got %q", got)
	}
	if got, _ := e.Attrs["error_pve3"].(string); got != "upload: disk full" {
		t.Fatalf("expected error_pve3=%q, got %q", "upload: disk full", got)
	}
	if got, _ := e.Attrs["error_pve4"].(string); got != "ensure-template: vmid conflict" {
		t.Fatalf("expected error_pve4=%q, got %q", "ensure-template: vmid conflict", got)
	}
}

// TestLogReplicationSummaryEmptyOutcomes verifies the no-replica-nodes case
// emits nothing at all.
func TestLogReplicationSummaryEmptyOutcomes(t *testing.T) {
	t.Parallel()
	logger, obs := log.NewObservedLogger(log.LevelInfo)

	logReplicationSummary(logger, "create_stemcell: replication", nil)

	if obs.Len() != 0 {
		t.Fatalf("expected no entries for empty outcomes, got %d", obs.Len())
	}
}
