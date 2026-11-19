package handlers

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// TestAdmitDiskIntoParker_RefusesARegisteredParkerTheRequestRetires proves a
// later step of a request never parks a disk in a registered parker that the
// request has queued for retirement, so the drain can never destroy a parker
// that a step of the same request planned to park in. An unregistered parker
// is never retired, so the request may still park in it.
func TestAdmitDiskIntoParker_RefusesARegisteredParkerTheRequestRetires(t *testing.T) {
	const volid = "nfs:vm-700-disk-1"
	registered := registeredParkerRig(config.HAModeAuto, "nfs:vm-90000-disk-0")
	registered.cluster.setPin(false, "pve01", ourMarker()+" bosh-vols=nfs")
	unregistered := newPHRig(config.HAModeAuto, map[int]map[string]any{90000: phParker("nfs:vm-90000-disk-0")})

	defer setRetireEmptiedParkerForTest(func(context.Context, Deps, string, int, string) {})()
	for name, tc := range map[string]struct {
		rig     *phRig
		queued  bool
		admits  bool
		meaning string
	}{
		"registered and not queued":   {rig: registered, admits: true, meaning: "the parker takes the disk"},
		"registered and queued":       {rig: registered, queued: true, meaning: "the request retires the parker"},
		"unregistered and queued":     {rig: unregistered, queued: true, admits: true, meaning: "nothing retires it"},
		"unregistered and not queued": {rig: unregistered, admits: true, meaning: "the parker takes every disk"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, drain := deferParkerRetirements(context.Background())
			defer drain()
			if tc.queued {
				retireEmptiedParker(ctx, tc.rig.deps, phNode, 90000, "test")
			}
			ok, err := admitDiskIntoParker(ctx, tc.rig.deps, phNode, 90000, volid)
			if err != nil || ok != tc.admits {
				t.Fatalf("ok %v err %v, want ok %v because %s", ok, err, tc.admits, tc.meaning)
			}
		})
	}
}
