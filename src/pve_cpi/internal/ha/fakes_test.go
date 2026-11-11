package ha_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
)

// now is the entry node's clock in every test, far from the Director's own.
const now int64 = 1_000_000_000

// wnode describes one node of the fake cluster.
type wnode struct {
	name    string
	online  bool
	votes   *int   // nil leaves quorum_votes out of the cluster config
	version string // pve-manager version, empty means 9.2.4
	cpu     string // CPU model, empty means "Xeon A"
	flags   string // CPU flags, empty leaves the flags out
	noCPU   bool   // the status carries no cpuinfo at all

	versionErr error
	statusErr  error
}

func votes(n int) *int { return &n }

// world is the whole fake cluster. The zero value of every error and flag
// means a healthy answer.
type world struct {
	nodes []wnode

	noClusterRow bool // standalone shape
	notQuorate   bool
	entry        string // defaults to the first node
	clusterName  string // the cluster row's name, "lab" when empty

	managerBody json.RawMessage // overrides the default manager status
	managerErr  error
	crmStamp    int64 // defaults to now
	master      string

	configNodesBody json.RawMessage
	configNodesErr  error
	qdeviceBody     json.RawMessage // nil means an empty object, no QDevice
	qdeviceErr      error
	rulesErr        error
	optionsBody     json.RawMessage
	optionsErr      error
	replicationErr  error
	replication     []json.RawMessage
	noResources     bool // the cluster has no HA resources, so the CRM is idle
	resourcesErr    error
	storageBody     []json.RawMessage
	storageErr      error

	perms    map[string]map[string]bool // path -> privileges; nil means Sys.Console on / and VM.Replicate on /vms
	permsErr error
	noPerms  bool

	cluster *fakeCluster
	nodesAP *fakeNodes
	storage *fakeStorage
	permAP  *fakePerms
}

func (w *world) defaults() {
	for i := range w.nodes {
		if w.nodes[i].votes == nil {
			w.nodes[i].votes = votes(1)
		}
	}
}

// three returns a healthy three-node cluster with one vote each.
func three() *world {
	return &world{nodes: []wnode{
		{name: "pve1", online: true}, {name: "pve2", online: true}, {name: "pve3", online: true},
	}}
}

// nNodes returns a healthy cluster of n nodes with one vote each.
func nNodes(n int) *world {
	w := &world{}
	for i := 1; i <= n; i++ {
		w.nodes = append(w.nodes, wnode{name: fmt.Sprintf("pve%d", i), online: true})
	}
	return w
}

func (w *world) deps(cfg *config.CPIConfig) ha.Deps {
	w.defaults()
	w.cluster = &fakeCluster{w: w}
	w.nodesAP = &fakeNodes{w: w}
	w.storage = &fakeStorage{w: w}
	d := ha.Deps{Config: cfg, Cluster: w.cluster, Nodes: w.nodesAP, Storage: w.storage}
	if !w.noPerms {
		w.permAP = &fakePerms{w: w}
		d.Permissions = w.permAP
	}
	return d
}

func cfgMode(mode string) *config.CPIConfig {
	c := config.HAChoice(mode)
	return &config.CPIConfig{HA: &config.HAConfig{Mode: &c}}
}

func cfgModeTopology(mode, topology string) *config.CPIConfig {
	cfg := cfgMode(mode)
	t := config.HAChoice(topology)
	cfg.HA.StorageTopology = &t
	return cfg
}

type fakeCluster struct {
	w     *world
	calls map[string]int
}

func (c *fakeCluster) hit(name string) {
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[name]++
}

func (c *fakeCluster) total() int {
	n := 0
	for _, v := range c.calls {
		n += v
	}
	return n
}

func (c *fakeCluster) ListStatus(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
	c.hit("status")
	w := c.w
	entry := w.entry
	if entry == "" && len(w.nodes) > 0 {
		entry = w.nodes[0].name
	}
	var rows []json.RawMessage
	if !w.noClusterRow {
		q := 1
		if w.notQuorate {
			q = 0
		}
		name := w.clusterName
		if name == "" {
			name = "lab"
		}
		rows = append(
			rows,
			mustJSON(map[string]any{"type": "cluster", "name": name, "nodes": len(w.nodes), "quorate": q}),
		)
	}
	for _, n := range w.nodes {
		online := 0
		if n.online {
			online = 1
		}
		row := map[string]any{"type": "node", "name": n.name, "online": online, "id": "node/" + n.name}
		if n.name == entry {
			row["local"] = 1
		}
		rows = append(rows, mustJSON(row))
	}
	resp := sdkcluster.ListStatusResponse(rows)
	return &resp, nil
}

