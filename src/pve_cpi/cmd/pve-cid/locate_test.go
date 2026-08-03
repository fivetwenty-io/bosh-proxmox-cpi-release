package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// fakeReader implements Reader with fully in-memory fixtures. Production
// (pveReader in client.go) adapts a real pve.Client; this fake lets locate/
// stemcells logic be tested without a live PVE cluster.
type fakeReader struct {
	vms       []ClusterVM
	configs   map[string]map[string]any // key: fmt.Sprintf("%s/%d", node, vmid)
	templates map[string][]pve.TemplateRef
	volumes   map[string]string               // key: node+"|"+storage+"|"+filename -> volid ("" or missing = not found)
	content   map[string][]StorageContentItem // key: node+"|"+storage
	nodes     []string

	// storageShared/storageSharedKnown key storage IDs. A storage absent from
	// storageSharedKnown reports known=false (undetermined), matching
	// production's fail-safe-to-local behavior when the /storage lookup
	// cannot classify an entry.
	storageShared      map[string]bool
	storageSharedKnown map[string]bool

	listClusterVMsErr error
}

func (f *fakeReader) ListClusterVMs(context.Context) ([]ClusterVM, error) {
	if f.listClusterVMsErr != nil {
		return nil, f.listClusterVMsErr
	}
	return f.vms, nil
}

func (f *fakeReader) VMConfig(_ context.Context, node string, vmid int) (map[string]any, error) {
	cfg, ok := f.configs[fmt.Sprintf("%s/%d", node, vmid)]
	if !ok {
		return nil, fmt.Errorf("fakeReader: no config for %s/%d", node, vmid)
	}
	return cfg, nil
}

func (f *fakeReader) TemplatesBySHA8(_ context.Context, sha8 string) ([]pve.TemplateRef, error) {
	return f.templates[sha8], nil
}

func (f *fakeReader) FindStemcellVolume(_ context.Context, node, storage, filename string) (string, error) {
	return f.volumes[node+"|"+storage+"|"+filename], nil
}

func (f *fakeReader) ListStorageContent(_ context.Context, node, storage string) ([]StorageContentItem, error) {
	return f.content[node+"|"+storage], nil
}

func (f *fakeReader) ListNodes(context.Context) ([]string, error) {
	return f.nodes, nil
}

func (f *fakeReader) StorageIsShared(_ context.Context, storage string) (shared bool, known bool) {
	if !f.storageSharedKnown[storage] {
		return false, false
	}
	return f.storageShared[storage], true
}

func diskCfg(description string, disks map[string]string) map[string]any {
	cfg := map[string]any{"description": description}
	for k, v := range disks {
		cfg[k] = v
	}
	return cfg
}

func TestLocateDisk_HolderFoundOnBusSlot(t *testing.T) {
	r := &fakeReader{
		vms: []ClusterVM{
			{VMID: 100, Node: "pve1", Name: "vm-a"},
			{VMID: 200, Node: "pve1", Name: "vm-b"},
		},
		configs: map[string]map[string]any{
			"pve1/100": diskCfg("", map[string]string{"scsi0": "local-lvm:vm-100-disk-0,size=20G"}),
			"pve1/200": diskCfg("", map[string]string{"scsi3": "local-lvm:vm-9500-disk-0,size=64G,cache=writeback"}),
		},
	}

	result, err := locateDisk(context.Background(), r, "local-lvm:vm-9500-disk-0")
	if err != nil {
		t.Fatalf("locateDisk error = %v", err)
	}
	if result.Holder == nil {
		t.Fatal("expected a holder to be found")
	}
	if result.Holder.VMID != 200 || result.Holder.Node != "pve1" || result.Holder.Slot != "scsi3" {
		t.Errorf("holder = %+v", result.Holder)
	}
	if len(result.SentinelMatches) != 0 {
		t.Errorf("expected no sentinel matches, got %v", result.SentinelMatches)
	}
}

