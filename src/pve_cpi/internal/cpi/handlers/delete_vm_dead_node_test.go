package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// These tests drive the legacy delete of VM 101 through the handler while its
// node, pve-node2, is dead and PVE HA holds the service in recovery. The CRM
// view comes from the delete scene, and a hook on its manager reads plays the
// CRM's recovery: once the VM's pin is strict and written by the CPI, and no
// anti-affinity rule holds the VM, the CRM moves the config onto the first
// online node the pin names and leaves the VM stopped there, because the
// requested state is stopped.

const (
	deadNodeOwner = "director-1"
	deadNode      = "pve-node2"
)

// deadNodeMarker is the ownership marker the CPI writes for deadNodeOwner.
var deadNodeMarker = "bosh-cpi:v1 owner=" + deadNodeOwner + " deployment=- group=-"

// deadNodeStorage serves a fixed storage index.
type deadNodeStorage struct {
	clusterstorage.Service
	entries []map[string]any
	raw     []json.RawMessage
	err     error
}

func (s *deadNodeStorage) ListStorage(
	context.Context, *clusterstorage.ListStorageParams,
) (*clusterstorage.ListStorageResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := clusterstorage.ListStorageResponse{}
	for _, e := range s.entries {
		raw, _ := json.Marshal(e)
		out = append(out, raw)
	}
	out = append(out, s.raw...)
	return &out, nil
}

// deadNodeCluster wraps the scene's cluster service so a test can lose the
// answer of a write that landed, fail a read, or run a hook on a rules read.
type deadNodeCluster struct {
	*handlers.HASceneCluster
	mu sync.Mutex
	// loseRuleAnswers makes that many rule writes land and then answer 500.
	loseRuleAnswers int
	// loseStopAnswer makes the next resource write land and answer 500.
	loseStopAnswer bool
	// failGetResource fails every HA resource read.
	failGetResource bool
	// ruleErrors adds PVE feasibility errors to the named rule on every read.
	ruleErrors map[string]string
	// onRulesRead runs before every rules read with its count.
	onRulesRead func(n int)
	rulesReads  int
}

var errLostAnswer = errors.New("500 Internal Server Error: proxy loop timed out (HTTP500)")

func (c *deadNodeCluster) takeRuleLoss() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loseRuleAnswers > 0 {
		c.loseRuleAnswers--
		return true
	}
	return false
}

func (c *deadNodeCluster) CreateHaRules(ctx context.Context, params *cluster.CreateHaRulesParams) error {
	if err := c.HASceneCluster.CreateHaRules(ctx, params); err != nil {
		return err
	}
	if c.takeRuleLoss() {
		return errLostAnswer
	}
	return nil
}

func (c *deadNodeCluster) UpdateHaRules(ctx context.Context, rule string, params *cluster.UpdateHaRulesParams) error {
	if err := c.HASceneCluster.UpdateHaRules(ctx, rule, params); err != nil {
		return err
	}
	if c.takeRuleLoss() {
		return errLostAnswer
	}
	return nil
}

func (c *deadNodeCluster) UpdateHaResources(
	ctx context.Context, sid string, params *cluster.UpdateHaResourcesParams,
) error {
	if err := c.HASceneCluster.UpdateHaResources(ctx, sid, params); err != nil {
		return err
	}
	c.mu.Lock()
	lose := c.loseStopAnswer
	c.loseStopAnswer = false
	c.mu.Unlock()
	if lose {
		return errLostAnswer
	}
	return nil
}

func (c *deadNodeCluster) GetHaResources(ctx context.Context, sid string) (*cluster.GetHaResourcesResponse, error) {
	c.mu.Lock()
	fail := c.failGetResource
	c.mu.Unlock()
	if fail {
		return nil, errors.New("503 Service Unavailable (HTTP503)")
	}
	return c.HASceneCluster.GetHaResources(ctx, sid)
}

func (c *deadNodeCluster) ListHaRules(
	ctx context.Context, params *cluster.ListHaRulesParams,
) (*cluster.ListHaRulesResponse, error) {
	c.mu.Lock()
	c.rulesReads++
	n, hook := c.rulesReads, c.onRulesRead
	inject := c.ruleErrors
	c.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	resp, err := c.HASceneCluster.ListHaRules(ctx, params)
	if err != nil || len(inject) == 0 {
		return resp, err
	}
	out := cluster.ListHaRulesResponse{}
	for _, raw := range *resp {
		var row map[string]any
		_ = json.Unmarshal(raw, &row)
		if msg, ok := inject[row["rule"].(string)]; ok {
			row["errors"] = map[string]any{"resources": []string{msg}}
		}
		b, _ := json.Marshal(row)
		out = append(out, b)
	}
	return &out, nil
}

// deadNodeQEMU answers the snapshot list of a VM that has none.
type deadNodeQEMU struct{ *mockQEMUService }

func (deadNodeQEMU) ListSnapshots(context.Context, string, int) ([]map[string]any, error) {
	return []map[string]any{{"name": "current"}}, nil
}

