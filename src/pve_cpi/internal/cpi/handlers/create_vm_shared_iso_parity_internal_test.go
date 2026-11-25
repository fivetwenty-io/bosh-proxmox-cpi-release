package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
)

// The storages of the shared-ISO parity fixture. The disks go on a shared NFS
// export or on a node-local ZFS pool, which the replicate policy can copy,
// and the config-drive ISO goes on a node-local directory or a shared export.
const (
	parityDiskShared = "disk-nfs"
	parityDiskLocal  = "disk-zfs"
	parityISOLocal   = "iso-dir"
	parityISOShared  = "iso-nfs"
)

// isoParityRow is one combination of the inputs that decide whether a
// node-local config-drive ISO fails create_vm.
type isoParityRow struct {
	mode        string
	features    bool   // pve.ha.dlb.enabled gives the VM an HA registration feature
	policy      string // pve.ha.local_policy
	topology    string // pve.ha.storage_topology
	haOff       bool   // the ha cloud property is false
	setting     bool   // pve.require_shared_iso_for_ha
	sharedDisks bool   // the VM's disks are on shared storage
}

func (r isoParityRow) String() string {
	return fmt.Sprintf("mode=%s topology=%s policy=%s features=%v ha_false=%v setting=%v shared_disks=%v",
		r.mode, r.topology, r.policy, r.features, r.haOff, r.setting, r.sharedDisks)
}

func (r isoParityRow) disk() string {
	if r.sharedDisks {
		return parityDiskShared
	}
	return parityDiskLocal
}

func parityISO(shared bool) string {
	if shared {
		return parityISOShared
	}
	return parityISOLocal
}

// isoParityRows lists every combination of the row's inputs.
func isoParityRows() []isoParityRow {
	rows := make([]isoParityRow, 0, 3*3*3*16)
	for _, mode := range []string{config.HAModeOff, config.HAModeAuto, config.HAModeManaged} {
		for _, topology := range []string{
			config.HAStorageTopologyAuto, config.HAStorageTopologyShared, config.HAStorageTopologyLocal,
		} {
			for _, policy := range []string{
				config.HALocalPolicyNone, config.HALocalPolicyHold, config.HALocalPolicyReplicate,
			} {
				rows = append(rows, isoParityFlagRows(mode, topology, policy)...)
			}
		}
	}
	return rows
}

// isoParityFlagRows lists every combination of the row's boolean inputs under
// one mode, topology, and local policy.
func isoParityFlagRows(mode, topology, policy string) []isoParityRow {
	var rows []isoParityRow
	for n := range 16 {
		r := isoParityRow{
			mode: mode, topology: topology, policy: policy,
			features: n&1 != 0, haOff: n&2 != 0, setting: n&4 != 0, sharedDisks: n&8 != 0,
		}
		// A VM with an HA registration feature needs its disks on storage
		// that every HA node reaches, so a storage plan refuses a
		// node-local disk for that reason alone, before the ISO matters.
		if r.features && !r.sharedDisks && r.mode != config.HAModeOff && !r.haOff {
			continue
		}
		rows = append(rows, r)
	}
	return rows
}

// applyISOParityRow sets the row's HA keys and the setting on cfg, with the
// ISO on iso and the root disk on the row's disk storage.
func applyISOParityRow(cfg *config.CPIConfig, r isoParityRow, iso string) {
	cfg.HA = &config.HAConfig{Mode: haChoiceOf(r.mode), LocalPolicy: haChoiceOf(r.policy), StorageTopology: haChoiceOf(r.topology)}
	if r.features {
		enabled := true
		cfg.HA.DLB = &config.HADLBConfig{Enabled: &enabled}
	}
	setting := r.setting
	cfg.RequireSharedISOForHA = &setting
	cfg.ISOStorage = iso
	cfg.VMStorage = r.disk()
}

// isoParityParsed returns the create's parsed arguments for the row, with the
// ha cloud property resolved the way create_vm resolves it.
func isoParityParsed(t *testing.T, cfg *config.CPIConfig, r isoParityRow) *createVMParsedArgs {
	t.Helper()
	parsed := &createVMParsedArgs{cloudProps: createVMCloudProps{Disk: 1024}, cloudPropsMap: map[string]any{}}
	if r.haOff {
		parsed.cloudPropsMap[haCloudProp] = false
	}
	if err := resolveCreateVMHAOptOut(parsed, cfg); err != nil {
		t.Fatalf("%s: resolve ha: %v", r, err)
	}
	return parsed
}

// parityStorages are the storage definitions /storage reports to the plain
// create path, and the ones the HA profile carries.
func parityStorages() []pve.StorageInfo {
	return []pve.StorageInfo{
		{Name: parityDiskShared, Type: pve.StorageTypeNFS, Shared: true, Content: "images"},
		{Name: parityDiskLocal, Type: pve.StorageTypeZFSPool, Content: "images,rootdir"},
		{Name: parityISOLocal, Type: pve.StorageTypeDir, Content: "iso"},
		{Name: parityISOShared, Type: pve.StorageTypeNFS, Shared: true, Content: "iso"},
	}
}

