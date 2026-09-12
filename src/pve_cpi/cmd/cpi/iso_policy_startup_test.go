package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"go.opentelemetry.io/otel/trace"
)

type isoPolicyStartupStorage struct{ clusterstorage.Service }

func (isoPolicyStartupStorage) ListStorage(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	response := clusterstorage.ListStorageResponse{json.RawMessage(`{"storage":"local-lvm","type":"nfs","shared":1,"content":"images,iso"}`)}
	return &response, nil
}

type isoPolicyStartupClient struct{ nilPVEClient }

func (isoPolicyStartupClient) ClusterStorage() clusterstorage.Service {
	return isoPolicyStartupStorage{}
}

func TestISOStoragePolicyStartupRetainsOperatorSelection(t *testing.T) {
	cases := []struct{ name, extra, original, effective string }{
		{"default", "", "local", "local-lvm"},
		{"local", `,"iso_storage":"local"`, "local", "local-lvm"},
		{"empty", `,"iso_storage":""`, "local", "local-lvm"},
		{"fixed", `,"iso_storage":"fixed-iso"`, "fixed-iso", "fixed-iso"},
		{"following disabled", `,"iso_storage":"local","iso_storage_follow_vm_storage":false`, "local", "local"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			path := writeOTelWiringConfig(t, tt.extra)
			var loaded *config.CPIConfig
			factory := func(cfg *config.CPIConfig, _ *log.Logger, _ trace.Tracer) (pve.Client, error) {
				loaded = cfg
				return isoPolicyStartupClient{}, nil
			}
			var stdout, stderr bytes.Buffer
			code := runWithArgs([]string{"--config", path}, strings.NewReader(""), &stdout, &stderr, runOptions{ClientFactory: factory})
			if code != 0 {
				t.Fatalf("startup failed: %s", stderr.String())
			}
			if loaded == nil {
				t.Fatal("startup did not construct client")
			}
			if loaded.ISOStorage != tt.effective || loaded.OriginalISOStorage() != tt.original {
				t.Fatalf("startup effective/original = %q/%q, want %q/%q", loaded.ISOStorage, loaded.OriginalISOStorage(), tt.effective, tt.original)
			}
		})
	}
}
