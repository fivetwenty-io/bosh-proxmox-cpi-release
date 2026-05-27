package agent

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// newObservedLogger returns a log.Logger backed by an observer so tests can
// inspect emitted log entries, plus the observer itself.
func newObservedLogger() (*log.Logger, *log.Observer) {
	return log.NewObservedLogger(log.LevelDebug)
}

func TestNewNoAgent(t *testing.T) {
	t.Parallel()
	a := NewNoAgent(log.NewNopLogger())
	if a == nil {
		t.Fatal("NewNoAgent returned nil")
	}
}

func TestConfigure_NoOp(t *testing.T) {
	t.Parallel()
	a := NewNoAgent(log.NewNopLogger())
	err := a.Configure(context.Background(), "pve-node1", 101, AgentConfig{})
	if err != nil {
		t.Fatalf("Configure: expected nil error, got %v", err)
	}
}

func TestRemove_NoOp(t *testing.T) {
	t.Parallel()
	a := NewNoAgent(log.NewNopLogger())
	err := a.Remove(context.Background(), "pve-node1", 101)
	if err != nil {
		t.Fatalf("Remove: expected nil error, got %v", err)
	}
}

func TestUpdateDiskHints_NoOp(t *testing.T) {
	t.Parallel()
	a := NewNoAgent(log.NewNopLogger())

	if err := a.UpdateDiskHints(context.Background(), 101, nil); err != nil {
		t.Fatalf("UpdateDiskHints(nil): expected nil error, got %v", err)
	}

	if err := a.UpdateDiskHints(context.Background(), 101, []DiskHint{}); err != nil {
		t.Fatalf("UpdateDiskHints(empty): expected nil error, got %v", err)
	}

	hints := []DiskHint{
		{DiskCID: "local-lvm:vm-101-disk-0", DevicePath: "/dev/sdb"},
		{DiskCID: "local-lvm:vm-101-disk-1", DevicePath: "/dev/sdc"},
	}
	if err := a.UpdateDiskHints(context.Background(), 101, hints); err != nil {
		t.Fatalf("UpdateDiskHints(hints): expected nil error, got %v", err)
	}
}

func TestSatisfiesInterface(t *testing.T) {
	t.Parallel()
	var iface Agent = NewNoAgent(log.NewNopLogger())
	_ = iface // compile-time proof that *noAgent satisfies Agent
}

func TestLogsAtDebug_Configure(t *testing.T) {
	t.Parallel()
	logger, logs := newObservedLogger()
	a := NewNoAgent(logger)

	if err := a.Configure(context.Background(), "node1", 42, AgentConfig{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries := logs.All()
	if len(entries) == 0 {
		t.Fatal("Configure: expected at least one log entry, got none")
	}
	if entries[0].Level != log.LevelDebug {
		t.Fatalf("Configure: expected Debug level, got %v", entries[0].Level)
	}
}

func TestLogsAtDebug_Remove(t *testing.T) {
	t.Parallel()
	logger, logs := newObservedLogger()
	a := NewNoAgent(logger)

	if err := a.Remove(context.Background(), "node1", 42); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries := logs.All()
	if len(entries) == 0 {
		t.Fatal("Remove: expected at least one log entry, got none")
	}
	if entries[0].Level != log.LevelDebug {
		t.Fatalf("Remove: expected Debug level, got %v", entries[0].Level)
	}
}

func TestLogsAtDebug_UpdateDiskHints(t *testing.T) {
	t.Parallel()
	logger, logs := newObservedLogger()
	a := NewNoAgent(logger)

	hints := []DiskHint{{DiskCID: "local:disk-0", DevicePath: "/dev/sdb"}}
	if err := a.UpdateDiskHints(context.Background(), 42, hints); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries := logs.All()
	if len(entries) == 0 {
		t.Fatal("UpdateDiskHints: expected at least one log entry, got none")
	}
	if entries[0].Level != log.LevelDebug {
		t.Fatalf("UpdateDiskHints: expected Debug level, got %v", entries[0].Level)
	}
}