func (c *fakeCluster) ListHaStatusManagerStatus(_ context.Context) (*sdkcluster.ListHaStatusManagerStatusResponse, error) {
	c.hit("manager")
	w := c.w
	if w.managerErr != nil {
		return nil, w.managerErr
	}
	if w.managerBody != nil {
		body := w.managerBody
		return &body, nil
	}
	stamp := w.crmStamp
	if stamp == 0 {
		stamp = now
	}
	master := w.master
	if master == "" && len(w.nodes) > 0 {
		master = w.nodes[0].name
	}
	nodeStatus := map[string]string{}
	lrm := map[string]any{}
	for _, n := range w.nodes {
		if n.online {
			nodeStatus[n.name] = "online"
			lrm[n.name] = map[string]any{"state": "wait_for_agent_lock", "mode": "active", "timestamp": now}
		} else {
			nodeStatus[n.name] = "unknown"
		}
	}
	body := mustJSON(map[string]any{
		"manager_status": map[string]any{"master_node": master, "node_status": nodeStatus, "timestamp": stamp},
		"lrm_status":     lrm,
		"quorum":         map[string]any{"node": master, "quorate": 1},
	})
	return &body, nil
}

func (c *fakeCluster) ListHaStatusCurrent(_ context.Context) (*sdkcluster.ListHaStatusCurrentResponse, error) {
	c.hit("current")
	resp := sdkcluster.ListHaStatusCurrentResponse{}
	return &resp, nil
}

func (c *fakeCluster) ListConfigNodes(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	c.hit("confignodes")
	w := c.w
	if w.configNodesErr != nil {
		return nil, w.configNodesErr
	}
	if w.configNodesBody != nil {
		var rows sdkcluster.ListConfigNodesResponse
		if err := json.Unmarshal(w.configNodesBody, &rows); err != nil {
			return nil, err
		}
		return &rows, nil
	}
	var rows sdkcluster.ListConfigNodesResponse
	for i, n := range w.nodes {
		row := map[string]any{"node": n.name, "nodeid": i + 1, "ring0_addr": "10.0.0." + fmt.Sprint(i+1)}
		if n.votes != nil {
			row["quorum_votes"] = fmt.Sprint(*n.votes) // PVE sends this as a string on some releases
		}
		rows = append(rows, mustJSON(row))
	}
	return &rows, nil
}

func (c *fakeCluster) ListConfigQdevice(_ context.Context) (*sdkcluster.ListConfigQdeviceResponse, error) {
	c.hit("qdevice")
	w := c.w
	if w.qdeviceErr != nil {
		return nil, w.qdeviceErr
	}
	body := w.qdeviceBody
	if body == nil {
		body = json.RawMessage(`{}`)
	}
	return &body, nil
}

func (c *fakeCluster) ListHaResources(_ context.Context, _ *sdkcluster.ListHaResourcesParams) (*sdkcluster.ListHaResourcesResponse, error) {
	c.hit("resources")
	w := c.w
	if w.resourcesErr != nil {
		return nil, w.resourcesErr
	}
	resp := sdkcluster.ListHaResourcesResponse{}
	if !w.noResources {
		resp = append(resp, mustJSON(map[string]any{"sid": "vm:100", "state": "started"}))
	}
	return &resp, nil
}

func (c *fakeCluster) ListHaRules(_ context.Context, _ *sdkcluster.ListHaRulesParams) (*sdkcluster.ListHaRulesResponse, error) {
	c.hit("rules")
	if c.w.rulesErr != nil {
		return nil, c.w.rulesErr
	}
	resp := sdkcluster.ListHaRulesResponse{}
	return &resp, nil
}

func (c *fakeCluster) ListOptions(_ context.Context) (*sdkcluster.ListOptionsResponse, error) {
	c.hit("options")
	w := c.w
	if w.optionsErr != nil {
		return nil, w.optionsErr
	}
	resp := &sdkcluster.ListOptionsResponse{}
	if w.optionsBody != nil {
		resp.Crs = w.optionsBody
	}
	return resp, nil
}

