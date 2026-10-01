package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// PendingFromConfig turns a fake's config map into the response PVE's pending
// endpoint (GET /nodes/{node}/qemu/{vmid}/pending) gives for a VM with no
// pending changes. Like config_with_pending_array, it emits one item for every
// config key, digest and description included, with the key's current value
// and no pending value or delete flag. Fakes serve their pending endpoint
// through it, so a test fixture written for the config read describes the
// same VM to both reads.
//
// It's exported so the external test package can use it too.
func PendingFromConfig(cfg map[string]any) *nodes.ListQemuPendingResponse {
	keys := make([]string, 0, len(cfg))
	for key := range cfg {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make(nodes.ListQemuPendingResponse, 0, len(keys))
	for _, key := range keys {
		raw, err := json.Marshal(map[string]any{"key": key, "value": cfg[key]})
		if err != nil {
			panic(fmt.Sprintf("PendingFromConfig: key %q: %v", key, err))
		}
		out = append(out, raw)
	}
	return &out
}

// PendingFromConfigRead serves a fake's pending endpoint from the fake's own
// config read, so the pending read fails exactly where the config read fails,
// with the same error, and an injected error or a call count keeps its
// meaning.
func PendingFromConfigRead(
	ctx context.Context,
	read func(ctx context.Context, node string, vmid int) (map[string]any, error),
	node, vmid string,
) (*nodes.ListQemuPendingResponse, error) {
	id, err := strconv.Atoi(vmid)
	if err != nil {
		return nil, fmt.Errorf("fake pending endpoint: vmid %q: %w", vmid, err)
	}
	cfg, err := read(ctx, node, id)
	if err != nil {
		return nil, err
	}
	return PendingFromConfig(cfg), nil
}

// pendingMirrorsConfig reports how resp differs from what PVE's pending
// endpoint returns for cfg with no pending changes: one item per config key
// with the key's current value, digest and description included, and no
// pending value or delete flag.
func pendingMirrorsConfig(resp *nodes.ListQemuPendingResponse, cfg map[string]any) error {
	if resp == nil {
		return errors.New("nil response")
	}
	seen := map[string]bool{}
	for _, raw := range *resp {
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			return fmt.Errorf("item %s: %w", raw, err)
		}
		key, _ := item["key"].(string)
		want, ok := cfg[key]
		if !ok {
			return fmt.Errorf("item %s names a key the config doesn't have", raw)
		}
		if seen[key] {
			return fmt.Errorf("item %s repeats key %q", raw, key)
		}
		seen[key] = true
		if item["value"] != want {
			return fmt.Errorf("item %s carries value %v, want the current value %v", raw, item["value"], want)
		}
		for _, field := range []string{"pending", "delete"} {
			if _, present := item[field]; present {
				return fmt.Errorf("item %s carries %q, want no pending change", raw, field)
			}
		}
		if len(item) != 2 {
			return fmt.Errorf("item %s carries fields beyond key and value", raw)
		}
	}
	for key := range cfg {
		if !seen[key] {
			return fmt.Errorf("no item for config key %q", key)
		}
	}
	return nil
}

// TestPendingFromConfigMirrorsConfigWithPendingArray checks this package's
// copy of the helper: one item per config key with the key's current value,
// digest and description included, and no pending value or delete flag. It
// also proves the check catches each way a response can drift. Each package
// carries its own copy of the row, so a helper that drifts in one package
// can't hide behind a passing row in another.
func TestPendingFromConfigMirrorsConfigWithPendingArray(t *testing.T) {
	cfg := map[string]any{
		"scsi1":       "a:123/vm-123-disk-0.raw,serial=bpd-0011223344556677,size=5G",
		"unused0":     "a:777/vm-777-disk-1.raw",
		"digest":      "0123456789abcdef",
		"description": "<!--BOSH:{}-->",
		"cores":       float64(2),
	}
	resp := PendingFromConfig(cfg)
	if err := pendingMirrorsConfig(resp, cfg); err != nil {
		t.Fatalf("PendingFromConfig: %v", err)
	}

	drifted := map[string]func(nodes.ListQemuPendingResponse) nodes.ListQemuPendingResponse{
		"missing item": func(r nodes.ListQemuPendingResponse) nodes.ListQemuPendingResponse {
			return r[1:]
		},
		"extra key": func(r nodes.ListQemuPendingResponse) nodes.ListQemuPendingResponse {
			return append(r, json.RawMessage(`{"key":"scsi9","value":"a:1/vm-1-disk-9.raw"}`))
		},
		"pending value": func(r nodes.ListQemuPendingResponse) nodes.ListQemuPendingResponse {
			return append(r[1:], json.RawMessage(`{"key":"cores","value":2,"pending":4}`))
		},
		"delete flag": func(r nodes.ListQemuPendingResponse) nodes.ListQemuPendingResponse {
			return append(r[1:], json.RawMessage(`{"key":"cores","value":2,"delete":1}`))
		},
	}
	for name, drift := range drifted {
		t.Run(name, func(t *testing.T) {
			base := PendingFromConfig(cfg)
			// Item 0 is "cores", the first key in sorted order, which the
			// pending and delete cases replace.
			bad := drift(append(nodes.ListQemuPendingResponse(nil), (*base)...))
			if err := pendingMirrorsConfig(&bad, cfg); err == nil {
				t.Fatalf("the check accepted a response with a %s", name)
			}
		})
	}

	injected := errors.New("injected config read failure")
	if _, err := PendingFromConfigRead(context.Background(), func(context.Context, string, int) (map[string]any, error) {
		return nil, injected
	}, "n1", "777"); !errors.Is(err, injected) {
		t.Fatalf("PendingFromConfigRead = %v, want the config read's own error", err)
	}
}
