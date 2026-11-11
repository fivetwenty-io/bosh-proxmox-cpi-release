package pve

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// pve.node_endpoints holds addresses vetted for uploads, and some of them are
// link0 addresses where pveproxy may not listen. The failover pool never takes
// them. Each case lists configured endpoints that refuse connections, names a
// healthy server in node_endpoints, and sends a read through the production
// client. A client that seeded the pool from node_endpoints would dial that
// server, fail the read over to it, list it in the pool-built line, or name it
// in the error that says no configured endpoint answered.
func TestNodeEndpointsNeverSeedTheFailoverPool(t *testing.T) {
	cases := []struct {
		name       string
		configured []string
		mode       config.DiscoveryMode
	}{
		{"one host, discovery off", nil, config.DiscoveryModeOff},
		{"one host, discovery on", nil, config.DiscoveryModeOn},
		{
			"two endpoints, discovery off",
			[]string{"pve1." + coldWireDomain + ":8006", "pve2." + coldWireDomain + ":8006"},
			config.DiscoveryModeOff,
		},
		{
			"two endpoints, discovery on",
			[]string{"pve1." + coldWireDomain + ":8006", "pve2." + coldWireDomain + ":8006"},
			config.DiscoveryModeOn,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lab := newColdLab(t)
			// The routed host answers, and no configured endpoint is routed
			// anywhere, so every dial to one of them is refused.
			routes := pvetest.NewRoutes()
			routes.JSON(http.MethodGet, "/pools/p1", http.StatusOK, tetherPoolBody)
			leaf := lab.ca.NewLeaf(t, pvetest.LeafOptions{DNSNames: []string{"upload.lab"}})
			routed := pvetest.NewLive(t, leaf, routes)
			lab.route("upload.lab", routed)
			cfg := lab.config(t)
			cfg.Placement = nil
			cfg.Endpoints = tc.configured
			cfg.APIEndpointDiscovery = tc.mode
			cfg.NodeEndpoints = map[string]string{"pve9": "upload.lab:8006"}
			logger, obs := log.NewObservedLogger(log.LevelDebug)
			c, err := newClient(cfg, logger, nil)
			if err != nil {
				t.Fatalf("newClient: %v", err)
			}

			_, _, readErr := c.Pools().GetPoolComment(t.Context(), "p1")

			if readErr == nil {
				t.Fatal("the read succeeded, so the client reached the node_endpoints server")
			}
			if len(tc.configured) > 1 {
				var walk *endpointpool.EntryUnreachableError
				if !errors.As(readErr, &walk) {
					t.Errorf("read error = %v, want the walk's unreachable error", readErr)
				}
			}
			if strings.Contains(readErr.Error(), "upload.lab") {
				t.Errorf("the read error names the node_endpoints host: %v", readErr)
			}
			if n := lab.dials("upload.lab:8006"); n != 0 {
				t.Errorf("the client dialed the node_endpoints host %d times, want none", n)
			}
			if got := len(routed.Seen()); got != 0 {
				t.Errorf("the node_endpoints server read %d requests, want none", got)
			}
			for _, line := range obs.All() {
				if strings.Contains(line.Message, "upload.lab") {
					t.Errorf("a log line names the node_endpoints host: %q", line.Message)
				}
			}
			if len(tc.configured) > 1 {
				want := "pve endpoints: pool [" + tc.configured[0] + " configured, " + tc.configured[1] +
					" configured], starting on "
				if lines := linesAt(obs.All(), log.LevelDebug, want); len(lines) != 1 {
					t.Errorf("pool-built lines = %v, want one that lists the configured endpoints alone", lines)
				}
			}
			drainProcessRegistry(t)
		})
	}
}