// deadNodeDeleteFixture is one dead-node delete of VM 101.
type deadNodeDeleteFixture struct {
	t        *testing.T
	scene    *handlers.HADeleteScene
	cluster  *deadNodeCluster
	storage  *deadNodeStorage
	deps     handlers.Deps
	episodes *ha.EpisodeStore
	// configs are the VM's configs by node once the CRM moved it there.
	configs map[string]map[string]any
	// scanLags keeps the scan on the dead node after the CRM moved the VM.
	scanLags atomic.Bool
	// gone is the VM after its destroy: every config read answers not found.
	gone atomic.Bool
	// pinAtMove is the pin's nodes when the CRM moved the VM.
	pinAtMove atomic.Value
	// pinCommentAtMove is the pin's comment when the CRM moved the VM.
	pinCommentAtMove atomic.Value
}

// newDeadNodeDelete builds the fixture: VM 101 requests started, the CRM
// holds it in recovery on the dead pve-node2, its HA resource carries this
// Director's marker, the VM has no pin, so every member is admissible, and the
// recovery episode began an hour ago, so the grace window has expired.
func newDeadNodeDelete(t *testing.T) *deadNodeDeleteFixture {
	t.Helper()
	t.Cleanup(handlers.SetHADeleteMotionPollForTest(10 * time.Millisecond))
	f := &deadNodeDeleteFixture{t: t, scene: handlers.NewHADeleteScene(), configs: map[string]map[string]any{}}
	f.scene.Manage(101, deadNode, "started", "recovery")
	f.scene.MarkNodeDead(deadNode)
	f.scene.Comments["vm:101"] = deadNodeMarker
	f.storage = &deadNodeStorage{entries: []map[string]any{
		{"storage": "ceph", "type": "rbd", "shared": 1, "content": "images"},
		{"storage": "backups", "type": "dir", "content": "backup"},
	}}
	f.scene.OnRead = f.crmRecovery
	f.episodes = &ha.EpisodeStore{Dir: filepath.Join(t.TempDir(), "cache"), Now: func() time.Time { return time.Now().Add(-time.Hour) }}
	if _, err := f.episodes.Observe("vm:101", "uid-101"); err != nil {
		t.Fatal(err)
	}
	f.episodes.Now = nil

	qemuSvc := deadNodeQEMU{&mockQEMUService{
		configFn: func(_ context.Context, node string, _ int) (map[string]any, error) {
			if f.gone.Load() {
				return nil, notFoundAPIErr()
			}
			if node == deadNode && f.nodeOffline(deadNode) {
				return nil, &sdkerrors.APIError{HTTPCode: 595, Message: "no route to host"}
			}
			var cfg map[string]any
			f.scene.Do(func(s *handlers.HADeleteScene) {
				if s.IndexNode[101] == node {
					cfg = f.configs[node]
					if cfg == nil {
						cfg = map[string]any{"scsi0": "ceph:vm-101-disk-0,size=10G"}
					}
				}
			})
			if cfg == nil {
				return nil, notFoundAPIErr()
			}
			return cfg, nil
		},
	}}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, node, _ string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			f.gone.Store(true)
			f.scene.Do(func(s *handlers.HADeleteScene) {
				s.Events = append(s.Events, "DeleteQemu:"+node)
				if params != nil && params.Purge != nil && *params.Purge {
					delete(s.Requested, "vm:101")
					delete(s.Services, "vm:101")
				}
				delete(s.IndexNode, 101)
			})
			return &nodes.DeleteQemuResponse{}, nil
		},
	}
	tasksSvc := &mockTasksService{
		waitFn: func(_ context.Context, _, _ string, _ *tasks.WaitOptions) (*tasks.Status, error) {
			return &tasks.Status{ExitStatus: "OK"}, nil
		},
	}
	agentSvc := &mockAgentService{removeFn: func(_ context.Context, _ string, _ int) error { return nil }}
	f.cluster = &deadNodeCluster{HASceneCluster: &handlers.HASceneCluster{
		Service: &mockClusterSvc{listConfigNodesFn: func(context.Context) (*cluster.ListConfigNodesResponse, error) {
			return handlers.AbsenceConfigNodes("pve-node1", "pve-node2", "pve-node3"), nil
		}},
		Scene:    f.scene,
		UseIndex: true,
	}}
	cfg := dlbEnabledConfig()
	f.deps = testDepsFoundVM(101, qemuSvc.mockQEMUService, nodesSvc, tasksSvc, agentSvc)
	f.deps.Config = cfg
	f.deps.RequestDirectorUUID = deadNodeOwner
	f.deps.HAEpisodes = f.episodes
	f.deps.HAProfile = func(context.Context) (*ha.Profile, error) {
		p := &ha.Profile{HAVersionOK: true}
		for _, n := range []string{"pve-node1", "pve-node2", "pve-node3"} {
			p.Nodes = append(p.Nodes, ha.NodeFacts{Name: n, Online: n != deadNode, Version: "9.1.2"})
		}
		return p, nil
	}
	f.deps.PVE = &handlers.HAScenePermsClient{Scene: f.scene, Client: &mockPVEClient{
		qemuSvc:           &handlers.HASceneQEMU{Service: qemuSvc, Scene: f.scene},
		nodesSvc:          &handlers.HASceneNodes{Service: nodesSvc, Scene: f.scene},
		tasksSvc:          tasksSvc,
		storageSvc:        &mockStorageService{},
		clusterSvc:        f.cluster,
		clusterStorageSvc: f.storage,
		poolsSvc:          &noopPoolService{},
	}}
	return f
}

