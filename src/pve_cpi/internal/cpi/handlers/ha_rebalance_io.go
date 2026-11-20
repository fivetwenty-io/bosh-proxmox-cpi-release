package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/harebalance"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// rebalanceIO is every read, lock, and write of a rebalance run, so the run's
// order and its failure handling can be tested without PVE.
type rebalanceIO interface {
	now() time.Time
	// sleep pauses for d, and returns ctx's error when ctx ends first.
	sleep(ctx context.Context, d time.Duration) error
	// runLock takes the run lock bosh-lock-ha-rebalance once, with lease ttl,
	// and returns its release. A live holder returns pve.ErrClusterLockHeld.
	runLock(ctx context.Context, ttl time.Duration) (func(), error)
	profile(ctx context.Context) (*ha.Profile, error)
	snapshot(ctx context.Context, profile *ha.Profile) (*rebalanceSnapshot, error)
	activity(ctx context.Context) (rebalanceActivity, error)
	journal(ctx context.Context) rebalanceJournal
	// serviceStates returns the HA manager's raw state of each service.
	serviceStates(ctx context.Context) (map[string]string, error)
	// withRecord runs fn with the VM's journal record held, tried once. A
	// record another call holds returns an error that matches
	// errRebalanceBusy, and fn does not run.
	withRecord(ctx context.Context, vmid int, cfg map[string]any, fn func() error) error
	// withClaim runs fn with the VM's VMID claim held, taken once. A claim
	// another call holds returns an error that matches errRebalanceBusy, and a
	// claim no VM can take returns a *rebalanceHalt.
	withClaim(ctx context.Context, vmid int, fn func(context.Context) error) error
	// reread reads the VM's config on node, its HA resource, and its pin. A
	// VM or resource that is gone reads as nil with no error.
	reread(ctx context.Context, vmid int, node string) (map[string]any, *sdkcluster.GetHaResourcesResponse,
		*ha.Rule, error)
	// readResource reads the VM's HA resource, nil when it has none.
	readResource(ctx context.Context, vmid int) (*sdkcluster.GetHaResourcesResponse, error)
	// mark and unmark are ha.MarkRebalance and ha.UnmarkRebalance.
	mark(ctx context.Context, vmid int, read *sdkcluster.GetHaResourcesResponse, node string) (bool, error)
	unmark(ctx context.Context, vmid int, read *sdkcluster.GetHaResourcesResponse, failback bool) (bool, error)
	// migrate is ha.MoveIntoSet with the set and the targets of one node.
	migrate(ctx context.Context, vmid int, from, to string, before func(context.Context) error) (pve.VMLocation,
		error)
	// writePin is writeReplacedPin with the rules lock tried once.
	writePin(ctx context.Context, profile *ha.Profile, d *ha.VMDecision, vmid int, oldPin, newPin *ha.Rule) (bool,
		error)
	// wantFailback is wantVMFailbackForRebalance.
	wantFailback(ctx context.Context, profile *ha.Profile, vmid int, loc pve.VMLocation, node string) (
		vmFailbackWant, error)
	// locate returns the node the VM is on.
	locate(ctx context.Context, vmid int) (string, error)
	// drainState is ha.StopForDrain when stop is set, with the drained node,
	// and ha.StartAfterDrain otherwise.
	drainState(ctx context.Context, vmid int, read *sdkcluster.GetHaResourcesResponse, stop bool, node string) (
		bool, error)
}

// pveRebalanceIO is rebalanceIO against PVE.
type pveRebalanceIO struct {
	deps   Deps
	opts   RebalanceOptions
	logger *log.Logger
	// holdsRunLock is set once this run holds the run lock, so the activity
	// read does not count it.
	holdsRunLock bool
}

func newPVERebalanceIO(deps Deps, opts RebalanceOptions) *pveRebalanceIO {
	return &pveRebalanceIO{deps: deps, opts: opts, logger: deps.Log(context.Background())}
}

// rebalanceRunLockWait bounds the one try at the run lock.
const rebalanceRunLockWait = 30 * time.Second

func (p *pveRebalanceIO) now() time.Time { return time.Now() }

