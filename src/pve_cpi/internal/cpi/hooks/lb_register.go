package hooks

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/lb"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// defaultLBTimeout bounds a Data Plane API call when TimeoutMS is unset.
const defaultLBTimeout = 10 * time.Second

// lbAddrKey / lbVMIDKey carry per-call state from Before to After: the VM IP to
// register (create_vm) and the VM CID to deregister (delete_vm).
type lbAddrKey struct{}
type lbVMIDKey struct{}

// LBRegisterHook registers a created VM into an HAProxy backend (via the Data
// Plane API) on create_vm and deregisters it on delete_vm. Registration is
// best-effort: a Data Plane API failure is logged and never fails the CPI call,
// so an unreachable LB never blocks a deploy or delete.
type LBRegisterHook struct {
	logger     *log.Logger
	registrar  lb.Registrar
	backend    string
	port       int
	inertCause string // non-empty => hook does nothing but log this once-per-call
}

// NewLBRegisterHook constructs an LBRegisterHook from Deps.LBRegister. A nil
// config or a registrar that fails to construct yields an inert hook that logs
// the cause rather than failing dispatch wiring.
func NewLBRegisterHook(d Deps) cpi.Hook {
	if d.LBRegister == nil {
		return &LBRegisterHook{logger: d.Logger, inertCause: "no lb_register config"}
	}
	timeout := defaultLBTimeout
	if d.LBRegister.TimeoutMS > 0 {
		timeout = time.Duration(d.LBRegister.TimeoutMS) * time.Millisecond
	}
	reg, err := lb.NewHAProxyRegistrar(lb.HAProxyConfig{
		Endpoint:       d.LBRegister.Endpoint,
		User:           d.LBRegister.User,
		Password:       d.LBRegister.Password,
		CACertPEM:      d.LBRegister.CACertPEM,
		AllowPrivateIP: d.LBRegister.AllowPrivateIP,
		Timeout:        timeout,
	})
	if err != nil {
		return &LBRegisterHook{logger: d.Logger, inertCause: "lb_register registrar init failed: " + err.Error()}
	}
	return &LBRegisterHook{
		logger:    d.Logger,
		registrar: reg,
		backend:   d.LBRegister.Backend,
		port:      d.LBRegister.Port,
	}
}

var _ cpi.Hook = (*LBRegisterHook)(nil)

// Before stashes the data After needs: the first static IP from the networks
// arg for create_vm, and the VM CID for delete_vm.
func (h *LBRegisterHook) Before(
	ctx context.Context, method string, args []json.RawMessage, _ jsonrpc.Context,
) context.Context {
	if h.inertCause != "" {
		return ctx
	}
	switch method {
	case "create_vm":
		if len(args) >= 4 {
			if ip := firstNetworkIP(args[3]); ip != "" {
				ctx = context.WithValue(ctx, lbAddrKey{}, ip)
			}
		}
	case "delete_vm":
		if len(args) >= 1 {
			if vmid, ok := vmidFromArg(args[0]); ok {
				ctx = context.WithValue(ctx, lbVMIDKey{}, vmid)
			}
		}
	}
	return ctx
}

// After registers (create_vm) or deregisters (delete_vm) the VM, best-effort.
func (h *LBRegisterHook) After(ctx context.Context, method string, result any, err error) (any, error) {
	if h.inertCause != "" {
		if method == "create_vm" || method == "delete_vm" {
			h.logger.Debug("lb_register: inert", log.String("cause", h.inertCause))
		}
		return result, err
	}
	if err != nil {
		return result, err
	}
	switch method {
	case "create_vm":
		h.handleCreate(ctx, result)
	case "delete_vm":
		h.handleDelete(ctx)
	}
	return result, err
}

func (h *LBRegisterHook) handleCreate(ctx context.Context, result any) {
	vmid, ok := vmidFromResult(result)
	if !ok {
		return
	}
	addr, _ := ctx.Value(lbAddrKey{}).(string)
	if addr == "" {
		h.logger.Debug("lb_register: no static IP to register; skipping", log.Int("vmid", vmid))
		return
	}
	srv := lb.Server{Name: lbServerName(vmid), Address: addr, Port: h.port}
	if regErr := h.registrar.Register(ctx, h.backend, srv); regErr != nil {
		h.logger.Warn("lb_register: register failed (non-fatal)",
			log.Int("vmid", vmid), log.String("backend", h.backend), log.Err(regErr))
		return
	}
	h.logger.Info("lb_register: registered VM in backend",
		log.Int("vmid", vmid), log.String("backend", h.backend), log.String("address", addr))

	// Register a rollback so that if a later post-hook fails and the dispatch
	// rolls the create back, this VM is removed from the backend rather than
	// left routing traffic to a destroyed (or keep-failed, agent-dead) VM.
	backend, serverName := h.backend, lbServerName(vmid)
	cpi.RegisterRollback(ctx, func(c context.Context) {
		if derr := h.registrar.Deregister(c, backend, serverName); derr != nil {
			h.logger.Warn("lb_register: rollback deregister failed (non-fatal)",
				log.Int("vmid", vmid), log.String("backend", backend), log.Err(derr))
			return
		}
		h.logger.Info("lb_register: rollback deregistered VM from backend",
			log.Int("vmid", vmid), log.String("backend", backend))
	})
}

func (h *LBRegisterHook) handleDelete(ctx context.Context) {
	vmid, ok := ctx.Value(lbVMIDKey{}).(int)
	if !ok {
		return
	}
	if regErr := h.registrar.Deregister(ctx, h.backend, lbServerName(vmid)); regErr != nil {
		h.logger.Warn("lb_register: deregister failed (non-fatal)",
			log.Int("vmid", vmid), log.String("backend", h.backend), log.Err(regErr))
		return
	}
	h.logger.Info("lb_register: deregistered VM from backend",
		log.Int("vmid", vmid), log.String("backend", h.backend))
}

// lbServerName is the stable HAProxy server label for a VM CID.
func lbServerName(vmid int) string { return "vm-" + strconv.Itoa(vmid) }

// firstNetworkIP returns the "ip" of a network in a create_vm networks arg (map
// of name -> {ip,...}). It iterates network names in sorted order so a multi-NIC
// VM registers a deterministic, stable address across deploys rather than a
// map-random one. Returns "" when none is present or parseable.
func firstNetworkIP(raw json.RawMessage) string {
	var nets map[string]struct {
		IP string `json:"ip"`
	}
	if err := json.Unmarshal(raw, &nets); err != nil {
		return ""
	}
	names := make([]string, 0, len(nets))
	for name := range nets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ip := nets[name].IP; ip != "" {
			return ip
		}
	}
	return ""
}

// vmidFromArg decodes a JSON string VM CID (strconv.Itoa(vmid)) to its int.
func vmidFromArg(raw json.RawMessage) (int, bool) {
	var cid string
	if err := json.Unmarshal(raw, &cid); err != nil {
		return 0, false
	}
	vmid, err := strconv.Atoi(cid)
	if err != nil {
		return 0, false
	}
	return vmid, true
}
