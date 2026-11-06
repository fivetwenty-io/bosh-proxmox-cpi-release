package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// haSim models one HA-managed VM, vm:100 on pve2, in a three-node cluster
// entered through pve1. A state request lands in the resource configuration at
// once, and the CRM processes it after delay reads, which moves the raw state
// and the power state the way PVE does. The CRM counts one round per read of
// the HA resource list, which every normalized read makes exactly once.
type haSim struct {
	mu sync.Mutex

	raw       string // CRM raw state; empty means the CRM holds no entry
	requested string // configured request; empty means no HA resource
	power     string // power state the owner node reports
	node      string
	uid       int

	// delay is how many CRM rounds a request takes, one when zero. frozen
	// keeps the CRM from ever processing a request. failStart puts the service
	// in error when it processes a start. nextRaw, when set, is applied at the
	// next processed round instead of the request, which models motion that
	// was already running. onApply runs after every processed round.
	delay     int
	pending   int
	frozen    bool
	failStart bool
	nextRaw   []string
	onApply   func(s *haSim)

	// nodeState is pve2's CRM node state, online when empty. lrmStale makes
	// pve2's LRM timestamp stale.
	nodeState string
	lrmStale  bool

	// updateErrs fail successive resource writes, and landOnErr applies a
	// failing write anyway. listErr fails every read of the HA resource list.
	// statusErr fails every power read. onWrite runs after every resource write
	// and direct call is recorded, before it fails or lands.
	updateErrs []error
	landOnErr  bool
	listErr    error
	statusErr  error
	onWrite    func(s *haSim)

	// awaitErr is what the direct call's task wait returns. directErrs fail
	// successive direct calls. directRefused keeps a direct call whose task
	// PVE refuses from changing the request.
	awaitErr      error
	directErrs    []error
	directRefused bool

	writes []string // states written through PUT /cluster/ha/resources/{sid}
	direct []string // direct calls, start or stop, with the node they went to
}

func newHASim(raw, requested, power string) *haSim {
	return &haSim{raw: raw, requested: requested, power: power, node: "pve2", uid: 1}
}

func (s *haSim) rows() []json.RawMessage {
	return []json.RawMessage{
		nhClusterRow(1), nhNode("pve1", 1, true, ""), nhNode("pve2", 1, false, ""), nhNode("pve3", 1, false, ""),
	}
}

func (s *haSim) ListStatus(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
	resp := sdkcluster.ListStatusResponse(s.rows())
	return &resp, nil
}

func (s *haSim) ListHaStatusManagerStatus(_ context.Context) (*sdkcluster.ListHaStatusManagerStatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pve2 := s.nodeState
	if pve2 == "" {
		pve2 = "online"
	}
	lrm2 := nhActive(nhNow - 2)
	if s.lrmStale {
		lrm2 = nhActive(nhNow - 120)
	}
	services := map[string]any{"vm:200": haSvc("started", "pve3", "other")}
	if s.raw != "" {
		services["vm:100"] = haSvc(s.raw, s.node, fmt.Sprintf("u-%d", s.uid))
	}
	body := haManagerBody(nhNow-5, "pve1",
		map[string]string{"pve1": "online", "pve2": pve2, "pve3": "online"},
		map[string]map[string]any{"pve1": nhActive(nhNow - 2), "pve2": lrm2, "pve3": nhActive(nhNow - 2)},
		services)
	return &body, nil
}

func (s *haSim) ListHaStatusCurrent(_ context.Context) (*sdkcluster.ListHaStatusCurrentResponse, error) {
	resp := sdkcluster.ListHaStatusCurrentResponse(nil)
	return &resp, nil
}

func (s *haSim) ListHaResources(
	_ context.Context, _ *sdkcluster.ListHaResourcesParams,
) (*sdkcluster.ListHaResourcesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	rows := []json.RawMessage{haResource("vm:200", "started")}
	if s.requested != "" {
		rows = append(rows, haResource("vm:100", s.requested))
	}
	s.round()
	resp := sdkcluster.ListHaResourcesResponse(rows)
	return &resp, nil
}

func (s *haSim) GetHaResources(_ context.Context, sid string) (*sdkcluster.GetHaResourcesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &sdkcluster.GetHaResourcesResponse{Sid: sid}
	if sid == "vm:100" && s.requested != "" {
		state := s.requested
		resp.State = &state
	}
	return resp, nil
}

