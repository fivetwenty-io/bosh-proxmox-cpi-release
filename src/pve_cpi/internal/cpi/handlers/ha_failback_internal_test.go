package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// failbackCluster is the attach stub with a failback flag on VM 100's HA
// resource, nil for PVE's default of on, and a record of the flags written.
//
// Its resources.cfg has a digest that changes with every write that changes
// the file, and a write that names an older digest is refused the way PVE
// refuses it.
type failbackCluster struct {
	*attachHAStub
	failback  *sdkclient.PVEBool
	writes    []bool
	updateErr error
	version   int
}

func (c *failbackCluster) digest() string { return fmt.Sprintf("digest-%d", c.version) }

func (c *failbackCluster) GetHaResources(ctx context.Context, sid string) (*cluster.GetHaResourcesResponse, error) {
	res, err := c.attachHAStub.GetHaResources(ctx, sid)
	if res != nil {
		res.Failback = c.failback
		res.Digest = c.digest()
	}
	return res, err
}

func (c *failbackCluster) UpdateHaResources(_ context.Context, _ string, p *cluster.UpdateHaResourcesParams) error {
	if p.Failback == nil || p.State != nil || p.Comment != nil {
		return errors.New("the failback reconcile writes failback alone")
	}
	if c.updateErr != nil {
		return c.updateErr
	}
	if p.Digest != nil && *p.Digest != c.digest() {
		return errors.New("detected modified configuration - file changed by other user")
	}
	c.writes = append(c.writes, *p.Failback)
	if ha.FailbackOf(c.failback) != *p.Failback || c.failback == nil {
		c.version++
	}
	c.failback = failbackFlag(*p.Failback)
	return nil
}

// failbackRig is VM 100 on pve01 with its root disk on nfs, the HA-managed
// tag, and a non-strict pin of this Director over every node.
type failbackRig struct {
	deps    Deps
	cluster *failbackCluster
	cfg     map[string]any
	loc     pve.VMLocation
	obs     *log.Observer
	// onConfig, when set, runs at each config read, numbered from 1, and the
	// config it returns, when not nil, is the one the read answers.
	onConfig    func(read int) map[string]any
	configReads int
}

func newFailbackRig(t *testing.T, failback *sdkclient.PVEBool) *failbackRig {
	t.Helper()
	deps, stub, _ := attachRig(config.HAModeAuto)
	stub.recordPin("nfs")
	r := &failbackRig{
		deps: deps, cluster: &failbackCluster{attachHAStub: stub, failback: failback},
		cfg: map[string]any{"scsi0": "nfs:vm-100-disk-0,size=10G", "cpu": "x86-64-v3", "tags": haManagedTag},
		loc: pve.VMLocation{Node: "pve01", Found: true, Tags: haManagedTag},
	}
	q := &icQEMUService{configFn: func(context.Context, string, int) (map[string]any, error) {
		r.configReads++
		if r.onConfig != nil {
			if cfg := r.onConfig(r.configReads); cfg != nil {
				return cfg, nil
			}
		}
		return maps.Clone(r.cfg), nil
	}}
	r.deps.PVE = &icPVEClient{clusterSvc: r.cluster, qemuSvc: q}
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	r.deps.Logger, r.obs = logger, obs
	return r
}

// failbackLogged counts the entries at level whose message contains text.
func failbackLogged(obs *log.Observer, level slog.Level, text string) int {
	n := 0
	for _, e := range obs.All() {
		if e.Level == level && strings.Contains(e.Message, text) {
			n++
		}
	}
	return n
}

func (r *failbackRig) reconcile() {
	reconcileVMFailback(context.Background(), r.deps, "set_vm_metadata", 100, r.loc, r.deps.Logger)
}

func (r *failbackRig) requireWrites(t *testing.T, want ...bool) {
	t.Helper()
	if len(r.cluster.writes) != len(want) {
		t.Fatalf("writes %v, want %v", r.cluster.writes, want)
	}
	for i := range want {
		if r.cluster.writes[i] != want[i] {
			t.Fatalf("writes %v, want %v", r.cluster.writes, want)
		}
	}
}

