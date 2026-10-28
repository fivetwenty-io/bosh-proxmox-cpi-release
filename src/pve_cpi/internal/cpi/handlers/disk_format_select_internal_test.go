package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

const gib = uint64(1 << 30)

// formatWarnings returns the warning messages that mention a disk format.
func formatWarnings(obs *log.Observer) []string {
	var out []string
	for _, e := range obs.All() {
		if strings.Contains(e.Message, "disk format") {
			out = append(out, e.Message)
		}
	}
	return out
}

// An empty or "auto" disk_format or vm_disk_format counts as unset in every
// cloud-property layer: the call, a disk_type profile, and a vm_type profile.
func TestLayeredResolverDiskFormat_EmptyOrAutoIsUnsetAtEveryLayer(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		DiskTypes: map[string]config.TypeProfile{
			"dt-raw":  {CloudProperties: map[string]any{"disk_format": "raw"}},
			"dt-auto": {CloudProperties: map[string]any{"disk_format": "AUTO", "vm_disk_format": ""}},
		},
		VMTypes: map[string]config.TypeProfile{
			"vt-vmdk": {CloudProperties: map[string]any{"vm_disk_format": "vmdk"}},
			"vt-auto": {CloudProperties: map[string]any{"vm_disk_format": "auto", "disk_format": " "}},
		},
	}
	cases := []struct {
		name   string
		call   map[string]any
		want   string
		wantOK bool
	}{
		{"call auto falls to disk_type", map[string]any{"disk_format": "auto", "disk_type": "dt-raw"}, "raw", true},
		{"call empty falls to disk_type", map[string]any{"vm_disk_format": "", "disk_type": "dt-raw"}, "raw", true},
		{"disk_type auto falls to vm_type", map[string]any{"disk_type": "dt-auto", "vm_type": "vt-vmdk"}, "vmdk", true},
		{"vm_type auto is unset", map[string]any{"vm_type": "vt-auto"}, "", false},
		{"every layer auto is unset", map[string]any{"disk_format": "Auto", "disk_type": "dt-auto", "vm_type": "vt-auto"}, "", false},
		{"auto under the first key yields to the second key", map[string]any{"vm_disk_format": "auto", "disk_format": "raw"}, "raw", true},
		{"explicit call value wins and is trimmed", map[string]any{"vm_disk_format": " qcow2 ", "disk_type": "dt-raw"}, "qcow2", true},
		{"non-string value is skipped", map[string]any{"vm_disk_format": 7, "vm_type": "vt-vmdk"}, "vmdk", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := newLayeredResolver(tc.call, cfg)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := r.DiskFormat("vm_disk_format", "disk_format")
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("DiskFormat = (%q, %v); want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// The job-level value (and through it the pve_vm_disk_format override) is
// the last rung, and an empty or "auto" value there is unset too.
func TestRequestedDiskFormat_JobLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		call      map[string]any
		job       string
		want      string
		wantLayer bool
	}{
		{"nothing set", nil, "", "", false},
		{"job auto is unset", nil, "auto", "", false},
		{"job raw", nil, "raw", "raw", false},
		{"call auto falls to job raw", map[string]any{"disk_format": "auto"}, "raw", "raw", false},
		{"call qcow2 beats job raw", map[string]any{"disk_format": "qcow2"}, "raw", "qcow2", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.CPIConfig{VMDiskFormat: tc.job}
			r, err := newLayeredResolver(tc.call, cfg)
			if err != nil {
				t.Fatal(err)
			}
			got, layer := requestedDiskFormat(r, cfg, "disk_format")
			if got != tc.want || layer != tc.wantLayer {
				t.Errorf("requestedDiskFormat = (%q, %v); want (%q, %v)", got, layer, tc.want, tc.wantLayer)
			}
		})
	}
	r, _ := newLayeredResolver(nil, &config.CPIConfig{})
	if got, _ := requestedDiskFormat(r, nil, "disk_format"); got != "" {
		t.Errorf("nil config: requestedDiskFormat = %q; want empty", got)
	}
}

