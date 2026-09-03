// Parker implements the persistent-disk parking mechanism: detached BOSH
// persistent disks are held on scsi0..scsi30 slots of a dedicated never-started
// "parker" VM instead of floating free in PVE storage. This gives each
// unattached disk a PVE-visible owner VM, making accidental deletion harder.
//
// # ParkerConfig construction
//
// Callers assemble ParkerConfig from CPIConfig accessors:
//
//	cfg := pve.ParkerConfig{
//	    VMIDRangeStart: cpiCfg.ParkedDiskVMIDRangeStartValue(),
//	    VMIDRangeEnd:   cpiCfg.ParkedDiskVMIDRangeEndValue(),
//	    DirectorID:     deps.RequestDirectorUUID, // empty = omit director scope tag
//	}
//
// # Tag constants
//
// ParkerTag ("bosh-parker") marks every parker VM. When DirectorID is set a
// second tag "director--<sanitized-id>" is appended (mirrors stemcell provenance
// convention from create_stemcell.go).
//
// # Slot capacity
//
// Each parker VM holds up to 31 disks (scsi0..scsi30). When every existing
// parker on a node is full, a fresh parker VMID is allocated in the same range.
// New parks always reuse the lowest existing parker that still has a free slot
// before creating another parker, so the VMID band is not exhausted one disk
// at a time.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// CpiOwnershipTag is the fixed ownership marker stamped on every VM and
// template created by this CPI. It mirrors handlers.ownershipTag ("bosh-cpi")
// and is defined here so the pve package (which cannot import handlers) can
// apply the same marker to parker VMs without an import cycle.
const CpiOwnershipTag = "bosh-cpi"

// ParkerTag is the PVE tag applied to every parker VM. Downstream code
// (set_disk_metadata, snapshot_disk, delete_vm) identifies parker VMs via
// IsParkerVM, which requires both a VMID-range check and this tag.
const ParkerTag = "bosh-parker"

// parkerMaxSlots is the number of scsi slots available on a single parker VM
// (scsi0 through scsi30 inclusive).
const parkerMaxSlots = 31

// ErrNoSlots is returned when all scsi0..scsi30 slots on a parker VM are
// occupied. Callers (ParkDisk) react by creating a second parker VM for the
// same node.
var ErrNoSlots = errors.New("parker VM has no free scsi slots")

// ParkerConfig holds the parker VMID range and optional director scope. Built
// by callers from CPIConfig accessors — see package-level doc comment.
type ParkerConfig struct {
	// VMIDRangeStart is the first VMID in the parker band (inclusive). Must be
	// ≥ 100 and less than VMIDRangeEnd.
	VMIDRangeStart int
	// VMIDRangeEnd is the last VMID in the parker band (inclusive). Must be
	// greater than VMIDRangeStart.
	VMIDRangeEnd int
	// DirectorID is the optional BOSH director identifier. When non-empty a
	// "director--<sanitized-id>" tag is added to newly created parker VMs so
	// operators can distinguish parkers per director in multi-director clusters.
	DirectorID string
	// DiskStorage is the CPI's configured pve.disk_storage pool. When set,
	// createParkerVM passes it to pve.WithStorageScan so parker VMID
	// allocation also scans that storage's volume content, closing the same
	// cross-cluster co-mingling gap WithStorageScan already closes for the
	// VM, disk, and template ranges (see WithStorageScan's doc comment):
	// without this, two independent PVE clusters sharing one storage backend
	// can each allocate a parker at the same VMID, and a later
	// DestroyUnreferencedDisks-driven destroy of one cluster's parker frees
	// the other cluster's parked disks by matching VMID. Empty (the
	// zero-value default) makes the scan a no-op — byte-identical to prior
	// releases for callers that do not set it.
	DiskStorage string
	// FallbackNode names the node to attribute a cluster-resources row to when
	// the row itself elides "node". A row without a node cannot be read, and
	// dropping it silently is how the holder scan concludes a volume is free
	// while a running VM still has it on a bus slot -- two configs referencing
	// one volume. Empty (the zero value) keeps the previous behavior of
	// skipping such rows, which is correct only when no node is known.
	FallbackNode string
	// ParkedEnabled reports whether detached_disk_strategy resolves to "parked"
	// for this load. It controls only the log level of the in-band-without-tag
	// anomaly in the holder scan: under "parked" a workload VM inside the parker
	// band is surprising and warns; under "free" or a stood-down default the
	// band can legitimately overlap vmid_range (overlap validation is relaxed
	// there), so the same state logs at debug. Nothing gates behavior on it.
	ParkedEnabled bool
	// NowFunc returns the current time. Nil defaults to time.Now().UTC().
	// Tests inject a fixed clock to assert parked_at values deterministically.
	NowFunc func() time.Time
	// AnchorStrict enforces the anchor-missing invariant: when a parker the
	// holder scan identified vanishes before its config can be read
	// (resolveDiskHolder) or before the unpark detach runs (unparkAtLocked),
	// strict refuses with a Cloud error naming the vanished parker and the
	// recovery, instead of silently treating the volume as free-floating.
	// A parker is durable infrastructure the CPI never deletes, so a vanished
	// one means an out-of-band deletion. Gated on ParkedEnabled: under "free"
	// or a stood-down default the permissive behavior stands regardless.
	AnchorStrict bool
}

// parkerNow returns the configured clock time or time.Now().UTC() when NowFunc is nil.
func parkerNow(cfg ParkerConfig) time.Time {
	if cfg.NowFunc != nil {
		return cfg.NowFunc()
	}
	return time.Now().UTC()
}

// ---------------------------------------------------------------------------
// Provenance sentinel codec (local to pve package — no handlers import to avoid cycle)
// ---------------------------------------------------------------------------
//
// parseParkerSentinel/renderParkerSentinel are the bosh_parked_disks-specific
// wrapper around the shared ParseSentinel/RenderSentinel codec in sentinel.go
// (also used by the bosh_attached_disks codec in attached_disks.go). Same
// wire format as set_disk_metadata's independent codec; all these codecs
// coexist on one VM description by using distinct top-level JSON keys.

// ParkContext carries per-call attribution for provenance records written on
// park. Fields are optional: zero values are omitted from the sentinel JSON.
// DiskCID should be the encoded CID as the Director knows it (may include a
// metadata suffix). SourceVMCID is the VM the disk was detached from.
type ParkContext struct {
	// DiskCID is the full disk CID passed by the Director (may include encoded
	// metadata suffix). Stored as disk_cid in the provenance entry so disk-audit
	// can cross-reference back to the Director's view of the disk.
	DiskCID string
	// SourceVMCID is the BOSH VM CID from which the disk was detached. Omitted
	// when unknown (e.g. re-park on retry when only the bare volid is available).
	SourceVMCID string
	// StableID is the disk's stable identity token (DiskCIDMeta.ID). When set,
	// the parker attach carries it as a serial= drive option so the identity
	// rides the drive entry, and the provenance record is keyed by it instead
	// of the volid — a move_disk reassignment renames the volume, so a
	// volid-keyed record would be orphaned by the first transfer. Empty for
	// legacy disks, which keep the volid keying forever.
	StableID string
	// Opts carries the disk's recorded drive-option overrides (operator
	// updates made through update_disk) into the provenance entry, so the
	// overrides survive the park and are merged back into the drive string at
	// the next attach. Empty means no overrides are recorded.
	Opts map[string]string
}

// parkerProvEntry is a single parked-disk record stored in the sentinel.
// Optional fields are omitted when empty so the JSON stays minimal.
//
// Legacy entries are keyed by the bare volid and omit Volid/Slot. Stable-ID
// entries are keyed by the disk's bpd- token and carry Volid (the volid the
// parker currently — or imminently, during a transfer — holds the volume
// under) plus Slot (the parker bus slot a detach-side transfer targets).
// During a transfer the entry is the intent record D13's write ordering
// requires: written before the source VM's slot is deleted, finalized with
// the landed volid after the serial is re-applied.
type parkerProvEntry struct {
	DiskCID     string `json:"disk_cid"`
	SourceVMCID string `json:"source_vm_cid,omitempty"`
	ParkedAt    string `json:"parked_at"`
	Node        string `json:"node"`
	DirectorID  string `json:"director_id,omitempty"`
	Volid       string `json:"volid,omitempty"`
	Slot        string `json:"slot,omitempty"`
	// Opts holds the disk's drive-option overrides while it is parked (see
	// disk_opt_overlay.go). Either keying generation may carry it; absent
	// means no overrides are recorded.
	Opts map[string]string `json:"opts,omitempty"`
}

// parseParkerSentinel extracts the description text outside the sentinel
// (nonBOSH) and the current bosh_parked_disks map. Corrupted JSON → fresh
// empty map (sentinel rebuilt from scratch; nonBOSH text preserved).
func parseParkerSentinel(desc string) (nonBOSH string, disks map[string]parkerProvEntry, raw map[string]json.RawMessage) {
	nonBOSH, raw = ParseSentinel(desc)
	disks = make(map[string]parkerProvEntry)

	// Extract our own key.
	if rawDisks, ok := raw["bosh_parked_disks"]; ok {
		_ = json.Unmarshal(rawDisks, &disks) // best-effort; corruption → empty map
		delete(raw, "bosh_parked_disks")     // will be re-serialised below
	}
	return
}

// renderParkerSentinel builds the full description string from the nonBOSH
// prefix, the updated bosh_parked_disks map, and the raw remainder of other
// codec keys. When both disks and raw are empty, returns nonBOSH unchanged
// (no sentinel block emitted — avoids writing an empty <!--BOSH:{}-->).
func renderParkerSentinel(nonBOSH string, disks map[string]parkerProvEntry, raw map[string]json.RawMessage) (string, error) {
	if len(disks) == 0 && len(raw) == 0 {
		return nonBOSH, nil
	}

	// Merge: start from raw (other keys), add our key.
	merged := make(map[string]json.RawMessage, len(raw)+1)
	for k, v := range raw {
		merged[k] = v
	}
	if len(disks) > 0 {
		b, err := json.Marshal(disks)
		if err != nil {
			return "", err
		}
		merged["bosh_parked_disks"] = json.RawMessage(b)
	}

	return RenderSentinel(nonBOSH, merged)
}

// updateParkerProvenance merges a parked-disk entry into the parker VM
// description sentinel and writes it back via UpdateQemuConfig.
//
// Best-effort: any failure is logged at WARN and the function returns nil.
// Park/unpark success is never gated on provenance writes.
//
// Concurrent-park lost-update is acceptable — two CPI processes racing to
// park different disks on the same parker may each overwrite the other's
// provenance entry. The disk itself remains correctly attached; provenance
// is advisory metadata for disk-audit.
func updateParkerProvenance(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, bareVolid, slot string, cfg ParkerConfig, pctx ParkContext) {
	entry := buildParkerProvEntry(node, bareVolid, slot, cfg, pctx)
	if err := writeParkerProvenance(ctx, c, logger, node, parkerVMID, parkerProvKey(bareVolid, pctx.StableID), entry, cfg); err != nil {
		if logger != nil {
			logger.Warn("parker provenance: provenance not updated",
				log.Int("parker_vmid", parkerVMID),
				log.String("node", node),
				log.String("volid", bareVolid),
				log.Err(err),
			)
		}
	}
}

// parkerProvKey is the sentinel key a provenance entry lives under: the
// stable ID when the disk has one (rename-proof), the bare volid otherwise.
func parkerProvKey(bareVolid, stableID string) string {
	if stableID != "" {
		return stableID
	}
	return bareVolid
}

// buildParkerProvEntry assembles one provenance record. Volid and Slot are
// recorded only for stable-ID disks — legacy entries otherwise keep their
// pre-stable-ID JSON shape, which external readers (scripts/disk-audit,
// scripts/_pve_verify.py, pve-cid) parse; those readers tolerate the
// optional opts field, which either generation may carry.
func buildParkerProvEntry(node, bareVolid, slot string, cfg ParkerConfig, pctx ParkContext) parkerProvEntry {
	// disk_cid: prefer the full encoded CID from ParkContext (as the Director
	// knows it); fall back to bareVolid when context is absent.
	diskCIDField := bareVolid
	if pctx.DiskCID != "" {
		diskCIDField = pctx.DiskCID
	}
	entry := parkerProvEntry{
		DiskCID:     diskCIDField,
		SourceVMCID: pctx.SourceVMCID,
		ParkedAt:    parkerNow(cfg).Format(time.RFC3339),
		Node:        node,
		DirectorID:  cfg.DirectorID,
	}
	if pctx.StableID != "" {
		entry.Volid = bareVolid
		entry.Slot = slot
	}
	if len(pctx.Opts) > 0 {
		entry.Opts = sanitizeDiskOptOverlay(pctx.Opts)
	}
	return entry
}

// ErrProvenanceFull reports that a parker's description cannot hold another
// parked-disk record. It is a capacity condition, exactly like ErrNoSlots:
// the parker is unusable for this park, another one is not. Every park loop
// treats the two identically (see isParkerCapacityError), so a full store
// moves the disk to the next parker instead of failing the detach.
var ErrProvenanceFull = errors.New("parker provenance store is full")

// parkerDescriptionLimit is PVE's hard cap on a guest description. Exceeding
// it fails the config PUT with "description: value may only be 8192
// characters long" — observed on lab-pmx parker 90472, where it turned
// detach_disk and delete_vm into fail-closed refusals. The cap is stated in
// characters and measured here in bytes, which for any description longer in
// bytes than in characters errs toward refusing early.
const parkerDescriptionLimit = 8192