func (s *haSim) UpdateHaResources(_ context.Context, sid string, params *sdkcluster.UpdateHaResourcesParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sid != "vm:100" || params == nil || params.State == nil {
		return fmt.Errorf("unexpected update of %s", sid)
	}
	state := *params.State
	s.writes = append(s.writes, state)
	if s.onWrite != nil {
		s.onWrite(s)
	}
	var failure error
	if len(s.updateErrs) > 0 {
		failure, s.updateErrs = s.updateErrs[0], s.updateErrs[1:]
	}
	if failure != nil && !s.landOnErr {
		return failure
	}
	if s.raw == "error" && state != "disabled" {
		return errors.New("500 service 'vm:100' in error state, must be disabled and fixed first")
	}
	s.requestLocked(state)
	return failure
}

func (s *haSim) requestLocked(state string) {
	s.requested = state
	s.pending = s.delay
	if s.pending <= 0 {
		s.pending = 1
	}
}

// round is one CRM round. It processes the request once its delay has passed.
func (s *haSim) round() {
	if s.frozen || s.pending <= 0 {
		return
	}
	s.pending--
	if s.pending > 0 {
		return
	}
	switch {
	case len(s.nextRaw) > 0:
		s.raw, s.nextRaw = s.nextRaw[0], s.nextRaw[1:]
		if len(s.nextRaw) > 0 {
			s.pending = 1
		}
	case s.raw == "error" && s.requested != "disabled":
		// PVE leaves an errored service alone until it is disabled.
	case s.requested == "started" && s.failStart:
		s.raw = "error"
	case s.requested == "started":
		s.raw, s.power = "started", "running"
	case s.requested == "stopped", s.requested == "disabled":
		s.raw, s.power = "stopped", "stopped"
	}
	s.uid++
	if s.onApply != nil {
		s.onApply(s)
	}
}

func (s *haSim) Status(_ context.Context, node string, vmid int) (map[string]interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusErr != nil {
		return nil, s.statusErr
	}
	if vmid != 100 || node != s.node {
		return nil, fmt.Errorf("VM %d is not on node %s", vmid, node)
	}
	return map[string]interface{}{"status": s.power, "vmid": float64(vmid)}, nil
}

func (s *haSim) Start(_ context.Context, node string, _ int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.directCallLocked("start@" + node); err != nil {
		return "", err
	}
	if !s.directRefused {
		s.requestLocked("started")
	}
	return "UPID:" + node + ":0001:hastart", nil
}

func (s *haSim) Stop(_ context.Context, node string, _ int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.directCallLocked("stop@" + node); err != nil {
		return "", err
	}
	if !s.directRefused {
		s.requestLocked("stopped")
	}
	return "UPID:" + node + ":0002:hastop", nil
}

// directCallLocked records a direct call and returns its failure, if any.
func (s *haSim) directCallLocked(call string) error {
	s.direct = append(s.direct, call)
	if s.onWrite != nil {
		s.onWrite(s)
	}
	if len(s.directErrs) > 0 {
		var failure error
		failure, s.directErrs = s.directErrs[0], s.directErrs[1:]
		if failure != nil {
			return failure
		}
	}
	if s.raw == "error" {
		return errors.New("500 service 'vm:100' in error state, must be disabled and fixed first")
	}
	return nil
}

func (s *haSim) deps() pve.HAStateDeps {
	return pve.HAStateDeps{
		Cluster: s,
		Nodes:   &nhNodes{now: nhNow},
		QEMU:    s,
		AwaitTask: func(context.Context, string, string, time.Duration) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.awaitErr
		},
	}
}

func (s *haSim) seen() (writes, direct []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...), append([]string(nil), s.direct...)
}

func haStateCtx(t *testing.T) context.Context {
	t.Helper()
	return pve.WithTestBackoff(t.Context(), func(int) time.Duration { return 0 })
}

func haStateIn(target pve.HATarget, caller pve.HACaller, sysConsole bool) pve.SetHAStateInput {
	return pve.SetHAStateInput{
		VMID: 100, Node: "pve2", Target: target, Caller: caller, SysConsole: sysConsole,
		Budget: 2 * time.Second, PollInterval: time.Millisecond,
	}
}

func wantRetriable(t *testing.T, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("want a retriable error, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("error is not retriable: %v", err)
	}
	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error %q does not mention %q", err, f)
		}
	}
}

func wantNoWrites(t *testing.T, s *haSim) {
	t.Helper()
	writes, direct := s.seen()
	if len(writes) != 0 || len(direct) != 0 {
		t.Fatalf("writes %v and direct calls %v; want none", writes, direct)
	}
}

