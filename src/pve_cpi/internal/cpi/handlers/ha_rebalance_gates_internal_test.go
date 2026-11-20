package handlers

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func rbConfig(mode string) *config.CPIConfig {
	cfg := icMinConfig()
	cfg.HA = &config.HAConfig{Mode: haChoiceOf(mode)}
	return cfg
}

func TestRebalanceGates_EachGateRefusesOnItsOwn(t *testing.T) {
	healthy := func() (*ha.Profile, *rebalanceSnapshot) {
		return rbProfile(), &rebalanceSnapshot{states: map[string]string{"vm:101": "started"}}
	}
	cases := []struct {
		gate   string
		apply  bool
		mode   string
		breakk func(p *ha.Profile, s *rebalanceSnapshot, a *rebalanceActivity, j *rebalanceJournal)
		detail string
	}{
		{RebalanceGateProfile, false, config.HAModeOff, nil, "pve.ha.mode is off"},
		{RebalanceGateProfile, false, config.HAModeAuto, func(p *ha.Profile, _ *rebalanceSnapshot,
			_ *rebalanceActivity, _ *rebalanceJournal,
		) {
			p.Effective, p.Reason = ha.EffectiveObserveOnly, "PVE is older than 9"
		}, "older than 9"},
		{RebalanceGateQuorum, false, "", func(p *ha.Profile, _ *rebalanceSnapshot, _ *rebalanceActivity,
			_ *rebalanceJournal,
		) {
			p.Health.Quorate = false
		}, "quorum"},
		{RebalanceGateNodes, false, "", func(p *ha.Profile, _ *rebalanceSnapshot, _ *rebalanceActivity,
			_ *rebalanceJournal,
		) {
			p.Nodes[1].Online = false
		}, "pve02 is offline"},
		{RebalanceGateNodes, false, "", func(p *ha.Profile, _ *rebalanceSnapshot, _ *rebalanceActivity,
			_ *rebalanceJournal,
		) {
			p.Health.Nodes[2].Maintenance = true
		}, "pve03 is in maintenance"},
		{RebalanceGateNodes, false, "", func(p *ha.Profile, _ *rebalanceSnapshot, _ *rebalanceActivity,
			_ *rebalanceJournal,
		) {
			p.Health.Nodes[0].DeadOrFenced = true
		}, "pve01 can't take work"},
		{RebalanceGateNodes, false, "", func(p *ha.Profile, _ *rebalanceSnapshot, _ *rebalanceActivity,
			_ *rebalanceJournal,
		) {
			p.Health.Nodes = p.Health.Nodes[:2]
		}, "pve03 has no health entry"},
		{RebalanceGateManager, false, "", func(p *ha.Profile, _ *rebalanceSnapshot, _ *rebalanceActivity,
			_ *rebalanceJournal,
		) {
			p.CRMMaster = ""
		}, "no master"},
		{RebalanceGateManager, false, "", func(p *ha.Profile, _ *rebalanceSnapshot, _ *rebalanceActivity,
			_ *rebalanceJournal,
		) {
			p.Health.HA = pve.HAViewStale
		}, "not fresh"},
		{RebalanceGateSettled, false, "", func(_ *ha.Profile, s *rebalanceSnapshot, _ *rebalanceActivity,
			_ *rebalanceJournal,
		) {
			s.states["vm:102"] = "relocate"
		}, "vm:102"},
		{RebalanceGateJournal, true, "", func(_ *ha.Profile, _ *rebalanceSnapshot, _ *rebalanceActivity,
			j *rebalanceJournal,
		) {
			*j = rebalanceJournal{configured: true, err: errors.New("permission denied")}
		}, "vcap on the Director"},
		{RebalanceGateJournal, false, "", func(_ *ha.Profile, _ *rebalanceSnapshot, _ *rebalanceActivity,
			j *rebalanceJournal,
		) {
			*j = rebalanceJournal{configured: true, open: []aj.Record{{ID: "r1", State: aj.Submitted}}}
		}, "r1 (submitted)"},
		{RebalanceGateActivity, false, "", func(_ *ha.Profile, _ *rebalanceSnapshot, a *rebalanceActivity,
			_ *rebalanceJournal,
		) {
			a.locks = []string{"bosh-lock-ha-rules"}
		}, "bosh-lock-ha-rules"},
		{RebalanceGateActivity, false, "", func(_ *ha.Profile, _ *rebalanceSnapshot, a *rebalanceActivity,
			_ *rebalanceJournal,
		) {
			a.tasks = []string{"qmclone 200 on pve01"}
		}, "qmclone 200"},
		{RebalanceGateRunLock, false, "", func(_ *ha.Profile, _ *rebalanceSnapshot, a *rebalanceActivity,
			_ *rebalanceJournal,
		) {
			a.runLock = true
		}, "another rebalance"},
	}
	for _, c := range cases {
		t.Run(c.gate+" "+c.detail, func(t *testing.T) {
			p, s := healthy()
			var a rebalanceActivity
			var j rebalanceJournal
			if c.breakk != nil {
				c.breakk(p, s, &a, &j)
			}
			mode := c.mode
			if mode == "" {
				mode = config.HAModeAuto
			}
			gates := rebalanceGates(rbConfig(mode), p, s, a, j, c.apply)
			for _, g := range gates {
				switch {
				case g.Name == c.gate && (g.OK || !strings.Contains(g.Detail, c.detail)):
					t.Fatalf("gate %+v, want a refusal that names %q", g, c.detail)
				case g.Name != c.gate && !g.OK:
					t.Fatalf("gate %+v refused too", g)
				}
			}
		})
	}
}

