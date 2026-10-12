package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
)

type isoPolicyTargetCapture struct {
	storage.Service
	mu      sync.Mutex
	targets map[string]string
}

func (s *isoPolicyTargetCapture) DeleteVolumeAsync(_ context.Context, node, pool, volume string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targets[node] = pool + "/" + volume
	return "", nil
}

func TestISOStoragePolicyOverrideAgentsAndOriginalChoicesAreIsolated(t *testing.T) {
	base := overrideTestBaseConfig()
	base.AgentMode = config.AgentModeCloudInit
	base.ISOStorage = "local"
	base.ApplyDefaults()
	base.ISOStorage = "legacy-resolved"
	capture := &isoPolicyTargetCapture{targets: map[string]string{}}
	runtime := &handlers.RequestOverrideRuntime{Logger: log.NewNopLogger(), BaseHost: base.Host, ClientFactory: func(*config.CPIConfig, *log.Logger) (pve.Client, error) {
		return &mockPVEClient{storageSvc: capture}, nil
	}}
	deps := handlers.Deps{Config: base, Logger: log.NewNopLogger(), Overrides: runtime}
	var wg sync.WaitGroup
	for _, choice := range []string{"fixed-a", "fixed-b"} {
		wg.Go(func() {
			effective, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{Extra: map[string]any{"pve_iso_storage": choice}})
			if err != nil {
				t.Error(err)
				return
			}
			if effective.Config.OriginalISOStorage() != choice || effective.Config.ISOStorage != choice {
				t.Errorf("lost explicit ISO choice %q", choice)
				return
			}
			// Exercise the actual boot-agent target through a fake storage service. This
			// proves NewAgent was bound to the request's pool without inspecting fields.
			if err = effective.Agent.Remove(context.Background(), choice, 101); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	capture.mu.Lock()
	for _, choice := range []string{"fixed-a", "fixed-b"} {
		if !strings.HasPrefix(capture.targets[choice], choice+"/"+choice+":iso/") {
			t.Errorf("agent target %q = %q", choice, capture.targets[choice])
		}
	}
	capture.mu.Unlock()
	inherited, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{Extra: map[string]any{"pve_host": "alias.example"}})
	if err != nil {
		t.Fatal(err)
	}
	if inherited.Config.OriginalISOStorage() != "local" || inherited.Config.ISOStorage != "legacy-resolved" {
		t.Fatal("inherited alias lost original or changed existing effective pool")
	}
	if base.OriginalISOStorage() != "local" || base.ISOStorage != "legacy-resolved" {
		t.Fatal("request construction mutated shared base")
	}
}

func TestISOStoragePolicyOverrideEmptyReplacesPriorPin(t *testing.T) {
	base := overrideTestBaseConfig()
	base.ISOStorage = "pinned"
	base.ApplyDefaults()
	deps := handlers.Deps{Config: base, Logger: log.NewNopLogger(), Overrides: &handlers.RequestOverrideRuntime{Logger: log.NewNopLogger(), ClientFactory: func(*config.CPIConfig, *log.Logger) (pve.Client, error) { return &mockPVEClient{}, nil }}}
	effective, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{Extra: map[string]any{"pve_iso_storage": ""}})
	if err != nil {
		t.Fatal(err)
	}
	if effective.Config.ISOStorage != "" || effective.Config.OriginalISOStorage() != "" {
		t.Fatal("explicit empty override inherited old pin")
	}
	if base.OriginalISOStorage() != "pinned" {
		t.Fatal("empty override changed base")
	}
}

type isoLookupStorage struct {
	clusterstorage.Service
	calls atomic.Int32
}

func (s *isoLookupStorage) ListStorage(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	s.calls.Add(1)
	response := clusterstorage.ListStorageResponse{json.RawMessage(`{"storage":"local-lvm","type":"nfs","shared":1,"content":"images,iso"}`)}
	return &response, nil
}