// movedTo sets the scene as the completion leaves it once the CRM has moved
// the config onto node: the resource requests stopped and carries the
// dead-node token, and the VM is stopped on node.
func (f *deadNodeDeleteFixture) movedTo(node string) {
	f.scene.Do(func(s *handlers.HADeleteScene) {
		s.Services["vm:101"] = handlers.HASceneService{State: "stopped", Node: node, UID: "uid-101"}
		s.Requested["vm:101"] = "stopped"
		s.Comments["vm:101"] = deadNodeMarker + " dead-node-from=" + deadNode
		s.IndexNode[101] = node
		s.IndexHAState[101] = "stopped"
		s.Power[101] = "stopped"
	})
}

func (f *deadNodeDeleteFixture) nodeOffline(node string) bool {
	offline := false
	f.scene.Do(func(s *handlers.HADeleteScene) { offline = s.Offline[node] })
	return offline
}

// crmRecovery is the CRM's recovery round, run under the scene's lock at the
// start of every manager status read.
func (f *deadNodeDeleteFixture) crmRecovery(s *handlers.HADeleteScene, _ int) {
	svc, ok := s.Services["vm:101"]
	if !ok || svc.State != "recovery" || s.Requested["vm:101"] != "stopped" {
		return
	}
	for _, rule := range s.Rules {
		if rule["type"] == "resource-affinity" && rule["affinity"] == "negative" &&
			strings.Contains(rule["resources"].(string), "vm:101") {
			return
		}
	}
	pin, ok := s.Rules["bosh-na-101"]
	if !ok || pin["strict"] != true {
		return
	}
	nodesCSV, _ := pin["nodes"].(string)
	target := ""
	for _, n := range strings.Split(nodesCSV, ",") {
		n, _, _ = strings.Cut(n, ":")
		if s.Offline[n] {
			continue
		}
		target = n
		break
	}
	if target == "" {
		return
	}
	f.pinAtMove.Store(nodesCSV)
	comment, _ := pin["comment"].(string)
	f.pinCommentAtMove.Store(comment)
	s.Events = append(s.Events, "CRMRecovered:"+target)
	s.Services["vm:101"] = handlers.HASceneService{State: "stopped", Node: target, UID: svc.UID}
	s.Power[101] = "stopped"
	if !f.scanLags.Load() {
		s.IndexNode[101] = target
		s.IndexHAState[101] = "stopped"
	}
}

func (f *deadNodeDeleteFixture) run() error {
	_, err := handlers.HandleDeleteVM(f.deps).Handle(context.Background(), marshalArgs("101"),
		jsonrpc.Context{DirectorUUID: f.deps.RequestDirectorUUID})
	return err
}

func (f *deadNodeDeleteFixture) events() []string { return f.scene.EventsSnapshot() }

// pin presets VM 101's strict pin, this Director's, naming nodes.
func (f *deadNodeDeleteFixture) pin(nodes string, extra map[string]any) {
	rule := map[string]any{"type": "node-affinity", "resources": "vm:101", "nodes": nodes, "strict": 1,
		"comment": deadNodeMarker}
	for k, v := range extra {
		rule[k] = v
	}
	f.scene.Rules["bosh-na-101"] = rule
}

// requireNoRuleWrites fails when the delete changed an HA rule or destroyed
// anything.
func (f *deadNodeDeleteFixture) requireNoRuleWrites() {
	f.t.Helper()
	for _, e := range f.events() {
		if strings.HasPrefix(e, "CreateHaRules") || strings.HasPrefix(e, "UpdateHaRules") ||
			strings.HasPrefix(e, "DeleteHaRules") || strings.HasPrefix(e, "DeleteQemu") {
			f.t.Fatalf("no rule may change and nothing may be destroyed; events: %v", f.events())
		}
	}
}

// requireNoWrites fails when the delete wrote anything at all.
func (f *deadNodeDeleteFixture) requireNoWrites() {
	f.t.Helper()
	if ev := f.events(); len(ev) != 0 {
		f.t.Fatalf("a deferred completion must write nothing; events: %v", ev)
	}
}

// requireCompletedOn fails unless the delete destroyed the VM on node, after
// the stop request and the CRM's move.
func (f *deadNodeDeleteFixture) requireCompletedOn(node string) {
	f.t.Helper()
	ev := f.events()
	stop := eventIndex(ev, "UpdateHaResources:vm:101=stopped")
	moved := eventIndex(ev, "CRMRecovered:"+node)
	destroy := eventIndex(ev, "DeleteQemu:"+node)
	if stop == -1 || moved == -1 || destroy == -1 || stop > moved || moved > destroy {
		f.t.Fatalf("want the stop request, the move to %s, and the destroy there in that order; events: %v", node, ev)
	}
	if at := eventIndex(ev, "DeleteQemu:"+deadNode); at != -1 {
		f.t.Fatalf("nothing may be destroyed on the dead node; events: %v", ev)
	}
	if at := eventIndex(ev, "DeleteHaRules:bosh-na-101"); at != -1 && at < destroy {
		f.t.Fatalf("the pin must stay until the destroy; events: %v", ev)
	}
}