func TestRebalanceGates_HealthyPasses(t *testing.T) {
	for _, apply := range []bool{false, true} {
		gates := rebalanceGates(rbConfig(config.HAModeManaged), rbProfile(), &rebalanceSnapshot{},
			rebalanceActivity{runLock: apply}, rebalanceJournal{configured: true, err: errors.New("no access")}, apply)
		for _, g := range gates {
			if g.Name == RebalanceGateJournal && !apply && !strings.Contains(g.Detail, "unchecked") {
				t.Fatalf("journal gate %+v", g)
			}
			if g.Name == RebalanceGateJournal && apply {
				continue
			}
			if !g.OK {
				t.Fatalf("apply %v: gate %+v refused", apply, g)
			}
		}
		if len(gates) != 8 {
			t.Fatalf("%d gates", len(gates))
		}
	}
}

func TestRebalanceGates_NilProfileFailsClosed(t *testing.T) {
	gates := rebalanceGates(rbConfig(config.HAModeAuto), nil, &rebalanceSnapshot{}, rebalanceActivity{},
		rebalanceJournal{}, false)
	for _, g := range gates[:4] {
		if g.OK {
			t.Fatalf("gate %+v passed with no profile", g)
		}
	}
}

func TestOpenJournalRecords(t *testing.T) {
	recs := []aj.Record{{ID: "a", State: aj.Planned}, {ID: "b", State: aj.Adopted},
		{ID: "c", State: aj.ReconciliationRequired}, {ID: "d", State: aj.Observed}}
	open := openJournalRecords(recs)
	if len(open) != 3 || open[0].ID != "a" || open[1].ID != "c" || open[2].ID != "d" {
		t.Fatalf("open %+v", open)
	}
}

func TestLiveSentinel(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	comment := func(exp time.Time) string { return "owner=x exp=" + strconv.FormatInt(exp.Unix(), 10) }
	switch {
	case !liveSentinel("owner=x", now):
		t.Fatal("a comment with no expiry must read as live, so the gate fails closed")
	case liveSentinel(comment(now.Add(-time.Minute)), now):
		t.Fatal("an expired sentinel reads as live")
	case !liveSentinel(comment(now.Add(time.Minute)), now):
		t.Fatal("a live sentinel reads as expired")
	}
}

func TestMovingServices(t *testing.T) {
	got := movingServices(map[string]string{"vm:3": "relocate", "vm:1": "migrate", "vm:2": "started"})
	if strings.Join(got, ",") != "vm:1,vm:3" {
		t.Fatalf("moving %v", got)
	}
}
