package handlers

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/harebalance"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const rbGiB = int64(1) << 30

// rbVM is one VM of the fake cluster.
type rbVM struct {
	node     string
	mem      int64
	tags     string
	cfg      map[string]any
	comment  string
	failback bool
	state    string
	// raw is the HA manager's state, started unless set.
	raw string
	// noResource leaves the VM without an HA resource.
	noResource bool
	// dlb sets the resource's auto-rebalance flag.
	dlb bool
}

// fakeRebalanceIO is a cluster of three nodes of 64 GiB each, pve01 to pve03,
// that the run reads and writes through rebalanceIO. It records every call in
// order, and each hook replaces one call's behavior.
type fakeRebalanceIO struct {
	mu     sync.Mutex
	clock  time.Time
	calls  []string
	digest int

	profileVal *ha.Profile
	nodes      []harebalance.Node
	vms        map[int]*rbVM
	pins       map[int]*ha.Rule
	extraRules []ha.Rule

	act     rebalanceActivity
	journ   rebalanceJournal
	lockErr error
	release int

	// moving names the VMs the HA manager reports in migrate, and settleAfter
	// clears it after that many service state reads.
	moving      map[int]bool
	settleAfter int

	recordErr  map[int]error
	claimErr   map[int]error
	markHook   func(vmid int) (bool, error)
	unmarkHook func(vmid int, failback bool) (bool, error)
	migrateFn  func(ctx context.Context, vmid int, from, to string) (pve.VMLocation, error)
	pinErr     error
	want       vmFailbackWant
	wantErr    error
	// afterConfirm runs on the check that the VM stayed on its target.
	afterConfirm func(vmid int) (string, error)
	sleepHook    func(ctx context.Context, d time.Duration) error
	// beforeReread runs before the read of step 1, and beforeMigrate before
	// the check that precedes the migrate request.
	beforeReread  func(vmid int)
	beforeMigrate func(vmid int)
	// observeHook runs before each snapshot with its count, from 1.
	observeHook func(n int)
	snapshots   int
	unmarks     []string
	pinWrites   []string

	// foreign holds the guests of no VM of this Director, by VMID, with their
	// node, and foreignState their HA manager state, started unless set.
	foreign      map[int]string
	foreignState map[int]string
	// jobs are the replication jobs the snapshot reads.
	jobs []ha.ReplicaJobState
	// drainHook replaces the result of a drain state write.
	drainHook func(vmid int, stop bool) (bool, error)
	// stopSettle lets a VM the drain stopped read as stopped to the HA
	// manager after that many service state reads, and startSettle the same
	// for a VM the undrain started. Zero settles at once.
	stopSettle, startSettle int
}

func newFakeRebalanceIO() *fakeRebalanceIO {
	f := &fakeRebalanceIO{
		clock: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
		vms:   map[int]*rbVM{}, pins: map[int]*ha.Rule{}, moving: map[int]bool{},
		recordErr: map[int]error{}, claimErr: map[int]error{},
		want: vmFailbackWant{decided: true, want: true},
	}
	f.profileVal = rbProfile()
	for _, n := range []string{"pve01", "pve02", "pve03"} {
		f.nodes = append(f.nodes, harebalance.Node{Name: n, TotalMemBytes: 64 * rbGiB, Online: true})
	}
	return f
}

// rbProfile is a healthy full-HA profile over pve01 to pve03 with quorum, a
// CRM master, and a fresh manager status.
func rbProfile() *ha.Profile {
	p := stagingProfile(config.HAModeAuto)
	p.Quorate, p.CRMMaster = true, "pve01"
	p.Health = &pve.NodeHealthReport{Quorate: true, HA: pve.HAViewManagerStatus}
	for _, n := range []string{"pve01", "pve02", "pve03"} {
		p.Health.Nodes = append(p.Health.Nodes, pve.NodeHealthEntry{Name: n, Placeable: true})
	}
	return p
}

// addVM adds a shared VM of this Director on node with a flat pin over every
// node, failback on, and its root disk on nfs.
func (f *fakeRebalanceIO) addVM(vmid int, node string, memGiB int64) *rbVM {
	v := &rbVM{node: node, mem: memGiB * rbGiB, tags: rbTags(), failback: true, state: "started",
		comment: haIdentity{Owner: aaDirector}.marker(),
		cfg: map[string]any{"scsi0": "nfs:vm-" + strconv.Itoa(vmid) + "-disk-0,size=10G", "cpu": "x86-64-v3",
			"memory": memGiB * 1024}}
	f.vms[vmid] = v
	f.pins[vmid] = rbPin(vmid, map[string]int{"pve01": 0, "pve02": 0, "pve03": 0})
	return v
}

