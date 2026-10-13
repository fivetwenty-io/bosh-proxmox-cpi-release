package handlers_test

import (
	"context"
	"encoding/json"
	"strconv"
	"sync/atomic"
	"testing"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// countingNotesDeps returns found-VM Deps whose node service counts description
// writes, so a test can tell which cluster's client an annotation went through.
func countingNotesDeps(vmid int, writes *atomic.Int32) handlers.Deps {
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			writes.Add(1)
			return nil
		},
	}
	return testDepsFoundVM(vmid, &mockQEMUService{}, nodesSvc, nil, &mockAgentService{})
}

// annotateThroughCreateVM runs create_vm (which fails on its empty args, after
// the request overrides resolved) wrapped by a hook that, like notes_audit,
// writes Notes through whatever annotator the handler published for the request.
func annotateThroughCreateVM(t *testing.T, deps handlers.Deps, reqCtx jsonrpc.Context, vmid int) {
	t.Helper()
	var annotated bool
	hook := cpi.HookFunc{
		AfterFn: func(ctx context.Context, _ string, r any, e error) (any, error) {
			a, ok := cpi.RequestAnnotator(ctx).(interface {
				AnnotateNotes(context.Context, string, string) error
			})
			if !ok {
				t.Error("create_vm published no annotator for the After hooks")
				return r, e
			}
			if err := a.AnnotateNotes(ctx, strconv.Itoa(vmid), "bosh-audit: group=g"); err != nil {
				t.Errorf("AnnotateNotes: %v", err)
			}
			annotated = true
			return r, e
		},
	}
	wrapped := cpi.WrapHandler("create_vm", handlers.HandleCreateVM(deps), []cpi.Hook{hook})
	_, _ = wrapped.Handle(context.Background(), []json.RawMessage{}, reqCtx)
	if !annotated {
		t.Fatal("After hook did not annotate")
	}
}

func TestHandleCreateVM_PublishesOverrideAnnotator_NotJobClient(t *testing.T) {
	t.Parallel()
	const vmid = 301
	var jobWrites, overrideWrites atomic.Int32
	deps := countingNotesDeps(vmid, &jobWrites)
	overrideClient := countingNotesDeps(vmid, &overrideWrites).PVE
	deps.Config = overrideTestBaseConfig()
	deps.Overrides = &handlers.RequestOverrideRuntime{
		ClientFactory: func(*config.CPIConfig, *log.Logger) (pve.Client, error) { return overrideClient, nil },
		Logger:        log.NewNopLogger(),
	}

	annotateThroughCreateVM(t, deps, jsonrpc.Context{Extra: map[string]any{"pve_api_token": overrideTestEntryToken, "pve_host": "az2.example"}}, vmid)

	if got := overrideWrites.Load(); got != 1 {
		t.Errorf("override client saw %d description write(s), want 1", got)
	}
	if got := jobWrites.Load(); got != 0 {
		t.Errorf("job-level client saw %d description write(s), want 0", got)
	}
}

func TestHandleCreateVM_NoOverride_AnnotatorUsesJobClient(t *testing.T) {
	t.Parallel()
	const vmid = 302
	var jobWrites atomic.Int32
	deps := countingNotesDeps(vmid, &jobWrites)

	annotateThroughCreateVM(t, deps, jsonrpc.Context{}, vmid)

	if got := jobWrites.Load(); got != 1 {
		t.Errorf("job-level client saw %d description write(s), want 1", got)
	}
}
