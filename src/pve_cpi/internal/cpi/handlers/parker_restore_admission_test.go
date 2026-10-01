package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// admissionRead names one of the three reads the lifecycle guard's before
// makes to admit a parker protection restore.
type admissionRead string

const (
	admissionStorageDefinition admissionRead = "storage definition"
	admissionClusterIdentity   admissionRead = "cluster identity"
	admissionParkerConfig      admissionRead = "parker config"
	// restoreWriteControl hangs the restore write itself instead, through
	// hungRestorePVE, the case the guard already handles. The tests run it
	// beside the three reads to show what their assertions expect.
	restoreWriteControl admissionRead = "restore write control"
)

// admissionAnswer is how PVE answers the admission read a case picks out.
type admissionAnswer string

const (
	// admissionHangs never answers. The read blocks until the restore's
	// deadline ends its context and then fails with the context's error.
	admissionHangs admissionAnswer = "hung"
	// admissionFails answers at once with a PVE 500, the way a pveproxy under
	// the same stress that hangs a read also answers fast.
	admissionFails admissionAnswer = "fails"
	// admissionDisagrees answers at once with a value that disagrees with the
	// record: another backing for the storage definition, or another cluster
	// root for the cluster identity.
	admissionDisagrees admissionAnswer = "disagrees"
)

// admissionCase is one admission read and how PVE answers it.
type admissionCase struct {
	read   admissionRead
	answer admissionAnswer
}

func (a admissionCase) String() string {
	if a.read == restoreWriteControl {
		return string(a.read)
	}
	return string(a.read) + " " + string(a.answer)
}

// errAdmissionReadRefused is the PVE 500 a failing admission read answers
// with.
var errAdmissionReadRefused = &sdkerrors.APIError{HTTPCode: 500, Message: "pveproxy could not answer the read"}

// hungAdmissionPVE is hungRestorePVE with more ways for PVE to answer badly.
// Once armed, the first read of the chosen kind made on the protection
// restore's own context, after the transfer off the parker has landed, gets
// the chosen answer. A hung read blocks until that context ends and then fails
// with its error, a failing read gets a PVE 500 at once, and a disagreeing
// read gets an answer that differs from the record's. The restore's context is
// the one whose deadline is no farther away than the restore timeout, because
// putParkerProtectionBack builds it with that timeout and every other context
// in the attach carries a longer deadline or none. The answer fires once, so
// PVE answers every read after it.
type hungAdmissionPVE struct {
	*hungRestorePVE
	read    admissionRead
	answer  admissionAnswer
	timeout time.Duration

	mu         sync.Mutex
	armed      bool
	movesAtArm int
	fired      bool
	endedBy    error
}

func (c *hungAdmissionPVE) arm() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed, c.movesAtArm = true, c.client().moves
}

func (c *hungAdmissionPVE) client() *lifecycleFlowPVE { return c.lifecycleFlowPVE }

// outcome reports whether the answer fired and the error the read ended
// with, which is nil for a read that disagreed.
func (c *hungAdmissionPVE) outcome() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fired, c.endedBy
}

func (c *hungAdmissionPVE) ended(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endedBy = err
}

// intercept decides how PVE answers a read of the given kind. It returns the
// error the read must fail with, and reports disagree when the read must
// answer with a value that differs from the record's.
func (c *hungAdmissionPVE) intercept(ctx context.Context, read admissionRead) (disagree bool, err error) {
	c.mu.Lock()
	deadline, bounded := ctx.Deadline()
	fire := c.armed && read == c.read && c.client().moves > c.movesAtArm && bounded && time.Until(deadline) <= c.timeout
	if fire {
		c.armed, c.fired = false, true
	}
	c.mu.Unlock()
	if !fire {
		return false, nil
	}
	switch c.answer {
	case admissionDisagrees:
		return true, nil
	case admissionFails:
		c.ended(errAdmissionReadRefused)
		return false, errAdmissionReadRefused
	case admissionHangs:
	}
	<-ctx.Done()
	c.ended(ctx.Err())
	return false, ctx.Err()
}

func (c *hungAdmissionPVE) ClusterStorage() clusterstorage.Service {
	return hungAdmissionStorage{Service: c.client().ClusterStorage(), owner: c}
}

func (c *hungAdmissionPVE) Nodes() nodes.Service {
	return hungAdmissionNodes{hungRestoreNodes: c.hungRestorePVE.Nodes().(hungRestoreNodes), owner: c}
}