// TestDeadNodeDelete_PinNamesOnlyTheNodeThatReachesTheStorage puts the VM on
// storage that pve-node2 and pve-node3 reach, so its strict pin names those
// two, and kills pve-node2. The new pin must name pve-node3 alone, so the CRM
// can never steal the config onto pve-node1. The failed node's own stale LRM
// timestamp does not block it, and the pin keeps the old pin's comment.
func TestDeadNodeDelete_PinNamesOnlyTheNodeThatReachesTheStorage(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.storage.entries = []map[string]any{
		{"storage": "nfs-23", "type": "nfs", "shared": 1, "nodes": "pve-node2,pve-node3", "content": "images,iso"},
		{"storage": "backups", "type": "dir", "content": "backup"},
	}
	pinComment := "bosh-cpi:v1 owner=" + deadNodeOwner + " deployment=cf group=router"
	f.pin("pve-node2:2,pve-node3", map[string]any{"comment": pinComment})
	f.configs["pve-node3"] = map[string]any{"scsi0": "nfs-23:101/vm-101-disk-0.raw,size=10G"}

	if err := f.run(); err != nil {
		t.Fatalf("the delete must complete on the deletion destination: %v", err)
	}
	f.requireCompletedOn("pve-node3")
	if got, _ := f.pinAtMove.Load().(string); got != "pve-node3" {
		t.Fatalf("the pin named %q when the CRM moved the VM; want pve-node3 alone", got)
	}
	if got, _ := f.pinCommentAtMove.Load().(string); got != pinComment+" dead-node-from="+deadNode {
		t.Fatalf("the pin's comment is %q; want the old comment with the dead-node token", got)
	}
	ev := f.events()
	if eventIndex(ev, "UpdateHaRules:bosh-na-101=vm:101") == -1 {
		t.Fatalf("the completion must replace the VM's pin in place; events: %v", ev)
	}
	var comment string
	f.scene.Do(func(s *handlers.HADeleteScene) { comment = s.Comments["vm:101"] })
	if !strings.Contains(comment, "dead-node-from="+deadNode) || !strings.HasPrefix(comment, deadNodeMarker) {
		t.Fatalf("the resource comment keeps the marker and names the dead node: %q", comment)
	}
}

// TestDeadNodeDelete_CompletesOnAClusterWithNodeLocalStorage is the common
// cluster: every node also carries the stock local and local-lvm storages, and
// a disabled NFS storage is left in the index. The VM's disks are all on Ceph,
// so it has no strict pin, and those storages must not block the delete.
func TestDeadNodeDelete_CompletesOnAClusterWithNodeLocalStorage(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.storage.entries = []map[string]any{
		{"storage": "ceph", "type": "rbd", "shared": 1, "content": "images"},
		{"storage": "local", "type": "dir", "content": "iso,vztmpl,backup"},
		{"storage": "local-lvm", "type": "lvmthin", "content": "images,rootdir"},
		{"storage": "nfs-old", "type": "nfs", "shared": 1, "disable": 1, "content": "images"},
	}
	if err := f.run(); err != nil {
		t.Fatalf("the delete must complete: %v", err)
	}
	f.requireCompletedOn("pve-node1")
	if got, _ := f.pinAtMove.Load().(string); got != "pve-node1,pve-node3" {
		t.Fatalf("the pin named %q at the move; want every live node", got)
	}
	if got, _ := f.pinCommentAtMove.Load().(string); got != deadNodeMarker+" dead-node-from="+deadNode {
		t.Fatalf("a new pin starts from the marker: %q", got)
	}
}

// TestDeadNodeDelete_StrictPinThatBlockedRecovery replaces a strict pin that
// named the dead node with a priority and one live node. The new pin names
// the live node alone, at priority 0, and replaces the old one in place.
func TestDeadNodeDelete_StrictPinThatBlockedRecovery(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.pin("pve-node1,pve-node2:2", nil)
	if err := f.run(); err != nil {
		t.Fatalf("the delete must complete: %v", err)
	}
	f.requireCompletedOn("pve-node1")
	if got, _ := f.pinAtMove.Load().(string); got != "pve-node1" {
		t.Fatalf("the pin named %q at the move; want pve-node1 alone at priority 0", got)
	}
	if eventIndex(f.events(), "UpdateHaRules:bosh-na-101=vm:101") == -1 {
		t.Fatalf("the old pin is replaced in place; events: %v", f.events())
	}
}

// TestDeadNodeDelete_FullAntiAffinityGroup removes the VM from a full
// anti-affinity rule. The VM has no strict pin, so every node the rules allow
// is admissible, and the release goes first, then the pin.
func TestDeadNodeDelete_FullAntiAffinityGroup(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.scene.Rules["bosh-aa-0123456789ab"] = map[string]any{
		"type": "resource-affinity", "affinity": "negative", "resources": "vm:101,vm:102,vm:103",
		"comment": deadNodeMarker + " deployment=cf group=router",
	}
	if err := f.run(); err != nil {
		t.Fatalf("the delete must complete: %v", err)
	}
	f.requireCompletedOn("pve-node1")
	ev := f.events()
	release := eventIndex(ev, "UpdateHaRules:bosh-aa-0123456789ab=vm:102,vm:103")
	pin := eventIndex(ev, "CreateHaRules:bosh-na-101")
	if release == -1 || pin == -1 || release > pin {
		t.Fatalf("want the anti-affinity release and then the pin; events: %v", ev)
	}
}

