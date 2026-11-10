// Package pve white-box tests for buildTransportOpts (§7.30).
// Uses package pve (not pve_test) to access the unexported helper directly.
package pve

import (
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

func nopLogger() *log.Logger { return log.NewNopLogger() }

// minCfg returns a CPIConfig with only the fields buildTransportOpts reads.
func minCfg() *config.CPIConfig {
	return &config.CPIConfig{
		Host: "pve.example.com",
		Port: 8006,
	}
}

// TestBuildTransportOpts_ByteIdentical asserts that a config with all 5
// transport fields at their zero value produces Options whose transport fields
// are all 0 — byte-identical to any Options built before §7.30 was added.
// This is the critical regression-prevention test.
func TestBuildTransportOpts_ByteIdentical(t *testing.T) {
	t.Parallel()
	cfg := minCfg()
	// Confirm all 5 fields are unset (zero).
	if cfg.PVEDialTimeoutSec != 0 {
		t.Fatal("precondition: PVEDialTimeoutSec must be 0")
	}
	if cfg.PVETLSHandshakeTimeoutSec != 0 {
		t.Fatal("precondition: PVETLSHandshakeTimeoutSec must be 0")
	}
	if cfg.PVEMaxIdleConnsPerHost != 0 {
		t.Fatal("precondition: PVEMaxIdleConnsPerHost must be 0")
	}
	if cfg.PVEIdleConnTimeoutSec != 0 {
		t.Fatal("precondition: PVEIdleConnTimeoutSec must be 0")
	}
	if cfg.PVETCPKeepAliveSec != 0 {
		t.Fatal("precondition: PVETCPKeepAliveSec must be 0")
	}

	opts := buildTransportOpts(cfg)

	// Connection fields.
	if opts.Host != "pve.example.com" {
		t.Errorf("Host: got %q, want %q", opts.Host, "pve.example.com")
	}
	if opts.Port != 8006 {
		t.Errorf("Port: got %d, want 8006", opts.Port)
	}
	if opts.Protocol != sdkclient.ProtocolHTTPS {
		t.Errorf("Protocol: got %q, want %q", opts.Protocol, sdkclient.ProtocolHTTPS)
	}
	if opts.Timeout != 30*time.Minute {
		t.Errorf("Timeout: got %v, want 30m", opts.Timeout)
	}

	// Transport fields — all must be 0 when config is unset.
	if opts.DialTimeoutSec != 0 {
		t.Errorf("DialTimeoutSec: got %d, want 0 (byte-identical)", opts.DialTimeoutSec)
	}
	if opts.TLSHandshakeTimeoutSec != 0 {
		t.Errorf("TLSHandshakeTimeoutSec: got %d, want 0 (byte-identical)", opts.TLSHandshakeTimeoutSec)
	}
	if opts.MaxIdleConnsPerHost != 0 {
		t.Errorf("MaxIdleConnsPerHost: got %d, want 0 (byte-identical)", opts.MaxIdleConnsPerHost)
	}
	if opts.IdleConnTimeoutSec != 0 {
		t.Errorf("IdleConnTimeoutSec: got %d, want 0 (byte-identical)", opts.IdleConnTimeoutSec)
	}
	if opts.TCPKeepAliveSec != 0 {
		t.Errorf("TCPKeepAliveSec: got %d, want 0 (byte-identical)", opts.TCPKeepAliveSec)
	}
}

// TestBuildTransportOpts_AllFieldsSet asserts that non-zero config values are
// forwarded 1:1 to the corresponding Options fields.
func TestBuildTransportOpts_AllFieldsSet(t *testing.T) {
	t.Parallel()
	cfg := minCfg()
	cfg.PVEDialTimeoutSec = 5
	cfg.PVETLSHandshakeTimeoutSec = 10
	cfg.PVEMaxIdleConnsPerHost = 20
	cfg.PVEIdleConnTimeoutSec = 90
	cfg.PVETCPKeepAliveSec = 30

	opts := buildTransportOpts(cfg)

	if opts.DialTimeoutSec != 5 {
		t.Errorf("DialTimeoutSec: got %d, want 5", opts.DialTimeoutSec)
	}
	if opts.TLSHandshakeTimeoutSec != 10 {
		t.Errorf("TLSHandshakeTimeoutSec: got %d, want 10", opts.TLSHandshakeTimeoutSec)
	}
	if opts.MaxIdleConnsPerHost != 20 {
		t.Errorf("MaxIdleConnsPerHost: got %d, want 20", opts.MaxIdleConnsPerHost)
	}
	if opts.IdleConnTimeoutSec != 90 {
		t.Errorf("IdleConnTimeoutSec: got %d, want 90", opts.IdleConnTimeoutSec)
	}
	if opts.TCPKeepAliveSec != 30 {
		t.Errorf("TCPKeepAliveSec: got %d, want 30", opts.TCPKeepAliveSec)
	}
	// Connection fields unaffected.
	if opts.Timeout != 30*time.Minute {
		t.Errorf("Timeout unchanged: got %v, want 30m", opts.Timeout)
	}
}

// TestBuildTransportOpts_PartialSet asserts that only the set fields propagate;
// unset fields remain 0.
func TestBuildTransportOpts_PartialSet(t *testing.T) {
	t.Parallel()
	cfg := minCfg()
	cfg.PVEDialTimeoutSec = 15
	// All others zero.

	opts := buildTransportOpts(cfg)

	if opts.DialTimeoutSec != 15 {
		t.Errorf("DialTimeoutSec: got %d, want 15", opts.DialTimeoutSec)
	}
	if opts.TLSHandshakeTimeoutSec != 0 {
		t.Errorf("TLSHandshakeTimeoutSec: got %d, want 0", opts.TLSHandshakeTimeoutSec)
	}
	if opts.MaxIdleConnsPerHost != 0 {
		t.Errorf("MaxIdleConnsPerHost: got %d, want 0", opts.MaxIdleConnsPerHost)
	}
	if opts.IdleConnTimeoutSec != 0 {
		t.Errorf("IdleConnTimeoutSec: got %d, want 0", opts.IdleConnTimeoutSec)
	}
	if opts.TCPKeepAliveSec != 0 {
		t.Errorf("TCPKeepAliveSec: got %d, want 0", opts.TCPKeepAliveSec)
	}
}

// TestBuildTransportOpts_DefaultPort asserts the port=0 → 8006 default applies.
func TestBuildTransportOpts_DefaultPort(t *testing.T) {
	t.Parallel()
	cfg := minCfg()
	cfg.Port = 0

	opts := buildTransportOpts(cfg)
	if opts.Port != 8006 {
		t.Errorf("Port: got %d, want 8006 (default)", opts.Port)
	}
}

// TestNewClient_TransportFieldsSet verifies that NewClientWithTracer succeeds
// when all 5 transport-tuning fields are set to non-zero values in the
// config. This is an integration test over the full client construction path.
func TestNewClient_TransportFieldsSet(t *testing.T) {
	t.Parallel()

	//nolint:modernize // helper supports non-zero bool values; new(bool) only gives false
	boolPtr := func(b bool) *bool { return &b }

	cfg := &config.CPIConfig{
		Host:                      "pve.example.com",
		Port:                      8006,
		User:                      "root",
		Password:                  "secret",
		Realm:                     "pam",
		VerifySSL:                 boolPtr(true),
		PVEDialTimeoutSec:         5,
		PVETLSHandshakeTimeoutSec: 10,
		PVEMaxIdleConnsPerHost:    20,
		PVEIdleConnTimeoutSec:     90,
		PVETCPKeepAliveSec:        30,
	}
	c, err := NewClientWithTracer(cfg, nopLogger(), nil)
	if err != nil {
		t.Fatalf("expected no error with transport fields set, got: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

// A pool of more than one endpoint takes the 5-second dial and 10-second
// handshake defaults for each value the operator left at 0.
func TestPoolTimeoutDefaultsForMoreThanOneEndpoint(t *testing.T) {
	t.Parallel()
	cfg := minCfg()
	cfg.Endpoints = []string{"pve.example.com:8006", "pve2.example.com:8006"}
	before := buildTransportOpts(cfg)
	got := applyPoolTimeoutDefaults(before, 2)
	if got.DialTimeoutSec != 5 {
		t.Errorf("DialTimeoutSec = %d, want 5", got.DialTimeoutSec)
	}
	if got.TLSHandshakeTimeoutSec != 10 {
		t.Errorf("TLSHandshakeTimeoutSec = %d, want 10", got.TLSHandshakeTimeoutSec)
	}
	// Nothing else moves.
	got.DialTimeoutSec, got.TLSHandshakeTimeoutSec = 0, 0
	if got.Host != before.Host || got.Port != before.Port || got.Timeout != before.Timeout ||
		got.MaxIdleConnsPerHost != before.MaxIdleConnsPerHost || got.IdleConnTimeoutSec != before.IdleConnTimeoutSec ||
		got.TCPKeepAliveSec != before.TCPKeepAliveSec {
		t.Errorf("applyPoolTimeoutDefaults changed more than the two timeouts: %+v vs %+v", got, before)
	}
}

// An operator's value wins, for either timeout, and the other still defaults.
func TestPoolTimeoutDefaultsKeepTheOperatorsValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		dial, tls     int
		wantD, wantTL int
	}{
		{"dial set", 30, 0, 30, 10},
		{"handshake set", 0, 45, 5, 45},
		{"both set", 2, 3, 2, 3},
		{"both set above the defaults", 120, 60, 120, 60},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := minCfg()
			cfg.PVEDialTimeoutSec = tc.dial
			cfg.PVETLSHandshakeTimeoutSec = tc.tls
			got := applyPoolTimeoutDefaults(buildTransportOpts(cfg), 3)
			if got.DialTimeoutSec != tc.wantD || got.TLSHandshakeTimeoutSec != tc.wantTL {
				t.Errorf("dial/handshake = %d/%d, want %d/%d",
					got.DialTimeoutSec, got.TLSHandshakeTimeoutSec, tc.wantD, tc.wantTL)
			}
		})
	}
}

