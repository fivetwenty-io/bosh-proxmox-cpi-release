package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	cs "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	ns "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

const findingAllocationID = "12345678-1234-4234-8234-123456789abc"

// grantDeniedAuditClient proves visibility fails on a typed missing grant.
type grantDeniedAuditClient struct{ *allocationAuditClient }

func (*grantDeniedAuditClient) StorageAuditVisibility(context.Context) error {
	return &pve.AuditVisibilityError{Privilege: "VM.Audit", Path: "/vms"}
}

// blindAuditClient hides StorageAuditVisibility behind the plain Client
// interface, so the audit cannot prove visibility at all.
type blindAuditClient struct{ pve.Client }

func findingMarker(t *testing.T, agent string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(agent))
	marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Kind: "vm", Namespace: "director", AllocationID: findingAllocationID, AgentSHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatal(err)
	}
	return marker
}

func findingDiskProvenance(t *testing.T, vmid int, node string) string {
	t.Helper()
	volume := fmt.Sprintf("a:%d/vm-%d-disk-1.qcow2", vmid, vmid)
	entry := pve.DiskAllocationProvenance{Version: 1, AllocationID: findingAllocationID, AllocationNamespace: "director", Volid: volume, Node: node, Backing: "nfs://nas/a"}
	raw, err := json.Marshal(map[string]pve.DiskAllocationProvenance{fmt.Sprintf("bpd-%d", vmid): entry})
	if err != nil {
		t.Fatal(err)
	}
	description, err := pve.RenderSentinel("", map[string]json.RawMessage{"bosh_disk_allocations": raw})
	if err != nil {
		t.Fatal(err)
	}
	return description
}

func twoNodeCluster(c *allocationAuditClient, status ...map[string]any) {
	c.clusterRead = &allocationAuditCluster{Service: c.idFakeClient.Cluster(), members: []string{"pve1", "pve2"}, status: status}
	if c.nodesRead.guestNodes == nil {
		c.nodesRead.guestNodes = map[int]string{}
	}
}

func findingDisk(backing string, volume string) aj.Record {
	return aj.Record{ID: findingAllocationID, Namespace: "director", Kind: allocationKindDisk, State: aj.Observed, Steps: []aj.Step{{ID: "owned", State: aj.Observed, Target: aj.Target{Node: "pve1", Storage: "a", Backing: backing}, VolIDs: []string{volume}}}}
}

