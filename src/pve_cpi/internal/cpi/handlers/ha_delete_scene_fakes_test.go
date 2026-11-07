package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// HASceneService is one service_status entry of the CRM view.
type HASceneService struct {
	State, Node, UID string
}

// HADeleteScene models the PVE HA view a delete reads. The cluster is entered
// through its first member, which is quorate, and the CRM view is fresh on the
// entry node's clock unless a test ages it. Every read and write takes the
// mutex, so a scene can back concurrent handlers.
//
// PVE redirects a stop of an HA-managed VM to the CRM, so the scene models
// both the HA resource write and the direct stop as a request that the CRM
// carries out at once: the resource requests stopped, the service is stopped,
// and the VM is powered off.
type HADeleteScene struct {
	mu sync.Mutex

	// Now is the entry node's clock in Unix seconds.
	Now int64
	// Members are the cluster members; the first is the entry node.
	Members []string
	// Offline members are reported online:0 by /cluster/status.
	Offline map[string]bool
	// NodeState is the CRM's node_status; a missing member is online.
	NodeState map[string]string
	// LRMAge is the age in seconds of each member's LRM timestamp; a missing
	// member's is two seconds old.
	LRMAge map[string]int64
	// CRMAge is the age in seconds of the CRM timestamp.
	CRMAge int64
	// NoCRM answers manager status with an empty body, the shape of a
	// cluster that runs no HA.
	NoCRM bool
	// NoLocal leaves the local flag off every /cluster/status row, the shape
	// of a cluster status that does not mark the node that answered.
	NoLocal bool
	// Services are the CRM's service_status entries by SID.
	Services map[string]HASceneService
	// Requested are the HA resources by SID and their requested state.
	Requested map[string]string
	// Power is each VM's power state on its node; a missing VM is running.
	Power map[int]string
	// IndexNode and IndexHAState are the /cluster/resources row of each VM.
	// A VM without an index node is absent from the index.
	IndexNode    map[int]string
	IndexHAState map[int]string
	// IndexLock is the lock each VM's index row reports, and IndexTags the
	// tags every row carries. Both are empty by default.
	IndexLock map[int]string
	IndexTags string
	// Rules are the HA rules by name, each a /cluster/ha/rules row.
	Rules map[string]map[string]any
	// Perms is the token's privileges on /. Nil makes the read fail.
	Perms map[string]bool
	// StopApplies, when false, leaves the CRM's view unchanged by a stop
	// request, so a test can model a stop that never lands.
	StopApplies bool
	// OnRead runs under the lock at the start of every manager status read,
	// so a test can move the CRM between reads.
	OnRead func(s *HADeleteScene, reads int)
	// OnIndexRead runs under the lock at the start of every cluster index
	// read, which is what the settle wait polls, so a test can end the
	// motion while the wait runs.
	OnIndexRead func(s *HADeleteScene, reads int)

	// Events records every mutation in order.
	Events []string
	// ManagerReads counts the manager status reads.
	ManagerReads int
	// IndexReads counts the cluster index reads.
	IndexReads int
}

// NewHADeleteScene returns a three-member scene entered through pve-node1 in
// which nothing is HA-managed yet.
func NewHADeleteScene() *HADeleteScene {
	return &HADeleteScene{
		Now:          time.Now().Unix(),
		Members:      []string{"pve-node1", "pve-node2", "pve-node3"},
		Offline:      map[string]bool{},
		NodeState:    map[string]string{},
		LRMAge:       map[string]int64{},
		CRMAge:       5,
		Services:     map[string]HASceneService{},
		Requested:    map[string]string{},
		Power:        map[int]string{},
		IndexNode:    map[int]string{},
		IndexHAState: map[int]string{},
		IndexLock:    map[int]string{},
		Rules:        map[string]map[string]any{},
		Perms:        map[string]bool{"Sys.Console": true, "VM.PowerMgmt": true, "VM.Allocate": true},
		StopApplies:  true,
	}
}

// Manage registers vmid as an HA resource requesting requested, held by the
// CRM in raw on node, and places it on node in the cluster index.
func (s *HADeleteScene) Manage(vmid int, node, requested, raw string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sid := haResourceSid(vmid)
	if requested != "" {
		s.Requested[sid] = requested
	}
	if raw != "" {
		s.Services[sid] = HASceneService{State: raw, Node: node, UID: "uid-" + strconv.Itoa(vmid)}
	}
	s.IndexNode[vmid] = node
	// The index's hastate is the CRM's state when it holds one, as PVE
	// reports it, and the configured request otherwise.
	s.IndexHAState[vmid] = requested
	if raw != "" {
		s.IndexHAState[vmid] = raw
	}
	if raw == "stopped" || raw == "error" {
		s.Power[vmid] = "stopped"
	}
}

