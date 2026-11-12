package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
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
	// EmptyResources answers GET /cluster/ha/resources with no rows, the list
	// the SDK returns for a 200 that carries no data, whatever Requested holds.
	EmptyResources bool
	// EmptyCurrent answers GET /cluster/ha/status/current with no rows, the
	// list the SDK returns for a 200 that carries no data.
	EmptyCurrent bool
	// Services are the CRM's service_status entries by SID.
	Services map[string]HASceneService
	// Requested are the HA resources by SID and their requested state.
	Requested map[string]string
	// Comments are the HA resources' comments by SID; a resource without one
	// has none.
	Comments map[string]string
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

	// Configs are the VM configs by VMID. A VM with a config here has its
	// config read, its config writes, and its index tags served from it, so a
	// test can follow the tags and the description an order writes.
	Configs map[int]map[string]any
	// RefuseRemoval refuses an HA resource removal while a rule names the
	// resource, one answer PVE may give.
	RefuseRemoval bool
	// PurgeKeepsRules leaves the rules that name a removed resource as they
	// were, and DisableOrphanRules disables each of them, as PVE may for a
	// rule that names a missing resource. With neither, a removal prunes the
	// resource from every rule and deletes a rule it leaves empty.
	PurgeKeepsRules    bool
	DisableOrphanRules bool
	// Tick advances Now by this many seconds on every manager status read,
	// so a CRM round newer than a request can be seen. Zero leaves the clock.
	Tick int64
	// uidSerial numbers the uid of a service the scene creates.
	uidSerial int

	// MoveTasks are the qmmove tasks each node's active task list holds, and
	// MoveTasksErr fails every listing when set.
	MoveTasks    map[string][]pve.ActiveTask
	MoveTasksErr error
	// lockPools backs the client's pool service, whose create refuses a pool
	// that exists, so the VMID locks the scene's handlers take exclude each
	// other. Its calls are not events.
	lockPools *exclusiveLockPools

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
		Comments:     map[string]string{},
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
// A service in recovery keeps its state and its node, as PVE's CRM does: it
// records the request and acts on it once it has recovered the service.
func (s *HADeleteScene) applyStop(vmid int) {
	if !s.StopApplies {
		return
	}
	sid := haResourceSid(vmid)
	if svc, ok := s.Services[sid]; ok && svc.State == "recovery" {
		if _, ok := s.Requested[sid]; ok {
			s.Requested[sid] = "stopped"
		}
		return
	}
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
		if _, held := s.Services[sid]; !held {
			// The CRM takes up a resource that leaves ignored on the node the
			// VM runs on, under a uid of its own.
			s.uidSerial++
			s.Services[sid] = HASceneService{State: "started", Node: s.IndexNode[vmid],
				UID: fmt.Sprintf("uid-%d-%d", vmid, s.uidSerial)}
		}
	}
	if svc, ok := s.Services[sid]; ok {
		svc.State = "started"
		s.Services[sid] = svc
	}
	s.Power[vmid] = "running"
}

