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

// buildCustomTags drops every key whose entry would claim a CPI-owned prefix,
// whether the prefix is rebuilt by set_vm_metadata or written once at create
// time, and buildCustomTagsReporting names the keys it dropped.
func TestBuildCustomTags_DropsCPIOwnedPrefixes(t *testing.T) {
	t.Parallel()

	in := map[string]string{
		"bosh-owner":     "x",
		"BOSH_OWNER":     "y",
		"bosh-cid":       "z",
		"bosh-cluster":   "c",
		"stemcell":       "s",
		"director":       "d",
		"instance_group": "g",
		"vm-prefix":      "p",
		"env":            "prod",
		"bosh-owners":    "kept",
	}
	got, refused := buildCustomTagsReporting(in)
	if want := []string{"bosh-owners--kept", "env--prod"}; !slices.Equal(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
	wantRefused := []string{"BOSH_OWNER", "bosh-cid", "bosh-cluster", "bosh-owner", "director", "instance_group", "stemcell", "vm-prefix"}
	if !slices.Equal(refused, wantRefused) {
		t.Errorf("refused = %v, want %v", refused, wantRefused)
	}
	if plain := buildCustomTags(in); !slices.Equal(plain, got) {
		t.Errorf("buildCustomTags = %v, want %v", plain, got)
	}
}

// The create-time identity prefixes are CPI-owned, so set_disk_metadata's
// filter refuses them too.
func TestHasCPIOwnedPrefix_IdentityPrefixes(t *testing.T) {
	t.Parallel()

	for _, e := range []string{"bosh-owner--a", "Bosh-Owner--a", "bosh-cid--a", "bosh-cluster--a"} {
		if !hasCPIOwnedPrefix(e) {
			t.Errorf("hasCPIOwnedPrefix(%q) = false, want true", e)
		}
	}
	if hasCPIOwnedPrefix("bosh-cpi") {
		t.Error("hasCPIOwnedPrefix(bosh-cpi) = true; the bare ownership tag is not a prefix")
	}
}

// hoistHeadTags puts bosh-cpi first and the identity tags right behind it,
// and leaves everything else in its original order.
func TestHoistHeadTags(t *testing.T) {
	t.Parallel()

	in := []string{"advrt-a", "bosh-cluster--1", "bosh-cpi", "bosh-owner--u", "env--prod", "bosh-cid--c"}
	want := []string{"bosh-cpi", "bosh-cluster--1", "bosh-owner--u", "bosh-cid--c", "advrt-a", "env--prod"}
	if got := hoistHeadTags(in); !slices.Equal(got, want) {
		t.Errorf("hoistHeadTags = %v, want %v", got, want)
	}
}

// A job named after a reserved prefix shares that prefix with a CPI tag, so
// its tags cannot be read as identity evidence.
func TestStoredInstanceIdentities_IgnoresAmbiguousJobPrefix(t *testing.T) {
	t.Parallel()

	tags := []string{"bosh-cpi", "job--director", "director--9f0e1d2c-3b4a-4596-8877-665544332211"}
	if ev := storedInstanceEvidence("", tags); len(ev.values) != 0 || len(ev.orphans) != 0 {
		t.Errorf("storedInstanceEvidence = %+v; want none", ev)
	}
}

func TestStoredInstanceEvidence_UntiedUUIDTag(t *testing.T) {
	t.Parallel()

	const other = "9f0e1d2c-3b4a-4596-8877-665544332211"
	cases := []struct {
		name       string
		tags       []string
		wantOrphan bool
	}{
		{"name tag with no job record", []string{"bosh-cpi", "web--" + other}, true},
		{"name tag tied to its job", []string{"bosh-cpi", "job--web", "web--" + other}, false},
		{"name tag under a different job", []string{"bosh-cpi", "job--api", "web--" + other}, true},
		{"CPI-owned prefix", []string{"bosh-cpi", "bosh-owner--" + other, "director--" + other}, false},
		{"operator tag with a non-UUID value", []string{"bosh-cpi", "env--prod", "web--7"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if orphan := len(storedInstanceEvidence("", tc.tags).orphans) > 0; orphan != tc.wantOrphan {
				t.Errorf("orphan = %v, want %v", orphan, tc.wantOrphan)
			}
		})
	}
}

func TestOwnerTagFor_Lowercases(t *testing.T) {
	t.Parallel()

	if got, want := ownerTagFor("5E1F0C2A-7D4B-4C8E-9A1F-2B3C4D5E6F70"),
		"bosh-owner--5e1f0c2a-7d4b-4c8e-9a1f-2b3c4d5e6f70"; got != want {
		t.Errorf("ownerTagFor = %q, want %q", got, want)
	}
}

// Every tag writer that rebuilds a VM's list from what PVE returned puts the
// ownership record back at the head before the byte cap truncates the tail.
// PVE may hand the tags back sorted, so long operator tags can come first.
func TestTagWriters_KeepOwnerTagOnALongSortedList(t *testing.T) {
	t.Parallel()

	const owner = "bosh-owner--5e1f0c2a-7d4b-4c8e-9a1f-2b3c4d5e6f70"
	sorted := make([]string, 0, 7)
	for i := range 5 {
		sorted = append(sorted, fmt.Sprintf("a%d--%s", i, strings.Repeat("x", 60)))
	}
	sorted = append(sorted, ownershipTag, owner)
	existing := strings.Join(sorted, ";")

	writers := map[string]func(Deps){
		"delete_vm deleting tag": func(deps Deps) {
			stampDeletingTag(context.Background(), deps, "pve01", "100", 100, log.NewNopLogger())
		},
		"create_vm failed tag": func(deps Deps) {
			tagFailedVM(context.Background(), deps, "pve01", 100, kfEnv(), log.NewNopLogger())
		},
		"set_disk_metadata custom tags": func(deps Deps) {
			if err := applyCustomTagsToVM(context.Background(), deps, "pve01", 100,
				map[string]string{"env": "prod"}, "disk-cid"); err != nil {
				t.Errorf("applyCustomTagsToVM: %v", err)
			}
		},
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			nodes := &kfNodes{}
			write(kfDeps(false, nodes, &kfQEMU{existingTag: existing}))
			if nodes.updatedTags == nil {
				t.Fatal("no tag write")
			}
			got := strings.Split(*nodes.updatedTags, ";")
			if len(got) < 2 || got[0] != ownershipTag || got[1] != owner {
				t.Errorf("tags = %q; want %s then %s at the head", *nodes.updatedTags, ownershipTag, owner)
			}
		})
	}
}