// create_vm's ladder (call and profile layers, then the struct field, then the
// job value) skips empty and "auto" values at every rung.
func TestResolveVMShapeStorage_DiskFormat_AutoAtEveryRungFallsThrough(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{
		VMStorage:    "nfs-pool",
		VMDiskFormat: "raw",
		VMTypes: map[string]config.TypeProfile{
			"auto": {CloudProperties: map[string]any{"vm_disk_format": "auto"}},
		},
	}
	parsed := minimalParsedArgsWithCP("stemcell-store", map[string]any{"vm_disk_format": "", "disk_format": "auto", "vm_type": "auto"})
	parsed.cloudProps.VMDiskFormat = "auto"
	_, got, _, err := resolveVMShapeStorage(cfg, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if got != "raw" {
		t.Errorf("requested format = %q; want raw from the job value", got)
	}

	cfg.VMDiskFormat = "AUTO"
	_, got, _, err = resolveVMShapeStorage(cfg, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("requested format = %q; want empty, so the storage type decides", got)
	}
}

func shapeDeps(vmStorage, jobFormat string, entries []map[string]any, listErr error) (Deps, *log.Observer) {
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	return Deps{
		Config: &config.CPIConfig{Node: "pve", VMStorage: vmStorage, VMDiskFormat: jobFormat, VMIDRangeStart: 100},
		PVE:    &shapeTestPVEClient{clusterStorageSvc: &shapeTestClusterStorage{entries: entries, err: listErr}},
		Logger: logger,
	}, obs
}

// The root disk's format follows the type of the pool it lands on.
func TestBuildVMShape_RootFormatFromStorageType(t *testing.T) {
	t.Parallel()
	entries := []map[string]any{
		{"storage": "local-lvm", "type": "lvmthin"},
		{"storage": "thick", "type": "lvm"},
		{"storage": "nfs-store", "type": "nfs"},
		{"storage": "odd", "type": "esxi"},
	}
	cases := []struct {
		name, vmStorage, job string
		call                 map[string]any
		listErr              error
		want                 string
		wantWarn             string
	}{
		{"unset on lvmthin is raw", "local-lvm", "", nil, nil, "raw", ""},
		{"auto on lvmthin is raw", "local-lvm", "auto", map[string]any{"vm_disk_format": "auto"}, nil, "raw", ""},
		{"unset on thick lvm is raw", "thick", "", nil, nil, "raw", ""},
		{"unset on nfs is qcow2", "nfs-store", "", nil, nil, "qcow2", ""},
		{"job raw on nfs is raw", "nfs-store", "raw", nil, nil, "raw", ""},
		{"explicit qcow2 on lvmthin coerces and warns", "local-lvm", "", map[string]any{"vm_disk_format": "qcow2"}, nil, "raw", "cannot be stored on block storage"},
		{"job qcow2 on lvmthin coerces and warns", "local-lvm", "qcow2", nil, nil, "raw", "cannot be stored on block storage"},
		{"explicit qcow2 on thick lvm passes", "thick", "", map[string]any{"vm_disk_format": "qcow2"}, nil, "qcow2", ""},
		{"unknown type keeps qcow2 and warns", "odd", "", nil, nil, "qcow2", "falls back to qcow2"},
		{"failed lookup keeps qcow2 and warns", "local-lvm", "", nil, fmt.Errorf("PVE /storage: connection refused"), "qcow2", "falls back to qcow2"},
		{"failed lookup keeps an explicit raw", "local-lvm", "raw", nil, fmt.Errorf("PVE /storage: connection refused"), "raw", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, obs := shapeDeps(tc.vmStorage, tc.job, entries, tc.listErr)
			call := tc.call
			if call == nil {
				call = map[string]any{}
			}
			shape, err := resolveVMShape(context.Background(), deps, minimalParsedArgsWithCP("stemcell-store", call))
			if err != nil {
				t.Fatal(err)
			}
			if shape.vmDiskFormat != tc.want {
				t.Errorf("vmDiskFormat = %q; want %q", shape.vmDiskFormat, tc.want)
			}
			warnings := formatWarnings(obs)
			switch {
			case tc.wantWarn == "" && len(warnings) != 0:
				t.Errorf("unexpected format warnings: %v", warnings)
			case tc.wantWarn != "" && (len(warnings) != 1 || !strings.Contains(warnings[0], tc.wantWarn)):
				t.Errorf("format warnings = %v; want one containing %q", warnings, tc.wantWarn)
			}
		})
	}
}

