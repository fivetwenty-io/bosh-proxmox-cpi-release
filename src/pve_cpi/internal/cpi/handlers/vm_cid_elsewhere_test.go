package handlers_test

// Handler-level pins for a VMID that is absent from the cluster a request
// talks to. A bare CID, and a CID carrying the unknown stamp, keep the answer
// each handler gave a missing VM before CIDs carried a cluster stamp. A
// stamped CID whose stamp is this cluster's, or one of its declared aliases,
// keeps it too. A stamped CID that names another cluster must never be
// answered as gone, because its VM may still run there: the mutations refuse,
// has_vm answers false with an error log, and delete_snapshot answers success
// with a warning. A failed read of the cluster identity is retriable.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

const (
	// elsewhereStamp is a cluster stamp that is not testClusterStamp, so a
	// CID carrying it names another cluster.
	elsewhereStamp = "99aabbcc"
	// elsewhereToken is the per-VM token every CID in this file carries.
	elsewhereToken = "4e5f6a7b"
)

// absentCIDForms are the CID forms the table runs for each handler, by the
// cluster stamp they carry. elsewhere is the only one that names another
// cluster.
var absentCIDForms = []struct {
	name      string
	stamp     string // "" for a bare CID
	elsewhere bool
}{
	{name: "bare", stamp: ""},
	{name: "unknown stamp", stamp: "00000000"},
	{name: "this cluster", stamp: testClusterStamp},
	{name: "other cluster", stamp: elsewhereStamp, elsewhere: true},
}

// absentCID is the CID for vmid in the given form.
func absentCID(vmid, stamp string) string {
	if stamp == "" {
		return vmid
	}
	return vmid + "-" + stamp + "-" + elsewhereToken
}

// lockedBuffer is a log sink handlers may write from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records returns every JSON log record written so far.
func (b *lockedBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// countAt counts the records at level whose message contains substr.
func (b *lockedBuffer) countAt(t *testing.T, level, substr string) int {
	t.Helper()
	n := 0
	for _, rec := range b.records(t) {
		msg, _ := rec["msg"].(string)
		if rec["level"] == level && strings.Contains(msg, substr) {
			n++
		}
	}
	return n
}

// absentFixture is a one-node cluster on which no VM 100 or 101 exists. The
// index lists only the guests in hint, and every config probe answers
// not-found. It records every call that would act on a VM.
type absentFixture struct {
	mu        sync.Mutex
	certErr   error
	certReads int
	hint      []json.RawMessage
	removed   bool
	writes    []string
	logs      lockedBuffer
}

func (f *absentFixture) record(op string) {
	f.mu.Lock()
	f.writes = append(f.writes, op)
	f.mu.Unlock()
}

func (f *absentFixture) acted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.writes...)
	if f.removed {
		out = append(out, "agent.Remove")
	}
	return out
}

func (f *absentFixture) stampReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.certReads
}

// absentQEMU answers the reads a handler makes of a VM that does not exist
// with not-found, and records an attach.
type absentQEMU struct {
	*mockQEMUService
	f *absentFixture
}

func (q absentQEMU) ListSnapshots(context.Context, string, int) ([]map[string]any, error) {
	return nil, sdkerrors.ErrNotFound
}

func (q absentQEMU) AttachDisk(context.Context, string, int, string, string, *qemu.AttachOpts) (string, error) {
	q.f.record("AttachDisk")
	return "", sdkerrors.ErrNotFound
}

