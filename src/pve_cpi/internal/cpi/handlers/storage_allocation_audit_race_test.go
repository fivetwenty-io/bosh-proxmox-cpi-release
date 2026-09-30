package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// raceAuditClient runs hook once, just before the audit reads the
// configuration of VM vmid, so a test can change the cluster and the journal
// between the audit's first journal read and the rest of its scan. A hook that
// returns an error fails that configuration read with it.
type raceAuditClient struct {
	*allocationAuditClient
	vmid int
	once sync.Once
	hook func() error
}

func (c *raceAuditClient) QEMU() sdkqemu.Service {
	return &raceAuditQEMU{Service: c.allocationAuditClient.QEMU(), client: c}
}

type raceAuditQEMU struct {
	sdkqemu.Service
	client *raceAuditClient
}

func (q *raceAuditQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if vmid == q.client.vmid {
		var err error
		q.client.once.Do(func() { err = q.client.hook() })
		if err != nil {
			return nil, err
		}
	}
	return q.Service.Config(ctx, node, vmid)
}

// raceSibling is a create_vm for agent-a that runs beside the audit and
// follows the order the CPI uses: it acquires its record, saves each step,
// and only then clones the VM or creates the volume the step names.
type raceSibling struct {
	t      *testing.T
	j      *aj.Journal
	client *allocationAuditClient
	def    pve.StorageInfo
	handle *aj.Handle
}

func newRaceSibling(t *testing.T, j *aj.Journal, client *allocationAuditClient) *raceSibling {
	t.Helper()
	return &raceSibling{t: t, j: j, client: client, def: moveDefinition(t, string(client.storageRead.definitions[0]))}
}

// acquire writes the sibling's record.
func (s *raceSibling) acquire() {
	s.t.Helper()
	intent := movePlan(s.t, "agent-a", "pve1", map[string]pve.StorageInfo{"a": s.def}, "a:")
	fingerprint, err := storageCallerIntentFingerprint("create_vm", []json.RawMessage{json.RawMessage(`"agent-a"`)})
	if err != nil {
		s.t.Fatal(err)
	}
	intent.IntentFingerprint = fingerprint
	s.handle, err = s.j.AcquireVM(context.Background(), "agent-a", intent)
	if err != nil {
		s.t.Fatal(err)
	}
}

// step saves a Planned step in the sibling's active attempt.
func (s *raceSibling) step(method string, target aj.Target) {
	s.t.Helper()
	if _, err := storageMutationIntent(s.handle, "vm."+method, target, nil); err != nil {
		s.t.Fatal(err)
	}
}

// mark puts the allocation marker of agent on VM 500, as the clone does.
func (s *raceSibling) mark(agent string) {
	s.t.Helper()
	marker := moveMarker(s.t, s.handle.Record().ID, agent)
	s.client.mu.Lock()
	s.client.configs[500]["description"] = marker
	s.client.mu.Unlock()
}

// ephemeral saves the ephemeral volume step for VM 500 and then lists the
// allocation-named volume on pve1.
func (s *raceSibling) ephemeral() {
	s.t.Helper()
	_, suffix, err := pve.ManagedEphemeralVolumeName(s.def.Type, "raw", 500, "director", s.handle.Record().ID)
	if err != nil {
		s.t.Fatal(err)
	}
	volume := "a:" + suffix
	s.step(managedVMCallCreateVolume, aj.Target{Node: "pve1", VMID: 500, Storage: "a", Backing: s.def.BackingKey(), IntendedVolume: volume})
	s.client.nodesRead.volumesByNode = map[string][]string{"pve1": {volume}}
}

func (s *raceSibling) close() {
	s.t.Helper()
	if err := s.handle.Close(); err != nil {
		s.t.Fatal(err)
	}
}

// raceAuditFixture is the audit fixture with VM 500 listed and unmarked, and
// with hook run just before the audit reads VM 500's configuration.
func raceAuditFixture(t *testing.T, hook func(*raceSibling) error) (Deps, *aj.Journal, *raceSibling) {
	t.Helper()
	deps, j, c := auditFixture(t)
	c.configs[500] = map[string]any{}
	sibling := newRaceSibling(t, j, c)
	deps.PVE = &raceAuditClient{allocationAuditClient: c, vmid: 500, hook: func() error { return hook(sibling) }}
	return deps, j, sibling
}