func rbTags() string { return haManagedTag + ";" + ownerTagPrefix + aaDirector }

// rbPin is the pin of vmid over nodes that the CPI writes for a shared VM
// with no AZ, owned by aaDirector.
func rbPin(vmid int, nodes map[string]int) *ha.Rule {
	return &ha.Rule{ID: naRuleNameFor(vmid), Type: ha.RuleTypeNodeAffinity, Affinity: ha.AffinityPositive,
		Nodes:     nodes,
		Resources: []string{haResourceSid(vmid)},
		Comment:   ha.WithVolumeRecord(haIdentity{Owner: aaDirector}.marker(), []string{"nfs"})}
}

func (f *fakeRebalanceIO) log(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

// callsWith returns the calls that start with prefix.
func (f *fakeRebalanceIO) callsWith(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// writes returns the calls that write, in order.
func (f *fakeRebalanceIO) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		for _, p := range []string{"mark ", "unmark ", "migrate ", "pin "} {
			if strings.HasPrefix(c, p) {
				out = append(out, c)
			}
		}
	}
	return out
}

func (f *fakeRebalanceIO) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

func (f *fakeRebalanceIO) sleep(ctx context.Context, d time.Duration) error {
	if f.sleepHook != nil {
		if err := f.sleepHook(ctx, d); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(d)
	f.log("sleep %s", d)
	return nil
}

func (f *fakeRebalanceIO) runLock(context.Context, time.Duration) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("runlock")
	if f.lockErr != nil {
		return nil, f.lockErr
	}
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.release++
		f.log("release")
	}, nil
}

func (f *fakeRebalanceIO) profile(context.Context) (*ha.Profile, error) { return f.profileVal, nil }

func (f *fakeRebalanceIO) resource(vmid int) *sdkcluster.GetHaResourcesResponse {
	v := f.vms[vmid]
	if v == nil || v.noResource {
		return nil
	}
	comment, state := v.comment, v.state
	fb, dlb := sdkclient.PVEBool(v.failback), sdkclient.PVEBool(v.dlb)
	return &sdkcluster.GetHaResourcesResponse{Comment: &comment, State: &state, Failback: &fb,
		AutoRebalance: &dlb, Digest: "d" + strconv.Itoa(f.digest)}
}