func (c *hungAdmissionPVE) QEMU() qemu.Service {
	return hungAdmissionQEMU{Service: c.client().QEMU(), owner: c}
}

type hungAdmissionStorage struct {
	clusterstorage.Service
	owner *hungAdmissionPVE
}

func (s hungAdmissionStorage) ListStorage(ctx context.Context, p *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	disagree, err := s.owner.intercept(ctx, admissionStorageDefinition)
	if err != nil {
		return nil, err
	}
	listing, err := s.Service.ListStorage(ctx, p)
	if err != nil || !disagree {
		return listing, err
	}
	return movedStorageDefinitions(listing)
}

// movedStorageDefinitions returns listing with every storage pointed somewhere
// else, so that each one names another backing.
func movedStorageDefinitions(listing *clusterstorage.ListStorageResponse) (*clusterstorage.ListStorageResponse, error) {
	moved := make(clusterstorage.ListStorageResponse, 0, len(*listing))
	for _, raw := range *listing {
		var entry map[string]any
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, err
		}
		for _, key := range []string{"server", "path"} {
			if value, ok := entry[key].(string); ok {
				entry[key] = value + "-elsewhere"
			}
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		moved = append(moved, encoded)
	}
	return &moved, nil
}

type hungAdmissionNodes struct {
	hungRestoreNodes
	owner *hungAdmissionPVE
}

func (n hungAdmissionNodes) ListQemuPending(ctx context.Context, node, vmid string) (*nodes.ListQemuPendingResponse, error) {
	return PendingFromConfigRead(ctx, n.owner.QEMU().Config, node, vmid)
}

func (n hungAdmissionNodes) ListCertificatesInfo(ctx context.Context, node string) (*nodes.ListCertificatesInfoResponse, error) {
	disagree, err := n.owner.intercept(ctx, admissionClusterIdentity)
	if err != nil {
		return nil, err
	}
	if !disagree {
		return n.hungRestoreNodes.ListCertificatesInfo(ctx, node)
	}
	// Another cluster's root, the shape of the fixture's own.
	raw, err := json.Marshal(map[string]any{"filename": "pve-root-ca.pem", "fingerprint": strings.TrimSuffix(strings.Repeat("22:", 32), ":")})
	if err != nil {
		return nil, err
	}
	r := nodes.ListCertificatesInfoResponse{raw}
	return &r, nil
}

type hungAdmissionQEMU struct {
	qemu.Service
	owner *hungAdmissionPVE
}

func (q hungAdmissionQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if vmid == q.owner.parker {
		if _, err := q.owner.intercept(ctx, admissionParkerConfig); err != nil {
			return nil, err
		}
	}
	return q.Service.Config(ctx, node, vmid)
}

// admissionCutOff is a parked, journal-managed disk whose attach moved it off
// its parker while one of the restore's admission reads got a bad answer from
// PVE.
type admissionCutOff struct {
	*cutOffRestore
	pve       *hungAdmissionPVE
	admission admissionCase
	timeout   time.Duration
}

// newAdmissionCutOff parks a fresh managed disk, arms the case's answer on one
// of the restore's admission reads, and attaches the disk again.
func newAdmissionCutOff(t *testing.T, a admissionCase, timeout time.Duration) *admissionCutOff {
	t.Helper()
	c := &admissionCutOff{cutOffRestore: newParkedRestoreDisk(t, timeout), admission: a, timeout: timeout}
	c.pve = &hungAdmissionPVE{hungRestorePVE: c.hung, read: a.read, answer: a.answer, timeout: timeout}
	c.deps.PVE = c.pve
	c.hung.forgetParkerWrites()
	if a.read == restoreWriteControl {
		c.hung.arm(true)
	} else {
		c.pve.arm()
	}
	start := time.Now()
	_, c.attachErr = HandleAttachDisk(c.deps).Handle(c.ctx, c.attachArgs, jsonrpc.Context{})
	c.elapsed = time.Since(start)
	c.hung.arm(false)
	fired, ended := c.pve.outcome()
	if a.read == restoreWriteControl {
		fired, ended = true, c.hung.outcome()
	}
	if !fired {
		t.Fatalf("the %s was never reached on the restore's context", a)
	}
	switch a.answer {
	case admissionHangs:
		if !errors.Is(ended, context.DeadlineExceeded) {
			t.Fatalf("the %s was never hung by the restore deadline (ended by %v)", a, ended)
		}
	case admissionFails:
		if !errors.Is(ended, errAdmissionReadRefused) {
			t.Fatalf("the %s did not fail with PVE's 500 (ended by %v)", a, ended)
		}
	case admissionDisagrees:
	}
	return c
}

