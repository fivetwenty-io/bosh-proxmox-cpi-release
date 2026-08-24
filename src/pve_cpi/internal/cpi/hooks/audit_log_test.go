package hooks_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/hooks"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

func TestAuditLogHook_OkPath(t *testing.T) {
	var buf bytes.Buffer
	logger, err := log.NewLogger("info", &buf)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	h := hooks.NewAuditLogHook(hooks.Deps{Logger: logger})

	ctx := h.Before(context.Background(), "create_vm", nil, jsonrpc.Context{})
	res, e := h.After(ctx, "create_vm", "result", nil)
	if res != "result" || e != nil {
		t.Errorf("After must return result/err unchanged on ok path; got %v / %v", res, e)
	}

	out := buf.String()
	for _, want := range []string{"cpi_audit", "create_vm", `"outcome":"ok"`, "duration_ms"} {
		if !strings.Contains(out, want) {
			t.Errorf("audit log %q missing %q", out, want)
		}
	}
}

func TestAuditLogHook_ErrorPath(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := log.NewLogger("info", &buf)
	h := hooks.NewAuditLogHook(hooks.Deps{Logger: logger})

	wantErr := cpierrors.Cloud("boom")
	ctx := h.Before(context.Background(), "create_vm", nil, jsonrpc.Context{})
	res, e := h.After(ctx, "create_vm", nil, wantErr)
	if !errors.Is(e, wantErr) {
		t.Errorf("After must return the error unchanged; got %v", e)
	}
	if res != nil {
		t.Errorf("result should remain nil; got %v", res)
	}

	out := buf.String()
	for _, want := range []string{`"outcome":"error"`, "error_type"} {
		if !strings.Contains(out, want) {
			t.Errorf("audit error log %q missing %q", out, want)
		}
	}
}

func TestRegistry_AuditLogKnown(t *testing.T) {
	if !hooks.Known("audit_log") {
		t.Error("audit_log must be a known hook")
	}
	if hooks.Known("does_not_exist") {
		t.Error("an unregistered name must not be known")
	}
	if hooks.Registry["audit_log"] == nil {
		t.Error("Registry[audit_log] constructor must be non-nil")
	}
	names := hooks.Names()
	want := []string{"audit_log", "external_command", "lb_register", "notes_audit"}
	if len(names) != len(want) {
		t.Fatalf("Names() = %v; want %v", names, want)
	}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("Names()[%d] = %q; want %q (full: %v)", i, names[i], w, names)
		}
	}
}
