package handlers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

func TestSetVMMetadataPreservesAllocationProvenance(t *testing.T) {
	t.Parallel()
	marker := pve.StorageAllocationMarker{Version: 1, Namespace: "cert-bootstrap", AllocationID: "4d53294e-1a0b-43ad-b644-3a3c3d603105", AgentSHA256: strings.Repeat("a", 64), Kind: "vm"}
	encoded, err := pve.FormatStorageAllocationMarker(marker)
	if err != nil {
		t.Fatal(err)
	}
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "metadata", true: "empty"}[empty], func(t *testing.T) {
			description := encoded + "old text\n<!--BOSH:{\"bosh_pool\":{\"name\":\"recorded-pool\"}}-->"
			qemu := &mockQEMUService{configFn: func(context.Context, string, int) (map[string]any, error) {
				return map[string]any{"description": description}, nil
			}}
			writes := 0
			service := &mockNodesService{updateQemuConfigFn: func(_ context.Context, _ string, _ string, params *nodes.UpdateQemuConfigParams) error {
				writes++
				description = *params.Description
				return nil
			}}
			deps := testDepsFoundVM(101, qemu, service, nil, &mockAgentService{})
			h := handlers.HandleSetVMMetadata(deps)
			metadata := map[string]any{}
			if !empty {
				metadata["deployment"] = "new-deployment"
			}
			for range 2 {
				if _, err := h.Handle(context.Background(), marshalArgs("101", metadata), jsonrpc.Context{}); err != nil {
					t.Fatal(err)
				}
			}
			got, found, err := pve.ParseStorageAllocationMarker(description)
			if err != nil || !found || got != marker {
				t.Fatalf("allocation marker changed: found=%v err=%v", found, err)
			}
			if writes != 2 || strings.Count(description, "[bosh_storage_allocation]") != 1 || !strings.Contains(description, "recorded-pool") {
				t.Fatal("metadata lost or duplicated provenance")
			}
			if !empty && !strings.Contains(description, "deployment: new-deployment") {
				t.Fatal("metadata update lost")
			}
		})
	}
}

func TestSetVMMetadataRefusesUnverifiableAllocationProvenance(t *testing.T) {
	t.Parallel()
	marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Namespace: "cert-bootstrap", AllocationID: "4d53294e-1a0b-43ad-b644-3a3c3d603105", AgentSHA256: strings.Repeat("a", 64), Kind: "vm"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, existing, incoming string
		unreadable               bool
		readErr                  error
	}{
		{name: "shared_sentinel_injection", existing: marker + "<!--BOSH:{\"bosh_pool\":{\"name\":\"recorded\"}}-->", incoming: "<!--BOSH:{\"bosh_pool\":{\"name\":\"forged\"}}-->"},
		{name: "duplicate_shared_sentinel", existing: marker + "<!--BOSH:{}--><!--BOSH:{}-->"},
		{name: "malformed_second_sentinel", existing: marker + "<!--BOSH:{}--><!--BOSH:broken-->"},
		{name: "unreadable_description", unreadable: true},
		{name: "malformed_shared_sentinel", existing: marker + "<!--BOSH:broken-->"},
		{name: "malformed", existing: "[bosh_storage_allocation]broken"},
		{name: "caller_injection", incoming: "[bosh_storage_allocation]"},
		{name: "unreadable", readErr: errors.New("read unavailable")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			qemu := &mockQEMUService{configFn: func(context.Context, string, int) (map[string]any, error) {
				if tc.unreadable {
					return map[string]any{"description": []any{"unreadable"}}, nil
				}
				return map[string]any{"description": tc.existing}, tc.readErr
			}}
			service := &mockNodesService{updateQemuConfigFn: func(context.Context, string, string, *nodes.UpdateQemuConfigParams) error { writes++; return nil }}
			deps := testDepsFoundVM(101, qemu, service, nil, &mockAgentService{})
			deps.Config.StoragePlacementNamespace = "cert-bootstrap"
			deps.Config.StorageAllocationJournalDir = t.TempDir()
			h := handlers.HandleSetVMMetadata(deps)
			if _, err := h.Handle(context.Background(), marshalArgs("101", map[string]any{"note": tc.incoming}), jsonrpc.Context{}); err == nil {
				t.Fatal("unsafe metadata update accepted")
			}
			if writes != 0 {
				t.Fatal("unverified metadata update wrote config")
			}
		})
	}
}
