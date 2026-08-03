package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// envConfigPath is the environment variable checked when --config is not
// given.
const envConfigPath = "PVE_CPI_CONFIG"

// defaultConfigPath is the pve_cpi release job's default config install
// location, used when neither --config nor $PVE_CPI_CONFIG is set.
const defaultConfigPath = "/var/vcap/jobs/pve_cpi/config/cpi.json"

// ClusterVM is the subset of a GET /cluster/resources "qemu" row pve-cid's
// online subcommands need.
type ClusterVM struct {
	VMID int
	Node string
	Name string
	Tags string
}

// StorageContentItem is the subset of a GET
// /nodes/{node}/storage/{storage}/content row pve-cid's online subcommands
// need.
type StorageContentItem struct {
	VolID string
	Size  int64
}

// Reader is the narrow slice of PVE read operations the locate and
// stemcells subcommands depend on. Production is satisfied by pveReader,
// which adapts a real pve.Client; tests substitute a fake implementing this
// interface directly — mirroring how internal/pve's own tests mock narrow
// service slices rather than the full SDK client. pve-cid never calls a
// mutating PVE endpoint, so Reader exposes only List/Get-shaped methods.
type Reader interface {
	// ListClusterVMs returns every QEMU guest (VM or template) visible via
	// GET /cluster/resources?type=vm, decoded into ClusterVM.
	ListClusterVMs(ctx context.Context) ([]ClusterVM, error)
	// VMConfig returns the raw GET /nodes/{node}/qemu/{vmid}/config response
	// for vmid on node.
	VMConfig(ctx context.Context, node string, vmid int) (map[string]any, error)
	// TemplatesBySHA8 returns every stemcell-cache template (cluster-wide)
	// tagged "bosh-stemcell-sha-<sha8>".
	TemplatesBySHA8(ctx context.Context, sha8 string) ([]pve.TemplateRef, error)
	// FindStemcellVolume returns the full volid of the qcow2 named filename
	// on storage (queried via node), or "" if not found.
	FindStemcellVolume(ctx context.Context, node, storage, filename string) (string, error)
	// ListStorageContent returns the "import" content items on storage,
	// queried via node.
	ListStorageContent(ctx context.Context, node, storage string) ([]StorageContentItem, error)
	// ListNodes returns every PVE node name in the cluster.
	ListNodes(ctx context.Context) ([]string, error)
	// StorageIsShared classifies storage as cluster-shared or node-local via
	// the same GET /storage entry and pve.StorageInfo.IsShared rule the CPI
	// itself uses. known is false when the classification could not be made
	// (storage absent from the index, or the request failed) — callers must
	// treat known=false as "assume local" (the safer, more-scanning choice
	// for an inventory tool) rather than "assume shared".
	StorageIsShared(ctx context.Context, storage string) (shared bool, known bool)
}

// pveReader adapts a real pve.Client to Reader.
type pveReader struct {
	client pve.Client
}

// newPVEReader constructs a production Reader backed by client.
func newPVEReader(client pve.Client) Reader {
	return &pveReader{client: client}
}

func strPtr(s string) *string { return &s }