func (c *fakeCluster) ListReplication(_ context.Context) (*sdkcluster.ListReplicationResponse, error) {
	c.hit("replication")
	w := c.w
	if w.replicationErr != nil {
		return nil, w.replicationErr
	}
	resp := sdkcluster.ListReplicationResponse(w.replication)
	return &resp, nil
}

type fakeNodes struct {
	w     *world
	calls map[string]int
}

func (n *fakeNodes) hit(name string) {
	if n.calls == nil {
		n.calls = map[string]int{}
	}
	n.calls[name]++
}

func (n *fakeNodes) find(name string) (wnode, bool) {
	for _, x := range n.w.nodes {
		if x.name == name {
			return x, true
		}
	}
	return wnode{}, false
}

func (n *fakeNodes) ListTime(_ context.Context, _ string) (*sdknodes.ListTimeResponse, error) {
	n.hit("time")
	var resp sdknodes.ListTimeResponse
	raw := mustJSON(map[string]any{"time": now, "localtime": now, "timezone": "UTC"})
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (n *fakeNodes) ListVersion(_ context.Context, node string) (*sdknodes.ListVersionResponse, error) {
	n.hit("version:" + node)
	x, ok := n.find(node)
	if !ok {
		return nil, errors.New("no such node " + node)
	}
	if x.versionErr != nil {
		return nil, x.versionErr
	}
	v := x.version
	if v == "" {
		v = "9.2.4"
	}
	return &sdknodes.ListVersionResponse{Version: v, Release: "9", Repoid: "abc"}, nil
}

func (n *fakeNodes) ListStatus(_ context.Context, node string) (*sdknodes.ListStatusResponse, error) {
	n.hit("status:" + node)
	x, ok := n.find(node)
	if !ok {
		return nil, errors.New("no such node " + node)
	}
	if x.statusErr != nil {
		return nil, x.statusErr
	}
	resp := &sdknodes.ListStatusResponse{}
	if x.noCPU {
		return resp, nil
	}
	model := x.cpu
	if model == "" {
		model = "Xeon A"
	}
	info := map[string]any{"model": model, "cpus": 8, "sockets": 1}
	if x.flags != "" {
		info["flags"] = x.flags
	}
	resp.Cpuinfo = mustJSON(info)
	return resp, nil
}

type fakeStorage struct {
	w     *world
	calls int
}

func (s *fakeStorage) ListStorage(_ context.Context, _ *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
	s.calls++
	if s.w.storageErr != nil {
		return nil, s.w.storageErr
	}
	resp := sdkclusterstorage.ListStorageResponse(s.w.storageBody)
	return &resp, nil
}

type fakePerms struct {
	w     *world
	paths []string
}

func (p *fakePerms) EffectivePermissions(_ context.Context, path string) (map[string]bool, error) {
	p.paths = append(p.paths, path)
	if p.w.permsErr != nil {
		return nil, p.w.permsErr
	}
	if p.w.perms != nil {
		return p.w.perms[path], nil
	}
	switch path {
	case "/":
		return map[string]bool{"Sys.Console": true, "Sys.Audit": true}, nil
	case "/vms":
		return map[string]bool{"VM.Replicate": true}, nil
	}
	return map[string]bool{}, nil
}

func storageRow(name, typ, content string, shared bool, nodes string) json.RawMessage {
	row := map[string]any{"storage": name, "type": typ}
	if content != "" {
		row["content"] = content
	}
	if shared {
		row["shared"] = 1
	}
	if nodes != "" {
		row["nodes"] = nodes
	}
	return mustJSON(row)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// sdkClusterStatusResponse aliases the SDK type for the wrappers in the tests.
type sdkClusterStatusResponse = sdkcluster.ListStatusResponse

// rowsCluster adds extra rows to the cluster status.
type rowsCluster struct {
	*fakeCluster
	extra []json.RawMessage
}

func (r *rowsCluster) ListStatus(ctx context.Context) (*sdkcluster.ListStatusResponse, error) {
	resp, err := r.fakeCluster.ListStatus(ctx)
	if err != nil {
		return nil, err
	}
	out := append(sdkcluster.ListStatusResponse{}, *resp...)
	out = append(out, r.extra...)
	return &out, nil
}