// TestAllocationAuditFindingsNameWhatTheySaw drives each finding site through
// the fakes and pins that its text names the allocation, VM, node, storage,
// or volume it observed, keeps its original leading phrase, and lands in the
// right list. Each expectation is a substring, so an added hint or a sorted
// neighbour never breaks it.
func TestAllocationAuditFindingsNameWhatTheySaw(t *testing.T) {
	sharedBacking := pve.StorageInfo{Name: "a", Type: "nfs", Server: "nas", Export: "/a", Content: "images", Shared: true}.BackingKey()
	volume := "a:123/vm-123-disk-0.qcow2"
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, deps *Deps, c *allocationAuditClient) []aj.Record
		list    string
		want    string
		vmScan  bool
		records bool
	}{
		{name: "visibility grant missing", list: "vm_scan_issues", vmScan: true,
			setup: func(_ *testing.T, deps *Deps, c *allocationAuditClient) []aj.Record {
				deps.PVE = &grantDeniedAuditClient{c}
				return nil
			},
			want: "cluster-wide VM and storage audit visibility is unproven: allocation audit requires VM.Audit at /vms"},
		{name: "visibility unprovable", list: "vm_scan_issues", vmScan: true,
			setup: func(_ *testing.T, deps *Deps, c *allocationAuditClient) []aj.Record {
				deps.PVE = blindAuditClient{c}
				return nil
			},
			want: "cluster-wide VM and storage audit visibility is unproven: PVE client cannot prove audit visibility (CPI defect)"},
		{name: "enumeration failed", list: "vm_scan_issues", vmScan: true,
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				twoNodeCluster(c)
				c.nodesRead.qemuFailure = map[string]error{"pve2": errors.New("opaque listing secret-password")}
				return nil
			},
			want: "cluster VM enumeration failed: could not list guests on node(s) pve2"},
		{name: "offline node", list: "vm_scan_issues", vmScan: true,
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				twoNodeCluster(c, map[string]any{"type": "cluster", "quorate": 1}, map[string]any{"type": "node", "name": "pve1", "online": 1}, map[string]any{"type": "node", "name": "pve2", "online": 0})
				return nil
			},
			want: "some cluster nodes could not be inspected: pve2 (reported offline by /cluster/status)"},
		{name: "config read failed", list: "vm_scan_issues", vmScan: true,
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.configs[123] = map[string]any{}
				c.configFailure = map[int]error{123: sdkerrors.ParseAPIError(500, []byte(`{"message":"unable to read VM config"}`))}
				return nil
			},
			want: "VM 123 configuration could not be inspected on pve1: HTTP 500: unable to read VM config"},
		{name: "config empty", list: "vm_scan_issues", vmScan: true,
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.configs[123] = map[string]any{}
				c.emptyConfigs = map[int]bool{123: true}
				return nil
			},
			want: "VM 123 configuration could not be inspected on pve1: PVE returned an empty configuration"},
		{name: "malformed allocation provenance", list: "vm_scan_issues", vmScan: true,
			setup: func(t *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				marker := findingMarker(t, "agent")
				c.configs[123] = map[string]any{"description": marker + marker}
				return nil
			},
			want: "VM 123 has malformed allocation provenance on pve1: ambiguous storage allocation provenance"},
		{name: "malformed disk provenance", list: "issues",
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.configs[123] = map[string]any{"description": `<!--BOSH:{"bosh_disk_allocations":{"bpd-x":{"version":1},"bpd-x":{}}}-->`}
				return nil
			},
			want: "VM 123 has malformed disk provenance on pve1: "},
		{name: "disk provenance carrier not an object", list: "issues",
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.configs[123] = map[string]any{"description": `<!--BOSH:{"bosh_parked_disks":null}-->`}
				return nil
			},
			want: "VM 123 has malformed disk provenance on pve1: carrier bosh_parked_disks is not an object"},
		{name: "disk provenance key malformed", list: "issues",
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.configs[123] = map[string]any{"description": `<!--BOSH:{"bosh_parked_disks":{"bpd-x":{"allocation_id":"not-a-uuid","allocation_namespace":"director","disk_cid":"x","parked_at":"now","node":"pve1"}}}-->`}
				return nil
			},
			want: `VM 123 has malformed disk provenance on pve1: key "bpd-x": malformed parked allocation provenance`},
		{name: "storage definitions failed", list: "issues",
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.storageRead.failure = sdkerrors.ParseAPIError(403, []byte(`{"message":"Permission check failed (/storage, Datastore.Audit)"}`))
				return nil
			},
			want: "storage definitions could not be inspected: HTTP 403: Permission check failed (/storage, Datastore.Audit)"},
		{name: "storage definition malformed", list: "issues",
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.storageRead.definitions = append(c.storageRead.definitions, json.RawMessage(`{"type":"nfs"}`))
				return nil
			},
			want: "a storage definition was malformed: entry 1 has no storage name"},
		{name: "storage definition duplicated", list: "issues",
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.storageRead.definitions = append(c.storageRead.definitions, c.storageRead.definitions[0])
				return nil
			},
			want: `storage definitions contain a duplicate identity: storage "a"`},
		{name: "historical backing changed", list: "issues", records: true,
			setup: func(*testing.T, *Deps, *allocationAuditClient) []aj.Record {
				return []aj.Record{findingDisk("nfs://other/a", volume)}
			},
			want: `historical mutation backing for "a" changed or disappeared: allocation ` + findingAllocationID + " step owned on pve1 recorded nfs://other/a, current " + sharedBacking},
		{name: "historical plan undecodable", list: "issues", records: true,
			setup: func(*testing.T, *Deps, *allocationAuditClient) []aj.Record {
				return []aj.Record{findingDisk(sharedBacking, volume)}
			},
			want: "historical allocation plan could not be decoded: allocation " + findingAllocationID},
		{name: "node enumeration failed", list: "issues",
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.nodesRead.nodesFailure = sdkerrors.ParseAPIError(500, []byte(`{"message":"cluster not ready"}`))
				return nil
			},
			want: "storage audit node enumeration failed: HTTP 500: cluster not ready"},
		{name: "node entry malformed", list: "issues",
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.nodesRead.nodeNames = []string{"pve1", ""}
				return nil
			},
			want: "storage audit node entry malformed: entry 1 has no node name"},
		{name: "historical storage absent", list: "issues", records: true,
			setup: func(*testing.T, *Deps, *allocationAuditClient) []aj.Record {
				record := findingDisk("", "gone:vm-100-disk-0")
				record.Steps[0].Target.Storage = "gone"
				return []aj.Record{record}
			},
			want: `historical storage "gone" is absent from definitions: recorded on pve1`},
		{name: "content listing failed on one node", list: "issues",
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.nodesRead.nodeNames = []string{"pve1", "pve2"}
				c.nodesRead.failureByNode = map[string]error{"pve2": errors.New("transport response secret-password")}
				return nil
			},
			want: `storage "a" on node "pve2" could not be inspected: unclassified error`},
		{name: "content malformed", list: "issues",
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.nodesRead.content = ns.ListStorageContentResponse{json.RawMessage(`{"volid":"b:123/vm-123-disk-0.qcow2"}`)}
				return nil
			},
			want: `storage "a" on node "pve1" returned malformed content: entry 0 has no volid on this storage`},
		{name: "known volume disagrees", list: "issues", records: true,
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.nodesRead.content = ns.ListStorageContentResponse{json.RawMessage(`{"volid":"` + volume + `"}`)}
				return []aj.Record{findingDisk("nfs://other/a", volume)}
			},
			want: `known volume ` + volume + ` on storage "a" on node "pve1" disagrees with recorded physical target: allocation ` + findingAllocationID + " recorded pve1 (backing_changed)"},
		{name: "multiple holders", list: "conflicts", records: true,
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.configs[201] = map[string]any{"scsi1": volume}
				c.configs[202] = map[string]any{"scsi1": volume}
				return []aj.Record{findingDisk(sharedBacking, volume)}
			},
			want: "disk allocation " + findingAllocationID + " has multiple active holders or duplicate stable tokens: volume " + volume + " on pve1 (VM 201), volume " + volume + " on pve1 (VM 202)"},
		{name: "marker missing from recorded VM", list: "conflicts", records: true,
			setup: func(_ *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.configs[123] = map[string]any{"description": ""}
				return []aj.Record{{ID: findingAllocationID, Namespace: "director", Kind: "vm", State: aj.Observed, Steps: []aj.Step{{Target: aj.Target{Node: "pve1", VMID: 123}}}}}
			},
			want: "recorded VM target for allocation " + findingAllocationID + " lacks matching ownership provenance: VM 123 on pve1 carries no allocation marker"},
		{name: "disk provenance on another node", list: "conflicts",
			setup: func(t *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.configs[201] = map[string]any{"description": findingDiskProvenance(t, 201, "pve1")}
				twoNodeCluster(c)
				c.nodesRead.guestNodes[201] = "pve2"
				return nil
			},
			want: "disk ownership provenance disagrees with actual holder node: disk allocation " + findingAllocationID + " (volume a:201/vm-201-disk-1.qcow2) held by VM 201 on pve2, provenance names pve1; a migration outside BOSH is the usual cause"},
		{name: "unknown allocation", list: "conflicts",
			setup: func(t *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.configs[123] = map[string]any{"description": findingMarker(t, "agent")}
				return nil
			},
			want: "remote allocation " + findingAllocationID + " is missing from retained journal; audit required: observed VM 123 on pve1"},
		{name: "journal identity differs", list: "conflicts", records: true,
			setup: func(t *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.configs[123] = map[string]any{"description": findingMarker(t, "agent")}
				return []aj.Record{{ID: findingAllocationID, Namespace: "director", Kind: allocationKindDisk, State: aj.Observed}}
			},
			want: `remote allocation ` + findingAllocationID + ` disagrees with journal identity: record kind disk in namespace "director", observed vm evidence VM 123 on pve1`},
		{name: "closed attempt", list: "conflicts", records: true,
			setup: func(t *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.configs[123] = map[string]any{"description": findingMarker(t, "agent")}
				return []aj.Record{{ID: findingAllocationID, Namespace: "director", Kind: "vm", State: aj.Observed, AgentID: "agent", Attempts: []aj.Attempt{{Number: 0}, {Number: 1}}, Steps: []aj.Step{{Attempt: 0, Target: aj.Target{Node: "pve1", VMID: 123}}}}}
			},
			want: "allocation " + findingAllocationID + " has resources from a closed attempt: VM 123 on pve1, active attempt 1"},
		{name: "VM outside recorded node", list: "conflicts", records: true,
			setup: func(t *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.configs[123] = map[string]any{"description": findingMarker(t, "agent")}
				twoNodeCluster(c)
				c.nodesRead.guestNodes[123] = "pve2"
				return []aj.Record{{ID: findingAllocationID, Namespace: "director", Kind: "vm", State: aj.Observed, AgentID: "agent", Steps: []aj.Step{{Target: aj.Target{Node: "pve1", VMID: 123}}}}}
			},
			want: "remote allocation " + findingAllocationID + " (VM 123) is outside recorded mutation targets: observed on pve2, recorded pve1 (node_mismatch); a migration outside BOSH is the usual cause"},
		{name: "agent digest differs", list: "conflicts", records: true,
			setup: func(t *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.configs[123] = map[string]any{"description": findingMarker(t, "other-agent")}
				return []aj.Record{{ID: findingAllocationID, Namespace: "director", Kind: "vm", State: aj.Observed, AgentID: "agent", Steps: []aj.Step{{Target: aj.Target{Node: "pve1", VMID: 123}}}}}
			},
			want: "VM allocation " + findingAllocationID + " has inconsistent agent provenance: VM 123 on pve1 carries a different agent digest"},
		{name: "terminal allocation", list: "conflicts", records: true,
			setup: func(t *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.configs[123] = map[string]any{"description": findingMarker(t, "agent")}
				return []aj.Record{{ID: findingAllocationID, Namespace: "director", Kind: "vm", State: aj.Deleted, AgentID: "agent", Steps: []aj.Step{{Target: aj.Target{Node: "pve1", VMID: 123}}}}}
			},
			want: "terminal allocation " + findingAllocationID + " still has remote provenance; audit required: deleted record, observed VM 123 on pve1"},
		{name: "VM-deleted allocation outside retained artifacts", list: "conflicts", records: true,
			setup: func(t *testing.T, _ *Deps, c *allocationAuditClient) []aj.Record {
				c.configs[123] = map[string]any{"description": findingMarker(t, "agent")}
				return []aj.Record{{ID: findingAllocationID, Namespace: "director", Kind: "vm", State: aj.VMDeletedRetained, AgentID: "agent", Steps: []aj.Step{{Target: aj.Target{Node: "pve1", VMID: 123}}}}}
			},
			want: "VM-deleted allocation " + findingAllocationID + " has provenance outside retained artifacts: observed VM 123 on pve1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, j, c := auditFixture(t)
			records := tc.setup(t, &deps, c)
			var report StorageAllocationAudit
			var err error
			if tc.records {
				report, err = auditStorageAllocationRecords(context.Background(), deps, records, []string{"pve1"})
			} else {
				report, err = AuditStorageAllocations(context.Background(), deps, j, []string{"pve1"})
			}
			if err != nil {
				t.Fatal(err)
			}
			lists := map[string][]string{"issues": report.Issues, "vm_scan_issues": report.VMScanIssues, "conflicts": report.Conflicts}
			if !slices.ContainsFunc(lists[tc.list], func(finding string) bool { return strings.Contains(finding, tc.want) }) {
				t.Fatalf("%s lacks %q:\n%s", tc.list, tc.want, strings.Join(lists[tc.list], "\n"))
			}
			if tc.vmScan && (report.VMScanComplete || !slices.Equal(report.VMScanIssues, intersect(report.Issues, report.VMScanIssues))) {
				t.Fatalf("VM-scan issue not mirrored into both lists: %+v", report)
			}
			if !tc.vmScan && tc.list != "vm_scan_issues" && slices.ContainsFunc(report.VMScanIssues, func(finding string) bool { return strings.Contains(finding, tc.want) }) {
				t.Fatalf("storage finding leaked into vm_scan_issues: %v", report.VMScanIssues)
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "secret-password") {
				t.Fatal("transport error text reached the report")
			}
		})
	}
}