// MarkNodeDead reports node offline in cluster status, unknown to the CRM,
// and with an LRM timestamp four hours old, the shape of a node that died.
func (s *HADeleteScene) MarkNodeDead(node string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Offline[node] = true
	s.NodeState[node] = "unknown"
	s.LRMAge[node] = 4 * 3600
}

// Place puts vmid on node in the cluster index without any HA registration.
func (s *HADeleteScene) Place(vmid int, node string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.IndexNode[vmid] = node
}

// Do runs f under the scene's lock.
func (s *HADeleteScene) Do(f func(s *HADeleteScene)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// EventsSnapshot returns a copy of the recorded mutations.
func (s *HADeleteScene) EventsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.Events...)
}

func (s *HADeleteScene) record(event string) { s.Events = append(s.Events, event) }

// applyStop is the CRM carrying out a stop request for vmid. Called locked.
func (s *HADeleteScene) applyStop(vmid int) {
	if !s.StopApplies {
		return
	}
	sid := haResourceSid(vmid)
	if _, ok := s.Requested[sid]; ok {
		s.Requested[sid] = "stopped"
		s.IndexHAState[vmid] = "stopped"
	}
	if svc, ok := s.Services[sid]; ok {
		svc.State = "stopped"
		s.Services[sid] = svc
	}
	s.Power[vmid] = "stopped"
}

// applyStart is the CRM carrying out a start request for vmid. Called locked.
func (s *HADeleteScene) applyStart(vmid int) {
	if !s.StopApplies {
		return
	}
	sid := haResourceSid(vmid)
	if _, ok := s.Requested[sid]; ok {
		s.Requested[sid] = "started"
		s.IndexHAState[vmid] = "started"
	}
	if svc, ok := s.Services[sid]; ok {
		svc.State = "started"
		s.Services[sid] = svc
	}
	s.Power[vmid] = "running"
}

func (s *HADeleteScene) statusRows() *cluster.ListStatusResponse {
	rows := make(cluster.ListStatusResponse, 0, 1+len(s.Members))
	rows = append(rows, json.RawMessage(
		fmt.Sprintf(`{"type":"cluster","name":"scene","quorate":1,"nodes":%d}`, len(s.Members))))
	for i, m := range s.Members {
		online := 1
		if s.Offline[m] {
			online = 0
		}
		row := map[string]any{"type": "node", "name": m, "online": online, "id": "node/" + m}
		if i == 0 && !s.NoLocal {
			row["local"] = 1
		}
		raw, _ := json.Marshal(row)
		rows = append(rows, raw)
	}
	return &rows
}

func (s *HADeleteScene) managerBody() *cluster.ListHaStatusManagerStatusResponse {
	s.ManagerReads++
	if s.OnRead != nil {
		s.OnRead(s, s.ManagerReads)
	}
	if s.NoCRM {
		body := cluster.ListHaStatusManagerStatusResponse(`{}`)
		return &body
	}
	nodeStatus := map[string]string{}
	lrm := map[string]any{}
	for _, m := range s.Members {
		state := s.NodeState[m]
		if state == "" {
			state = "online"
		}
		nodeStatus[m] = state
		age, ok := s.LRMAge[m]
		if !ok {
			age = 2
		}
		lrm[m] = map[string]any{"mode": "active", "state": "active", "timestamp": s.Now - age}
	}
	services := map[string]any{}
	for sid, svc := range s.Services {
		entry := map[string]any{"state": svc.State, "node": svc.Node}
		if svc.UID != "" {
			entry["uid"] = svc.UID
		}
		services[sid] = entry
	}
	raw, _ := json.Marshal(map[string]any{
		"manager_status": map[string]any{
			"master_node": s.Members[0], "node_status": nodeStatus, "service_status": services,
			"timestamp": s.Now - s.CRMAge,
		},
		"lrm_status": lrm,
		"quorum":     map[string]any{"node": s.Members[0], "quorate": "1"},
	})
	body := cluster.ListHaStatusManagerStatusResponse(raw)
	return &body
}

func (s *HADeleteScene) resourceRows() *cluster.ListHaResourcesResponse {
	rows := make(cluster.ListHaResourcesResponse, 0, len(s.Requested))
	for sid, state := range s.Requested {
		raw, _ := json.Marshal(map[string]any{"sid": sid, "type": "vm", "state": state})
		rows = append(rows, raw)
	}
	return &rows
}

