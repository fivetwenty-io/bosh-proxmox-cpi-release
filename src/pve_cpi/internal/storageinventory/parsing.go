package storageinventory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// exactUint accepts canonical nonnegative integer JSON numbers and PVE's quoted
// integer form. Fractional/exponent syntax, signs, whitespace, overflow, and
// null never become a zero or an omitted fact.
func exactUint(raw json.RawMessage) (uint64, error) {
	text := string(bytes.TrimSpace(raw))
	if len(text) > 0 && text[0] == '"' {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return 0, err
		}
		text = v
	}
	if text == "" {
		return 0, fmt.Errorf("missing integer")
	}
	for _, c := range text {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid exact unsigned integer")
		}
	}
	if len(text) > 1 && text[0] == '0' {
		return 0, fmt.Errorf("noncanonical integer")
	}
	v, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("integer overflow: %w", err)
	}
	return v, nil
}

func exactBool(raw json.RawMessage) (bool, error) {
	switch string(bytes.TrimSpace(raw)) {
	case "true", "1", "\"1\"":
		return true, nil
	case "false", "0", "\"0\"":
		return false, nil
	default:
		return false, fmt.Errorf("expected boolean or exact 0/1")
	}
}

func parseDefinition(raw json.RawMessage) (pve.StorageInfo, error) {
	info, err := pve.ParseStorageEntry(raw)
	if err != nil {
		return pve.StorageInfo{}, err
	}
	if strings.TrimSpace(info.Name) == "" || strings.TrimSpace(info.Type) == "" {
		return pve.StorageInfo{}, fmt.Errorf("definition requires storage and type")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return pve.StorageInfo{}, err
	}
	if shared, present := fields["shared"]; present {
		if _, err := exactBool(shared); err != nil {
			return pve.StorageInfo{}, fmt.Errorf("shared: %w", err)
		}
	}
	for _, field := range []string{"storage", "type", "content", "server", "export", "share", "path", "nodes"} {
		if value, present := fields[field]; present && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return pve.StorageInfo{}, fmt.Errorf("definition %s must not be null", field)
		}
	}
	return info, nil
}

type storageStatus struct {
	id               string
	active, enabled  bool
	total, available uint64
}

func parseStatus(raw json.RawMessage) (storageStatus, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return storageStatus{}, err
	}
	var result storageStatus
	if err := json.Unmarshal(fields["storage"], &result.id); err != nil || strings.TrimSpace(result.id) == "" {
		return storageStatus{}, fmt.Errorf("status requires storage ID")
	}
	var err error
	result.active, err = exactBool(fields["active"])
	if err != nil {
		return result, fmt.Errorf("active: %w", err)
	}
	result.enabled, err = exactBool(fields["enabled"])
	if err != nil {
		return result, fmt.Errorf("enabled: %w", err)
	}
	// An explicitly inactive/disabled storage needs no invented capacity facts.
	if !result.active || !result.enabled {
		return result, nil
	}
	result.total, err = exactUint(fields["total"])
	if err != nil {
		return result, fmt.Errorf("total: %w", err)
	}
	result.available, err = exactUint(fields["avail"])
	if err != nil {
		return result, fmt.Errorf("avail: %w", err)
	}
	if result.total == 0 || result.available > result.total {
		return result, fmt.Errorf("impossible total/available statistics")
	}
	if used, present := fields["used"]; present {
		v, err := exactUint(used)
		if err != nil || v > result.total {
			return result, fmt.Errorf("invalid used statistics")
		}
	}
	return result, nil
}

func hasContent(content, wanted string) bool {
	for _, item := range strings.Split(content, ",") {
		if strings.TrimSpace(item) == wanted {
			return true
		}
	}
	return false
}
