// Guards that keep one PVE cluster from freeing another cluster's volumes when
// several clusters mount the same export.
//
// PVE names a volume for the VM ID that owns it, as in
// images/<vmid>/vm-<vmid>-disk-0.qcow2, and each cluster allocates VM IDs on
// its own. Two clusters whose IDs overlap therefore put their volumes in one
// namespace on a shared export. No CPI call sees a sibling cpi_config entry, so
// the CPI cannot refuse the overlap itself. Instead, the client refuses the two
// operations that would act on the other cluster's volumes: a destroy that asks
// PVE to sweep every unreferenced volume named for the VM's ID, and a volume
// delete whose owner ID lies outside every band this configuration allocates
// from. Both decorators sit below every handler, so every destroy and every
// delete the CPI sends passes through them.
package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// The parker band's built-in bounds. They mirror the defaults that
// config.ApplyDefaults fills (config cannot import this package), the same way
// the other band constants in vmid.go do.
const (
	VMIDRangeParkerStart = 90000
	VMIDRangeParkerEnd   = 90999
)

// VMIDBand is one inclusive VMID range.
type VMIDBand struct {
	Start int
	End   int
}

// Contains reports whether id lies inside the band.
func (b VMIDBand) Contains(id int) bool {
	return id >= b.Start && id <= b.End
}

// Overlaps reports whether the two bands share at least one VMID. A band whose
// start lies above its end holds no VMID and overlaps nothing.
func (b VMIDBand) Overlaps(o VMIDBand) bool {
	if b.Start > b.End || o.Start > o.End {
		return false
	}
	return b.Start <= o.End && o.Start <= b.End
}

// String renders the band as "start-end".
func (b VMIDBand) String() string {
	return fmt.Sprintf("%d-%d", b.Start, b.End)
}

// VMIDBands holds the four bands one configuration allocates from: VMs,
// persistent disks, stemcell templates, and parkers.
type VMIDBands struct {
	VM       VMIDBand
	Disk     VMIDBand
	Template VMIDBand
	Parker   VMIDBand
}

// DefaultVMIDBands returns the four bands a configuration that sets none of
// them allocates from.
func DefaultVMIDBands() VMIDBands {
	return VMIDBands{
		VM:       VMIDBand{Start: VMIDRangeVMStart, End: VMIDRangeVMEnd},
		Disk:     VMIDBand{Start: VMIDRangeDiskStart, End: VMIDRangeDiskEnd},
		Template: VMIDBand{Start: VMIDRangeTemplateStart, End: VMIDRangeTemplateEnd},
		Parker:   VMIDBand{Start: VMIDRangeParkerStart, End: VMIDRangeParkerEnd},
	}
}

// VMIDBandsFromConfig returns the effective bands of cfg, which are the
// ranges the allocators actually draw from. A pair that WithRange would
// ignore, because its start lies below 100 or its end does not lie above its
// start, takes the built-in band, since that is the band the allocators fall
// back to. The VM band has one wrinkle: create_vm lifts a start below 100 to
// 100 before it calls WithRange, while the parker and migration paths pass the
// pair as it is, so such a band covers both [100, end] and the built-in band.
// The parker band comes from the config's own accessors, which apply the same
// per-bound fill the parked strategy uses. A nil cfg yields the defaults.
func VMIDBandsFromConfig(cfg *config.CPIConfig) VMIDBands {
	bands := DefaultVMIDBands()
	if cfg == nil {
		return bands
	}
	bands.VM = vmBandFromPair(cfg.VMIDRangeStart, cfg.VMIDRangeEnd, bands.VM)
	bands.Disk = bandFromPair(cfg.DiskVMIDRangeStart, cfg.DiskVMIDRangeEnd, bands.Disk)
	bands.Template = bandFromPair(cfg.StemcellTemplateVMIDRangeStart, cfg.StemcellTemplateVMIDRangeEnd, bands.Template)
	bands.Parker = VMIDBand{Start: cfg.ParkedDiskVMIDRangeStartValue(), End: cfg.ParkedDiskVMIDRangeEndValue()}
	return bands
}

// bandFromPair returns [start, end] when WithRange accepts the pair, and def
// otherwise.
func bandFromPair(start, end int, def VMIDBand) VMIDBand {
	if start >= VMIDRangeVMStart && end > start {
		return VMIDBand{Start: start, End: end}
	}
	return def
}

// vmBandFromPair is bandFromPair for the VM band, widened to cover create_vm's
// lift of a start below 100 to 100 (see VMIDBandsFromConfig).
func vmBandFromPair(start, end int, def VMIDBand) VMIDBand {
	if start < VMIDRangeVMStart && end > VMIDRangeVMStart {
		return VMIDBand{Start: VMIDRangeVMStart, End: max(end, def.End)}
	}
	return bandFromPair(start, end, def)
}