func (p *pveRebalanceIO) sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (p *pveRebalanceIO) runLock(ctx context.Context, ttl time.Duration) (func(), error) {
	owner := "ha-rebalance/" + p.deps.RequestDirectorUUID
	h, err := pve.AcquireClusterLock(ctx, p.deps.PVE.Pools(), RebalanceRunLockName, owner, ttl,
		rebalanceRunLockWait, pve.WithoutWait())
	if err != nil {
		return nil, err
	}
	p.holdsRunLock = true
	return func() {
		if err := h.Release(context.WithoutCancel(ctx)); err != nil {
			p.logger.Warn(rebalanceOp+": the run lock could not be released, and it expires with its lease",
				log.Err(err))
		}
	}, nil
}

func (p *pveRebalanceIO) profile(ctx context.Context) (*ha.Profile, error) {
	return resolveHAProfile(ctx, p.deps)
}

// rebalanceResourceRow is the part of a /cluster/resources row the run reads.
type rebalanceResourceRow struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	VMID     sdkclient.PVEInt  `json:"vmid"`
	Node     string            `json:"node"`
	Name     string            `json:"name"`
	Status   string            `json:"status"`
	Pool     string            `json:"pool"`
	Tags     string            `json:"tags"`
	Maxmem   sdkclient.PVEInt  `json:"maxmem"`
	Template sdkclient.PVEBool `json:"template"`
}

