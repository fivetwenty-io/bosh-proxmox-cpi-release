package handlers

import (
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

func selectorBool(value bool) *bool { return &value }
func selectorInt(value int) *int    { return &value }
func selectorConfig() *config.CPIConfig {
	sets := map[string]config.StorageSet{}
	for _, name := range []string{"e", "e2", "p", "pin", "root"} {
		sets[name] = config.StorageSet{Names: []string{name + "-1"}, Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}
	}
	p := sets["p"]
	p.Names = []string{"p-1", "p-2"}
	sets["p"] = p
	pin := sets["pin"]
	pin.Names = []string{"p-1"}
	sets["pin"] = pin
	return &config.CPIConfig{VMStorage: "legacy-vm", DiskStorage: "legacy-disk", StorageSets: sets,
		StorageTiers: map[string]config.StorageTierCriteria{"tier": {Shared: selectorBool(true)}, "enc": {Encrypted: selectorBool(true)}},
		VMTypes:      map[string]config.TypeProfile{}, DiskTypes: map[string]config.TypeProfile{}}
}

func requireSelectors(t *testing.T, cfg *config.CPIConfig, op string, cp map[string]any, dedicated bool) *StoragePlacementSelection {
	t.Helper()
	r, err := ResolveStoragePlacementSelectors(cfg, op, cp, dedicated)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestStorageSelectorsActivationMatrix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, op, e, p, root                    string
		cp                                      map[string]any
		dedicated, managed, bundle              bool
		future                                  string
		wantRoot, wantEphemeral, wantPersistent string
	}{
		{name: "unused", op: "create_vm", wantRoot: "legacy-vm"},
		{name: "P-only VM", op: "create_vm", p: "p", future: "p", wantRoot: "legacy-vm"},
		{name: "P-only disk", op: "create_disk", p: "p", managed: true, wantPersistent: "p"},
		{name: "E-only disk", op: "create_disk", e: "e", wantPersistent: "legacy-disk"},
		{name: "E root-only", op: "create_vm", e: "e", managed: true, bundle: true, wantRoot: "e"},
		{name: "E dedicated", op: "create_vm", e: "e", dedicated: true, managed: true, bundle: true, wantRoot: "e", wantEphemeral: "e"},
		{name: "base P E", op: "create_vm", e: "e", p: "p", dedicated: true, managed: true, bundle: true, future: "p", wantRoot: "e", wantEphemeral: "e"},
		{name: "separate global root", op: "create_vm", e: "e", root: "root", dedicated: true, managed: true, wantRoot: "root", wantEphemeral: "e"},
		{name: "root-only opt-in mixed companion", op: "create_vm", root: "root", dedicated: true, managed: true, wantRoot: "root", wantEphemeral: "legacy-vm"},
		{name: "resource E", op: "create_vm", cp: map[string]any{"ephemeral_storage_set": "e"}, dedicated: true, managed: true, bundle: true, wantRoot: "e", wantEphemeral: "e"},
		{name: "resource persistent singleton", op: "create_disk", p: "p", cp: map[string]any{"storage_set": "pin"}, managed: true, wantPersistent: "pin"},
		{name: "root scalar split", op: "create_vm", e: "e", cp: map[string]any{"storage_pool": "e-1"}, dedicated: true, managed: true, wantRoot: "e-1", wantEphemeral: "e"},
		{name: "dedicated scalar split", op: "create_vm", e: "e", cp: map[string]any{"ephemeral_storage_pool": "e-1"}, dedicated: true, managed: true, wantRoot: "e", wantEphemeral: "e-1"},
		{name: "unused dedicated override never creates disk", op: "create_vm", cp: map[string]any{"ephemeral_storage_pool": "local-lvm"}, wantRoot: "legacy-vm"},
		{name: "lifecycle ignores allocation policy", op: "attach_disk", e: "e", p: "p"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := selectorConfig()
			cfg.EphemeralStorageSet = tc.e
			cfg.PersistentStorageSet = tc.p
			cfg.RootStorageSet = tc.root
			r := requireSelectors(t, cfg, tc.op, tc.cp, tc.dedicated)
			if r.SetManaged != tc.managed || r.BundleVMDisks != tc.bundle || r.FuturePersistentSet != tc.future {
				t.Fatalf("activation got managed=%v bundle=%v future=%q", r.SetManaged, r.BundleVMDisks, r.FuturePersistentSet)
			}
			for _, entry := range []struct {
				role *StorageRoleSelection
				want string
			}{{r.Root, tc.wantRoot}, {r.Ephemeral, tc.wantEphemeral}, {r.Persistent, tc.wantPersistent}} {
				if entry.want == "" {
					if entry.role != nil {
						t.Fatalf("unexpected created role %+v", entry.role)
					}
					continue
				}
				if entry.role == nil || entry.role.Value != entry.want {
					t.Fatalf("role %+v want %q", entry.role, entry.want)
				}
			}
		})
	}
}