func TestLocateDisk_SentinelOnlyMatch(t *testing.T) {
	sentinelDesc := buildSentinelDescription(t, map[string]any{
		attachedDisksSentinelKey: map[string]string{
			"local-lvm:vm-9500-disk-0": "pvd-stale-cid",
		},
	})
	r := &fakeReader{
		vms: []ClusterVM{
			{VMID: 100, Node: "pve1", Name: "vm-a"},
		},
		configs: map[string]map[string]any{
			// No bus slot holds the volid; the sentinel is the only trace.
			"pve1/100": diskCfg(sentinelDesc, map[string]string{"scsi0": "local-lvm:vm-100-disk-0,size=20G"}),
		},
	}

	result, err := locateDisk(context.Background(), r, "local-lvm:vm-9500-disk-0")
	if err != nil {
		t.Fatalf("locateDisk error = %v", err)
	}
	if result.Holder != nil {
		t.Fatalf("expected no bus-slot holder, got %+v", result.Holder)
	}
	if len(result.SentinelMatches) != 1 {
		t.Fatalf("expected exactly one sentinel match, got %d", len(result.SentinelMatches))
	}
	if result.SentinelMatches[0].AttachedCID != "pvd-stale-cid" {
		t.Errorf("AttachedCID = %q, want %q", result.SentinelMatches[0].AttachedCID, "pvd-stale-cid")
	}
	if result.SentinelMatches[0].VMID != 100 {
		t.Errorf("sentinel match VMID = %d, want 100", result.SentinelMatches[0].VMID)
	}
}

func TestLocateDisk_Unattached(t *testing.T) {
	r := &fakeReader{
		vms: []ClusterVM{
			{VMID: 100, Node: "pve1", Name: "vm-a"},
		},
		configs: map[string]map[string]any{
			"pve1/100": diskCfg("", map[string]string{"scsi0": "local-lvm:vm-100-disk-0,size=20G"}),
		},
	}

	result, err := locateDisk(context.Background(), r, "local-lvm:vm-does-not-exist-disk-0")
	if err != nil {
		t.Fatalf("locateDisk error = %v", err)
	}
	if result.Holder != nil {
		t.Errorf("expected no holder, got %+v", result.Holder)
	}
	if len(result.SentinelMatches) != 0 {
		t.Errorf("expected no sentinel matches, got %v", result.SentinelMatches)
	}
}

func TestLocateDisk_SkipsVMsWithConfigFetchFailure(t *testing.T) {
	r := &fakeReader{
		vms: []ClusterVM{
			{VMID: 100, Node: "pve1", Name: "vm-a"}, // no config entry -> fetch fails
			{VMID: 200, Node: "pve1", Name: "vm-b"},
		},
		configs: map[string]map[string]any{
			"pve1/200": diskCfg("", map[string]string{"scsi1": "local-lvm:vm-500-disk-0"}),
		},
	}
	result, err := locateDisk(context.Background(), r, "local-lvm:vm-500-disk-0")
	if err != nil {
		t.Fatalf("locateDisk error = %v", err)
	}
	if result.Holder == nil || result.Holder.VMID != 200 {
		t.Errorf("holder = %+v, want vmid=200", result.Holder)
	}
}

func TestLocateDisk_ListClusterVMsError(t *testing.T) {
	r := &fakeReader{listClusterVMsErr: fmt.Errorf("boom")}
	if _, err := locateDisk(context.Background(), r, "x:y"); err == nil {
		t.Fatal("expected error to propagate from ListClusterVMs")
	}
}

func templateProvenanceDescription(t *testing.T, p templateProvenance) string {
	t.Helper()
	// Round-trip through the same struct locate/stemcells decode, so the
	// fixture is guaranteed schema-compatible with parseTemplateProvenance.
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal provenance: %v", err)
	}
	return string(b)
}

func TestLocateStemcell_TemplateFoundWithRefs(t *testing.T) {
	decoded, err := DecodeCID(":heavy:local:import/bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2")
	if err != nil {
		t.Fatalf("DecodeCID: %v", err)
	}

	prov := templateProvenance{
		Name: "ubuntu-jammy", Version: "1.719", SHA8: "cafebabe",
		Kind: "heavy", CID: decoded.Raw, Created: "2026-08-01T00:00:00Z",
		DirectorRefs: []string{"dir-1", "dir-2"},
	}
	r := &fakeReader{
		templates: map[string][]pve.TemplateRef{
			"cafebabe": {{VMID: 6042, Node: "pve1", Name: "bosh-stemcell-ubuntu-jammy-1.719-cafebabe"}},
		},
		configs: map[string]map[string]any{
			"pve1/6042": {"description": templateProvenanceDescription(t, prov)},
		},
		volumes: map[string]string{
			"pve1|local|bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2": "local:import/bosh-stemcell-ubuntu-jammy-1.719-cafebabe.qcow2",
		},
	}

	result, err := locateStemcell(context.Background(), r, decoded)
	if err != nil {
		t.Fatalf("locateStemcell error = %v", err)
	}
	if !result.VolumeExists {
		t.Error("expected VolumeExists = true")
	}
	if len(result.Templates) != 1 {
		t.Fatalf("expected 1 template hit, got %d", len(result.Templates))
	}
	hit := result.Templates[0]
	if hit.VMID != 6042 || !hit.HasProvenance {
		t.Errorf("hit = %+v", hit)
	}
	if len(hit.DirectorRefs) != 2 {
		t.Errorf("DirectorRefs = %v, want 2 entries", hit.DirectorRefs)
	}
}