// wantEnding is how the first call's error must say the restore ended, or ""
// for the control, whose write was sent.
func (c *admissionCutOff) wantEnding() string {
	switch {
	case c.admission.read == restoreWriteControl:
		return ""
	case c.admission.answer == admissionHangs:
		return fmt.Sprintf("was not sent because the checks before it did not finish within %s, so protection is still off", c.timeout)
	default:
		return "was not sent because the checks before it failed, so protection is still off"
	}
}

// plannedRestore returns the planned protection-on step on the parker, if the
// record holds one.
func (c *admissionCutOff) plannedRestore(t *testing.T) (aj.Step, bool) {
	t.Helper()
	record := c.record(t)
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Target.VMID == c.parker && step.State == aj.Planned && isParkerProtectionStep(record, *step) &&
			string(step.Parameters) == `{"kind":"parker_protection_on","version":1}` {
			return *step, true
		}
	}
	return aj.Step{}, false
}

func (c *admissionCutOff) cleanup(t *testing.T) error {
	t.Helper()
	_, err := CleanupStorageAllocation(c.ctx, c.deps, c.journal, []string{"n1"},
		StorageAllocationDecision{Action: "cleanup", AllocationID: c.id, DecisionID: "restore-admission-cut-off"})
	return err
}

// firstCallMessage logs what the first attach left behind and returns its
// scrubbed error text.
func (c *admissionCutOff) firstCallMessage(t *testing.T) string {
	t.Helper()
	msg := ""
	if c.attachErr != nil {
		msg = log.ScrubMessage(c.attachErr.Error())
	}
	t.Logf("attach_disk returned after %s with error: %s", c.elapsed.Round(time.Millisecond), msg)
	t.Logf("parker %d protection now %v", c.parker, c.client.state.configs[c.parker]["protection"])
	t.Logf("pools left on PVE: %v", c.client.state.pools)
	logAdmissionRecord(t, c.record(t))
	return msg
}

func logAdmissionRecord(t *testing.T, record aj.Record) {
	t.Helper()
	t.Logf("record state %s, reason %q", record.State, record.Reason)
	for i := range record.Steps {
		step := &record.Steps[i]
		t.Logf("  step %s kind %s vmid %d state %s parameters %s", step.ID, step.Kind, step.Target.VMID, step.State, string(step.Parameters))
	}
}

// decisionCause returns the error behind an adopt or cleanup refusal, which
// the storage-journal CLI may render only by class, or nil when there is none.
func decisionCause(err error) error {
	var observation *storageDecisionObservationError
	if errors.As(err, &observation) {
		return observation.cause
	}
	return nil
}

// admissionCutOffCases are the cases whose restore must be left planned: each
// read hung until the restore deadline, each read failing at once with a PVE
// 500, and the control that hangs the restore write itself.
var admissionCutOffCases = []admissionCase{
	{admissionStorageDefinition, admissionHangs},
	{admissionClusterIdentity, admissionHangs},
	{admissionParkerConfig, admissionHangs},
	{admissionStorageDefinition, admissionFails},
	{admissionClusterIdentity, admissionFails},
	{admissionParkerConfig, admissionFails},
	{restoreWriteControl, admissionHangs},
}

// TestManagedAttachRestoreAdmissionReadCutOff makes each read the lifecycle
// guard runs to admit the parker's protection restore fail, one at a time,
// either by hanging until the restore deadline ends it or by getting a PVE 500
// at once. The restore was never sent, and the parker was left unprotected by
// the transfer's own window. The attach must say the restore was not sent
// because the checks before it did not finish, rather than that an operation
// already uncertain refused it. It must not lock the guard for a read that
// could not finish, and it must leave the restore planned on the record, so
// that the next call reads the parker back before it takes the disk.
func TestManagedAttachRestoreAdmissionReadCutOff(t *testing.T) {
	t.Parallel()
	const timeout = 200 * time.Millisecond
	for _, a := range admissionCutOffCases {
		t.Run(a.String(), func(t *testing.T) {
			t.Parallel()
			c := newAdmissionCutOff(t, a, timeout)
			msg := c.firstCallMessage(t)
			if c.attachErr == nil {
				t.Fatal("attach_disk succeeded although the parker's protection was never put back")
			}
			assertRestoreCutOffError(t, c, msg)
			assertRestoreLeftPlanned(t, c)
		})
	}
}

