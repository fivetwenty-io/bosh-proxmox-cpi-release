package pve

import (
	"testing"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

func TestSetEndpointNodesForTestNamesConfiguredEndpointsAndRestores(t *testing.T) {
	cfg := &config.CPIConfig{Host: "10.0.0.1", Port: 8006, Endpoints: []string{"10.0.0.1:8006", "10.0.0.2:8006"}}
	opts := sdkclient.Options{Host: "10.0.0.1", Port: 8006}

	restore := SetEndpointNodesForTest(map[string]string{"10.0.0.2:8006": "pve2"})
	plan, err := planEndpoints(cfg, opts, noDiscoveredEndpoints{})
	if err != nil {
		t.Fatalf("planEndpoints: %v", err)
	}
	if got := []string{plan.endpoints[0].Node, plan.endpoints[1].Node}; got[0] != "" || got[1] != "pve2" {
		t.Errorf("nodes = %q, want only the second endpoint named pve2", got)
	}

	restore()
	plan, err = planEndpoints(cfg, opts, noDiscoveredEndpoints{})
	if err != nil {
		t.Fatalf("planEndpoints after restore: %v", err)
	}
	if got := []string{plan.endpoints[0].Node, plan.endpoints[1].Node}; got[0] != "" || got[1] != "" {
		t.Errorf("nodes after restore = %q, want none", got)
	}
}
