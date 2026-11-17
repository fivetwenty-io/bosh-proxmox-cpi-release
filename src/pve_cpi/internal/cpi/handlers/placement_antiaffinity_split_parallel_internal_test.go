package handlers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// splitCreate is aaFleet.create in split mode on a cluster of three nodes,
// logging to logger. Its clone tags the guest before it stages its HA
// resource, so a sibling that plans in between sees a member that is not an
// HA resource yet.
func (f *aaFleet) splitCreate(vmid int, mode string, logger *log.Logger) func(step func()) error {
	return func(step func()) error {
		deps := f.deps(step)
		deps.Config.HA = &config.HAConfig{Mode: haChoiceOf(mode),
			AntiAffinityOverflow: haChoiceOf(config.HAOverflowSplit)}
		deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3")
		f.tagged[vmid] = true
		step()
		sid := haResourceSid(vmid)
		f.stub.resources[sid] = true
		f.stub.resComments[sid] = haIdentity{Owner: aaDirector}.marker()
		step()
		return ensureAntiAffinityMembership(context.Background(), deps, aaIdent("web"), vmid, logger)
	}
}

// TestEnsureAntiAffinity_SplitParallelCreatesNameOnlyHAResources runs four
// sibling creates of one group in split mode on three nodes, with the rules
// lock off, their reads, plans, and writes interleaved in many orders. Each
// create tags its guest before it stages its HA resource, and PVE refuses a
// rule that names a resource it does not manage. No write may name a sibling
// that is not an HA resource yet, because PVE refuses it, and the create that
// sent it then waits and plans again, or fails once its attempts run out. No
// create may fail. The group's first rule must end with
// three members and no second rule may exist, since one member alone cannot
// make a negative rule. The member left in no rule must be logged at Warn,
// naming the group's second rule.
func TestEnsureAntiAffinity_SplitParallelCreatesNameOnlyHAResources(t *testing.T) {
	t.Parallel()
	vmids := []int{101, 102, 103, 104}
	for _, mode := range aaModes {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			for seed := range uint64(aaConcurrentSeeds) {
				run := fmt.Sprintf("%s, seed %d", mode, seed)
				f := newAAFleet()
				logger, observed := log.NewObservedLogger(log.LevelDebug)
				workers := make([]func(step func()) error, 0, len(vmids))
				for _, v := range vmids {
					workers = append(workers, f.splitCreate(v, mode, logger))
				}
				for i, err := range runAATurns(seed, workers) {
					if err != nil {
						t.Fatalf("%s: create of %d failed, which rolls back a built VM: %v", run, vmids[i], err)
					}
				}
				requireSplitRunRight(t, run, f, observed, vmids)
			}
		})
	}
}

// requireSplitRunRight checks one run of the split creates. No write was
// refused for naming a sibling that is not an HA resource yet, the first rule
// holds three members, no second rule exists, and the member in no rule was
// warned about, naming the second rule.
func requireSplitRunRight(t *testing.T, run string, f *aaFleet, observed *log.Observer, vmids []int) {
	t.Helper()
	for _, e := range observed.All() {
		if err := fmt.Sprint(e.Attrs["error"]); strings.Contains(err, "unmanaged resource") {
			t.Fatalf("%s: a write named a sibling that is not an HA resource yet: %s", run, err)
		}
	}
	second := aaChunkName(aaRule, 2)
	first := strings.Split(f.stub.rules[aaRule], ",")
	if len(first) != 3 {
		t.Fatalf("%s: first rule = %q; want three members", run, f.stub.rules[aaRule])
	}
	if got, ok := f.stub.rules[second]; ok {
		t.Fatalf("%s: second rule = %q; want none for one member", run, got)
	}
	var lone string
	for _, v := range vmids {
		if sid := haResourceSid(v); !slices.Contains(first, sid) {
			lone = sid
		}
	}
	for _, e := range observed.All() {
		if strings.Contains(e.Message, "member is in none of the group's rules") &&
			e.Level == log.LevelWarn && e.Attrs["sid"] == lone && e.Attrs["rule"] == second {
			return
		}
	}
	t.Fatalf("%s: %s is in no rule and no create warned about it", run, lone)
}