// TestManagedAttachRestoreAdmissionAnswerDisagrees is the boundary of the
// cases above. PVE answers the restore's admission read at once, and the
// answer disagrees with the record: the storage names another backing, or the
// cluster has another root. That check finished, and what it found is not
// about the parker, so the guard must still lock itself before the restore,
// and the record must hold no planned restore.
func TestManagedAttachRestoreAdmissionAnswerDisagrees(t *testing.T) {
	t.Parallel()
	for _, read := range []admissionRead{admissionStorageDefinition, admissionClusterIdentity} {
		a := admissionCase{read, admissionDisagrees}
		t.Run(a.String(), func(t *testing.T) {
			t.Parallel()
			c := newAdmissionCutOff(t, a, 200*time.Millisecond)
			msg := c.firstCallMessage(t)
			if c.attachErr == nil {
				t.Fatal("attach_disk succeeded although the restore's admission found the record contradicted")
			}
			if !strings.Contains(msg, "managed allocation blocked before Nodes.UpdateQemuConfig") {
				t.Errorf("the guard did not lock itself on an answer that disagrees with the record")
			}
			if strings.Contains(msg, "was not sent because the checks before it") {
				t.Errorf("error says the checks did not finish, but they finished and disagreed")
			}
			if writes := c.hung.landedProtectionWrites(); len(writes) != 1 || writes[0] {
				t.Errorf("protection writes on parker %d = %v, want only the transfer's [false]", c.parker, writes)
			}
			if step, planned := c.plannedRestore(t); planned {
				t.Errorf("the record holds planned protection restore %s on parker %d after an answer that disagrees", step.ID, c.parker)
			}
		})
	}
}

// assertRestoreCutOffError checks what the first attach sent and how its
// error reads.
func assertRestoreCutOffError(t *testing.T, c *admissionCutOff, msg string) {
	t.Helper()
	writes := c.hung.landedProtectionWrites()
	t.Logf("protection writes that landed on parker %d, oldest first: %v", c.parker, writes)
	if len(writes) != 1 || writes[0] {
		t.Errorf("protection writes on parker %d = %v, want only the transfer's [false]", c.parker, writes)
	}
	if want := fmt.Sprintf("protection restore on parker vmid %d", c.parker); !strings.Contains(msg, want) {
		t.Errorf("error does not name the protection restore (%q)", want)
	}
	if ending := c.wantEnding(); ending != "" && !strings.Contains(msg, ending) {
		t.Errorf("error does not say the restore %q", ending)
	}
	if c.admission.read == restoreWriteControl && strings.Contains(msg, "was not sent") {
		t.Errorf("error says the restore was not sent, but the control sent it")
	}
	if strings.Contains(msg, "already uncertain") {
		t.Errorf("error says the restore was refused by an operation already uncertain, but nothing failed before the restore")
	}
	if strings.Contains(msg, "managed allocation blocked before Nodes.UpdateQemuConfig") {
		t.Errorf("the guard locked itself on an admission read that could not finish")
	}
	if want := fmt.Sprintf("qm set %d --protection 1", c.parker); !strings.Contains(msg, want) {
		t.Errorf("error does not give %q", want)
	}
	var typed *cpierrors.Error
	if !errors.As(c.attachErr, &typed) || !typed.OkToRetry() {
		t.Errorf("error is not retriable")
	}
}

// assertRestoreLeftPlanned checks that the record makes the next call read
// the parker back, and that the receiving side was recorded.
func assertRestoreLeftPlanned(t *testing.T, c *admissionCutOff) {
	t.Helper()
	record := c.record(t)
	if record.State != aj.ReconciliationRequired {
		t.Errorf("allocation state = %s, want %s", record.State, aj.ReconciliationRequired)
	}
	step, planned := c.plannedRestore(t)
	if !planned {
		t.Errorf("the record holds no planned protection restore on parker %d, so nothing makes a later call read the parker back", c.parker)
	}
	for i := range record.Steps {
		other := &record.Steps[i]
		if other.ID != step.ID && other.State != aj.Observed {
			t.Errorf("step %s (%s, vmid %d) is %s; the restore should be the only unsettled step", other.ID, other.Kind, other.Target.VMID, other.State)
		}
	}
	vmDesc, _ := c.client.state.configs[777]["description"].(string)
	if !strings.Contains(vmDesc, c.id) || !strings.Contains(vmDesc, c.cid) {
		t.Errorf("VM 777's description lacks the allocation provenance or the CID, so the receiving side was not recorded")
	}
}

