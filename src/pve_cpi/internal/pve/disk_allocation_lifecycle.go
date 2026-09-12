package pve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

const diskAllocationsKey = "bosh_disk_allocations"

// DiskAllocationProvenance corroborates one managed disk's current location.
// Neither this marker nor its stable token authorizes deletion without the full
// journal, cluster identity, and actual resource ownership checks.
type DiskAllocationProvenance struct {
	Version             int    `json:"version"`
	AllocationID        string `json:"allocation_id"`
	AllocationNamespace string `json:"allocation_namespace"`
	Volid               string `json:"volid"`
	Node                string `json:"node"`
	Backing             string `json:"backing"`
}

func validateDiskAllocationProvenance(p DiskAllocationProvenance) error {
	if p.Version != 1 || !allocationUUID.MatchString(p.AllocationID) || p.AllocationNamespace == "" || strings.TrimSpace(p.AllocationNamespace) != p.AllocationNamespace || len(p.AllocationNamespace) > 1024 || strings.ContainsAny(p.AllocationNamespace, "/\\\x00\r\n") || p.AllocationNamespace == "." || p.AllocationNamespace == ".." || p.Node == "" || p.Backing == "" {
		return fmt.Errorf("invalid managed disk provenance")
	}
	if _, _, err := ParseDiskCID(p.Volid); err != nil {
		return fmt.Errorf("invalid managed disk volume identity")
	}
	return nil
}
func strictDiskAllocationSentinel(desc string) (string, map[string]json.RawMessage, error) {
	matches := sentinelPattern.FindAllStringSubmatchIndex(desc, -1)
	if len(matches) == 0 {
		if strings.Contains(desc, "<!--BOSH:") {
			return "", nil, fmt.Errorf("malformed disk provenance sentinel")
		}
		return desc, map[string]json.RawMessage{}, nil
	}
	if len(matches) != 1 || strings.Count(desc, "<!--BOSH:") != 1 {
		return "", nil, fmt.Errorf("ambiguous disk provenance sentinel")
	}
	m := matches[0]
	var raw map[string]json.RawMessage
	if err := decodeUniqueJSON([]byte(desc[m[2]:m[3]]), &raw); err != nil || raw == nil {
		return "", nil, fmt.Errorf("malformed disk provenance sentinel")
	}
	// Preserve text both before and after the shared sentinel, including the VM
	// allocation marker which can coexist outside the legacy HTML sentinel.
	other := strings.TrimSpace(desc[:m[0]] + desc[m[1]:])
	return other, raw, nil
}

// decodeUniqueJSON rejects duplicate fields at every nesting level and leaves
// schema validation to the destination decoder. Error text never echoes data.
func decodeUniqueJSON(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := uniqueJSONValue(d); err != nil {
		return fmt.Errorf("ambiguous JSON provenance")
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON provenance")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("malformed JSON provenance")
	}
	return nil
}
func uniqueJSONValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := k.(string)
			if !ok || seen[key] {
				return fmt.Errorf("duplicate field")
			}
			seen[key] = true
			if err := uniqueJSONValue(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueJSONValue(d); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected delimiter")
	}
	_, err = d.Token()
	return err
}

