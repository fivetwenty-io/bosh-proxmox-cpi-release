package handlers

import (
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// A domain whose nodes the health report marks unplaceable is known to have
// no room, so the planner must not treat its count of 0 as unknown.
func TestAATopology_ZeroPlaceableIsKnownWhenTheProfileWasRead(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{Placement: &config.PlacementConfig{
		AZMap: map[string][]string{"az1": {"pve1", "pve2"}, "az2": {"pve3", "pve4"}},
	}}
	profile := &ha.Profile{HAVersionOK: true, Health: &pve.NodeHealthReport{}}
	for _, n := range []string{"pve1", "pve2", "pve3", "pve4"} {
		profile.Nodes = append(profile.Nodes, ha.NodeFacts{Name: n, Online: true, Version: "9.2.4"})
		profile.Health.Nodes = append(profile.Health.Nodes, pve.NodeHealthEntry{Name: n, Placeable: n == "pve3"})
	}
	doms := newAATopology(cfg, profile).domains(map[string]bool{"": true, "az1": true, "az2": true})
	for key, want := range map[string]int{"": 1, "az1": 0, "az2": 1} {
		d := doms[key]
		if !d.PlaceableKnown || d.Placeable != want {
			t.Errorf("domain %q: placeable %d known %v; want %d known", key, d.Placeable, d.PlaceableKnown, want)
		}
	}
}

// Without a profile the counts were never read, so every domain's count stays
// unknown and the planner keeps its rules.
func TestAATopology_NoProfileLeavesPlaceableUnknown(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{Placement: &config.PlacementConfig{
		AZMap: map[string][]string{"az1": {"pve1", "pve2"}},
	}}
	for key, d := range newAATopology(cfg, nil).domains(map[string]bool{"": true, "az1": true}) {
		if d.PlaceableKnown {
			t.Errorf("domain %q: placeable known without a profile", key)
		}
	}
}