// parkerDescriptionBudget is the largest description this package will write,
// leaving parkerDescriptionReserve below the cap. The reserve covers growth
// this read-modify-write cannot see: the other sentinel keys
// (bosh_attached_disks, bosh_disk_opt_overlays) belong to other writers who
// may add to the same description between our read and PVE's write. Losing
// that race at the PUT would surface as an opaque PVE rejection on a parker
// the caller has already committed to; losing it at the budget check surfaces
// as ErrProvenanceFull, which the park loops know how to route around.
const parkerDescriptionBudget = parkerDescriptionLimit - parkerDescriptionReserve

// parkerDescriptionReserve is the headroom left below the PVE cap.
const parkerDescriptionReserve = 512

// parkerProvenanceGraceWindow is how long an unreferenced record is protected
// from collection. A detach-side transfer writes its intent record BEFORE the
// disk lands on the parker (the write ordering D13 requires), so "no slot on
// this parker names the volume" is the normal state of a young record, not
// evidence that it is stale. A transfer that has not converged in an hour has
// long since been re-driven or abandoned by the Director.
const parkerProvenanceGraceWindow = time.Hour

// parkerReferencedVolids returns every volid a parker's config currently
// names, from an active bus slot or an unusedN key. Values are bare: option
// suffixes are stripped so they compare directly against recorded volids.
func parkerReferencedVolids(vmCfg map[string]any) map[string]bool {
	out := make(map[string]bool)
	for _, val := range qemu.ParseDisks(vmCfg) {
		bare := val
		if comma := strings.IndexByte(bare, ','); comma >= 0 {
			bare = bare[:comma]
		}
		if bare != "" {
			out[bare] = true
		}
	}
	for _, volid := range FindUnusedDiskEntries(vmCfg) {
		if volid != "" {
			out[volid] = true
		}
	}
	return out
}

// provEntryVolid is the volume a record names: the volid field for stable-ID
// records, and the key itself for legacy records, which are keyed by the bare
// volid and omit the field.
func provEntryVolid(key string, entry parkerProvEntry) string {
	if entry.Volid != "" {
		return entry.Volid
	}
	return key
}

// collectStaleParkerProvenance deletes from disks every record whose volume
// nothing on the parker references and whose parked_at is older than the
// grace window. It returns the keys it removed, for the caller's log line.
//
// keepKey is the record being written and is never collected — it is the
// youngest record in the store by definition, and for a transfer intent it is
// the disk's only identity carrier.
//
// An unparseable or absent parked_at counts as older than the window. Every
// writer in this package stamps RFC3339, so a value that will not parse is
// corruption rather than a young record, and corruption that also names a
// volume nothing holds is exactly what this is here to clear.
func collectStaleParkerProvenance(
	disks map[string]parkerProvEntry, vmCfg map[string]any, keepKey string, now time.Time,
) []string {
	referenced := parkerReferencedVolids(vmCfg)
	var pruned []string
	for key, entry := range disks {
		if key == keepKey {
			continue
		}
		if referenced[provEntryVolid(key, entry)] {
			continue
		}
		if parsed, parseErr := time.Parse(time.RFC3339, entry.ParkedAt); parseErr == nil {
			if now.Sub(parsed) < parkerProvenanceGraceWindow {
				continue
			}
		}
		delete(disks, key)
		pruned = append(pruned, key)
	}
	sort.Strings(pruned)
	return pruned
}

// projectParkerProvenance renders the description a provenance write would
// push for vmCfg with entry added under key, applying collection first, and
// reports the keys it collected. It returns ErrProvenanceFull when the result
// would exceed the budget: the refusal belongs here rather than at the PUT,
// because a caller that learns the store is full can still choose another
// parker, while PVE's rejection arrives too late to be useful and says only
// that a string was too long.
func projectParkerProvenance(
	vmCfg map[string]any, node string, parkerVMID int,
	key string, entry parkerProvEntry, cfg ParkerConfig,
) (desc string, pruned []string, err error) {
	nonBOSH, disks, rawOther := parseParkerSentinel(DescriptionFromConfig(vmCfg))
	disks[key] = entry

	// The caller's config read is the same one the reference test needs, so
	// collection costs no extra API call.
	pruned = collectStaleParkerProvenance(disks, vmCfg, key, parkerNow(cfg))

	desc, marshalErr := renderParkerSentinel(nonBOSH, disks, rawOther)
	if marshalErr != nil {
		return "", pruned, cpierrors.Wrap(marshalErr, "parker provenance: marshal")
	}
	if len(desc) > parkerDescriptionBudget {
		return "", pruned, fmt.Errorf(
			"parker provenance: parker vmid %d on node %s holds %d live records in %d bytes of description, over the %d budget: %w",
			parkerVMID, node, len(disks), len(desc), parkerDescriptionBudget, ErrProvenanceFull)
	}
	return desc, pruned, nil
}

// parkerProvenanceRoom reports whether parkerVMID can still record a park of
// bareVolid, returning ErrProvenanceFull when it cannot.
//
// The park path needs this ahead of the attach, not after it. A parker with a
// free slot and a full store takes the disk perfectly well, so nothing on the
// attach path refuses it, and the write that follows is best-effort: it logs a
// warning and drops the record. That record is not decoration. It carries the
// disk's CID, its source VM, and its per-disk option overlay, and it is what
// findParkedDiskIntentByStableID reads to resolve a disk whose volume was
// renamed under it. Losing it silently costs more than parking one slot later
// on the next parker, so capacity is checked before the disk moves.
//
// The probe is one config read outside the protection window. A park that wins
// the race between this read and the write still lands on a full store and
// still drops its record; that is the same concurrent-park lost update the
// ParkContext doc already declares acceptable, narrowed to the moments two
// parks target one nearly-full parker.
func parkerProvenanceRoom(
	ctx context.Context, c Client, node string, parkerVMID int, bareVolid string,
	cfg ParkerConfig, pctx ParkContext,
) error {
	vmCfg, err := c.QEMU().Config(ctx, node, parkerVMID)
	if err != nil {
		return cpierrors.Wrap(WrapConfigReadError(err), "parker provenance: capacity probe")
	}
	// The landed slot is unknown before the attach. Probe with the widest slot
	// name a parker can hand out so the estimate is never optimistic.
	widestSlot := fmt.Sprintf("scsi%d", parkerMaxSlots-1)
	entry := buildParkerProvEntry(node, bareVolid, widestSlot, cfg, pctx)
	_, _, projectErr := projectParkerProvenance(vmCfg, node, parkerVMID, parkerProvKey(bareVolid, pctx.StableID), entry, cfg)
	return projectErr
}

// writeParkerProvenance is the strict read-modify-write behind every
// provenance update. Ordinary parks treat a failure as advisory (the
// best-effort wrapper above); the detach-side transfer does not — its intent
// record is the crash-window identity carrier, so the transfer refuses to
// delete the source slot until this write has landed.
//
// Every write collects stale records first. Nothing else does: the only other
// deletion, removeParkerProvenance, removes the single volid it was handed, so
// a disk that leaves a parker by any other route (an out-of-band delete, a
// remove whose config write failed and logged, a director torn down while its
// disks were reaped by hand) leaves its record behind forever. That is how the
// store reaches PVE's description cap by accumulation rather than by load.
func writeParkerProvenance(
	ctx context.Context, c Client, logger *log.Logger,
	node string, parkerVMID int, key string, entry parkerProvEntry, cfg ParkerConfig,
) error {
	vmCfg, err := c.QEMU().Config(ctx, node, parkerVMID)
	if err != nil {
		return cpierrors.Wrap(WrapConfigReadError(err), "parker provenance: config fetch")
	}

	newDesc, pruned, projectErr := projectParkerProvenance(vmCfg, node, parkerVMID, key, entry, cfg)
	if len(pruned) > 0 && logger != nil {
		logger.Info("parker provenance: collected stale parked-disk records",
			log.Int("parker_vmid", parkerVMID),
			log.String("node", node),
			log.Int("collected", len(pruned)),
			log.String("keys", strings.Join(pruned, ",")),
		)
	}
	if projectErr != nil {
		return projectErr
	}

	nodesSvc := c.Nodes()
	if nodesSvc == nil {
		// No nodes service available (e.g. test stub without injection). Skip silently.
		return nil
	}
	vmidStr := fmt.Sprintf("%d", parkerVMID)
	if updateErr := nodesSvc.UpdateQemuConfig(ctx, node, vmidStr, &sdknodes.UpdateQemuConfigParams{
		Description: &newDesc,
	}); updateErr != nil {
		return cpierrors.Wrap(WrapMutationError(updateErr), "parker provenance: UpdateQemuConfig")
	}
	return nil
}

// removeParkerProvenance removes the bareVolid entry from the parker VM
// description sentinel. When no entry exists, no API call is made.
//
// Best-effort: any failure is logged at WARN and the function returns nil.
func removeParkerProvenance(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, bareVolid string, _ ParkerConfig) {
	vmidStr := fmt.Sprintf("%d", parkerVMID)

	// Detached and bounded: this runs after the volume has already left the
	// parker, so a context stopped by the protection window's deadline would
	// leave a provenance entry naming a volume that is no longer there -- the
	// stale record disk-audit would then report against every future scan.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), parkerProvenanceRemoveTimeout)
	defer cancel()

	vmCfg, err := c.QEMU().Config(ctx, node, parkerVMID)
	if err != nil {
		if logger != nil {
			logger.Warn("parker provenance: config fetch failed on remove — provenance not removed",
				log.Int("parker_vmid", parkerVMID),
				log.String("node", node),
				log.String("volid", bareVolid),
				log.Err(err),
			)
		}
		return
	}

	currentDesc := DescriptionFromConfig(vmCfg)

	nonBOSH, disks, rawOther := parseParkerSentinel(currentDesc)

	// Match both keyings: legacy entries are keyed by the bare volid; stable-ID
	// entries are keyed by the bpd- token and name the volid in their Volid
	// field. The caller only ever knows the volid it just moved off the parker,
	// and that matches exactly one entry under either scheme.
	removed := false
	for key, entry := range disks {
		if key == bareVolid || entry.Volid == bareVolid {
			delete(disks, key)
			removed = true
		}
	}
	// Absent entry — nothing to remove; skip the API call.
	if !removed {
		return
	}

	newDesc, marshalErr := renderParkerSentinel(nonBOSH, disks, rawOther)
	if marshalErr != nil {
		if logger != nil {
			logger.Warn("parker provenance: marshal failed on remove — provenance not removed",
				log.Int("parker_vmid", parkerVMID),
				log.String("volid", bareVolid),
				log.Err(marshalErr),
			)
		}
		return
	}

	nodesSvc := c.Nodes()
	if nodesSvc == nil {
		// No nodes service available (e.g. test stub without injection). Skip silently.
		return
	}
	updateErr := nodesSvc.UpdateQemuConfig(ctx, node, vmidStr, &sdknodes.UpdateQemuConfigParams{
		Description: &newDesc,
	})
	if updateErr != nil {
		if logger != nil {
			logger.Warn("parker provenance: UpdateQemuConfig failed on remove — provenance not removed",
				log.Int("parker_vmid", parkerVMID),
				log.String("node", node),
				log.String("volid", bareVolid),
				log.Err(updateErr),
			)
		}
	}
}

// parkerTagSanitizeRe removes characters that PVE rejects in tag values.
// PVE tags allow [a-zA-Z0-9._-]; anything else is stripped.
var parkerTagSanitizeRe = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// sanitizeParkerTagValue strips PVE-illegal tag characters from v. Returns ""
// when the sanitized result is empty (so callers can omit the tag rather than
// emitting a blank one).
func sanitizeParkerTagValue(v string) string {
	return parkerTagSanitizeRe.ReplaceAllString(v, "")
}

// parkerVMName returns the canonical name for a parker VM with the given VMID.
func parkerVMName(vmid int) string {
	return fmt.Sprintf("bosh-parker-%d", vmid)
}

// buildParkerTags returns the semicolon-joined tag string for a new parker VM.
// CpiOwnershipTag ("bosh-cpi") is always first so operators can filter all
// CPI-managed guests by a single tag. ParkerTag follows; "director--<id>" is
// appended when DirectorID is set and sanitizes to a non-empty value (mirrors
// stemcell provenance pattern).
func buildParkerTags(cfg ParkerConfig) string {
	tags := []string{CpiOwnershipTag, ParkerTag}
	if dt := parkerDirectorTag(cfg.DirectorID); dt != "" {
		tags = append(tags, dt)
	}
	return strings.Join(tags, ";")
}

// parkerDirectorTagPrefix marks the tag that attributes a parker to the
// director that created it.
const parkerDirectorTagPrefix = "director--"

// parkerDirectorTag returns the attribution tag for a director ID, or "" when
// there is no ID or nothing survives sanitizing.
func parkerDirectorTag(directorID string) string {
	if directorID == "" {
		return ""
	}
	sd := sanitizeParkerTagValue(directorID)
	if sd == "" {
		return ""
	}
	return parkerDirectorTagPrefix + sd
}