// deps builds the fixture's Deps. cfg is changed by edit before use.
func (f *absentFixture) deps(t *testing.T, edit func(*config.CPIConfig)) handlers.Deps {
	t.Helper()
	notFound := func(context.Context, string, int) (map[string]any, error) {
		return nil, sdkerrors.ErrNotFound
	}
	cfg := testConfig()
	cfg.DetachedDiskStrategy = "free"
	if edit != nil {
		edit(cfg)
	}
	logger, err := log.NewLogger("debug", &f.logs)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	nodesSvc := &mockNodesService{
		certificatesInfoFn: func(context.Context, string) (*nodes.ListCertificatesInfoResponse, error) {
			f.mu.Lock()
			f.certReads++
			certErr := f.certErr
			f.mu.Unlock()
			if certErr != nil {
				return nil, certErr
			}
			return testCertificatesInfo(), nil
		},
		deleteQemuFn: func(context.Context, string, string, *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			f.record("DeleteQemu")
			return &nodes.DeleteQemuResponse{}, nil
		},
		updateQemuConfigFn: func(context.Context, string, string, *nodes.UpdateQemuConfigParams) error {
			f.record("UpdateQemuConfig")
			return nil
		},
		createQemuStatusRebootFn: func(context.Context, string, string, *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
			f.record("Reboot")
			return rebootRawUPID("UPID:node:reboot-task"), nil
		},
	}
	return handlers.Deps{
		Config: cfg,
		PVE: &mockPVEClient{
			qemuSvc: absentQEMU{f: f, mockQEMUService: &mockQEMUService{
				configFn: notFound,
				stopFn: func(context.Context, string, int) (string, error) {
					f.record("Stop")
					return "UPID:node:stop-task", nil
				},
				detachDiskFn: func(context.Context, string, int, string) error {
					f.record("DetachDisk")
					return nil
				},
			}},
			nodesSvc:   nodesSvc,
			tasksSvc:   &mockTasksService{},
			storageSvc: &mockStorageService{},
			clusterSvc: &mockClusterSvc{
				listResourcesFn: func(context.Context, *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
					resp := cluster.ListResourcesResponse(append([]json.RawMessage(nil), f.hint...))
					return &resp, nil
				},
			},
			poolsSvc: &noopPoolService{},
		},
		Agent: &mockAgentService{removeFn: func(context.Context, string, int) error {
			f.mu.Lock()
			f.removed = true
			f.mu.Unlock()
			return nil
		}},
		Logger: logger,
	}
}

