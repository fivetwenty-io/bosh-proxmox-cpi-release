// Disk CID parsing, formatting, and disk-slot resolution helpers used by
// detach_disk, resize_disk, and set_disk_metadata.
package pve

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// ErrDiskNotAttached is returned (wrapped via fmt.Errorf with %w) by
// ResolveDiskID when the requested volid is not present on any active bus
// slot of the target VM. Callers should detect this condition via
// errors.Is(err, pve.ErrDiskNotAttached) rather than relying on the error
// type or message text: handlers such as detach_disk treat it as
// idempotent success, while resize_disk and update_disk treat it as a
// hard cloud error.
var ErrDiskNotAttached = errors.New("disk not attached to vm")

// ErrDiskNotAttachedToAnyVM is the sentinel wrapped by FindVMByDiskVolid when
// the cluster-wide scan completes without finding any VM that holds the disk.
// FindVMByDiskVolidOrNone detects this sentinel via errors.Is to translate the
// not-found condition into (0, "", false, nil) rather than propagating the
// error. Other callers that need to distinguish "missing" from "transport
// failure" may also use errors.Is(err, ErrDiskNotAttachedToAnyVM).
var ErrDiskNotAttachedToAnyVM = errors.New("disk not attached to any VM")

// unusedDiskKeyPattern matches PVE "unusedN" config keys. PVE moves a disk
// to such a slot when it is removed from its bus slot (e.g., scsi1) via
// PUT config delete:scsi1, instead of fully clearing the entry. Persistent
// volumes still owned by BOSH but holding such an entry will be destroyed
// by the next DELETE /qemu/{vmid}; delete_vm therefore refuses to issue
// the destroy when an unusedN entry references a volume on the configured
// pve.disk_storage. (The DetachDisk SDK call cleans these up on its own;
// this guard catches paths where DetachDisk was bypassed or failed mid-way.)
var unusedDiskKeyPattern = regexp.MustCompile(`^unused\d+$`)

// FindUnusedDiskEntries returns every (slot, volid) pair in cfg whose key
// matches "unusedN" and whose value is a non-empty string. The returned
// volids are bare (any ",options" suffix is stripped) for direct equality
// comparison with storage-prefixed volume identifiers.
func FindUnusedDiskEntries(cfg map[string]any) map[string]string {
	out := make(map[string]string)
	for key, raw := range cfg {
		if !unusedDiskKeyPattern.MatchString(key) {
			continue
		}
		val, ok := raw.(string)
		if !ok || val == "" {
			continue
		}
		bare := val
		if comma := strings.Index(val, ","); comma >= 0 {
			bare = val[:comma]
		}
		out[key] = bare
	}
	return out
}

// embeddedDiskVMIDPattern extracts the VMID label from a PVE disk volid. PVE
// names every VM-owned volume "vm-<vmid>-disk-<n>"; the <vmid> identifies the
// VM the volume was allocated against. Matches both the flat form
// ("storage:vm-15689-disk-0") and the path form
// ("storage:15689/vm-15689-disk-0.qcow2").
var embeddedDiskVMIDPattern = regexp.MustCompile(`vm-(\d+)-disk-\d+`)

