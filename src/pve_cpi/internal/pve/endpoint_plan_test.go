package pve

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// fixedSource is a discoveredEndpointSource that returns a fixed list.
type fixedSource []endpointpool.Endpoint

func (s fixedSource) DiscoveredEndpoints() []endpointpool.Endpoint { return s }

func planAddresses(p endpointPlan) []string {
	out := make([]string, 0, len(p.endpoints))
	for _, ep := range p.endpoints {
		out = append(out, ep.Address)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPlanEndpoints(t *testing.T) {
	t.Parallel()
	pin := endpointpool.PinOf([]byte("pve9 leaf certificate"))
	discovered := endpointpool.Endpoint{
		Address: "pve9.example.com:8006", Kind: endpointpool.KindDiscovered, Pins: []endpointpool.Pin{pin},
	}
	cases := []struct {
		name           string
		host           string
		port           int
		endpoints      []string
		source         discoveredEndpointSource
		want           []string
		wantConfigured int
		wantInstall    bool
		wantFailover   bool
		wantErr        string
	}{
		{
			name: "no list reads the host and port", host: "pve1.example.com", port: 8006,
			want: []string{"pve1.example.com:8006"}, wantConfigured: 1,
		},
		{
			name: "no list and no port reads the default port", host: "pve1.example.com", port: 0,
			want: []string{"pve1.example.com:8006"}, wantConfigured: 1,
		},
		{
			name: "a list of one installs nothing", host: "pve1.example.com", port: 8006,
			endpoints: []string{"pve1.example.com:8006"},
			want:      []string{"pve1.example.com:8006"}, wantConfigured: 1,
		},
		{
			name: "a list of three installs the wrapper", host: "pve1.example.com", port: 8006,
			endpoints:      []string{"pve1.example.com:8006", "pve2.example.com:8006", "pve3.example.com:9000"},
			want:           []string{"pve1.example.com:8006", "pve2.example.com:8006", "pve3.example.com:9000"},
			wantConfigured: 3, wantInstall: true, wantFailover: true,
		},
		{
			name: "entries without a port take pve.port", host: "pve1.example.com", port: 8443,
			endpoints:      []string{"pve1.example.com", "pve2.example.com"},
			want:           []string{"pve1.example.com:8443", "pve2.example.com:8443"},
			wantConfigured: 2, wantInstall: true, wantFailover: true,
		},
		{
			name: "hosts are canonical", host: "PVE1.example.com.", port: 8006,
			endpoints:      []string{"pve1.example.com:8006", "PVE2.Example.com:8006"},
			want:           []string{"pve1.example.com:8006", "pve2.example.com:8006"},
			wantConfigured: 2, wantInstall: true, wantFailover: true,
		},
		{
			name: "IPv6 literals keep their brackets", host: "fd00::1", port: 8006,
			endpoints:      []string{"[fd00::1]:8006", "fd00::2"},
			want:           []string{"[fd00::1]:8006", "[fd00::2]:8006"},
			wantConfigured: 2, wantInstall: true, wantFailover: true,
		},
		{
			name: "a source adds discovered endpoints after the configured ones", host: "pve1.example.com", port: 8006,
			endpoints: []string{"pve1.example.com:8006"}, source: fixedSource{discovered},
			want:           []string{"pve1.example.com:8006", "pve9.example.com:8006"},
			wantConfigured: 1, wantInstall: true,
		},
		{
			name: "a nil source adds nothing", host: "pve1.example.com", port: 8006,
			endpoints: []string{"pve1.example.com:8006"}, source: nil,
			want: []string{"pve1.example.com:8006"}, wantConfigured: 1,
		},
		{
			name: "a first entry that is not pve.host", host: "pve1.example.com", port: 8006,
			endpoints: []string{"pve2.example.com:8006", "pve1.example.com:8006"},
			wantErr:   "the first pve.endpoints entry (pve2.example.com:8006) is not pve.host with pve.port",
		},
		{
			name: "a first entry on another port", host: "pve1.example.com", port: 8006,
			endpoints: []string{"pve1.example.com:9000", "pve2.example.com:8006"},
			wantErr:   "the first pve.endpoints entry (pve1.example.com:9000) is not pve.host with pve.port",
		},
		{
			name: "an invalid entry", host: "pve1.example.com", port: 8006,
			endpoints: []string{"pve1.example.com:8006", "user@pve2.example.com"},
			wantErr:   "pve.endpoints[1] is not a host or host:port",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.CPIConfig{Host: tc.host, Port: tc.port, Endpoints: tc.endpoints}
			opts := buildTransportOpts(cfg)
			plan, err := planEndpoints(cfg, opts, tc.source)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("planEndpoints error = %v, want it to contain %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), "user@") {
					t.Errorf("the error echoes the entry: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("planEndpoints: %v", err)
			}
			if got := planAddresses(plan); !equalStrings(got, tc.want) {
				t.Errorf("endpoints = %v, want %v", got, tc.want)
			}
			if plan.configured != tc.wantConfigured {
				t.Errorf("configured = %d, want %d", plan.configured, tc.wantConfigured)
			}
			if got := plan.install(); got != tc.wantInstall {
				t.Errorf("install() = %v, want %v", got, tc.wantInstall)
			}
			if got := plan.failover(); got != tc.wantFailover {
				t.Errorf("failover() = %v, want %v", got, tc.wantFailover)
			}
			for i, ep := range plan.endpoints[:plan.configured] {
				if ep.Kind != endpointpool.KindConfigured {
					t.Errorf("endpoint %d kind = %v, want configured", i, ep.Kind)
				}
			}
		})
	}
}

// The empty source is the one newClient uses, and it must add nothing and
// leave discovery off for every config.
func TestNoDiscoveredEndpointsAddsNothing(t *testing.T) {
	t.Parallel()
	if got := (noDiscoveredEndpoints{}).DiscoveredEndpoints(); len(got) != 0 {
		t.Errorf("DiscoveredEndpoints = %v, want none", got)
	}
	var _ discoveredEndpointSource = noDiscoveredEndpoints{}
	on := true
	cfg := &config.CPIConfig{Host: "pve1.example.com", NodeEndpointsDiscovery: &on}
	if endpointDiscoveryOn(cfg) || endpointDiscoveryOn(nil) {
		t.Error("the effective endpoint discovery mode must be off")
	}
}

// A plan with discovery on installs the wrapper for a pool of one, and sees
// failover as possible, so a config that builds today cannot fail to build
// once discovery fills the cache.
func TestEndpointPlanDiscoveryOn(t *testing.T) {
	t.Parallel()
	plan := endpointPlan{
		endpoints:  []endpointpool.Endpoint{{Address: "pve1.example.com:8006", Kind: endpointpool.KindConfigured}},
		configured: 1,
		discovery:  true,
	}
	if !plan.install() {
		t.Error("install() = false with discovery on, want true")
	}
	if !plan.failover() {
		t.Error("failover() = false with discovery on, want true")
	}
}