// Building an override bundle used to run the follow-vm-storage lookup against
// the overridden cluster before the request could start. It now waits for the
// first handler that asks for the pool, runs once, and shapes the answer a
// handler sees through Deps.EffectiveISOStorage.
func TestISOStorageOverrideResolvesOnFirstUseOnly(t *testing.T) {
	cases := []struct {
		name, iso string
		wantCalls int32
		want      string
	}{
		{"default follows vm storage", "local", 1, "local-lvm"},
		{"explicit pin is kept", "fixed-iso", 0, "fixed-iso"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := overrideTestBaseConfig()
			base.AgentMode = config.AgentModeCloudInit
			base.VMStorage = "local-lvm"
			base.ISOStorage = tc.iso
			base.ApplyDefaults()
			lookup := &isoLookupStorage{}
			runtime := &handlers.RequestOverrideRuntime{Logger: log.NewNopLogger(), BaseHost: base.Host, ClientFactory: func(*config.CPIConfig, *log.Logger) (pve.Client, error) {
				return &mockPVEClient{clusterStorageSvc: lookup}, nil
			}}
			deps := handlers.Deps{Config: base, Logger: log.NewNopLogger(), Overrides: runtime}

			effective, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{Extra: map[string]any{"pve_host": "cluster-b.example"}})
			if err != nil {
				t.Fatal(err)
			}
			if got := lookup.calls.Load(); got != 0 {
				t.Fatalf("building the bundle issued %d ListStorage calls, want 0", got)
			}
			for range 2 {
				if got := effective.EffectiveISOStorage(context.Background()); got != tc.want {
					t.Fatalf("EffectiveISOStorage() = %q, want %q", got, tc.want)
				}
			}
			if got := lookup.calls.Load(); got != tc.wantCalls {
				t.Errorf("ListStorage calls = %d, want %d", got, tc.wantCalls)
			}
			if base.ISOStorage != tc.iso {
				t.Errorf("shared base config mutated: ISOStorage = %q, want %q", base.ISOStorage, tc.iso)
			}
		})
	}
}

// A Deps with no resolver (every handler test literal) reads the configured
// pool unchanged.
func TestEffectiveISOStorageWithoutResolverUsesConfig(t *testing.T) {
	deps := handlers.Deps{Config: &config.CPIConfig{ISOStorage: "bosh-isos"}}
	if got := deps.EffectiveISOStorage(context.Background()); got != "bosh-isos" {
		t.Errorf("EffectiveISOStorage() = %q, want bosh-isos", got)
	}
	if got := (handlers.Deps{}).EffectiveISOStorage(context.Background()); got != "" {
		t.Errorf("nil config: EffectiveISOStorage() = %q, want empty", got)
	}
}

type isoClusterStorage struct {
	clusterstorage.Service
	shared int
	stype  string
	calls  atomic.Int32
}

func (s *isoClusterStorage) ListStorage(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	s.calls.Add(1)
	response := clusterstorage.ListStorageResponse{
		json.RawMessage(fmt.Sprintf(`{"storage":"local-lvm","type":%q,"shared":%d,"content":"images,iso"}`, s.stype, s.shared)),
	}
	return &response, nil
}

// Each cpi_config entry owns its resolver and memo: one cluster whose vm_storage
// is shared follows it, another whose vm_storage is node-local keeps the
// fallback, each asks its own cluster exactly once, and neither touches the
// job-level resolver.
func TestISOStorageMemoIsPerCluster(t *testing.T) {
	base := overrideTestBaseConfig()
	base.AgentMode = config.AgentModeCloudInit
	base.VMStorage = "local-lvm"
	base.ISOStorage = "local"
	base.ApplyDefaults()

	storages := map[string]*isoClusterStorage{
		"cluster-a.example": {shared: 1, stype: "nfs"},
		"cluster-b.example": {shared: 0, stype: "dir"},
	}
	runtime := &handlers.RequestOverrideRuntime{Logger: log.NewNopLogger(), BaseHost: base.Host, ClientFactory: func(cfg *config.CPIConfig, _ *log.Logger) (pve.Client, error) {
		return &mockPVEClient{clusterStorageSvc: storages[cfg.Host]}, nil
	}}
	jobStorage := &isoClusterStorage{shared: 1, stype: "nfs"}
	jobISO := agent.NewISOStorageResolver(base, &mockPVEClient{clusterStorageSvc: jobStorage}, log.NewNopLogger())
	deps := handlers.Deps{Config: base, Logger: log.NewNopLogger(), Overrides: runtime, ISO: jobISO}

	resolve := func(host string) handlers.Deps {
		t.Helper()
		effective, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{Extra: map[string]any{"pve_host": host}})
		if err != nil {
			t.Fatal(err)
		}
		return effective
	}
	a, b := resolve("cluster-a.example"), resolve("cluster-b.example")
	if a.ISO == b.ISO || a.ISO == jobISO || b.ISO == jobISO {
		t.Fatal("entries must hold distinct resolvers from each other and from the job level")
	}
	for range 2 {
		if got := a.EffectiveISOStorage(context.Background()); got != "local-lvm" {
			t.Errorf("cluster A: EffectiveISOStorage() = %q, want local-lvm", got)
		}
		if got := b.EffectiveISOStorage(context.Background()); got != "local" {
			t.Errorf("cluster B: EffectiveISOStorage() = %q, want local", got)
		}
	}
	if got := storages["cluster-a.example"].calls.Load(); got != 1 {
		t.Errorf("cluster A ListStorage calls = %d, want 1", got)
	}
	if got := storages["cluster-b.example"].calls.Load(); got != 1 {
		t.Errorf("cluster B ListStorage calls = %d, want 1", got)
	}
	if got := jobStorage.calls.Load(); got != 0 {
		t.Errorf("job-level ListStorage calls = %d, want 0", got)
	}
}