// parkerBelongsToDirector reports whether a parker carrying tagStr may be
// adopted by the director identified by directorID.
//
// Two directors sharing one PVE cluster is a supported configuration, and each
// writes its own attribution tag, but every parker lookup matched on the
// bosh-parker tag alone — so the first director to park a disk created a parker
// the second one then filled with its own disks. That is not corruption, but it
// couples the two deployments: neither can be torn down without reasoning about
// the other's volumes, which is exactly what the attribution tag exists to
// prevent.
//
// A parker with no attribution tag is adoptable by anyone. Parkers created
// before the tag existed, or by a configuration with no director UUID to hand,
// carry none, and refusing them would strand their disks.
func parkerBelongsToDirector(tagStr, directorID string) bool {
	want := parkerDirectorTag(directorID)
	if want == "" {
		return true
	}
	// Every attribution tag is considered, not just the first. A parker carrying
	// two of them (an operator edit, or a rename mid-flight) would otherwise be
	// adopted or refused on tag order, which is not a property anything should
	// depend on. Any match means ours.
	sawAttribution := false
	for _, t := range splitTagString(tagStr) {
		if !strings.HasPrefix(strings.ToLower(t), parkerDirectorTagPrefix) {
			continue
		}
		sawAttribution = true
		if strings.EqualFold(t, want) {
			return true
		}
	}
	return !sawAttribution
}

// tagContainsParker reports whether a PVE tag string contains ParkerTag as a
// whole token. Comparison is case-insensitive to tolerate PVE tag
// normalization.
//
// All three of PVE's separators are accepted — its pve-tag-list format is
// `tag(?:[;, ]tag)*` — matching what PVE's own API accepts on the way in and
// what handlers.parseTagsField already assumed on the way out. Splitting on a
// space cannot produce a false positive, because a legal PVE tag contains none. Splitting on ";" alone was survivable while the tag was one signal
// among several; it is not now that this string is what stands between
// delete_vm's skiplock+purge and a parker holding up to 31 other deployments'
// disks. A tag string this function fails to tokenize is a guard that silently
// does not fire.
func tagContainsParker(tagStr string) bool {
	for _, t := range splitTagString(tagStr) {
		if strings.EqualFold(t, ParkerTag) {
			return true
		}
	}
	return false
}

