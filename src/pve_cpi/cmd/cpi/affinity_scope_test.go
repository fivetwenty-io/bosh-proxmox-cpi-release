package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// scopeSighting is what a command's PVE call saw on its context.
type scopeSighting struct {
	scope          *endpointpool.Scope
	releasedAtCall bool
}

// scopeRecorder collects the scope on the context of each PVE call a command
// makes.
type scopeRecorder struct {
	mu        sync.Mutex
	sightings []scopeSighting
}

func (r *scopeRecorder) observe(ctx context.Context) {
	scope := endpointpool.ScopeFromContext(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sightings = append(r.sightings, scopeSighting{scope: scope, releasedAtCall: scope.Released()})
}

// requireOneLiveScope fails unless the recorder saw at least minCalls calls,
// all under one scope that was live at each call and is released now that the
// command has returned.
func (r *scopeRecorder) requireOneLiveScope(t *testing.T, minCalls int) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sightings) < minCalls {
		t.Fatalf("the command made %d PVE calls, want at least %d", len(r.sightings), minCalls)
	}
	first := r.sightings[0].scope
	if first == nil {
		t.Fatal("the command's PVE call carried no affinity scope")
	}
	for i, s := range r.sightings {
		if s.scope != first {
			t.Errorf("call %d ran under a different scope than call 0", i)
		}
		if s.releasedAtCall {
			t.Errorf("call %d ran after its scope was released", i)
		}
	}
	if !first.Released() {
		t.Error("the command returned without releasing its scope")
	}
}

func TestCommandContextInstallsAScopeThatCancelReleases(t *testing.T) {
	ctx, cancel := commandContext(time.Minute)
	scope := endpointpool.ScopeFromContext(ctx)
	if scope == nil {
		t.Fatal("the command context carries no affinity scope")
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) <= 0 {
		t.Errorf("the command context lost its deadline: %v %v", deadline, ok)
	}
	var ran int
	if !scope.OnRelease(func() { ran++ }) {
		t.Fatal("a fresh scope refused a release function")
	}
	if scope.Released() {
		t.Fatal("the scope was released before the command ended")
	}
	cancel()
	if !scope.Released() || ran != 1 {
		t.Errorf("released=%v ran=%d; want the scope released once", scope.Released(), ran)
	}
	if ctx.Err() == nil {
		t.Error("cancel did not cancel the command context")
	}
	cancel()
	if ran != 1 {
		t.Errorf("a second cancel ran the release functions again: %d", ran)
	}
}

func TestStemcellRefsReportRunsUnderAnAffinityScope(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	client := refsCLIFixture()
	rec := &scopeRecorder{}
	client.onCall = rec.observe
	if code, stdout, stderr := runStemcellRefsTest(t, client, "report", "--config", cfgPath); code != 0 {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	rec.requireOneLiveScope(t, 1)
}

func TestStemcellRefsAdoptApplyKeepsOneAffinityScope(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	client := uncoveredCLIClient()
	rec := &scopeRecorder{}
	client.onCall = rec.observe
	code, stdout, stderr := runStemcellRefsTest(t, client, "adopt-owners", "--config", cfgPath,
		"--director-uuid", cliLiveUUID, "--director-name", "bosh", "--deployment", "cf", "--deployment", "redis", "--apply")
	if code != 0 || client.writes == 0 {
		t.Fatalf("exit %d writes %d stdout %q stderr %q", code, client.writes, stdout, stderr)
	}
	// The scan and each write share the one scope the command installed.
	rec.requireOneLiveScope(t, 1+client.writes)
}

// scopeNodes serves only the node listing the storage-journal command starts
// with, and reports the context it was called under.
type scopeNodes struct {
	nodes.Service
	rec *scopeRecorder
}

func (n scopeNodes) ListNodes(ctx context.Context) (*nodes.ListNodesResponse, error) {
	n.rec.observe(ctx)
	return &nodes.ListNodesResponse{}, nil
}

type scopeNodesClient struct {
	nilPVEClient
	rec *scopeRecorder
}

func (c scopeNodesClient) Nodes() nodes.Service { return scopeNodes{rec: c.rec} }

func TestStorageJournalRunsUnderAnAffinityScope(t *testing.T) {
	cfg := minimalCfg()
	base := provisionTemp(t)
	dir := filepath.Join(base, "allocations")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.StorageAllocationJournalDir = dir
	cfg.StoragePlacementNamespace = "director"
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "cpi.json")
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	rec := &scopeRecorder{}
	factory := func(*config.CPIConfig, *log.Logger, trace.Tracer) (pve.Client, error) {
		return scopeNodesClient{rec: rec}, nil
	}
	var stdout, stderr bytes.Buffer
	code := runStorageJournal([]string{"audit", "--config", path}, &stdout, &stderr, runOptions{ClientFactory: factory})
	if code != 1 || !strings.Contains(stderr.String(), "PVE listed no nodes") {
		t.Fatalf("exit %d, want 1 from the empty node listing; output %s%s", code, stdout.String(), stderr.String())
	}
	rec.requireOneLiveScope(t, 1)
}

func TestStemcellRefsPruneApplyKeepsOneAffinityScope(t *testing.T) {
	cfgPath := writeStorageJournalConfig(t, minimalCfg())
	client := &refsCLIClient{pools: &refsCLIPools{}, guests: []refsCLIGuest{
		{vmid: 101, name: "bosh-stemcell-a-1-aaaa1111", template: true,
			tags: "bosh-stemcell-cache;bosh-stemcell-sha-aaaa1111;director--old-uuid;director--new-uuid",
			desc: `{"name":"a","version":"1","sha8":"aaaa1111","kind":"heavy","director_refs":["old-uuid","new-uuid"]}`},
	}}
	rec := &scopeRecorder{}
	client.onCall = rec.observe
	code, stdout, stderr := runStemcellRefsTest(t, client, "prune", "--config", cfgPath, "--director", "old-uuid", "--apply")
	if code != 0 || !strings.Contains(stdout, "applied") {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// The scan and the tag write run under the one scope.
	rec.requireOneLiveScope(t, 2)
}