// All returns the four bands in a fixed order, with their names.
func (b VMIDBands) All() []NamedVMIDBand {
	return []NamedVMIDBand{
		{Name: "vm", Band: b.VM},
		{Name: "disk", Band: b.Disk},
		{Name: "stemcell template", Band: b.Template},
		{Name: "parker", Band: b.Parker},
	}
}

// NamedVMIDBand pairs a band with the name a message uses for it.
type NamedVMIDBand struct {
	Name string
	Band VMIDBand
}

// Contains reports whether id lies inside any of the four bands.
func (b VMIDBands) Contains(id int) bool {
	for _, band := range b.All() {
		if band.Band.Contains(id) {
			return true
		}
	}
	return false
}

// String names every band, as in "vm 100-8999, disk 9000-29999, ...".
func (b VMIDBands) String() string {
	parts := make([]string, 0, 4)
	for _, band := range b.All() {
		parts = append(parts, band.Name+" "+band.Band.String())
	}
	return strings.Join(parts, ", ")
}

// AllDefault reports whether every band is at its built-in value.
func (b VMIDBands) AllDefault() bool {
	return b == DefaultVMIDBands()
}

// sharedVolumeLocatorPattern finds an allocation locator anywhere in a volume's
// last path segment. It is deliberately looser than allocationVolume, because a
// refusal should catch every name that claims another namespace, while the
// exemption for this namespace's own volumes requires the full allocation name.
var sharedVolumeLocatorPattern = regexp.MustCompile(`-bosh-([0-9a-f]{16})-alloc-`)

// volumeLastSegment returns the part of a volume ID after its last slash or
// colon, which is the name PVE reads the owner from.
func volumeLastSegment(volume string) string {
	return volume[strings.LastIndexAny(volume, "/:")+1:]
}

// ForeignVolumeError reports whether a volume on shared storage may belong to
// another cluster that mounts the same export, and returns the permanent
// refusal for it when it may. It reads nothing from PVE: the caller decides
// whether the storage is shared.
//
// A volume is foreign in two cases. In the first, its name carries the
// allocation locator of a storage placement namespace other than namespace,
// whatever its owner ID, because the managed paths act only on volumes with
// their own locator. In the second, PVE's owner rule (vm-<N>-* or base-<N>-*,
// which covers the config-drive ISO vm-<N>-config.iso) names an owner outside
// every band of bands. A full allocation name with this namespace's locator is
// never foreign, because the allocation journal proves that this namespace
// made it. A name that carries no owner, such as a stemcell import file, is
// never foreign either.
func ForeignVolumeError(storageName, volume string, bands VMIDBands, namespace string) error {
	segment := volumeLastSegment(volume)
	ours := ""
	if strings.TrimSpace(namespace) != "" {
		ours = AllocationNamespaceLocator(namespace)
	}
	if m := sharedVolumeLocatorPattern.FindStringSubmatch(segment); len(m) == 2 && m[1] != ours {
		return cpierrors.Cloud("refusing to delete volume %q on storage %q: its name carries the allocation "+
			"locator %s of another storage placement namespace, so on storage that several clusters can mount "+
			"it may belong to another cluster, and only that namespace's own paths may delete it",
			volume, storageName, m[1])
	}
	if ours != "" {
		if m := allocationVolume.FindStringSubmatch(segment); len(m) >= 4 && m[2] == ours && allocationUUID.MatchString(m[3]) {
			return nil
		}
	}
	ownerText, ok := volumeOwnerVMID(volume)
	if !ok {
		return nil
	}
	// An owner too large to parse lies outside every band by definition.
	owner, err := strconv.Atoi(ownerText)
	if err == nil && bands.Contains(owner) {
		return nil
	}
	return cpierrors.Cloud("refusing to delete volume %q on storage %q: its name makes VM %s its owner, and %s "+
		"lies outside every VMID band of this configuration (%s); on storage that several clusters can mount, "+
		"that volume may belong to another cluster, so the CPI sends no delete. To delete a disk this "+
		"configuration made before a band moved, widen a band to cover %s first",
		volume, storageName, ownerText, ownerText, bands, ownerText)
}

// ownedConfigDriveISOKey carries the VMID whose config-drive ISO a caller has
// proven it owns.
type ownedConfigDriveISOKey struct{}

// WithOwnedConfigDriveISO returns a context that lets the volume guard pass a
// delete of exactly vm-<vmid>-config.iso, whatever band the VMID lies in. A
// caller sets it only after proving the CPI owns that VM (the CID and owner
// tag it just read, or a VMID it allocated itself), and before the destroy is
// sent, because the cluster no longer holds the VM afterwards. The permit
// covers no other VMID and no other volume name. A non-positive vmid leaves ctx
// as it is.
func WithOwnedConfigDriveISO(ctx context.Context, vmid int) context.Context {
	if vmid <= 0 {
		return ctx
	}
	return context.WithValue(ctx, ownedConfigDriveISOKey{}, vmid)
}