// splitTagString tokenizes a PVE tag string on either separator, dropping empty
// entries and surrounding space. One tokenizer for every parker tag decision.
func splitTagString(tagStr string) []string {
	parts := strings.FieldsFunc(tagStr, func(r rune) bool { return r == ';' || r == ',' || r == ' ' })
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// TagsMarkParker reports whether a PVE tag string carries the bosh-parker tag,
// independent of any VMID band. It exists for diagnostics: a VM outside the
// configured band that is nonetheless tagged as a parker is the signature of a
// band that was moved or unset while disks were still parked, and saying so is
// far more useful than reporting the volume as attached to a stranger.
func TagsMarkParker(tags string) bool {
	return tagContainsParker(tags)
}

// IsParkerVM reports whether vmid is a parker VM: both the VMID must fall
// within [cfg.VMIDRangeStart, cfg.VMIDRangeEnd] and tags must contain
// ParkerTag. The range check is fast and avoids a config read; the tag check
// confirms the VM was created as a parker (not an unrelated VM that happens
// to occupy a VMID in the range).
func IsParkerVM(vmid int, tags string, cfg ParkerConfig) bool {
	if vmid < cfg.VMIDRangeStart || vmid > cfg.VMIDRangeEnd {
		return false
	}
	return tagContainsParker(tags)
}

// FindParkerForNode scans cluster VM resources and returns the LOWEST parker
// VMID on node. It does NOT check slot capacity — the returned parker may be
// full. Callers that need a parker with a free slot (e.g. parkDiskOnNode) must
// use ListParkersForNode and iterate, attaching to the first parker that
// accepts the disk. EnsureParker uses this only to detect whether any parker
// already exists before creating one.
//
// Returns (vmid, true, nil) when at least one parker exists; (0, false, nil)
// when none found. Returns (0, false, err) on transport failure.
func FindParkerForNode(ctx context.Context, c Client, node string, cfg ParkerConfig) (int, bool, error) {
	parkers, err := ListParkersForNode(ctx, c, node, cfg)
	if err != nil {
		return 0, false, err
	}
	if len(parkers) == 0 {
		return 0, false, nil
	}
	return parkers[0], true, nil
}

// ListParkersForNode returns all parker VMIDs on node in ascending VMID order.
//
// One GET /nodes/<node>/qemu request carries the vmid and the tag string of
// every guest on the node, which is everything the band check and the
// parker-tag check need, and it is served from the node's own pmxcfs view
// with none of the /cluster/resources index lag. A row in the band whose
// tags are empty gets one config read to settle the question, since an empty
// field means either an untagged VM or a PVE that does not populate it there.
//
// An earlier shape walked the band VMID by VMID and issued a fresh cluster
// listing plus a config read for each occupied one — 1+2K requests where K is
// the number of guests anywhere in the band. That is on the detach path, and an
// operator whose cluster already used 9xxxx VMIDs before upgrading would have
// paid it on every single detach.
//
// When cfg.DirectorID is set, a parker attributed to a DIFFERENT director is
// skipped: this list drives which parker a park adopts, and two directors
// sharing a cluster should not fill each other's parkers. Unattributed parkers
// remain adoptable — see parkerBelongsToDirector. Lookups that have to find a
// disk wherever it sits (IsDiskParked, UnparkDisk) deliberately do not filter,
// so a disk parked under one attribution is still reachable under another.
//
// Returns an empty slice (not an error) when no parkers exist.
func ListParkersForNode(ctx context.Context, c Client, node string, cfg ParkerConfig) ([]int, error) {
	if c == nil {
		return nil, cpierrors.Cloud("ListParkersForNode: client must not be nil")
	}
	if node == "" {
		return nil, cpierrors.Cloud("ListParkersForNode: node must not be empty")
	}
	if cfg.VMIDRangeStart <= 0 || cfg.VMIDRangeEnd <= cfg.VMIDRangeStart {
		return nil, cpierrors.Cloud("ListParkersForNode: invalid VMID range [%d, %d]",
			cfg.VMIDRangeStart, cfg.VMIDRangeEnd)
	}

	// The node's own qemu listing, not the /cluster/resources index: the
	// listing is served from the node's pmxcfs view with no index lag (a
	// parker created moments ago by a concurrent park is visible), it
	// answers with QEMU guests only (no LXC rows to filter), and every row
	// belongs to this node by construction (no empty-node fallback needed).
	var raws []json.RawMessage
	listErr := RetryOnTransient(ctx, nil, "list_parkers_for_node", 0, func() error {
		resp, inner := c.Nodes().ListQemu(ctx, node, nil)
		if inner != nil {
			return inner
		}
		raws = nil
		if resp != nil {
			raws = *resp
		}
		return nil
	})
	if listErr != nil {
		return nil, cpierrors.Wrap(WrapConfigReadError(listErr), "ListParkersForNode: list node guests")
	}

	var result []int
	for _, raw := range raws {
		var entry qemuListItem
		if jsonErr := json.Unmarshal(raw, &entry); jsonErr != nil {
			continue
		}
		vmid := int(entry.Vmid.Int())
		if vmid < cfg.VMIDRangeStart || vmid > cfg.VMIDRangeEnd {
			continue
		}
		tags := ""
		if entry.Tags != nil {
			tags = *entry.Tags
		}
		if tags == "" {
			// The row carries no tags. That is either a genuinely untagged VM
			// sharing the band, or a PVE that does not populate the field, and
			// the two are indistinguishable from here. Confirm with a config
			// read rather than assume: assuming "not a parker" would create a
			// fresh parker on every park and exhaust the band.
			vmCfg, cfgErr := c.QEMU().Config(ctx, node, vmid)
			if cfgErr != nil {
				if parkerConfigGone(cfgErr) {
					continue
				}
				// WrapError, not a blanket retriable: a 403 on this read is a
				// grant only a human can add, and re-driving every detach on the
				// node forever does not add it.
				return nil, cpierrors.Wrap(WrapConfigReadError(cfgErr),
					fmt.Sprintf("ListParkersForNode: config fetch for vmid %d", vmid))
			}
			tags, _ = ConfigString(vmCfg, "tags")
		}
		// A mover carries the parker tag too (every parker guard must fire
		// for it), but it is a single-purpose migration vehicle: parking an
		// unrelated disk onto one would carry that disk along with the next
		// mover migration. Movers are never park targets.
		if TagsMarkDiskMover(tags) {
			continue
		}
		if tagContainsParker(tags) && parkerBelongsToDirector(tags, cfg.DirectorID) {
			result = append(result, vmid)
		}
	}
	sort.Ints(result)
	return result, nil
}

// createParkerVM allocates a VMID in cfg's parker range and creates a fresh
// parker VM there, retrying past VMID conflicts by re-scanning and adopting
// whichever parker won the race. It is shared by EnsureParker (called after
// its existing-parker short-circuit) and EnsureFreshParker (called directly,
// since its whole contract is "always allocate a NEW parker" — it must never
// short-circuit on an existing one).
//
// The parker VM is created with:
//   - name: "bosh-parker-<vmid>"
//   - onboot: 0 (never auto-started)
//   - protection: 1 (prevent accidental deletion)
//   - tags: "bosh-parker[;director--<id>]"
//   - scsihw: "virtio-scsi-pci"
//   - memory: 16 MiB
//   - cores: 1
//   - no NIC, no disk
//
// opLabel identifies the caller in RetryOnTransientOrLock's log lines and in
// wrapped error messages (e.g. "ensure_parker_create", "ensure_fresh_parker_create").
//
// Validates cfg's VMID range before allocating: an invalid range (start<=0 or
// end<=start) fails loudly here rather than letting WithRange silently ignore
// it and AllocateWithRetry fall back to the general VM range [100,8999].
func createParkerVM(ctx context.Context, c Client, logger *log.Logger, node string, cfg ParkerConfig, opLabel string) (int, error) {
	if cfg.VMIDRangeStart <= 0 || cfg.VMIDRangeEnd <= cfg.VMIDRangeStart {
		return 0, cpierrors.Cloud("%s: invalid VMID range [%d, %d]",
			opLabel, cfg.VMIDRangeStart, cfg.VMIDRangeEnd)
	}

	tags := buildParkerTags(cfg)
	protection := 1
	onboot := 0
	memory := 16
	cores := 1
	scsihw := "virtio-scsi-pci"

	vmid, createErr := AllocateWithRetry(
		ctx,
		c,
		func(vmid int) error {
			params := map[string]any{
				cfgKeyVMID:      vmid,
				cfgKeyName:      parkerVMName(vmid),
				cfgKeyTags:      tags,
				paramProtection: protection,
				"onboot":        onboot,
				"memory":        memory,
				"cores":         cores,
				"scsihw":        scsihw,
			}
			var upid string
			var innerErr error
			retryErr := RetryOnTransientOrLock(ctx, logger, opLabel, 0, func() error {
				upid, innerErr = c.QEMU().Create(ctx, node, params)
				return innerErr
			})
			if retryErr != nil {
				return retryErr
			}
			if upid != "" {
				if awaitErr := AwaitTask(ctx, c, node, upid); awaitErr != nil {
					// Classified: AwaitTask reports a task PVE rejected outright
					// ("task failed: exit status ...") the same way it reports a
					// poll that timed out, and only the second is worth retrying.
					return cpierrors.Wrap(WrapMutationError(awaitErr),
						fmt.Sprintf("%s: await create task for vmid %d", opLabel, vmid))
				}
			}
			return nil
		},
		// nil conflict predicate + single attempt: a VMID conflict means another
		// CPI created a parker at this VMID. We do NOT want AllocateWithRetry to
		// regenerate a fresh VMID (that would create a duplicate parker); instead
		// we surface the conflict so the branch below re-scans and ADOPTS the
		// winner (PC-4). Transient/lock errors are still retried inside the create
		// closure via RetryOnTransientOrLock.
		nil,
		1,
		WithRange(cfg.VMIDRangeStart, cfg.VMIDRangeEnd),
		WithNoBackoff(),
		WithStorageScan(node, cfg.DiskStorage),
	)
	if createErr != nil {
		// Create-conflict path: another CPI won the race. Re-find and adopt.
		if IsVMIDConflict(createErr) {
			winner, found, findErr := FindParkerForNode(ctx, c, node, cfg)
			if findErr != nil {
				return 0, findErr
			}
			if found {
				return winner, nil
			}
			return 0, cpierrors.Retriable("%s: VMID conflict but no parker found after re-scan on node %q", opLabel, node)
		}
		return 0, cpierrors.Wrap(createErr, fmt.Sprintf("%s: create parker VM", opLabel))
	}
	return vmid, nil
}

// EnsureParker returns a parker VMID for node, creating one if none exists.
// See createParkerVM for the VM's creation parameters and conflict-adoption
// behavior.
//
// Returns the parker VMID on success.
func EnsureParker(ctx context.Context, c Client, logger *log.Logger, node string, cfg ParkerConfig) (int, error) {
	if c == nil {
		return 0, cpierrors.Cloud("EnsureParker: client must not be nil")
	}
	if node == "" {
		return 0, cpierrors.Cloud("EnsureParker: node must not be empty")
	}

	// Check for existing parker first.
	existing, found, err := FindParkerForNode(ctx, c, node, cfg)
	if err != nil {
		return 0, err
	}
	if found {
		return existing, nil
	}

	return createParkerVM(ctx, c, logger, node, cfg, "ensure_parker_create")
}

// EnsureFreshParker is like EnsureParker but specifically allocates a new
// parker VM distinct from any existing parkers (used when all existing parkers
// are full). It intentionally skips the existing-parker short-circuit — the
// range-validation guard and create/allocate/await logic live in the shared
// createParkerVM helper.
func EnsureFreshParker(ctx context.Context, c Client, logger *log.Logger, node string, cfg ParkerConfig) (int, error) {
	if c == nil {
		return 0, cpierrors.Cloud("EnsureFreshParker: client must not be nil")
	}
	if node == "" {
		return 0, cpierrors.Cloud("EnsureFreshParker: node must not be empty")
	}

	return createParkerVM(ctx, c, logger, node, cfg, "ensure_fresh_parker_create")
}

// diskHolder is the outcome of one cluster-wide search for whichever VM holds a
// volid. slot is set only when the holder is a parker and the disk was located
// in its config.
type diskHolder struct {
	found    bool
	vmid     int
	node     string
	isParker bool
	slot     string
	// tags is the holder's raw tag string as the scan read it. It carries the
	// answer to "what kind of VM is this?" out of the scan, so a caller that
	// finds a holder outside the parker band can still tell a stranded parker
	// from an ordinary VM without a second config read.
	tags string
}

// resolveDiskHolder answers "who holds this volid, and is it a parker?" in a
// single pass: one cluster-wide scan plus at most one config read.
//
// It exists because that scan is the expensive operation in the whole parked
// lifecycle — FindVMByDiskVolid reads the config of every VM in the cluster and
// cannot short-circuit for a free-floating disk, which is exactly the state a
// just-detached disk is in. IsDiskParked and DiskHeldByRealVM ask two questions
// of the same answer, so ParkDisk resolves the holder once and reads both from
// it rather than paying for the sweep twice on every detach.
func resolveDiskHolder(ctx context.Context, c Client, logger *log.Logger, bareVolid string, cfg ParkerConfig) (diskHolder, error) {
	holderVMID, holderNode, holderTags, found, err := FindVMByDiskVolidOrNoneTagged(ctx, c, bareVolid)
	if err != nil {
		return diskHolder{}, err
	}
	if !found {
		return diskHolder{}, nil
	}

	// Out of the parker band → a real VM, no config read needed. The tags come
	// from the scan, so a caller that needs to tell a stranded parker from an
	// ordinary VM can do it without a second read.
	if holderVMID < cfg.VMIDRangeStart || holderVMID > cfg.VMIDRangeEnd {
		return diskHolder{found: true, vmid: holderVMID, node: holderNode, tags: holderTags}, nil
	}

	vmCfg, cfgErr := c.QEMU().Config(ctx, holderNode, holderVMID)
	if cfgErr != nil {
		if parkerConfigGone(cfgErr) {
			// Holder vanished between the scan and the read. The cluster
			// listing said a VM inside the parker band references the volume,
			// and now that VM has no config: the CPI never deletes parkers, so
			// under the strict invariant this is an out-of-band deletion, not
			// a benign race, and proceeding as free-floating would run against
			// a volume whose anchor vanished.
			if cfg.AnchorStrict && cfg.ParkedEnabled {
				return diskHolder{}, cpierrors.Cloud(
					"disk holder vmid %d (node %s) sits inside the parker band [%d,%d] but its config "+
						"vanished mid-scan; a parker VM holding %s was likely deleted out-of-band. Verify the "+
						"volume is intact, then set pve.parked_anchor_strict: false to treat it as "+
						"free-floating and retry",
					holderVMID, holderNode, cfg.VMIDRangeStart, cfg.VMIDRangeEnd, bareVolid,
				)
			}
			// Permissive: the disk is free-floating as far as this call is
			// concerned.
			return diskHolder{}, nil
		}
		// WrapError keeps a 403 permanent: it names a grant to add, and no
		// number of retries adds it.
		return diskHolder{}, cpierrors.Wrap(WrapConfigReadError(cfgErr),
			fmt.Sprintf("config fetch for vmid %d on node %s", holderVMID, holderNode))
	}

	tagsRaw, _ := ConfigString(vmCfg, "tags")
	if !tagContainsParker(tagsRaw) {
		if logger != nil {
			// Under "parked" a workload VM inside the parker band is surprising
			// (overlap validation should have rejected the config). Under "free"
			// or a stood-down default the band can legitimately overlap
			// vmid_range, so the same state is routine — keep it at debug.
			logUntagged := logger.Debug
			if cfg.ParkedEnabled {
				logUntagged = logger.Warn
			}
			logUntagged("disk holder is in the parker range but carries no bosh-parker tag — treating it as a real VM",
				log.Int("vmid", holderVMID),
				log.String("node", holderNode),
				log.String("volid", bareVolid),
				log.String("tags", tagsRaw),
			)
		}
		return diskHolder{found: true, vmid: holderVMID, node: holderNode, tags: tagsRaw}, nil
	}

	slot, _ := FindDiskIDByVolID(qemu.ParseDisks(vmCfg), bareVolid)
	return diskHolder{found: true, vmid: holderVMID, node: holderNode, isParker: true, slot: slot, tags: tagsRaw}, nil
}

// IsDiskParked reports whether bareVolid is currently held on a parker VM.
//
// Algorithm:
//  1. Cluster-wide holder scan. Not found → false,nil.
//  2. Holder VMID not in [VMIDRangeStart, VMIDRangeEnd] → false,nil (no config read).
//  3. One config read: check bosh-parker tag. Missing → false (logged at WARN
//     when cfg.ParkedEnabled, debug otherwise).
//  4. Find slot via pve.FindDiskIDByVolID. Slot miss → retriable (config listed it).
//
// Returns (vmid, node, slot, parked, err).
func IsDiskParked(ctx context.Context, c Client, logger *log.Logger, bareVolid string, cfg ParkerConfig) (int, string, string, bool, error) {
	if c == nil {
		return 0, "", "", false, cpierrors.Cloud("IsDiskParked: client must not be nil")
	}
	if bareVolid == "" {
		return 0, "", "", false, cpierrors.Cloud("IsDiskParked: bareVolid must not be empty")
	}

	holder, err := resolveDiskHolder(ctx, c, logger, bareVolid, cfg)
	if err != nil {
		return 0, "", "", false, cpierrors.Wrap(err, "IsDiskParked: cluster scan")
	}
	return parkedFromHolder(holder, bareVolid)
}

// parkedFromHolder converts a resolved holder into IsDiskParked's return shape.
// A parker holder whose slot could not be located is unexpected — the scan just
// confirmed the disk is on that VM — so it is reported as a transient
// config-read race rather than "not parked".
func parkedFromHolder(holder diskHolder, bareVolid string) (int, string, string, bool, error) {
	if !holder.found || !holder.isParker {
		return 0, "", "", false, nil
	}
	if holder.slot == "" {
		return 0, "", "", false, cpierrors.Retriable(
			"IsDiskParked: disk %q confirmed on parker vmid %d but slot not found in config (possible race)",
			bareVolid, holder.vmid,
		)
	}
	return holder.vmid, holder.node, holder.slot, true, nil
}

// DiskHeldByRealVM reports whether bareVolid is currently attached to a VM that
// is NOT a parker (a workload VM, stemcell template, or any VMID outside the
// parker range, or an in-range VMID lacking the bosh-parker tag).
//
// It exists because IsDiskParked collapses "held by a non-parker VM" and "not
// held at all" into the same (false, nil) result, discarding a signal the park
// path needs: a disk still referenced by a running VM must never be parked, or
// PVE config would reference the same volume from two VMs (double-attach).
//
// Returns:
//   - held=true, vmid>0 when a non-parker VM holds the disk.
//   - held=false, vmid=0 when the disk is free-floating or held by a parker.
//   - err on transport/scan failure.
func DiskHeldByRealVM(ctx context.Context, c Client, logger *log.Logger, bareVolid string, cfg ParkerConfig) (held bool, vmid int, node string, err error) {
	if c == nil {
		return false, 0, "", cpierrors.Cloud("DiskHeldByRealVM: client must not be nil")
	}
	if bareVolid == "" {
		return false, 0, "", cpierrors.Cloud("DiskHeldByRealVM: bareVolid must not be empty")
	}

	holder, scanErr := resolveDiskHolder(ctx, c, logger, bareVolid, cfg)
	if scanErr != nil {
		return false, 0, "", cpierrors.Wrap(scanErr, "DiskHeldByRealVM: cluster scan")
	}
	return realHolder(holder)
}

// realHolder converts a resolved holder into DiskHeldByRealVM's return shape:
// anything found that is not a parker is a real VM.
func realHolder(holder diskHolder) (held bool, vmid int, node string, err error) {
	if !holder.found || holder.isParker {
		return false, 0, "", nil
	}
	return true, holder.vmid, holder.node, nil
}

// DiskHolder describes the VM that currently references a volume, in the exported
// shape callers outside this package need.
type DiskHolder struct {
	// Found is false when no VM in the cluster references the volume.
	Found bool
	// VMID and Node identify the holder when Found is true.
	VMID int
	Node string
	// IsParker is true when the holder is a bosh-parker VM inside the
	// configured band. The band resolves to the built-in one under every
	// strategy, so a false here for a tagged parker means the band was moved
	// away from it; such a parker is reported as an ordinary holder rather
	// than skipped.
	IsParker bool
	// Slot is the parker's scsiN key holding the volume; set only for parkers.
	Slot string
	// Tags is the holder's raw tag string, from the same scan. A holder that is
	// not IsParker but whose tags mark a parker is one the configured band no
	// longer covers -- the state every stranded-parker refusal keys on.
	Tags string
}

// ResolveDiskHolder answers "who holds this volid, and is it a parker?" with one
// cluster-wide scan plus at most one config read.
//
// attach_disk needs both halves of that answer before it writes a volid into a
// VM config: a parker holder must be unparked first, and a non-parker holder
// means the volume is already attached somewhere else and attaching it again
// would leave two VM configs referencing one volume — a state PVE permits and
// nothing later detects, until whichever holder is destroyed takes the volume
// with it.
func ResolveDiskHolder(ctx context.Context, c Client, logger *log.Logger, bareVolid string, cfg ParkerConfig) (DiskHolder, error) {
	if c == nil {
		return DiskHolder{}, cpierrors.Cloud("ResolveDiskHolder: client must not be nil")
	}
	if bareVolid == "" {
		return DiskHolder{}, cpierrors.Cloud("ResolveDiskHolder: bareVolid must not be empty")
	}
	h, err := resolveDiskHolder(ctx, c, logger, bareVolid, cfg)
	if err != nil {
		return DiskHolder{}, cpierrors.Wrap(err, "ResolveDiskHolder: cluster scan")
	}
	return DiskHolder{
		Found: h.found, VMID: h.vmid, Node: h.node, IsParker: h.isParker, Slot: h.slot, Tags: h.tags,
	}, nil
}

// ParkedFromHolder answers IsDiskParked's question from a DiskHolder a caller
// has already resolved, so a code path that needs both "is it parked?" and "is
// a real VM holding it?" pays one cluster scan instead of two. The return shape
// and the empty-slot race handling match IsDiskParked exactly.
func ParkedFromHolder(h DiskHolder, bareVolid string) (vmid int, node, slot string, parked bool, err error) {
	return parkedFromHolder(diskHolder{
		found:    h.Found,
		vmid:     h.VMID,
		node:     h.Node,
		isParker: h.IsParker,
		slot:     h.Slot,
		tags:     h.Tags,
	}, bareVolid)
}

// ParkDisk attaches bareVolid to a parker VM on node. It is idempotent: if the
// disk is already parked on any parker VM the call returns nil immediately.
//
// pctx carries optional per-call attribution written into the provenance record.
// Pass a zero ParkContext when the source VM or full disk CID are unavailable.
//
// The algorithm:
//  1. One cluster-wide holder scan, read two ways: already parked -> nil, held
//     by a real VM -> refuse (parking would double-reference a live volume).
//  2. List the parkers on node and attach to the lowest one with a free slot,
//     creating a parker only when none exists or all are full.
//  3. AttachDisk with an explicit DiskID (scsiN), then re-read to confirm the
//     slot holds this volume -- a concurrent park can win the same slot.
//  4. Re-assert protection=1 and write the provenance record.
//
// All PVE mutations are wrapped with RetryOnTransientOrLock.
func ParkDisk(ctx context.Context, c Client, logger *log.Logger, node, bareVolid string, cfg ParkerConfig, pctx ParkContext) error {
	if c == nil {
		return cpierrors.Cloud("ParkDisk: client must not be nil")
	}
	if node == "" {
		return cpierrors.Cloud("ParkDisk: node must not be empty")
	}
	if bareVolid == "" {
		return cpierrors.Cloud("ParkDisk: bareVolid must not be empty")
	}

	// One holder scan answers both questions below. The scan reads every VM
	// config in the cluster, so resolving it twice would double the cost of the
	// detach path that now runs by default.
	//
	// The disk's own node is the fallback for a cluster-resources row that
	// elides "node": on a single-node PVE that is every row, and skipping them
	// would report the volume free while the workload VM still holds it, which
	// is precisely what the refusal below exists to catch.
	if cfg.FallbackNode == "" {
		cfg.FallbackNode = node
	}
	holder, scanErr := resolveDiskHolder(ctx, c, logger, bareVolid, cfg)
	if scanErr != nil {
		return cpierrors.Wrap(scanErr, "ParkDisk: holder scan")
	}

	// Idempotency: already parked → nil.
	_, _, _, alreadyParked, parkedErr := parkedFromHolder(holder, bareVolid)
	if parkedErr != nil {
		return cpierrors.Wrap(parkedErr, "ParkDisk: idempotency check")
	}
	if alreadyParked {
		return nil
	}

	// Refuse to park a disk still attached to a real (non-parker) VM. Parking it
	// would add a second config reference to a volume a running VM owns
	// (double-attach). This guards a stale-Director-retry path where the disk
	// was re-attached elsewhere between the failed detach and this re-park.
	heldByReal, realVMID, _, _ := realHolder(holder)
	if heldByReal {
		if logger != nil {
			logger.Warn("ParkDisk: disk attached to a non-parker VM — refusing to park (idempotent no-op)",
				log.String("volid", bareVolid),
				log.Int("holder_vmid", realVMID),
			)
		}
		return nil
	}

	return parkDiskOnNode(ctx, c, logger, node, bareVolid, cfg, pctx)
}

// parkDiskOnNode performs the actual attach to a parker VM on node. Separated
// from ParkDisk so the EnsureFreshParker overflow path stays out of the
// idempotency pre-check.
//
// Capacity reuse: it lists every parker on node in ascending VMID order
// and attaches to the FIRST parker with a free slot. A fresh parker is created
// only when all existing parkers are full (or none exist). This prevents the
// parker-per-disk leak where each overflow disk would otherwise spawn a new
// parker VM, exhausting the VMID band.
func parkDiskOnNode(ctx context.Context, c Client, logger *log.Logger, node, bareVolid string, cfg ParkerConfig, pctx ParkContext) error {
	parkers, listErr := ListParkersForNode(ctx, c, node, cfg)
	if listErr != nil {
		return cpierrors.Wrap(listErr, "ParkDisk: list parkers")
	}

	// No parker exists yet → create the first one and attach there.
	if len(parkers) == 0 {
		parkerVMID, ensureErr := EnsureParker(ctx, c, logger, node, cfg)
		if ensureErr != nil {
			return cpierrors.Wrap(ensureErr, "ParkDisk: ensure parker")
		}
		if attachErr := attachAndSecure(ctx, c, logger, node, parkerVMID, bareVolid, cfg, pctx); attachErr != nil {
			return cpierrors.Wrap(attachErr, "ParkDisk: attach to parker")
		}
		return nil
	}

	// Try existing parkers in ascending VMID order. Attach to the first one
	// with a free slot. ErrNoSlots on a parker means it is full — move to the
	// next. Any other error is a real failure and propagates.
	for _, parkerVMID := range parkers {
		attachErr := attachAndSecure(ctx, c, logger, node, parkerVMID, bareVolid, cfg, pctx)
		if attachErr == nil {
			return nil
		}
		if isParkerCapacityError(attachErr) {
			continue
		}
		return cpierrors.Wrap(attachErr, "ParkDisk: attach to parker")
	}

	// All existing parkers are full → create fresh ones until one takes the
	// disk. EnsureFreshParker's VMID-conflict branch adopts the lowest existing
	// parker, which at this point is by definition full, so a single attempt can
	// come back ErrNoSlots on a parker nobody can use. That is a capacity
	// condition with an obvious next step -- allocate another -- not a reason to
	// fail the detach permanently, which is what wrapping ErrNoSlots as a Cloud
	// error used to do the moment a node's parkers filled up.
	var freshErr error
	for attempt := 0; attempt < freshParkerAttempts; attempt++ {
		var freshVMID int
		freshVMID, freshErr = EnsureFreshParker(ctx, c, logger, node, cfg)
		if freshErr != nil {
			return cpierrors.Wrap(freshErr, "ParkDisk: ensure fresh parker after all parkers full")
		}
		attachErr := attachAndSecure(ctx, c, logger, node, freshVMID, bareVolid, cfg, pctx)
		if attachErr == nil {
			return nil
		}
		if !isParkerCapacityError(attachErr) {
			return cpierrors.Wrap(attachErr, "ParkDisk: attach to fresh parker")
		}
		if logger != nil {
			logger.Warn("parker: freshly ensured parker had no capacity for the disk; allocating another",
				log.Int("parker_vmid", freshVMID),
				log.String("node", node),
				log.Int("attempt", attempt+1),
			)
		}
	}
	return cpierrors.Retriable(
		"ParkDisk: could not find a parker on node %s with both a free slot and room in its provenance store, "+
			"after %d fresh-parker attempts",
		node, freshParkerAttempts)
}

// parkerWindowMaxAttempts bounds every retry loop that runs inside a parker's
// protection window. The window is guarded by a cluster lock with a TTL
// (parkerProtectionLockTTL), and a later acquirer steals a lock whose TTL has
// expired -- so a call that retries past the TTL does not just run slowly, it
// runs on past the point where another park or unpark is entitled to enter the
// same window. The storage-lock curve alone would sleep past two minutes over
// its default ten attempts, before the calls around it. Four attempts keeps the
// worst case at roughly ten seconds of backoff per call, and a genuinely stuck
// storage layer is better handed back to the Director than retried under a lock
// we are about to lose.
//
// This bounds each loop; the window deadline in withParkerProtectionLock bounds
// their sum. Both are needed: per-loop bounds keep a single stuck call short,
// and the deadline is what a composition of bounded loops cannot exceed.
const parkerWindowMaxAttempts = 4

// freshParkerAttempts bounds the overflow loop. EnsureFreshParker can hand back
// a full parker when its VMID-conflict branch adopts an existing one, so a
// single attempt is not enough; an unbounded loop would spin against a genuinely
// exhausted band.
const freshParkerAttempts = 3

// isParkerCapacityError reports whether err says "this parker cannot take the
// disk, another one can". Two conditions qualify and the park loops treat
// them identically: every bus slot is taken (ErrNoSlots), and the provenance
// store has no room for the record (ErrProvenanceFull). Both are reached
// before anything destructive has run, so moving to the next parker is always
// safe; neither is a reason to fail the detach that asked for the park.
func isParkerCapacityError(err error) bool {
	return errors.Is(err, ErrNoSlots) || errors.Is(err, ErrProvenanceFull)
}

// attachAndSecure performs one park onto a known parker with that parker's
// protection window held: attach, put protection back, record provenance.
//
// The lock is what makes the unpark window safe. An unpark clears protection,
// issues the detach, and PVE demotes the volume to an unusedN key that a second
// request removes; a park writing protection=1 into that gap makes PVE refuse
// the second request and the in-window sweep, leaving the volume referenced by a
// key no probe can see while the unpark reports a retriable failure. Serializing
// the park's protection write against the unpark's window closes that, and it
// also stops a park claiming a slot an unpark is about to detach by name.
func attachAndSecure(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, bareVolid string, cfg ParkerConfig, pctx ParkContext) error {
	// Capacity is both halves: a slot for the volume and room for its record.
	// Checked before the attach so a parker that fails either one is simply
	// the wrong parker, not a disk parked where its provenance cannot follow.
	if roomErr := parkerProvenanceRoom(ctx, c, node, parkerVMID, bareVolid, cfg, pctx); roomErr != nil {
		return roomErr
	}

	var landedSlot string
	if lockErr := withParkerProtectionLock(ctx, c, logger, parkerVMID, "park", func(wctx context.Context) error {
		slot, attachErr := attachToParkerLocked(wctx, c, logger, node, parkerVMID, bareVolid, pctx.StableID)
		if attachErr != nil {
			return attachErr
		}
		landedSlot = slot
		reassertParkerProtection(wctx, c, logger, node, parkerVMID)
		return nil
	}); lockErr != nil {
		return lockErr
	}
	// Provenance is written OUTSIDE the window on purpose. It is advisory
	// metadata for disk-audit whose lost-update race is already declared
	// acceptable (see the ParkContext doc), so serializing it protects nothing
	// that matters -- while a config read plus a description write on a parker
	// carrying up to 31 sentinel entries is real time added to a critical
	// section that must finish inside the lock's TTL.
	updateParkerProvenance(ctx, c, logger, node, parkerVMID, bareVolid, landedSlot, cfg, pctx)
	return nil
}

// attachParkerVerifyRetries bounds the read-after-write slot-verify loop in
// attachToParkerLocked. Each iteration reads config, chooses a free slot, attaches,
// then re-reads to confirm the chosen slot holds our volid. A concurrent park
// that won the same slot demotes our disk to unusedN; on mismatch we retry the
// next free slot. The bound caps how many concurrent parkers we tolerate
// stealing slots before giving up retriably.
const attachParkerVerifyRetries = 5

// attachToParkerLocked reads the current config of parkerVMID, selects the first free
// scsiN slot, calls AttachDisk with an explicit DiskID, then re-reads the
// config to confirm the chosen slot actually holds bareVolid. Returns the
// slot the volume landed on. When stableID is non-empty the attach bakes a
// serial=<stableID> option into the drive entry — the parker slot is the
// disk's identity carrier while it is parked, and a reassignment to a
// workload VM carries the whole option string with it (D13).
//
// Slot-race verify (F-W6-03): the PVE config PUT for an explicit DiskID is
// blind — two concurrent parks can both pick scsi0, and PVE replaces the slot
// for the second writer while demoting the first writer's volume to unusedN
// (silently un-parked, despite the first detach_disk already reporting
// success). After each attach this function re-reads the parker config and
// confirms bareVolid occupies the chosen slot. On mismatch it retries with the
// next free slot (excluding slots already proven stolen), bounded by
// attachParkerVerifyRetries. Exhausting free slots returns ErrNoSlots so the
// caller falls through to a fresh parker; exhausting the retry budget returns a
// retriable error so the Director re-drives the park.
func attachToParkerLocked(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, bareVolid, stableID string) (landedSlot string, err error) {
	// Slots proven to hold someone else's volid after our attach — never retry these.
	stolen := make(map[string]bool)
	// Set once a slot is lost to a concurrent park, which is the moment PVE
	// demotes our volid to an unusedN key on this parker. Every exit from the
	// loop then has to sweep that key, including the two failure exits: leaving
	// it behind strands a reference no holder probe in this package can see, and
	// the caller goes on to park the same volume on a different parker.
	demoted := false
	defer func() {
		if !demoted {
			return
		}
		// Cleanup, so it runs on a detached, bounded context. A cancelled or
		// timed-out park is exactly when the stranded reference matters most,
		// and both the sentinel-pool acquire and the detach it guards would
		// otherwise fail instantly on the dead context and leave the unusedN
		// key behind for good.
		sweepCtx, sweepCancel := context.WithTimeout(context.WithoutCancel(ctx), parkerDemotedSweepTimeout)
		defer sweepCancel()
		// The *Locked* variant: this runs with the parker's protection lock
		// already held by attachAndSecure, and AcquireClusterLock is not
		// reentrant -- taking it again here would wait out its own timeout.
		if sweepParkerUnusedSlotsProtectedLocked(sweepCtx, c, logger, node, parkerVMID, bareVolid) {
			return
		}
		// The reference survived, and it is invisible: every holder probe in
		// this package matches active-bus keys only. Whatever this call was
		// about to return, returning it unchanged sends the caller on to park
		// the same volume somewhere else -- the next slot, the next parker, or
		// the Director's own retry -- while this parker still names it. That is
		// the double reference the sweep exists to prevent, and purging either
		// holder then frees a live volume.
		//
		// So every failing exit is rewritten, not just ErrNoSlots: a permanent
		// unswept-reference error naming the parker and the commands that clear
		// it. Permanent because a retry does not sweep -- it re-runs the holder
		// scan, which cannot see an unusedN key, concludes the volume is free,
		// and parks it again.
		if err != nil {
			reportUnsweptReference(logger, node, parkerVMID, bareVolid, err)
			err = unsweptReferenceErrorFor(
				"ParkDisk", "lost the slot holding", node, parkerVMID, bareVolid, err)
			return
		}
		// The attach succeeded on another slot, so this parker legitimately
		// holds the volume and the stranded key names a volume the same parker
		// already carries -- discoverable, unlike the failure exits. Say so
		// loudly and let the park stand: failing a park that worked would leave
		// the disk free-floating, which is worse.
		reportUnsweptReference(logger, node, parkerVMID, bareVolid,
			cpierrors.Cloud("attachToParker: park succeeded on parker vmid %d but a demoted reference to %q "+
				"survived the sweep", parkerVMID, bareVolid))
	}()

	for attempt := 0; attempt < attachParkerVerifyRetries; attempt++ {
		// Fresh config read for slot selection.
		vmCfg, cfgErr := c.QEMU().Config(ctx, node, parkerVMID)
		if cfgErr != nil {
			// Classified, not forced retriable: a 403 for a missing
			// VM.Audit/VM.Config.Disk grant never comes right on its own, and
			// labelling it retriable makes the Director drive a park forever
			// against a permission that has to be granted by hand.
			return "", cpierrors.Wrap(WrapConfigReadError(cfgErr),
				fmt.Sprintf("attachToParker: config fetch for parker vmid %d", parkerVMID))
		}

		slot, slotErr := chooseParkSlotExcluding(qemu.ParseDisks(vmCfg), stolen)
		if slotErr != nil {
			// A full parker that already names our volume on an unusedN key is
			// not a parker to walk away from. ErrNoSlots sends the caller to the
			// next parker, and attaching the volume there while this one still
			// references it is the double reference the sweep exists to prevent.
			// The config is already in hand, so the check costs nothing.
			//
			// demoted is what makes the deferred sweep run on the way out, and
			// it is the right flag: the reference is exactly the one it clears.
			if unusedEntriesReference(vmCfg, bareVolid) {
				demoted = true
			}
			// ErrNoSlots — caller decides what to do, unless the deferred sweep
			// above finds this parker still references the volume, in which case
			// it converts this into a permanent unswept-reference error.
			return "", slotErr
		}

		// AttachDisk returns the disk key it wrote ("scsi3"), NOT a UPID: the
		// attach is a synchronous config PUT, exactly like every other call
		// site treats it. An earlier version fed that key to AwaitTask, which
		// asked PVE for the status of task "scsi0" and failed every park the
		// first time this code met a real cluster. There is no task to await;
		// the read-after-write below is the completion check.
		// The serial rides in the volid argument: PVE accepts the full drive
		// option string there, and this is the attach boundary — the only
		// point D13 permits writing an identity serial.
		volidArg := bareVolid
		if stableID != "" {
			volidArg = bareVolid + ",serial=" + stableID
		}
		var attachErr error
		retryErr := RetryOnTransientOrLock(ctx, logger, "park_disk_attach", parkerWindowMaxAttempts, func() error {
			_, attachErr = c.QEMU().AttachDisk(ctx, node, parkerVMID, volidArg, "scsi", &qemu.AttachOpts{
				DiskID: slot,
			})
			return attachErr
		})
		if retryErr != nil {
			return "", cpierrors.Wrap(WrapMutationError(retryErr),
				fmt.Sprintf("attachToParker: attach %q at slot %s on parker vmid %d", bareVolid, slot, parkerVMID))
		}

		// Read-after-write: confirm our volid landed in the chosen slot. The
		// protection lock already serializes parks against each other on the
		// happy path, so this is the backstop for the paths where it does not:
		// no pool service, an identity without Pool.Allocate, a lock stolen
		// past its TTL, or an older release parking without a lock at all.
		// Losing the slot is silent otherwise -- PVE demotes our volume to an
		// unusedN key and answers 200.
		verifyCfg, verifyErr := c.QEMU().Config(ctx, node, parkerVMID)
		if verifyErr != nil {
			return "", cpierrors.Wrap(WrapConfigReadError(verifyErr),
				fmt.Sprintf("attachToParker: verify config read for parker vmid %d", parkerVMID))
		}
		if slotHoldsVolid(qemu.ParseDisks(verifyCfg), slot, bareVolid) {
			return slot, nil
		}

		// A concurrent park won this slot. Mark it stolen and retry the next free
		// slot. Our disk was demoted to unusedN by PVE; the next attach with an
		// explicit DiskID at a different slot re-references the same volid.
		stolen[slot] = true
		demoted = true
		if logger != nil {
			logger.Warn("attachToParker: chosen slot lost to concurrent park, retrying next slot",
				log.Int("parker_vmid", parkerVMID),
				log.String("slot", slot),
				log.String("volid", bareVolid),
			)
		}
	}

	return "", cpierrors.Retriable(
		"attachToParker: slot verify failed after %d attempts for %q on parker vmid %d (concurrent park contention)",
		attachParkerVerifyRetries, bareVolid, parkerVMID,
	)
}

// chooseParkSlotExcluding scans the parsed disk map returned by
// qemu.ParseDisks and returns the first free scsiN slot in [scsi0, scsi30],
// skipping any slot in the exclude set (slots proven stolen by a concurrent
// park). A nil exclude set behaves as "exclude nothing". Returns ErrNoSlots
// when no non-excluded free slot remains.
//
// Inputs:
//   - disks: map[diskID]optString from qemu.ParseDisks; nil treated as empty.
//
// Failure modes:
//   - all non-excluded slots in scsi0..scsi30 occupied → ErrNoSlots.
func chooseParkSlotExcluding(disks map[string]string, exclude map[string]bool) (string, error) {
	for i := 0; i < parkerMaxSlots; i++ {
		slot := fmt.Sprintf("scsi%d", i)
		if _, occupied := disks[slot]; occupied {
			continue
		}
		if exclude[slot] {
			continue
		}
		return slot, nil
	}
	return "", ErrNoSlots
}

// slotHoldsVolid reports whether the given slot in the parsed disk map holds
// bareVolid (exact match or option-string "<volid>,..." form). PVE may append
// ",size=..." to the value, so an exact-only check would false-negative.
func slotHoldsVolid(disks map[string]string, slot, bareVolid string) bool {
	v, ok := disks[slot]
	if !ok {
		return false
	}
	return v == bareVolid || strings.HasPrefix(v, bareVolid+",")
}

// UnparkDisk detaches bareVolid from its parker VM. It is idempotent: if the
// disk is not parked the call returns nil.
//
// The algorithm:
//  1. One cluster-wide holder scan -- not held by a parker -> nil.
//  2. Take the parker's protection-window lock.
//  3. Clear protection=1, which PVE otherwise honors by refusing the detach.
//  4. DetachDisk(node, parkerVMID, slot) with RetryOnTransientOrLock, then
//     sweep any unusedN key still naming this volume.
//  5. Restore protection and drop the provenance record.
func UnparkDisk(ctx context.Context, c Client, logger *log.Logger, bareVolid string, cfg ParkerConfig) error {
	if c == nil {
		return cpierrors.Cloud("UnparkDisk: client must not be nil")
	}
	if bareVolid == "" {
		return cpierrors.Cloud("UnparkDisk: bareVolid must not be empty")
	}

	parkerVMID, parkerNode, slot, parked, err := IsDiskParked(ctx, c, logger, bareVolid, cfg)
	if err != nil {
		return cpierrors.Wrap(err, "UnparkDisk: is-parked check")
	}
	if !parked {
		return nil
	}
	return unparkAt(ctx, c, logger, bareVolid, parkerVMID, parkerNode, slot, cfg)
}

// UnparkDiskAt detaches bareVolid from a parker whose location the caller has
// already resolved, skipping the cluster-wide scan UnparkDisk performs.
//
// It exists for callers that had to resolve the holder for their own reasons
// anyway — attach_disk resolves it to refuse a volume already attached to
// another VM — so the parked path costs the same single scan it did before that
// guard was added. A holder that is not a parker is not an error: there is
// nothing to unpark, and the caller has already decided what that means.
func UnparkDiskAt(ctx context.Context, c Client, logger *log.Logger, bareVolid string, holder DiskHolder, cfg ParkerConfig) error {
	if c == nil {
		return cpierrors.Cloud("UnparkDiskAt: client must not be nil")
	}
	if bareVolid == "" {
		return cpierrors.Cloud("UnparkDiskAt: bareVolid must not be empty")
	}
	if !holder.Found || !holder.IsParker {
		return nil
	}
	if holder.Slot == "" {
		// The scan placed the disk on this parker, so a missing slot means the
		// config moved underneath the read rather than that the disk is free.
		return cpierrors.Retriable(
			"UnparkDiskAt: disk %q confirmed on parker vmid %d but slot not found in config (possible race)",
			bareVolid, holder.VMID)
	}
	return unparkAt(ctx, c, logger, bareVolid, holder.VMID, holder.Node, holder.Slot, cfg)
}

// parkerProtectionLockTTL bounds how long a held protection-window lock is
// considered live. A holder whose recorded expiry has passed is treated as
// crashed and its lock is stolen, so this has to exceed the longest window any
// caller can legitimately hold — otherwise the lock is taken from a process
// that is still inside its own window, which is the exact race it exists to
// prevent, now silent.
//
// The longest window is the park path: up to attachParkerVerifyRetries
// iterations of config read, attach, task await, and verify read, then a
// protection write and a provenance write. Rather than assume 180s covers the
// sum of those retry curves -- it does not, on the pushback curve -- the window
// runs under a deadline derived from this TTL (parkerWindowBudget), so work
// that would outlive the lock is cut off instead of continuing past the point
// where another caller may enter. A waiter that times out is handed a retriable
// error rather than proceeding unserialized, since reaching the deadline means a
// live holder was inside the window throughout.
const parkerProtectionLockTTL = 180 * time.Second

// parkerProtectionLockTimeout bounds the wait for the lock. On timeout the
// window runs unserialized rather than failing, which is what every release
// before the lock existed did; see withParkerProtectionLock.
const parkerProtectionLockTimeout = 15 * time.Second

// parkerLockReleaseTimeout bounds the deferred sentinel-pool delete.
const parkerLockReleaseTimeout = 10 * time.Second

// parkerLockTimeoutsKey carries a test override for the protection-window
// lock's TTL and acquire timeout. The production values are tuned for a real
// cluster -- a 15s wait and a 180s TTL -- and a test that exercises the
// contended paths would otherwise have to spend them in wall-clock time.
// Test-only, like WithTestBackoff: production code must leave the constants in
// place.
type parkerLockTimeoutsKey struct{}

type parkerLockTimeouts struct {
	ttl     time.Duration
	timeout time.Duration
}

// withTestParkerLockTimeouts returns a context that shortens the
// protection-window lock's TTL and acquire timeout.
func withTestParkerLockTimeouts(ctx context.Context, ttl, timeout time.Duration) context.Context {
	return context.WithValue(ctx, parkerLockTimeoutsKey{}, parkerLockTimeouts{ttl: ttl, timeout: timeout})
}

// parkerLockTimeoutsFrom returns the override installed by
// withTestParkerLockTimeouts, or the production constants.
func parkerLockTimeoutsFrom(ctx context.Context) (time.Duration, time.Duration) {
	if o, ok := ctx.Value(parkerLockTimeoutsKey{}).(parkerLockTimeouts); ok && o.ttl > 0 && o.timeout > 0 {
		return o.ttl, o.timeout
	}
	return parkerProtectionLockTTL, parkerProtectionLockTimeout
}

// parkerProvenanceRemoveTimeout bounds the detached provenance removal that runs
// after a volume leaves its parker. Two calls, a config read and a description
// write, on a context the window deadline cannot stop.
const parkerProvenanceRemoveTimeout = 20 * time.Second

// parkerDemotedSweepTimeout bounds the detached cleanup that clears an unusedN
// key left by a slot lost to a concurrent park. It covers a lock acquire, a
// config read, and a detach, so it is generous relative to the lock's own wait.
const parkerDemotedSweepTimeout = 45 * time.Second

// withParkerProtectionLock serializes the protection windows on one parker VM.
//
// Every window is clear protection -> mutate -> restore protection, and two of
// them interleaved on the same parker produce a restore that lands while the
// other caller is still mid-detach: PVE then refuses that detach, because
// protection "will disable the remove VM and remove disk operations". The
// caller sees a retriable failure on work that was in fact fine. Worse, the
// loser's restore can be the last write, leaving the flag down on a parker that
// may hold other deployments' disks.
//
// The key is the same "vm-<vmid>" scheme the handlers use for per-VMID
// read-modify-write serialization, so a parker is serialized under one name
// cluster-wide. Nothing here ever holds a second lock, so there is no ordering
// to deadlock on.
//
// An acquire failure the CPI causes is not fatal: the mechanism is advisory, the
// uncontended path is correct without it, and refusing to unpark a disk because
// the CPI cannot create a sentinel pool would be a worse trade than the race it
// prevents. A missing pool service, a denied grant, and a transport fault all
// proceed unlocked and say so. A timeout is the one exception and is returned
// retriably: reaching the deadline means a live holder was inside the window the
// whole time, which is exactly the interleaving this lock exists to prevent.
func withParkerProtectionLock(ctx context.Context, c Client, logger *log.Logger, parkerVMID int, purpose string, fn func(context.Context) error) error {
	var pools PoolService
	if c != nil {
		pools = c.Pools()
	}
	if pools == nil {
		if logger != nil {
			logger.Warn("parker: no pool service; running the protection window unserialized",
				log.Int("parker_vmid", parkerVMID),
				log.String("purpose", purpose),
			)
		}
		return fn(ctx)
	}
	owner := fmt.Sprintf("%s/%d/%d", purpose, os.Getpid(), parkerVMID)
	ttl, timeout := parkerLockTimeoutsFrom(ctx)
	handle, lockErr := AcquireClusterLock(ctx, pools,
		fmt.Sprintf("vm-%d", parkerVMID), owner, ttl, timeout)
	if lockErr != nil {
		if errors.Is(lockErr, ErrClusterLockTimeout) {
			// A timeout is not "the lock is unavailable to me", it is "somebody
			// else is inside the window right now": an expired or unreadable
			// holder is stolen rather than waited on, so the only way to reach
			// the deadline is for a live holder to have held it throughout.
			// Proceeding here would run the exact interleaving this lock exists
			// to prevent, and it would do so precisely when contention is
			// highest. Hand it back retriable and let the Director re-drive.
			return lockErr
		}
		// Every other acquire failure means the mechanism is unavailable, not
		// that somebody holds it: no pool service, a denied CreatePool, a
		// transport fault. Proceed unserialized rather than fail. An identity
		// without Pool.Allocate on bosh-lock-* would otherwise never complete an
		// attach_disk or delete_disk for a parked disk, and running unlocked is
		// what every release before this one did. Matches the nil-pool branch
		// above and stampDeletingTag's fallback.
		if logger != nil {
			logger.Warn("parker: could not acquire the protection-window lock; running the window unserialized",
				log.Int("parker_vmid", parkerVMID),
				log.String("purpose", purpose),
				log.Err(lockErr),
			)
		}
		// No lock, no TTL to stay inside: run on the caller's own deadline.
		return fn(ctx)
	}
	defer func() {
		// Release on a detached context: an already-cancelled request would
		// otherwise fail the delete instantly and strand the sentinel pool
		// until a later acquirer steals it past the TTL.
		relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), parkerLockReleaseTimeout)
		defer relCancel()
		if relErr := handle.Release(relCtx); relErr != nil && logger != nil {
			logger.Warn("parker: could not release the protection-window lock (non-fatal)",
				log.Int("parker_vmid", parkerVMID),
				log.Err(relErr),
			)
		}
	}()
	// The window runs on a deadline derived from the lock's own TTL, less the
	// release budget. Bounding each retry loop separately is not enough: their
	// worst cases compose, and a window that outlives its TTL is stolen by the
	// next acquirer while this one is still inside it -- protection restored
	// mid-detach, the sweep refused, exactly the interleaving the lock exists to
	// prevent. A deadline is the one bound that cannot be composed past.
	//
	// Restores use context.WithoutCancel, so protection still goes back on when
	// this deadline is what stopped the work.
	windowCtx, windowCancel := context.WithTimeout(ctx, parkerWindowBudget(ttl))
	defer windowCancel()
	return fn(windowCtx)
}