// EmbeddedDiskVMID returns the VMID encoded in a PVE disk volid and true when
// the "vm-<vmid>-disk-<n>" pattern is present. Returns (0, false) for volids
// that carry no such label: cdrom/ISO entries ("none,media=cdrom",
// "local:iso/foo.iso"), cloudinit drives ("vm-<vmid>-cloudinit"), efidisk /
// tpmstate volumes, or unparseable values.
func EmbeddedDiskVMID(volid string) (int, bool) {
	m := embeddedDiskVMIDPattern.FindStringSubmatch(volid)
	if len(m) != 2 {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// FindForeignActiveDisks returns every (slot -> bare volid) on an active bus
// slot (scsi/virtio/ide/sata) of cfg whose embedded VMID label differs from
// ownerVMID. These are persistent disks the Director attached to the VM
// (created by create_disk under a synthetic VMID); PVE's DELETE /qemu/{vmid}
// would destroy them with the VM. delete_vm detaches them first so the volume
// survives.
//
// Slots whose volid carries no "vm-<n>-disk-<n>" label (cdrom/ISO, cloudinit)
// are skipped — not persistent BOSH disks. Slots whose embedded VMID equals
// ownerVMID are the VM's own root/ephemeral disks and are NOT returned (they
// are destroyed with the VM) — UNLESS the drive entry carries a bpd- stable-ID
// serial: a move_disk reassignment renames a persistent volume to match its
// new owner, so after a transfer the embedded-VMID heuristic reads the disk
// as the VM's own, and the guard built on it would let delete_vm destroy a
// persistent disk. The serial is written only on CPI persistent disks, so its
// presence is authoritative: such an entry is ALWAYS foreign. Returned volids
// are bare (option suffix stripped).
func FindForeignActiveDisks(cfg map[string]any, ownerVMID int) map[string]string {
	out := make(map[string]string)
	for slot, entry := range FindForeignActiveDiskDetails(cfg, ownerVMID) {
		out[slot] = entry.Volid
	}
	return out
}

// ForeignActiveDisk is one foreign persistent-disk entry on an active bus
// slot, with the stable-ID serial the drive carries (empty for legacy disks
// recognized by the embedded-VMID heuristic alone).
type ForeignActiveDisk struct {
	Volid    string
	StableID string
}

// FindForeignActiveDiskDetails is FindForeignActiveDisks with the stable-ID
// serial preserved, for callers (delete_vm) that must pick a preservation
// mechanism per disk: a renamed identity disk is owner-named, and a plain
// detach would let PVE's unusedN sweep deallocate it, so it moves by
// reassignment instead.
func FindForeignActiveDiskDetails(cfg map[string]any, ownerVMID int) map[string]ForeignActiveDisk {
	out := make(map[string]ForeignActiveDisk)
	for slot, optstr := range qemu.ParseDisks(cfg) {
		bare := optstr
		if comma := strings.Index(optstr, ","); comma >= 0 {
			bare = optstr[:comma]
		}
		if id, has := StableIDFromDriveOptStr(optstr); has {
			out[slot] = ForeignActiveDisk{Volid: bare, StableID: id}
			continue
		}
		vmid, ok := EmbeddedDiskVMID(bare)
		if !ok || vmid == ownerVMID {
			continue
		}
		out[slot] = ForeignActiveDisk{Volid: bare}
	}
	return out
}

// ParseDiskCID splits a disk CID of the form "<storage>:<volume>" on the first
// colon. Returns an error if cid is empty or contains no colon.
func ParseDiskCID(cid string) (storage, volume string, err error) {
	if cid == "" {
		return "", "", cpierrors.Cloud("disk CID must not be empty")
	}
	s, v, ok := strings.Cut(cid, ":")
	if !ok {
		return "", "", cpierrors.Cloud("invalid disk CID %q: expected format <storage>:<volume>", cid)
	}
	if s == "" {
		return "", "", cpierrors.Cloud("invalid disk CID %q: storage part must not be empty", cid)
	}
	if v == "" {
		return "", "", cpierrors.Cloud("invalid disk CID %q: volume part must not be empty", cid)
	}
	return s, v, nil
}

// FormatDiskCID joins storage and volume into the canonical disk CID string.
func FormatDiskCID(storage, volume string) string {
	return storage + ":" + volume
}

// DiskCIDMeta carries optional placement metadata encoded into a disk CID
// suffix. Fields use omitempty so a partially-populated struct produces a
// compact JSON payload. All fields are informational: consumers that do not
// need them can safely discard the meta return value of ParseEncodedDiskCID.
type DiskCIDMeta struct {
	// Pool is the PVE storage pool name ("local-lvm", "data", …). Populated
	// from the resolved storage at create_disk time.
	Pool string `json:"pool,omitempty"`
	// Node is the PVE node that owns the disk. Populated for node-local
	// backends; empty for shared storage where the volume is reachable from
	// any node.
	Node string `json:"node,omitempty"`
	// AZ is the availability-zone label at create_disk time. Populated when
	// the placement layer resolves an AZ; empty otherwise.
	AZ string `json:"az,omitempty"`
	// Opts carries PVE per-disk performance options (iothread, cache, discard,
	// ssd, mbps_rd, mbps_wr, iops_rd, iops_wr) resolved at create_disk time so
	// attach_disk can apply them on the VM config attach. Empty (default) means
	// no options were requested; the encoded CID is byte-identical to prior
	// releases.
	Opts map[string]string `json:"opts,omitempty"`
	// Anchor records that the disk was created under the parked strategy and
	// is promised a parker anchor whenever it is detached. The attach and
	// delete holder guards read it: a detached disk carrying this promise
	// with no holder anywhere in the cluster means a parker VM was deleted
	// out-of-band (see pve.parked_anchor_strict). Omitted when false, so
	// legacy CIDs and disks created under "free" decode to false and keep
	// the permissive behavior — their anchor was never promised.
	Anchor bool `json:"anchor,omitempty"`
	// Format is the disk-image format ("qcow2", "raw", "vmdk") resolved at
	// create_disk time — per-call cloud_properties, then vm_disk_format, then
	// the qcow2 default. attach_disk prefers it over re-deriving the format
	// from current config, so a vm_disk_format change between create and
	// attach cannot flip the discard/ssd auto-resolution the disk was created
	// under. Empty on legacy CIDs; consumers fall back to the config-derived
	// guess.
	Format string `json:"f,omitempty"`
	// ID is the disk's stable identity token ("bpd-" + 16 lowercase hex
	// characters, 20 bytes — exactly PVE's drive-serial cap). Generated at
	// create_disk time under the parked strategy and carried on the PVE side
	// as a drive serial= option, so the identity survives the volume rename
	// PVE performs on every move_disk reassignment. The envelope volid then
	// becomes a birth record: consumers resolve the current volid through the
	// stable-ID scan first and fall back to the birth volid for legacy CIDs
	// (which stay volid-resolved forever — CIDs are immutable once stored by
	// the Director, so a legacy disk can never gain an ID). Empty on legacy
	// CIDs and on disks created under the "free" strategy.
	ID string `json:"id,omitempty"`
}

// diskCIDPrefix marks the envelope disk CID format. Everything after the
// prefix is base64url (RFC 4648 §5, no padding), so an emitted CID uses only
// [A-Za-z0-9_-] — safe in a URI path segment (the Director's
// /disks/<cid>/attachments route) and in bosh CLI argument passthrough,
// unlike the raw PVE volid whose path form embeds "/".
const diskCIDPrefix = "pvd-"

// diskCIDCompressedPrefix marks the gzip-compressed envelope disk CID format.
// The payload is base64url(gzip(json)) with the same JSON envelope and charset
// guarantee as diskCIDPrefix. Emitted only by EncodeDiskCIDCompressed and only
// when the plain pvd- form would exceed DiskCIDLengthTarget; decoded
// unconditionally by ParseEncodedDiskCID, because the Director replays stored
// CIDs indefinitely for the lifetime of a deployment.
const diskCIDCompressedPrefix = "pvz-"

// DiskCIDLengthTarget is the longest disk CID guaranteed to fit every BOSH
// Director database backend: MySQL (and the newer dynamic_disks table on all
// backends) stores disk_cid in a varchar(255) column. PostgreSQL's classic
// disk tables use unbounded text, but the CPI cannot know which backend the
// Director runs, so the bound is enforced universally. Exported so callers
// (e.g. create_disk) can enforce the same bound as a hard error when even the
// compressed form overflows it.
const DiskCIDLengthTarget = 255

// maxDiskCIDEnvelopeBytes caps the decompressed size of a pvz- envelope so a
// hostile CID cannot decompression-bomb the CPI process. Real envelopes are a
// few hundred bytes; 64 KiB is orders of magnitude of headroom.
const maxDiskCIDEnvelopeBytes = 64 << 10

// diskCIDEnvelope is the JSON payload behind diskCIDPrefix. V carries the
// exact PVE volid ("storage:volume"); M carries optional placement metadata.
type diskCIDEnvelope struct {
	V string       `json:"v"`
	M *DiskCIDMeta `json:"m,omitempty"`
}

// EncodeDiskCID wraps a bare disk CID (a PVE volid) and optional metadata in
// the pvd- envelope:
//
//	pvd-<base64url(json({"v":"<storage>:<volume>","m":{...}}))>
//
// The CID is always wrapped, even when meta is nil or all-zero: path-form
// volids ("storage:100/vm-100-disk-0.qcow2") embed "/", which 404s the
// Director's /disks/<cid>/attachments route and breaks bosh CLI argument
// handling, so the bare form must never escape as a Director-visible CID.
// A nil or all-zero meta is omitted from the payload.
//
// Returns an error when bareCID is empty: encoding an empty volid is always a
// programming error in the caller (there is no such thing as a disk with no
// underlying PVE volume), and letting it through would silently produce a
// CID that decodes to an empty bare CID, breaking every downstream ParseCID
// call. Round-trip totality (EncodeDiskCID/ParseEncodedDiskCID never produce
// a CID that cannot itself be correctly re-decoded) is a documented invariant
// callers may rely on.
func EncodeDiskCID(bareCID string, meta *DiskCIDMeta) (string, error) {
	if bareCID == "" {
		return "", cpierrors.Cloud("EncodeDiskCID: bareCID must not be empty; encoding an empty volid is a programming error in the caller")
	}
	b, err := marshalDiskCIDEnvelope(bareCID, meta)
	if err != nil {
		// json.Marshal on a plain struct never returns an error; guard anyway
		// to satisfy the contract that EncodeDiskCID never panics.
		return "", cpierrors.Wrap(err, "EncodeDiskCID: marshal envelope")
	}
	return diskCIDPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// marshalDiskCIDEnvelope builds the JSON envelope payload shared by the plain
// (pvd-) and compressed (pvz-) encoders. A nil or all-zero meta is omitted.
func marshalDiskCIDEnvelope(bareCID string, meta *DiskCIDMeta) ([]byte, error) {
	env := diskCIDEnvelope{V: bareCID}
	if meta != nil && (meta.Pool != "" || meta.Node != "" || meta.AZ != "" || len(meta.Opts) > 0 || meta.Anchor || meta.Format != "" || meta.ID != "") {
		env.M = meta
	}
	return json.Marshal(env)
}

// EncodeDiskCIDCompressed is the overflow-safe disk CID encoder. It emits the
// same pvd- envelope as EncodeDiskCID whenever that form fits
// DiskCIDLengthTarget — the common case stays byte-identical and
// operator-inspectable — and switches to
//
//	pvz-<base64url(gzip(json({"v":…,"m":{…}})))>
//
// only when the plain form would overflow a varchar(255) disk_cid column.
// gzip (RFC 1952) is used over raw deflate for stock-tool inspectability
// (base64url decode | gunzip) and its CRC32 integrity check. If the payload is
// so incompressible that gzip does not shorten it, the plain form is returned
// even though it is still over the target; the caller (create_disk) is
// responsible for treating a still-over-target result as a hard error.
//
// Returns an error when bareCID is empty, for the same round-trip-totality
// reason as EncodeDiskCID.
func EncodeDiskCIDCompressed(bareCID string, meta *DiskCIDMeta) (string, error) {
	if bareCID == "" {
		return "", cpierrors.Cloud("EncodeDiskCIDCompressed: bareCID must not be empty; encoding an empty volid is a programming error in the caller")
	}
	b, err := marshalDiskCIDEnvelope(bareCID, meta)
	if err != nil {
		// Unreachable in practice (see EncodeDiskCID); keep the never-panic
		// contract.
		return "", cpierrors.Wrap(err, "EncodeDiskCIDCompressed: marshal envelope")
	}
	plain := diskCIDPrefix + base64.RawURLEncoding.EncodeToString(b)
	if len(plain) <= DiskCIDLengthTarget {
		return plain, nil
	}
	var buf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return plain, nil
	}
	if _, err := gw.Write(b); err != nil {
		return plain, nil
	}
	if err := gw.Close(); err != nil {
		return plain, nil
	}
	compressed := diskCIDCompressedPrefix + base64.RawURLEncoding.EncodeToString(buf.Bytes())
	if len(compressed) < len(plain) {
		return compressed, nil
	}
	return plain, nil
}

// ParseEncodedDiskCID decodes a disk CID into its bare volid component and the
// optional DiskCIDMeta.
//
// Accepted forms (the only two forms the CPI ever emits):
//   - "pvd-<base64url>" — envelope CID (default emitted format)
//   - "pvz-<base64url>" — gzip-compressed envelope CID (emitted only under the
//     opt-in disk_cid_compression property; decoded unconditionally)
//
// Any other input — including a bare PVE volid, an empty string, or any CID
// emitted by a release predating the pvd- envelope — is a hard parse error.
// This package has no backward-compatibility requirement: pre-release CIDs
// are never persisted by a Director this CPI has ever talked to.
//
// Returns an error when:
//   - cid is empty
//   - cid does not begin with "pvd-" or "pvz-"
//   - a pvd- payload is empty, not valid base64url, not valid JSON, or has an
//     empty "v" field
//   - a pvz- payload is not valid base64url, not a valid gzip stream, its
//     decompressed size exceeds maxDiskCIDEnvelopeBytes, or the decompressed
//     JSON is malformed or has an empty "v" field
//
// ParseDiskCID may be called on the returned bareCID without modification; it
// is guaranteed to be a valid "storage:volume" string when the original CID was
// well-formed (validation of the bare portion is left to ParseDiskCID itself so
// error messages are consistent).
func ParseEncodedDiskCID(cid string) (bareCID string, meta *DiskCIDMeta, err error) {
	if cid == "" {
		return "", nil, cpierrors.Cloud("disk CID must not be empty")
	}
	switch {
	case strings.HasPrefix(cid, diskCIDPrefix):
		return parseDiskCIDEnvelope(cid)
	case strings.HasPrefix(cid, diskCIDCompressedPrefix):
		return parseCompressedDiskCIDEnvelope(cid)
	default:
		return "", nil, cpierrors.Cloud(
			"invalid disk CID %q: expected a %q or %q envelope prefix", cid, diskCIDPrefix, diskCIDCompressedPrefix,
		)
	}
}

// parseDiskCIDEnvelope decodes the payload after diskCIDPrefix.
func parseDiskCIDEnvelope(cid string) (string, *DiskCIDMeta, error) {
	payload := cid[len(diskCIDPrefix):]
	if payload == "" {
		return "", nil, cpierrors.Cloud("invalid disk CID %q: envelope payload is empty", cid)
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, cpierrors.Cloud("invalid disk CID %q: envelope payload is not valid base64url: %v", cid, err)
	}
	return unmarshalDiskCIDEnvelope(cid, raw)
}

// parseCompressedDiskCIDEnvelope decodes the payload after
// diskCIDCompressedPrefix: base64url, then a size-capped gzip decompression,
// then the shared JSON envelope validation.
func parseCompressedDiskCIDEnvelope(cid string) (string, *DiskCIDMeta, error) {
	payload := cid[len(diskCIDCompressedPrefix):]
	if payload == "" {
		return "", nil, cpierrors.Cloud("invalid disk CID %q: compressed envelope payload is empty", cid)
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, cpierrors.Cloud("invalid disk CID %q: compressed envelope payload is not valid base64url: %v", cid, err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return "", nil, cpierrors.Cloud("invalid disk CID %q: compressed envelope is not a gzip stream: %v", cid, err)
	}
	// Read one byte past the cap so an over-limit envelope is detectable, and
	// close before the length check so the reader is always released.
	decompressed, readErr := io.ReadAll(io.LimitReader(gr, maxDiskCIDEnvelopeBytes+1))
	if cerr := gr.Close(); readErr == nil && cerr != nil {
		readErr = cerr
	}
	if readErr != nil {
		return "", nil, cpierrors.Cloud("invalid disk CID %q: compressed envelope decompression failed: %v", cid, readErr)
	}
	if len(decompressed) > maxDiskCIDEnvelopeBytes {
		return "", nil, cpierrors.Cloud("invalid disk CID %q: compressed envelope exceeds %d decompressed bytes", cid, maxDiskCIDEnvelopeBytes)
	}
	return unmarshalDiskCIDEnvelope(cid, decompressed)
}

// unmarshalDiskCIDEnvelope is the shared JSON tail of the pvd- and pvz-
// decoders: parse the envelope and require a non-empty volid.
func unmarshalDiskCIDEnvelope(cid string, raw []byte) (string, *DiskCIDMeta, error) {
	var env diskCIDEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", nil, cpierrors.Cloud("invalid disk CID %q: envelope JSON decode failed: %v", cid, err)
	}
	if env.V == "" {
		return "", nil, cpierrors.Cloud("invalid disk CID %q: envelope volid is empty", cid)
	}
	// A stable ID longer than PVE's drive-serial cap can never have been
	// written to a drive entry, so no volume can carry it and no scan can
	// resolve it — the CID is invalid, not merely unresolvable.
	if env.M != nil && len(env.M.ID) > DiskStableIDLen {
		return "", nil, cpierrors.Cloud(
			"invalid disk CID %q: stable ID %q exceeds the %d-byte PVE drive-serial cap",
			cid, env.M.ID, DiskStableIDLen)
	}
	return env.V, env.M, nil
}

// ParseSnapshotCID splits a snapshot CID of the form "<vm_cid>:<snap_name>" on the
// first colon. Returns an error if cid is empty or contains no colon.
func ParseSnapshotCID(cid string) (vmCID, snapName string, err error) {
	if cid == "" {
		return "", "", cpierrors.Cloud("snapshot CID must not be empty")
	}
	v, s, ok := strings.Cut(cid, ":")
	if !ok {
		return "", "", cpierrors.Cloud("invalid snapshot CID %q: expected format <vm_cid>:<snap_name>", cid)
	}
	if v == "" {
		return "", "", cpierrors.Cloud("invalid snapshot CID %q: vm_cid part must not be empty", cid)
	}
	if s == "" {
		return "", "", cpierrors.Cloud("invalid snapshot CID %q: snap_name part must not be empty", cid)
	}
	return v, s, nil
}

// FormatSnapshotCID joins vmCID and snapName into the canonical snapshot CID string.
func FormatSnapshotCID(vmCID, snapName string) string {
	return vmCID + ":" + snapName
}

// ResolveDiskID finds the PVE disk slot (e.g., "scsi1", "ide0") that holds volid
// on the specified VM. It calls QEMU().Config to retrieve the current VM config,
// then uses option-string-tolerant lookup to locate the slot: a config entry
// "data:vm-9003-disk-0,size=64G" matches volid "data:vm-9003-disk-0".
//
// Returns ("", err) wrapping ErrDiskNotAttached when volid is not present on
// any active bus slot of the VM. Callers may detect this case via
// errors.Is(err, ErrDiskNotAttached) to decide between idempotent success
// (detach_disk) and a hard cloud error (resize_disk, update_disk).
// Returns ("", err) when the Config call fails (the underlying error is
// wrapped via %w so callers may inspect it).
func ResolveDiskID(ctx context.Context, c Client, node string, vmid int, volid string) (string, error) {
	if node == "" {
		return "", cpierrors.Cloud("ResolveDiskID: node must not be empty")
	}
	if vmid <= 0 {
		return "", cpierrors.Cloud("ResolveDiskID: vmid must be positive, got %d", vmid)
	}
	if volid == "" {
		return "", cpierrors.Cloud("ResolveDiskID: volid must not be empty")
	}

	cfg, err := c.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return "", fmt.Errorf("ResolveDiskID: config fetch failed for VM %d on node %q: %w", vmid, node, err)
	}

	diskID, ok := FindDiskIDByVolID(qemu.ParseDisks(cfg), volid)
	if !ok {
		// Wrap the sentinel so callers can use errors.Is to distinguish a
		// not-attached disk from any other ResolveDiskID failure (config
		// fetch error, validation error). The human-readable prefix
		// preserves the original message shape for log readability.
		return "", fmt.Errorf("resolve disk %q on VM %d (node %q): %w",
			volid, vmid, node, ErrDiskNotAttached)
	}

	return diskID, nil
}

// FindVMByDiskVolid scans cluster VM resources to find the VMID + node whose
// QEMU config contains a disk entry matching volid. The volid may appear as
// the bare value or as the prefix before comma-separated options in a disk
// option string (e.g., "local-lvm:vm-100-disk-1" matches
// "local-lvm:vm-100-disk-1,cache=wb").
//
// fallbackNode is consulted only when a cluster resource entry omits the
// "node" field (rare in modern PVE); pass the configured default node so
// scans still work in single-node deployments where /cluster/resources may
// elide that field.
//
// Returns (vmid, node, nil) on success.
// Returns (0, "", cpierrors.Error) when:
//   - cluster resource listing fails (wrapped error).
//   - no VM holds the disk: cpierrors.Cloud("...disk not attached to any VM...").
func FindVMByDiskVolid(ctx context.Context, c Client, fallbackNode, volid string) (int, string, error) {
	vmid, node, _, err := FindVMByDiskVolidTagged(ctx, c, fallbackNode, volid)
	return vmid, node, err
}

// FindVMByDiskVolidTagged is FindVMByDiskVolid with the holder's tag string
// returned alongside its VMID and node.
//
// The scan reads the holder's config to decide it is the holder, so the tags
// are already in hand; a caller that needs to know what kind of VM it found --
// snapshot_disk asking whether the holder is a parker -- would otherwise pay a
// second read of a config this function just discarded.
func FindVMByDiskVolidTagged(ctx context.Context, c Client, fallbackNode, volid string) (int, string, string, error) {
	hit, err := findVMByDiskIdentityScan(ctx, c, fallbackNode, volid, "")
	return hit.VMID, hit.Node, hit.Tags, err
}

// DiskScanHit is one identity-scan result: the VM whose config references the
// disk, plus everything the scan read on its way there — the tag string, the
// bus slot, and the volid the config actually carries (which differs from the
// birth volid after a move_disk reassignment renamed the volume).
type DiskScanHit struct {
	VMID  int
	Node  string
	Tags  string
	Slot  string
	Volid string
}

// findVMByDiskIdentityScan is the single cluster-wide disk scan behind
// FindVMByDiskVolid and the stable-ID resolver. It reads every QEMU guest
// config once and matches each active-bus drive entry two ways in the same
// pass: by volid (exact or option-string prefix), and — when stableID is
// non-empty — by a serial=<stableID> drive option. The serial match is what
// finds a volume move_disk renamed, at zero extra API cost over the volid
// scan every caller already paid.
func findVMByDiskIdentityScan(ctx context.Context, c Client, fallbackNode, volid, stableID string) (DiskScanHit, error) {
	if c == nil {
		return DiskScanHit{}, cpierrors.Cloud("FindVMByDiskVolid: client must not be nil")
	}
	if volid == "" {
		return DiskScanHit{}, cpierrors.Cloud("FindVMByDiskVolid: volid must not be empty")
	}

	typeStr := "vm"
	var resources *sdkcluster.ListResourcesResponse
	listErr := RetryOnTransient(ctx, nil, "find_vm_by_disk_volid_list", 0, func() error {
		var inner error
		resources, inner = c.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &typeStr})
		return inner
	})
	if listErr != nil {
		// WrapError first, as ListParkersForNode does: the SDK error arriving
		// here is not a *cpierrors.Error, so wrapping it directly would fall
		// through to TypeCloud and label a corosync blip, a quorum loss, or a
		// pvedaemon restart as permanent. The Director would give up on
		// delete_disk and detach_disk instead of re-driving them. WrapError
		// makes the retriable/permanent split on what the error actually is.
		return DiskScanHit{}, cpierrors.Wrap(WrapError(listErr), "FindVMByDiskVolid: list cluster resources")
	}
	if resources == nil {
		// Retriable: a pvedaemon coming back up answers with an empty body, and
		// the scan now runs on delete_disk, attach_disk, and detach_disk alike,
		// so a permanent failure here fails all three during a restart that
		// resolves itself in seconds.
		return DiskScanHit{}, cpierrors.Retriable("FindVMByDiskVolid: nil response from cluster resources")
	}

	type resourceEntry struct {
		VMID int64  `json:"vmid"`
		Node string `json:"node"`
		Type string `json:"type"`
	}

	for _, raw := range *resources {
		var entry resourceEntry
		if jsonErr := json.Unmarshal(raw, &entry); jsonErr != nil || entry.VMID <= 0 {
			continue
		}

		// /cluster/resources?type=vm answers with LXC containers alongside QEMU
		// guests, and a container's config lives at a path the QEMU endpoint
		// cannot read: PVE returns a pmxcfs "Configuration file ... does not
		// exist" error, which is not a 404 and would abort the whole scan
		// retriably. One container anywhere in the cluster would then fail every
		// park, unpark, and holder probe until somebody deleted it. A row that
		// elides the field is kept, matching ListParkersForNode.
		if entry.Type != "" && entry.Type != clusterResourceTypeQemu {
			continue
		}

		vmNode := entry.Node
		if vmNode == "" {
			vmNode = fallbackNode
		}
		if vmNode == "" {
			// No node hint; cannot fetch config. Skip.
			continue
		}

		vmid := int(entry.VMID)
		cfg, cfgErr := c.QEMU().Config(ctx, vmNode, vmid)
		if cfgErr != nil {
			// Skip config-gone errors: the VM was deleted, or is a template whose
			// config was concurrently removed, or the row named a guest this
			// endpoint cannot read. Either way it holds no QEMU disk. Any other
			// error is potentially a fault on the VM that holds the disk, and
			// concluding "not attached to any VM" from it is how a volume ends
			// up attached twice, so it is returned rather than skipped.
			//
			// WrapError makes the retriable/permanent split on what the error
			// actually is: a 403 on one VM's config is a grant only a human can
			// add, and re-driving it forever helps nobody.
			// The pmxcfs skip is deliberately narrower than the not-found one:
			// it applies only to a row that elided "type". A row that named
			// itself qemu and then answers "Configuration file ... does not
			// exist" is most likely a guest mid-migration, whose .conf has moved
			// to another node while the row still names the old one -- and
			// concluding "not attached to any VM" from that is how a running
			// VM's volume gets attached to a second VM. Containers, the case the
			// skip exists for, are already filtered by type above; a row that
			// elides type is the only one that can still be one.
			if IsNotFound(cfgErr) || (entry.Type == "" && IsPmxcfsConfigMissing(cfgErr)) {
				continue
			}
			return DiskScanHit{}, cpierrors.Wrap(
				WrapConfigReadError(cfgErr),
				fmt.Sprintf("FindVMByDiskVolid: Config error for vm %d on node %s", vmid, vmNode),
			)
		}

		if slot, current, ok := matchDiskIdentity(qemu.ParseDisks(cfg), volid, stableID); ok {
			tags, _ := cfg["tags"].(string)
			return DiskScanHit{VMID: vmid, Node: vmNode, Tags: tags, Slot: slot, Volid: current}, nil
		}
	}

	return DiskScanHit{}, fmt.Errorf("disk %q: %w", volid, ErrDiskNotAttachedToAnyVM)
}

