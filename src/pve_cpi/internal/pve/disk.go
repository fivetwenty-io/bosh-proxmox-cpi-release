// Package pve provides disk CID parsing, formatting, and disk-slot resolution
// helpers used by detach_disk, resize_disk, and set_disk_metadata.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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

// unusedDiskKeyPattern matches PVE "unusedN" config keys. PVE moves a disk
// to such a slot when it is removed from its bus slot (e.g., scsi1) via
// PUT config delete:scsi1, instead of fully clearing the entry. Persistent
// volumes still owned by BOSH but holding such an entry will be destroyed
// by the next DELETE /qemu/{vmid}; delete_vm therefore refuses to issue
// the destroy when an unusedN entry references a volume on the configured
// pve_disk_storage. (The DetachDisk SDK call cleans these up on its own;
// this guard catches paths where DetachDisk was bypassed or failed mid-way.)
var unusedDiskKeyPattern = regexp.MustCompile(`^unused\d+$`)

// FindUnusedDiskEntries returns every (slot, volid) pair in cfg whose key
// matches "unusedN" and whose value is a non-empty string. The returned
// volids are bare (any ",options" suffix is stripped) for direct equality
// comparison with storage-prefixed volume identifiers.
func FindUnusedDiskEntries(cfg map[string]interface{}) map[string]string {
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
			// Skip VMs whose config cannot be fetched (templates, transient errors).
			continue
		}

		if DiskOptStrContainsVolid(qemu.ParseDisks(cfg), volid) {
			return vmid, vmNode, nil
		}
	}

	return 0, "", cpierrors.Cloud(
		"disk %q not attached to any VM", volid,
	)
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
	if c == nil || vmid <= 0 {
		return "", false, nil
	}
	typ := "vm"
	var resp *sdkcluster.ListResourcesResponse
	listErr := RetryOnTransient(ctx, nil, "find_vm_node_via_cluster_list", 0, func() error {
		var inner error
		resp, inner = c.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &typ})
		return inner
	})
	if listErr != nil {
		return "", false, cpierrors.Wrap(listErr, "findVMNodeViaCluster: list cluster vms")
	}
	if resp == nil {
		return "", false, nil
	}
	for _, raw := range *resp {
		var entry struct {
			VMID int64  `json:"vmid"`
			Node string `json:"node"`
		}
		if jsonErr := json.Unmarshal(raw, &entry); jsonErr != nil {
			continue
		}
		if int(entry.VMID) == vmid && entry.Node != "" {
			return entry.Node, true, nil
		}
	}
	return "", false, nil
}