func wantWrites(t *testing.T, s *haSim, want ...string) {
	t.Helper()
	writes, direct := s.seen()
	if strings.Join(writes, ",") != strings.Join(want, ",") {
		t.Fatalf("writes = %v; want %v", writes, want)
	}
	if len(direct) != 0 {
		t.Fatalf("direct calls %v on the Sys.Console path", direct)
	}
}

func wantNeverDisabled(t *testing.T, s *haSim) {
	t.Helper()
	writes, _ := s.seen()
	for _, w := range writes {
		if w == "disabled" {
			t.Fatalf("disabled was sent to a running VM that BOSH keeps: writes %v", writes)
		}
	}
}

func wantReached(t *testing.T, res *pve.SetHAStateResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("SetHAState: %v", err)
	}
	if res == nil || res.Outcome != pve.HAStateReached {
		t.Fatalf("result = %+v; want reached", res)
	}
	if res.SID != "vm:100" || res.VMID != 100 || res.Node != "pve2" || res.State == nil {
		t.Fatalf("result fields: %+v", res)
	}
	if res.ReregistrationError() != nil {
		t.Fatalf("a reached result must carry no re-registration error")
	}
}

func TestSetHAState_SysConsoleStop(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	wantReached(t, res, err)
	wantWrites(t, s, "stopped")
	if res.ClearedError || res.State.Raw != "stopped" || res.State.Requested != "stopped" {
		t.Fatalf("result: %+v state %+v", res, res.State)
	}
}

func TestSetHAState_SysConsoleStart(t *testing.T) {
	t.Parallel()
	s := newHASim("stopped", "stopped", "stopped")
	s.delay = 3
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStarted, pve.HACallerKeep, true))
	wantReached(t, res, err)
	wantWrites(t, s, "started")
}

// The CRM shows started before the LRM has started the guest, so the raw state
// alone is not the target.
func TestSetHAState_StartedWaitsForThePowerState(t *testing.T) {
	t.Parallel()
	s := newHASim("stopped", "stopped", "stopped")
	powered := 0
	s.onApply = func(sim *haSim) {
		if sim.raw == "started" && powered == 0 {
			sim.power = "stopped"
			powered++
			sim.nextRaw = []string{"started"}
			sim.pending = 3
		} else if sim.raw == "started" {
			sim.power = "running"
		}
	}
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStarted, pve.HACallerKeep, true))
	wantReached(t, res, err)
	if s.power != "running" {
		t.Fatalf("returned before the guest ran: power %s", s.power)
	}
}

func TestSetHAState_DirectStartWithoutSysConsole(t *testing.T) {
	t.Parallel()
	s := newHASim("stopped", "stopped", "stopped")
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStarted, pve.HACallerKeep, false))
	wantReached(t, res, err)
	writes, direct := s.seen()
	if len(writes) != 0 || strings.Join(direct, ",") != "start@pve2" {
		t.Fatalf("writes %v direct %v; want one direct start on pve2 and no resource write", writes, direct)
	}
}

func TestSetHAState_DirectStopWithoutSysConsole(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, false))
	wantReached(t, res, err)
	writes, direct := s.seen()
	if len(writes) != 0 || strings.Join(direct, ",") != "stop@pve2" {
		t.Fatalf("writes %v direct %v; want one direct stop and no resource write", writes, direct)
	}
}

// The CRM's node wins over the scan's node, so the direct call goes where the
// service is.
func TestSetHAState_DirectCallFollowsTheCRMNode(t *testing.T) {
	t.Parallel()
	s := newHASim("stopped", "stopped", "stopped")
	s.node = "pve3"
	in := haStateIn(pve.HATargetStarted, pve.HACallerKeep, false)
	in.Node = "pve2"
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), in)
	if err != nil {
		t.Fatalf("SetHAState: %v", err)
	}
	_, direct := s.seen()
	if res.Node != "pve3" || strings.Join(direct, ",") != "start@pve3" {
		t.Fatalf("node %s direct %v; want pve3", res.Node, direct)
	}
}

func TestSetHAState_DirectTaskUnresolvedStillPolls(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	s.awaitErr = errors.New("AwaitTask UPID: poll timeout — task still running")
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, false))
	wantReached(t, res, err)
}

func TestSetHAState_DirectTaskRefused(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	s.frozen = true
	s.awaitErr = errors.New("task UPID:pve2:0002:hastop failed: exit status \"permission denied\"")
	s.directRefused = true
	_, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, false))
	if err == nil || !strings.Contains(err.Error(), "refused the direct stopped request") {
		t.Fatalf("err = %v; want the refusal", err)
	}
	_, direct := s.seen()
	if len(direct) != 1 {
		t.Fatalf("a refused direct call was repeated: %v", direct)
	}
}