// matchDiskIdentity matches one parsed disk map against a disk identity: the
// volid itself (exact or "<volid>,options" prefix), or — when stableID is
// non-empty — a serial=<stableID> option on any entry. Returns the slot and
// the bare volid the entry actually carries.
func matchDiskIdentity(disks map[string]string, volid, stableID string) (slot, currentVolid string, ok bool) {
	for id, v := range disks {
		if v == volid || strings.HasPrefix(v, volid+",") {
			return id, volid, true
		}
		if stableID != "" {
			if serial, has := StableIDFromDriveOptStr(v); has && serial == stableID {
				bare := v
				if comma := strings.Index(v, ","); comma >= 0 {
					bare = v[:comma]
				}
				return id, bare, true
			}
		}
	}
	return "", "", false
}

// FindVMByDiskVolidOrNone is a wrapper around FindVMByDiskVolid that maps the
// "disk not attached to any VM" condition to (0, "", false, nil) rather than
// returning an error. All other errors (transport failures, transient faults)
// are passed through unchanged so callers receive retriable signals correctly.
//
// Detection uses errors.Is(err, ErrDiskNotAttachedToAnyVM) — the shared
// sentinel extracted alongside ErrDiskNotAttachedToAnyVM — so there is no
// substring matching on error messages.
//
// Existing callers of FindVMByDiskVolid are unaffected: they still receive the
// wrapped ErrDiskNotAttachedToAnyVM error (detectable via errors.Is) and the
// human-readable format is preserved.
func FindVMByDiskVolidOrNone(ctx context.Context, c Client, fallbackNode, volid string) (vmid int, node string, found bool, err error) {
	v, n, _, found, findErr := FindVMByDiskVolidOrNoneTagged(ctx, c, fallbackNode, volid)
	return v, n, found, findErr
}