// TestDeadNodeDelete_PinGoesFirstWhenTheRulesAllowAnUnreachableNode has a VM
// whose strict pin names pve-node2 and pve-node3 and that PVE disabled, so the
// current rules allow pve-node1, which is outside the reach. The completion
// writes a bridge pin of the destinations and the dead node, releases the
// anti-affinity rule, which is deleted because it held two, and then writes
// the final pin, so the CRM never recovers the VM onto pve-node1 in between.
func TestDeadNodeDelete_PinGoesFirstWhenTheRulesAllowAnUnreachableNode(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.storage.entries = []map[string]any{
		{"storage": "nfs-23", "type": "nfs", "shared": 1, "nodes": "pve-node2,pve-node3", "content": "images"},
	}
	f.pin("pve-node2,pve-node3", map[string]any{"disable": 1})
	f.configs["pve-node3"] = map[string]any{"scsi0": "nfs-23:101/vm-101-disk-0.raw,size=10G"}
	f.scene.Rules["bosh-aa-0123456789ab"] = map[string]any{
		"type": "resource-affinity", "affinity": "negative", "resources": "vm:101,vm:102", "comment": deadNodeMarker,
	}
	if err := f.run(); err != nil {
		t.Fatalf("the delete must complete: %v", err)
	}
	f.requireCompletedOn("pve-node3")
	requirePinReleasePin(t, f.events())
}

// requirePinReleasePin fails unless the events hold a pin write, then the
// release of the anti-affinity rule, then a second pin write.
func requirePinReleasePin(t *testing.T, ev []string) {
	t.Helper()
	var pins []int
	for i, e := range ev {
		if e == "UpdateHaRules:bosh-na-101=vm:101" {
			pins = append(pins, i)
		}
	}
	release := eventIndex(ev, "DeleteHaRules:bosh-aa-0123456789ab")
	if len(pins) != 2 || release == -1 || pins[0] > release || release > pins[1] {
		t.Fatalf("want the bridge pin, the release, and the final pin in that order; events: %v", ev)
	}
}

// TestDeadNodeDelete_PinToTheDeadNodeAloneKeepsDeferring covers a hold VM and
// a VM whose persistent disk lives on node-local storage of the dead node.
// Both carry a strict pin to the dead node alone, which is the CPI's record
// that no other node reaches their volumes, so the delete keeps deferring,
// names the operator's steps, and writes nothing. Node-local storage in the
// index does not change that answer either way.
func TestDeadNodeDelete_PinToTheDeadNodeAloneKeepsDeferring(t *testing.T) {
	cases := []struct {
		name    string
		nodes   string
		storage map[string]any
	}{
		{"hold VM on local LVM", "pve-node2", map[string]any{"storage": "local-lvm", "type": "lvmthin",
			"content": "images,rootdir"}},
		{"persistent disk on local ZFS", "pve-node2:2", map[string]any{"storage": "local-zfs", "type": "zfspool",
			"content": "images"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeadNodeDelete(t)
			f.storage.entries = append(f.storage.entries, tc.storage)
			f.pin(tc.nodes, nil)
			requireHARetriable(t, f.run(), "no deletion destination", "admits dead node "+deadNode+" alone",
				"Bring the node back", "bosh cck")
			f.requireNoWrites()
		})
	}
}

// TestDeadNodeDelete_ObserveOnlyTokenKeepsDeferring needs Sys.Console for the
// rule edits, so a token without it defers and names the privilege.
func TestDeadNodeDelete_ObserveOnlyTokenKeepsDeferring(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.scene.Perms = map[string]bool{"VM.PowerMgmt": true, "VM.Allocate": true}
	requireHARetriable(t, f.run(), "Sys.Console", "observe-only")
	f.requireNoWrites()
}

// TestDeadNodeDelete_AccessSetWithoutALiveFreshNodeKeepsDeferring kills both
// nodes of the access set, and separately leaves the other one online with a
// stale LRM timestamp. Neither may receive the config.
func TestDeadNodeDelete_AccessSetWithoutALiveFreshNodeKeepsDeferring(t *testing.T) {
	cases := []struct {
		name  string
		setup func(s *handlers.HADeleteScene)
	}{
		{"both access nodes dead", func(s *handlers.HADeleteScene) {
			s.Offline["pve-node3"] = true
			s.NodeState["pve-node3"] = "unknown"
			s.LRMAge["pve-node3"] = 4 * 3600
		}},
		{"the other access node has a stale LRM", func(s *handlers.HADeleteScene) { s.LRMAge["pve-node3"] = 3600 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeadNodeDelete(t)
			f.pin("pve-node2,pve-node3", nil)
			f.scene.Do(tc.setup)
			requireHARetriable(t, f.run(), "no deletion destination", "pve-node3",
				"not online with a fresh LRM timestamp")
			f.requireNoWrites()
		})
	}
}

// TestDeadNodeDelete_GraceWindowStillRunning keeps the gate's deferral while
// the window runs, and writes nothing.
func TestDeadNodeDelete_GraceWindowStillRunning(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.deps.HAEpisodes = &ha.EpisodeStore{Dir: filepath.Join(t.TempDir(), "cache")}
	requireHARetriable(t, f.run(), "grace window remain")
	f.requireNoWrites()
}

