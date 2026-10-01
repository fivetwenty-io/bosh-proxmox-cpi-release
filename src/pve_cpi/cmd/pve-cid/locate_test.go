package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
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

	result, err := locateDisk(context.Background(), r, "local-lvm:vm-9500-disk-0", "")
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

	result, err := locateDisk(context.Background(), r, "local-lvm:vm-9500-disk-0", "")
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

	result, err := locateDisk(context.Background(), r, "local-lvm:vm-does-not-exist-disk-0", "")
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
	result, err := locateDisk(context.Background(), r, "local-lvm:vm-500-disk-0", "")
	if err != nil {
		t.Fatalf("locateDisk error = %v", err)
	}
	if result.Holder == nil || result.Holder.VMID != 200 {
		t.Errorf("holder = %+v, want vmid=200", result.Holder)
	}
}

func TestLocateDisk_SerialMatchesRenamedVolume(t *testing.T) {
	const stableID = "bpd-0011223344556677"
	r := &fakeReader{
		vms: []ClusterVM{
			{VMID: 700, Node: "pve1", Name: "vm-a"},
		},
		configs: map[string]map[string]any{
			// The birth volid (vm-9500) is gone: a reassignment renamed the
			// volume for VM 700. The drive serial is the surviving identity.
			"pve1/700": diskCfg("", map[string]string{
				"scsi1": "local-lvm:vm-700-disk-2,serial=" + stableID + ",size=64G",
			}),
		},
	}

	result, err := locateDisk(context.Background(), r, "local-lvm:vm-9500-disk-0", stableID)
	if err != nil {
		t.Fatalf("locateDisk error = %v", err)
	}
	if result.Holder == nil || result.Holder.VMID != 700 || result.Holder.Slot != "scsi1" {
		t.Fatalf("holder = %+v, want the serial-matched slot", result.Holder)
	}
	if result.CurrentVolid != "local-lvm:vm-700-disk-2" {
		t.Errorf("current_volid = %q, want the renamed name", result.CurrentVolid)
	}
	if result.StableID != stableID {
		t.Errorf("stable_id = %q", result.StableID)
	}
}

