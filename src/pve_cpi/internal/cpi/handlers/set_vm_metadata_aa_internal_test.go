package handlers

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// aaMetadataDeps returns deps whose settings let a VM join its group's rules
// on a cluster of four nodes, so a rule holds at most three members.
func aaMetadataDeps(stub *aaClusterStub) Deps {
	deps := aaDeps(stub)
	deps.Config = aaMembershipConfig("off", false, 0)
	deps.HAProfile = pve9ClusterProfile("pve1", "pve2", "pve3", "pve4")
	return deps
}

// markWebMembers gives each VMID an HA resource marked as a member of the
// "web" group of the test Director.
func markWebMembers(stub *aaClusterStub, vmids ...int) {
	for _, v := range vmids {
		stub.resources[haResourceSid(v)] = true
		stub.resComments[haResourceSid(v)] = aaIdent("web").resourceMarker("", 1700000000+int64(v))
	}
}

// TestJoinAntiAffinityAtMetadata_PlacesTheVMByItsMetadataIndex pins that the
// join reads the instance index from the metadata, so a VM whose guest does
// not show its index tag yet still takes its place by index. On four nodes a
// rule holds three members, and the VM with index 0 takes the place the
// member with index 3 would take if the VM's index were unknown.
func TestJoinAntiAffinityAtMetadata_PlacesTheVMByItsMetadataIndex(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.tagged(aaQEMU(100, aaTag+";index--1"), aaQEMU(101, aaTag+";index--2"), aaQEMU(102, aaTag+";index--3"),
		aaQEMU(103, aaTag))
	markWebMembers(stub, 100, 101, 102, 103)
	joinAntiAffinityAtMetadata(context.Background(), aaMetadataDeps(stub), 103, map[string]any{"index": 0.0},
		log.NewNopLogger())
	if got := stub.rules[aaRule]; got != "vm:100,vm:101,vm:103" {
		t.Fatalf("rule = %q; want vm:103 in by its index 0 and vm:102 (index 3) out", got)
	}
}

// TestJoinAntiAffinityAtMetadata_WritesNothingForAVMItDoesNotOwn pins that
// the join reads the group from the VM's own HA resource, and writes no rule
// for a VM with no HA resource, with one from an older CPI, or with one
// another Director marked.
func TestJoinAntiAffinityAtMetadata_WritesNothingForAVMItDoesNotOwn(t *testing.T) {
	t.Parallel()
	for name, comment := range map[string]*string{
		"no resource":    nil,
		"unmarked":       new("staged by hand"),
		"other director": new(haIdentity{Owner: aaOtherDirector, Deployment: aaDeployment, Group: "web"}.marker()),
		"no group":       new(haIdentity{Owner: aaDirector}.marker()),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			stub.tagged(aaQEMU(100, aaTag+";index--0"))
			markWebMembers(stub, 100)
			if comment != nil {
				stub.resources["vm:101"] = true
				stub.resComments["vm:101"] = *comment
			}
			joinAntiAffinityAtMetadata(context.Background(), aaMetadataDeps(stub), 101,
				map[string]any{"index": "1"}, log.NewNopLogger())
			if len(stub.rules) != 0 || stub.createRuleCalls+stub.updateRuleCalls != 0 {
				t.Fatalf("rules = %v; want none written", stub.rules)
			}
		})
	}
}

// TestJoinAntiAffinityAtMetadata_ReadsNothingWhenTheSettingsAreOff pins that
// no setting that turns the join off costs a PVE call.
func TestJoinAntiAffinityAtMetadata_ReadsNothingWhenTheSettingsAreOff(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*Deps){
		"mode off":          func(d *Deps) { d.Config.HA = haModeOff() },
		"anti_affinity off": func(d *Deps) { d.Config.HA.AntiAffinity = haChoiceOf(config.HARuleOff) },
		"scheduler off":     func(d *Deps) { d.Config.Placement.AntiAffinity = nil },
		"no director":       func(d *Deps) { d.RequestDirectorUUID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := newAAStub()
			stub.tagged(aaQEMU(100, aaTag+";index--0"), aaQEMU(101, aaTag))
			markWebMembers(stub, 100, 101)
			deps := aaMetadataDeps(stub)
			mutate(&deps)
			joinAntiAffinityAtMetadata(context.Background(), deps, 101, map[string]any{"index": "1"},
				log.NewNopLogger())
			if stub.listHaResCalls != 0 || len(stub.rules) != 0 {
				t.Fatalf("HA resource reads = %d, rules = %v; want no call", stub.listHaResCalls, stub.rules)
			}
		})
	}
}

// TestJoinAntiAffinityAtMetadata_AFailedWriteOnlyWarns pins that a join that
// fails is logged and never returned, so set_vm_metadata still succeeds.
func TestJoinAntiAffinityAtMetadata_AFailedWriteOnlyWarns(t *testing.T) {
	t.Parallel()
	stub := newAAStub()
	stub.tagged(aaQEMU(100, aaTag+";index--0"), aaQEMU(101, aaTag))
	markWebMembers(stub, 100, 101)
	stub.failCreateRule = true
	logger, obs := log.NewObservedLogger(slog.LevelDebug)
	joinAntiAffinityAtMetadata(context.Background(), aaMetadataDeps(stub), 101, map[string]any{"index": "1"},
		logger)
	for _, e := range obs.All() {
		if e.Level == slog.LevelWarn && strings.Contains(e.Message, "could not join") {
			return
		}
	}
	t.Fatalf("no warning about the failed join in %+v", obs.All())
}

func TestMetadataIndex_ReadsAStringOrANumber(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   any
		want int
	}{
		{"3", 3}, {3.0, 3}, {0.0, 0}, {"0", 0}, {"03", -1}, {nil, -1}, {-1.0, -1}, {2.5, -1}, {"x", -1},
		{"", -1}, {true, -1},
	} {
		md := map[string]any{"index": tc.in}
		if got := metadataIndex(md); got != tc.want {
			t.Errorf("metadataIndex(%#v) = %d; want %d", tc.in, got, tc.want)
		}
	}
	if got := metadataIndex(map[string]any{}); got != -1 {
		t.Errorf("metadataIndex of no index = %d; want -1", got)
	}
}