func intersect(all, subset []string) []string {
	var out []string
	for _, item := range subset {
		if slices.Contains(all, item) {
			out = append(out, item)
		}
	}
	return out
}

// TestAllocationAuditDistinctVMsNeverCollapse is the field regression: two
// VMs whose disk provenance names a node they left produced identical
// strings, and compaction merged them into one conflict.
func TestAllocationAuditDistinctVMsNeverCollapse(t *testing.T) {
	deps, j, c := auditFixture(t)
	twoNodeCluster(c)
	c.configs[201] = map[string]any{"description": findingDiskProvenance(t, 201, "pve1")}
	c.configs[202] = map[string]any{"description": findingDiskProvenance(t, 202, "pve1")}
	c.nodesRead.guestNodes[201] = "pve2"
	c.nodesRead.guestNodes[202] = "pve2"
	report, err := AuditStorageAllocations(context.Background(), deps, j, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	moved := 0
	for _, conflict := range report.Conflicts {
		if strings.HasPrefix(conflict, "disk ownership provenance disagrees with actual holder node") {
			moved++
		}
	}
	if moved != 2 {
		t.Fatalf("expected one disk-provenance conflict per VM, got %d:\n%s", moved, strings.Join(report.Conflicts, "\n"))
	}
}

// TestAllocationAuditVMScanIssuesSortedAndCompacted pins that the VM-scan
// list gets the same ordering and deduplication as issues and conflicts, and
// that it serializes without omitempty like its siblings.
func TestAllocationAuditVMScanIssuesSortedAndCompacted(t *testing.T) {
	deps, j, c := auditFixture(t)
	c.configs[124] = map[string]any{}
	c.configs[123] = map[string]any{}
	c.emptyConfigs = map[int]bool{123: true, 124: true}
	report, err := AuditStorageAllocations(context.Background(), deps, j, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.VMScanIssues) != 2 || !slices.IsSorted(report.VMScanIssues) {
		t.Fatalf("VM-scan issues not sorted and compacted: %v", report.VMScanIssues)
	}
	cleanDeps, cleanJournal, _ := auditFixture(t)
	clean, err := AuditStorageAllocations(context.Background(), cleanDeps, cleanJournal, []string{"pve1"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(clean)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"vm_scan_issues":null`) {
		t.Fatalf("vm_scan_issues missing from a clean report: %s", encoded)
	}
}

// Compile-time guard that the fixture's storage fake still satisfies the
// SDK service it stands in for.
var _ cs.Service = (*allocationAuditStorage)(nil)