// parkerProtectionRestoreReserve is time set aside for the protection restore
// that runs after the window body, on a detached context so a cancelled call
// still puts the flag back. Four attempts on the pushback curve (5s, 7.5s,
// 11.25s) plus the writes themselves land under 30s.
const parkerProtectionRestoreReserve = 30 * time.Second

// parkerWindowBudget is how long work may run inside a protection window whose
// lock carries the given TTL.
//
// Not simply the TTL: three things run AFTER the body, all on detached contexts
// precisely so a deadline cannot stop them -- the deferred unusedN sweep
// (parkerDemotedSweepTimeout), the protection restore
// (parkerProtectionRestoreReserve), and the sentinel release
// (parkerLockReleaseTimeout). Give the body the whole TTL and those three run
// past the recorded expiry, where a waiter is entitled to steal the lock and
// enter its own window -- so the deadline would move the interleaving past the
// fence rather than remove it. The reserve is subtracted instead.
//
// Never less than a second, so a test clock that sets a tiny TTL still runs its
// body rather than expiring before the first call.
func parkerWindowBudget(ttl time.Duration) time.Duration {
	reserve := parkerLockReleaseTimeout + parkerDemotedSweepTimeout + parkerProtectionRestoreReserve
	budget := ttl - reserve
	if budget < time.Second {
		return time.Second
	}
	return budget
}

