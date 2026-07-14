// Disk CID parsing, formatting, and disk-slot resolution helpers used by
// detach_disk, resize_disk, and set_disk_metadata.
package pve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"

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
// are destroyed with the VM). Returned volids are bare (option suffix stripped).
func FindForeignActiveDisks(cfg map[string]any, ownerVMID int) map[string]string {
	out := make(map[string]string)
	for slot, optstr := range qemu.ParseDisks(cfg) {
		bare := optstr
		if comma := strings.Index(optstr, ","); comma >= 0 {
			bare = optstr[:comma]
		}
		vmid, ok := EmbeddedDiskVMID(bare)
		if !ok || vmid == ownerVMID {
			continue
		}
		out[slot] = bare
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
}

// diskCIDSep is the delimiter between the bare PVE volid and the optional
// base64url-encoded metadata suffix. PVE volids are always "<storage>:<name>"
// where name is "vm-<int>-disk-<int>" or a path — neither form ever contains
// a pipe character, so "|" is safe as an unambiguous separator.
const diskCIDSep = "|"

// EncodeDiskCID appends a base64url-JSON metadata suffix to a bare disk CID.
//
// If meta is nil or all fields are zero-valued the bare CID is returned
// unchanged, preserving backward compatibility with consumers that store the
// string directly.
//
// The encoded form is:
//
//	<bare-cid>|<base64url(json(meta))>
//
// where base64url uses standard RFC 4648 §5 encoding with no padding (URL-safe
// alphabet, no '=' padding characters).
func EncodeDiskCID(bareCID string, meta *DiskCIDMeta) string {
	if meta == nil || (meta.Pool == "" && meta.Node == "" && meta.AZ == "" && len(meta.Opts) == 0) {
		return bareCID
	}
	b, err := json.Marshal(meta)
	if err != nil {
		// json.Marshal on a plain struct never returns an error; guard anyway
		// to satisfy the contract that EncodeDiskCID never panics.
		return bareCID
	}
	return bareCID + diskCIDSep + base64.RawURLEncoding.EncodeToString(b)
}

// ParseEncodedDiskCID splits an optionally-annotated disk CID into its bare
// volid component and the optional DiskCIDMeta.
//
// Accepted forms:
//   - "<storage>:<volid>"          — bare legacy CID; returns meta=nil, err=nil
//   - "<storage>:<volid>|<base64>" — annotated CID; decodes suffix into meta
//
// Returns an error when:
//   - cid is empty
//   - the "|" separator is present but the suffix is empty, not valid base64url,
//     or not valid JSON for DiskCIDMeta
//
// ParseDiskCID may be called on the returned bareCID without modification; it
// is guaranteed to be a valid "storage:volume" string when the original CID was
// well-formed (validation of the bare portion is left to ParseDiskCID itself so
// error messages are consistent).
func ParseEncodedDiskCID(cid string) (bareCID string, meta *DiskCIDMeta, err error) {
	if cid == "" {
		return "", nil, cpierrors.Cloud("disk CID must not be empty")
	}
	idx := strings.Index(cid, diskCIDSep)
	if idx < 0 {
		// No separator — bare legacy CID; meta is absent.
		return cid, nil, nil
	}
	bareCID = cid[:idx]
	suffix := cid[idx+len(diskCIDSep):]
	if suffix == "" {
		return "", nil, cpierrors.Cloud("invalid disk CID %q: pipe separator present but suffix is empty", cid)
	}
	raw, decErr := base64.RawURLEncoding.DecodeString(suffix)
	if decErr != nil {
		return "", nil, cpierrors.Cloud("invalid disk CID %q: suffix is not valid base64url: %v", cid, decErr)
	}
	var m DiskCIDMeta
	if jsonErr := json.Unmarshal(raw, &m); jsonErr != nil {
		return "", nil, cpierrors.Cloud("invalid disk CID %q: suffix JSON decode failed: %v", cid, jsonErr)
	}
	return bareCID, &m, nil
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
	if c == nil {
		return 0, "", cpierrors.Cloud("FindVMByDiskVolid: client must not be nil")
	}
	if volid == "" {
		return 0, "", cpierrors.Cloud("FindVMByDiskVolid: volid must not be empty")
	}

	typeStr := "vm"
	var resources *sdkcluster.ListResourcesResponse
	listErr := RetryOnTransient(ctx, nil, "find_vm_by_disk_volid_list", 0, func() error {
		var inner error
		resources, inner = c.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &typeStr})
		return inner
	})
	if listErr != nil {
		return 0, "", cpierrors.Wrap(listErr, "FindVMByDiskVolid: list cluster resources")
	}
	if resources == nil {
		return 0, "", cpierrors.Cloud("FindVMByDiskVolid: nil response from cluster resources")
	}

	type resourceEntry struct {
		VMID int64  `json:"vmid"`
		Node string `json:"node"`
	}

	for _, raw := range *resources {
		var entry resourceEntry
		if jsonErr := json.Unmarshal(raw, &entry); jsonErr != nil || entry.VMID <= 0 {
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
			// Skip only not-found-style errors: the VM was deleted or is a template
			// whose config was concurrently removed. Any other error is potentially
			// a transient fault on the VM that holds the disk; returning it as a
			// retriable error lets the caller retry rather than producing a false
			// "disk not attached to any VM" result.
			if IsNotFound(cfgErr) {
				continue
			}
			return 0, "", cpierrors.WrapAs(
				cfgErr,
				cpierrors.TypeRetriableCloud,
				fmt.Sprintf("FindVMByDiskVolid: transient Config error for vm %d on node %s", vmid, vmNode),
			)
		}

		if DiskOptStrContainsVolid(qemu.ParseDisks(cfg), volid) {
			return vmid, vmNode, nil
		}
	}

	return 0, "", fmt.Errorf("disk %q: %w", volid, ErrDiskNotAttachedToAnyVM)
}

// FindVMByDiskVolidOrNone is a wrapper around FindVMByDiskVolid that maps the
// "disk not attached to any VM" condition to (0, "", false, nil) rather than
// returning an error. All other errors (transport failures, transient faults)
// are passed through unchanged so callers receive retriable signals correctly.
//
// Detection uses errors.Is(err, ErrDiskNotAttachedToAnyVM) — the shared
// sentinel extracted alongside ErrDiskNotAttachedToAnyVM — so there is no
// substring matching on error messages (W2 F-14).
//
// Existing callers of FindVMByDiskVolid are unaffected: they still receive the
// wrapped ErrDiskNotAttachedToAnyVM error (detectable via errors.Is) and the
// human-readable format is preserved.
func FindVMByDiskVolidOrNone(ctx context.Context, c Client, fallbackNode, volid string) (vmid int, node string, found bool, err error) {
	v, n, findErr := FindVMByDiskVolid(ctx, c, fallbackNode, volid)
	if findErr != nil {
		if errors.Is(findErr, ErrDiskNotAttachedToAnyVM) {
			return 0, "", false, nil
		}
		return 0, "", false, findErr
	}
	return v, n, true, nil
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
	if c == nil || vmid <= 0 {
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
		return "", "", false, cpierrors.Wrap(listErr, "findVMNodeViaCluster: list cluster vms")
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
