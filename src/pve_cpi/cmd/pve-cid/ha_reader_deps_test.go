package main

import (
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// cacheKeyableConfig is an entry that can key a profile cache, so the
// production wiring would put the cache in Deps.Slow.
func cacheKeyableConfig() *config.CPIConfig {
	cfg := &config.CPIConfig{Host: "pve1.dc1.example.com", APIToken: "cpi@pve!ha=11111111-2222-3333-4444-555555555555"}
	mode := config.HAChoice(config.HAModeOff)
	cfg.HA = &config.HAConfig{Mode: &mode}
	return cfg
}

// doctorStubClient answers the three service accessors DepsFromClient reads
// with none, which is enough to build the inputs and never probes.
type doctorStubClient struct{ pve.Client }

func (*doctorStubClient) Cluster() sdkcluster.Service               { return nil }
func (*doctorStubClient) Nodes() sdknodes.Service                   { return nil }
func (*doctorStubClient) ClusterStorage() sdkclusterstorage.Service { return nil }

func TestDoctorHADepsReadEveryFactLive(t *testing.T) {
	cfg := cacheKeyableConfig()
	client := &doctorStubClient{}

	// The same entry given to the production wiring gets a cache, so the
	// check below is not satisfied by an entry that never had one.
	if ha.DepsFromClient(client, cfg, nil).Slow == nil {
		t.Fatal("the test entry must be able to key a profile cache")
	}
	deps := doctorHADeps(client, cfg, nil)
	if deps.Slow != nil {
		t.Errorf("the doctor must not read slow facts from the profile cache, got %T", deps.Slow)
	}
	if !deps.ProbeWhenOff {
		t.Error("the doctor must probe in mode off")
	}
	if deps.Config != cfg {
		t.Error("the doctor must resolve against the configuration it was given")
	}
}