func (s *HADeleteScene) indexRows() *cluster.ListResourcesResponse {
	rows := make(cluster.ListResourcesResponse, 0, len(s.IndexNode))
	for vmid, node := range s.IndexNode {
		row := map[string]any{"vmid": vmid, "node": node, "type": "qemu", "status": s.powerOf(vmid)}
		if hastate := s.IndexHAState[vmid]; hastate != "" {
			row["hastate"] = hastate
		}
		if lock := s.IndexLock[vmid]; lock != "" {
			row["lock"] = lock
		}
		if s.IndexTags != "" {
			row["tags"] = s.IndexTags
		}
		raw, _ := json.Marshal(row)
		rows = append(rows, raw)
	}
	return &rows
}

func (s *HADeleteScene) powerOf(vmid int) string {
	if p := s.Power[vmid]; p != "" {
		return p
	}
	return "running"
}

// HASceneCluster serves the scene's HA and status reads and delegates every
// other call to the embedded service.
type HASceneCluster struct {
	cluster.Service
	Scene *HADeleteScene
	// UseIndex serves /cluster/resources from the scene; otherwise the
	// embedded service answers it.
	UseIndex bool
}

func (c *HASceneCluster) ListStatus(context.Context) (*cluster.ListStatusResponse, error) {
	c.Scene.mu.Lock()
	defer c.Scene.mu.Unlock()
	return c.Scene.statusRows(), nil
}

// ListConfigNodes names the scene's members when no embedded service answers
// the member list, so an absence proof that runs after a destroy has members
// to probe.
func (c *HASceneCluster) ListConfigNodes(ctx context.Context) (*cluster.ListConfigNodesResponse, error) {
	if c.Service != nil {
		return c.Service.ListConfigNodes(ctx)
	}
	c.Scene.mu.Lock()
	defer c.Scene.mu.Unlock()
	return AbsenceConfigNodes(c.Scene.Members...), nil
}

func (c *HASceneCluster) ListHaStatusManagerStatus(context.Context) (*cluster.ListHaStatusManagerStatusResponse, error) {
	c.Scene.mu.Lock()
	defer c.Scene.mu.Unlock()
	return c.Scene.managerBody(), nil
}

func (c *HASceneCluster) ListHaStatusCurrent(context.Context) (*cluster.ListHaStatusCurrentResponse, error) {
	return AbsenceNoHAStatus(), nil
}

func (c *HASceneCluster) ListHaResources(
	context.Context, *cluster.ListHaResourcesParams,
) (*cluster.ListHaResourcesResponse, error) {
	c.Scene.mu.Lock()
	defer c.Scene.mu.Unlock()
	return c.Scene.resourceRows(), nil
}

func (c *HASceneCluster) GetHaResources(_ context.Context, sid string) (*cluster.GetHaResourcesResponse, error) {
	c.Scene.mu.Lock()
	defer c.Scene.mu.Unlock()
	state, ok := c.Scene.Requested[sid]
	if !ok {
		return nil, errors.New("no such resource '" + sid + "' (HTTP500)")
	}
	return &cluster.GetHaResourcesResponse{Sid: sid, State: &state}, nil
}

func (c *HASceneCluster) UpdateHaResources(_ context.Context, sid string, params *cluster.UpdateHaResourcesParams) error {
	c.Scene.mu.Lock()
	defer c.Scene.mu.Unlock()
	state := ""
	if params != nil && params.State != nil {
		state = *params.State
	}
	c.Scene.record("UpdateHaResources:" + sid + "=" + state)
	if _, ok := c.Scene.Requested[sid]; !ok {
		return errors.New("no such resource '" + sid + "' (HTTP500)")
	}
	vmid, _ := strconv.Atoi(strings.TrimPrefix(sid, "vm:"))
	switch state {
	case "stopped":
		c.Scene.applyStop(vmid)
	case "disabled":
		// The CRM stops a disabled service, which clears an error, and
		// keeps it stopped, as PVE does.
		if !c.Scene.StopApplies {
			c.Scene.Requested[sid] = "disabled"
			break
		}
		c.Scene.applyStop(vmid)
		c.Scene.Requested[sid] = "disabled"
	case "started":
		c.Scene.applyStart(vmid)
	default:
		c.Scene.Requested[sid] = state
	}
	return nil
}