// A storage tier can place the root disk on a pool whose type differs from
// vm_storage's, and the format follows the chosen pool in both directions.
func TestBuildVMShape_TierPlacementUsesChosenPoolType(t *testing.T) {
	t.Parallel()
	entries := []map[string]any{
		{"storage": "local-lvm", "type": "lvmthin"},
		{"storage": "nfs-store", "type": "nfs"},
	}
	cases := []struct{ vmStorage, tierPool, want string }{
		{"nfs-store", "local-lvm", "raw"},
		{"local-lvm", "nfs-store", "qcow2"},
	}
	for _, tc := range cases {
		t.Run(tc.tierPool, func(t *testing.T) {
			t.Parallel()
			deps, _ := shapeDeps(tc.vmStorage, "", entries, nil)
			parsed := minimalParsedArgsWithCP("stemcell-store", map[string]any{"storage_tier": "fast"})
			tier := func(string) (string, error) { return tc.tierPool, nil }
			shape, err := buildVMShapeForNode(context.Background(), deps, parsed, "pve", tier)
			if err != nil {
				t.Fatal(err)
			}
			if shape.vmStorage != tc.tierPool || shape.vmDiskFormat != tc.want {
				t.Errorf("shape = (%q, %q); want (%q, %q)", shape.vmStorage, shape.vmDiskFormat, tc.tierPool, tc.want)
			}
		})
	}
}

func managedFormatPlan(rootID, rootType, ephID, ephType string) *StorageAllocationPlan {
	plan := &StorageAllocationPlan{
		Node:        "pve",
		Targets:     []StoragePlanTarget{{Role: "root", Node: "pve", StorageID: rootID, VirtualBytes: 10 * gib}},
		Definitions: map[string]pve.StorageInfo{rootID: {Name: rootID, Type: rootType}},
	}
	if ephID != "" {
		plan.Targets = append(plan.Targets, StoragePlanTarget{Role: storageRoleEphemeral, Node: "pve", StorageID: ephID, VirtualBytes: 20 * gib})
		plan.Definitions[ephID] = pve.StorageInfo{Name: ephID, Type: ephType}
	}
	return plan
}

// A frozen managed plan (storage-set or tier placement) resolves the root
// format against the plan's own definitions, not vm_storage. The plan freezes
// one DiskFormat, and every release derives the ephemeral disk's format from
// it and the ephemeral pool's type, so the fresh shape and the shape resumed
// from the frozen plan must allocate the same ephemeral format.
func TestBuildVMShape_ManagedPlanResolvesEachRolePerPool(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                 string
		plan                 *StorageAllocationPlan
		job                  string
		call                 map[string]any
		wantFrozen, wantEph  string
		wantEphemeralWarning bool
	}{
		{"nfs set root with lvmthin vm_storage", managedFormatPlan("set-a", "nfs", "", ""), "", nil, "qcow2", "", false},
		{"lvmthin root without ephemeral is raw", managedFormatPlan("local-lvm", "lvmthin", "", ""), "", nil, "raw", "", false},
		{"job qcow2 coerced on lvmthin root without ephemeral", managedFormatPlan("local-lvm", "lvmthin", "", ""), "qcow2", nil, "raw", "", false},
		{"lvmthin root and nfs ephemeral", managedFormatPlan("local-lvm", "lvmthin", "set-a", "nfs"), "", nil, "qcow2", "qcow2", false},
		{"lvmthin root and dir ephemeral", managedFormatPlan("local-lvm", "lvmthin", "set-a", "dir"), "", nil, "qcow2", "qcow2", false},
		{"nfs root and lvmthin ephemeral", managedFormatPlan("set-a", "nfs", "local-lvm", "lvmthin"), "", nil, "qcow2", "raw", false},
		{"job qcow2 on lvmthin root keeps nfs ephemeral qcow2", managedFormatPlan("local-lvm", "lvmthin", "set-a", "nfs"), "qcow2", nil, "qcow2", "qcow2", false},
		{"job raw everywhere", managedFormatPlan("local-lvm", "lvmthin", "set-a", "nfs"), "raw", nil, "raw", "raw", false},
		{"job qcow2 on lvmthin root and lvmthin ephemeral", managedFormatPlan("local-lvm", "lvmthin", "eph-lvm", "lvmthin"), "qcow2", nil, "raw", "raw", true},
		{"zfspool ephemeral is raw", managedFormatPlan("set-a", "nfs", "tank", "zfspool"), "", nil, "qcow2", "raw", false},
		{"thick lvm root keeps its raw for an nfs ephemeral", managedFormatPlan("thick", "lvm", "set-a", "nfs"), "", nil, "raw", "raw", false},
		{"thick lvm root keeps an explicit qcow2", managedFormatPlan("thick", "lvm", "", ""), "", map[string]any{"vm_disk_format": "qcow2"}, "qcow2", "", false},
		{"thick lvm root qcow2 with nfs ephemeral", managedFormatPlan("thick", "lvm", "set-a", "nfs"), "", map[string]any{"vm_disk_format": "qcow2"}, "qcow2", "qcow2", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, obs := shapeDeps("local-lvm", tc.job, nil, nil)
			call := tc.call
			if call == nil {
				call = map[string]any{}
			}
			parsed := minimalParsedArgsWithCP("stemcell-store", call)
			parsed.storagePlan = tc.plan
			shape, err := buildVMShapeForNode(context.Background(), deps, parsed, "pve")
			if err != nil {
				t.Fatal(err)
			}
			execution, err := freezeManagedVMExecution(deps.Config, shape)
			if err != nil {
				t.Fatal(err)
			}
			if execution.DiskFormat != tc.wantFrozen {
				t.Errorf("frozen DiskFormat = %q; want %q", execution.DiskFormat, tc.wantFrozen)
			}
			resumed, err := execution.shape(deps.Config)
			if err != nil {
				t.Fatal(err)
			}
			if resumed.vmDiskFormat != shape.vmDiskFormat {
				t.Errorf("resumed format = %q; fresh format = %q", resumed.vmDiskFormat, shape.vmDiskFormat)
			}
			if eph, ok := managedVMRoleTarget(tc.plan, storageRoleEphemeral); ok {
				ephType := tc.plan.Definitions[eph.StorageID].Type
				got, err := pve.EphemeralVolumeFormat(ephType, resumed.vmDiskFormat)
				if err != nil || got != tc.wantEph {
					t.Errorf("resumed ephemeral allocation format = (%q, %v); want %q", got, err, tc.wantEph)
				}
			}
			ephWarnings := 0
			for _, w := range formatWarnings(obs) {
				if strings.Contains(w, "ephemeral") {
					ephWarnings++
				}
			}
			if (ephWarnings == 1) != tc.wantEphemeralWarning || ephWarnings > 1 {
				t.Errorf("ephemeral format warnings = %v; want warning=%v", formatWarnings(obs), tc.wantEphemeralWarning)
			}
		})
	}
}