// unparkAt performs the detach itself once the parker, its node, and the slot
// are known. Shared by UnparkDisk and UnparkDiskAt so both spend the protection
// flag the same way.
func unparkAt(ctx context.Context, c Client, logger *log.Logger, bareVolid string, parkerVMID int, parkerNode, slot string, cfg ParkerConfig) error {
	return withParkerProtectionLock(ctx, c, logger, parkerVMID, "unpark", func(wctx context.Context) error {
		return unparkAtLocked(wctx, c, logger, bareVolid, parkerVMID, parkerNode, slot, cfg)
	})
}

// unparkAtLocked is unparkAt's body, run with the parker's protection window
// held. Split out so the lock scope is exactly the window and nothing else.
func unparkAtLocked(ctx context.Context, c Client, logger *log.Logger, bareVolid string, parkerVMID int, parkerNode, slot string, cfg ParkerConfig) error {
	// Re-resolve the slot under the lock. The caller's slot came from a scan
	// taken before the lock was held, and detaching a slot by name is a blind
	// write: PVE detaches whatever occupies scsiN now, not the volume we looked
	// up. Two unparks of the same disk that overlap -- a Director retry after a
	// timeout, or two CPI processes racing -- would otherwise have the second
	// one detach a volume a concurrent park had just placed in the freed slot,
	// silently un-parking a disk whose own detach_disk already reported success.
	// One config read inside the window is a small price for not detaching
	// someone else's volume.
	verifyCfg, verifyErr := c.QEMU().Config(ctx, parkerNode, parkerVMID)
	if verifyErr != nil {
		if parkerConfigGone(verifyErr) {
			// The parker vanished between the holder scan and this read. The
			// CPI never deletes parkers, so under the strict invariant this is
			// an out-of-band deletion: refuse rather than report an unpark
			// that never ran against a volume whose anchor is missing.
			if cfg.AnchorStrict && cfg.ParkedEnabled {
				return cpierrors.Cloud(
					"UnparkDisk: parker vmid %d (node %s) vanished before the detach of %s; the parker "+
						"anchor is missing (deleted out-of-band). Verify the volume is intact, then set "+
						"pve.parked_anchor_strict: false to treat it as free-floating and retry",
					parkerVMID, parkerNode, bareVolid,
				)
			}
			// Permissive: the parker is gone, and with it the reference.
			// Nothing to unpark.
			return nil
		}
		return cpierrors.Wrap(WrapConfigReadError(verifyErr),
			fmt.Sprintf("UnparkDisk: re-read parker vmid %d on node %s before detach", parkerVMID, parkerNode))
	}
	actualSlot, onActiveBus := FindDiskIDByVolID(qemu.ParseDisks(verifyCfg), bareVolid)
	if onActiveBus {
		if actualSlot != slot && logger != nil {
			logger.Info("parker: disk moved slots between the holder scan and the unpark; using the current one",
				log.Int("parker_vmid", parkerVMID),
				log.String("volid", bareVolid),
				log.String("scanned_slot", slot),
				log.String("actual_slot", actualSlot),
			)
		}
		slot = actualSlot
	} else {
		// Not on any active bus. Either another caller already detached it, or a
		// previous attempt failed between PVE's demotion to unusedN and the key
		// removal. The first case is done; the second still has to be swept, and
		// the sweep needs the protection window.
		if !unusedEntriesReference(verifyCfg, bareVolid) {
			// The volume is off this parker entirely. Drop the provenance entry
			// as every other success path does: the record exists to name what
			// this parker holds, and one that outlives the disk it names is what
			// a later audit reads as a parker still holding a volume.
			removeParkerProvenance(ctx, c, logger, parkerNode, parkerVMID, bareVolid, cfg)
			return nil
		}
		return sweepDemotedUnderProtection(ctx, c, logger, parkerNode, parkerVMID, bareVolid, cfg)
	}

	// A volume named for the parker that holds it must never take this path.
	// The SDK's DetachDisk sweeps the unusedN entry PVE demotes the volume to,
	// and PVE physically removes an unused volume its holder owns — so a
	// legacy unpark of a parker-owned volume deletes the disk it is meant to
	// free. Two states produce that name: a stable-ID disk a reassignment
	// transferred onto this parker (the normal renamed state, moved off by
	// reassignment only), and a legacy volume whose synthetic creation VMID
	// collides with the parker band (a band-overlap misconfiguration the
	// sweep's own guard also refuses). Refusing up front turns a silent
	// deletion into an actionable error either way.
	if embedded, ok := EmbeddedDiskVMID(bareVolid); ok && embedded == parkerVMID {
		return cpierrors.Cloud(
			"UnparkDisk: volume %q is named for parker vmid %d, which owns it; a config-edit detach would let "+
				"PVE deallocate it. A stable-ID disk moves off its parker by reassignment (attach_disk does this "+
				"itself); a legacy volume with this name means the parker band overlaps the disk VMID band -- "+
				"correct parked_disk_vmid_range_start/end",
			bareVolid, parkerVMID,
		)
	}

	// Every parker carries protection=1, and PVE states that the flag "will
	// disable the remove VM and remove disk operations" — detaching a disk from
	// its slot is exactly such an operation, so PVE rejects it while the flag is
	// set. Clear protection for the length of the detach and put it straight
	// back. Failing to clear it is fail-closed: without this the detach cannot
	// succeed, so there is no point attempting it.
	if protErr := setParkerProtection(ctx, c, logger, parkerNode, parkerVMID, false); protErr != nil {
		// Classify rather than assume: a 403 for a missing VM.Config.Options
		// grant never comes right on its own, and labelling it retriable makes
		// the Director drive an unpark forever against a permission that has to
		// be granted by hand.
		return cpierrors.Wrap(WrapMutationError(protErr),
			fmt.Sprintf("UnparkDisk: clear protection on parker vmid %d on node %s", parkerVMID, parkerNode))
	}

	retryErr := RetryOnTransientOrLock(ctx, logger, "unpark_disk_detach", parkerWindowMaxAttempts, func() error {
		return c.QEMU().DetachDisk(ctx, parkerNode, parkerVMID, slot)
	})

	// PVE does not free a detached volume: it demotes it to an unusedN key. The
	// SDK's DetachDisk clears that key as a second request, so a transient
	// failure between the two leaves the volume referenced as unusedN — and the
	// retry then finds the active slot already gone and reports success.
	//
	// Nothing downstream can see that reference. Every holder probe here matches
	// on the active-bus keys only, so IsDiskParked answers "not parked" forever,
	// attach_disk's holder guard sees no holder, and the volume is attached to a
	// workload VM while the parker still points at it. Purging the parker then
	// frees a live persistent disk along with it: qm destroy walks the config's
	// unused entries too. Sweep it while protection is still down,
	// which is the only window in which PVE will accept the removal.
	//
	// Detached and separately bounded, like the park path's deferred sweep and
	// for the same reason: the detach has already happened, so a cancelled or
	// deadline-stopped unpark is exactly when the stranded reference matters
	// most. On the window context this sweep would fail on the dead context
	// without looking at the parker at all, and report a permanent action item
	// telling the operator to unlink a key the SDK's second request had probably
	// already removed. The window budget reserves this time (parkerWindowBudget).
	sweepCtx, sweepCancel := context.WithTimeout(context.WithoutCancel(ctx), parkerDemotedSweepTimeout)
	sweepErr := sweepParkerUnusedSlots(sweepCtx, c, logger, parkerNode, parkerVMID, bareVolid)
	sweepCancel()

	// Restore protection whether or not the detach succeeded: leaving a parker
	// unprotected is the one outcome worse than a failed unpark, since the
	// parker may still hold other deployments' disks. context.WithoutCancel
	// keeps the restore reachable when the request context is already done —
	// a cancelled or timed-out unpark is exactly when the window would otherwise
	// stay open indefinitely. A restore failure is logged rather than returned:
	// the detach result is what the caller acts on, and the park path re-asserts
	// the flag on every attach.
	if protErr := setParkerProtection(context.WithoutCancel(ctx), c, logger, parkerNode, parkerVMID, true); protErr != nil && logger != nil {
		logger.Warn("UnparkDisk: could not restore protection on parker — re-set it by hand (qm set <vmid> --protection 1)",
			log.Int("parker_vmid", parkerVMID),
			log.String("node", parkerNode),
			log.Err(protErr),
		)
	}

	if retryErr != nil {
		// Classified like the protection write one line up: a 403 for a missing
		// VM.Config.Disk grant is a permission to add, not a fault to re-drive.
		return cpierrors.Wrap(WrapMutationError(retryErr),
			fmt.Sprintf("UnparkDisk: detach %q from parker vmid %d slot %s on node %s",
				bareVolid, parkerVMID, slot, parkerNode))
	}
	if sweepErr != nil {
		// The active slot is gone but PVE's unusedN key for this volume is not.
		// Nothing downstream can see that reference: every holder probe here
		// matches active-bus keys only, so a retry of this call finds the disk
		// free and attaches it to a workload VM while the parker still points
		// at it.
		//
		// The retry does NOT clear it. Control only reaches here with the detach
		// already successful, so the retry's holder scan finds nothing on this
		// parker, UnparkDiskAt returns early, and no sweep runs. A later park
		// starts with demoted=false, so its defer never fires either. The
		// reference stands until an operator clears it -- which is why the error
		// below is permanent and carries the commands that clear it.
		reportUnsweptReference(logger, parkerNode, parkerVMID, bareVolid, sweepErr)
		return unsweptReferenceError(parkerNode, parkerVMID, bareVolid, sweepErr)
	}
	removeParkerProvenance(ctx, c, logger, parkerNode, parkerVMID, bareVolid, cfg)
	return nil
}