// TestDeadNodeDelete_RefusalsWriteNothing covers the read-only checks that
// keep the delete deferring before the first write.
func TestDeadNodeDelete_RefusalsWriteNothing(t *testing.T) {
	off := config.HAChoice(config.HAModeOff)
	cases := []struct {
		name  string
		setup func(f *deadNodeDeleteFixture)
		want  []string
	}{
		{"mode off", func(f *deadNodeDeleteFixture) {
			cfg := *f.deps.Config
			cfg.HA = &config.HAConfig{Mode: &off}
			f.deps.Config = &cfg
		}, []string{"pve.ha.mode is off"}},
		{"no director uuid", func(f *deadNodeDeleteFixture) { f.deps.RequestDirectorUUID = "" },
			[]string{"no director_uuid"}},
		{"foreign resource marker", func(f *deadNodeDeleteFixture) {
			f.scene.Comments["vm:101"] = "bosh-cpi:v1 owner=other deployment=- group=-"
		}, []string{"does not carry this Director's ownership marker"}},
		{"unmarked resource", func(f *deadNodeDeleteFixture) { delete(f.scene.Comments, "vm:101") },
			[]string{"does not carry this Director's ownership marker"}},
		{"operator node-affinity rule", func(f *deadNodeDeleteFixture) {
			f.scene.Rules["operator-pin"] = map[string]any{"type": "node-affinity", "resources": "vm:101",
				"nodes": "pve-node2"}
		}, []string{"cannot be narrowed", "operator-pin"}},
		{"foreign pin", func(f *deadNodeDeleteFixture) {
			f.scene.Rules["bosh-na-101"] = map[string]any{"type": "node-affinity", "resources": "vm:101",
				"nodes": "pve-node2", "strict": 1, "comment": "bosh-cpi:v1 owner=other"}
		}, []string{"cannot be narrowed", "ownership marker"}},
		{"strict pin that leaves out the dead node", func(f *deadNodeDeleteFixture) { f.pin("pve-node1", nil) },
			[]string{"compute the deletion destinations", "does not name dead node " + deadNode}},
		{"pin of another resource too", func(f *deadNodeDeleteFixture) {
			f.pin("pve-node1,pve-node2", map[string]any{"resources": "vm:101,vm:102"})
		}, []string{"cannot be narrowed", "not vm:101 alone"}},
		{"HA resource unreadable", func(f *deadNodeDeleteFixture) { f.cluster.failGetResource = true },
			[]string{"read HA resource vm:101"}},
		{"rules on a PVE 8 node", func(f *deadNodeDeleteFixture) {
			f.deps.HAProfile = func(context.Context) (*ha.Profile, error) {
				return &ha.Profile{HAVersionOK: true, Nodes: []ha.NodeFacts{{Name: "pve-node1", Version: "8.4.1"}}}, nil
			}
		}, []string{"HA rules cannot be written"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeadNodeDelete(t)
			tc.setup(f)
			requireHARetriable(t, f.run(), tc.want...)
			f.requireNoWrites()
		})
	}
}

// TestDeadNodeDelete_NodeComesBackBeforeTheRuleEdits proves the fence again
// under the rules lock, so a node that returned after the first read stops
// the completion before any rule changes.
func TestDeadNodeDelete_NodeComesBackBeforeTheRuleEdits(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.cluster.onRulesRead = func(n int) {
		if n == 1 {
			f.scene.Do(func(s *handlers.HADeleteScene) {
				s.Offline[deadNode] = false
				s.NodeState[deadNode] = "online"
				s.LRMAge[deadNode] = 2
			})
		}
	}
	requireHARetriable(t, f.run(), "the fence is no longer proven")
	f.requireNoRuleWrites()
}

// TestDeadNodeDelete_NewRecoveryEpisodeBeforeTheRuleEdits has the CRM start a
// new recovery episode, with a new uid, after the first proof. The proof under
// the rules lock sees the new uid and changes no rule, because the grace
// window measured the old episode.
func TestDeadNodeDelete_NewRecoveryEpisodeBeforeTheRuleEdits(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.cluster.onRulesRead = func(n int) {
		if n == 1 {
			f.scene.Do(func(s *handlers.HADeleteScene) {
				svc := s.Services["vm:101"]
				svc.UID = "uid-202"
				s.Services["vm:101"] = svc
			})
		}
	}
	requireHARetriable(t, f.run(), "new recovery episode", "uid uid-202, was uid-101")
	f.requireNoRuleWrites()
}

// TestDeadNodeDelete_RulesChangeBeforeTheRuleEdits has an operator add a
// node-affinity rule for the VM after the first read. The check under the
// rules lock refuses it before any rule changes.
func TestDeadNodeDelete_RulesChangeBeforeTheRuleEdits(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.cluster.onRulesRead = func(n int) {
		if n == 2 {
			f.scene.Do(func(s *handlers.HADeleteScene) {
				s.Rules["operator-pin"] = map[string]any{"type": "node-affinity", "resources": "vm:101",
					"nodes": "pve-node1"}
			})
		}
	}
	requireHARetriable(t, f.run(), "cannot be narrowed", "operator-pin")
	f.requireNoRuleWrites()
}