// legacyStorageVMExecution is StorageVMExecution as 0.9.5 defined it. An
// older CPI decodes frozen plans strictly, so every plan this release writes
// must decode into it with unknown fields refused.
type legacyStorageVMExecution struct {
	Version           int
	InputFingerprint  string
	Node              string
	Storage           string
	StorageType       string
	DiskFormat        string
	RootGiB           int
	MaxAttempts       int
	Cores             int
	Sockets           int
	MemoryMiB         int
	Hotplug           string
	NUMA              bool
	Tags              string
	InitialName       string
	RootOptions       map[string]string
	RootDevice        string
	SCSIController    string
	EphemeralGiB      int
	EphemeralStorage  string
	CPUType           string
	BalloonMiB        *int
	Pool              string
	PoolComment       string
	PoolLayer         string
	PoolDirector      string
	PoolDeployment    string
	PoolInstanceGroup string
}

// Every frozen VM execution this release writes keeps exactly the 0.9.5 keys
// and decodes into the 0.9.5 struct with unknown fields refused, so an older
// CPI can read, verify, and clean up the plans and journal records it finds.
func TestFreezeManagedVMExecution_ReadableByOlderRelease(t *testing.T) {
	t.Parallel()
	want := reflectFieldNames(legacyStorageVMExecution{})
	sort.Strings(want)
	plans := []*StorageAllocationPlan{
		managedFormatPlan("local-lvm", "lvmthin", "set-a", "nfs"),
		managedFormatPlan("set-a", "nfs", "local-lvm", "lvmthin"),
		managedFormatPlan("thick", "lvm", "", ""),
	}
	for _, job := range []string{"", "qcow2", "raw"} {
		for _, plan := range plans {
			deps, _ := shapeDeps("local-lvm", job, nil, nil)
			parsed := minimalParsedArgsWithCP("stemcell-store", map[string]any{})
			parsed.storagePlan = plan
			shape, err := buildVMShapeForNode(context.Background(), deps, parsed, "pve")
			if err != nil {
				t.Fatal(err)
			}
			execution, err := freezeManagedVMExecution(deps.Config, shape)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(execution)
			if err != nil {
				t.Fatal(err)
			}
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(raw, &keys); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(keys))
			for k := range keys {
				got = append(got, k)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("job %q: frozen keys = %v; want the 0.9.5 keys %v", job, got, want)
			}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			var legacy legacyStorageVMExecution
			if err := dec.Decode(&legacy); err != nil {
				t.Errorf("job %q: the 0.9.5 strict decoder refused the plan: %v", job, err)
			}
			if legacy.DiskFormat != execution.DiskFormat {
				t.Errorf("job %q: legacy DiskFormat = %q; want %q", job, legacy.DiskFormat, execution.DiskFormat)
			}
		}
	}
}