// FindVMByDiskVolidOrNoneTagged is FindVMByDiskVolidOrNone with the holder's
// tag string, which the scan read on its way to identifying the holder. A
// caller that has to know what KIND of VM holds the volume gets that for free
// rather than issuing a second read of the same config -- a read whose own
// failure would otherwise have to be handled, on a path where the safe answer
// and the available answer are not the same.
func FindVMByDiskVolidOrNoneTagged(
	ctx context.Context, c Client, fallbackNode, volid string,
) (vmid int, node, tags string, found bool, err error) {
	v, n, t, findErr := FindVMByDiskVolidTagged(ctx, c, fallbackNode, volid)
	if findErr != nil {
		if errors.Is(findErr, ErrDiskNotAttachedToAnyVM) {
			return 0, "", "", false, nil
		}
		return 0, "", "", false, findErr
	}
	return v, n, t, true, nil
}

// DiskOptStrContainsVolid reports whether any entry in disks has a value that
// equals volid or begins with "volid," (option-string format). Exported for
// reuse by handlers that need to detect attachment without re-scanning.
func DiskOptStrContainsVolid(disks map[string]string, volid string) bool {
	for _, v := range disks {
		if v == volid || strings.HasPrefix(v, volid+",") {
			return true
		}
	}
	return false
}