// listResourceRows reads every row of /cluster/resources.
func (p *pveRebalanceIO) listResourceRows(ctx context.Context) ([]rebalanceResourceRow, error) {
	resp, err := p.deps.PVE.Cluster().ListResources(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list the cluster resources: %w", pve.WrapError(err))
	}
	if resp == nil {
		return nil, errors.New("list the cluster resources: no listing returned")
	}
	rows := make([]rebalanceResourceRow, 0, len(*resp))
	for i, raw := range *resp {
		var row rebalanceResourceRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("cluster resource row %d does not parse: %w", i, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// snapshot reads the cluster in one pass and fails on any read it can't make,
// because a plan over part of the cluster would move VMs on a wrong picture.
func (p *pveRebalanceIO) snapshot(ctx context.Context, profile *ha.Profile) (*rebalanceSnapshot, error) {
	api := p.deps.PVE.Cluster()
	rows, err := p.listResourceRows(ctx)
	if err != nil {
		return nil, err
	}
	haResources, err := api.ListHaResources(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list the HA resources: %w", pve.WrapError(err))
	}
	if haResources == nil {
		return nil, errors.New("list the HA resources: no listing returned")
	}
	managed, err := recoveredServiceIDs(haResources)
	if err != nil {
		return nil, err
	}
	rules, err := readHARuleSet(ctx, api)
	if err != nil {
		return nil, fmt.Errorf("read the HA rules: %w", err)
	}
	states, err := pve.ReadHAServiceStates(ctx, api)
	if err != nil {
		return nil, fmt.Errorf("read the HA manager status: %w", err)
	}
	jobs, err := ha.NewReplicationAPI(p.deps.PVE).ListJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the replication jobs: %w", err)
	}
	snap := &rebalanceSnapshot{rules: rules, states: states, jobs: jobs}
	all := profileNodeNames(profile)
	for i := range rows {
		row := &rows[i]
		switch {
		case row.Type == "node":
			snap.nodes = append(snap.nodes, harebalance.Node{Name: row.Node, TotalMemBytes: row.Maxmem.Int(),
				Online: row.Status == "online"})
			continue
		case row.Type != resourceTypeQemu && row.Type != "lxc", row.Template.Bool():
			continue
		case row.Node == "" || row.VMID.Int() <= 0:
			return nil, fmt.Errorf("cluster resource row %d names no node or no VM ID", i)
		}
		vmid := int(row.VMID.Int())
		guest := harebalance.Guest{VMID: vmid, Node: row.Node, MemBytes: row.Maxmem.Int()}
		sid := headroomResourceRow{Type: row.Type, VMID: row.VMID}.haSID()
		if managed[sid] {
			guest.Admissible = recoveryNodes(rules, sid, all)
			guest.HA = len(guest.Admissible) != 1 || guest.Admissible[0] != row.Node
		}
		snap.guests = append(snap.guests, guest)
		if row.Type != resourceTypeQemu || !p.ownedRow(row) {
			continue
		}
		facts, ok, err := p.vmFacts(ctx, row)
		if err != nil {
			return nil, err
		}
		if ok {
			snap.vms = append(snap.vms, facts)
		}
	}
	slices.SortFunc(snap.nodes, func(a, b harebalance.Node) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(snap.vms, func(a, b rebalanceVMFacts) int { return a.vmid - b.vmid })
	return snap, nil
}

// ownedRow reports whether a guest row may be one of this Director's HA VMs:
// it carries the HA tag and this Director's owner tag, and it is neither
// another Director's, a parker, nor the create-env Director.
func (p *pveRebalanceIO) ownedRow(row *rebalanceResourceRow) bool {
	tags := lowerTags(parseTagsField(row.Tags))
	owner := strings.ToLower(sanitizeTagValue(p.deps.RequestDirectorUUID))
	return contains(tags, haManagedTag) && contains(tagValues(tags, ownerTagPrefix), owner) &&
		!stragglerRowForeign(row.Tags, p.deps.RequestDirectorUUID) &&
		!pve.IsParkerVM(int(row.VMID.Int()), row.Tags, parkerReadConfigFor(p.deps)) &&
		!contains(tags, strings.ToLower(directorTagPrefix+createEnvDirectorName))
}

// vmFacts reads an owned VM's config and HA resource. ok is false for a VM
// that is gone, or whose HA resource belongs to another Director.
func (p *pveRebalanceIO) vmFacts(ctx context.Context, row *rebalanceResourceRow) (rebalanceVMFacts, bool, error) {
	vmid := int(row.VMID.Int())
	res, err := p.readResource(ctx, vmid)
	if err != nil {
		return rebalanceVMFacts{}, false, err
	}
	if res != nil && (res.Comment == nil || haOwnershipOf(*res.Comment, p.deps.RequestDirectorUUID) != haOwnedByUs) {
		return rebalanceVMFacts{}, false, nil
	}
	cfg, err := p.deps.PVE.QEMU().Config(ctx, row.Node, vmid)
	switch {
	case err != nil && pve.IsNotFound(err):
		return rebalanceVMFacts{}, false, nil
	case err != nil:
		return rebalanceVMFacts{}, false, fmt.Errorf("read the config of VM %d on node %s: %w", vmid, row.Node,
			pve.WrapError(err))
	}
	return rebalanceVMFacts{vmid: vmid, name: row.Name, node: row.Node, tags: row.Tags, mem: row.Maxmem.Int(),
		cfg: cfg, res: res}, true, nil
}

// rebalanceTask is the part of a task row the activity gate reads.
type rebalanceTask struct {
	UPID    string `json:"upid"`
	Type    string `json:"type"`
	ID      string `json:"id"`
	User    string `json:"user"`
	TokenID string `json:"tokenid"`
}

// activity reads the live sentinel pools and the running tasks of the CPI's
// token user.
func (p *pveRebalanceIO) activity(ctx context.Context) (rebalanceActivity, error) {
	rows, err := p.listResourceRows(ctx)
	if err != nil {
		return rebalanceActivity{}, err
	}
	var act rebalanceActivity
	now := p.now()
	runPool := pve.ClusterLockPoolName(RebalanceRunLockName)
	var nodes []string
	for i := range rows {
		row := &rows[i]
		if row.Type == "node" && row.Status == "online" {
			nodes = append(nodes, row.Node)
		}
		if row.Type != "pool" {
			continue
		}
		pool := row.Pool
		if pool == "" {
			pool = strings.TrimPrefix(row.ID, "/pool/")
		}
		if !pve.IsClusterLockPool(pool) {
			continue
		}
		comment, found, err := p.deps.PVE.Pools().GetPoolComment(ctx, pool)
		if err != nil {
			return rebalanceActivity{}, fmt.Errorf("read the lock pool %s: %w", pool, err)
		}
		if !found || !liveSentinel(comment, now) {
			continue
		}
		if pool == runPool {
			act.runLock = act.runLock || !p.holdsRunLock
			continue
		}
		act.locks = append(act.locks, pool)
	}
	slices.Sort(act.locks)
	user := rebalanceTaskUser(p.deps)
	if user == "" {
		return act, nil
	}
	slices.Sort(nodes)
	for _, node := range nodes {
		tasks, err := p.nodeTasks(ctx, node, user)
		if err != nil {
			return rebalanceActivity{}, err
		}
		act.tasks = append(act.tasks, tasks...)
	}
	return act, nil
}

// nodeTasks lists the running tasks of user on node.
func (p *pveRebalanceIO) nodeTasks(ctx context.Context, node, user string) ([]string, error) {
	source := "active"
	resp, err := p.deps.PVE.Nodes().ListTasks(ctx, node, &sdknodes.ListTasksParams{Source: &source,
		Userfilter: &user})
	if err != nil {
		return nil, fmt.Errorf("list the running tasks on node %s: %w", node, pve.WrapError(err))
	}
	if resp == nil {
		return nil, nil
	}
	var out []string
	for i, raw := range *resp {
		var t rebalanceTask
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, fmt.Errorf("task row %d on node %s does not parse: %w", i, node, err)
		}
		if t.User != user && (t.TokenID == "" || t.User+"!"+t.TokenID != user) {
			continue
		}
		what := t.Type
		if t.ID != "" {
			what += " " + t.ID
		}
		out = append(out, what+" on "+node)
	}
	return out, nil
}

// rebalanceTaskUser is the user PVE records on the CPI's tasks: the token's
// user@realm!tokenid, or the user@realm of a password login.
func rebalanceTaskUser(deps Deps) string {
	cfg := deps.Config
	switch {
	case cfg == nil:
		return ""
	case cfg.APIToken != "":
		id, _, _ := strings.Cut(cfg.APIToken, "=")
		return id
	case cfg.User == "":
		return ""
	case strings.Contains(cfg.User, "@"):
		return cfg.User
	}
	realm := cfg.Realm
	if realm == "" {
		realm = "pam"
	}
	return cfg.User + "@" + realm
}

// journal reads the allocation journal's records when the configuration
// names one.
func (p *pveRebalanceIO) journal(ctx context.Context) rebalanceJournal {
	cfg := p.deps.Config
	if cfg == nil || cfg.StoragePlacementNamespace == "" && cfg.StorageAllocationJournalDir == "" {
		return rebalanceJournal{}
	}
	j := rebalanceJournal{configured: true}
	nodes, err := clusterNodeNames(ctx, p.deps)
	if err != nil {
		j.err = err
		return j
	}
	journal, err := openStorageAllocationJournal(ctx, p.deps, nodes)
	if err != nil {
		j.err = err
		return j
	}
	records, err := journal.List()
	j.err = errors.Join(err, journal.Close())
	if j.err == nil {
		j.open = openJournalRecords(records)
	}
	return j
}

func (p *pveRebalanceIO) serviceStates(ctx context.Context) (map[string]string, error) {
	return pve.ReadHAServiceStates(ctx, p.deps.PVE.Cluster())
}

func (p *pveRebalanceIO) withRecord(ctx context.Context, vmid int, cfg map[string]any, fn func() error) error {
	ran := false
	pick := func(records []aj.Record) (*aj.Record, error) { return replicaWidenRecordFor(records, vmid, cfg) }
	err := withVMRecordWhere(ctx, p.deps, pick, vmRecordTryOnce, func(*aj.Handle) error {
		ran = true
		return fn()
	})
	switch {
	case err == nil || ran:
		return err
	case errors.Is(err, errVMRecordBusy):
		return fmt.Errorf("%w: another call holds the VM's journal record", errRebalanceBusy)
	case errors.Is(err, errReplicaWidenRecordUnmatched):
		return &rebalanceSkip{reason: err.Error()}
	}
	return err
}

func (p *pveRebalanceIO) withClaim(ctx context.Context, vmid int, fn func(context.Context) error) error {
	// The run takes claims of its own, so a claim set the caller's context
	// carried is dropped.
	ctx = context.WithValue(ctx, vmidClaimSetKey{}, (*vmidClaimSet)(nil))
	ran := false
	err := withVMIDClaim(ctx, p.deps, vmid, "ha-rebalance/"+strconv.Itoa(vmid), claimForRebalance, p.logger,
		func(ctx context.Context) error {
			ran = true
			return fn(ctx)
		})
	switch {
	case err == nil || ran:
		return err
	case isVMIDClaimHeld(err), errors.Is(err, pve.ErrClusterLockNoTimeToWait):
		return fmt.Errorf("%w: another call claims the VM, such as a create, a delete, or a metadata write",
			errRebalanceBusy)
	case errors.Is(err, errVMIDClaimNoPools):
		return &rebalanceHalt{reason: "the PVE client offers no pool service, so no VM can be claimed"}
	case pve.IsClusterLockPermissionRefused(err):
		return &rebalanceHalt{reason: "PVE refused the VMID claim for lack of permission, so no VM can be " +
			"claimed; grant Pool.Allocate and Pool.Audit on /pool so the CPI can create bosh-lock-vm-<vmid>"}
	}
	return err
}

func (p *pveRebalanceIO) reread(ctx context.Context, vmid int, node string) (map[string]any,
	*sdkcluster.GetHaResourcesResponse, *ha.Rule, error,
) {
	cfg, err := p.deps.PVE.QEMU().Config(ctx, node, vmid)
	switch {
	case err != nil && pve.IsNotFound(err):
		cfg = nil
	case err != nil:
		return nil, nil, nil, fmt.Errorf("read the config of VM %d on node %s: %w", vmid, node, pve.WrapError(err))
	}
	res, err := p.readResource(ctx, vmid)
	if err != nil {
		return nil, nil, nil, err
	}
	rules, err := readHARuleSet(ctx, p.deps.PVE.Cluster())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read the HA rules: %w", err)
	}
	return cfg, res, rules.Find(naRuleNameFor(vmid)), nil
}

func (p *pveRebalanceIO) readResource(ctx context.Context, vmid int) (*sdkcluster.GetHaResourcesResponse, error) {
	sid := haResourceSid(vmid)
	res, err := p.deps.PVE.Cluster().GetHaResources(ctx, sid)
	switch {
	case err != nil && (isHaNotFound(err) || pve.IsNotFound(err)):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read the HA resource %s: %w", sid, pve.WrapError(err))
	case res == nil:
		return nil, fmt.Errorf("the read of the HA resource %s returned nothing", sid)
	}
	return res, nil
}

func (p *pveRebalanceIO) mark(ctx context.Context, vmid int, read *sdkcluster.GetHaResourcesResponse,
	node string,
) (bool, error) {
	return ha.MarkRebalance(ctx, p.deps.PVE.Cluster(), haResourceSid(vmid), read, node)
}

func (p *pveRebalanceIO) unmark(ctx context.Context, vmid int, read *sdkcluster.GetHaResourcesResponse,
	failback bool,
) (bool, error) {
	return ha.UnmarkRebalance(ctx, p.deps.PVE.Cluster(), haResourceSid(vmid), read, failback, time.Now())
}

func (p *pveRebalanceIO) migrate(ctx context.Context, vmid int, from, to string,
	before func(context.Context) error,
) (pve.VMLocation, error) {
	deps := ha.NarrowDepsFor(p.deps.PVE, p.logger, haMotionWait(p.deps))
	return ha.MoveIntoSet(ctx, deps, ha.MoveInput{VMID: vmid, Node: from, Set: []string{to}, Targets: []string{to},
		Budget: p.opts.SettleTimeout, BeforeMigrate: before})
}

func (p *pveRebalanceIO) writePin(ctx context.Context, profile *ha.Profile, d *ha.VMDecision, vmid int,
	oldPin, newPin *ha.Rule,
) (bool, error) {
	return writeReplacedPin(withHARulesLockTryOnce(ctx), p.deps, profile, d, vmid, oldPin, newPin, p.logger)
}

func (p *pveRebalanceIO) wantFailback(ctx context.Context, profile *ha.Profile, vmid int, loc pve.VMLocation,
	node string,
) (vmFailbackWant, error) {
	return wantVMFailbackForRebalance(ctx, p.deps, p.deps.PVE.Cluster(), profile, vmid, loc, node)
}

func (p *pveRebalanceIO) drainState(ctx context.Context, vmid int, read *sdkcluster.GetHaResourcesResponse,
	stop bool, node string,
) (bool, error) {
	if stop {
		return ha.StopForDrain(ctx, p.deps.PVE.Cluster(), haResourceSid(vmid), read, node)
	}
	return ha.StartAfterDrain(ctx, p.deps.PVE.Cluster(), haResourceSid(vmid), read)
}

func (p *pveRebalanceIO) locate(ctx context.Context, vmid int) (string, error) {
	node, _, found, err := pve.FindVMViaCluster(ctx, p.deps.PVE, vmid)
	switch {
	case err != nil:
		return "", fmt.Errorf("find VM %d: %w", vmid, err)
	case !found:
		return "", fmt.Errorf("VM %d is not in the cluster", vmid)
	}
	return node, nil
}