func (r *pveReader) ListClusterVMs(ctx context.Context) ([]ClusterVM, error) {
	if r.client == nil || r.client.Cluster() == nil {
		return nil, fmt.Errorf("pve-cid: no cluster service available")
	}
	typ := "vm"
	resp, err := r.client.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &typ})
	if err != nil {
		return nil, fmt.Errorf("pve-cid: list cluster resources: %w", err)
	}
	if resp == nil {
		return nil, nil
	}

	var out []ClusterVM
	for _, raw := range *resp {
		var entry struct {
			Type string `json:"type"`
			VMID int64  `json:"vmid"`
			Node string `json:"node"`
			Name string `json:"name"`
			Tags string `json:"tags"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			// Malformed element — skip; do not fail the whole scan (matches
			// internal/pve's own tolerance of individual malformed rows).
			continue
		}
		if entry.Type != "qemu" || entry.VMID <= 0 {
			// Excludes lxc containers and any other non-VM resource row.
			continue
		}
		out = append(out, ClusterVM{VMID: int(entry.VMID), Node: entry.Node, Name: entry.Name, Tags: entry.Tags})
	}
	return out, nil
}

func (r *pveReader) VMConfig(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if r.client == nil || r.client.QEMU() == nil {
		return nil, fmt.Errorf("pve-cid: no QEMU service available")
	}
	return r.client.QEMU().Config(ctx, node, vmid)
}

func (r *pveReader) TemplatesBySHA8(ctx context.Context, sha8 string) ([]pve.TemplateRef, error) {
	return pve.FindTemplatesBySHATagCluster(ctx, r.client, sha8)
}

func (r *pveReader) FindStemcellVolume(ctx context.Context, node, storage, filename string) (string, error) {
	return pve.FindStemcellByFilename(ctx, r.client, node, storage, filename)
}

func (r *pveReader) ListStorageContent(ctx context.Context, node, storage string) ([]StorageContentItem, error) {
	if r.client == nil || r.client.Nodes() == nil {
		return nil, fmt.Errorf("pve-cid: no nodes service available")
	}
	resp, err := r.client.Nodes().ListStorageContent(ctx, node, storage, &sdknodes.ListStorageContentParams{
		Content: strPtr("import"),
	})
	if err != nil {
		return nil, fmt.Errorf("pve-cid: list storage content on %q/%q: %w", node, storage, err)
	}
	if resp == nil {
		return nil, nil
	}

	var out []StorageContentItem
	for _, raw := range *resp {
		var item struct {
			VolID string `json:"volid"`
			Size  int64  `json:"size"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		if item.VolID == "" {
			continue
		}
		out = append(out, StorageContentItem{VolID: item.VolID, Size: item.Size})
	}
	return out, nil
}

func (r *pveReader) ListNodes(ctx context.Context) ([]string, error) {
	if r.client == nil || r.client.Nodes() == nil {
		return nil, fmt.Errorf("pve-cid: no nodes service available")
	}
	resp, err := r.client.Nodes().ListNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("pve-cid: list nodes: %w", err)
	}
	if resp == nil {
		return nil, nil
	}

	var out []string
	for _, raw := range *resp {
		var item struct {
			Node string `json:"node"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		if item.Node != "" {
			out = append(out, item.Node)
		}
	}
	return out, nil
}

func (r *pveReader) StorageIsShared(ctx context.Context, storage string) (shared bool, known bool) {
	if r.client == nil || r.client.ClusterStorage() == nil || storage == "" {
		return false, false
	}
	resp, err := r.client.ClusterStorage().ListStorage(ctx, &sdkclusterstorage.ListStorageParams{})
	if err != nil || resp == nil {
		return false, false
	}
	for _, raw := range *resp {
		info, perr := pve.ParseStorageEntry(raw)
		if perr != nil {
			continue
		}
		if info.Name == storage {
			return info.IsShared(), true
		}
	}
	return false, false
}

// loadConfigAndReader resolves the CPI config path (configPath flag value,
// falling back to $PVE_CPI_CONFIG, then defaultConfigPath), loads it via the
// same config.LoadFile the cpi binary itself uses, and constructs a live
// Reader against the configured PVE cluster reusing pve.NewClientWithTracer
// — the identical client-construction path cmd/cpi/main.go uses, so
// pve-cid's connection/auth/retry behavior matches the CPI exactly.
//
// stderr receives any client-init log output (transport warnings, etc.).
func loadConfigAndReader(configPath string, stderr io.Writer) (*config.CPIConfig, Reader, error) {
	if configPath == "" {
		configPath = os.Getenv(envConfigPath)
	}
	if configPath == "" {
		configPath = defaultConfigPath
	}

	cfg, err := config.LoadFile(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("pve-cid: config load failed (%s): %w", configPath, err)
	}

	logger, err := log.NewLogger(cfg.LogLevel, stderr)
	if err != nil {
		return nil, nil, fmt.Errorf("pve-cid: logger init failed: %w", err)
	}

	// Nil tracer: pve.NewClientWithTracer skips the tracing decorator layer
	// entirely on nil (mirrors cmd/cpi/main.go's tracing-disabled path) — a
	// read-only inspection tool has no reason to stand up an OTel pipeline.
	client, err := pve.NewClientWithTracer(cfg, logger, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("pve-cid: pve client init failed: %w", err)
	}

	return cfg, newPVEReader(client), nil
}