// FindDiskIDByVolID returns the diskID (e.g. "scsi1") for the given volid by
// scanning a parsed disks map. Comparison tolerates PVE's option-string format:
// a config value of "data:vm-9003-disk-0,size=64G" matches volid
// "data:vm-9003-disk-0". The SDK's qemu.FindDiskIDByVolID does exact string
// match and silently misses these entries, causing the caller to treat the
// disk as not attached and re-attach it at a fresh slot — a duplicate that
// surfaces as "disk found on N VMs" at the next set_disk_metadata.
func FindDiskIDByVolID(disks map[string]string, volid string) (string, bool) {
	for id, v := range disks {
		if v == volid || strings.HasPrefix(v, volid+",") {
			return id, true
		}
	}
	return "", false
}

// FindVMNodeViaCluster returns the PVE node hosting vmid by querying
// /cluster/resources?type=vm. Returns (node, true, nil) on hit,
// ("", false, nil) when the VM is not present (e.g., not yet created), and
// ("", false, err) on transport failure.
//
// Exported so handlers can verify co-location (e.g., attach_disk under the
// local backend) without going through the full disk-scan in FindVMByDiskVolid.
func FindVMNodeViaCluster(ctx context.Context, c Client, vmid int) (string, bool, error) {
	node, _, found, err := FindVMViaCluster(ctx, c, vmid)
	return node, found, err
}