// TestDeadNodeDelete_NodeComesBackWhileTheCRMMoves has the dead node return
// while the completion waits, and the CRM leave the VM stopped on it. That
// node is not a deletion destination, so the call answers retriable and
// destroys nothing. The retry runs the normal delete on the returned node:
// the dead-node token there names the node the VM is on, so the volume check
// passes, and the VM is destroyed there and nowhere else.
func TestDeadNodeDelete_NodeComesBackWhileTheCRMMoves(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.scene.OnRead = func(s *handlers.HADeleteScene, _ int) {
		if _, ok := s.Rules["bosh-na-101"]; !ok || f.gone.Load() {
			return
		}
		s.Offline[deadNode] = false
		s.NodeState[deadNode] = "online"
		s.LRMAge[deadNode] = 2
		s.Services["vm:101"] = handlers.HASceneService{State: "stopped", Node: deadNode, UID: "uid-101"}
		s.IndexHAState[101] = "stopped"
		s.Power[101] = "stopped"
	}
	requireHARetriable(t, f.run(), "dead node "+deadNode+" is back", "runs the delete against that node")
	if at := eventIndex(f.events(), "DeleteQemu"); at != -1 {
		t.Fatalf("nothing may be destroyed; events: %v", f.events())
	}

	if err := f.run(); err != nil {
		t.Fatalf("the retry must delete the VM on the returned node: %v", err)
	}
	ev := f.events()
	if eventIndex(ev, "DeleteQemu:"+deadNode) == -1 {
		t.Fatalf("the retry must destroy the VM on the returned node; events: %v", ev)
	}
	for _, e := range ev {
		if strings.HasPrefix(e, "DeleteQemu:") && e != "DeleteQemu:"+deadNode {
			t.Fatalf("nothing may be destroyed on another node; events: %v", ev)
		}
	}
}

// TestDeadNodeDelete_ScanLagsBehindTheMove ends the first call while the scan
// still shows the dead node, after the CRM had moved the config. The retry
// finds the VM on the destination, checks the volumes against the dead-node
// token, and completes there.
func TestDeadNodeDelete_ScanLagsBehindTheMove(t *testing.T) {
	f := newDeadNodeDelete(t)
	one := 1
	cfg := *f.deps.Config
	ha := *cfg.HA
	ha.MotionWaitSec = &one
	cfg.HA = &ha
	f.deps.Config = &cfg
	f.scanLags.Store(true)

	requireHARetriable(t, f.run(), "did not move the config", "next retry resumes")
	if at := eventIndex(f.events(), "DeleteQemu"); at != -1 {
		t.Fatalf("the first call must destroy nothing; events: %v", f.events())
	}
	f.scene.Do(func(s *handlers.HADeleteScene) {
		s.IndexNode[101] = "pve-node1"
		s.IndexHAState[101] = "stopped"
	})
	if err := f.run(); err != nil {
		t.Fatalf("the retry must complete on the destination: %v", err)
	}
	f.requireCompletedOn("pve-node1")
}

// TestDeadNodeDelete_LostWriteAnswers loses the answer of the stop request
// and of the pin write, both of which landed. The read-back counts each as
// landed, and the delete completes.
func TestDeadNodeDelete_LostWriteAnswers(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.cluster.loseStopAnswer = true
	f.cluster.loseRuleAnswers = 1
	if err := f.run(); err != nil {
		t.Fatalf("lost answers of landed writes must not stop the completion: %v", err)
	}
	f.requireCompletedOn("pve-node1")
}

// TestDeadNodeDelete_PinNotInEffectStopsBeforeTheAntiAffinity has PVE report
// the VM's pin in error on every read, so the current rules allow every node
// and the bridge pin goes first. The bridge is not in effect either, so the
// completion stops there: the anti-affinity rule is left alone, no final pin
// is written, and the delete defers.
func TestDeadNodeDelete_PinNotInEffectStopsBeforeTheAntiAffinity(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.pin("pve-node2,pve-node3", nil)
	f.scene.Rules["bosh-aa-0123456789ab"] = map[string]any{
		"type": "resource-affinity", "affinity": "negative", "resources": "vm:101,vm:102", "comment": deadNodeMarker,
	}
	f.cluster.ruleErrors = map[string]string{"bosh-na-101": "resource in conflicting rules"}
	requireHARetriable(t, f.run(), "bridge pin")
	ev := f.events()
	for _, e := range ev {
		if strings.Contains(e, "bosh-aa-0123456789ab") || strings.HasPrefix(e, "DeleteQemu") {
			t.Fatalf("the anti-affinity rule must stay and nothing may be destroyed; events: %v", ev)
		}
	}
	var nodesCSV string
	f.scene.Do(func(s *handlers.HADeleteScene) { nodesCSV, _ = s.Rules["bosh-na-101"]["nodes"].(string) })
	if nodesCSV != "pve-node2,pve-node3" {
		t.Fatalf("the pin must hold the bridge, not the final pin: %q", nodesCSV)
	}
}

