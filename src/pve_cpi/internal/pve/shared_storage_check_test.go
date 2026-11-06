package pve

import (
	"reflect"
	"strings"
	"testing"
)

// disjointLayout returns a cluster whose four bands all sit inside
// [base, base+3999], with a next-id range just above it.
func disjointLayout(name string, base int, storages ...StorageInfo) ClusterVMIDLayout {
	return ClusterVMIDLayout{
		Name:     name,
		Storages: storages,
		Bands: VMIDBands{
			VM:       VMIDBand{base, base + 999},
			Disk:     VMIDBand{base + 1000, base + 1999},
			Template: VMIDBand{base + 2000, base + 2099},
			Parker:   VMIDBand{base + 3000, base + 3999},
		},
		NextID: VMIDBand{base + 4000, base + 4999},
	}
}

func nfsStorage(name, server, export string) StorageInfo {
	return StorageInfo{Name: name, Type: "nfs", Shared: true, Server: server, Export: export, Content: "images,iso"}
}

func findingKinds(findings []SharedExportFinding) []SharedExportFindingKind {
	out := make([]SharedExportFindingKind, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Kind)
	}
	return out
}

func TestNextIDRange(t *testing.T) {
	t.Parallel()
	if got := NextIDRange(0, 0); got != (VMIDBand{100, 999999}) {
		t.Errorf("NextIDRange(0, 0) = %v, want PVE's default 100-999999", got)
	}
	if got := NextIDRange(50000, 60000); got != (VMIDBand{50000, 59999}) {
		t.Errorf("NextIDRange(50000, 60000) = %v, want the exclusive upper bound dropped", got)
	}
}

func TestCheckSharedExports(t *testing.T) {
	t.Parallel()
	export := nfsStorage("nfs-a", "10.0.0.5", "/exports/pve")
	sameExport := nfsStorage("nfs-b", "10.0.0.5", "/exports/pve/")
	otherExport := nfsStorage("nfs-b", "10.0.0.5", "/exports/other")
	byHostname := nfsStorage("nfs-b", "filer.lab", "/exports/pve")

	overlapping := disjointLayout("az2", 100000, sameExport)
	overlapping.Bands.Disk = VMIDBand{1500, 1600} // inside az1's VM band

	defaultsA := ClusterVMIDLayout{Name: "az1", Storages: []StorageInfo{export}, Bands: DefaultVMIDBands(), NextID: VMIDBand{200000, 209999}}
	defaultsB := ClusterVMIDLayout{Name: "az2", Storages: []StorageInfo{sameExport}, Bands: DefaultVMIDBands(), NextID: VMIDBand{210000, 219999}}

	nextOverOwn := disjointLayout("az1", 1000, export)
	nextOverOwn.NextID = NextIDRange(0, 0)

	nextOverPeerDisk := disjointLayout("az1", 1000, export)
	nextOverPeerDisk.NextID = VMIDBand{11000, 11999} // az2's disk band is 11000-11999

	tests := []struct {
		name     string
		clusters []ClusterVMIDLayout
		want     []SharedExportFindingKind
	}{
		{
			name:     "overlapping bands on one export",
			clusters: []ClusterVMIDLayout{disjointLayout("az1", 1000, export), overlapping},
			want:     []SharedExportFindingKind{FindingBandOverlap},
		},
		{
			name:     "disjoint bands on one export",
			clusters: []ClusterVMIDLayout{disjointLayout("az1", 1000, export), disjointLayout("az2", 10000, sameExport)},
			want:     nil,
		},
		{
			name:     "default bands on one export",
			clusters: []ClusterVMIDLayout{defaultsA, defaultsB},
			want:     []SharedExportFindingKind{FindingBandOverlap},
		},
		{
			name: "overlapping bands on different exports",
			clusters: []ClusterVMIDLayout{
				{Name: "az1", Storages: []StorageInfo{export}, Bands: DefaultVMIDBands(), NextID: VMIDBand{200000, 209999}},
				{Name: "az2", Storages: []StorageInfo{otherExport}, Bands: DefaultVMIDBands(), NextID: VMIDBand{210000, 219999}},
			},
			want: nil,
		},
		{
			name: "a shared dir with one path",
			clusters: []ClusterVMIDLayout{
				{Name: "az1", Storages: []StorageInfo{{Name: "d", Type: "dir", Shared: true, Path: "/mnt/pve/shared", Nodes: []string{"a1"}}}, Bands: DefaultVMIDBands(), NextID: VMIDBand{200000, 209999}},
				{Name: "az2", Storages: []StorageInfo{{Name: "d", Type: "dir", Shared: true, Path: "/mnt/pve/shared/", Nodes: []string{"b1"}}}, Bands: DefaultVMIDBands(), NextID: VMIDBand{210000, 219999}},
			},
			want: []SharedExportFindingKind{FindingBandOverlap},
		},
		{
			name: "an unshared dir with one path is local to each cluster",
			clusters: []ClusterVMIDLayout{
				{Name: "az1", Storages: []StorageInfo{{Name: "local", Type: "dir", Path: "/var/lib/vz"}}, Bands: DefaultVMIDBands(), NextID: VMIDBand{200000, 209999}},
				{Name: "az2", Storages: []StorageInfo{{Name: "local", Type: "dir", Path: "/var/lib/vz"}}, Bands: DefaultVMIDBands(), NextID: VMIDBand{210000, 219999}},
			},
			want: nil,
		},
		{
			name: "one export with backup content only",
			clusters: []ClusterVMIDLayout{
				{Name: "az1", Storages: []StorageInfo{{Name: "b", Type: "nfs", Server: "s", Export: "/bk", Content: "backup"}}, Bands: DefaultVMIDBands(), NextID: VMIDBand{200000, 209999}},
				{Name: "az2", Storages: []StorageInfo{{Name: "b", Type: "nfs", Server: "s", Export: "/bk", Content: "backup"}}, Bands: DefaultVMIDBands(), NextID: VMIDBand{210000, 219999}},
			},
			want: nil,
		},
		{
			name: "a disabled storage on one export still counts",
			clusters: []ClusterVMIDLayout{
				{Name: "az1", Storages: []StorageInfo{export}, Bands: DefaultVMIDBands(), NextID: VMIDBand{200000, 209999}},
				{Name: "az2", Storages: []StorageInfo{{Name: "x", Type: "nfs", Server: "10.0.0.5", Export: "/exports/pve", Content: "images", Disabled: true}}, Bands: DefaultVMIDBands(), NextID: VMIDBand{210000, 219999}},
			},
			want: []SharedExportFindingKind{FindingBandOverlap},
		},
		{
			name: "one export path behind different server strings",
			clusters: []ClusterVMIDLayout{
				{Name: "az1", Storages: []StorageInfo{export}, Bands: DefaultVMIDBands(), NextID: VMIDBand{200000, 209999}},
				{Name: "az2", Storages: []StorageInfo{byHostname}, Bands: DefaultVMIDBands(), NextID: VMIDBand{210000, 219999}},
			},
			want: []SharedExportFindingKind{FindingPossibleSharedBacking},
		},
		{
			name:     "one export path behind different servers with disjoint ranges",
			clusters: []ClusterVMIDLayout{disjointLayout("az1", 1000, export), disjointLayout("az2", 10000, byHostname)},
			want:     nil,
		},
		{
			name:     "a next-id range over the cluster's own VM band",
			clusters: []ClusterVMIDLayout{nextOverOwn},
			want:     []SharedExportFindingKind{FindingNextIDOverOwnBand},
		},
		{
			name:     "a next-id range over the other cluster's disk band",
			clusters: []ClusterVMIDLayout{nextOverPeerDisk, disjointLayout("az2", 10000, sameExport)},
			want:     []SharedExportFindingKind{FindingNextIDOverPeer},
		},
		{
			name: "a cluster whose options cannot be read",
			clusters: []ClusterVMIDLayout{
				disjointLayout("az1", 1000, export),
				{Name: "az2", Unchecked: "GET /cluster/options: 403 permission denied"},
			},
			want: []SharedExportFindingKind{FindingUnchecked, FindingUnchecked},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CheckSharedExports(tc.clusters)
			if kinds := findingKinds(got); !reflect.DeepEqual(kinds, tc.want) && (len(kinds) != 0 || len(tc.want) != 0) {
				t.Fatalf("findings = %v, want kinds %v", got, tc.want)
			}
		})
	}
}