func TestAllocationAuditSettlesASiblingCreatedDuringTheScan(t *testing.T) {
	cases := []struct {
		name string
		// before runs ahead of the audit's first journal read.
		before func(*raceSibling)
		// during runs after that read, before the audit reads VM 500.
		during func(*raceSibling)
	}{
		{name: "record and marker written during the scan",
			during: func(s *raceSibling) {
				s.acquire()
				s.step(managedVMCallClone, aj.Target{Node: "pve1", VMID: 500})
				s.mark("agent-a")
				s.close()
			}},
		{name: "ephemeral volume listed after the scan",
			during: func(s *raceSibling) {
				s.acquire()
				s.step(managedVMCallClone, aj.Target{Node: "pve1", VMID: 500})
				s.mark("agent-a")
				s.ephemeral()
				s.close()
			}},
		{name: "record read before its first step (not_in_step)",
			before: func(s *raceSibling) {
				s.acquire()
			},
			during: func(s *raceSibling) {
				s.step(managedVMCallClone, aj.Target{Node: "pve1", VMID: 500})
				s.mark("agent-a")
				s.close()
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("audit", func(t *testing.T) {
				deps, j, sibling := raceAuditFixture(t, func(s *raceSibling) error { tc.during(s); return nil })
				if tc.before != nil {
					tc.before(sibling)
				}
				report, err := AuditStorageAllocations(context.Background(), deps, j, []string{"pve1"})
				if err != nil {
					t.Fatal(err)
				}
				if !report.Complete || !report.VMScanComplete || len(report.Conflicts) != 0 || len(report.Issues) != 0 {
					t.Fatalf("sibling created during the scan was not settled: complete=%t vm_scan=%t conflicts=%q issues=%q", report.Complete, report.VMScanComplete, report.Conflicts, report.Issues)
				}
			})
			t.Run("admission", func(t *testing.T) {
				deps, j, sibling := raceAuditFixture(t, func(s *raceSibling) error { tc.during(s); return nil })
				if tc.before != nil {
					tc.before(sibling)
				}
				if err := admitStorageVMAllocation(context.Background(), deps, j, []string{"pve1"}, "agent-b"); err != nil {
					t.Fatalf("create_vm refused beside a sibling create: %v", err)
				}
			})
		})
	}
}

func TestAllocationAuditSkipsAGuestDeletedDuringTheScan(t *testing.T) {
	const issue = "VM 124 configuration could not be inspected on pve1: HTTP 500: Configuration file 'nodes/pve1/qemu-server/124.conf' does not exist"
	cases := []struct {
		name string
		// failListing fails the listing that follows the configuration read.
		failListing bool
		wantIssues  []string
	}{
		{name: "guest gone from the second listing"},
		{name: "second listing fails", failListing: true, wantIssues: []string{issue}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := func(t *testing.T) (Deps, *aj.Journal) {
				t.Helper()
				deps, j, c := auditFixture(t)
				c.configs[124] = map[string]any{"virtio0": "a:124/vm-124-disk-0.qcow2"}
				deps.PVE = &raceAuditClient{allocationAuditClient: c, vmid: 124, hook: func() error {
					c.mu.Lock()
					delete(c.configs, 124)
					c.mu.Unlock()
					if tc.failListing {
						c.nodesRead.qemuFailure = map[string]error{"pve1": errors.New("node listing unavailable")}
					}
					return sdkerrors.ParseAPIError(500, []byte(`{"message":"Configuration file 'nodes/pve1/qemu-server/124.conf' does not exist"}`))
				}}
				return deps, j
			}
			t.Run("audit", func(t *testing.T) {
				deps, j := fixture(t)
				report, err := AuditStorageAllocations(context.Background(), deps, j, []string{"pve1"})
				if err != nil {
					t.Fatal(err)
				}
				if len(report.Conflicts) != 0 {
					t.Fatalf("conflicts = %q", report.Conflicts)
				}
				if len(tc.wantIssues) == 0 {
					if !report.Complete || !report.VMScanComplete || len(report.Issues) != 0 {
						t.Fatalf("deleted guest was not skipped: complete=%t vm_scan=%t issues=%q", report.Complete, report.VMScanComplete, report.Issues)
					}
					return
				}
				if report.Complete || report.VMScanComplete || !slices.Equal(report.Issues, tc.wantIssues) || !slices.Equal(report.VMScanIssues, tc.wantIssues) {
					t.Fatalf("unproven absence skipped the guest: complete=%t vm_scan=%t issues=%q vm_scan_issues=%q", report.Complete, report.VMScanComplete, report.Issues, report.VMScanIssues)
				}
			})
			t.Run("admission", func(t *testing.T) {
				deps, j := fixture(t)
				err := admitStorageVMAllocation(context.Background(), deps, j, []string{"pve1"}, "agent-b")
				if len(tc.wantIssues) == 0 && err != nil {
					t.Fatalf("create_vm refused beside a deleted guest: %v", err)
				}
				if len(tc.wantIssues) > 0 && (err == nil || !strings.Contains(err.Error(), issue)) {
					t.Fatalf("create_vm admission = %v, want the unread configuration named", err)
				}
			})
		})
	}
}