// requireElsewhereRefusal checks err is the non-retriable refusal for a CID
// that names another cluster, and that it names the escapes.
func requireElsewhereRefusal(t *testing.T, err error, cid string) {
	t.Helper()
	if err == nil {
		t.Fatal("want the cluster-mismatch refusal, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Fatalf("error type: want %s, got %v", cpierrors.TypeCloud, err)
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) || cpiErr.OkToRetry() {
		t.Errorf("the refusal must not be retriable: %v", err)
	}
	for _, want := range []string{
		"refusing to answer for CID " + cid,
		"the CID names another cluster (cluster-mismatch)",
		"pve.cluster_stamp_aliases",
		`"Delete VM reference"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err.Error(), want)
		}
	}
}

// requireRetriable checks err is retriable, which is the answer for a CID
// whose cluster could not be decided.
func requireRetriable(t *testing.T, err error) {
	t.Helper()
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("error = %v, want a retriable error while the cluster identity is unread", err)
	}
}

// absentHandlerCase runs one handler against the absent fixture. It returns
// the handler's answer.
type absentHandlerCase struct {
	name string
	vmid string
	call func(t *testing.T, deps handlers.Deps, cid string) (any, error)
	// absent checks the answer a CID that names no other cluster gets, which
	// is the answer the handler gave a missing VM before.
	absent func(t *testing.T, result any, err error)
	// elsewhere checks the answer a CID that names another cluster gets.
	elsewhere func(t *testing.T, f *absentFixture, cid string, result any, err error)
	// readFailure checks the answer when the cluster identity cannot be read.
	readFailure func(t *testing.T, result any, err error)
}

func absentHandlerCases() []absentHandlerCase {
	refuses := func(t *testing.T, _ *absentFixture, cid string, _ any, err error) {
		t.Helper()
		requireElsewhereRefusal(t, err, cid)
	}
	retriable := func(t *testing.T, _ any, err error) {
		t.Helper()
		requireRetriable(t, err)
	}
	vmNotFound := func(t *testing.T, _ any, err error) {
		t.Helper()
		if !cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
			t.Fatalf("error = %v, want VMNotFound", err)
		}
	}
	succeeds := func(t *testing.T, result any, err error) {
		t.Helper()
		if err != nil || result != nil {
			t.Fatalf("answer = (%v, %v), want success", result, err)
		}
	}
	return []absentHandlerCase{
		{
			name: "has_vm", vmid: "101",
			call: func(_ *testing.T, deps handlers.Deps, cid string) (any, error) {
				return handlers.HandleHasVM(deps).Handle(context.Background(), marshalArgs(cid), ownRPC)
			},
			absent: func(t *testing.T, result any, err error) {
				t.Helper()
				if err != nil || result != false {
					t.Fatalf("has_vm = (%v, %v), want false", result, err)
				}
			},
			elsewhere: func(t *testing.T, f *absentFixture, _ string, result any, err error) {
				t.Helper()
				if err != nil || result != false {
					t.Fatalf("has_vm = (%v, %v), want false", result, err)
				}
				if f.logs.countAt(t, "ERROR", "the CID names another cluster") != 1 {
					t.Errorf("has_vm did not log the other cluster at error level: %v", f.logs.records(t))
				}
			},
			readFailure: retriable,
		},
		{
			name: "delete_vm", vmid: "101",
			call: func(_ *testing.T, deps handlers.Deps, cid string) (any, error) {
				return handlers.HandleDeleteVM(deps).Handle(context.Background(), marshalArgs(cid), ownRPC)
			},
			absent:      succeeds,
			elsewhere:   refuses,
			readFailure: retriable,
		},
		{
			name: "reboot_vm", vmid: "101",
			call: func(_ *testing.T, deps handlers.Deps, cid string) (any, error) {
				return handlers.HandleRebootVM(deps).Handle(context.Background(), marshalArgs(cid), ownRPC)
			},
			absent:      vmNotFound,
			elsewhere:   refuses,
			readFailure: retriable,
		},
		{
			name: "set_vm_metadata", vmid: "101",
			call: func(_ *testing.T, deps handlers.Deps, cid string) (any, error) {
				metadata := map[string]any{"director": "bosh", "deployment": "cf", "job": "router", "index": "0", "id": "6f3c"}
				return handlers.HandleSetVMMetadata(deps).Handle(context.Background(), marshalArgs(cid, metadata), ownRPC)
			},
			absent:      vmNotFound,
			elsewhere:   refuses,
			readFailure: retriable,
		},
		{
			name: "attach_disk", vmid: "100",
			call: func(t *testing.T, deps handlers.Deps, cid string) (any, error) {
				return handlers.HandleAttachDisk(deps).Handle(context.Background(), attachArgs(t, cid, diskCID), ownRPC)
			},
			absent: func(t *testing.T, _ any, err error) {
				t.Helper()
				// attach_disk has no answer of its own for a missing VM. The
				// attach fails further on, and only the refusal is new.
				if err != nil && strings.Contains(err.Error(), "cluster-mismatch") {
					t.Fatalf("attach_disk refused a CID that names this cluster or none: %v", err)
				}
			},
			elsewhere:   refuses,
			readFailure: retriable,
		},
		{
			name: "detach_disk", vmid: "100",
			call: func(t *testing.T, deps handlers.Deps, cid string) (any, error) {
				return handlers.HandleDetachDisk(deps).Handle(context.Background(), detachArgs(t, cid, diskCID), ownRPC)
			},
			absent:      succeeds,
			elsewhere:   refuses,
			readFailure: retriable,
		},
		{
			name: "delete_snapshot", vmid: "100",
			call: func(_ *testing.T, deps handlers.Deps, cid string) (any, error) {
				return handlers.HandleDeleteSnapshot(deps).Handle(context.Background(), marshalArgs(cid+":bosh-1234567890-abcd1234"), ownRPC)
			},
			absent: succeeds,
			elsewhere: func(t *testing.T, f *absentFixture, _ string, result any, err error) {
				t.Helper()
				if err != nil || result != nil {
					t.Fatalf("delete_snapshot = (%v, %v), want success", result, err)
				}
				if f.logs.countAt(t, "WARN", "the snapshot CID names another cluster") != 1 {
					t.Errorf("delete_snapshot did not warn about the other cluster: %v", f.logs.records(t))
				}
			},
			readFailure: func(t *testing.T, result any, err error) {
				t.Helper()
				if err != nil || result != nil {
					t.Fatalf("delete_snapshot = (%v, %v), want success, because nothing is here to remove", result, err)
				}
			},
		},
	}
}

// TestAbsentVMID_AnswersByCIDCluster runs every handler the absent-VMID rule
// touches against each CID form, in both guard modes.
func TestAbsentVMID_AnswersByCIDCluster(t *testing.T) {
	t.Parallel()
	for _, hc := range absentHandlerCases() {
		for _, form := range absentCIDForms {
			for _, mode := range guardModes {
				for _, stale := range []bool{false, true} {
					name := hc.name + "/" + form.name + "/" + mode
					if stale {
						name += "/stale row"
					}
					t.Run(name, func(t *testing.T) {
						t.Parallel()
						runAbsentCase(t, hc, form.stamp, form.elsewhere, mode, stale)
					})
				}
			}
		}
	}
}

// staleRow is an index row that still places vmid on the fixture's node with
// no tags, so the handler finds the VM, the guard reads its config, and the
// config probe of every member then proves it absent.
func staleRow(vmid string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"vmid": json.Number(vmid), "node": "pve-node1", "type": "qemu", "tags": ""})
	return raw
}

// runAbsentCase runs one handler against the absent fixture with a CID
// carrying stamp. With stale set, the VMID is reached through a stale index
// row, which takes the guard's proven-absence path instead of the lookup's.
func runAbsentCase(t *testing.T, hc absentHandlerCase, stamp string, elsewhere bool, mode string, stale bool) {
	t.Helper()
	f := &absentFixture{}
	if stale {
		f.hint = []json.RawMessage{staleRow(hc.vmid)}
	}
	deps := asDirector(f.deps(t, nil), mode)
	cid := absentCID(hc.vmid, stamp)
	result, err := hc.call(t, deps, cid)
	if elsewhere {
		hc.elsewhere(t, f, cid, result, err)
		if acted := f.acted(); len(acted) > 0 {
			t.Errorf("%s acted on a VM the CID places on another cluster: %v", hc.name, acted)
		}
	} else {
		hc.absent(t, result, err)
	}
	// Only a CID with a real stamp that is not an alias reads the live
	// identity, and it reads it at most once. attach_disk goes on past an
	// absence it cannot answer for, so only its upper bound is pinned.
	wantReads := 0
	if stamp != "" && stamp != "00000000" {
		wantReads = 1
	}
	switch got := f.stampReads(); {
	case hc.name == "attach_disk" && !elsewhere:
		if got > wantReads {
			t.Errorf("attach_disk read the cluster identity %d times, want at most %d", got, wantReads)
		}
	case got != wantReads:
		t.Errorf("%s read the cluster identity %d times, want %d", hc.name, got, wantReads)
	}
}

// TestAbsentVMID_DeleteVMKeepsTheAgentRecordForAnotherCluster pins that the
// refusal comes before delete_vm's best-effort agent cleanup, which would
// otherwise remove the registry or cloud-init state of a VM that still runs
// on the cluster the CID names.
func TestAbsentVMID_DeleteVMKeepsTheAgentRecordForAnotherCluster(t *testing.T) {
	t.Parallel()
	f := &absentFixture{}
	cid := absentCID("101", elsewhereStamp)
	_, err := handlers.HandleDeleteVM(f.deps(t, nil)).Handle(context.Background(), marshalArgs(cid), ownRPC)
	requireElsewhereRefusal(t, err, cid)
	f.mu.Lock()
	removed := f.removed
	f.mu.Unlock()
	if removed {
		t.Error("delete_vm removed the agent state of a VM the CID places on another cluster")
	}

	own := &absentFixture{}
	if _, err := handlers.HandleDeleteVM(own.deps(t, nil)).Handle(context.Background(), marshalArgs(absentCID("101", testClusterStamp)), ownRPC); err != nil {
		t.Fatalf("delete_vm on this cluster's absent VM = %v, want success", err)
	}
	own.mu.Lock()
	removed = own.removed
	own.mu.Unlock()
	if !removed {
		t.Error("delete_vm skipped the agent cleanup for an absent VM of this cluster")
	}
}

// TestAbsentVMID_AliasNamesThisCluster pins that a stamp listed in
// cluster_stamp_aliases is this cluster, so an absent VM keeps the answer a
// missing VM gets, with no read of the live identity.
func TestAbsentVMID_AliasNamesThisCluster(t *testing.T) {
	t.Parallel()
	for _, hc := range absentHandlerCases() {
		t.Run(hc.name, func(t *testing.T) {
			t.Parallel()
			f := &absentFixture{}
			deps := f.deps(t, func(cfg *config.CPIConfig) { cfg.ClusterStampAliases = []string{elsewhereStamp} })
			result, err := hc.call(t, deps, absentCID(hc.vmid, elsewhereStamp))
			hc.absent(t, result, err)
			if got := f.stampReads(); got != 0 {
				t.Errorf("%s read the cluster identity %d times for an alias, want 0", hc.name, got)
			}
		})
	}
}

// TestAbsentVMID_IdentityReadFailure pins the answer when the live identity
// cannot be read. Nothing is decided, so the mutations and has_vm are
// retriable, and delete_snapshot, which has nothing here to remove, warns and
// succeeds. A bare CID and the unknown stamp never read, so a broken read
// leaves their answers alone.
func TestAbsentVMID_IdentityReadFailure(t *testing.T) {
	t.Parallel()
	for _, hc := range absentHandlerCases() {
		t.Run(hc.name+"/stamped", func(t *testing.T) {
			t.Parallel()
			f := &absentFixture{certErr: errors.New("connection refused")}
			result, err := hc.call(t, f.deps(t, nil), absentCID(hc.vmid, elsewhereStamp))
			hc.readFailure(t, result, err)
			if acted := f.acted(); len(acted) > 0 {
				t.Errorf("%s acted while the CID's cluster was undecided: %v", hc.name, acted)
			}
			if hc.name == "delete_snapshot" && f.logs.countAt(t, "WARN", "the cluster identity could not be read") != 1 {
				t.Errorf("delete_snapshot did not warn about the failed read: %v", f.logs.records(t))
			}
		})
		for _, stamp := range []string{"", "00000000"} {
			t.Run(hc.name+"/"+orBare(stamp), func(t *testing.T) {
				t.Parallel()
				f := &absentFixture{certErr: errors.New("connection refused")}
				result, err := hc.call(t, f.deps(t, nil), absentCID(hc.vmid, stamp))
				hc.absent(t, result, err)
			})
		}
	}
}

func orBare(stamp string) string {
	if stamp == "" {
		return "bare"
	}
	return stamp
}

// TestAbsentVMID_RefusalHintsAtARotatedCA pins the hint. Guests on this
// cluster that carry the CID's cluster tag are what a root CA rotation leaves
// behind, so the refusal counts them and names the alias key. They are never
// a match, so the answer is still the refusal.
func TestAbsentVMID_RefusalHintsAtARotatedCA(t *testing.T) {
	t.Parallel()
	row := func(vmid int, tags string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"vmid": vmid, "node": "pve-node1", "type": "qemu", "tags": tags})
		return raw
	}
	f := &absentFixture{hint: []json.RawMessage{
		row(102, "bosh-cpi;bosh-cluster--"+elsewhereStamp+";bosh-cid--11111111"),
		row(103, "bosh-cpi;BOSH-CLUSTER--"+strings.ToUpper(elsewhereStamp)+";bosh-cid--22222222"),
		row(104, "bosh-cpi;bosh-cluster--"+testClusterStamp+";bosh-cid--33333333"),
	}}
	cid := absentCID("101", elsewhereStamp)
	_, err := handlers.HandleRebootVM(f.deps(t, nil)).Handle(context.Background(), marshalArgs(cid), ownRPC)
	requireElsewhereRefusal(t, err, cid)
	for _, want := range []string{
		"this cluster's stamp is " + testClusterStamp,
		"2 guests on this cluster carry the tag bosh-cluster--" + elsewhereStamp,
		"add " + elsewhereStamp + " to pve.cluster_stamp_aliases",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err.Error(), want)
		}
	}

	none := &absentFixture{}
	_, err = handlers.HandleRebootVM(none.deps(t, nil)).Handle(context.Background(), marshalArgs(cid), ownRPC)
	requireElsewhereRefusal(t, err, cid)
	if strings.Contains(err.Error(), "guests on this cluster carry") {
		t.Errorf("refusal %q hints at guests that do not exist", err.Error())
	}
}
