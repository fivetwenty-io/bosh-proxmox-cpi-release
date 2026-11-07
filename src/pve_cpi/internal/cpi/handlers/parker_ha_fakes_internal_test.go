package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// phResource is one HA resource as the parker HA fakes hold it.
type phResource struct {
	state   string
	comment string
}

// phCluster is the HA side of the parker HA fakes: the node-affinity rule
// store of naClusterStub, HA resources with their requested state and
// comment, and an ordered log of every HA write.
type phCluster struct {
	*naClusterStub
	mu        sync.Mutex
	log       *stagingLog
	res       map[string]*phResource
	getErr    error
	rulesErr  error
	updateErr error
	// mangle, when set, rewrites every rule the CPI creates or updates
	// right after it is stored, the way an out-of-band edit lands between
	// each write and its verification.
	mangle   func(p *cluster.CreateHaRulesParams)
	getCalls int
}

func newPHCluster(members ...string) *phCluster {
	stub := newNAStub()
	stub.members = members
	return &phCluster{naClusterStub: stub, log: &stagingLog{}, res: map[string]*phResource{}}
}

func (c *phCluster) GetHaResources(_ context.Context, sid string) (*cluster.GetHaResourcesResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.getCalls++
	if c.getErr != nil {
		return nil, c.getErr
	}
	r, ok := c.res[sid]
	if !ok {
		return nil, fmt.Errorf("no such resource '%s'", sid)
	}
	state, comment := r.state, r.comment
	return &cluster.GetHaResourcesResponse{Sid: sid, State: &state, Comment: &comment}, nil
}

func (c *phCluster) CreateHaResources(_ context.Context, p *cluster.CreateHaResourcesParams) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.res[p.Sid]; ok {
		return fmt.Errorf("resource ID '%s' already defined", p.Sid)
	}
	r := &phResource{state: *p.State}
	if p.Comment != nil {
		r.comment = *p.Comment
	}
	c.res[p.Sid] = r
	c.log.add("stage:" + r.state)
	return nil
}

func (c *phCluster) UpdateHaResources(_ context.Context, sid string, p *cluster.UpdateHaResourcesParams) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.updateErr != nil {
		return c.updateErr
	}
	r, ok := c.res[sid]
	if !ok {
		return fmt.Errorf("no such resource '%s'", sid)
	}
	if p.Comment != nil {
		r.comment = *p.Comment
	}
	if p.State != nil {
		r.state = *p.State
		c.log.add("request:" + r.state)
	}
	return nil
}

func (c *phCluster) DeleteHaResources(_ context.Context, sid string, _ *cluster.DeleteHaResourcesParams) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.res, sid)
	c.log.add("unstage")
	return nil
}

func (c *phCluster) ListHaRules(
	ctx context.Context, p *cluster.ListHaRulesParams,
) (*cluster.ListHaRulesResponse, error) {
	if c.rulesErr != nil {
		return nil, c.rulesErr
	}
	return c.naClusterStub.ListHaRules(ctx, p)
}

func (c *phCluster) CreateHaRules(ctx context.Context, p *cluster.CreateHaRulesParams) error {
	c.log.add("pin:" + p.Rule)
	if err := c.naClusterStub.CreateHaRules(ctx, p); err != nil {
		return err
	}
	c.mangleStored(p.Rule)
	return nil
}

// mangleStored applies mangle to the stored rule.
func (c *phCluster) mangleStored(rule string) {
	if c.mangle == nil {
		return
	}
	stored := c.rules[rule]
	c.mangle(&stored)
	c.rules[rule] = stored
}

func (c *phCluster) UpdateHaRules(ctx context.Context, rule string, p *cluster.UpdateHaRulesParams) error {
	c.log.add("repin:" + rule)
	if err := c.naClusterStub.UpdateHaRules(ctx, rule, p); err != nil {
		return err
	}
	c.mangleStored(rule)
	return nil
}

func (c *phCluster) DeleteHaRules(ctx context.Context, rule string) error {
	c.log.add("unpin:" + rule)
	return c.naClusterStub.DeleteHaRules(ctx, rule)
}

// setPin stores a node-affinity rule naming parker 90000, as an earlier
// registration wrote it.
func (c *phCluster) setPin(strict bool, nodes, comment string) {
	const vmid = 90000
	rule := naRuleNameFor(vmid)
	c.rules[rule] = cluster.CreateHaRulesParams{
		Rule: rule, Type: naRuleType, Resources: haResourceSid(vmid), Nodes: &nodes, Strict: &strict,
		Comment: &comment,
	}
	c.digest++
}

// phClient is a single-node PVE for the parker HA tests: the guests of node
// phNode with their configs and power states, protection writes recorded in
// order, and the HA side in cluster.
type phClient struct {
	pve.Client
	mu         sync.Mutex
	configs    map[int]map[string]any
	snapshots  map[int][]string
	status     map[int]string
	configErr  map[int]error
	protection []string
	cluster    *phCluster
	listCalls  int
}

const phNode = "pve01"

func newPHClient(configs map[int]map[string]any, hac *phCluster) *phClient {
	return &phClient{configs: configs, cluster: hac, snapshots: map[int][]string{}, status: map[int]string{},
		configErr: map[int]error{}}
}

func (c *phClient) QEMU() qemu.Service         { return &phQEMU{c: c} }
func (c *phClient) Nodes() sdknodes.Service    { return &phNodes{c: c} }
func (c *phClient) Cluster() cluster.Service   { return c.cluster }
func (c *phClient) Pools() pve.PoolService     { return nil }
func (c *phClient) protectionWrites() []string { c.mu.Lock(); defer c.mu.Unlock(); return c.protection }

type phQEMU struct {
	qemu.Service
	c *phClient
}