func TestStorageSelectorsAtomicRoleLayers(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"root", "ephemeral", "persistent"} {
		t.Run(role, func(t *testing.T) {
			setKey, poolKeys, _ := storageRoleKeys(role)
			op := "create_vm"
			if role == "persistent" {
				op = "create_disk"
			}
			for _, tc := range []struct {
				name                           string
				call, disk, vm                 map[string]any
				wantKind, wantValue, wantLayer string
			}{
				{"higher set", map[string]any{setKey: "e"}, nil, map[string]any{poolKeys[0]: "legacy"}, "set", "e", "call"},
				{"higher scalar", map[string]any{poolKeys[0]: "nfs-choice"}, map[string]any{setKey: "e"}, nil, "pool", "nfs-choice", "call"},
				{"disk beats VM", nil, map[string]any{setKey: "e"}, map[string]any{poolKeys[0]: "lower"}, "set", "e", "disk_type:d"},
				{"VM set", nil, nil, map[string]any{setKey: "e"}, "set", "e", "vm_type:v"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					cfg := selectorConfig()
					cfg.VMTypes["v"] = config.TypeProfile{CloudProperties: tc.vm}
					cfg.DiskTypes["d"] = config.TypeProfile{CloudProperties: tc.disk}
					call := map[string]any{"vm_type": "v", "disk_type": "d"}
					for k, v := range tc.call {
						call[k] = v
					}
					r := requireSelectors(t, cfg, op, call, true)
					s := r.Root
					if role == "ephemeral" {
						s = r.Ephemeral
					}
					if role == "persistent" {
						s = r.Persistent
					}
					if s.Kind != tc.wantKind || s.Value != tc.wantValue || s.Source.Layer != tc.wantLayer || !s.Explicit {
						t.Fatalf("wrong atomic winner %+v", s)
					}
				})
			}
		})
	}
}

func TestStorageSelectorsBundleUsesWinningChoices(t *testing.T) {
	t.Parallel()
	cfg := selectorConfig()
	cfg.VMTypes["v"] = config.TypeProfile{CloudProperties: map[string]any{"storage_pool": "legacy-lower"}}
	r := requireSelectors(t, cfg, "create_vm", map[string]any{"vm_type": "v", "ephemeral_storage_set": "e"}, true)
	if !r.BundleVMDisks || r.Root.Value != "e" || r.Ephemeral.Value != "e" {
		t.Fatalf("shadowed root scalar split bundle: %+v", r)
	}
	r = requireSelectors(t, cfg, "create_vm", map[string]any{"ephemeral_storage_set": "e", "root_storage_set": "root"}, true)
	if r.BundleVMDisks || r.Root.Value != "root" || r.Ephemeral.Value != "e" {
		t.Fatal("same-layer explicit root did not split bundle")
	}
	r = requireSelectors(t, cfg, "create_vm", map[string]any{"ephemeral_storage_set": "e", "storage_pool": "nfs-root"}, true)
	if r.BundleVMDisks || r.Root.Kind != "pool" {
		t.Fatal("same-layer legacy root override did not split bundle")
	}
	cfg.RootStorageSet = "root"
	r = requireSelectors(t, cfg, "create_vm", map[string]any{"ephemeral_storage_set": "e"}, false)
	if r.Root.Value != "e" || r.Root.BoundaryName != "root" {
		t.Fatal("higher E lost separate global root boundary")
	}
	if err := ValidateStoragePlacementBoundary(*r.Root, []string{"e-1"}, []string{"root-1"}); err == nil {
		t.Fatal("higher E escaped root boundary")
	}
}

func TestStorageSelectorsAtomicScalarDoesNotActivateJournal(t *testing.T) {
	t.Parallel()
	cfg := selectorConfig()
	cfg.DiskTypes["d"] = config.TypeProfile{CloudProperties: map[string]any{"ephemeral_storage_set": "e"}}
	cfg.VMTypes["v"] = config.TypeProfile{CloudProperties: map[string]any{"ephemeral_storage_tier": "tier"}}
	call := map[string]any{"disk_type": "d", "vm_type": "v", "storage_pool": "legacy-root", "ephemeral_storage_pool": "legacy-ephemeral"}
	r := requireSelectors(t, cfg, "create_vm", call, true)
	if r.SetManaged || !r.UsesAtomicSelectors || !r.Root.Atomic || !r.Ephemeral.Atomic || r.Ephemeral.Kind != "pool" || r.Ephemeral.Value != "legacy-ephemeral" {
		t.Fatalf("atomic scalar routing lost: %+v", r)
	}
	if r.BundleVMDisks {
		t.Fatal("explicit winning scalars cannot form a set bundle")
	}
}