// OwnedConfigDriveISOPermits reports whether ctx carries a permit that covers
// volume, which must end in exactly vm-<vmid>-config.iso for the permitted VMID.
func OwnedConfigDriveISOPermits(ctx context.Context, volume string) bool {
	vmid, ok := ctx.Value(ownedConfigDriveISOKey{}).(int)
	return ok && vmid > 0 && volumeLastSegment(volume) == "vm-"+strconv.Itoa(vmid)+"-config.iso"
}

// storageClassifier answers whether a storage is shared. StorageInfoCache
// satisfies it.
type storageClassifier interface {
	Get(ctx context.Context, name string) (StorageInfo, error)
}

// sharedVolumeGuardIndexTTL is how long the volume guard trusts its storage
// index. It matches the job-level cache in cmd/cpi, so one CPI call reads
// /storage for the guard at most once, while a long-lived process still sees
// an operator's storage.cfg edit within a minute.
const sharedVolumeGuardIndexTTL = 60 * time.Second

// sharedVolumeGuardStorage refuses a volume delete that ForeignVolumeError
// calls foreign, on a storage that is shared or that the storage index cannot
// classify. Every other storage call passes through untouched.
type sharedVolumeGuardStorage struct {
	storage.Service
	classifier storageClassifier
	bands      VMIDBands
	namespace  string
}

// newSharedVolumeGuardStorage wraps inner with the volume guard for cfg's
// bands and namespace, classifying storages through an index read from
// lister.
func newSharedVolumeGuardStorage(inner storage.Service, lister StorageLister, cfg *config.CPIConfig) *sharedVolumeGuardStorage {
	guard := &sharedVolumeGuardStorage{Service: inner, bands: VMIDBandsFromConfig(cfg)}
	if cfg != nil {
		guard.namespace = cfg.StoragePlacementNamespace
	}
	if lister != nil {
		guard.classifier = NewStorageInfoCache(lister, sharedVolumeGuardIndexTTL)
	}
	return guard
}

// refusal returns the error that stops a delete of volume on storageName, or
// nil when the delete may go ahead. The storage index is read only for a name
// that would be foreign, so a delete of an ordinary volume costs nothing
// extra. A storage the index cannot answer for counts as shared.
func (s *sharedVolumeGuardStorage) refusal(ctx context.Context, storageName, volume string) error {
	refused := ForeignVolumeError(storageName, volume, s.bands, s.namespace)
	if refused == nil || OwnedConfigDriveISOPermits(ctx, volume) {
		return nil
	}
	if s.classifier != nil && storageName != "" {
		if info, err := s.classifier.Get(ctx, storageName); err == nil && !info.IsShared() {
			return nil
		}
	}
	return refused
}

// DeleteVolume refuses a foreign volume and otherwise delegates.
func (s *sharedVolumeGuardStorage) DeleteVolume(ctx context.Context, node, storageName, volume string) error {
	if err := s.refusal(ctx, storageName, volume); err != nil {
		return err
	}
	// The guard forwards the caller's own call; it adds no call site of its own.
	return s.Service.DeleteVolume(ctx, node, storageName, volume) //nolint:forbidigo // decorator passthrough
}

// DeleteVolumeIfExists refuses a foreign volume and otherwise delegates.
func (s *sharedVolumeGuardStorage) DeleteVolumeIfExists(ctx context.Context, node, storageName, volume string) (bool, error) {
	if err := s.refusal(ctx, storageName, volume); err != nil {
		return false, err
	}
	return s.Service.DeleteVolumeIfExists(ctx, node, storageName, volume)
}

// DeleteVolumeAsync refuses a foreign volume and otherwise delegates.
func (s *sharedVolumeGuardStorage) DeleteVolumeAsync(ctx context.Context, node, storageName, volume string) (string, error) {
	if err := s.refusal(ctx, storageName, volume); err != nil {
		return "", err
	}
	return s.Service.DeleteVolumeAsync(ctx, node, storageName, volume)
}

// DeleteVolumeIfExistsAsync refuses a foreign volume and otherwise delegates.
func (s *sharedVolumeGuardStorage) DeleteVolumeIfExistsAsync(ctx context.Context, node, storageName, volume string) (bool, string, error) {
	if err := s.refusal(ctx, storageName, volume); err != nil {
		return false, "", err
	}
	return s.Service.DeleteVolumeIfExistsAsync(ctx, node, storageName, volume)
}

