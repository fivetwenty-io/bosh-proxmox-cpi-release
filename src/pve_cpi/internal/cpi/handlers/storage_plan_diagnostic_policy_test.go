package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
)

type diagnosticAliasClient struct{ pve.Client }

func (c diagnosticAliasClient) ClusterStorage() clusterstorage.Service {
	return diagnosticAliasDefinitions{}
}

type diagnosticAliasDefinitions struct{ managedDiskTestDefinitions }

func (diagnosticAliasDefinitions) ListStorage(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	rows := clusterstorage.ListStorageResponse{
		json.RawMessage(`{"storage":"a","type":"nfs","server":"nas","export":"/same","shared":1,"content":"images"}`),
		json.RawMessage(`{"storage":"b","type":"nfs","server":"nas","export":"/same","shared":1,"content":"images"}`),
	}
	return &rows, nil
}

func TestStoragePlanDiagnosticClassifiesRealPolicyErrorsWithoutRawDetails(t *testing.T) {
	for _, code := range []string{"missing_storage_ids", "backing_aliases", "overlapping_sets"} {
		t.Run(code, func(t *testing.T) {
			m, _, state := managedDiskFixture(t, "spread", true)
			cfg := m.deps.Config
			switch code {
			case "missing_storage_ids":
				set := cfg.StorageSets[cfg.PersistentStorageSet]
				set.Names = []string{"DO_NOT_PRINT_MISSING_ID"}
				cfg.StorageSets[cfg.PersistentStorageSet] = set
			case "backing_aliases":
				m.deps.PVE = diagnosticAliasClient{m.deps.PVE}
			case "overlapping_sets":
				enabled := true
				cfg.RequireDisjointStorageSets = &enabled
				cfg.EphemeralStorageSet = cfg.PersistentStorageSet
			}
			before := diagnosticFiles(t, cfg.StorageAllocationJournalDir)
			args := []json.RawMessage{json.RawMessage(`64`), json.RawMessage(`{}`)}
			report, err := ObserveStoragePlanDiagnostics(t.Context(), m.deps, StoragePlanDiagnosticRequest{Method: "create_disk", Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Targets) != 0 || !strings.Contains(strings.Join(report.Rejections, ";"), "planning rejected: "+code) {
				t.Fatalf("missing typed diagnostic: %+v", report)
			}
			raw, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "DO_NOT_PRINT_MISSING_ID") {
				t.Fatal("raw policy error leaked into diagnostic")
			}
			_, callErr := HandleCreateDisk(m.deps).Handle(t.Context(), args, jsonrpc.Context{})
			if callErr == nil || !strings.Contains(callErr.Error(), "managed allocation failed") {
				t.Fatalf("expected real bounded CPI rejection, got %v", callErr)
			}
			if len(state.created) != 0 || !reflect.DeepEqual(before, diagnosticFiles(t, cfg.StorageAllocationJournalDir)) {
				t.Fatal("policy rejection changed allocation state")
			}
		})
	}
}

func TestStorageDiagnosticDoesNotClassifyTextThatImpersonatesPolicyError(t *testing.T) {
	if got := storageDiagnosticRejectionKind(errors.New("missing storage IDs: API-controlled text")); got != "configuration or observation" {
		t.Fatalf("untyped source text became authority: %s", got)
	}
}