func TestStorageSelectorsLegacyPrecedenceAndEncryption(t *testing.T) {
	t.Parallel()
	cfg := selectorConfig()
	cfg.VMTypes["v"] = config.TypeProfile{CloudProperties: map[string]any{"ephemeral_storage_tier": "tier", "storage_pool": "root-low"}}
	call := map[string]any{"vm_type": "v", "ephemeral_storage_pool": "ephemeral-high", "storage_tier": "tier"}
	r := requireSelectors(t, cfg, "create_vm", call, true)
	if r.Root.Kind != "pool" || r.Root.Value != "root-low" || r.Ephemeral.Kind != "tier" || r.Ephemeral.Value != "tier" || r.SetManaged {
		t.Fatalf("legacy key-first precedence changed: %+v %+v", r.Root, r.Ephemeral)
	}
	cfg.EphemeralStorageSet = "e"
	r = requireSelectors(t, cfg, "create_vm", call, true)
	if r.Root.Kind != "tier" || r.Ephemeral.Kind != "pool" {
		t.Fatal("bound roles did not use atomic layer precedence")
	}
	cfg.EphemeralStorageSet = ""
	cfg.Encrypted = selectorBool(true)
	if _, err := ResolveStoragePlacementSelectors(cfg, "create_vm", call, true); err == nil {
		t.Fatal("legacy encryption accepted explicit pool hidden by tier")
	}
	r = requireSelectors(t, cfg, "create_disk", nil, false)
	if r.Persistent.Kind != "tier" || r.Persistent.Value != "enc" {
		t.Fatal("legacy encrypted auto-tier changed")
	}
	r = requireSelectors(t, cfg, "create_disk", map[string]any{"encrypted": false}, false)
	if r.Persistent.Kind != "pool" || r.Persistent.Encrypted {
		t.Fatal("explicit false failed to override global encryption")
	}
}

func TestStorageSelectorsRejectMalformedShadowedPolicies(t *testing.T) {
	t.Parallel()
	for name, cp := range map[string]map[string]any{
		"null":                      {"root_storage_set": nil},
		"blank":                     {"ephemeral_storage_set": " "},
		"wrong type":                {"storage_set": 42},
		"unknown set":               {"root_storage_set": "missing"},
		"competing root":            {"root_storage_set": "e", "storage_pool": "e-1"},
		"competing ephemeral":       {"ephemeral_storage_set": "e", "ephemeral_storage_tier": "tier"},
		"competing persistent":      {"storage_set": "p", "storage": "p-1"},
		"journal redirect":          {"storage_allocation_journal_dir": "/other"},
		"namespace redirect":        {"storage_placement_namespace": "other"},
		"nested namespace redirect": {"pve": map[string]any{"storage_placement_namespace": "other"}},
		"global boundary redirect":  {"persistent_storage_set": "p"},
		"policy typo":               {"storage_setz": "p"},
		"case alias":                {"Storage_Set": "p"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := selectorConfig()
			cfg.VMTypes["v"] = config.TypeProfile{CloudProperties: cp}
			call := map[string]any{"vm_type": "v", "root_storage_set": "e", "ephemeral_storage_set": "e", "storage_set": "p"}
			op := "create_vm"
			if name == "competing persistent" {
				op = "create_disk"
			}
			if _, err := ResolveStoragePlacementSelectors(cfg, op, call, true); err == nil {
				t.Fatalf("accepted malformed shadowed profile %v", cp)
			}
		})
	}
	cfg := selectorConfig()
	cfg.EphemeralStorageSet = "e"
	for _, raw := range []any{nil, "", 2, false} {
		if _, err := ResolveStoragePlacementSelectors(cfg, "create_vm", map[string]any{"storage_pool": raw}, false); err == nil {
			t.Fatalf("bound root accepted malformed scalar %v", raw)
		}
	}
	if _, err := ResolveStoragePlacementSelectors(cfg, "creat_vm", nil, false); err == nil {
		t.Fatal("unknown operation accepted")
	}
	if _, err := ResolveStoragePlacementSelectors(nil, "create_vm", nil, false); err == nil {
		t.Fatal("nil config accepted")
	}
}