func TestSetHAState_QueuedRequestNeverCompletes(t *testing.T) {
	t.Parallel()
	for _, console := range []bool{true, false} {
		t.Run(fmt.Sprintf("sys_console=%v", console), func(t *testing.T) {
			t.Parallel()
			s := newHASim("started", "started", "running")
			s.frozen = true
			in := haStateIn(pve.HATargetStopped, pve.HACallerTeardown, console)
			in.Budget = 40 * time.Millisecond
			_, err := pve.SetHAState(haStateCtx(t), s.deps(), in)
			wantRetriable(t, err, "did not reach stopped", "vm:100 is started")
		})
	}
}

func TestSetHAState_VMReachesError(t *testing.T) {
	t.Parallel()
	s := newHASim("stopped", "stopped", "stopped")
	s.failStart = true
	_, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStarted, pve.HACallerKeep, true))
	wantRetriable(t, err, "entered the HA error state", "PVE HA gave up")
	wantWrites(t, s, "started")
}

func transportDrop() error { return fmt.Errorf("cluster.UpdateHaResources: %w", io.ErrUnexpectedEOF) }

func TestSetHAState_HeldTransportErrorThatLanded(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	s.updateErrs = []error{transportDrop()}
	s.landOnErr = true
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	wantReached(t, res, err)
	// The re-read found the request in place, so it was not sent again.
	wantWrites(t, s, "stopped")
}

func TestSetHAState_HeldTransportErrorThatDidNotLand(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	s.updateErrs = []error{transportDrop()}
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	wantReached(t, res, err)
	wantWrites(t, s, "stopped", "stopped")
}

// A retry needs a read that can judge it, so an unreadable HA state after a
// failed write ends the call with that one write.
func TestSetHAState_HeldTransportErrorWithUnreadableState(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	s.updateErrs = []error{transportDrop(), transportDrop()}
	s.onWrite = func(sim *haSim) { sim.listErr = transportDrop() }
	_, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	wantRetriable(t, err, "requesting stopped for vm:100 failed", "could not be read again", "not sent again")
	wantWrites(t, s, "stopped")
}

// A write that keeps failing stops at the bounded four attempts.
func TestSetHAState_HeldTransportErrorIsBounded(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	s.updateErrs = []error{transportDrop(), transportDrop(), transportDrop(), transportDrop(), transportDrop()}
	_, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	wantRetriable(t, err, "requesting stopped for vm:100 failed")
	wantWrites(t, s, "stopped", "stopped", "stopped", "stopped")
}

// PVE refuses a state transition with a 500, which reads as transient, but the
// same request gets the same answer, so it is not sent again.
func TestSetHAState_StateRefusalAsServerErrorIsNotRetried(t *testing.T) {
	t.Parallel()
	refusal := fmt.Errorf("cluster.UpdateHaResources: %w: service 'vm:100' in error state, "+
		"must be disabled and fixed first", sdkerrors.ErrServer)
	if !pve.IsTransientTransport(refusal) {
		t.Fatal("the refusal must read as transient for this test to mean anything")
	}
	s := newHASim("started", "started", "running")
	s.updateErrs = []error{refusal}
	_, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	if err == nil || !strings.Contains(err.Error(), "must be disabled and fixed first") {
		t.Fatalf("err = %v; want the refusal", err)
	}
	wantWrites(t, s, "stopped")
}

// A barrier that sets ignored during the backoff wins: the teardown gets the
// not-managed outcome and the retry is never sent.
func TestSetHAState_IgnoredDuringTheBackoffForTeardown(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	s.updateErrs = []error{transportDrop()}
	s.onWrite = func(sim *haSim) { sim.raw, sim.requested = "", "ignored" }
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	if err != nil {
		t.Fatalf("SetHAState: %v", err)
	}
	if res.Outcome != pve.HAStateNotManaged || res.Node != "pve2" || res.State == nil ||
		res.State.Requested != "ignored" {
		t.Fatalf("result = %+v; want not managed", res)
	}
	wantWrites(t, s, "stopped")
}