// FindVMViaCluster is FindVMNodeViaCluster plus the VM's tag string from the
// same single /cluster/resources scan — zero extra API calls. Callers that
// need provenance tags at delete time (advertised-route cleanup) use this
// variant; the tag string is PVE's semicolon-separated encoding, "" when the
// VM has no tags or the row omits the field.
func FindVMViaCluster(ctx context.Context, c Client, vmid int) (node, tags string, found bool, err error) {
	// A nil Cluster service is the expected case in unit-test mocks that
	// don't wire one (mirrors lookupVMStorageType's ClusterStorage guard):
	// report not-found so callers keep their fallback behavior.
	if c == nil || c.Cluster() == nil || vmid <= 0 {
		return "", "", false, nil
	}
	typ := "vm"
	var resp *sdkcluster.ListResourcesResponse
	listErr := RetryOnTransient(ctx, nil, "find_vm_node_via_cluster_list", 0, func() error {
		var inner error
		resp, inner = c.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &typ})
		return inner
	})
	if listErr != nil {
		// WrapError first — see FindVMByDiskVolidTagged. This leg feeds
		// delete_vm's VM lookup, so a permanent label here strands a delete
		// the Director would otherwise retry.
		return "", "", false, cpierrors.Wrap(WrapError(listErr), "findVMNodeViaCluster: list cluster vms")
	}
	if resp == nil {
		return "", "", false, nil
	}
	for _, raw := range *resp {
		var entry struct {
			VMID int64  `json:"vmid"`
			Node string `json:"node"`
			Tags string `json:"tags"`
		}
		if jsonErr := json.Unmarshal(raw, &entry); jsonErr != nil {
			continue
		}
		if int(entry.VMID) == vmid && entry.Node != "" {
			return entry.Node, entry.Tags, true, nil
		}
	}
	return "", "", false, nil
}

