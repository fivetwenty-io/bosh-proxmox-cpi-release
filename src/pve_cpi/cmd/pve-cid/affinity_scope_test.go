package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// scopeRecorder collects the affinity scope on the context of each PVE call a
// subcommand makes, and whether the scope was already released at that call.
type scopeRecorder struct {
	mu     sync.Mutex
	scopes []*endpointpool.Scope
	early  []bool
}

func (r *scopeRecorder) observe(ctx context.Context) {
	scope := endpointpool.ScopeFromContext(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scopes = append(r.scopes, scope)
	r.early = append(r.early, scope.Released())
}

// requireOneLiveScope fails unless the recorder saw at least one call, all
// under one scope that was live at each call and is released now that the
// subcommand has returned.
func (r *scopeRecorder) requireOneLiveScope(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.scopes) == 0 {
		t.Fatal("the subcommand made no PVE call to observe")
	}
	if r.scopes[0] == nil {
		t.Fatal("the subcommand's PVE call carried no affinity scope")
	}
	for i, s := range r.scopes {
		if s != r.scopes[0] {
			t.Errorf("call %d ran under a different scope than call 0", i)
		}
		if r.early[i] {
			t.Errorf("call %d ran after its scope was released", i)
		}
	}
	if !r.scopes[0].Released() {
		t.Error("the subcommand returned without releasing its scope")
	}
}

func TestCommandContextCarriesAScopeThatReleaseEnds(t *testing.T) {
	ctx, release := commandContext()
	scope := endpointpool.ScopeFromContext(ctx)
	if scope == nil {
		t.Fatal("the command context carries no affinity scope")
	}
	if scope.Released() {
		t.Fatal("the scope was released before the command ended")
	}
	release()
	if !scope.Released() {
		t.Error("release did not release the scope")
	}
}

type scopeReader struct {
	*fakeReader
	rec *scopeRecorder
}

func (r scopeReader) ListClusterVMs(ctx context.Context) ([]ClusterVM, error) {
	r.rec.observe(ctx)
	return r.fakeReader.ListClusterVMs(ctx)
}

func (r scopeReader) ListNodes(ctx context.Context) ([]string, error) {
	r.rec.observe(ctx)
	return r.fakeReader.ListNodes(ctx)
}

func scopeReaderOpener(rec *scopeRecorder, reader *fakeReader) readerOpener {
	return func(string, io.Writer) (*config.CPIConfig, Reader, error) {
		return haCfg("auto", "none"), scopeReader{fakeReader: reader, rec: rec}, nil
	}
}

func TestLocateRunsUnderAnAffinityScope(t *testing.T) {
	rec := &scopeRecorder{}
	reader := &fakeReader{vms: []ClusterVM{{VMID: 100, Node: "pve1", Name: "vm-a"}}, configs: map[string]map[string]any{
		"pve1/100": diskCfg("", map[string]string{"scsi0": "local-lvm:vm-100-disk-0,size=20G"}),
	}}
	var stdout, stderr bytes.Buffer
	code := runLocateWith([]string{"local-lvm:vm-9500-disk-0"}, &stdout, &stderr, scopeReaderOpener(rec, reader))
	if code != exitOK {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	rec.requireOneLiveScope(t)
}

func TestStemcellsRunsUnderAnAffinityScope(t *testing.T) {
	rec := &scopeRecorder{}
	reader := &fakeReader{nodes: []string{"pve1"}}
	var stdout, stderr bytes.Buffer
	code := runStemcellsWith([]string{"--storage", "local"}, &stdout, &stderr, scopeReaderOpener(rec, reader))
	if code != exitOK {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	rec.requireOneLiveScope(t)
}

type scopeHA struct {
	*fakeHA
	rec *scopeRecorder
}

func (h scopeHA) Profile(ctx context.Context) (*ha.Profile, error) {
	h.rec.observe(ctx)
	return h.fakeHA.Profile(ctx)
}

func TestHARunsUnderAnAffinityScope(t *testing.T) {
	rec := &scopeRecorder{}
	d := newHATestDeps(haCfg("auto", "none"), scopeHA{fakeHA: haBaseFake(), rec: rec})
	if code, out, errOut := runHATest(t, d); code != exitOK {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
	}
	rec.requireOneLiveScope(t)
}

func TestStoragePlanRunsUnderAnAffinityScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, []byte(`{"method":"create_disk","arguments":[1024,{}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &scopeRecorder{}
	var stdout, stderr bytes.Buffer
	code := runStoragePlanWithObserver([]string{
		"--request",
		path,
		"--config",
		"config.json",
		"--json",
	}, &stdout, &stderr,
		func(string) (handlers.Deps, error) { return handlers.Deps{}, nil },
		func(
			ctx context.Context,
			_ handlers.Deps,
			_ handlers.StoragePlanDiagnosticRequest,
		) (*handlers.StoragePlanDiagnostic, error) {
			rec.observe(ctx)
			return &handlers.StoragePlanDiagnostic{}, nil
		})
	if code != exitOK {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	rec.requireOneLiveScope(t)
}
