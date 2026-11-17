package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// widenCluster is the attach stub with SDN vnet listing and a hook that runs
// before a chosen read of the HA rules, so a test can change the pin between
// the widening's first read and its write.
type widenCluster struct {
	*attachHAStub
	vnets      []string
	ruleReads  int
	beforeRead map[int]func()
}

func (c *widenCluster) ListSdnVnets(context.Context, *cluster.ListSdnVnetsParams) (*cluster.ListSdnVnetsResponse, error) {
	out := make(cluster.ListSdnVnetsResponse, 0, len(c.vnets))
	for _, v := range c.vnets {
		out = append(out, json.RawMessage(`{"vnet":"`+v+`","zone":"z1"}`))
	}
	return &out, nil
}

func (c *widenCluster) ListHaRules(ctx context.Context, p *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	c.ruleReads++
	if fn := c.beforeRead[c.ruleReads]; fn != nil {
		fn()
	}
	return c.attachHAStub.ListHaRules(ctx, p)
}

// widenNodes answers each node's storage and network lists. A node in
// storageErr or netErr fails that read, as a node that is down does. A node
// in inactive reports nfs as inactive, and a node in noBridge lists no vmbr0.
type widenNodes struct {
	sdknodes.Service
	storageErr map[string]error
	netErr     map[string]error
	inactive   map[string]bool
	noBridge   map[string]bool
	storageFor map[string]int
}

func (n *widenNodes) ListStorage(_ context.Context, node string, _ *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error) {
	if n.storageFor == nil {
		n.storageFor = map[string]int{}
	}
	n.storageFor[node]++
	if err := n.storageErr[node]; err != nil {
		return nil, err
	}
	active := "1"
	if n.inactive[node] {
		active = "0"
	}
	out := sdknodes.ListStorageResponse{
		json.RawMessage(`{"storage":"nfs","type":"nfs","active":` + active + `,"enabled":1,"content":"images,iso"}`),
	}
	return &out, nil
}

func (n *widenNodes) ListNetwork(_ context.Context, node string, _ *sdknodes.ListNetworkParams) (*sdknodes.ListNetworkResponse, error) {
	if err := n.netErr[node]; err != nil {
		return nil, err
	}
	out := sdknodes.ListNetworkResponse{json.RawMessage(`{"iface":"lo","type":"loopback"}`)}
	if !n.noBridge[node] {
		out = append(out, json.RawMessage(`{"iface":"vmbr0","type":"bridge"}`))
	}
	return &out, nil
}

// widenRig is VM 100 on pve01 with its root disk on nfs, one NIC on vmbr0,
// and a strict pin over pve01 and pve03 that create_vm wrote while pve02 was
// down. Every node is back, reaches nfs, and lists vmbr0.
type widenRig struct {
	deps    Deps
	cluster *widenCluster
	nodes   *widenNodes
	profile *ha.Profile
	cfg     map[string]any
	loc     pve.VMLocation
	obs     *log.Observer
}

func newWidenRig(t *testing.T) *widenRig {
	t.Helper()
	deps, stub, profile := attachRig(config.HAModeAuto)
	stub.pin(true, "pve01:2,pve03", "nfs")
	r := &widenRig{
		deps: deps, cluster: &widenCluster{attachHAStub: stub}, nodes: &widenNodes{}, profile: profile,
		cfg: map[string]any{
			"scsi0": "nfs:vm-100-disk-0,size=10G", "cpu": "x86-64-v3",
			"net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0", "tags": haManagedTag,
		},
		loc: pve.VMLocation{Node: "pve01", Found: true, Tags: haManagedTag},
	}
	q := &icQEMUService{configFn: func(context.Context, string, int) (map[string]any, error) { return r.cfg, nil }}
	r.deps.PVE = &icPVEClient{clusterSvc: r.cluster, qemuSvc: q, nodesSvc: r.nodes}
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	r.deps.Logger, r.obs = logger, obs
	return r
}

func (r *widenRig) touch(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := reconcileTouchedVMHA(ctx, r.deps, "set_vm_metadata", "100", 100, r.loc, r.deps.Log(ctx)); err != nil {
		t.Fatalf("reconcileTouchedVMHA: %v", err)
	}
}

// widen runs the widening step alone, for the cases a touch call's other
// steps would act on, such as a replicate VM.
func (r *widenRig) widen() {
	ctx := context.Background()
	widenNarrowedPin(ctx, r.deps, "set_vm_metadata", 100, r.loc, r.deps.Log(ctx))
}

func (r *widenRig) pinNodes(t *testing.T) (string, bool) {
	t.Helper()
	rule, ok := r.cluster.rules["bosh-na-100"]
	if !ok || rule.Nodes == nil {
		t.Fatal("the pin bosh-na-100 is gone")
	}
	strict := rule.Strict != nil && *rule.Strict
	return *rule.Nodes, strict
}