func (f *fakeRebalanceIO) snapshot(context.Context, *ha.Profile) (*rebalanceSnapshot, error) {
	f.mu.Lock()
	f.snapshots++
	n, hook := f.snapshots, f.observeHook
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("snapshot")
	snap := &rebalanceSnapshot{nodes: slices.Clone(f.nodes), states: f.statesLocked(),
		rules: &ha.RuleSet{Rules: slices.Clone(f.extraRules)}}
	ids := make([]int, 0, len(f.vms))
	for id := range f.vms {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		v := f.vms[id]
		snap.guests = append(snap.guests, harebalance.Guest{VMID: id, Node: v.node, MemBytes: v.mem, HA: true,
			Admissible: []string{"pve01", "pve02", "pve03"}})
		snap.vms = append(snap.vms, rebalanceVMFacts{vmid: id, name: "vm-" + strconv.Itoa(id), node: v.node,
			tags: v.tags, mem: v.mem, cfg: maps.Clone(v.cfg), res: f.resource(id)})
		if p := f.pins[id]; p != nil {
			snap.rules.Rules = append(snap.rules.Rules, *p)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(f.foreign)) {
		snap.guests = append(snap.guests, harebalance.Guest{VMID: id, Node: f.foreign[id], HA: true,
			Admissible: []string{"pve01", "pve02", "pve03"}})
	}
	snap.jobs = slices.Clone(f.jobs)
	return snap, nil
}

func (f *fakeRebalanceIO) statesLocked() map[string]string {
	out := map[string]string{}
	for id, v := range f.vms {
		raw := v.raw
		switch {
		case raw != "":
		case v.state == "stopped":
			raw = "stopped"
		default:
			raw = "started"
		}
		if f.moving[id] {
			raw = "migrate"
		}
		out[haResourceSid(id)] = raw
	}
	for id := range f.foreign {
		raw := f.foreignState[id]
		if raw == "" {
			raw = "started"
		}
		out[haResourceSid(id)] = raw
	}
	return out
}

func (f *fakeRebalanceIO) activity(context.Context) (rebalanceActivity, error) { return f.act, nil }

func (f *fakeRebalanceIO) journal(context.Context) rebalanceJournal { return f.journ }

func (f *fakeRebalanceIO) serviceStates(context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("states")
	if f.settleAfter > 0 {
		f.settleAfter--
		if f.settleAfter == 0 {
			f.moving = map[int]bool{}
		}
	}
	f.stopSettle = f.settleRaw(f.stopSettle, "stopped", "started")
	f.startSettle = f.settleRaw(f.startSettle, "started", "stopped")
	return f.statesLocked(), nil
}

func (f *fakeRebalanceIO) withRecord(_ context.Context, vmid int, _ map[string]any, fn func() error) error {
	f.mu.Lock()
	err := f.recordErr[vmid]
	f.log("record %d", vmid)
	f.mu.Unlock()
	if err != nil {
		return err
	}
	defer f.logLocked("unrecord %d", vmid)
	return fn()
}

// logLocked records a call under the lock.
func (f *fakeRebalanceIO) logLocked(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log(format, args...)
}

func (f *fakeRebalanceIO) withClaim(ctx context.Context, vmid int, fn func(context.Context) error) error {
	f.mu.Lock()
	err := f.claimErr[vmid]
	f.log("claim %d", vmid)
	f.mu.Unlock()
	if err != nil {
		return err
	}
	defer f.logLocked("unclaim %d", vmid)
	return fn(ctx)
}

func (f *fakeRebalanceIO) reread(_ context.Context, vmid int, node string) (map[string]any,
	*sdkcluster.GetHaResourcesResponse, *ha.Rule, error,
) {
	if f.beforeReread != nil {
		f.beforeReread(vmid)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("reread %d", vmid)
	v := f.vms[vmid]
	if v == nil || v.node != node {
		return nil, f.resource(vmid), f.pins[vmid], nil
	}
	var pin *ha.Rule
	if p := f.pins[vmid]; p != nil {
		cp := *p
		pin = &cp
	}
	return maps.Clone(v.cfg), f.resource(vmid), pin, nil
}

func (f *fakeRebalanceIO) readResource(_ context.Context, vmid int) (*sdkcluster.GetHaResourcesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resource(vmid), nil
}

func (f *fakeRebalanceIO) checkDigest(read *sdkcluster.GetHaResourcesResponse) bool {
	return read != nil && read.Digest == "d"+strconv.Itoa(f.digest)
}

func (f *fakeRebalanceIO) mark(ctx context.Context, vmid int, read *sdkcluster.GetHaResourcesResponse,
	node string,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if f.markHook != nil {
		ok, err := f.markHook(vmid)
		if !ok || err != nil {
			f.mu.Lock()
			f.log("mark %d %s refused", vmid, node)
			f.mu.Unlock()
			return ok, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.checkDigest(read) {
		f.log("mark %d %s conflict", vmid, node)
		return false, nil
	}
	v := f.vms[vmid]
	v.comment, v.failback = ha.WithRebalanceMark(v.comment, node), false
	f.digest++
	f.log("mark %d %s", vmid, node)
	return true, nil
}

func (f *fakeRebalanceIO) unmark(ctx context.Context, vmid int, read *sdkcluster.GetHaResourcesResponse,
	failback bool,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if f.unmarkHook != nil {
		ok, err := f.unmarkHook(vmid, failback)
		if !ok || err != nil {
			f.mu.Lock()
			f.log("unmark %d refused", vmid)
			f.mu.Unlock()
			return ok, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.checkDigest(read) {
		f.log("unmark %d conflict", vmid)
		return false, nil
	}
	v := f.vms[vmid]
	v.comment, _ = ha.WithoutRebalanceMark(v.comment)
	v.failback = failback
	f.digest++
	f.log("unmark %d failback=%v", vmid, failback)
	f.unmarks = append(f.unmarks, fmt.Sprintf("%d:%v", vmid, failback))
	return true, nil
}

func (f *fakeRebalanceIO) migrate(ctx context.Context, vmid int, from, to string,
	before func(context.Context) error,
) (pve.VMLocation, error) {
	if f.beforeMigrate != nil {
		f.beforeMigrate(vmid)
	}
	if err := before(ctx); err != nil {
		f.mu.Lock()
		f.log("migrate %d %s>%s not sent", vmid, from, to)
		f.mu.Unlock()
		return pve.VMLocation{Node: from}, &ha.MoveError{Failure: ha.MoveRefused, Reason: "prepare", Err: err}
	}
	f.mu.Lock()
	f.log("migrate %d %s>%s", vmid, from, to)
	fn := f.migrateFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, vmid, from, to)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vms[vmid].node = to
	return pve.VMLocation{Node: to, Found: true}, nil
}

func (f *fakeRebalanceIO) writePin(ctx context.Context, _ *ha.Profile, _ *ha.VMDecision, vmid int,
	oldPin, newPin *ha.Rule,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("pin %d", vmid)
	if f.pinErr != nil {
		return false, f.pinErr
	}
	if cur := f.pins[vmid]; cur == nil || !ha.SameRule(cur, oldPin) {
		return false, errPinChanged
	}
	cp := *newPin
	f.pins[vmid] = &cp
	f.pinWrites = append(f.pinWrites, fmt.Sprintf("%d:%v", vmid, newPin.Nodes))
	return true, nil
}

func (f *fakeRebalanceIO) wantFailback(context.Context, *ha.Profile, int, pve.VMLocation, string) (
	vmFailbackWant, error,
) {
	return f.want, f.wantErr
}

func (f *fakeRebalanceIO) locate(_ context.Context, vmid int) (string, error) {
	f.mu.Lock()
	hook := f.afterConfirm
	v := f.vms[vmid]
	f.mu.Unlock()
	if v == nil {
		return "", errors.New("gone")
	}
	if hook != nil {
		if node, err := hook(vmid); node != "" || err != nil {
			return node, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vms[vmid].node, nil
}

// rbRun returns a run over f with the given options and the default ones.
func rbRun(f *fakeRebalanceIO, opts RebalanceOptions) *rebalanceRun {
	cfg := icMinConfig()
	cfg.HA = &config.HAConfig{Mode: haChoiceOf(config.HAModeAuto)}
	deps := Deps{Config: cfg, RequestDirectorUUID: aaDirector, Logger: log.NewNopLogger()}
	return newRebalanceRun(deps, opts.withDefaults(), f)
}

// rbSkewed adds three VMs of 8 GiB on pve01 and none elsewhere, a spread of
// 0.375 that memory evening narrows.
func rbSkewed(f *fakeRebalanceIO) {
	f.addVM(101, "pve01", 8)
	f.addVM(102, "pve01", 8)
	f.addVM(103, "pve01", 8)
}

func (f *fakeRebalanceIO) drainState(_ context.Context, vmid int, read *sdkcluster.GetHaResourcesResponse,
	stop bool, node string,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	verb := "start"
	if stop {
		verb = "stop"
	}
	f.log("%s %d", verb, vmid)
	if f.drainHook != nil {
		if ok, err := f.drainHook(vmid, stop); !ok || err != nil {
			return ok, err
		}
	}
	v := f.vms[vmid]
	if v == nil || v.noResource || read == nil || read.Digest != "d"+strconv.Itoa(f.digest) {
		return false, nil
	}
	f.digest++
	if stop {
		v.state, v.comment, v.raw = "stopped", ha.WithDrainMark(v.comment, node), "started"
		if f.stopSettle == 0 {
			v.raw = ""
		}
		return true, nil
	}
	v.comment, _ = ha.WithoutDrainMark(v.comment)
	v.state, v.raw = "started", "stopped"
	if f.startSettle == 0 {
		v.raw = ""
	}
	return true, nil
}

// settleRaw counts down left, and when it reaches zero it lets every VM whose
// resource requests state, and that the HA manager still reports as raw, read
// as state.
func (f *fakeRebalanceIO) settleRaw(left int, state, raw string) int {
	if left <= 0 {
		return left
	}
	left--
	if left == 0 {
		for _, v := range f.vms {
			if v.state == state && v.raw == raw {
				v.raw = ""
			}
		}
	}
	return left
}