func (c *HASceneCluster) DeleteHaResources(_ context.Context, sid string, _ *cluster.DeleteHaResourcesParams) error {
	c.Scene.mu.Lock()
	defer c.Scene.mu.Unlock()
	c.Scene.record("DeleteHaResources:" + sid)
	if _, ok := c.Scene.Requested[sid]; !ok {
		return errors.New("no such resource '" + sid + "' (HTTP500)")
	}
	delete(c.Scene.Requested, sid)
	delete(c.Scene.Services, sid)
	return nil
}

func (c *HASceneCluster) ListHaRules(context.Context, *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	c.Scene.mu.Lock()
	defer c.Scene.mu.Unlock()
	rows := make(cluster.ListHaRulesResponse, 0, len(c.Scene.Rules))
	for name, fields := range c.Scene.Rules {
		row := map[string]any{"rule": name}
		for k, v := range fields {
			row[k] = v
		}
		raw, _ := json.Marshal(row)
		rows = append(rows, raw)
	}
	return &rows, nil
}

func (c *HASceneCluster) DeleteHaRules(_ context.Context, rule string) error {
	c.Scene.mu.Lock()
	defer c.Scene.mu.Unlock()
	c.Scene.record("DeleteHaRules:" + rule)
	if _, ok := c.Scene.Rules[rule]; !ok {
		return errors.New("no such rule '" + rule + "' (HTTP500)")
	}
	delete(c.Scene.Rules, rule)
	return nil
}

func (c *HASceneCluster) CreateHaRules(_ context.Context, params *cluster.CreateHaRulesParams) error {
	c.Scene.mu.Lock()
	defer c.Scene.mu.Unlock()
	c.Scene.record("CreateHaRules:" + params.Rule + "=" + params.Resources)
	row := map[string]any{"type": params.Type, "resources": params.Resources}
	if params.Affinity != nil {
		row["affinity"] = *params.Affinity
	}
	if params.Comment != nil {
		row["comment"] = *params.Comment
	}
	c.Scene.Rules[params.Rule] = row
	return nil
}

// UpdateHaRules records the write and applies the fields it sets to an
// existing rule, as PVE's rule update does; delete names fields to clear.
func (c *HASceneCluster) UpdateHaRules(_ context.Context, rule string, params *cluster.UpdateHaRulesParams) error {
	c.Scene.mu.Lock()
	defer c.Scene.mu.Unlock()
	resources := ""
	if params != nil && params.Resources != nil {
		resources = *params.Resources
	}
	c.Scene.record("UpdateHaRules:" + rule + "=" + resources)
	row, ok := c.Scene.Rules[rule]
	if !ok {
		return errors.New("no such rule '" + rule + "' (HTTP500)")
	}
	if params == nil {
		return nil
	}
	if params.Delete != nil {
		for _, field := range strings.Split(*params.Delete, ",") {
			delete(row, strings.TrimSpace(field))
		}
	}
	set := map[string]*string{
		"resources": params.Resources, "affinity": params.Affinity, "comment": params.Comment, "nodes": params.Nodes,
	}
	for field, value := range set {
		if value != nil {
			row[field] = *value
		}
	}
	if params.Disable != nil {
		row["disable"] = *params.Disable
	}
	if params.Strict != nil {
		row["strict"] = *params.Strict
	}
	return nil
}

func (c *HASceneCluster) ListResources(
	ctx context.Context, params *cluster.ListResourcesParams,
) (*cluster.ListResourcesResponse, error) {
	if !c.UseIndex && c.Service != nil {
		return c.Service.ListResources(ctx, params)
	}
	c.Scene.mu.Lock()
	defer c.Scene.mu.Unlock()
	c.Scene.IndexReads++
	if c.Scene.OnIndexRead != nil {
		c.Scene.OnIndexRead(c.Scene, c.Scene.IndexReads)
	}
	return c.Scene.indexRows(), nil
}

// HAFreshManagerStatus is a manager status body that entry served at now: a
// fresh CRM view with every member online and a fresh LRM, holding services.
func HAFreshManagerStatus(
	now int64, entry string, members []string, services map[string]HASceneService,
) *cluster.ListHaStatusManagerStatusResponse {
	scene := &HADeleteScene{Now: now, Members: append([]string{entry}, members...), CRMAge: 5, Services: services}
	return scene.managerBody()
}

// HAClockNodes serves a node clock of the current time and delegates every
// other call, for fixtures whose nodes service has no clock of its own.
type HAClockNodes struct{ nodes.Service }