func TestReconcileVMFailback_SharedVMGetsFailbackOn(t *testing.T) {
	r := newFailbackRig(t, failbackFlag(false))
	r.reconcile()
	r.requireWrites(t, true)
	if failbackLogged(r.obs, log.LevelInfo, "set the failback flag") != 1 {
		t.Fatalf("the change must be logged, logs %v", r.obs.All())
	}
	// A second touch finds the flag already set and writes nothing.
	r.reconcile()
	r.requireWrites(t, true)
}

func TestReconcileVMFailback_UnsetFlagReadsAsOn(t *testing.T) {
	r := newFailbackRig(t, nil)
	r.reconcile()
	r.requireWrites(t)
}

func TestReconcileVMFailback_TurnsFailbackOff(t *testing.T) {
	cases := map[string]func(*failbackRig){
		"pve.ha.failback is false": func(r *failbackRig) {
			off := false
			r.deps.Config.HA.Failback = &off
		},
		"the VM carries the replicate tag": func(r *failbackRig) {
			r.loc.Tags = haManagedTag + ";" + haReplicaTag
		},
		"the VM config carries the replicate tag": func(r *failbackRig) {
			r.cfg["tags"] = haManagedTag + ";" + haReplicaTag
		},
		"the pin carries a replica token": func(r *failbackRig) {
			pin := r.cluster.rules["bosh-na-100"]
			comment := ha.WithReplicaPartner(haIdentity{Owner: aaDirector}.marker(), "pve02")
			pin.Comment = &comment
			r.cluster.rules["bosh-na-100"] = pin
		},
		"a node-local disk under the hold policy": func(r *failbackRig) {
			r.deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyHold)
			r.cfg["scsi1"] = testPinStorage + ":vm-100-disk-1,size=1G"
			r.cluster.pin(true, "pve01", "nfs", testPinStorage)
		},
		"a node-local disk under policy none": func(r *failbackRig) {
			r.cfg["scsi1"] = testPinStorage + ":vm-100-disk-1,size=1G"
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			r := newFailbackRig(t, nil)
			setup(r)
			r.reconcile()
			r.requireWrites(t, false)
		})
	}
}

func TestReconcileVMFailback_LeavesTheFlag(t *testing.T) {
	cases := map[string]func(*failbackRig){
		"mode off": func(r *failbackRig) { r.deps.Config.HA.Mode = haChoiceOf(config.HAModeOff) },
		"a VM without the HA-managed tag": func(r *failbackRig) {
			r.loc.Tags = "bosh-cpi"
		},
		"another Director's resource": func(r *failbackRig) {
			r.cluster.comment = haIdentity{Owner: "dir-2"}.marker()
		},
		"a VM with no HA resource": func(r *failbackRig) { r.cluster.missing = true },
		"no Director UUID":         func(r *failbackRig) { r.deps.RequestDirectorUUID = "" },
		"a node that is down": func(r *failbackRig) {
			p := attachProfile(config.HAModeAuto)
			p.Nodes[1].Online, p.Nodes[2].Online = false, false
			r.deps.HAProfile = func(context.Context) (*ha.Profile, error) { return p, nil }
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			r := newFailbackRig(t, failbackFlag(false))
			setup(r)
			r.reconcile()
			r.requireWrites(t)
		})
	}
}

func TestReconcileVMFailback_WriteFailureOnlyLogs(t *testing.T) {
	r := newFailbackRig(t, failbackFlag(false))
	r.cluster.updateErr = errors.New("cfs lock timeout (HTTP500)")
	r.reconcile()
	if failbackLogged(r.obs, log.LevelWarn, "could not be set") != 1 {
		t.Fatalf("the failed write must be logged as a warning, logs %v", r.obs.All())
	}
}

// indexedFailbackCluster is the failback stub with a cluster index that lists
// VM 100 on pve01 with the HA-managed tag, so the detach path can locate it.
type indexedFailbackCluster struct{ *failbackCluster }

func (indexedFailbackCluster) ListResources(context.Context, *cluster.ListResourcesParams) (
	*cluster.ListResourcesResponse, error,
) {
	entry, err := json.Marshal(map[string]any{"vmid": 100, "node": "pve01", "type": "qemu", "tags": haManagedTag})
	if err != nil {
		return nil, err
	}
	resp := cluster.ListResourcesResponse{entry}
	return &resp, nil
}