// TestDeadNodeDelete_BridgePinLetsTheReleaseRun is a full anti-affinity group
// with an allowed node outside the reach. VM 101's strict pin names pve-node2
// and pve-node3 but is disabled, and VM 102, the other member of its
// anti-affinity rule, is pinned to pve-node3. A pin of pve-node3 alone for VM
// 101 would leave the two members one node, which the rule writer refuses as
// infeasible, so the completion writes the bridge pin first, which keeps the
// dead node and so two nodes. The release then runs, the rule of two is
// deleted, and the final pin names pve-node3 alone.
func TestDeadNodeDelete_BridgePinLetsTheReleaseRun(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.pin("pve-node2,pve-node3", map[string]any{"disable": 1})
	f.scene.Rules["bosh-na-102"] = map[string]any{"type": "node-affinity", "resources": "vm:102",
		"nodes": "pve-node3", "strict": 1, "comment": deadNodeMarker}
	f.scene.Rules["bosh-aa-0123456789ab"] = map[string]any{
		"type": "resource-affinity", "affinity": "negative", "resources": "vm:101,vm:102", "comment": deadNodeMarker,
	}
	if err := f.run(); err != nil {
		t.Fatalf("the delete must complete: %v", err)
	}
	f.requireCompletedOn("pve-node3")
	requirePinReleasePin(t, f.events())
	if got, _ := f.pinAtMove.Load().(string); got != "pve-node2,pve-node3" && got != "pve-node3" {
		t.Fatalf("the CRM moved the VM under pin %q; want the bridge or the final pin", got)
	}
}

// TestDeadNodeDelete_MovedVMWithAVolumeLeftOnTheDeadNodeDefers runs the
// delete of a VM whose resource carries the dead-node token and whose config,
// now readable on pve-node3, names a volume on node-local storage. That
// volume lives on the dead node, so the delete defers and destroys nothing.
func TestDeadNodeDelete_MovedVMWithAVolumeLeftOnTheDeadNodeDefers(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.storage.entries = append(f.storage.entries,
		map[string]any{"storage": "local-zfs", "type": "zfspool", "content": "images"})
	f.movedTo("pve-node3")
	f.configs["pve-node3"] = map[string]any{
		"scsi0": "ceph:vm-101-disk-0,size=10G", "scsi1": "local-zfs:vm-101-disk-1,size=50G",
	}
	requireHARetriable(t, f.run(), "does not reach every volume", "dead node "+deadNode)
	if at := eventIndex(f.events(), "DeleteQemu"); at != -1 {
		t.Fatalf("nothing may be destroyed; events: %v", f.events())
	}
}

// TestDeadNodeDelete_MovedVMVolumeCheckNeedsTheStorageIndex defers the delete
// of a moved VM whose storage index cannot be read or decoded, because the
// delete cannot then show that its node reaches every volume.
func TestDeadNodeDelete_MovedVMVolumeCheckNeedsTheStorageIndex(t *testing.T) {
	cases := []struct {
		name  string
		setup func(s *deadNodeStorage)
		want  string
	}{
		{"unreadable", func(s *deadNodeStorage) { s.err = errors.New("500 storage read failed (HTTP500)") },
			"read the storage index"},
		{"malformed entry", func(s *deadNodeStorage) {
			s.raw = []json.RawMessage{json.RawMessage(`{"storage": 7}`)}
		}, "decode a storage index entry"},
		{"duplicate entry", func(s *deadNodeStorage) {
			s.entries = append(s.entries, map[string]any{"storage": "ceph", "type": "rbd", "shared": 1})
		}, "names storage ceph twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeadNodeDelete(t)
			f.movedTo("pve-node3")
			tc.setup(f.storage)
			requireHARetriable(t, f.run(), tc.want)
			if at := eventIndex(f.events(), "DeleteQemu"); at != -1 {
				t.Fatalf("nothing may be destroyed; events: %v", f.events())
			}
		})
	}
}

// TestDeadNodeDelete_TokenCheckNeedsTheResource defers the delete of an
// HA-managed VM whose HA resource cannot be read, because the delete cannot
// tell whether its config left a dead node.
func TestDeadNodeDelete_TokenCheckNeedsTheResource(t *testing.T) {
	scene := handlers.NewHADeleteScene()
	scene.Manage(101, "pve-node1", "started", "started")
	d := newLegacyHADelete(t, scene, "pve-node1", dlbEnabledConfig())
	c := d.deps.PVE.(*handlers.HAScenePermsClient).Client.(*mockPVEClient)
	c.clusterSvc = &deadNodeCluster{HASceneCluster: c.clusterSvc.(*handlers.HASceneCluster), failGetResource: true}
	requireHARetriable(t, d.run(), "check whether its config left a dead node")
	if at := eventIndex(scene.EventsSnapshot(), "DeleteQemu"); at != -1 {
		t.Fatalf("nothing may be destroyed; events: %v", scene.EventsSnapshot())
	}
}

// TestDeadNodeDelete_AbsenceProofToleratesOnlyTheDeadSource destroys the VM
// on pve-node1 while pve-node3 is down as well. The absence proof after the
// destroy may rest on the index for the dead node the config left, and for
// no other member, so the call answers retriable until pve-node3 answers.
func TestDeadNodeDelete_AbsenceProofToleratesOnlyTheDeadSource(t *testing.T) {
	f := newDeadNodeDelete(t)
	f.scene.MarkNodeDead("pve-node3")
	requireHARetriable(t, f.run(), "ties it to member pve-node3", "covers only dead node "+deadNode)
	f.requireCompletedOn("pve-node1")
}
