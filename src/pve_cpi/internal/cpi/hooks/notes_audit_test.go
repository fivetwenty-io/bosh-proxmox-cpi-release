package hooks_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/hooks"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

type recordingAnnotator struct{ calls []int }

func (r *recordingAnnotator) AnnotateNotes(_ context.Context, vmid int, _ string) error {
	r.calls = append(r.calls, vmid)
	return nil
}

// publishingHandler stands in for create_vm: it publishes an annotator on the
// request's effective client, then succeeds with a VM CID.
func publishingHandler(a *recordingAnnotator) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		if a != nil {
			cpi.PublishRequestAnnotator(ctx, a)
		}
		return []any{"42", map[string]any{}}, nil
	})
}

func runNotesAudit(t *testing.T, job *recordingAnnotator, h cpi.Handler) {
	t.Helper()
	hook := hooks.NewNotesAuditHook(hooks.Deps{Logger: log.NewNopLogger(), Annotator: job})
	args := []json.RawMessage{nil, nil, nil, nil, nil, json.RawMessage(`{"bosh":{"group":"d-dep-job"}}`)}
	if _, err := cpi.WrapHandler("create_vm", h, []cpi.Hook{hook}).Handle(context.Background(), args, jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNotesAuditHook_PrefersRequestAnnotator(t *testing.T) {
	t.Parallel()
	job, req := &recordingAnnotator{}, &recordingAnnotator{}
	runNotesAudit(t, job, publishingHandler(req))
	if len(req.calls) != 1 || len(job.calls) != 0 {
		t.Errorf("request annotator calls=%v job annotator calls=%v; want the request one only", req.calls, job.calls)
	}
}

func TestNotesAuditHook_FallsBackToJobAnnotator(t *testing.T) {
	t.Parallel()
	job := &recordingAnnotator{}
	runNotesAudit(t, job, publishingHandler(nil))
	if len(job.calls) != 1 {
		t.Errorf("job annotator calls=%v; want one", job.calls)
	}
}