// requirePinUnchanged checks that the pin still names pve01 and pve03 only,
// strictly, and that no rule was written.
func (r *widenRig) requirePinUnchanged(t *testing.T) {
	t.Helper()
	nodes, strict := r.pinNodes(t)
	if nodes != "pve01:2,pve03" || !strict {
		t.Errorf("pin = %q strict=%v; want the old strict pin pve01:2,pve03", nodes, strict)
	}
	if r.cluster.updateCalls != 0 || r.cluster.createCalls != 0 {
		t.Errorf("rule writes = %d updates, %d creates; want none", r.cluster.updateCalls, r.cluster.createCalls)
	}
}

func pinNodeSet(nodes string) []string {
	var out []string
	for part := range strings.SplitSeq(nodes, ",") {
		name, _, _ := strings.Cut(part, ":")
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// pinTopNodes returns the nodes of a pin's node list that carry its highest
// priority. A node with no priority has priority 0.
func pinTopNodes(nodes string) []string {
	best, out := -1, make([]string, 0, 3)
	for part := range strings.SplitSeq(nodes, ",") {
		name, prio, _ := strings.Cut(part, ":")
		p, _ := strconv.Atoi(prio)
		switch {
		case p > best:
			best, out = p, append(out[:0], name)
		case p == best:
			out = append(out, name)
		}
	}
	return out
}

func TestWidenNarrowedPin_NodeBackWidensThePin(t *testing.T) {
	r := newWidenRig(t)
	r.touch(t)
	nodes, _ := r.pinNodes(t)
	if got := pinNodeSet(nodes); !slices.Equal(got, []string{"pve01", "pve02", "pve03"}) {
		t.Fatalf("pin nodes = %v (%q); want pve01, pve02, and pve03 once pve02 answers", got, nodes)
	}
	rule := r.cluster.rules["bosh-na-100"]
	if rule.Strict != nil && *rule.Strict {
		t.Errorf("pin = %q is strict; with every node admitted it is the record pin create_vm writes", nodes)
	}
	if top := pinTopNodes(nodes); !slices.Contains(top, "pve01") {
		t.Errorf("pin = %q ranks %v above pve01, so the CRM would move the running VM", nodes, top)
	}
	if rule.Comment == nil || haOwnershipOf(*rule.Comment, aaDirector) != haOwnedByUs {
		t.Errorf("widened pin comment = %v; want this Director's marker", rule.Comment)
	}
	if got, ok := ha.VolumeRecord(*rule.Comment); !ok || !slices.Equal(got, []string{"nfs"}) {
		t.Errorf("widened pin volume record = %v; want nfs", got)
	}
	found := false
	for _, e := range r.obs.All() {
		if e.Level == log.LevelInfo && strings.Contains(e.Message, "widened the VM's strict pin") {
			found = true
		}
	}
	if !found {
		t.Error("the widening must log an Info naming the old and new nodes")
	}

	// A second touch finds nothing more to widen.
	writes := r.cluster.updateCalls
	r.touch(t)
	if r.cluster.updateCalls != writes {
		t.Errorf("a second touch wrote the pin again (%d -> %d updates)", writes, r.cluster.updateCalls)
	}
}

func TestWidenNarrowedPin_LeavesThePin(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *widenRig)
	}{
		{"the node still cannot read its storage", func(r *widenRig) {
			r.nodes.storageErr = map[string]error{"pve02": errors.New("pve02: connection refused")}
		}},
		{"the node shows the storage inactive", func(r *widenRig) {
			r.nodes.inactive = map[string]bool{"pve02": true}
		}},
		{"the node still cannot list its bridges", func(r *widenRig) {
			r.nodes.netErr = map[string]error{"pve02": errors.New("pve02: no route to host")}
		}},
		{"the node lacks the VM's bridge", func(r *widenRig) {
			r.nodes.noBridge = map[string]bool{"pve02": true}
		}},
		{"the profile reports the node offline", func(r *widenRig) {
			r.profile.Nodes[1].Online = false
		}},
		{"another Director owns the pin", func(r *widenRig) {
			rule := r.cluster.rules["bosh-na-100"]
			c := ha.WithVolumeRecord(haIdentity{Owner: "dir-other"}.marker(), []string{"nfs"})
			rule.Comment = &c
			r.cluster.rules["bosh-na-100"] = rule
		}},
		{"another Director owns the HA resource", func(r *widenRig) {
			r.cluster.comment = haIdentity{Owner: "dir-other"}.marker()
		}},
		{"the HA resource is held ignored", func(r *widenRig) { r.cluster.state = "ignored" }},
		{"the VM carries the replicate tag", func(r *widenRig) {
			r.loc.Tags = haManagedTag + ";" + haReplicaTag
		}},
		{"the VM is not HA-managed by the CPI", func(r *widenRig) { r.loc.Tags = "" }},
		{"the pin is not strict", func(r *widenRig) { r.cluster.pin(false, "pve01:2,pve03", "nfs") }},
		{"the pin records other storages", func(r *widenRig) { r.cluster.pin(true, "pve01:2,pve03", "nfs", "nfs2") }},
		{"a PCI device holds the VM to its node", func(r *widenRig) {
			r.cluster.pin(true, "pve01", "nfs")
			r.cfg["hostpci0"] = "0000:01:00.0"
		}},
		{"the availability zone leaves the node out", func(r *widenRig) {
			r.deps.Config.Placement = &config.PlacementConfig{AZMap: map[string][]string{
				"z1": {"pve01", "pve03"}, "z2": {"pve02"},
			}}
		}},
		{"the pin's nodes span no single availability zone", func(r *widenRig) {
			r.deps.Config.Placement = &config.PlacementConfig{AZMap: map[string][]string{
				"z1": {"pve01"}, "z2": {"pve02", "pve03"},
			}}
		}},
		{"mode off", func(r *widenRig) { r.deps.Config.HA = &config.HAConfig{Mode: haChoiceOf(config.HAModeOff)} }},
		{"the request carries no director_uuid", func(r *widenRig) { r.deps.RequestDirectorUUID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newWidenRig(t)
			tc.setup(r)
			before := r.cluster.rules["bosh-na-100"]
			r.widen()
			after := r.cluster.rules["bosh-na-100"]
			if *before.Nodes != *after.Nodes || *before.Strict != *after.Strict {
				t.Errorf("pin changed from %q strict=%v to %q strict=%v", *before.Nodes, *before.Strict,
					*after.Nodes, *after.Strict)
			}
			if r.cluster.updateCalls != 0 || r.cluster.createCalls != 0 {
				t.Errorf("rule writes = %d updates, %d creates; want none", r.cluster.updateCalls, r.cluster.createCalls)
			}
		})
	}
}