// admissionProbe is one of the calls that judges the record after the first
// attach. run makes the call and returns the text an operator reads with its
// error. That text is the scrubbed error for attach_disk, and the
// storage-journal CLI's rendering for adopt and cleanup.
type admissionProbe struct {
	name string
	run  func(*testing.T, *admissionCutOff) (string, error)
}

var admissionProbes = []admissionProbe{
	{"retry attach", func(t *testing.T, c *admissionCutOff) (string, error) {
		_, err := HandleAttachDisk(c.deps).Handle(c.ctx, c.attachArgs, jsonrpc.Context{})
		if err == nil {
			return "<nil>", nil
		}
		return log.ScrubMessage(err.Error()), err
	}},
	{"adopt", func(t *testing.T, c *admissionCutOff) (string, error) {
		_, err := c.adopt(t)
		if err == nil {
			return "<nil>", nil
		}
		return StorageAllocationDecisionFailure(err), err
	}},
	{"cleanup precheck", func(t *testing.T, c *admissionCutOff) (string, error) {
		err := c.cleanup(t)
		if err == nil {
			return "<nil>", nil
		}
		return StorageAllocationDecisionFailure(err), err
	}},
}

// TestRestoreAdmissionCutOffNextCalls follows the first call with each of the
// three calls that judge the record next, with PVE answering again: a rerun
// attach_disk, storage-journal adopt, and the cleanup precheck. While the
// parker reads unprotected, every one of them must refuse and give the qm set
// command that puts protection back. Once it reads protected, each must get
// past the restore, which it settles by reading the parker back.
func TestRestoreAdmissionCutOffNextCalls(t *testing.T) {
	t.Parallel()
	for _, a := range admissionCutOffCases {
		for _, protected := range []bool{false, true} {
			for _, p := range admissionProbes {
				t.Run(fmt.Sprintf("%s/protection %v/%s", a, protected, p.name), func(t *testing.T) {
					t.Parallel()
					runAdmissionProbe(t, a, protected, p)
				})
			}
		}
	}
}

func runAdmissionProbe(t *testing.T, a admissionCase, protected bool, p admissionProbe) {
	t.Helper()
	c := newAdmissionCutOff(t, a, 200*time.Millisecond)
	step, planned := c.plannedRestore(t)
	c.setProtection(protected)
	c.hung.forgetParkerWrites()
	msg, err := p.run(t, c)
	record := c.record(t)
	t.Logf("%s with parker %d protection %v returned: %s", p.name, c.parker, protected, msg)
	if cause := decisionCause(err); cause != nil {
		t.Logf("the error behind it: %s", log.ScrubMessage(cause.Error()))
	}
	t.Logf("parker %d protection after the call: %v; protection writes it sent: %v", c.parker,
		c.client.state.configs[c.parker]["protection"], c.hung.landedProtectionWrites())
	logAdmissionRecord(t, record)

	if !planned {
		t.Errorf("the first call left no planned protection restore on parker %d", c.parker)
	}
	qmSet := fmt.Sprintf("qm set %d --protection 1", c.parker)
	if !protected {
		if err == nil || !strings.Contains(msg, qmSet) {
			t.Errorf("%s did not refuse with %q while parker %d reads unprotected", p.name, qmSet, c.parker)
		}
		return
	}
	if err != nil && (strings.Contains(msg, qmSet) || strings.Contains(msg, "parker protection write could not be settled")) {
		t.Errorf("%s refused on the restore although parker %d reads protected", p.name, c.parker)
	}
	if planned {
		if got := stepState(t, record, step.ID); got != aj.Observed {
			t.Errorf("restore step %s left %s after a protected readback", step.ID, got)
		}
	}
	if p.name != "cleanup precheck" && err != nil {
		t.Errorf("%s failed with parker %d protected: %s", p.name, c.parker, msg)
	}
}