// parityStorageStub lists parityStorages for the plain path's ISO check.
type parityStorageStub struct{ panicStorageStub }

func (parityStorageStub) ListStorage(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	var resp clusterstorage.ListStorageResponse
	storages := parityStorages()
	for i := range storages {
		s := &storages[i]
		shared := 0
		if s.IsShared() {
			shared = 1
		}
		raw, err := json.Marshal(map[string]any{"storage": s.Name, "type": s.Type, "shared": shared, "content": s.Content})
		if err != nil {
			return nil, err
		}
		resp = append(resp, raw)
	}
	return &resp, nil
}

// parityProfile is a healthy three-node HA profile over parityStorages.
func parityProfile(mode string) *ha.Profile {
	p := &ha.Profile{
		Mode: mode, Effective: ha.EffectiveFull, MinVersion: "9.2.4", HAVersionOK: true, DLBSupported: true,
		RulesSupported: true, SysConsole: true, VMReplicate: true, Storages: parityStorages(),
	}
	for _, n := range []string{"pve01", "pve02", "pve03"} {
		p.Nodes = append(p.Nodes, ha.NodeFacts{Name: n, Online: true, Version: "9.2.4"})
	}
	return p
}

// plainPathFails runs the two checks of the plain create path that read the
// config-drive ISO: checkISOStorageForHA with the features
// haRegistrationFeatures finds, then, when decideCreatedVMHA makes one, the
// HA decision over the VM's root disk and its ISO. It reports whether either
// fails the create.
func plainPathFails(t *testing.T, r isoParityRow, isoShared bool) bool {
	t.Helper()
	cfg := isoHABaseConfig(parityISO(isoShared))
	applyISOParityRow(cfg, r, parityISO(isoShared))
	parsed := isoParityParsed(t, cfg, r)
	deps := dlbDeps(nil, nil, parityStorageStub{panicStorageStub{t}}, cfg)
	deps.RequestDirectorUUID = aaDirector
	features := haRegistrationFeatures(deps, parsed.cloudProps, "pve01", parsed.env)
	if err := checkISOStorageForHA(context.Background(), deps, 100, features, log.NewNopLogger()); err != nil {
		return true
	}
	if cfg.HAModeValue() == config.HAModeOff || parsed.cloudProps.haOff {
		return false
	}
	d, err := parityProfile(r.mode).DecideVM(ha.VMInput{
		Config: cfg, VMID: 100, Node: "pve01", CPUType: "x86-64-v3",
		Volumes: []ha.VMVolume{
			{Volume: r.disk() + ":vm-100-disk-0"},
			{Volume: parityISO(isoShared) + ":iso/vm-100-config.iso", Kind: ha.VolumeISO},
		},
	})
	if err != nil {
		t.Fatalf("%s: DecideVM: %v", r, err)
	}
	return d.Outcome == ha.OutcomeFail
}

// isoParityFixture is a storage plan request over the parity storages, with
// the root disk on the row's disk storage and the ISO on iso. The request
// takes its shared-ISO rule and its HA nodes from the create's own code, so
// the test reads the plan the create would make.
func isoParityFixture(t *testing.T, r isoParityRow, iso string) (StoragePlanRequest, *inv.Collector, Deps, *createVMParsedArgs) {
	t.Helper()
	var cfg *config.CPIConfig
	request, collector, _ := planFixture(t, func(source *planFixtureSource, c *config.CPIConfig) {
		c.EphemeralStorageSet = ""
		applyISOParityRow(c, r, iso)
		// Only a storage that a capacity domain names gets a status pair in
		// the inventory, and a domain does not ask for NFS, so each storage
		// of the fixture gets a domain of its own.
		c.StorageCapacityDomains = map[string]config.StorageCapacityDomain{}
		for _, id := range []string{parityDiskShared, parityDiskLocal, parityISOLocal, parityISOShared} {
			c.StorageCapacityDomains[id] = config.StorageCapacityDomain{Members: []string{id}}
		}
		source.defs, source.statuses = nil, map[string][]json.RawMessage{}
		for _, def := range []map[string]any{
			{"storage": parityDiskShared, "type": "nfs", "shared": 1, "server": "nas", "export": "/disk", "content": "images,import"},
			{"storage": parityDiskLocal, "type": "zfspool", "shared": 0, "pool": "rpool/data", "content": "images,import", "nodes": "n1"},
			{"storage": parityISOLocal, "type": "dir", "shared": 0, "path": "/iso", "content": "iso"},
			{"storage": parityISOShared, "type": "nfs", "shared": 1, "server": "nas", "export": "/iso", "content": "iso"},
			{"storage": "source", "type": "nfs", "shared": 1, "server": "nas", "export": "/source", "content": "import"},
			{"storage": "local-source", "type": "dir", "shared": 0, "path": "/local", "content": "import"},
		} {
			source.defs = append(source.defs, planJSON(t, def))
			for _, node := range []string{"n1", "n2"} {
				if def["storage"] == parityDiskLocal && node != "n1" {
					continue
				}
				source.statuses[node] = append(source.statuses[node], planJSON(t, map[string]any{
					"storage": def["storage"], "active": 1, "enabled": 1, "total": uint64(100) << 30,
					"avail": uint64(80) << 30,
				}))
			}
		}
		cfg = c
	})
	parsed := isoParityParsed(t, cfg, r)
	deps := Deps{Config: cfg, RequestDirectorUUID: aaDirector}
	facts := []placement.NodeFacts{{Node: "n1"}, {Node: "n2"}}
	request.Groups[0].HANodes = storageComputeHANodes(deps, parsed, request.Groups[0].AZ, request.Groups[0].Nodes, facts)
	configured, err := ConfigureStorageVMPlanRequest(deps, parsed, request, 1<<20)
	if err != nil {
		t.Fatalf("%s: ConfigureStorageVMPlanRequest: %v", r, err)
	}
	return configured, collector, deps, parsed
}