// An availability zone that holds every node keeps the widening inside it.
func TestWidenNarrowedPin_StaysInsideTheAZ(t *testing.T) {
	r := newWidenRig(t)
	r.deps.Config.Placement = &config.PlacementConfig{AZMap: map[string][]string{
		"z1": {"pve01", "pve02", "pve03"}, "z2": {"pve04"},
	}}
	r.touch(t)
	nodes, _ := r.pinNodes(t)
	if got := pinNodeSet(nodes); !slices.Equal(got, []string{"pve01", "pve02", "pve03"}) {
		t.Fatalf("pin nodes = %v; want z1's three nodes", got)
	}
}

// A pin another call changes between the widening's read and its write under
// the rules lock is left to that call.
func TestWidenNarrowedPin_ConcurrentChangeWins(t *testing.T) {
	r := newWidenRig(t)
	r.cluster.beforeRead = map[int]func(){2: func() { r.cluster.pin(true, "pve01:2", "nfs") }}
	r.touch(t)
	nodes, strict := r.pinNodes(t)
	if nodes != "pve01:2" || !strict {
		t.Errorf("pin = %q strict=%v; want the concurrent pin pve01:2 left as it is", nodes, strict)
	}
	if r.cluster.updateCalls != 0 {
		t.Errorf("rule updates = %d; want none", r.cluster.updateCalls)
	}
}

// A failure to read the HA rules is logged and never fails the touch call.
func TestWidenNarrowedPin_ReadFailureOnlyLogs(t *testing.T) {
	r := newWidenRig(t)
	r.cluster.getErr = errors.New("pve 503")
	r.touch(t)
	if _, ok := hasWarn(r.obs, "could not be checked for nodes it may now include"); !ok {
		t.Error("a failed widening check must log a Warn")
	}
	r.requirePinUnchanged(t)
}

func TestCreatedVMBridgeConstraints_UnreadableNodeIsLeftOutWithAWarn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	deps, client, obs := bridgeDeps(t, nil, map[string][]string{"pve1": {"vmbr0"}, "pve3": {"vmbr0"}}, "warn", nil)
	client.nodes.errs = map[string]error{"pve2": errors.New("pve2: connection refused")}
	got := createdVMBridgeConstraints(ctx, deps, oneNet(), []string{"pve1", "pve2", "pve3"}, deps.Log(ctx))
	if len(got) != 1 || got[0].Name != "bridge vmbr0" || !slices.Equal(got[0].Nodes, []string{"pve1", "pve3"}) {
		t.Fatalf("constraints = %+v; want bridge vmbr0 over pve1 and pve3", got)
	}
	e, ok := hasWarn(obs, "leaves out a node whose bridges could not be read")
	if !ok {
		t.Fatal("an unreadable node must be named in a Warn")
	}
	if e.Attrs["node"] != "pve2" || e.Attrs["bridge"] != "vmbr0" {
		t.Errorf("warn attrs = %v; want node pve2 and bridge vmbr0", e.Attrs)
	}
}
