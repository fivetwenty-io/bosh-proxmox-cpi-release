package handlers

import (
	"encoding/json"
	"fmt"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"sort"
	"strconv"
	"strings"
)

// managedMutationUPID accepts only the SDK's supported asynchronous result
// shapes. An empty or malformed response never proves non-submission.
func managedMutationUPID(result any) (string, error) {
	var upid string
	switch v := result.(type) {
	case string:
		upid = v
	case *json.RawMessage:
		if v == nil {
			return "", fmt.Errorf("missing mutation task result")
		}
		var err error
		upid, err = pve.UPIDFromRaw(*v)
		if err != nil {
			return "", fmt.Errorf("malformed mutation task result")
		}
	case json.RawMessage:
		var err error
		upid, err = pve.UPIDFromRaw(v)
		if err != nil {
			return "", fmt.Errorf("malformed mutation task result")
		}
	default:
		return "", fmt.Errorf("unsupported mutation task result")
	}
	if !strings.HasPrefix(upid, "UPID:") || strings.TrimSpace(upid) != upid || strings.ContainsAny(upid, "\r\n") {
		return "", fmt.Errorf("missing or malformed mutation task identity")
	}
	return upid, nil
}

// managedConfigFieldsMatch compares the exact writable fields returned by the
// typed SDK's JSON encoder against authoritative QEMU config. It handles PVE's
// scalar encodings and property-string order without logging any values.
func managedConfigFieldsMatch(config map[string]any, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("cannot encode requested VM config")
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil || len(fields) == 0 {
		return fmt.Errorf("empty or malformed requested VM config")
	}
	for key, want := range fields {
		if key == "digest" {
			continue
		}
		if key == "delete" {
			text, ok := want.(string)
			if !ok {
				return fmt.Errorf("invalid config deletion fields")
			}
			for _, deleted := range strings.Split(text, ",") {
				if _, present := config[deleted]; present {
					return fmt.Errorf("VM config deletion not observed for %s", deleted)
				}
			}
			continue
		}
		got, ok := config[key]
		if !ok {
			// PVE omits description after clearing its last text. Only this
			// explicit clear permits absence; identity fields remain required.
			if requested, isString := want.(string); key == pveConfigKeyDescription && isString && requested == "" {
				continue
			}
			return fmt.Errorf("VM config field %s missing from readback", key)
		}
		if managedConfigReadback(key, got, want) != managedConfigCanonical(key, want) {
			return fmt.Errorf("VM config field %s differs from requested value", key)
		}
	}
	return nil
}

func managedConfigReadback(key string, got, want any) string {
	actual, actualString := got.(string)
	requested, requestedString := want.(string)
	// PVE omits the formatter's single final LF when reading a VM marker
	// description. Every marker byte and surrounding text remains exact.
	if key == pveConfigKeyDescription && actualString && requestedString && strings.HasSuffix(requested, "\n") && !strings.HasSuffix(actual, "\n") && actual == strings.TrimSuffix(requested, "\n") {
		if _, found, err := pve.ParseStorageAllocationMarker(requested); err == nil && found {
			return requested
		}
	}
	if !managedVMVolumeDevice(key) || !actualString || !requestedString {
		return managedConfigCanonical(key, got)
	}
	for _, option := range strings.Split(requested, ",") {
		if strings.HasPrefix(option, "size=") {
			return managedConfigCanonical(key, got)
		}
	}
	// PVE appends the image's current size to drive readback even when a
	// config update only changes its options. Physical size is verified by
	// the allocation observer; every requested drive property remains exact.
	parts := strings.Split(actual, ",")
	kept := make([]string, 0, len(parts))
	sizes := 0
	for _, option := range parts {
		if strings.HasPrefix(option, "size=") {
			sizes++
			continue
		}
		kept = append(kept, option)
	}
	size, err := parseDiskSizeGiB(actual)
	if sizes != 1 || err != nil || size <= 0 {
		return managedConfigCanonical(key, got)
	}
	return managedConfigCanonical(key, strings.Join(kept, ","))
}

func managedConfigCanonical(key string, value any) string {
	var text string
	switch v := value.(type) {
	case string:
		text = v
	case bool:
		if v {
			return "1"
		}
		return "0"
	case float64:
		text = strconv.FormatFloat(v, 'f', -1, 64)
	default:
		text = fmt.Sprint(value)
	}
	if key == pveConfigKeyDescription || key == "name" {
		return text
	}
	if key == "tags" {
		parts := strings.Split(text, ";")
		sort.Strings(parts)
		return strings.Join(parts, ";")
	}
	if strings.Contains(text, "=") {
		parts := strings.Split(text, ",")
		sort.Strings(parts)
		return strings.Join(parts, ",")
	}
	return text
}