// FindVMPoolViaCluster returns the resource-pool membership of vmid from the
// same /cluster/resources?type=vm scan FindVMViaCluster performs, decoding
// the row's "pool" field instead of node/tags. PVE only populates "pool" on a
// resource-list row for a VM that is a member of a pool; a VM outside any
// pool omits the field entirely, so an empty string is a normal, expected
// result (not an error).
//
// Returns (poolID, true, nil) when the vmid row is found (poolID is "" when
// the row has no pool membership), ("", false, nil) when the vmid is not
// present in the cluster scan (already deleted or never existed), and
// ("", false, err) on transport failure.
//
// Kept as a dedicated function -- rather than folding into FindVMViaCluster --
// so that function's signature and every existing caller stay untouched; this
// extra field decode only runs when a caller (the delete_vm reaper) opts in.
func FindVMPoolViaCluster(ctx context.Context, c Client, vmid int) (pool string, found bool, err error) {
	// A nil Cluster service is the expected case in unit-test mocks that
	// don't wire one: report not-found so callers keep their fallback
	// (reaper simply no-ops).
	if c == nil || c.Cluster() == nil || vmid <= 0 {
		return "", false, nil
	}
	typ := "vm"
	var resp *sdkcluster.ListResourcesResponse
	listErr := RetryOnTransient(ctx, nil, "find_vm_pool_via_cluster_list", 0, func() error {
		var inner error
		resp, inner = c.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &typ})
		return inner
	})
	if listErr != nil {
		// WrapError first, for the same reason as its two siblings above: an
		// unclassified SDK error wraps to a permanent Cloud error.
		return "", false, cpierrors.Wrap(WrapError(listErr), "findVMPoolViaCluster: list cluster vms")
	}
	if resp == nil {
		return "", false, nil
	}
	for _, raw := range *resp {
		var entry struct {
			VMID int64  `json:"vmid"`
			Pool string `json:"pool"`
		}
		if jsonErr := json.Unmarshal(raw, &entry); jsonErr != nil {
			continue
		}
		if int(entry.VMID) == vmid {
			return entry.Pool, true, nil
		}
	}
	return "", false, nil
}