func TestSetHAState_IgnoredDuringTheBackoffForKeep(t *testing.T) {
	t.Parallel()
	s := newHASim("stopped", "stopped", "stopped")
	s.updateErrs = []error{transportDrop()}
	s.onWrite = func(sim *haSim) { sim.raw, sim.requested = "", "ignored" }
	_, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStarted, pve.HACallerKeep, true))
	wantRetriable(t, err, "is ignored")
	if !errors.Is(err, pve.ErrHAResourceIgnored) {
		t.Fatalf("err = %v; want ErrHAResourceIgnored in its chain", err)
	}
	wantWrites(t, s, "started")
}

// Another writer changed the request during the backoff, so the retry would
// overwrite it.
func TestSetHAState_CompetingWriterDuringTheBackoff(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	s.updateErrs = []error{transportDrop()}
	s.onWrite = func(sim *haSim) { sim.requested = "disabled" }
	_, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	wantRetriable(t, err, "another writer", `"disabled"`)
	wantWrites(t, s, "stopped")
}

// A kept VM's guest starts while a failed disabled write waits to be retried,
// so disabled is never sent to the running guest.
func TestSetHAState_KeptGuestStartsBeforeTheDisabledRetry(t *testing.T) {
	t.Parallel()
	s := newHASim("error", "started", "stopped")
	s.updateErrs = []error{transportDrop()}
	s.onWrite = func(sim *haSim) { sim.power = "running" }
	_, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStarted, pve.HACallerKeep, true))
	wantRetriable(t, err, "is running on node pve2", "disabled, which would stop it, was not sent again")
	wantWrites(t, s, "disabled")
	if s.power != "running" {
		t.Fatalf("the guest was stopped")
	}
}

func TestSetHAState_KeptGuestPowerUnreadableBeforeTheDisabledRetry(t *testing.T) {
	t.Parallel()
	s := newHASim("error", "started", "stopped")
	s.updateErrs = []error{transportDrop()}
	s.onWrite = func(sim *haSim) { sim.statusErr = errors.New("500 node pve2 is busy") }
	_, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStarted, pve.HACallerKeep, true))
	if err == nil || !strings.Contains(err.Error(), "unreadable before a retry of disabled") {
		t.Fatalf("err = %v; want the unreadable power refusal", err)
	}
	wantWrites(t, s, "disabled")
}

// The VM moved during the backoff, so the direct retry goes to its new node.
func TestSetHAState_DirectRetryFollowsTheMove(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	s.directErrs = []error{fmt.Errorf("qemu.Stop: %w", io.ErrUnexpectedEOF)}
	s.onWrite = func(sim *haSim) {
		if len(sim.direct) == 1 {
			sim.node = "pve3"
		}
	}
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, false))
	if err != nil {
		t.Fatalf("SetHAState: %v", err)
	}
	_, direct := s.seen()
	if res.Outcome != pve.HAStateReached || res.Node != "pve3" || strings.Join(direct, ",") != "stop@pve2,stop@pve3" {
		t.Fatalf("result %+v direct %v; want the retry on pve3", res, direct)
	}
}

func TestSetHAState_DefiniteRejectionIsNotRetried(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	s.updateErrs = []error{errors.New("403 Permission check failed (/, Sys.Console)")}
	_, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	if err == nil || !strings.Contains(err.Error(), "Permission check failed") {
		t.Fatalf("err = %v; want the rejection", err)
	}
	wantWrites(t, s, "stopped")
}

func TestSetHAState_ErrorClearedForTeardown(t *testing.T) {
	t.Parallel()
	// The teardown destroys the VM next, so disabled may stop the guest.
	s := newHASim("error", "started", "running")
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	wantReached(t, res, err)
	wantWrites(t, s, "disabled", "stopped")
	if !res.ClearedError || s.power != "stopped" {
		t.Fatalf("result %+v power %s", res, s.power)
	}
}

func TestSetHAState_ErrorClearedForTeardownWithUnreadablePower(t *testing.T) {
	t.Parallel()
	s := newHASim("error", "started", "running")
	s.statusErr = errors.New("500 node pve2 is busy")
	in := haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true)
	in.Budget = 40 * time.Millisecond
	_, err := pve.SetHAState(haStateCtx(t), s.deps(), in)
	// The teardown still requests disabled, and the wait for it then needs the
	// power state, which stays unreadable, so the target is never requested.
	wantRetriable(t, err, "did not reach disabled", "power state on node pve2 is unreadable")
	wantWrites(t, s, "disabled")
}

func TestSetHAState_StoppedKeptVMInErrorTakesDisabled(t *testing.T) {
	t.Parallel()
	s := newHASim("error", "started", "stopped")
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStarted, pve.HACallerKeep, true))
	wantReached(t, res, err)
	wantWrites(t, s, "disabled", "started")
	if !res.ClearedError || res.State.Raw != "started" || s.power != "running" {
		t.Fatalf("result %+v state %+v power %s", res, res.State, s.power)
	}
}

