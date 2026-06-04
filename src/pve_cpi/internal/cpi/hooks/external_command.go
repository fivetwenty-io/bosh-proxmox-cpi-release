package hooks

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	safeexec "github.com/fivetwenty-io/bosh-pve-cpi/internal/exec"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// defaultExternalCommandTimeout bounds an invocation when TimeoutMS is unset.
const defaultExternalCommandTimeout = 30 * time.Second

// ecVMIDKey carries the VM CID parsed in Before (delete_vm) to After.
type ecVMIDKey struct{}

// ExternalCommandHook runs an operator-configured, allowlisted host command on
// selected CPI methods. It is the sanctioned vehicle for site-specific VIP/LB/
// IPAM glue. The command runs with no shell, an absolute-path allowlist, a
// scrubbed environment, and a timeout (see internal/exec). Execution is
// best-effort by default: a non-zero exit or timeout is logged, not propagated.
type ExternalCommandHook struct {
	logger     *log.Logger
	runner     *safeexec.Runner
	command    string
	args       []string
	methods    map[string]bool
	inertCause string
}

// NewExternalCommandHook constructs an ExternalCommandHook. A nil config, an
// empty allowlist, or an empty command yields an inert hook — it never executes
// anything. The allowlist/no-shell/scrub guarantees live in the runner.
func NewExternalCommandHook(d Deps) cpi.Hook {
	c := d.ExternalCommand
	if c == nil || len(c.Allowlist) == 0 || c.Command == "" {
		cause := "no external_command config"
		if c != nil && len(c.Allowlist) == 0 {
			cause = "empty allowlist (hook inert)"
		} else if c != nil && c.Command == "" {
			cause = "no command configured"
		}
		return &ExternalCommandHook{logger: d.Logger, inertCause: cause}
	}
	timeout := defaultExternalCommandTimeout
	if c.TimeoutMS > 0 {
		timeout = time.Duration(c.TimeoutMS) * time.Millisecond
	}
	methods := map[string]bool{}
	if len(c.Methods) == 0 {
		methods["create_vm"] = true
		methods["delete_vm"] = true
	} else {
		for _, m := range c.Methods {
			methods[m] = true
		}
	}
	return &ExternalCommandHook{
		logger:  d.Logger,
		runner:  safeexec.New(c.Allowlist, c.EnvPasslist, timeout, d.Logger),
		command: c.Command,
		args:    c.Args,
		methods: methods,
	}
}

var _ cpi.Hook = (*ExternalCommandHook)(nil)

// Before stashes the VM CID for delete_vm (args[0]); After has no args.
func (h *ExternalCommandHook) Before(
	ctx context.Context, method string, args []json.RawMessage, _ jsonrpc.Context,
) context.Context {
	if h.inertCause != "" || !h.methods[method] {
		return ctx
	}
	if method == "delete_vm" && len(args) >= 1 {
		if vmid, ok := vmidFromArg(args[0]); ok {
			ctx = context.WithValue(ctx, ecVMIDKey{}, vmid)
		}
	}
	return ctx
}

// After runs the configured command for a successful matching method, injecting
// CPI_METHOD and (when known) CPI_VMID into the scrubbed child environment.
func (h *ExternalCommandHook) After(ctx context.Context, method string, result any, err error) (any, error) {
	if h.inertCause != "" || err != nil || !h.methods[method] {
		return result, err
	}
	extraEnv := map[string]string{"CPI_METHOD": method}
	if vmid, ok := ecVMID(ctx, method, result); ok {
		extraEnv["CPI_VMID"] = strconv.Itoa(vmid)
	}
	out, runErr := h.runner.Run(ctx, h.command, h.args, extraEnv)
	if runErr != nil {
		h.logger.Warn("external_command: invocation failed (non-fatal)",
			log.String("method", method), log.String("command", h.command), log.Err(runErr))
		return result, err
	}
	h.logger.Info("external_command: ran",
		log.String("method", method), log.String("command", h.command), log.String("stdout", out))
	return result, err
}

// ecVMID derives the VM CID from the create_vm result or the delete_vm context.
func ecVMID(ctx context.Context, method string, result any) (int, bool) {
	switch method {
	case "create_vm":
		return vmidFromResult(result)
	case "delete_vm":
		vmid, ok := ctx.Value(ecVMIDKey{}).(int)
		return vmid, ok
	default:
		return 0, false
	}
}
