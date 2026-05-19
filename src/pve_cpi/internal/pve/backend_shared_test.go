package pve

import (
	"context"
	"testing"
)

func TestSharedBackend_NodeForCreate_PrefersCloudPropNode(t *testing.T) {
	b := newSharedBackend(nil, StorageInfo{Name: "ceph", Type: "rbd", Shared: true}, "pve-default")
	got, err := b.NodeForCreate(context.Background(), "100", "pve-explicit")
	if err != nil {
		t.Fatalf("NodeForCreate: %v", err)
	}
	if got != "pve-explicit" {
		t.Fatalf("got %q, want pve-explicit", got)
	}
}

func TestSharedBackend_NodeForCreate_FallsBackToDefault(t *testing.T) {
	b := newSharedBackend(nil, StorageInfo{Name: "ceph", Type: "rbd", Shared: true}, "pve-default")
	got, err := b.NodeForCreate(context.Background(), "", "")
	if err != nil {
		t.Fatalf("NodeForCreate: %v", err)
	}
	if got != "pve-default" {
		t.Fatalf("got %q, want pve-default", got)
	}
}

func TestSharedBackend_NodeForCreate_ErrorsWhenNothingResolves(t *testing.T) {
	b := newSharedBackend(nil, StorageInfo{Name: "ceph", Type: "rbd", Shared: true}, "")
	_, err := b.NodeForCreate(context.Background(), "", "")
	if err == nil {
		t.Fatalf("expected error when no node hints available")
	}
}

func TestSharedBackend_NodeForExisting_UsesDefault(t *testing.T) {
	b := newSharedBackend(nil, StorageInfo{Name: "ceph", Type: "rbd", Shared: true}, "pve-default")
	got, err := b.NodeForExisting(context.Background(), "vm-100-disk-0")
	if err != nil {
		t.Fatalf("NodeForExisting: %v", err)
	}
	if got != "pve-default" {
		t.Fatalf("got %q, want pve-default", got)
	}
}

func TestSharedBackend_NodeForExisting_FallsBackToInfoNodes(t *testing.T) {
	b := newSharedBackend(nil, StorageInfo{Name: "nfs", Type: "nfs", Nodes: []string{"pve-02", "pve-03"}}, "")
	got, err := b.NodeForExisting(context.Background(), "anything")
	if err != nil {
		t.Fatalf("NodeForExisting: %v", err)
	}
	if got != "pve-02" {
		t.Fatalf("got %q, want pve-02", got)
	}
}