func TestSetHAState_RunningKeptVMInErrorNeedsReregistration(t *testing.T) {
	t.Parallel()
	for _, target := range []pve.HATarget{pve.HATargetStarted, pve.HATargetStopped} {
		t.Run(string(target), func(t *testing.T) {
			t.Parallel()
			s := newHASim("error", "started", "running")
			res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(target, pve.HACallerKeep, true))
			if err != nil {
				t.Fatalf("SetHAState: %v", err)
			}
			wantNeverDisabled(t, s)
			wantNoWrites(t, s)
			if res.Outcome != pve.HAStateNeedsReregistration || res.ClearedError {
				t.Fatalf("result = %+v; want needs re-registration", res)
			}
			if res.State == nil || res.State.Raw != "error" || res.State.UID != "u-1" || res.Node != "pve2" {
				t.Fatalf("decision read not carried: %+v", res.State)
			}
			wantRetriable(t, res.ReregistrationError(), "error state", "ha-manager remove vm:100",
				"ha-manager add vm:100")
			if s.power != "running" {
				t.Fatalf("the guest was stopped")
			}
		})
	}
}

func TestSetHAState_KeptVMInErrorWithUnreadablePower(t *testing.T) {
	t.Parallel()
	s := newHASim("error", "started", "running")
	s.statusErr = errors.New("500 node pve2 is busy")
	_, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStarted, pve.HACallerKeep, true))
	if err == nil || !strings.Contains(err.Error(), "power state of its VM on node pve2 is unreadable") {
		t.Fatalf("err = %v; want the unreadable power refusal", err)
	}
	wantNoWrites(t, s)
}

func TestSetHAState_ErrorWithoutSysConsole(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		power  string
		caller pve.HACaller
		target pve.HATarget
		want   []string
		absent string
	}{
		{"kept and stopped", "stopped", pve.HACallerKeep, pve.HATargetStarted,
			[]string{"Sys.Console", "ha-manager set vm:100 --state disabled", "ha-manager set vm:100 --state started"},
			"ha-manager remove"},
		{"kept and running", "running", pve.HACallerKeep, pve.HATargetStarted,
			[]string{"Sys.Console", "ha-manager remove vm:100", "ha-manager add vm:100", "is running"},
			"--state disabled"},
		{"teardown and stopped", "stopped", pve.HACallerTeardown, pve.HATargetStopped,
			[]string{"Sys.Console", "ha-manager set vm:100 --state disabled"}, "ha-manager remove"},
		{"teardown and running", "running", pve.HACallerTeardown, pve.HATargetStopped,
			[]string{"Sys.Console", "ha-manager set vm:100 --state disabled"}, "ha-manager remove"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newHASim("error", "started", tc.power)
			_, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(tc.target, tc.caller, false))
			wantRetriable(t, err, tc.want...)
			if strings.Contains(err.Error(), tc.absent) {
				t.Errorf("error %q names %q, which does not fit the caller", err, tc.absent)
			}
			wantNoWrites(t, s)
		})
	}
}

func TestSetHAState_KeptVMInErrorWithoutSysConsoleAndUnreadablePower(t *testing.T) {
	t.Parallel()
	s := newHASim("error", "started", "running")
	s.statusErr = errors.New("500 node pve2 is busy")
	_, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStarted, pve.HACallerKeep, false))
	wantRetriable(t, err, "is unreadable", "ha-manager remove vm:100")
	if strings.Contains(err.Error(), "--state disabled") {
		t.Errorf("error %q offers disabled for a VM that may be running", err)
	}
	wantNoWrites(t, s)
}

func TestSetHAState_AlreadyAtTarget(t *testing.T) {
	t.Parallel()
	s := newHASim("stopped", "stopped", "stopped")
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	wantReached(t, res, err)
	wantNoWrites(t, s)
}

func TestSetHAState_EnabledReadsAsStarted(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "enabled", "running")
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStarted, pve.HACallerKeep, true))
	wantReached(t, res, err)
	wantNoWrites(t, s)
}

func TestSetHAState_DisabledResourceStops(t *testing.T) {
	t.Parallel()
	s := newHASim("stopped", "disabled", "stopped")
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	wantReached(t, res, err)
	wantWrites(t, s, "stopped")
}