func (q *phQEMU) Config(_ context.Context, _ string, vmid int) (map[string]any, error) {
	q.c.mu.Lock()
	defer q.c.mu.Unlock()
	if err := q.c.configErr[vmid]; err != nil {
		return nil, err
	}
	cfg, ok := q.c.configs[vmid]
	if !ok {
		return nil, errors.New("Configuration file 'nodes/" + phNode + "/qemu-server/" + strconv.Itoa(vmid) +
			".conf' does not exist")
	}
	out := map[string]any{"digest": "phdigest"}
	for k, v := range cfg {
		out[k] = v
	}
	return out, nil
}

func (q *phQEMU) ListSnapshots(_ context.Context, _ string, vmid int) ([]map[string]any, error) {
	q.c.mu.Lock()
	defer q.c.mu.Unlock()
	out := make([]map[string]any, 0, 1+len(q.c.snapshots[vmid]))
	out = append(out, map[string]any{"name": "current"})
	for _, name := range q.c.snapshots[vmid] {
		out = append(out, map[string]any{"name": name})
	}
	return out, nil
}

func (q *phQEMU) Status(_ context.Context, _ string, vmid int) (map[string]any, error) {
	q.c.mu.Lock()
	defer q.c.mu.Unlock()
	if _, ok := q.c.configs[vmid]; !ok {
		return nil, errors.New("Configuration file 'nodes/" + phNode + "/qemu-server/" + strconv.Itoa(vmid) +
			".conf' does not exist")
	}
	if s := q.c.status[vmid]; s != "" {
		return map[string]any{"status": s}, nil
	}
	return map[string]any{"status": vmPowerStateStopped}, nil
}

type phNodes struct {
	sdknodes.Service
	c *phClient
}

func (n *phNodes) ListQemu(
	_ context.Context, _ string, _ *sdknodes.ListQemuParams,
) (*sdknodes.ListQemuResponse, error) {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	n.c.listCalls++
	out := sdknodes.ListQemuResponse{}
	for vmid, cfg := range n.c.configs {
		row := map[string]any{"vmid": vmid}
		if tags, ok := cfg["tags"].(string); ok {
			row["tags"] = tags
		}
		if name, ok := cfg["name"].(string); ok {
			row["name"] = name
		}
		raw, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return &out, nil
}

func (n *phNodes) ListQemuPending(ctx context.Context, node, vmid string) (*sdknodes.ListQemuPendingResponse, error) {
	return PendingFromConfigRead(ctx, n.c.QEMU().Config, node, vmid)
}

func (n *phNodes) UpdateQemuConfig(_ context.Context, _ string, vmid string, p *sdknodes.UpdateQemuConfigParams) error {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	if p != nil && p.Protection != nil {
		n.c.protection = append(n.c.protection, vmid+":"+strconv.FormatBool(*p.Protection))
	}
	return nil
}

// phParkerCfg is the parker band of the parker HA tests.
func phParkerCfg() pve.ParkerConfig {
	return pve.ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999}
}

// phParker returns a parker config holding the given volumes on scsi0 on.
func phParker(volumes ...string) map[string]any {
	cfg := map[string]any{"tags": "bosh-cpi;bosh-parker", "protection": 1}
	for i, v := range volumes {
		cfg["scsi"+strconv.Itoa(i)] = v
	}
	return cfg
}

// phProfile is a healthy full-HA profile over pve01 to pve03. Storage nfs
// reaches every node, ab reaches pve01 and pve02, ac reaches pve01 and
// pve03, and local-lvm is node-local.
func phProfile(mode string) *ha.Profile {
	p := &ha.Profile{
		Mode: mode, Effective: ha.EffectiveFull, MinVersion: "9.2.4", HAVersionOK: true, DLBSupported: true,
		RulesSupported: true, SysConsole: true, VMReplicate: true,
		Storages: []pve.StorageInfo{
			{Name: "nfs", Type: pve.StorageTypeNFS, Content: "images"},
			{Name: "ab", Type: pve.StorageTypeNFS, Content: "images", Nodes: []string{"pve01", "pve02"}},
			{Name: "ac", Type: pve.StorageTypeNFS, Content: "images", Nodes: []string{"pve01", "pve03"}},
			{Name: "local-lvm", Type: pve.StorageTypeLVMThin, Content: "images"},
		},
	}
	for _, n := range []string{"pve01", "pve02", "pve03"} {
		p.Nodes = append(p.Nodes, ha.NodeFacts{Name: n, Online: true, Version: "9.2.4"})
	}
	return p
}

// phRig is one parker HA pass against the fakes.
type phRig struct {
	client  *phClient
	cluster *phCluster
	profile *ha.Profile
	deps    Deps
}

func newPHRig(mode string, configs map[int]map[string]any) *phRig {
	hac := newPHCluster("pve01", "pve02", "pve03")
	client := newPHClient(configs, hac)
	profile := phProfile(mode)
	cfg := icMinConfig()
	cfg.HA = &config.HAConfig{Mode: haChoiceOf(mode)}
	return &phRig{
		client: client, cluster: hac, profile: profile,
		deps: Deps{
			Config: cfg, RequestDirectorUUID: aaDirector, PVE: client, Logger: log.NewNopLogger(),
			HAProfile: func(context.Context) (*ha.Profile, error) { return profile, nil },
		},
	}
}

func (r *phRig) pass() *parkerHA {
	return &parkerHA{
		deps: r.deps, logger: log.NewNopLogger(), node: phNode, cfg: phParkerCfg(), profile: r.profile,
		owner: aaDirector, managed: r.deps.Config.HAModeValue() == config.HAModeManaged,
	}
}

// ourMarker is the ownership marker this Director's HA writes carry.
func ourMarker() string { return haIdentity{Owner: aaDirector}.marker() }