func TestCheckSharedExportsDetails(t *testing.T) {
	t.Parallel()
	a := disjointLayout("az1", 1000, nfsStorage("nfs-a", "10.0.0.5", "/exports/pve"))
	b := disjointLayout("az2", 100000, nfsStorage("nfs-b", "10.0.0.5", "/exports/pve"))
	b.Bands.Disk = VMIDBand{1500, 1600}
	b.NextID = VMIDBand{2000, 2050} // over az1's disk band

	findings := CheckSharedExports([]ClusterVMIDLayout{a, b})
	if len(findings) != 2 {
		t.Fatalf("findings = %v, want a band overlap and a peer next-id overlap", findings)
	}
	overlap, peer := findings[0], findings[1]
	if overlap.Kind != FindingBandOverlap || peer.Kind != FindingNextIDOverPeer {
		t.Fatalf("findings out of order: %v", findings)
	}
	if !reflect.DeepEqual(overlap.Backings, []string{"nfs://10.0.0.5/exports/pve"}) {
		t.Errorf("backings = %v", overlap.Backings)
	}
	if !strings.Contains(overlap.Detail, "az1 vm band 1000-1999 overlaps az2 disk band 1500-1600") {
		t.Errorf("overlap detail = %q", overlap.Detail)
	}
	if !strings.Contains(peer.Detail, "az2 next-id 2000-2050 overlaps az1 disk band 2000-2999") {
		t.Errorf("peer detail = %q", peer.Detail)
	}
	for _, f := range findings {
		if !f.Failing() {
			t.Errorf("%s does not fail the run", f.Kind)
		}
		if !strings.HasPrefix(f.String(), string(f.Kind)+" [az1, az2] on nfs://10.0.0.5/exports/pve: ") {
			t.Errorf("String = %q", f.String())
		}
	}
	if (SharedExportFinding{Kind: FindingPossibleSharedBacking}).Failing() {
		t.Error("a possible shared backing fails the run")
	}
}

func TestCheckSharedExportsIsDeterministic(t *testing.T) {
	t.Parallel()
	clusters := []ClusterVMIDLayout{
		{Name: "az3", Unchecked: "unreachable"},
		{Name: "az1", Storages: []StorageInfo{nfsStorage("n", "s", "/e")}, Bands: DefaultVMIDBands(), NextID: NextIDRange(0, 0)},
		{Name: "az2", Storages: []StorageInfo{nfsStorage("n", "s", "/e")}, Bands: DefaultVMIDBands(), NextID: NextIDRange(0, 0)},
	}
	first := CheckSharedExports(clusters)
	for range 20 {
		if again := CheckSharedExports(clusters); !reflect.DeepEqual(again, first) {
			t.Fatalf("findings changed between runs:\n%v\n%v", first, again)
		}
	}
	if first[0].Kind != FindingUnchecked {
		t.Errorf("unchecked findings do not lead: %v", first)
	}
}