// unusedEntriesReference reports whether any unusedN key in cfg names bareVolid.
// A demoted volume is invisible to every active-bus probe, so this is the only
// way to tell "already unparked" from "unparked but still referenced".
func unusedEntriesReference(cfg map[string]any, bareVolid string) bool {
	for _, volid := range FindUnusedDiskEntries(cfg) {
		if volid == bareVolid {
			return true
		}
	}
	return false
}

// sweepDemotedUnderProtection opens the protection window for a sweep alone,
// for the unpark that finds its volume already off the active bus but still
// named by an unusedN key. Returns nil when the reference is cleared.
func sweepDemotedUnderProtection(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, bareVolid string, cfg ParkerConfig) error {
	if protErr := setParkerProtection(ctx, c, logger, node, parkerVMID, false); protErr != nil {
		return cpierrors.Wrap(WrapMutationError(protErr),
			fmt.Sprintf("UnparkDisk: clear protection on parker vmid %d to sweep a demoted reference", parkerVMID))
	}
	// Detached and bounded, as in unparkAtLocked: this sweep is the whole point
	// of the call, and running it on a context that may already be done would
	// report an action item for work never attempted.
	sweepCtx, sweepCancel := context.WithTimeout(context.WithoutCancel(ctx), parkerDemotedSweepTimeout)
	sweepErr := sweepParkerUnusedSlots(sweepCtx, c, logger, node, parkerVMID, bareVolid)
	sweepCancel()
	if protErr := setParkerProtection(context.WithoutCancel(ctx), c, logger, node, parkerVMID, true); protErr != nil && logger != nil {
		logger.Warn("UnparkDisk: could not restore protection on parker — re-set it by hand (qm set <vmid> --protection 1)",
			log.Int("parker_vmid", parkerVMID),
			log.String("node", node),
			log.Err(protErr),
		)
	}
	if sweepErr != nil {
		// Same condition, same consequence as the detach path: see
		// reportUnsweptReference.
		reportUnsweptReference(logger, node, parkerVMID, bareVolid, sweepErr)
		return unsweptReferenceError(node, parkerVMID, bareVolid, sweepErr)
	}
	removeParkerProvenance(ctx, c, logger, node, parkerVMID, bareVolid, cfg)
	return nil
}