// rulesNaming returns the names of the rules whose resources name sid.
// Called locked.
func (s *HADeleteScene) rulesNaming(sid string) []string {
	var out []string
	for name, row := range s.Rules {
		res, _ := row["resources"].(string)
		for _, r := range strings.Split(res, ",") {
			if strings.TrimSpace(r) == sid {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// pruneRules applies what a removal of sid does to the rules that name it.
// Called locked.
func (s *HADeleteScene) pruneRules(sid string) {
	for _, name := range s.rulesNaming(sid) {
		row := s.Rules[name]
		switch {
		case s.DisableOrphanRules:
			row["disable"] = 1
		case s.PurgeKeepsRules:
		default:
			res, _ := row["resources"].(string)
			var kept []string
			for _, r := range strings.Split(res, ",") {
				if r = strings.TrimSpace(r); r != "" && r != sid {
					kept = append(kept, r)
				}
			}
			if len(kept) == 0 {
				delete(s.Rules, name)
				continue
			}
			row["resources"] = strings.Join(kept, ",")
		}
	}
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
	s.Now += s.Tick
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

// currentRows answers GET /cluster/ha/status/current the way PVE builds it on
// the entry node: the quorum row, the master row, whose state is idle when its
// timestamp is more than 30 seconds old and no resource is configured, and a
// service row for every service and resource. The lrm rows are left out,
// because NodeHealth reads them only when manager status is empty, and a scene
// with no CRM answers with no rows at all.
func (s *HADeleteScene) currentRows() *cluster.ListHaStatusCurrentResponse {
	if s.NoCRM || s.EmptyCurrent {
		return AbsenceNoHAStatus()
	}
	row := func(v map[string]any) json.RawMessage {
		raw, _ := json.Marshal(v)
		return raw
	}
	state := "active"
	if s.CRMAge > 30 || s.CRMAge < -2 {
		state = "old timestamp - dead?"
		if len(s.Requested) == 0 {
			state = "idle"
		}
	}
	master := s.Members[0]
	rows := cluster.ListHaStatusCurrentResponse{
		row(map[string]any{"id": "quorum", "type": "quorum", "node": s.Members[0], "status": "OK", "quorate": 1}),
		row(map[string]any{
			"id": "master", "type": "master", "node": master, "timestamp": s.Now - s.CRMAge,
			"status": master + " (" + state + ", Mon Aug 24 16:37:54 2026)",
		}),
	}
	sids := map[string]struct{}{}
	for sid := range s.Services {
		sids[sid] = struct{}{}
	}
	for sid := range s.Requested {
		sids[sid] = struct{}{}
	}
	for sid := range sids {
		rows = append(rows, row(map[string]any{"id": "service:" + sid, "type": "service", "sid": sid}))
	}
	return &rows
}

func (s *HADeleteScene) resourceRows() *cluster.ListHaResourcesResponse {
	rows := make(cluster.ListHaResourcesResponse, 0, len(s.Requested))
	if s.EmptyResources {
		return &rows
	}
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
		if cfg, ok := s.Configs[vmid]; ok {
			delete(row, "tags")
			if tags, _ := cfg["tags"].(string); tags != "" {
				row["tags"] = tags
			}
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
	c.Scene.mu.Lock()
	defer c.Scene.mu.Unlock()
	return c.Scene.currentRows(), nil
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
	resp := &cluster.GetHaResourcesResponse{Sid: sid, State: &state}
	if comment, ok := c.Scene.Comments[sid]; ok {
		resp.Comment = &comment
	}
	return resp, nil
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
	if params != nil && params.Comment != nil {
		if c.Scene.Comments == nil {
			c.Scene.Comments = map[string]string{}
		}
		c.Scene.Comments[sid] = *params.Comment
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
	if c.Scene.RefuseRemoval && len(c.Scene.rulesNaming(sid)) > 0 {
		return &sdkerrors.APIError{HTTPCode: 500, Message: "resource '" + sid + "' is used in a rule"}
	}
	delete(c.Scene.Requested, sid)
	delete(c.Scene.Services, sid)
	delete(c.Scene.Comments, sid)
	c.Scene.pruneRules(sid)
	return nil
}

// CreateHaResources registers a resource in the state it names. An ignored
// resource is not one the CRM holds, so it gets no service entry until it is
// asked for started.
func (c *HASceneCluster) CreateHaResources(_ context.Context, params *cluster.CreateHaResourcesParams) error {
	c.Scene.mu.Lock()
	defer c.Scene.mu.Unlock()
	state := "started"
	if params.State != nil {
		state = *params.State
	}
	c.Scene.record("CreateHaResources:" + params.Sid + "=" + state)
	if _, ok := c.Scene.Requested[params.Sid]; ok {
		return errors.New("resource ID '" + params.Sid + "' already defined (HTTP500)")
	}
	c.Scene.Requested[params.Sid] = state
	if params.Comment != nil {
		c.Scene.Comments[params.Sid] = *params.Comment
	}
	vmid, _ := strconv.Atoi(strings.TrimPrefix(params.Sid, "vm:"))
	c.Scene.IndexHAState[vmid] = state
	if state == "started" {
		c.Scene.applyStart(vmid)
	}
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
	if params.Nodes != nil {
		row["nodes"] = *params.Nodes
	}
	if params.Strict != nil {
		row["strict"] = *params.Strict
	}
	if params.Disable != nil {
		row["disable"] = *params.Disable
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

// ListQemu lists the VMs the scene places on node, with the tags their index
// rows carry.
func (n *HASceneNodes) ListQemu(_ context.Context, node string, _ *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	n.Scene.mu.Lock()
	defer n.Scene.mu.Unlock()
	resp := nodes.ListQemuResponse{}
	for _, raw := range *n.Scene.indexRows() {
		var row map[string]any
		if json.Unmarshal(raw, &row) != nil || row["node"] != node {
			continue
		}
		delete(row, "node")
		out, _ := json.Marshal(row)
		resp = append(resp, out)
	}
	return &resp, nil
}

// UpdateQemuConfig applies the tags and the description of a write to a VM
// whose config the scene holds, and delegates every other write.
func (n *HASceneNodes) UpdateQemuConfig(
	ctx context.Context, node, vmid string, params *nodes.UpdateQemuConfigParams,
) error {
	id, _ := strconv.Atoi(vmid)
	n.Scene.mu.Lock()
	cfg, held := n.Scene.Configs[id]
	if !held {
		n.Scene.mu.Unlock()
		return n.Service.UpdateQemuConfig(ctx, node, vmid, params)
	}
	defer n.Scene.mu.Unlock()
	n.Scene.record("UpdateQemuConfig:" + vmid)
	if params.Digest != nil {
		if cur, _ := cfg["digest"].(string); cur != *params.Digest {
			return errors.New("detected modified configuration - file changed by other user? Try again. (HTTP500)")
		}
	}
	if params.Delete != nil {
		for _, field := range strings.Split(*params.Delete, ",") {
			delete(cfg, strings.TrimSpace(field))
		}
	}
	if params.Tags != nil {
		cfg["tags"] = *params.Tags
	}
	if params.Description != nil {
		cfg["description"] = *params.Description
	}
	n.Scene.uidSerial++
	cfg["digest"] = fmt.Sprintf("digest-%d", n.Scene.uidSerial)
	return nil
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

// Config serves a VM config the scene holds, a copy so a caller's edit never
// writes through, and delegates any other read.
func (q *HASceneQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	q.Scene.mu.Lock()
	cfg, held := q.Scene.Configs[vmid]
	if !held {
		q.Scene.mu.Unlock()
		return q.Service.Config(ctx, node, vmid)
	}
	defer q.Scene.mu.Unlock()
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out, nil
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

// Pools serves the scene's lock pools (see HADeleteScene.lockPoolService).
func (c *haSceneClient) Pools() pve.PoolService { return c.scene.lockPoolService() }

// ActiveMoveTasks serves the scene's MoveTasks for node.
func (c *haSceneClient) ActiveMoveTasks(_ context.Context, node string) ([]pve.ActiveTask, error) {
	return c.scene.activeMoveTasks(node)
}

// activeMoveTasks returns the scene's MoveTasks for node, or MoveTasksErr.
func (s *HADeleteScene) activeMoveTasks(node string) ([]pve.ActiveTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.MoveTasksErr != nil {
		return nil, s.MoveTasksErr
	}
	return slices.Clone(s.MoveTasks[node]), nil
}

// lockPoolService returns the pool service the scene's VMID locks live in, made on
// first use.
func (s *HADeleteScene) lockPoolService() *exclusiveLockPools {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockPools == nil {
		s.lockPools = newExclusiveLockPools()
	}
	return s.lockPools
}

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

// ActiveMoveTasks serves the scene's MoveTasks for node.
func (c *HAScenePermsClient) ActiveMoveTasks(_ context.Context, node string) ([]pve.ActiveTask, error) {
	return c.Scene.activeMoveTasks(node)
}