func (HAClockNodes) ListTime(context.Context, string) (*nodes.ListTimeResponse, error) {
	now := time.Now().Unix()
	var resp nodes.ListTimeResponse
	raw, _ := json.Marshal(map[string]any{"time": now, "localtime": now, "timezone": "UTC"})
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// HASceneNodes serves the entry node's clock and delegates every other call.
type HASceneNodes struct {
	nodes.Service
	Scene *HADeleteScene
}

func (n *HASceneNodes) ListTime(context.Context, string) (*nodes.ListTimeResponse, error) {
	n.Scene.mu.Lock()
	now := n.Scene.Now
	n.Scene.mu.Unlock()
	var resp nodes.ListTimeResponse
	raw, _ := json.Marshal(map[string]any{"time": now, "localtime": now, "timezone": "UTC"})
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteQemu delegates the destroy and, when it purges, removes the VM's HA
// resource, as PVE's destroy with purge does.
func (n *HASceneNodes) DeleteQemu(
	ctx context.Context, node, vmid string, params *nodes.DeleteQemuParams,
) (*nodes.DeleteQemuResponse, error) {
	resp, err := n.Service.DeleteQemu(ctx, node, vmid, params)
	if err == nil && params != nil && params.Purge != nil && *params.Purge {
		n.Scene.mu.Lock()
		delete(n.Scene.Requested, "vm:"+vmid)
		delete(n.Scene.Services, "vm:"+vmid)
		n.Scene.mu.Unlock()
	}
	return resp, err
}

// HASceneQEMU serves the VM's power state and the direct stop, and delegates
// every other call.
type HASceneQEMU struct {
	qemu.Service
	Scene *HADeleteScene
}

func (q *HASceneQEMU) Status(_ context.Context, _ string, vmid int) (map[string]any, error) {
	q.Scene.mu.Lock()
	defer q.Scene.mu.Unlock()
	return map[string]any{"status": q.Scene.powerOf(vmid)}, nil
}

func (q *HASceneQEMU) Stop(_ context.Context, node string, vmid int) (string, error) {
	q.Scene.mu.Lock()
	defer q.Scene.mu.Unlock()
	q.Scene.record(fmt.Sprintf("Stop:%s/%d", node, vmid))
	q.Scene.applyStop(vmid)
	return fmt.Sprintf("UPID:%s:stop-%d", node, vmid), nil
}

func (q *HASceneQEMU) Start(_ context.Context, node string, vmid int) (string, error) {
	q.Scene.mu.Lock()
	defer q.Scene.mu.Unlock()
	q.Scene.record(fmt.Sprintf("Start:%s/%d", node, vmid))
	q.Scene.applyStart(vmid)
	return fmt.Sprintf("UPID:%s:start-%d", node, vmid), nil
}

// haSceneTasks answers every task wait with success.
type haSceneTasks struct{ tasks.Service }

func (haSceneTasks) Wait(_ context.Context, _, upid string, _ *tasks.WaitOptions) (*tasks.Status, error) {
	return &tasks.Status{Status: "stopped", ExitStatus: "OK", UpID: upid}, nil
}

// haSceneClient is a PVE client made only of the scene. Any call the scene
// does not model panics on the nil embedded services, so a test notices.
type haSceneClient struct {
	pve.Client
	scene *HADeleteScene
}

func newHASceneClient(scene *HADeleteScene) *haSceneClient { return &haSceneClient{scene: scene} }

func (c *haSceneClient) Cluster() cluster.Service {
	return &HASceneCluster{Scene: c.scene, UseIndex: true}
}
func (c *haSceneClient) Nodes() nodes.Service { return &HASceneNodes{Scene: c.scene} }
func (c *haSceneClient) QEMU() qemu.Service   { return &HASceneQEMU{Scene: c.scene} }
func (c *haSceneClient) Tasks() tasks.Service { return haSceneTasks{} }

func (c *haSceneClient) EffectivePermissions(context.Context, string) (map[string]bool, error) {
	return c.scene.perms()
}

// perms is the token's privileges on /, or the refusal PVE answers when Perms
// is nil.
func (s *HADeleteScene) perms() (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Perms == nil {
		return nil, errors.New("permission denied - (HTTP403)")
	}
	out := make(map[string]bool, len(s.Perms))
	for k, v := range s.Perms {
		out[k] = v
	}
	return out, nil
}

// HAScenePermsClient serves the token's privileges from Scene and every other
// call from the wrapped client, for handler tests whose client has no
// permission reader of its own.
type HAScenePermsClient struct {
	pve.Client
	Scene *HADeleteScene
}

func (c *HAScenePermsClient) EffectivePermissions(context.Context, string) (map[string]bool, error) {
	return c.Scene.perms()
}