func TestSetHAState_WritesNothingWhenItCannotAct(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(s *haSim)
		want  string
	}{
		{"stale LRM", func(s *haSim) { s.lrmStale = true }, "LRM entry is missing or older"},
		{"fenced node", func(s *haSim) { s.nodeState = "fence" }, "fenced"},
		{"fence state", func(s *haSim) { s.raw = "fence" }, "fenced"},
		{"not an HA resource", func(s *haSim) { s.raw, s.requested = "", "" }, "not an HA resource"},
		{"being removed", func(s *haSim) { s.requested = "" }, "being removed"},
		{"ignored and still in the CRM", func(s *haSim) { s.requested = "ignored" }, "CRM still lists it"},
		{"ignored on a node with a stale LRM", func(s *haSim) { s.raw, s.requested, s.lrmStale = "", "ignored", true },
			"cannot be torn down now"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newHASim("started", "started", "running")
			tc.setup(s)
			for _, console := range []bool{true, false} {
				_, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, console))
				wantRetriable(t, err, tc.want)
			}
			wantNoWrites(t, s)
		})
	}
}

// An ignored resource that the CRM has dropped gets no state request. The
// teardown stops it directly, and a kept caller gets the typed refusal.
func TestSetHAState_IgnoredResource(t *testing.T) {
	t.Parallel()
	for _, console := range []bool{true, false} {
		t.Run(fmt.Sprintf("sys_console=%v", console), func(t *testing.T) {
			t.Parallel()
			s := newHASim("", "ignored", "running")
			res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, console))
			if err != nil {
				t.Fatalf("teardown: %v", err)
			}
			if res.Outcome != pve.HAStateNotManaged || res.SID != "vm:100" || res.Node != "pve2" || res.ClearedError ||
				res.ReregistrationError() != nil {
				t.Fatalf("teardown result = %+v; want not managed", res)
			}
			res, err = pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStarted, pve.HACallerKeep, console))
			if res != nil || !errors.Is(err, pve.ErrHAResourceIgnored) {
				t.Fatalf("keep: res %+v err %v; want ErrHAResourceIgnored", res, err)
			}
			wantRetriable(t, err, "is ignored")
			wantNoWrites(t, s)
		})
	}
}

// A request that becomes ignored while SetHAState waits ends the wait.
func TestSetHAState_IgnoredDuringTheWait(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	// The CRM's next round after the stop request finds the resource ignored.
	s.delay = 2
	s.nextRaw = []string{"started"}
	s.onApply = func(sim *haSim) { sim.raw, sim.requested = "", "ignored" }
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	if err != nil || res.Outcome != pve.HAStateNotManaged {
		t.Fatalf("res %+v err %v; want not managed", res, err)
	}
}

// The call ends five seconds before the context's deadline, not at the end
// of a longer budget.
func TestSetHAState_BudgetClampedToTheContextDeadline(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	s.frozen = true
	ctx, cancel := context.WithTimeout(haStateCtx(t), 5*time.Second+100*time.Millisecond)
	defer cancel()
	in := haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true)
	in.Budget = time.Hour
	start := time.Now()
	_, err := pve.SetHAState(ctx, s.deps(), in)
	wantRetriable(t, err, "did not reach stopped")
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the call took %s; want it to end at the clamped deadline", elapsed)
	}
	if ctx.Err() != nil {
		t.Fatal("the call used up the reserve it must leave on the context")
	}
}

func TestSetHAState_NoTimeLeftWritesNothing(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	ctx, cancel := context.WithTimeout(haStateCtx(t), 2*time.Second)
	defer cancel()
	_, err := pve.SetHAState(ctx, s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	wantRetriable(t, err, "deadline leaves no time")
	wantNoWrites(t, s)
}

func TestSetHAState_WaitsThroughMotionBeforeWriting(t *testing.T) {
	t.Parallel()
	s := newHASim("migrate", "started", "running")
	s.nextRaw = []string{"started"}
	s.pending = 3
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	wantReached(t, res, err)
	wantWrites(t, s, "stopped")
}

func TestSetHAState_MotionThatNeverEndsWritesNothing(t *testing.T) {
	t.Parallel()
	s := newHASim("migrate", "started", "running")
	in := haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true)
	in.Budget = 30 * time.Millisecond
	_, err := pve.SetHAState(haStateCtx(t), s.deps(), in)
	wantRetriable(t, err, "still in motion", "migrate")
	wantNoWrites(t, s)
}

