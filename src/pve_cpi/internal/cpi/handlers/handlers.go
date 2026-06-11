package handlers

import (
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	stemcellfetch "github.com/fivetwenty-io/bosh-pve-cpi/internal/pve/stemcell_fetch"
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
// FetchResolver, when non-nil, replaces the default stemcellfetch.ResolveSource
// call inside HandleCreateStemcell's resolveFetchSource path. Set by tests only;
// production code leaves it nil.
type Deps struct {
	Config *config.CPIConfig
	PVE    pve.Client
	Agent  agent.Agent
	// RegistryAgent is the registry-based Agent used when agent_mode="auto" and
	// the stemcell api_version < 2. Nil unless agent_mode=auto and registry_endpoint
	// are both set.
	RegistryAgent agent.Agent
	Logger        *log.Logger
	Resolver      pve.BackendResolver
	FetchResolver func(rawURL string) (stemcellfetch.Source, stemcellfetch.Reference, error)
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
