package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

func TestStoragePlanNodeBridges_LookupErrorLogsAndStaysFailClosed(t *testing.T) {
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	nd := &fwNodesStub{nodeIfaceErr: errors.New("pve 503")}
	deps := fwDeps(&fwClusterStub{}, nd, icMinConfig())
	deps.Logger = logger

	ok, err := storagePlanNodeBridges(context.Background(), deps, "pve2", []string{"vmbr0"})
	if ok || err == nil {
		t.Fatalf("want (false, error) on a lookup failure, got (%v, %v)", ok, err)
	}
	entries := obs.All()
	if len(entries) != 1 {
		t.Fatalf("want one warning, got %d", len(entries))
	}
	if entries[0].Level != log.LevelWarn || entries[0].Attrs["node"] != "pve2" {
		t.Errorf("warning should be Warn level and name the node, got %+v", entries[0])
	}
}

func TestStoragePlanNodeBridges_AbsentBridgeIsQuiet(t *testing.T) {
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	nd := &fwNodesStub{nodeIfaces: []string{"vmbr1"}}
	deps := fwDeps(&fwClusterStub{}, nd, icMinConfig())
	deps.Logger = logger

	ok, err := storagePlanNodeBridges(context.Background(), deps, "pve2", []string{"vmbr0"})
	if ok || err != nil {
		t.Fatalf("want (false, nil) for a proven absence, got (%v, %v)", ok, err)
	}
	if obs.Len() != 0 {
		t.Errorf("a proven absence must not warn, got %d entries", obs.Len())
	}
}