func reflectFieldNames(v any) []string {
	typ := reflect.TypeOf(v)
	out := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		out = append(out, typ.Field(i).Name)
	}
	return out
}

func TestManagedPlanDiskFormat(t *testing.T) {
	t.Parallel()
	cases := []struct{ rootType, rootFormat, ephType, requested, want string }{
		{"nfs", "qcow2", "lvmthin", "", "qcow2"},
		{"lvmthin", "raw", "nfs", "", "qcow2"},
		{"lvmthin", "raw", "dir", "qcow2", "qcow2"},
		{"lvmthin", "raw", "nfs", "raw", "raw"},
		{"lvmthin", "raw", "nfs", "vmdk", "vmdk"},
		{"lvmthin", "raw", "lvmthin", "qcow2", "raw"},
		{"rbd", "raw", "nfs", "", "qcow2"},
		{"lvm", "raw", "nfs", "", "raw"},
		{"lvm", "qcow2", "lvmthin", "qcow2", "qcow2"},
		{"lvmthin", "raw", "iscsi", "", "raw"},
		{"lvmthin", "raw", "", "", "raw"},
	}
	for _, tc := range cases {
		if got := managedPlanDiskFormat(tc.rootType, tc.rootFormat, tc.ephType, tc.requested); got != tc.want {
			t.Errorf("managedPlanDiskFormat(%q, %q, %q, %q) = %q; want %q", tc.rootType, tc.rootFormat, tc.ephType, tc.requested, got, tc.want)
		}
	}
}

// A plan frozen before the upgrade fingerprinted the qcow2 that ApplyDefaults
// used to store for an unset vm_disk_format, so an unset or "auto" value must
// fingerprint the same way, while an explicit raw still differs.
func TestManagedVMExecutionFingerprint_UnsetDiskFormatMatchesLegacyQCOW2(t *testing.T) {
	t.Parallel()
	fp := func(format string) string {
		t.Helper()
		got, err := managedVMExecutionFingerprint(&config.CPIConfig{Node: "pve", VMDiskFormat: format})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	legacy := fp("qcow2")
	for _, v := range []string{"", "auto"} {
		if fp(v) != legacy {
			t.Errorf("fingerprint for vm_disk_format=%q differs from the legacy qcow2 fingerprint", v)
		}
	}
	if fp("raw") == legacy {
		t.Error("fingerprint for vm_disk_format=raw matches qcow2")
	}
}

func TestFinalizeCreateDiskFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, storageType, requested string
		layer                        bool
		wantArg, wantExt, wantFormat string
		wantCoerced, wantFallback    bool
	}{
		{"unset on nfs sends qcow2 with extension", "nfs", "", false, "qcow2", ".qcow2", "qcow2", false, false},
		{"job raw on dir", "dir", "raw", false, "raw", ".raw", "raw", false, false},
		{"layer vmdk on cifs", "cifs", "vmdk", true, "vmdk", ".vmdk", "vmdk", false, false},
		{"unset on lvmthin sends nothing", "lvmthin", "", false, "", "", "raw", false, false},
		{"job qcow2 on lvmthin sends nothing and warns", "lvmthin", "qcow2", false, "", "", "raw", true, false},
		{"job qcow2 on thick lvm sends nothing and warns", "lvm", "qcow2", false, "", "", "raw", true, false},
		{"layer qcow2 on lvmthin sends raw and warns", "lvmthin", "qcow2", true, "raw", "", "raw", true, false},
		{"layer qcow2 on thick lvm sends raw and warns", "lvm", "qcow2", true, "raw", "", "raw", true, false},
		{"layer QCOW2 on nfs is lower-cased", "nfs", "QCOW2", true, "qcow2", ".qcow2", "qcow2", false, false},
		{"layer raw on rbd", "rbd", "raw", true, "raw", "", "raw", false, false},
		{"unset on unknown sends nothing", "", "", false, "", "", "qcow2", false, true},
		{"layer raw on unknown is sent", "", "raw", true, "raw", "", "raw", false, false},
		{"job raw on unknown is not sent", "", "raw", false, "", "", "raw", false, false},
		{"cephfs keeps a bare name", "cephfs", "", false, "", "", "qcow2", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			arg, ext, res := finalizeCreateDiskFormat(tc.storageType, tc.requested, tc.layer)
			if arg != tc.wantArg || ext != tc.wantExt || res.Format != tc.wantFormat || res.Coerced != tc.wantCoerced || res.Fallback != tc.wantFallback {
				t.Errorf("finalizeCreateDiskFormat = (%q, %q, %+v); want (%q, %q, format=%q coerced=%v fallback=%v)",
					arg, ext, res, tc.wantArg, tc.wantExt, tc.wantFormat, tc.wantCoerced, tc.wantFallback)
			}
		})
	}
}