// ParseDiskAllocationProvenance validates every managed disk ownership entry.
func ParseDiskAllocationProvenance(description string) (map[string]DiskAllocationProvenance, error) {
	_, raw, err := strictDiskAllocationSentinel(description)
	if err != nil {
		return nil, err
	}
	result := map[string]DiskAllocationProvenance{}
	encoded, ok := raw[diskAllocationsKey]
	if !ok {
		return result, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || result == nil {
		return nil, fmt.Errorf("malformed managed disk provenance")
	}
	for key, p := range result {
		if key == "" {
			return nil, fmt.Errorf("empty managed disk provenance key")
		}
		if err := validateDiskAllocationProvenance(p); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// WriteDiskAllocationProvenance preserves unrelated metadata and verifies the
// full receiving-side identity before a caller may erase source provenance.
func WriteDiskAllocationProvenance(ctx context.Context, c Client, node string, vmid int, key string, entry DiskAllocationProvenance) error {
	return updateDiskAllocationProvenance(ctx, c, node, vmid, key, entry, false)
}

// RemoveDiskAllocationProvenance removes only a matching allocation identity.
func RemoveDiskAllocationProvenance(ctx context.Context, c Client, node string, vmid int, key string, expected DiskAllocationProvenance) error {
	return updateDiskAllocationProvenance(ctx, c, node, vmid, key, expected, true)
}
func updateDiskAllocationProvenance(ctx context.Context, c Client, node string, vmid int, key string, entry DiskAllocationProvenance, remove bool) error {
	if c == nil || c.Nodes() == nil || c.QEMU() == nil || node == "" || vmid <= 0 || key == "" {
		return fmt.Errorf("managed disk provenance requires a concrete holder")
	}
	if err := validateDiskAllocationProvenance(entry); err != nil {
		return err
	}
	cfg, err := c.QEMU().Config(ctx, node, vmid)
	if err != nil || cfg == nil {
		return fmt.Errorf("cannot read managed disk provenance holder")
	}
	description := DescriptionFromConfig(cfg)
	nonBOSH, raw, err := strictDiskAllocationSentinel(description)
	if err != nil {
		return err
	}
	entries, err := ParseDiskAllocationProvenance(description)
	if err != nil {
		return err
	}
	prior, exists := entries[key]
	if exists && (prior.AllocationID != entry.AllocationID || prior.AllocationNamespace != entry.AllocationNamespace) {
		return fmt.Errorf("managed disk provenance identity conflicts")
	}
	if remove && exists && prior != entry {
		return fmt.Errorf("managed disk provenance location changed before removal")
	}
	if remove {
		if !exists {
			return nil
		}
		delete(entries, key)
	} else {
		entries[key] = entry
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		delete(raw, diskAllocationsKey)
	} else {
		raw[diskAllocationsKey] = encoded
	}
	newDesc, err := RenderSentinel(nonBOSH, raw)
	if err != nil {
		return err
	}
	params := &sdknodes.UpdateQemuConfigParams{Description: &newDesc}
	if digest, ok := ConfigString(cfg, "digest"); ok && digest != "" {
		params.Digest = &digest
	}
	if err := c.Nodes().UpdateQemuConfig(ctx, node, strconv.Itoa(vmid), params); err != nil {
		return fmt.Errorf("cannot persist managed disk provenance")
	}
	check, err := c.QEMU().Config(ctx, node, vmid)
	if err != nil || check == nil {
		return fmt.Errorf("cannot verify managed disk provenance")
	}
	observed, err := ParseDiskAllocationProvenance(DescriptionFromConfig(check))
	if err != nil {
		return err
	}
	actual, found := observed[key]
	if remove && found || !remove && (!found || actual != entry) {
		return fmt.Errorf("managed disk provenance readback mismatch")
	}
	return nil
}

// FindDiskAllocationProvenance reads the managed identity from the current
// workload record or legacy parker record. Missing backing in an initial parker
// entry must be corroborated against the journal and actual storage definition.
func FindDiskAllocationProvenance(description, key string) (DiskAllocationProvenance, bool, error) {
	entries, err := ParseDiskAllocationProvenance(description)
	if err != nil {
		return DiskAllocationProvenance{}, false, err
	}
	current, currentFound := entries[key]
	_, raw, err := strictDiskAllocationSentinel(description)
	if err != nil {
		return DiskAllocationProvenance{}, false, err
	}
	encoded, ok := raw["bosh_parked_disks"]
	if !ok {
		return current, currentFound, nil
	}
	var parked map[string]parkerProvEntry
	if err := decodeUniqueJSON(encoded, &parked); err != nil {
		return DiskAllocationProvenance{}, false, err
	}
	old, ok := parked[key]
	if !ok || old.AllocationID == "" && old.AllocationNamespace == "" {
		return current, currentFound, nil
	}
	entry := DiskAllocationProvenance{Version: 1, AllocationID: old.AllocationID, AllocationNamespace: old.AllocationNamespace, Volid: old.Volid, Node: old.Node, Backing: old.AllocationBacking}
	// Old initial parker entries omitted the backing. Validate every other field
	// identically; the caller must corroborate the missing backing externally.
	check := entry
	if check.Backing == "" {
		check.Backing = "legacy-unrecorded"
	}
	if err := validateDiskAllocationProvenance(check); err != nil {
		return DiskAllocationProvenance{}, false, fmt.Errorf("malformed parked allocation provenance")
	}
	if currentFound {
		if current.AllocationID != entry.AllocationID || current.AllocationNamespace != entry.AllocationNamespace || current.Volid != entry.Volid || current.Node != entry.Node || entry.Backing != "" && current.Backing != entry.Backing {
			return DiskAllocationProvenance{}, false, fmt.Errorf("conflicting managed disk provenance carriers")
		}
		return current, true, nil
	}
	return entry, true, nil
}