func TestLocateDisk_ListClusterVMsError(t *testing.T) {
	r := &fakeReader{listClusterVMsErr: fmt.Errorf("boom")}
	if _, err := locateDisk(context.Background(), r, "x:y", ""); err == nil {
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
	got, gotID, err := resolveBareVolid(cid)
	if err != nil {
		t.Fatalf("resolveBareVolid(pvd-): %v", err)
	}
	if got != "local:vm-1-disk-0" {
		t.Errorf("got %q, want %q", got, "local:vm-1-disk-0")
	}
	if gotID != "" {
		t.Errorf("stable ID for meta-less CID: got %q, want empty", gotID)
	}

	// pvd- envelope carrying a stable ID
	idCID, err := pve.EncodeDiskCID("local:vm-3-disk-0", &pve.DiskCIDMeta{ID: "bpd-0011223344556677"})
	if err != nil {
		t.Fatalf("EncodeDiskCID with ID: %v", err)
	}
	got, gotID, err = resolveBareVolid(idCID)
	if err != nil {
		t.Fatalf("resolveBareVolid(pvd- with ID): %v", err)
	}
	if got != "local:vm-3-disk-0" || gotID != "bpd-0011223344556677" {
		t.Errorf("got (%q, %q), want (%q, %q)", got, gotID, "local:vm-3-disk-0", "bpd-0011223344556677")
	}

	// raw volid passthrough
	got, gotID, err = resolveBareVolid("local:vm-2-disk-0")
	if err != nil {
		t.Fatalf("resolveBareVolid(raw volid): %v", err)
	}
	if got != "local:vm-2-disk-0" {
		t.Errorf("got %q, want %q", got, "local:vm-2-disk-0")
	}
	if gotID != "" {
		t.Errorf("stable ID for raw volid: got %q, want empty", gotID)
	}

	// garbage
	if _, _, err := resolveBareVolid("not-a-volid-or-cid"); err == nil {
		t.Error("expected error for garbage input")
	}
}

// TestLocateDisk_ListsEveryActiveHolder is a volume that two guests name on
// an active slot. Holder keeps naming the first by VMID, and Holders names
// both. Before this, the scan stopped at the first match and the second
// guest was invisible.
func TestLocateDisk_ListsEveryActiveHolder(t *testing.T) {
	r := &fakeReader{
		vms: []ClusterVM{{VMID: 888, Node: "pve2"}, {VMID: 777, Node: "pve1"}},
		configs: map[string]map[string]any{
			"pve1/777": diskCfg("", map[string]string{"scsi1": "a:123/vm-123-disk-0.raw,size=5G"}),
			"pve2/888": diskCfg("", map[string]string{"virtio2": "a:123/vm-123-disk-0.raw"}),
		},
	}
	result, err := locateDisk(context.Background(), r, "a:123/vm-123-disk-0.raw", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Holder == nil || result.Holder.VMID != 777 || result.Holder.Slot != "scsi1" {
		t.Fatalf("holder = %+v, want the first match by VMID, 777 scsi1", result.Holder)
	}
	want := []DiskHolderMatch{
		{VMID: 777, Node: "pve1", Slot: "scsi1", Volid: "a:123/vm-123-disk-0.raw", Match: "volid"},
		{VMID: 888, Node: "pve2", Slot: "virtio2", Volid: "a:123/vm-123-disk-0.raw", Match: "volid"},
	}
	if fmt.Sprint(result.Holders) != fmt.Sprint(want) {
		t.Fatalf("holders = %+v, want %+v", result.Holders, want)
	}
}

// TestLocateDisk_SerialOnOneGuestVolidOnAnother keeps Holder as the first
// match in VMID order while listing both kinds of match.
func TestLocateDisk_SerialOnOneGuestVolidOnAnother(t *testing.T) {
	const stableID = "bpd-0011223344556677"
	r := &fakeReader{
		vms: []ClusterVM{{VMID: 700, Node: "pve1"}, {VMID: 90100, Node: "pve1"}},
		configs: map[string]map[string]any{
			"pve1/700":   diskCfg("", map[string]string{"scsi1": "a:700/vm-700-disk-2.raw,serial=" + stableID}),
			"pve1/90100": diskCfg("", map[string]string{"scsi4": "a:123/vm-123-disk-0.raw,serial=bpd-ffffffffffffffff"}),
		},
	}
	result, err := locateDisk(context.Background(), r, "a:123/vm-123-disk-0.raw", stableID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Holder == nil || result.Holder.VMID != 700 || result.CurrentVolid != "a:700/vm-700-disk-2.raw" {
		t.Fatalf("holder = %+v current_volid = %q, want 700 under its renamed name", result.Holder, result.CurrentVolid)
	}
	if len(result.Holders) != 2 || result.Holders[0].Match != "serial" || result.Holders[1].Match != "volid" || result.Holders[1].VMID != 90100 {
		t.Fatalf("holders = %+v, want the serial match on 700 and the volid match on 90100", result.Holders)
	}
}

// TestLocateDisk_ListsUnusedEntriesUnderEveryName finds the volume on unused
// entries under its located name and under the name a serial match carries.
// Before this, unused entries were never read.
func TestLocateDisk_ListsUnusedEntriesUnderEveryName(t *testing.T) {
	const stableID = "bpd-0011223344556677"
	r := &fakeReader{
		vms: []ClusterVM{{VMID: 777, Node: "pve1"}, {VMID: 778, Node: "pve1"}, {VMID: 90100, Node: "pve1"}},
		configs: map[string]map[string]any{
			"pve1/777":   diskCfg("", map[string]string{"unused0": "a:123/vm-123-disk-0.raw"}),
			"pve1/778":   diskCfg("", map[string]string{"unused1": "a:90100/vm-90100-disk-3.raw", "unused0": "a:778/vm-778-disk-0.raw"}),
			"pve1/90100": diskCfg("", map[string]string{"scsi0": "a:90100/vm-90100-disk-3.raw,serial=" + stableID}),
		},
	}
	result, err := locateDisk(context.Background(), r, "a:123/vm-123-disk-0.raw", stableID)
	if err != nil {
		t.Fatal(err)
	}
	want := []UnusedReference{
		{VMID: 777, Node: "pve1", Slot: "unused0", Volid: "a:123/vm-123-disk-0.raw"},
		{VMID: 778, Node: "pve1", Slot: "unused1", Volid: "a:90100/vm-90100-disk-3.raw"},
	}
	if fmt.Sprint(result.UnusedRefs) != fmt.Sprint(want) {
		t.Fatalf("unused_refs = %+v, want %+v", result.UnusedRefs, want)
	}
}

// TestLocateDisk_ListsUnreadableVMs records the guests whose config read
// failed. Before this, they were skipped without a trace.
func TestLocateDisk_ListsUnreadableVMs(t *testing.T) {
	r := &fakeReader{
		vms: []ClusterVM{{VMID: 100, Node: "pve1"}, {VMID: 200, Node: "pve1"}, {VMID: 300, Node: "pve2"}},
		configs: map[string]map[string]any{
			"pve1/200": diskCfg("", map[string]string{"scsi1": "local-lvm:vm-500-disk-0"}),
		},
	}
	result, err := locateDisk(context.Background(), r, "local-lvm:vm-500-disk-0", "")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(result.UnreadableVMIDs) != "[100 300]" {
		t.Fatalf("unreadable_vmids = %v, want [100 300]", result.UnreadableVMIDs)
	}
	var stderr bytes.Buffer
	printDiskLocateWarnings(&stderr, result)
	if !strings.Contains(stderr.String(), "warning: the configs of 2 VM(s) could not be read (100, 300), so this result may be incomplete") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// TestLocateDisk_SingleHolderOutputIsUnchanged pins compatibility: one holder
// and nothing else prints exactly the text it printed before and warns about
// nothing. The JSON gains only the one-entry holders list.
func TestLocateDisk_SingleHolderOutputIsUnchanged(t *testing.T) {
	r := &fakeReader{
		vms:     []ClusterVM{{VMID: 200, Node: "pve1"}},
		configs: map[string]map[string]any{"pve1/200": diskCfg("", map[string]string{"scsi3": "local-lvm:vm-9500-disk-0,size=64G"})},
	}
	result, err := locateDisk(context.Background(), r, "local-lvm:vm-9500-disk-0", "")
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	printDiskLocateResult(&out, result)
	printDiskLocateWarnings(&stderr, result)
	if out.String() != "volid: local-lvm:vm-9500-disk-0\nholder: vmid=200 node=pve1 slot=scsi3\nsentinels: none\n" {
		t.Fatalf("stdout = %q", out.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want nothing", stderr.String())
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"holders":[{"vmid":200,"node":"pve1","slot":"scsi3","volid":"local-lvm:vm-9500-disk-0","match":"volid"}]`) {
		t.Fatalf("json = %s, want the one-entry holders list", raw)
	}
	for _, key := range []string{`"unused_refs"`, `"unreadable_vmids"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("json = %s, want no %s key for a single clean holder", raw, key)
		}
	}
}

// TestLocateDisk_WarnsWhenMoreThanOneGuestNamesTheDisk prints the extra
// holders and unused entries and warns on stderr, pointing at the runbook.
func TestLocateDisk_WarnsWhenMoreThanOneGuestNamesTheDisk(t *testing.T) {
	r := &fakeReader{
		vms: []ClusterVM{{VMID: 777, Node: "pve1"}, {VMID: 888, Node: "pve2"}, {VMID: 999, Node: "pve2"}},
		configs: map[string]map[string]any{
			"pve1/777": diskCfg("", map[string]string{"scsi1": "a:123/vm-123-disk-0.raw"}),
			"pve2/888": diskCfg("", map[string]string{"scsi2": "a:123/vm-123-disk-0.raw"}),
			"pve2/999": diskCfg("", map[string]string{"unused0": "a:123/vm-123-disk-0.raw"}),
		},
	}
	result, err := locateDisk(context.Background(), r, "a:123/vm-123-disk-0.raw", "")
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	printDiskLocateResult(&out, result)
	printDiskLocateWarnings(&stderr, result)
	for _, want := range []string{
		"holder: vmid=777 node=pve1 slot=scsi1\n",
		"also held by: vmid=888 node=pve2 slot=scsi2 volid=a:123/vm-123-disk-0.raw (matched by volid)\n",
		"unused: vmid=999 node=pve2 slot=unused0 volid=a:123/vm-123-disk-0.raw\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout = %q, missing %q", out.String(), want)
		}
	}
	for _, want := range []string{
		"warning: 3 guests name this disk (active slots: 2, unused entries: 1). The holder shows only the first active slot in VMID order.",
		"The Director's VM CID decides which guest really holds the disk.",
		`see "Auditing parked disks with scripts/disk-audit" in docs/operations.md of bosh-proxmox-cpi-release`,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, missing %q", stderr.String(), want)
		}
	}
}

// TestLocateDocsPointerHeadingExists pins the heading the warning quotes to a
// heading that exists in docs/operations.md.
func TestLocateDocsPointerHeadingExists(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "operations.md"))
	if err != nil {
		t.Fatal(err)
	}
	heading := strings.Split(locateDocsPointer, `"`)[1]
	for line := range strings.Lines(string(doc)) {
		if strings.HasPrefix(line, "#") && strings.ReplaceAll(strings.TrimSpace(strings.TrimLeft(line, "#")), "`", "") == heading {
			return
		}
	}
	t.Fatalf("docs/operations.md has no heading %q", heading)
}

// TestLocateDisk_WarnsWhenOnlyUnusedEntriesNameTheDisk is two guests naming
// the volume only on unused entries. The warning still fires, and it leaves
// out the holder sentence, because there is no active slot to show.
func TestLocateDisk_WarnsWhenOnlyUnusedEntriesNameTheDisk(t *testing.T) {
	r := &fakeReader{
		vms: []ClusterVM{{VMID: 777, Node: "pve1"}, {VMID: 888, Node: "pve2"}},
		configs: map[string]map[string]any{
			"pve1/777": diskCfg("", map[string]string{"unused0": "a:123/vm-123-disk-0.raw"}),
			"pve2/888": diskCfg("", map[string]string{"unused3": "a:123/vm-123-disk-0.raw"}),
		},
	}
	result, err := locateDisk(context.Background(), r, "a:123/vm-123-disk-0.raw", "")
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	printDiskLocateWarnings(&stderr, result)
	if !strings.HasPrefix(stderr.String(), "warning: 2 guests name this disk (active slots: 0, unused entries: 2). The Director's VM CID decides") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "The holder shows") {
		t.Fatalf("stderr = %q, want no holder sentence when no active slot matches", stderr.String())
	}
}
