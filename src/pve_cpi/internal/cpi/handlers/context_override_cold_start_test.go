package handlers_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// coldStartRecorder is a client factory that records the node names the
// cold start would get for each entry, which it asks of config the way the
// production client does, with the effective config and the job the runtime
// was built with.
type coldStartRecorder struct {
	mu    sync.Mutex
	names map[string][]string
}

func (r *coldStartRecorder) Factory(cfg, job *config.CPIConfig, _ *log.Logger) (pve.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names == nil {
		r.names = make(map[string][]string)
	}
	r.names[cfg.Host] = config.ColdStartNodeNames(cfg, job)
	return &mockPVEClient{}, nil
}

// coldStartJobConfig returns a job that meets every cold-start condition with
// a two-endpoint list and a two-AZ map.
func coldStartJobConfig(t *testing.T) *config.CPIConfig {
	t.Helper()
	discovery := config.DiscoveryModeOn
	job := overrideTestBaseConfig()
	job.Password = ""
	job.APIToken = "job@pve!cpi=JOB-TOKEN-PLACEHOLDER"
	job.APIEndpointDiscovery = discovery
	job.PVECACertPEM = handlers.SelfSignedCAPEMForTest(t)
	job.Endpoints = []string{"job-cluster.example", "job-cluster-b.example"}
	job.Placement = &config.PlacementConfig{AZMap: map[string][]string{
		"az1": {"pve1", "pve2"},
		"az2": {"pve3"},
	}}
	job.ApplyDefaults()
	if err := job.Validate(); err != nil {
		t.Fatalf("the cold-start job does not validate: %v", err)
	}
	return job
}

// TestWithRequestOverrides_ColdStartNamesFollowTheJobsEndpointList asserts
// that an entry which names no pve_placement gets the job's az_map names only
// when its endpoint list is the job's own, and none when it points at another
// host, because an az_map names the nodes of one cluster.
func TestWithRequestOverrides_ColdStartNamesFollowTheJobsEndpointList(t *testing.T) {
	t.Parallel()
	job := coldStartJobConfig(t)
	recorder := &coldStartRecorder{}
	deps := handlers.Deps{
		Config: job,
		PVE:    &mockPVEClient{},
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			Job:           job,
			ClientFactory: recorder.Factory,
			Logger:        log.NewNopLogger(),
		},
	}
	token := "entry@pve!cpi=ENTRY-TOKEN-PLACEHOLDER"
	cases := []struct {
		name  string
		host  string
		extra map[string]any
		want  []string
	}{
		{
			name:  "an unnamed entry on the job's own endpoint list",
			host:  "job-cluster.example",
			extra: map[string]any{"pve_api_token": token},
			want:  []string{"pve1", "pve2", "pve3"},
		},
		{
			name:  "an unnamed entry on another host",
			host:  "other-cluster.example",
			extra: map[string]any{"pve_api_token": token, "pve_host": "other-cluster.example"},
			want:  nil,
		},
		{
			name: "an unnamed entry on the job's host with a different endpoint list",
			host: "job-cluster.example",
			extra: map[string]any{
				"pve_api_token": token,
				"pve_endpoints": []any{"job-cluster.example", "elsewhere.example"},
			},
			want: nil,
		},
		{
			name: "an entry on another host that names its own placement",
			host: "named-cluster.example",
			extra: map[string]any{
				"pve_api_token": token,
				"pve_host":      "named-cluster.example",
				// Another host resets the discovery mode and the trust anchor, so
				// the entry sets both.
				"pve_api_endpoint_discovery": "on",
				"pve_ca_cert":                handlers.SelfSignedCAPEMForTest(t),
				"pve_placement":              map[string]any{"az_map": map[string]any{"azx": []any{"nodex"}}},
			},
			want: []string{"nodex"},
		},
	}
	for _, tc := range cases {
		if _, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{Extra: tc.extra}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		recorder.mu.Lock()
		got, built := recorder.names[tc.host]
		recorder.mu.Unlock()
		if !built {
			t.Fatalf("%s: the factory never built a client for %s", tc.name, tc.host)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: cold-start names = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestWithRequestOverrides_RefusesARuntimeWithoutAJob asserts that a runtime
// built with no job fails the request instead of passing nil on, because the
// cold start would otherwise treat every entry as the job.
func TestWithRequestOverrides_RefusesARuntimeWithoutAJob(t *testing.T) {
	t.Parallel()
	base := overrideTestBaseConfig()
	recorder := &coldStartRecorder{}
	deps := handlers.Deps{
		Config: base,
		PVE:    &mockPVEClient{},
		Logger: log.NewNopLogger(),
		Overrides: &handlers.RequestOverrideRuntime{
			ClientFactory: recorder.Factory,
			Logger:        log.NewNopLogger(),
		},
	}
	_, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{
		Extra: map[string]any{"pve_api_token": overrideTestEntryToken, "pve_host": "az1.example"},
	})
	if err == nil {
		t.Fatal("a runtime with no Job built a client")
	}
	if len(recorder.names) != 0 {
		t.Errorf("the factory ran %d time(s) for a runtime with no Job", len(recorder.names))
	}
}