// A pool of one endpoint keeps today's options, so a single-endpoint client
// with discovery off builds the same Options as before the pool existed.
func TestPoolTimeoutDefaultsLeaveAPoolOfOneAlone(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 1} {
		cfg := minCfg()
		before := buildTransportOpts(cfg)
		got := applyPoolTimeoutDefaults(before, size)
		if got.DialTimeoutSec != 0 || got.TLSHandshakeTimeoutSec != 0 {
			t.Errorf("pool of %d: dial/handshake = %d/%d, want 0/0", size, got.DialTimeoutSec, got.TLSHandshakeTimeoutSec)
		}
		if got.WrapTransport != nil {
			t.Errorf("pool of %d: WrapTransport set", size)
		}
	}
	cfg := minCfg()
	cfg.PVEDialTimeoutSec = 7
	if got := applyPoolTimeoutDefaults(buildTransportOpts(cfg), 1); got.DialTimeoutSec != 7 {
		t.Errorf("pool of one changed the operator's dial timeout to %d", got.DialTimeoutSec)
	}
}

// buildTransportOpts itself never reads the endpoint list, so a list changes
// nothing it returns.
func TestBuildTransportOptsIgnoresTheEndpointList(t *testing.T) {
	t.Parallel()
	plain := buildTransportOpts(minCfg())
	cfg := minCfg()
	cfg.Endpoints = []string{"pve.example.com:8006", "pve2.example.com:8006"}
	listed := buildTransportOpts(cfg)
	if plain.DialTimeoutSec != listed.DialTimeoutSec || plain.TLSHandshakeTimeoutSec != listed.TLSHandshakeTimeoutSec ||
		plain.WrapTransport != nil || listed.WrapTransport != nil || plain.Host != listed.Host {
		t.Errorf("buildTransportOpts reads the endpoint list: %+v vs %+v", plain, listed)
	}
}