func TestStorageSelectorsCapacityAndAssertions(t *testing.T) {
	t.Parallel()
	cfg := selectorConfig()
	cfg.PersistentStorageSet = "p"
	cfg.Encrypted = selectorBool(true)
	p := cfg.StorageSets["p"]
	p.MinFreeMB = 100
	p.MaxUtilizationPct = selectorInt(80)
	p.Encrypted = selectorBool(true)
	cfg.StorageSets["p"] = p
	pin := cfg.StorageSets["pin"]
	pin.MinFreeMB = 10
	pin.MaxUtilizationPct = selectorInt(90)
	pin.Encrypted = selectorBool(true)
	cfg.StorageSets["pin"] = pin
	cfg.Storage = &config.StorageConfig{MaxUtilizationPct: selectorInt(75), MaxUtilizationMode: "warn"}
	r := requireSelectors(t, cfg, "create_disk", map[string]any{"storage_set": "pin"}, false)
	if r.Persistent.ReserveMB != 100 || r.Persistent.CeilingPct == nil || *r.Persistent.CeilingPct != 75 || !r.Persistent.Encrypted || r.Persistent.BoundaryName != "p" {
		t.Fatalf("constraints relaxed: %+v", r.Persistent)
	}
	if err := ValidateStoragePlacementBoundary(*r.Persistent, []string{"p-1"}, []string{"p-1", "p-2"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateStoragePlacementBoundary(*r.Persistent, []string{"p-1", "outside"}, []string{"p-1", "p-2"}); err == nil {
		t.Fatal("subset expansion accepted")
	}
	if err := ValidateStoragePlacementBoundary(*r.Persistent, nil, []string{"p-1"}); err == nil {
		t.Fatal("missing resolved membership accepted")
	}
	// False at the request can disable the requirement without changing assertions.
	r = requireSelectors(t, cfg, "create_disk", map[string]any{"storage_set": "pin", "encrypted": false}, false)
	if r.Persistent.Encrypted {
		t.Fatal("request false lost")
	}
	// Omitted assertion, not contradictory aliases, isolates selected-set enforcement.
	pin.Encrypted = nil
	cfg.StorageSets["pin"] = pin
	if _, err := ResolveStoragePlacementSelectors(cfg, "create_disk", map[string]any{"storage_set": "pin"}, false); err == nil {
		t.Fatal("selected set omitted assertion accepted")
	}
	pin.Encrypted = selectorBool(true)
	cfg.StorageSets["pin"] = pin
	p.Encrypted = nil
	cfg.StorageSets["p"] = p
	if _, err := ResolveStoragePlacementSelectors(cfg, "create_disk", map[string]any{"storage_set": "pin"}, false); err == nil {
		t.Fatal("boundary omitted assertion accepted")
	}
	p.Encrypted = selectorBool(true)
	cfg.StorageSets["p"] = p
	if _, err := ResolveStoragePlacementSelectors(cfg, "create_disk", map[string]any{"storage_pool": "p-1"}, false); err == nil {
		t.Fatal("unverified scalar accepted with encryption")
	}
	r = requireSelectors(t, cfg, "create_disk", map[string]any{"storage_tier": "enc"}, false)
	if r.Persistent.BoundaryName != "p" || r.Persistent.Kind != "tier" {
		t.Fatal("encrypted tier lost boundary")
	}
	r = requireSelectors(t, cfg, "create_disk", map[string]any{"storage_pool": "outside", "encrypted": false}, false)
	if err := ValidateStoragePlacementBoundary(*r.Persistent, []string{"p-1"}, []string{"p-1", "p-2"}); err == nil {
		t.Fatal("caller-supplied subset hid out-of-bound scalar")
	}
	if !strings.Contains(r.Persistent.Source.Property, "storage_pool") {
		t.Fatal("lost winning property source")
	}
}

func TestStorageSelectorsSnapshotsAreIsolated(t *testing.T) {
	t.Parallel()
	cfg := selectorConfig()
	cfg.EphemeralStorageSet = "e"
	cfg.Encrypted = selectorBool(false)
	cfg.Storage = &config.StorageConfig{MaxUtilizationPct: selectorInt(85)}
	cfg.VMTypes["v"] = config.TypeProfile{CloudProperties: map[string]any{"ephemeral_storage_set": "e", "custom": map[string]any{"items": []any{"keep"}}}}
	call := map[string]any{"vm_type": "v", "custom": map[string]any{"items": []any{"keep"}}}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := ResolveStoragePlacementSelectors(cfg, "create_vm", call, true)
			if err != nil {
				t.Error(err)
				return
			}
			r.Root.Set.Names[0] = "changed"
			*r.Policy.Encrypted = true
			*r.Policy.Storage.MaxUtilizationPct = 1
			r.Policy.VMTypes["v"].CloudProperties["custom"].(map[string]any)["items"].([]any)[0] = "changed"
			r.CloudProperties["custom"].(map[string]any)["items"].([]any)[0] = "changed"
		}()
	}
	wg.Wait()
	if cfg.StorageSets["e"].Names[0] != "e-1" || *cfg.Encrypted || *cfg.Storage.MaxUtilizationPct != 85 {
		t.Fatal("policy mutation leaked")
	}
	if call["custom"].(map[string]any)["items"].([]any)[0] != "keep" || cfg.VMTypes["v"].CloudProperties["custom"].(map[string]any)["items"].([]any)[0] != "keep" {
		t.Fatal("nested map mutation leaked")
	}
}