// unsweptReferenceError is the failure an unpark returns when the volume left
// its active slot but the unusedN key PVE demoted it to could not be removed.
//
// Permanent, and that is the whole point. The detach has already succeeded, so
// the Director's retry resolves the holder, finds none -- every holder probe
// here matches active-bus keys only -- and goes on to attach the volume to the
// workload VM while the parker still references it. A retriable class would not
// re-run the sweep; it would skip straight past it to exactly the double
// reference the sweep exists to prevent. Fail the call, name the parker, and
// carry the commands that clear it.
func unsweptReferenceError(node string, parkerVMID int, bareVolid string, cause error) error {
	return unsweptReferenceErrorFor(
		"UnparkDisk", "detached", node, parkerVMID, bareVolid, cause)
}

// unsweptReferenceErrorFor is unsweptReferenceError with the operation named by
// the caller. The park path emits the same class for the same condition, and the
// unpark's wording ("detached ... from parker") tells an operator a false story
// about what happened when nothing was detached -- on the one error whose entire
// purpose is to be acted on by hand.
func unsweptReferenceErrorFor(
	op, verb, node string, parkerVMID int, bareVolid string, cause error,
) error {
	return cpierrors.Cloud(
		"%s: %s %q on parker vmid %d (node %s) and could not clear the unused reference PVE "+
			"left behind, and nothing clears it on its own. Until it is cleared the parker holds an invisible "+
			"reference to a live volume and must not be purged. Clear it with: "+
			"qm set %d --protection 0 && qm unlink %d --idlist <unusedN> && qm set %d --protection 1, "+
			"then retry. Cause: %s",
		op, verb, bareVolid, parkerVMID, node, parkerVMID, parkerVMID, parkerVMID, cause.Error())
}

// reportUnsweptReference announces the one parker state nothing recovers from on
// its own: PVE demoted a volume to an unusedN key and the CPI could not remove
// it. Every holder probe here matches active-bus keys only, so the reference is
// invisible — a later unpark of this volume finds no holder and returns early, a
// later park only sweeps a slot it lost in that same call, and destroying the
// parker by hand frees a live persistent disk along with it -- qm destroy walks
// the config's unused entries too. ERROR, with the commands
// that clear it, because it is an action item rather than a warning.
func reportUnsweptReference(logger *log.Logger, node string, parkerVMID int, bareVolid string, cause error) {
	if logger == nil {
		return
	}
	logger.Error("parker: could not clear the unused reference PVE left behind; "+
		"nothing clears this on its own — this parker holds an invisible reference to a live volume and "+
		"must not be purged until it is cleared by hand: "+
		"qm set <parker-vmid> --protection 0 && qm unlink <parker-vmid> --idlist <unusedN> "+
		"&& qm set <parker-vmid> --protection 1",
		log.Int("parker_vmid", parkerVMID),
		log.String("node", node),
		log.String("volid", bareVolid),
		log.Err(cause),
	)
}

// parkerConfigGone reports whether err means the parker's config is not there
// to read. PVE answers that question two ways: a 404 from the HTTP layer, and a
// task-level "Configuration file ... does not exist" from pmxcfs when the conf
// went away between calls. Both mean the VM carries no reference to anything,
// which for every caller here is the same as an empty config rather than a
// failure to read one.
func parkerConfigGone(err error) bool {
	return IsNotFound(err) || IsPmxcfsConfigMissing(err)
}

// sweepParkerUnusedSlots removes every unusedN key on a parker whose value is
// bareVolid. Callers must hold the parker's protection window open: PVE refuses
// to remove an unused disk entry while protection is set, exactly as it refuses
// the active-slot detach.
//
// It is idempotent and cheap on the common path — one config read that finds
// nothing to do. It exists because a volume demoted to unusedN is invisible to
// every holder probe in this package, so an unswept entry is a reference no
// later call can find and no later call can clear.
//
// The removal is verified with one re-read after the loop. Every other write in
// this file is read-after-write checked, and this one has the quietest failure:
// PVE routes an unused-key removal through a deallocation whose result gates the
// config-key removal, so a non-error response is not by itself proof the key is
// gone.
//
// A read that fails is a failed sweep, not a clean one. Callers act on the
// result -- one of them has already SEEN the unusedN entry and calls this to
// clear it -- so answering "nothing to do" from a read that never arrived would
// report a reference cleared that is still there, and then delete the
// provenance record that names it. A config that is gone is the one exception:
// no config, no reference.
//
// Note that a LATER call does not generally sweep again -- an unpark whose
// detach already succeeded finds no holder and returns early, and a park only
// sweeps when it lost a slot in that same call -- so a reference left behind
// here stands until an operator clears it.
func sweepParkerUnusedSlots(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, bareVolid string) error {
	cfg, err := c.QEMU().Config(ctx, node, parkerVMID)
	if err != nil {
		if parkerConfigGone(err) {
			return nil
		}
		return cpierrors.Wrap(WrapConfigReadError(err),
			fmt.Sprintf("parker: read config to sweep unused disk slots on parker vmid %d", parkerVMID))
	}
	swept := 0
	for slot, volid := range FindUnusedDiskEntries(cfg) {
		if volid != bareVolid {
			continue
		}
		// Never remove an unused entry whose volume belongs to this parker's own
		// VMID. Under an overlapping band a parked volume can be named for the
		// parker holding it, and PVE deallocates a disk it considers the VM's
		// own -- so the removal that is meant to clear a stale reference would
		// destroy the volume instead. Leave it and say so.
		if embedded, ok := EmbeddedDiskVMID(volid); ok && embedded == parkerVMID {
			// The reference stands, and this call cannot clear it. Report it as
			// the unswept reference it is rather than counting it as swept:
			// returning success here tells the unpark the volume is free while
			// the parker still names it.
			refusal := cpierrors.Cloud(
				"parker: unused entry %s on parker vmid %d references %q, whose volume ID is named for this "+
					"parker; removing it would deallocate the volume rather than drop the reference. The parker "+
					"band overlaps the disk band -- correct parked_disk_vmid_range_start/end",
				slot, parkerVMID, volid)
			reportUnsweptReference(logger, node, parkerVMID, bareVolid, refusal)
			return refusal
		}
		sweepErr := RetryOnTransientOrLock(ctx, logger, "parker_sweep_unused", parkerWindowMaxAttempts, func() error {
			return c.QEMU().DetachDisk(ctx, node, parkerVMID, slot)
		})
		if sweepErr != nil {
			if IsNotFound(sweepErr) {
				continue
			}
			return cpierrors.Wrap(WrapMutationError(sweepErr),
				fmt.Sprintf("parker: remove lingering %s referencing %q on parker vmid %d",
					slot, bareVolid, parkerVMID))
		}
		swept++
		if logger != nil {
			logger.Info("parker: removed lingering unused disk slot",
				log.Int("parker_vmid", parkerVMID),
				log.String("slot", slot),
				log.String("volid", bareVolid),
			)
		}
	}
	if swept == 0 {
		return nil
	}
	// Read-after-write. A non-error response is not proof the key is gone.
	//
	// A read that cannot be completed is a failure, not a shrug. Nothing reads
	// this parker again on the CPI's own initiative -- a later unpark of this
	// volume finds no holder and returns early, and a later park only sweeps a
	// slot it lost in that same call -- so "the next read catches it" describes
	// a read that never happens. Reporting success here hands attach_disk a
	// volume the parker may still reference on an unusedN key, and deletes the
	// provenance entry that named it.
	var verifyCfg map[string]any
	verifyErr := RetryOnTransient(ctx, logger, "parker_sweep_verify", parkerWindowMaxAttempts, func() error {
		cfg, err := c.QEMU().Config(ctx, node, parkerVMID)
		if err != nil {
			return err
		}
		verifyCfg = cfg
		return nil
	})
	if verifyErr != nil {
		if parkerConfigGone(verifyErr) {
			// No config, no reference. The parker went away between the removal
			// and the read, which takes the unusedN key with it.
			return nil
		}
		unverified := cpierrors.Cloud(
			"parker: removed the unused entry referencing %q on parker vmid %d but could not read the config "+
				"back to confirm it: %s",
			bareVolid, parkerVMID, verifyErr.Error())
		reportUnsweptReference(logger, node, parkerVMID, bareVolid, unverified)
		return unverified
	}
	if unusedEntriesReference(verifyCfg, bareVolid) {
		// Permanent, for the same reason the caller's own failure is: a retry
		// re-drives an unpark whose detach already succeeded, so its holder scan
		// finds nothing on the parker, it returns early without sweeping, and
		// the attach it guards proceeds against a volume this parker still
		// references.
		survived := cpierrors.Cloud(
			"parker: unused entry referencing %q survived removal on parker vmid %d", bareVolid, parkerVMID)
		reportUnsweptReference(logger, node, parkerVMID, bareVolid, survived)
		return survived
	}
	return nil
}

// sweepParkerUnusedSlotsProtectedLocked opens the protection window around a
// sweep and closes it again. The caller must already hold the parker's
// protection lock -- AcquireClusterLock is not reentrant, so taking it again
// here would wait out its own timeout against a lock this process holds.
//
// Every failure is logged rather than returned: the caller is a deferred
// cleanup with no result of its own to report.
// Returns false when the reference may still stand, so a caller whose next step
// depends on the volume being free can change course.
func sweepParkerUnusedSlotsProtectedLocked(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, bareVolid string) bool {
	if protErr := setParkerProtection(ctx, c, logger, node, parkerVMID, false); protErr != nil {
		reportUnsweptReference(logger, node, parkerVMID, bareVolid, protErr)
		return false
	}
	sweepErr := sweepParkerUnusedSlots(ctx, c, logger, node, parkerVMID, bareVolid)
	if sweepErr != nil {
		reportUnsweptReference(logger, node, parkerVMID, bareVolid, sweepErr)
	}
	if protErr := setParkerProtection(context.WithoutCancel(ctx), c, logger, node, parkerVMID, true); protErr != nil && logger != nil {
		logger.Warn("parker: could not restore protection after a sweep — re-set it by hand (qm set <vmid> --protection 1)",
			log.Int("parker_vmid", parkerVMID),
			log.Err(protErr),
		)
	}
	return sweepErr == nil
}

// reassertParkerProtection puts protection back on a parker after a successful
// attach. It is the counterpart to the unpark window: a restore that failed, a
// request cancelled mid-unpark, or a CPI process killed between the clear and
// the restore all leave a parker unprotected with no other caller able to
// notice. Since the park path already knows it just wrote to this parker, one
// idempotent PUT here closes that window at the next use.
//
// A failure is logged, never returned: the disk is parked either way, and
// failing the park would be a worse outcome than a protection flag that stays
// down until the next park.
func reassertParkerProtection(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int) {
	if protErr := setParkerProtection(ctx, c, logger, node, parkerVMID, true); protErr != nil && logger != nil {
		logger.Warn("parker: could not re-assert protection after a park — re-set it by hand (qm set <vmid> --protection 1)",
			log.Int("parker_vmid", parkerVMID),
			log.String("node", node),
			log.Err(protErr),
		)
	}
}

// setParkerProtection sets or clears the PVE protection flag on a parker VM.
// Used by UnparkDisk to open a bounded window for the detach, and by the park
// path to re-assert the flag on every successful attach, which is what closes
// the window a failed restore, a cancelled request, or a killed CPI process
// would otherwise leave open. A nil nodes service (test stubs without
// injection) is a no-op.
// cfgKeyTags is the QEMU create/update config key for the tag string.
const cfgKeyTags = "tags"

// cfgKeyName is the QEMU create/update config key for the VM name.
const cfgKeyName = "name"

// cfgKeyVMID is the QEMU create param key for the VM ID.
const cfgKeyVMID = "vmid"

// paramProtection is the QEMU create/update config key for the PVE
// protection flag.
const paramProtection = "protection"

func setParkerProtection(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, on bool) error {
	nodesSvc := c.Nodes()
	if nodesSvc == nil {
		return nil
	}
	vmidStr := strconv.Itoa(parkerVMID)
	value := on
	// Bounded like every other in-window call: the default ten attempts on the
	// storage-lock curve sleep past two minutes, and on the pushback curve past
	// four, either of which outlives the lock TTL guarding the window this write
	// opens and closes.
	return RetryOnTransientOrLock(ctx, logger, "parker_set_protection", parkerWindowMaxAttempts, func() error {
		return nodesSvc.UpdateQemuConfig(ctx, node, vmidStr, &sdknodes.UpdateQemuConfigParams{
			Protection: &value,
		})
	})
}
