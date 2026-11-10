package pve

import (
	"strings"
	"testing"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
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
			name: "resolved entries keep their own ports", host: "pve1.example.com", port: 8443,
			endpoints:      []string{"pve1.example.com:8443", "pve2.example.com:8006"},
			want:           []string{"pve1.example.com:8443", "pve2.example.com:8006"},
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
			endpoints:      []string{"[fd00::1]:8006", "[fd00::2]:8006"},
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
			name: "an entry without a port is refused, not filled", host: "pve1.example.com", port: 8443,
			endpoints: []string{"pve1.example.com:8443", "pve2.example.com"},
			wantErr:   "the endpoint list was not resolved: pve.endpoints[1] has no port",
		},
		{
			name: "a first entry without a port is refused even when it names the host", host: "pve1.example.com", port: 8443,
			endpoints: []string{"pve1.example.com", "pve2.example.com:8443"},
			wantErr:   "the endpoint list was not resolved: pve.endpoints[0] has no port",
		},
		{
			name: "a bare IPv6 entry is refused", host: "fd00::1", port: 8006,
			endpoints: []string{"[fd00::1]:8006", "fd00::2"},
			wantErr:   "the endpoint list was not resolved: pve.endpoints[1] has no port",
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
			cfg := &config.CPIConfig{
				Host: tc.host, Port: tc.port, Endpoints: tc.endpoints, APIEndpointDiscovery: config.DiscoveryModeOff,
			}
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

// The empty source is the one the plan starts from, and it must add nothing.
func TestNoDiscoveredEndpointsAddsNothing(t *testing.T) {
	t.Parallel()
	if got := (noDiscoveredEndpoints{}).DiscoveredEndpoints(); len(got) != 0 {
		t.Errorf("DiscoveredEndpoints = %v, want none", got)
	}
	var _ discoveredEndpointSource = noDiscoveredEndpoints{}
}

// The config alone decides whether discovery is on for the plan, by the mode
// table's rows that need no filesystem. A nil config and an unvalidated mode
// stay off, because discovery must never start from a config nobody validated.
func TestEndpointDiscoveryOnFollowsTheModeTable(t *testing.T) {
	t.Parallel()
	off := false
	cases := []struct {
		name string
		cfg  *config.CPIConfig
		want bool
	}{
		{"nil config", nil, false},
		{"one endpoint, a token, auto", &config.CPIConfig{Host: "a", APIToken: "u@pve!t=s"}, true},
		{"one endpoint, a password, auto", &config.CPIConfig{Host: "a", Password: "p"}, false},
		{
			"two endpoints, a token, auto",
			&config.CPIConfig{Host: "a", APIToken: "u@pve!t=s", Endpoints: []string{"a:8006", "b:8006"}}, false,
		},
		{
			"two endpoints, a token, on",
			&config.CPIConfig{
				Host: "a", APIToken: "u@pve!t=s", Endpoints: []string{"a:8006", "b:8006"},
				APIEndpointDiscovery: config.DiscoveryModeOn,
			}, true,
		},
		{
			"a password, on, with verification",
			&config.CPIConfig{Host: "a", Password: "p", APIEndpointDiscovery: config.DiscoveryModeOn}, true,
		},
		{
			"a password, on, without verification",
			&config.CPIConfig{
				Host: "a", Password: "p", VerifySSL: &off, APIEndpointDiscovery: config.DiscoveryModeOn,
			}, false,
		},
		{
			"a token, off",
			&config.CPIConfig{Host: "a", APIToken: "u@pve!t=s", APIEndpointDiscovery: config.DiscoveryModeOff}, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := endpointDiscoveryOn(tc.cfg); got != tc.want {
				t.Errorf("endpointDiscoveryOn = %v, want %v", got, tc.want)
			}
		})
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

// A cache directory found unusable turns the effective mode off, so the
// wrapper follows the pool alone, while the pinning input stays what the
// config said.
func TestEndpointPlanUnusableCacheDirectory(t *testing.T) {
	t.Parallel()
	plan := endpointPlan{
		endpoints:     []endpointpool.Endpoint{{Address: "pve1.example.com:8006", Kind: endpointpool.KindConfigured}},
		configured:    1,
		discovery:     true,
		cacheUnusable: true,
	}
	if plan.install() {
		t.Error("install() = true with discovery on and an unusable directory, want false")
	}
	if !plan.failover() {
		t.Error("failover() = false, want true: the directory must not change the pinning input")
	}
	plan.endpoints = append(plan.endpoints, endpointpool.Endpoint{Address: "pve2.example.com:8006"})
	if !plan.install() {
		t.Error("install() = false with a pool of two, want true")
	}
}

// wiringConfig is a config for wireEndpoints whose list holds endpoints, all
// on the default port, with discovery off so the plan decides on the list
// alone.
func wiringConfig(dial, handshake int, endpoints ...string) *config.CPIConfig {
	cfg := &config.CPIConfig{
		Host:                      "pve1.example.com",
		Port:                      8006,
		Endpoints:                 endpoints,
		PVEDialTimeoutSec:         dial,
		PVETLSHandshakeTimeoutSec: handshake,
		APIEndpointDiscovery:      config.DiscoveryModeOff,
	}
	return cfg
}

// wireEndpoints and apply are the seam through which newClient decides the
// timeouts and the transport wrapper. A client with one endpoint keeps the options
// buildTransportOpts gave it and no wrapper, and a pool of more than one gets
// the pool timeout defaults, for each value the operator left at 0, and the
// wrapper.
func TestWireEndpoints(t *testing.T) {
	t.Parallel()
	one := []string{"pve1.example.com:8006"}
	two := []string{"pve1.example.com:8006", "pve2.example.com:8006"}
	three := []string{"pve1.example.com:8006", "pve2.example.com:8006", "pve3.example.com:8006"}
	cases := []struct {
		name          string
		dial          int
		handshake     int
		endpoints     []string
		wantDial      int
		wantHandshake int
		wantWrapper   bool
	}{
		{"host only", 0, 0, nil, 0, 0, false},
		{"host only keeps the operator timeouts", 7, 9, nil, 7, 9, false},
		{"a list of one", 0, 0, one, 0, 0, false},
		{"a list of one keeps the operator timeouts", 7, 9, one, 7, 9, false},
		{"a list of two defaults both timeouts", 0, 0, two, poolDialTimeoutSec, poolTLSHandshakeTimeoutSec, true},
		{"a list of three defaults both timeouts", 0, 0, three, poolDialTimeoutSec, poolTLSHandshakeTimeoutSec, true},
		{"an operator dial timeout wins", 30, 0, two, 30, poolTLSHandshakeTimeoutSec, true},
		{"an operator handshake timeout wins", 0, 20, two, poolDialTimeoutSec, 20, true},
		{"both operator timeouts win", 30, 20, two, 30, 20, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts, wiring, err := wireEndpoints(wiringConfig(tc.dial, tc.handshake, tc.endpoints...))
			if err != nil {
				t.Fatalf("wireEndpoints: %v", err)
			}
			opts = wiring.apply(opts, log.NewNopLogger(), nil)
			if opts.DialTimeoutSec != tc.wantDial {
				t.Errorf("DialTimeoutSec = %d, want %d", opts.DialTimeoutSec, tc.wantDial)
			}
			if opts.TLSHandshakeTimeoutSec != tc.wantHandshake {
				t.Errorf("TLSHandshakeTimeoutSec = %d, want %d", opts.TLSHandshakeTimeoutSec, tc.wantHandshake)
			}
			if got := opts.WrapTransport != nil; got != tc.wantWrapper {
				t.Errorf("WrapTransport installed = %v, want %v", got, tc.wantWrapper)
			}
			if wiring == nil || wiring.plan.install() != tc.wantWrapper {
				t.Errorf("plan = %+v, want install() = %v", wiring, tc.wantWrapper)
			}
		})
	}
}

// A plan error comes back before any option is built.
func TestWireEndpointsReturnsThePlanError(t *testing.T) {
	t.Parallel()
	_, wiring, err := wireEndpoints(wiringConfig(0, 0, "pve2.example.com:8006", "pve1.example.com:8006"))
	if err == nil || !strings.Contains(err.Error(), "is not pve.host with pve.port") {
		t.Fatalf("wireEndpoints error = %v, want the first-endpoint refusal", err)
	}
	if wiring != nil {
		t.Errorf("wiring = %+v, want nil with an error", wiring)
	}
}

// assertPinningForPlan decides which of the two routing causes apply from the
// config and the plan, and asks the assertion about both.
func TestAssertPinningForPlan(t *testing.T) {
	t.Parallel()
	off := false
	pinned := sdkclient.Options{ManualVerification: true}
	multi := endpointPlan{
		endpoints:  []endpointpool.Endpoint{{Address: "a:8006"}, {Address: "b:8006"}},
		configured: 2,
	}
	single := endpointPlan{endpoints: []endpointpool.Endpoint{{Address: "a:8006"}}, configured: 1}
	plain := &config.CPIConfig{Host: "a", NodeEndpointsDiscovery: &off}
	routed := &config.CPIConfig{
		Host: "a", NodeEndpointsDiscovery: &off, NodeEndpoints: map[string]string{"pve2": "b:8006"},
	}
	cases := []struct {
		name         string
		opts         sdkclient.Options
		cfg          *config.CPIConfig
		plan         endpointPlan
		wantErr      bool
		wantFailover bool
		wantNode     bool
	}{
		{"a pool and no pinning", sdkclient.Options{}, plain, multi, false, false, false},
		{"one endpoint and pinning", pinned, plain, single, false, false, false},
		{"a pool and pinning", pinned, plain, multi, true, true, false},
		{"one endpoint, node routing, and pinning", pinned, routed, single, true, false, true},
		{"a pool, node routing, and pinning", pinned, routed, multi, true, true, true},
		{
			"discovery in the plan counts as failover", pinned, plain,
			endpointPlan{endpoints: single.endpoints, configured: 1, discovery: true}, true, true, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := assertPinningForPlan(tc.opts, tc.cfg, tc.plan)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("assertPinningForPlan = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("assertPinningForPlan = nil, want a refusal")
			}
			if got := strings.Contains(err.Error(), "endpoint failover"); got != tc.wantFailover {
				t.Errorf("error names failover = %v, want %v: %v", got, tc.wantFailover, err)
			}
			if got := strings.Contains(err.Error(), "direct-to-node upload routing"); got != tc.wantNode {
				t.Errorf("error names node routing = %v, want %v: %v", got, tc.wantNode, err)
			}
		})
	}
}
