package handlers_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
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
