package hooks

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// notesAuditKey is the unexported context key under which NotesAuditHook.Before
// stashes the audit line derived from the create_vm env, for After to write.
type notesAuditKey struct{}

// NotesAuditHook writes the BOSH deploy/job/index identity into a created VM's
// guest Notes (PVE description) so the PVE UI shows which deployment owns the
// VM. It observes only: After returns the handler result and error unchanged.
// Notes writing is best-effort — a failure is logged, never propagated.
type NotesAuditHook struct {
	logger    *log.Logger
	annotator VMAnnotator
}

// NewNotesAuditHook constructs a NotesAuditHook. A nil Deps.Annotator leaves the
// hook active but inert on the write path (it logs that no annotator is wired).
func NewNotesAuditHook(d Deps) cpi.Hook {
	return &NotesAuditHook{logger: d.Logger, annotator: d.Annotator}
}

var _ cpi.Hook = (*NotesAuditHook)(nil)

// Before parses the create_vm env arg (args[5]) into a single audit line and
// stashes it in the returned context. For other methods it is a no-op.
func (h *NotesAuditHook) Before(
	ctx context.Context, method string, args []json.RawMessage, _ jsonrpc.Context,
) context.Context {
	if method != "create_vm" || len(args) < 6 {
		return ctx
	}
	var env map[string]any
	if err := json.Unmarshal(args[5], &env); err != nil {
		return ctx
	}
	line := boshAuditLine(env)
	if line == "" {
		return ctx
	}
	return context.WithValue(ctx, notesAuditKey{}, line)
}

// After writes the stashed audit line to a successfully-created VM's Notes via
// the annotator, decoding the VM CID from the result. Any error is logged and
// swallowed; the handler result and error pass through unchanged.
func (h *NotesAuditHook) After(ctx context.Context, method string, result any, err error) (any, error) {
	if method != "create_vm" || err != nil {
		return result, err
	}
	line, ok := ctx.Value(notesAuditKey{}).(string)
	if !ok || line == "" {
		return result, err
	}
	vmid, ok := vmidFromResult(result)
	if !ok {
		return result, err
	}
	if h.annotator == nil {
		h.logger.Debug("notes_audit: no annotator wired; skipping Notes write", log.Int("vmid", vmid))
		return result, err
	}
	if annErr := h.annotator.AnnotateNotes(ctx, vmid, line); annErr != nil {
		h.logger.Warn("notes_audit: Notes write failed (non-fatal)", log.Int("vmid", vmid), log.Err(annErr))
	}
	return result, err
}

// boshAuditLine builds a compact "bosh: <group> index=<n>" line from the env
// map. env["bosh"]["group"] is "<director>-<deployment>-<job>"; index, id, and
// name are best-effort. Returns "" when no useful identity is present.
func boshAuditLine(env map[string]any) string {
	boshRaw, ok := env["bosh"].(map[string]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, 3)
	if group, _ := boshRaw["group"].(string); group != "" {
		parts = append(parts, "group="+group)
	}
	if id, _ := boshRaw["id"].(string); id != "" {
		parts = append(parts, "id="+id)
	}
	if name, _ := boshRaw["name"].(string); name != "" {
		parts = append(parts, "name="+name)
	}
	if len(parts) == 0 {
		return ""
	}
	return "bosh-audit: " + strings.Join(parts, " ")
}

// vmidFromResult extracts the integer VM CID from a create_vm result, which is
// []any{vmCID string, networks}. vmCID is strconv.Itoa(vmid).
func vmidFromResult(result any) (int, bool) {
	arr, ok := result.([]any)
	if !ok || len(arr) == 0 {
		return 0, false
	}
	cid, ok := arr[0].(string)
	if !ok {
		return 0, false
	}
	vmid, err := strconv.Atoi(cid)
	if err != nil {
		return 0, false
	}
	return vmid, true
}