// TestLocateStemcell_OrphanTemplate covers a cache template with zero
// director references — the locate-time signal that corroborates
// "pve-cid stemcells --orphans" for this one CID.
func TestLocateStemcell_OrphanTemplate(t *testing.T) {
	decoded, err := DecodeCID(":heavy:local:import/bosh-stemcell-x-1-deadbeef.qcow2")
	if err != nil {
		t.Fatalf("DecodeCID: %v", err)
	}
	prov := templateProvenance{
		Name: "x", Version: "1", SHA8: "deadbeef", Kind: "heavy",
		Created: "2026-08-01T00:00:00Z", DirectorRefs: []string{},
	}
	r := &fakeReader{
		templates: map[string][]pve.TemplateRef{
			"deadbeef": {{VMID: 7000, Node: "pve1", Name: "bosh-stemcell-x-1-deadbeef"}},
		},
		configs: map[string]map[string]any{
			"pve1/7000": {"description": templateProvenanceDescription(t, prov)},
		},
	}

	result, err := locateStemcell(context.Background(), r, decoded)
	if err != nil {
		t.Fatalf("locateStemcell error = %v", err)
	}
	if len(result.Templates) != 1 {
		t.Fatalf("expected 1 template hit, got %d", len(result.Templates))
	}
	if len(result.Templates[0].DirectorRefs) != 0 {
		t.Errorf("expected zero director refs (orphan), got %v", result.Templates[0].DirectorRefs)
	}
	if result.VolumeExists {
		t.Error("expected VolumeExists = false (no volume fixture registered)")
	}
}

func TestLocateStemcell_NoTemplatesFound(t *testing.T) {
	decoded, err := DecodeCID(":light:local:import/bosh-stemcell-x-1-00000001.qcow2")
	if err != nil {
		t.Fatalf("DecodeCID: %v", err)
	}
	r := &fakeReader{nodes: []string{"pve1"}}
	result, err := locateStemcell(context.Background(), r, decoded)
	if err != nil {
		t.Fatalf("locateStemcell error = %v", err)
	}
	if len(result.Templates) != 0 {
		t.Errorf("expected zero template hits, got %d", len(result.Templates))
	}
}

func TestLocateStemcell_RejectsNonStemcellFamily(t *testing.T) {
	decoded, err := DecodeCID("5042")
	if err != nil {
		t.Fatalf("DecodeCID: %v", err)
	}
	if _, err := locateStemcell(context.Background(), &fakeReader{}, decoded); err == nil {
		t.Fatal("expected error for a non-stemcell family")
	}
}

func TestResolveBareVolid(t *testing.T) {
	// pvd- envelope
	cid, err := pve.EncodeDiskCID("local:vm-1-disk-0", nil)
	if err != nil {
		t.Fatalf("EncodeDiskCID: %v", err)
	}
	got, err := resolveBareVolid(cid)
	if err != nil {
		t.Fatalf("resolveBareVolid(pvd-): %v", err)
	}
	if got != "local:vm-1-disk-0" {
		t.Errorf("got %q, want %q", got, "local:vm-1-disk-0")
	}

	// raw volid passthrough
	got, err = resolveBareVolid("local:vm-2-disk-0")
	if err != nil {
		t.Fatalf("resolveBareVolid(raw volid): %v", err)
	}
	if got != "local:vm-2-disk-0" {
		t.Errorf("got %q, want %q", got, "local:vm-2-disk-0")
	}

	// garbage
	if _, err := resolveBareVolid("not-a-volid-or-cid"); err == nil {
		t.Error("expected error for garbage input")
	}
}