// sweepSuppressedWarned gates the warning that a destroy's unreferenced-disk
// sweep was turned off to one firing per process. It is an atomic.Pointer so
// ResetSweepSuppressedWarnOnceForTest can swap in a fresh gate race-safely,
// the same seam pattern as duplicateBackingWarnOnce.
var sweepSuppressedWarned atomic.Pointer[atomic.Bool]

func init() {
	sweepSuppressedWarned.Store(&atomic.Bool{})
}

// ResetSweepSuppressedWarnOnceForTest re-arms the process-wide gate for the
// sweep warning and returns a restore function. Tests that use it must not
// call t.Parallel.
func ResetSweepSuppressedWarnOnceForTest() func() {
	prev := sweepSuppressedWarned.Swap(&atomic.Bool{})
	return func() {
		sweepSuppressedWarned.Store(prev)
	}
}

// sharedVolumeGuardNodes turns off a destroy's unreferenced-disk sweep on a
// node that sees shared storage. PVE's sweep frees every volume named for the
// VM's ID on every enabled images storage the node sees, and on a shared
// export that includes volumes another cluster still uses. A destroy that does
// not ask for the sweep passes through with no extra call, and so does every
// other node call.
type sharedVolumeGuardNodes struct {
	nodes.Service
	logger *log.Logger
}

// DeleteQemu sends the destroy with destroy-unreferenced-disks forced to false
// when the node's images storages include a shared one or cannot be listed.
// The caller's params are never modified.
func (n *sharedVolumeGuardNodes) DeleteQemu(ctx context.Context, node string, vmid string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
	if params == nil || params.DestroyUnreferencedDisks == nil || !*params.DestroyUnreferencedDisks {
		return n.Service.DeleteQemu(ctx, node, vmid, params)
	}
	if reason := n.sweepBlocker(ctx, node); reason != "" {
		off := false
		guarded := *params
		guarded.DestroyUnreferencedDisks = &off
		n.noteSweepSuppressed(ctx, node, vmid, reason)
		return n.Service.DeleteQemu(ctx, node, vmid, &guarded)
	}
	return n.Service.DeleteQemu(ctx, node, vmid, params)
}

// sweepBlocker returns why the sweep must stay off on node, or "" when every
// enabled images storage the node sees is local. A storage the node reports as
// inactive still counts, because PVE's sweep activates the storages it reads.
// A listing that fails, a nil listing, and a row that does not decode all
// block the sweep, because none of them proves the node sees only local
// storage.
func (n *sharedVolumeGuardNodes) sweepBlocker(ctx context.Context, node string) string {
	content, enabled := "images", true
	resp, err := n.ListStorage(ctx, node, &nodes.ListStorageParams{Content: &content, Enabled: &enabled})
	if err != nil {
		return "the node's storage listing failed: " + err.Error()
	}
	if resp == nil {
		return "the node's storage listing came back empty-handed"
	}
	for _, raw := range *resp {
		var row struct {
			Storage string             `json:"storage"`
			Type    string             `json:"type"`
			Content string             `json:"content"`
			Shared  *sdkclient.PVEBool `json:"shared"`
			Enabled *sdkclient.PVEBool `json:"enabled"`
		}
		if json.Unmarshal(raw, &row) != nil {
			return "the node's storage listing holds a row that does not decode"
		}
		if row.Enabled != nil && !row.Enabled.Bool() {
			continue
		}
		if row.Content != "" && !hasImagesContent(row.Content) {
			continue
		}
		info := StorageInfo{Name: row.Storage, Type: row.Type, Shared: row.Shared != nil && row.Shared.Bool()}
		if info.IsShared() {
			return fmt.Sprintf("the node sees shared storage %q (type %s)", row.Storage, row.Type)
		}
	}
	return ""
}

// noteSweepSuppressed logs the first suppressed sweep of the process as a
// warning that names the config key, and every later one at debug level.
func (n *sharedVolumeGuardNodes) noteSweepSuppressed(ctx context.Context, node, vmid, reason string) {
	fallback := n.logger
	if fallback == nil {
		fallback = log.NewNopLogger()
	}
	logger := log.FromContextOr(ctx, fallback)
	fields := []log.Field{log.String("node", node), log.String("vmid", vmid), log.String("reason", reason)}
	if sweepSuppressedWarned.Load().CompareAndSwap(false, true) {
		logger.Warn("pve: destroying the VM with destroy-unreferenced-disks sent as false, because PVE's sweep "+
			"would free every volume named for this VM ID on every storage the node sees, including volumes "+
			"another cluster that mounts the same export still uses; pve.destroy_unreferenced_disks takes "+
			"effect only on nodes that see local storage alone", fields...)
		return
	}
	logger.Debug("pve: destroy-unreferenced-disks sent as false", fields...)
}
