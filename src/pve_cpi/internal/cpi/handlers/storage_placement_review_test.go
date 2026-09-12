package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
)

type selectorReviewClient struct {
	pve.Client
	definitions clusterstorage.Service
}

func (c selectorReviewClient) ClusterStorage() clusterstorage.Service { return c.definitions }

type selectorReviewDefinitions struct{ clusterstorage.Service }

func (selectorReviewDefinitions) ListStorage(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	response := clusterstorage.ListStorageResponse{json.RawMessage(`{"storage":"tier-pool","type":"nfs","shared":1}`)}
	return &response, nil
}

// Compare against the actual legacy helpers: a new implementation and its own
// expectations could otherwise agree while changing pool-versus-tier precedence.
//
//nolint:gocognit // Keep the full legacy differential cross-product and its assertions together.
func TestStorageSelectorsReviewLegacyDifferential(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"root", "ephemeral", "persistent"} {
		for _, encrypted := range []bool{false, true} {
			if role == "root" && encrypted {
				continue
			}
			for call := range 7 {
				for disk := range 7 {
					for vm := range 7 {
						t.Run(fmt.Sprintf("%s/encrypted=%t/%d-%d-%d", role, encrypted, call, disk, vm), func(t *testing.T) {
							cfg := selectorConfig()
							cfg.Encrypted = &encrypted
							cp := selectorReviewLayer(role, call, "call")
							cp["disk_type"], cp["vm_type"] = "d", "v"
							cfg.DiskTypes["d"] = config.TypeProfile{CloudProperties: selectorReviewLayer(role, disk, "disk")}
							cfg.VMTypes["v"] = config.TypeProfile{CloudProperties: selectorReviewLayer(role, vm, "vm")}
							resolver, err := newLayeredResolver(cp, cfg)
							if err != nil {
								t.Fatal(err)
							}
							defs := selectorReviewDefinitions{}
							deps := Deps{Config: cfg, PVE: selectorReviewClient{definitions: defs}, Logger: log.NewNopLogger()}
							op := "create_vm"
							var want string
							var legacyErr error
							switch role {
							case "root":
								want, _, _, legacyErr = resolveVMShapeStorage(cfg, &createVMParsedArgs{cloudPropsMap: cp}, func(name string) (string, error) {
									return resolveStorageTier(t.Context(), defs, cfg, name, false)
								})
							case "ephemeral":
								want, legacyErr = resolveEphemeralStorage(t.Context(), deps, cfg, resolver, createVMCloudProps{}, encrypted)
							case "persistent":
								op = "create_disk"
								want, legacyErr = resolveStorageForDisk(t.Context(), resolver, deps, encrypted)
							}
							selection, err := ResolveStoragePlacementSelectors(cfg, op, cp, role == "ephemeral")
							if (err != nil) != (legacyErr != nil) {
								t.Fatalf("new error=%v; legacy error=%v", err, legacyErr)
							}
							if err != nil {
								return
							}
							got := selection.Root
							if role == "ephemeral" {
								got = selection.Ephemeral
							}
							if role == "persistent" {
								got = selection.Persistent
							}
							pool := got.Value
							if got.Kind == "tier" {
								pool, err = resolveStorageTier(t.Context(), defs, cfg, got.Value, encrypted)
								if err != nil {
									t.Fatal(err)
								}
							}
							if pool != want || got.Atomic || selection.SetManaged {
								t.Fatalf("new=%+v pool=%q managed=%t; legacy pool=%q", got, pool, selection.SetManaged, want)
							}
						})
					}
				}
			}
		}
	}
}

func selectorReviewLayer(role string, choice int, layer string) map[string]any {
	_, poolKeys, tierKey := storageRoleKeys(role)
	cp := map[string]any{}
	switch choice {
	case 1:
		cp[poolKeys[0]] = " " + layer + "-pool "
	case 2:
		cp[tierKey] = "tier"
	case 3:
		cp[tierKey] = "enc"
	case 4:
		cp[poolKeys[len(poolKeys)-1]], cp[tierKey] = layer+"-alias", "tier"
	case 5:
		cp[poolKeys[0]], cp[tierKey] = " ", " "
	case 6:
		cp[poolKeys[0]], cp[tierKey] = false, 7
	}
	return cp
}