// A managed create builds the VM's shape once per placement candidate, so the
// shape builder itself must not warn about a missing director_uuid. createVM
// says it once for the whole call instead.
func TestBuildVMShapeForNode_DoesNotWarnPerCandidate(t *testing.T) {
	t.Parallel()

	logger, obs := log.NewObservedLogger(log.LevelWarn)
	deps := Deps{
		Config: &config.CPIConfig{Node: "pve", VMStorage: "local-lvm", VMIDRangeStart: 100},
		PVE:    &shapeTestPVEClient{},
		Logger: logger,
	}
	parsed := minimalParsedArgs("test-storage")
	for _, node := range []string{"pve1", "pve2"} {
		shape, err := buildVMShapeForNode(context.Background(), deps, parsed, node)
		if err != nil {
			t.Fatalf("buildVMShapeForNode(%s): %v", node, err)
		}
		if strings.Contains(shape.initialTags, ownerTagPrefix) {
			t.Errorf("initial tags %q carry an owner tag without a director_uuid", shape.initialTags)
		}
	}
	for _, e := range obs.All() {
		if strings.Contains(e.Message, "no director_uuid") {
			t.Errorf("shape builder warned per candidate: %q", e.Message)
		}
	}
}

// An index from the Notes stays comparable with a UUID, and an index that only
// a tag records does not.
func TestStoredInstanceEvidence_TagOnlyIndexes(t *testing.T) {
	t.Parallel()

	ev := storedInstanceEvidence("name: bosh/0\n", []string{"bosh-cpi", "job--bosh", "bosh--0", "bosh--7"})
	if !slices.Equal(ev.values, []string{"0", "7"}) {
		t.Fatalf("values = %v, want [0 7]", ev.values)
	}
	if ev.tagOnly["0"] || !ev.tagOnly["7"] {
		t.Errorf("tagOnly = %v; want only 7, because the Notes record 0", ev.tagOnly)
	}
}
