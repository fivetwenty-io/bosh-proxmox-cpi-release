package main

import (
	"context"
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

// stubCluster is a cluster service that answers nothing. The reader only needs
// a non-nil service to build the inputs, and the test never lets Resolve run.
type stubCluster struct{ sdkcluster.Service }

// stubNodes is the matching nodes service.
type stubNodes struct{ sdknodes.Service }

// stubStorage is the matching cluster storage service.
type stubStorage struct{ sdkclusterstorage.Service }

// readyDoctorClient answers every accessor the doctor's profile reads with a
// non-nil service.
type readyDoctorClient struct{ pve.Client }

func (*readyDoctorClient) Cluster() sdkcluster.Service               { return stubCluster{} }
func (*readyDoctorClient) Nodes() sdknodes.Service                   { return stubNodes{} }
func (*readyDoctorClient) ClusterStorage() sdkclusterstorage.Service { return stubStorage{} }

// TestTheDoctorsProfileResolvesFromTheDoctorsInputs proves the path the ha
// subcommand takes. A reader whose profile bypassed doctorHADeps would hand
// Resolve the production inputs, which hold the profile cache and stop in mode
// off, and the helper's own test would still pass.
func TestTheDoctorsProfileResolvesFromTheDoctorsInputs(t *testing.T) {
	cfg := cacheKeyableConfig()
	client := &readyDoctorClient{}
	if ha.DepsFromClient(client, cfg, nil).Slow == nil {
		t.Fatal("the test entry must be able to key a profile cache")
	}

	var got []ha.Deps
	want := &ha.Profile{}
	reader := &pveHAReader{client: client, cfg: cfg, resolveProfile: func(_ context.Context, deps ha.Deps) (*ha.Profile, error) {
		got = append(got, deps)
		return want, nil
	}}
	profile, err := reader.Profile(context.Background())
	if err != nil || profile != want {
		t.Fatalf("Profile = %v, %v, want the resolved profile", profile, err)
	}
	if len(got) != 1 {
		t.Fatalf("Profile resolved %d times, want once", len(got))
	}
	if got[0].Slow != nil {
		t.Errorf("the doctor's profile must read every fact live, but it holds %T", got[0].Slow)
	}
	if !got[0].ProbeWhenOff {
		t.Error("the doctor's profile must probe in mode off")
	}
	if got[0].Config != cfg || got[0].Cluster == nil || got[0].Nodes == nil || got[0].Storage == nil {
		t.Errorf("the doctor's profile must resolve against its own configuration and clients, got %+v", got[0])
	}
}
