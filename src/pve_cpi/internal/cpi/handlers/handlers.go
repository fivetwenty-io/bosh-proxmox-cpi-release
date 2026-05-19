// Package handlers contains the 22 BOSH CPI v2 method implementations.
// Each method has its own file plus a sibling _test.go. main.go calls
// RegisterAll to install every handler on a Dispatcher.
package handlers

import (
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
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
type Deps struct {
	Config   *config.CPIConfig
	PVE      pve.Client
	Agent    agent.Agent
	Logger   *log.Logger
	Resolver pve.BackendResolver
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

// RegisterAll installs every CPI handler onto d. main.go calls this
// once after building deps.
func RegisterAll(d *cpi.Dispatcher, deps Deps) {
	d.Register("info", HandleInfo(deps))
	d.Register("create_stemcell", HandleCreateStemcell(deps))
	d.Register("delete_stemcell", HandleDeleteStemcell(deps))
	d.Register("create_vm", HandleCreateVM(deps))
	d.Register("delete_vm", HandleDeleteVM(deps))
	d.Register("has_vm", HandleHasVM(deps))
	d.Register("reboot_vm", HandleRebootVM(deps))
	d.Register("set_vm_metadata", HandleSetVMMetadata(deps))
	d.Register("calculate_vm_cloud_properties", HandleCalculateVMCloudProperties(deps))
	d.Register("create_disk", HandleCreateDisk(deps))
	d.Register("delete_disk", HandleDeleteDisk(deps))
	d.Register("has_disk", HandleHasDisk(deps))
	d.Register("attach_disk", HandleAttachDisk(deps))
	d.Register("detach_disk", HandleDetachDisk(deps))
	d.Register("snapshot_disk", HandleSnapshotDisk(deps))
	d.Register("delete_snapshot", HandleDeleteSnapshot(deps))
	d.Register("get_disks", HandleGetDisks(deps))
	d.Register("resize_disk", HandleResizeDisk(deps))
	d.Register("set_disk_metadata", HandleSetDiskMetadata(deps))
	d.Register("update_disk", HandleUpdateDisk(deps))
	d.Register("create_network", HandleCreateNetwork(deps))
	d.Register("delete_network", HandleDeleteNetwork(deps))
}