// A managed persistent disk's request is the first explicit format, or ""
// when the pool's type should decide, and an empty or "auto" value at any
// layer counts as unset.
func TestManagedDiskFormat_EmptyOrAutoIsUnset(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		call    map[string]any
		job     string
		want    string
		wantErr bool
	}{
		{"nothing set is unset", nil, "", "", false},
		{"call auto and job auto is unset", map[string]any{"disk_format": "auto"}, "auto", "", false},
		{"call QCOW2 is lower-cased", map[string]any{"disk_format": "QCOW2"}, "", "qcow2", false},
		{"call empty falls to job raw", map[string]any{"disk_format": ""}, "raw", "raw", false},
		{"disk_type auto falls to job vmdk", map[string]any{"disk_type": "auto"}, "vmdk", "vmdk", false},
		{"call raw wins", map[string]any{"disk_format": "raw"}, "qcow2", "raw", false},
		{"unsupported value fails", map[string]any{"disk_format": "qed"}, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.CPIConfig{VMDiskFormat: tc.job, DiskTypes: map[string]config.TypeProfile{
				"auto": {CloudProperties: map[string]any{"disk_format": "AUTO"}},
			}}
			r, err := newLayeredResolver(tc.call, cfg)
			if err != nil {
				t.Fatal(err)
			}
			got, err := managedDiskFormat(Deps{Config: cfg}, r)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("managedDiskFormat = (%q, %v); want (%q, error=%v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// The unmanaged ephemeral disk resolves its own pool's type: an unset
// preference gives qcow2 on file storage and raw on block storage, and an
// explicit qcow2 on block storage becomes raw with a warning.
func TestAttachEphemeralDisk_FormatFromEphemeralPoolType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, backend, preference string
		block, wantWarn           bool
	}{
		{"unset on nfs", "nfs", "", false, false},
		{"unset on lvmthin", "lvmthin", "", true, false},
		{"explicit qcow2 on lvmthin", "lvmthin", "qcow2", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := "e:123/vm-123-ephemeral-0.qcow2"
			if tc.block {
				want = "e:vm-123-ephemeral-0"
			}
			store := &namedEphemeralStorage{block: tc.block}
			guest := &ephemeralQEMU{configFn: func() (map[string]any, error) { return map[string]any{}, nil }, attachDiskFn: func(volume, _ string, _ *qemu.AttachOpts) (string, error) {
				if volume != want {
					t.Errorf("attached %q; want %q", volume, want)
				}
				return "scsi1", nil
			}}
			logger, obs := log.NewObservedLogger(log.LevelWarn)
			deps := Deps{PVE: &namedEphemeralClient{ephemeralClient: &ephemeralClient{qemu: guest, storage: store}, backend: tc.backend}, Logger: logger}
			shape := &createVMShape{node: "n1", vmStorage: "root", vmStorageType: "lvmthin", vmDiskFormat: "raw",
				ephemeralStorage: "e", ephemeralDiskGiB: 1, ephemeralDiskFormat: tc.preference}
			if _, err := attachEphemeralDisk(t.Context(), deps, logger, shape, 123); err != nil {
				t.Fatal(err)
			}
			if got := len(formatWarnings(obs)); (got == 1) != tc.wantWarn || got > 1 {
				t.Errorf("format warnings = %v; want warning=%v", formatWarnings(obs), tc.wantWarn)
			}
		})
	}
}