// A service already moving toward the target needs no request, only the wait.
func TestSetHAState_MotionTowardTheTarget(t *testing.T) {
	t.Parallel()
	s := newHASim("request_stop", "stopped", "running")
	s.nextRaw = []string{"stopped"}
	s.pending = 2
	s.onApply = func(sim *haSim) { sim.power = "stopped" }
	res, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	wantReached(t, res, err)
	wantNoWrites(t, s)
}

func TestSetHAState_FenceDuringTheWait(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	s.delay = 2
	calls := 0
	// The node is fenced before the CRM processes the stop.
	s.nextRaw = []string{"fence"}
	s.onApply = func(*haSim) { calls++ }
	_, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	wantRetriable(t, err, "cannot reach stopped now", "fence")
	wantWrites(t, s, "stopped")
	if calls == 0 {
		t.Fatal("the CRM never processed a round")
	}
}

func TestSetHAState_ResourceRemovedDuringTheWait(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	s.onApply = func(sim *haSim) { sim.raw, sim.requested = "", "" }
	_, err := pve.SetHAState(haStateCtx(t), s.deps(), haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true))
	wantRetriable(t, err, "disappeared from HA")
}

func TestSetHAState_ContextEnded(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	s.frozen = true
	ctx, cancel := context.WithCancel(haStateCtx(t))
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	in := haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true)
	in.Budget = time.Minute
	_, err := pve.SetHAState(ctx, s.deps(), in)
	wantRetriable(t, err, "ended with the call")
}

type haIn = pve.SetHAStateInput

func TestSetHAState_WrongCalls(t *testing.T) {
	t.Parallel()
	s := newHASim("started", "started", "running")
	good := haStateIn(pve.HATargetStopped, pve.HACallerTeardown, true)
	var nilCtx context.Context
	cases := []struct {
		name string
		ctx  context.Context
		deps func() pve.HAStateDeps
		in   func(in haIn) haIn
		want string
	}{
		{"nil ctx", nilCtx, s.deps, func(in haIn) haIn { return in }, "ctx"},
		{"nil cluster", t.Context(), func() pve.HAStateDeps { d := s.deps(); d.Cluster = nil; return d },
			func(in haIn) haIn { return in }, "clients"},
		{"no awaiter on the direct path", t.Context(), func() pve.HAStateDeps { d := s.deps(); d.AwaitTask = nil; return d },
			func(in haIn) haIn { in.SysConsole = false; return in }, "awaiter"},
		{"VM ID", t.Context(), s.deps, func(in haIn) haIn { in.VMID = 0; return in }, "VM ID"},
		{"node", t.Context(), s.deps, func(in haIn) haIn { in.Node = " "; return in }, "node"},
		{"target", t.Context(), s.deps,
			func(in haIn) haIn { in.Target = "disabled"; return in }, "target"},
		{"caller", t.Context(), s.deps, func(in haIn) haIn { in.Caller = 0; return in }, "caller"},
		{"teardown start", t.Context(), s.deps,
			func(in haIn) haIn { in.Target = pve.HATargetStarted; return in }, "teardown"},
		{"budget", t.Context(), s.deps, func(in haIn) haIn { in.Budget = 0; return in }, "budget"},
	}
	for _, tc := range cases {
		res, err := pve.SetHAState(tc.ctx, tc.deps(), tc.in(good))
		if err == nil || res != nil || cpierrors.IsType(err, cpierrors.TypeRetriableCloud) ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: res %v err %v; want a non-retriable error naming %q", tc.name, res, err, tc.want)
		}
	}
	wantNoWrites(t, s)
}

func TestSetHAStateResult_ReregistrationErrorOnlyForItsOutcome(t *testing.T) {
	t.Parallel()
	var nilResult *pve.SetHAStateResult
	if nilResult.ReregistrationError() != nil {
		t.Error("nil result must carry no error")
	}
	if (&pve.SetHAStateResult{Outcome: pve.HAStateReached}).ReregistrationError() != nil {
		t.Error("reached result must carry no error")
	}
}

func TestHAStateDepsFor_WiresTheClient(t *testing.T) {
	t.Parallel()
	deps := pve.HAStateDepsFor(&fakeEnsureClient{})
	if deps.AwaitTask == nil {
		t.Fatal("AwaitTask is not wired")
	}
	// AwaitTask refuses an empty UPID before it touches the client.
	if err := deps.AwaitTask(t.Context(), "pve2", "", time.Second); err == nil ||
		!strings.Contains(err.Error(), "upid must not be empty") {
		t.Fatalf("AwaitTask err = %v; want the empty-UPID refusal", err)
	}
}
