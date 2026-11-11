package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// The binary never sets runOptions.DrainWait, so the last drain of a
// subcommand gets a wait of zero and applies the default bound. A write that
// needs a fraction of that bound finishes before the subcommand returns, and a
// drain that took the zero literally would cancel it at once. No other test
// leaves the wait unset on a subcommand.
func TestSubcommandDrainAppliesTheDefaultBoundWhenNoWaitIsSet(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	var wrote atomic.Bool
	//nolint:unparam // the client always fails so the subcommand exits with code 1
	failing := func(*config.CPIConfig, *log.Logger, trace.Tracer) (pve.Client, error) {
		err := endpointcache.Go("down mark", config.DiscoveryModeAuto, func(ctx context.Context) {
			select {
			case <-time.After(300 * time.Millisecond):
				wrote.Store(true)
			case <-ctx.Done():
			}
		})
		if err != nil {
			t.Errorf("start background work: %v", err)
		}
		return nil, errors.New("dial refused")
	}

	var stderr bytes.Buffer
	code := runWithArgs([]string{"stemcell-refs", "report", "--config", cfgPath}, strings.NewReader(""),
		&bytes.Buffer{}, &stderr, runOptions{ClientFactory: failing})
	if code != 1 {
		t.Fatalf("exit %d, want the computed 1: %s", code, stderr.String())
	}
	if !wrote.Load() {
		t.Fatal("the subcommand returned before its background write finished, or cancelled it")
	}
}