// TestReconcileFailbackAfterDetach sets the flag only after a detach that
// succeeded, and leaves it to the next call after one that failed.
func TestReconcileFailbackAfterDetach(t *testing.T) {
	failed := errors.New("detach failed")
	r := newFailbackRig(t, failbackFlag(false))
	r.deps.PVE.(*icPVEClient).clusterSvc = indexedFailbackCluster{r.cluster}
	reconcileFailbackAfterDetach(context.Background(), r.deps, 100, failed)
	r.requireWrites(t)

	reconcileFailbackAfterDetach(context.Background(), r.deps, 100, nil)
	r.requireWrites(t, true)
}

// TestReconcileVMFailback_HoldVMAfterDetachKeepsFailbackOff reconciles a hold
// VM whose node-local disk was detached. Its own strict pin still names its
// node alone, so the decision leaves it nowhere to fail back to, and the
// reconcile must not turn failback on, which a later attach of a node-local
// disk that skips the decision would then find.
func TestReconcileVMFailback_HoldVMAfterDetachKeepsFailbackOff(t *testing.T) {
	r := newFailbackRig(t, failbackFlag(false))
	r.deps.Config.HA.LocalPolicy = haChoiceOf(config.HALocalPolicyHold)
	r.cluster.pin(true, "pve01", "nfs", testPinStorage)
	reconcileFailbackAfterDetach(context.Background(), withIndexedFailback(r), 100, nil)
	r.requireWrites(t)
	if r.cluster.failback == nil || r.cluster.failback.Bool() {
		t.Fatalf("failback %v, want it left off", r.cluster.failback)
	}
}

// withIndexedFailback returns the rig's deps with a cluster index that lists
// VM 100, so the detach path can locate it.
func withIndexedFailback(r *failbackRig) Deps {
	r.deps.PVE.(*icPVEClient).clusterSvc = indexedFailbackCluster{r.cluster}
	return r.deps
}

// TestReconcileVMFailback_DiskLandingBetweenReadAndWriteTurnsFailbackOffAgain
// lands a node-local disk, as an attach in another process does, after the
// reconcile has read the VM's config and before it writes. Nothing in
// resources.cfg changed, so the raise lands, and the reconcile must then see
// the disk in a fresh read of the config and turn failback off again.
func TestReconcileVMFailback_DiskLandingBetweenReadAndWriteTurnsFailbackOffAgain(t *testing.T) {
	r := newFailbackRig(t, failbackFlag(false))
	r.onConfig = func(read int) map[string]any {
		if read != 1 {
			return nil
		}
		stale := maps.Clone(r.cfg)
		r.cfg["scsi1"] = testPinStorage + ":vm-100-disk-1,size=1G"
		return stale
	}
	r.reconcile()
	r.requireWrites(t, true, false)
	if failbackLogged(r.obs, log.LevelWarn, "turned failback off again") != 1 {
		t.Fatalf("the undone raise must be logged as a warning, logs %v", r.obs.All())
	}
}

// TestReconcileVMFailback_ResourceChangedSinceTheReadIsNotRaised changes
// resources.cfg, as the off-write or the barrier of an attach in another
// process does, after the reconcile has read the resource. The raise names
// the digest it read, so PVE refuses it and failback stays off.
func TestReconcileVMFailback_ResourceChangedSinceTheReadIsNotRaised(t *testing.T) {
	r := newFailbackRig(t, failbackFlag(false))
	r.onConfig = func(int) map[string]any {
		r.cluster.version++
		return nil
	}
	r.reconcile()
	r.requireWrites(t)
	if r.cluster.failback.Bool() {
		t.Fatal("failback must stay off")
	}
}

// TestReconcileVMFailback_LeavesAResourceTheBarrierHolds leaves the flag of a
// resource the narrowing barrier holds to the call that holds it.
func TestReconcileVMFailback_LeavesAResourceTheBarrierHolds(t *testing.T) {
	r := newFailbackRig(t, failbackFlag(false))
	r.cluster.state = "ignored"
	r.cluster.comment = haIdentity{Owner: aaDirector}.marker() + " bosh-narrow-from=started"
	r.reconcile()
	r.requireWrites(t)
}
