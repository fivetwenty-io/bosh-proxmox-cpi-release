package handlers

import (
	"context"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	stemcellfetch "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/stemcell_fetch"
)

// Handler is a package-level alias for cpi.Handler so individual handler files
// can reference Handler and HandlerFunc without qualifying the cpi package.
type Handler = cpi.Handler

// HandlerFunc is a package-level alias for cpi.HandlerFunc.
type HandlerFunc = cpi.HandlerFunc

// Deps bundles cross-cutting dependencies every handler needs.
//
// Resolver maps a storage name to a persistent-disk Backend (shared or local).
// Production wiring (main.go) constructs it from a StorageInfoCache; tests may
// leave it nil to get the static "shared on Config.Node" default via the
// backendResolverOrDefault helper.
//
// FetchResolver, when non-nil, replaces the default
// stemcellfetch.ResolveSourceWith call inside HandleCreateStemcell's
// resolveFetchSource path. Set by tests only; production code leaves it nil.
type Deps struct {
	Config *config.CPIConfig
	PVE    pve.Client
	Agent  agent.Agent
	Logger *log.Logger
	// Resolver maps a storage name to a persistent-disk Backend (shared or
	// local). Production wiring (main.go) constructs it from a
	// StorageInfoCache; tests may leave it nil to get the static
	// "shared on Config.Node" default via the backendResolverOrDefault helper.
	Resolver      pve.BackendResolver
	FetchResolver func(rawURL string) (stemcellfetch.Source, stemcellfetch.Reference, error)
	// NodeEndpoints resolves the direct pveproxy address for node-scoped
	// storage uploads (stemcell images). Production wiring (main.go)
	// constructs it from pve.node_endpoints plus /cluster/status discovery;
	// nil (every test Deps literal that leaves it unset) means uploads take
	// the proxied path through the configured endpoint, the pre-existing
	// behavior.
	NodeEndpoints *pve.NodeEndpointResolver
	// Inflight holds the per-node in-flight semaphores that gate mutating
	// handlers when max_inflight_per_node is set. main.go constructs one via
	// NewInflightRegistry so all handlers share it. Nil (e.g. in test Deps
	// literals) behaves as unlimited: acquire is nil-receiver-safe.
	Inflight *nodeInflightRegistry
	// Overrides enables per-request pve_* context config overrides (BOSH
	// cpi-config multi-cluster support — see context_override.go). Every
	// handler's top-level wrapper calls Deps.WithRequestOverrides, which is a
	// no-op returning Deps unchanged whenever Overrides is nil (the zero
	// value; every existing handler-unit-test Deps literal leaves this
	// unset), so this field is opt-in and does not change behavior for any
	// deployment or test that does not wire it.
	Overrides *RequestOverrideRuntime
	// RequestDirectorUUID is the calling BOSH director's UUID from the
	// JSON-RPC request context, stamped by WithRequestOverrides so helpers
	// that only receive Deps (parker provenance, prune scoping) can attribute
	// work to the requesting director without threading jsonrpc.Context
	// through every signature. Empty when the caller sent no context
	// (hand-rolled CPI calls, some tests).
	RequestDirectorUUID string
}

// Log returns the per-request, span-correlated logger stored in ctx (attached
// by cmd/cpi's runCPI via log.IntoContext, carrying request_id/method/
// trace_id/span_id fields) when present, else falls back to d.Logger.
//
// A handler unit test that builds ctx via context.Background() and a Deps
// literal that sets Logger gets d.Logger unchanged (no correlation
// fields), so no test setup needs a ctx-stored logger. If d.Logger is
// also nil (a Deps literal that omits it entirely), Log falls back to a nop
// logger rather than panicking on a nil *log.Logger receiver.
func (d Deps) Log(ctx context.Context) *log.Logger {
	fallback := d.Logger
	if fallback == nil {
		fallback = log.NewNopLogger()
	}
	return log.FromContextOr(ctx, fallback)
}

// backendResolverOrDefault returns d.Resolver if non-nil; otherwise it builds a
// static resolver that classifies every storage as shared on d.Config.Node.
// This preserves pre-abstraction behavior for any handler test that does not
// wire a Resolver onto Deps.
func backendResolverOrDefault(d Deps) pve.BackendResolver {
	if d.Resolver != nil {
		return d.Resolver
	}
	defaultNode := ""
	if d.Config != nil {
		defaultNode = d.Config.Node
	}
	return pve.NewStaticBackendResolver(d.PVE, defaultNode)
}

// mustRegister calls d.Register and panics on error. All names passed here are
// compile-time constants matching the canonical Methods() set; an error indicates
// a programming mistake (e.g. a typo in a method name) that must be caught at
// startup rather than silently producing a broken dispatcher.
func mustRegister(d *cpi.Dispatcher, method string, h Handler) {
	// invariant violation: unknown or duplicate method name passed at startup; cannot occur at runtime
	if err := d.Register(method, h); err != nil {
		panic("cpi: RegisterAll: " + err.Error())
	}
}

// RegisterAll installs every CPI handler onto d. main.go calls this
// once after building deps.
func RegisterAll(d *cpi.Dispatcher, deps Deps) {
	mustRegister(d, "info", HandleInfo(deps))
	mustRegister(d, "create_stemcell", HandleCreateStemcell(deps))
	mustRegister(d, "delete_stemcell", HandleDeleteStemcell(deps))
	mustRegister(d, "create_vm", HandleCreateVM(deps))
	mustRegister(d, "delete_vm", HandleDeleteVM(deps))
	mustRegister(d, "has_vm", HandleHasVM(deps))
	mustRegister(d, "reboot_vm", HandleRebootVM(deps))
	mustRegister(d, "set_vm_metadata", HandleSetVMMetadata(deps))
	mustRegister(d, "calculate_vm_cloud_properties", HandleCalculateVMCloudProperties(deps))
	mustRegister(d, "create_disk", HandleCreateDisk(deps))
	mustRegister(d, "delete_disk", HandleDeleteDisk(deps))
	mustRegister(d, "has_disk", HandleHasDisk(deps))
	mustRegister(d, "attach_disk", HandleAttachDisk(deps))
	mustRegister(d, "detach_disk", HandleDetachDisk(deps))
	mustRegister(d, "snapshot_disk", HandleSnapshotDisk(deps))
	mustRegister(d, "delete_snapshot", HandleDeleteSnapshot(deps))
	mustRegister(d, "get_disks", HandleGetDisks(deps))
	mustRegister(d, "resize_disk", HandleResizeDisk(deps))
	mustRegister(d, "set_disk_metadata", HandleSetDiskMetadata(deps))
	mustRegister(d, "update_disk", HandleUpdateDisk(deps))
	mustRegister(d, "create_network", HandleCreateNetwork(deps))
	mustRegister(d, "delete_network", HandleDeleteNetwork(deps))
}