func TestAllocationAuditKeepsAConflictTheSecondReadCannotSettle(t *testing.T) {
	cases := []struct {
		name string
		// sibling writes the sibling's record during the scan and returns the
		// agent whose marker then lands on VM 500.
		sibling func(*raceSibling) string
		// rereadErr, when set, fails the audit's second journal read.
		rereadErr error
	}{
		{name: "active step names another node",
			sibling: func(s *raceSibling) string {
				s.acquire()
				s.step(managedVMCallClone, aj.Target{Node: "pve2", VMID: 500})
				return "agent-a"
			}},
		{name: "record has no step",
			sibling: func(s *raceSibling) string {
				s.acquire()
				return "agent-a"
			}},
		{name: "marker carries another agent",
			sibling: func(s *raceSibling) string {
				s.acquire()
				s.step(managedVMCallClone, aj.Target{Node: "pve1", VMID: 500})
				return "agent-other"
			}},
		{name: "second journal read fails",
			sibling: func(s *raceSibling) string {
				s.acquire()
				s.step(managedVMCallClone, aj.Target{Node: "pve1", VMID: 500})
				return "agent-a"
			},
			rereadErr: errors.New("journal directory unreadable")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, j, sibling := raceAuditFixture(t, func(s *raceSibling) error {
				agent := tc.sibling(s)
				s.mark(agent)
				s.close()
				return nil
			})
			logger, observed := log.NewObservedLogger(slog.LevelDebug)
			deps.Logger = logger
			var report StorageAllocationAudit
			var err error
			if tc.rereadErr == nil {
				report, err = AuditStorageAllocations(context.Background(), deps, j, []string{"pve1"})
			} else {
				records, listErr := j.List()
				if listErr != nil {
					t.Fatal(listErr)
				}
				report, err = auditStorageAllocationRecords(context.Background(), deps, records, func() ([]aj.Record, error) { return nil, tc.rereadErr }, []string{"pve1"})
			}
			if err != nil {
				t.Fatal(err)
			}
			id := sibling.handle.Record().ID
			conflict := "remote allocation " + id + " is missing from retained journal; audit required: observed VM 500 on pve1"
			brief := "VM 500 on pve1 carries unknown allocation " + id
			if !slices.Equal(report.Conflicts, []string{conflict}) || report.brief(conflict) != brief {
				t.Fatalf("conflicts = %q, brief = %q; want %q with brief %q", report.Conflicts, report.brief(conflict), conflict, brief)
			}
			if report.Complete || !report.VMScanComplete || len(report.Issues) != 0 {
				t.Fatalf("complete=%t vm_scan=%t issues=%q", report.Complete, report.VMScanComplete, report.Issues)
			}
			var warned []log.Entry
			for _, entry := range observed.All() {
				if entry.Level == slog.LevelWarn {
					warned = append(warned, entry)
				}
			}
			if tc.rereadErr == nil {
				if len(warned) != 0 {
					t.Fatalf("unexpected warnings: %+v", warned)
				}
				return
			}
			if len(warned) != 1 || warned[0].Attrs["unsettled_conflicts"] != int64(1) || !strings.Contains(warned[0].Attrs["error"].(string), tc.rereadErr.Error()) {
				t.Fatalf("want one Warn naming the error and one unsettled conflict, got %+v", warned)
			}
		})
	}
}
