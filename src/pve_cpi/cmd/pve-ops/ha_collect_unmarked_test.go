package main

import (
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestCollectUnmarkedResource reports a VM whose HA resource carries no CPI
// marker. A local-disk VM is a problem, because PVE cannot recover it on
// another node, and any other VM is advice. A marked resource, a VM outside
// the entry's band, and a VM without bosh-cpi are left alone.
func TestCollectUnmarkedResource(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		tags     string
		comment  string
		storage  string
		severity string
		frags    []string
	}{
		{"local disk, hand-added", "bosh-cpi", "", "local-zfs", haSeverityProblem,
			[]string{"vm:101", "local-zfs:vm-101-disk-0", "cannot recover the VM on another node", "ha-manager remove vm:101", "bosh recreate"}},
		{"local disk, foreign-looking text", "bosh-cpi", "added by hand", "local-zfs", haSeverityProblem, []string{"vm:101"}},
		{"shared disk, hand-added", "bosh-cpi", "", "nfs1", haSeverityAdvice,
			[]string{"vm:101", "no node-affinity pin", "anti-affinity rule", "ha-manager remove vm:101", "bosh recreate"}},
		{"marked resource", "bosh-cpi", haMarker, "local-zfs", "", nil},
		{"another Director's marker", "bosh-cpi", "bosh-cpi:v1 owner=other", "local-zfs", "", nil},
		{"no bosh-cpi tag", "other", "", "local-zfs", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := haBaseFake()
			localVM(f, "n2", tc.tags)
			f.configs[101] = map[string]any{"scsi0": tc.storage + ":vm-101-disk-0,size=10G"}
			f.resources = append(f.resources, HAResourceRow{SID: "vm:101", State: "started", Comment: tc.comment})
			r := runCollect(t, f, haCfg("auto", "hold"))
			if tc.severity == "" {
				wantNone(t, r, haKindResourceUnmarked)
				return
			}
			want(t, r, haKindResourceUnmarked, tc.severity, tc.frags...)
			// The unmarked resource must not also draw the findings that read
			// a missing resource or the marker.
			wantNone(t, r, haKindLocalUnprotected, haKindPinMissing, haKindOffModeResource)
		})
	}
}

// TestCollectUnmarkedResourceStaysInTheBand leaves a VM that the entry's band
// does not hold to the entry that does.
func TestCollectUnmarkedResourceStaysInTheBand(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	localVM(f, "n2", "bosh-cpi")
	f.resources = append(f.resources, HAResourceRow{SID: "vm:101", State: "started"})
	run := func(start, end int) *haReport {
		for i := range f.guests {
			f.guests[i].Tags = "bosh-cpi;bosh-owner--" + haDirector
		}
		return collectHAScoped(t.Context(), f, haCfg("auto", "hold"), haDirector, haNow,
			&haEntryScope{Band: pve.VMIDBand{Start: start, End: end}, Policy: "hold", Leader: true})
	}
	want(t, run(100, 199), haKindResourceUnmarked, haSeverityProblem, "vm:101")
	wantNone(t, run(200, 299), haKindResourceUnmarked)
}

// TestCollectUnmarkedResourceOffMode leaves the resource to off_mode_resource.
func TestCollectUnmarkedResourceOffMode(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	localVM(f, "n2", "bosh-cpi")
	f.resources = append(f.resources, HAResourceRow{SID: "vm:101", State: "started"})
	f.profile.Mode, f.profile.Effective = "off", ha.EffectiveOff
	r := runCollect(t, f, haCfg("off", "hold"))
	reported := false
	for _, fd := range findings(r, haKindOffModeResource) {
		reported = reported || fd.Subject == "vm:101"
	}
	if !reported {
		t.Errorf("off_mode_resource did not name vm:101: %+v", r.Findings)
	}
	wantNone(t, r, haKindResourceUnmarked)
}

// TestHAResourceUnmarkedKindString pins the kind string scripts match and the
// JSON the finding is written as.
func TestHAResourceUnmarkedKindString(t *testing.T) {
	t.Parallel()
	if haKindResourceUnmarked != "ha_resource_unmarked" {
		t.Fatalf("kind = %q, want ha_resource_unmarked", haKindResourceUnmarked)
	}
	raw, err := json.Marshal(haFinding{Kind: haKindResourceUnmarked, Severity: haSeverityProblem, Subject: "vm:101", Detail: "d"})
	if err != nil {
		t.Fatal(err)
	}
	const wantJSON = `{"kind":"ha_resource_unmarked","severity":"problem","subject":"vm:101","detail":"d"}`
	if string(raw) != wantJSON {
		t.Errorf("json = %s, want %s", raw, wantJSON)
	}
}