// planPathFails plans the row's create with its ISO on iso, and reports
// whether the plan fails. A plan that fails for a reason other than the ISO
// fails the test.
func planPathFails(t *testing.T, r isoParityRow, iso string) bool {
	t.Helper()
	request, _, _, _ := isoParityFixture(t, r, iso)
	it, err := NewStoragePlanIterator(request)
	if err != nil {
		t.Fatalf("%s: NewStoragePlanIterator: %v", r, err)
	}
	if _, err := it.Next(context.Background()); err != nil {
		for _, rejection := range it.rejections {
			if strings.Contains(rejection, "ISO target is not shared") {
				return true
			}
		}
		t.Fatalf("%s: the plan failed for a reason other than the ISO: %v", r, err)
	}
	return false
}

// revalidationFails revalidates, under the row's settings, a recorded plan
// whose ISO sits on iso, the way the guard does before each mutation. The plan
// is recorded under settings that ask nothing of the ISO and carries the HA
// nodes the row's create plans with.
func revalidationFails(t *testing.T, r isoParityRow, iso string) bool {
	t.Helper()
	recordedRow := isoParityRow{
		mode: config.HAModeOff, topology: r.topology, policy: r.policy, sharedDisks: r.sharedDisks,
	}
	request, collector, _, _ := isoParityFixture(t, recordedRow, iso)
	it, err := NewStoragePlanIterator(request)
	if err != nil {
		t.Fatalf("%s: NewStoragePlanIterator: %v", r, err)
	}
	plan, err := it.Next(context.Background())
	if err != nil {
		t.Fatalf("%s: the recorded plan could not be made: %v", r, err)
	}
	if target, ok := managedVMRoleTarget(plan, storageRoleISO); !ok || target.StorageID != iso {
		t.Fatalf("%s: recorded ISO target %+v; want %s", r, target, iso)
	}
	current, _, deps, parsed := isoParityFixture(t, r, iso)
	plan.HANodes = current.Groups[0].HANodes
	ledger := inv.NewLedger()
	for i := range plan.Charges {
		if ledger, err = ledger.WithPlanned(request.Inventory, plan.Charges[i].Charge); err != nil {
			t.Fatal(err)
		}
	}
	m := &managedVMAllocation{deps: deps, parsed: parsed, prepared: &managedVMPlan{
		collector: collector, inventory: request.Inventory, selection: request.Selection, plan: plan, ledger: ledger,
	}}
	return m.revalidate(context.Background()) != nil
}

// TestSharedISORule_PlanAgreesWithThePlainPath pins that a storage plan, and
// the revalidation of a recorded plan before each mutation, refuse a
// node-local config-drive ISO exactly when the plain create path fails the
// create because of it. The rows cover every combination of pve.ha.mode, an
// HA registration feature, the replicate local policy, the ha cloud property,
// pve.require_shared_iso_for_ha, and shared or node-local disks. Each row
// also checks that a shared ISO passes all three, so a failure always comes
// from the ISO.
func TestSharedISORule_PlanAgreesWithThePlainPath(t *testing.T) {
	t.Parallel()
	for _, r := range isoParityRows() {
		t.Run(r.String(), func(t *testing.T) {
			t.Parallel()
			if planPathFails(t, r, parityISOShared) || revalidationFails(t, r, parityISOShared) {
				t.Fatal("a shared ISO failed the plan, so the row cannot tell what the ISO decides")
			}
			// Under topology shared a node-local disk fails the create, and under
			// topology local with the replicate policy no storage reads as
			// shared, so the plain path refuses even a shared ISO.
			if plainPathFails(t, r, true) {
				t.Skip("the plain path fails the create whatever the ISO is, so the row says nothing of the ISO")
			}
			plain := plainPathFails(t, r, false)
			if got := planPathFails(t, r, parityISOLocal); got != plain {
				t.Errorf("the plan refuses a node-local ISO = %v; the plain path fails the create = %v", got, plain)
			}
			if got := revalidationFails(t, r, parityISOLocal); got != plain {
				t.Errorf("the revalidation refuses a node-local ISO = %v; the plain path fails the create = %v",
					got, plain)
			}
		})
	}
}
