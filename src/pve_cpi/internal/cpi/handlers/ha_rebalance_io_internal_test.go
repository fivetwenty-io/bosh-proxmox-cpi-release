package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestRebalanceTaskUser(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.CPIConfig
		want string
	}{
		{"no config", nil, ""},
		{"token", &config.CPIConfig{APIToken: "bosh@pve!cpi=secret", User: "root"}, "bosh@pve!cpi"},
		{"no user", &config.CPIConfig{}, ""},
		{"user with a realm", &config.CPIConfig{User: "bosh@pve", Realm: "pam"}, "bosh@pve"},
		{"user and realm", &config.CPIConfig{User: "bosh", Realm: "pve"}, "bosh@pve"},
		{"user with no realm", &config.CPIConfig{User: "root"}, "root@pam"},
	}
	for _, c := range cases {
		if got := rebalanceTaskUser(Deps{Config: c.cfg}); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRebalanceOwnedRow(t *testing.T) {
	p := newPVERebalanceIO(Deps{Config: icMinConfig(), RequestDirectorUUID: aaDirector}, RebalanceOptions{})
	ours := rbTags()
	cases := []struct {
		name string
		tags string
		want bool
	}{
		{"ours", ours, true},
		{"not under HA", ownerTagPrefix + aaDirector, false},
		{"another Director", haManagedTag + ";" + ownerTagPrefix + "dir-2", false},
		{"two owners", ours + ";" + ownerTagPrefix + "dir-2", false},
		{"a parker", ours + ";" + pve.ParkerTag, false},
		{"the create-env Director", ours + ";" + directorTagPrefix + createEnvDirectorName, false},
	}
	for _, c := range cases {
		row := &rebalanceResourceRow{Type: "qemu", VMID: 101, Tags: c.tags}
		if got := p.ownedRow(row); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRebalancePVEIOSleepStopsOnCancel(t *testing.T) {
	p := newPVERebalanceIO(Deps{Config: icMinConfig()}, RebalanceOptions{})
	if err := p.sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.sleep(ctx, time.Hour); err == nil {
		t.Fatal("a canceled sleep returned no error")
	}
	if p.now().IsZero() {
		t.Fatal("now is zero")
	}
}

func TestRebalanceHA_RefusesARunThatCannotStart(t *testing.T) {
	cases := map[string]Deps{
		"no config":     {RequestDirectorUUID: aaDirector},
		"no Director":   {Config: icMinConfig(), RequestDirectorUUID: "  "},
		"no PVE client": {Config: icMinConfig(), RequestDirectorUUID: aaDirector},
	}
	for name, deps := range cases {
		if _, err := RebalanceHA(context.Background(), deps, RebalanceOptions{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestRebalanceRun_ReportsTheClusterName(t *testing.T) {
	f := newFakeRebalanceIO()
	f.profileVal.ClusterName = "lab"
	if rep := rbRun(f, RebalanceOptions{}).run(context.Background()); rep.Cluster != "lab" {
		t.Fatalf("cluster %q", rep.Cluster)
	}
}
