package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
)

// replicaWidenOp prefixes the logs of the replica widening sweep.
const replicaWidenOp = "replica widen sweep"

// The replica widening sweep's bounds. A call that took the HA rules lock
// pays for at most this much of it, because the sweep is housekeeping that
// rides on another request.
const (
	// replicaWidenSweepMaxWidened caps the pins one sweep widens.
	replicaWidenSweepMaxWidened = 3
	// replicaWidenSweepMaxAttempts caps the VMs one sweep claims.
	replicaWidenSweepMaxAttempts = 6
	// replicaWidenSweepMaxStatusReads caps the job status reads one sweep
	// makes to decide which VMs are worth a claim.
	replicaWidenSweepMaxStatusReads = 12
	// replicaWidenSweepBudget bounds one sweep, and so does half of what
	// remains of the request's deadline.
	replicaWidenSweepBudget = 60 * time.Second
	// replicaWidenRecordWait bounds the wait for a VM's journal record. A
	// record held longer belongs to a call that is changing the VM, and the
	// VM waits for a later pass.
	replicaWidenRecordWait = 5 * time.Second
)

// ReplicaWidenOutcome is what a widening pass did for one VM.
type ReplicaWidenOutcome string

const (
	// ReplicaWidened is a pin the pass widened to the partner.
	ReplicaWidened ReplicaWidenOutcome = "widened"
	// ReplicaWouldWiden is a VM a dry run found ready for the check that
	// widens it, which runs only with the claim.
	ReplicaWouldWiden ReplicaWidenOutcome = "would_widen"
	// ReplicaWidenSkipped is a VM whose pin keeps the source alone for now.
	ReplicaWidenSkipped ReplicaWidenOutcome = "skipped"
	// ReplicaWidenFailed is a VM whose check failed on a read or a write.
	ReplicaWidenFailed ReplicaWidenOutcome = "failed"
)

// ReplicaWidenResult is one VM a widening pass considered.
type ReplicaWidenResult struct {
	VMID    int                 `json:"vmid"`
	Node    string              `json:"node"`
	Partner string              `json:"partner,omitempty"`
	Outcome ReplicaWidenOutcome `json:"outcome"`
	Reason  string              `json:"reason"`
}

// ReplicaWidenReport is what a widening pass did. Results list every
// replicate VM of this Director whose pin keeps its source alone, in VMID
// order. Deferred counts the VMs the caps or the time budget left for a later
// pass, and Halted says why the pass stopped early, when it did.
type ReplicaWidenReport struct {
	Results  []ReplicaWidenResult `json:"results"`
	Deferred int                  `json:"deferred"`
	Halted   string               `json:"halted,omitempty"`
}

// Count returns how many results have outcome o.
func (r ReplicaWidenReport) Count(o ReplicaWidenOutcome) int {
	n := 0
	for i := range r.Results {
		if r.Results[i].Outcome == o {
			n++
		}
	}
	return n
}

// ReplicaWidenOptions bound a widening pass. A zero cap means no cap.
type ReplicaWidenOptions struct {
	// MaxWidened caps the pins the pass widens.
	MaxWidened int
	// MaxAttempts caps the VMs the pass claims.
	MaxAttempts int
	// MaxStatusReads caps the job status reads that decide which VMs are
	// worth a claim.
	MaxStatusReads int
	// DryRun makes the pass read and report and take no claim, so it writes
	// nothing.
	DryRun bool
}

// WidenAwaitingReplicaPins widens the pin of every replicate VM of this
// Director whose pin still names its source alone and whose replica a
// verified sync now covers. It is the pass the calls that take the HA rules
// lock run after their work, and the one pve-cid ha --widen-replicas runs on
// demand. deps.RequestDirectorUUID names the Director. The check of each VM is
// the one a reboot_vm or set_vm_metadata call makes (see replicaWiden), run
// under the VM's VMID claim and its journal record, with the VM's HA state,
// config, and job read again under the rules lock right before the write. A
// VM whose state changed, or that another call holds, is skipped with the
// reason, and a later pass checks it again. The error is for a pass that
// could not start: no configuration, mode off, no director UUID, no client,
// a listing that failed, or an HA profile that does not let the CPI write.
func WidenAwaitingReplicaPins(ctx context.Context, deps Deps, opts ReplicaWidenOptions) (ReplicaWidenReport, error) {
	return newReplicaWidenPass(deps, opts).run(ctx)
}

// replicaWidenPass is one widening pass. deps.PVE is the client no managed
// allocation guard wraps.
type replicaWidenPass struct {
	deps   Deps
	logger *log.Logger
	opts   ReplicaWidenOptions
	owner  string
	api    ha.ReplicationAPI
	now    func() time.Time
	// record runs fn with the VM's journal record held. nil reads the
	// journal (see withVMRecordWhere); tests hand a record in here.
	record func(ctx context.Context, vmid int, cfg map[string]any, fn func(*aj.Handle) error) error
	// shuffle orders the candidates before the caps apply, so a VM that
	// stays unverified cannot take the same slot on every pass. nil
	// shuffles at random.
	shuffle func([]replicaWidenCandidate)
}

// replicaWidenCandidate is a VM the pass found worth a claim.
type replicaWidenCandidate struct {
	vmid   int
	node   string
	target string
}

func newReplicaWidenPass(deps Deps, opts ReplicaWidenOptions) *replicaWidenPass {
	return &replicaWidenPass{deps: deps, logger: deps.Log(context.Background()), opts: opts,
		owner: strings.TrimSpace(deps.RequestDirectorUUID), now: time.Now}
}

// replicaWidenRow is the part of a /cluster/resources row the pass reads.
type replicaWidenRow struct {
	Type     string            `json:"type"`
	VMID     sdkclient.PVEInt  `json:"vmid"`
	Node     string            `json:"node"`
	Tags     string            `json:"tags"`
	Template sdkclient.PVEBool `json:"template"`
}

func (p *replicaWidenPass) run(ctx context.Context) (ReplicaWidenReport, error) {
	var report ReplicaWidenReport
	switch {
	case p.deps.Config == nil:
		return report, errors.New("replica widen: no CPI configuration")
	case p.deps.Config.HAModeValue() == config.HAModeOff:
		return report, errors.New("replica widen: pve.ha.mode is off, so the CPI keeps no pins")
	case p.owner == "":
		return report, errors.New("replica widen: no director UUID names the Director whose VMs to widen")
	}
	client := unguardedPVE(p.deps.PVE)
	if client == nil || client.Cluster() == nil || client.QEMU() == nil {
		return report, errors.New("replica widen: no PVE client")
	}
	p.deps = withRequestHAProfile(p.deps)
	p.deps.PVE = client
	if p.deps.Replication != nil {
		p.api = p.deps.Replication
	} else {
		p.api = ha.NewReplicationAPI(client)
	}

	rows, err := p.listRows(ctx)
	if err != nil || len(rows) == 0 {
		return report, err
	}
	set, err := readHARuleSet(ctx, client.Cluster())
	if err != nil {
		return report, fmt.Errorf("replica widen: read the HA rules: %w", err)
	}
	now := p.now()
	type pinned struct {
		row       replicaWidenRow
		changedAt time.Time
		stamped   bool
	}
	var awaiting []pinned
	for _, row := range rows {
		vmid := int(row.VMID.Int())
		pin := set.Find(naRuleNameFor(vmid))
		if pin == nil || haOwnershipOf(pin.Comment, p.owner) != haOwnedByUs {
			// A pin another Director owns, or none, is not this pass's to
			// widen, and the doctor's other findings speak for a missing one.
			continue
		}
		source, partner, ok := ha.ReplicaPinShape(pin)
		if !ok || partner != "" {
			continue
		}
		skip := func(reason string) {
			report.Results = append(report.Results, ReplicaWidenResult{VMID: vmid, Node: row.Node,
				Outcome: ReplicaWidenSkipped, Reason: reason})
		}
		if source != row.Node {
			skip("the VM runs on node " + row.Node + ", away from its pinned node " + source)
			continue
		}
		if partner, pending := ha.ReplicaRebuild(pin.Comment); pending {
			skip("the replication job toward node " + partner + " waits to be set up again, which the next " +
				"reboot_vm or set_vm_metadata call does")
			continue
		}
		changedAt, stamped := ha.ReplicaChanged(pin.Comment)
		if stamped && !now.After(ha.ReplicaSyncEarliest(changedAt)) {
			skip("the VM's volumes changed at " + changedAt.UTC().Format(time.RFC3339) + ", less than five " +
				"minutes ago, so no sync can count yet")
			continue
		}
		awaiting = append(awaiting, pinned{row: row, changedAt: changedAt, stamped: stamped})
	}
	if len(awaiting) == 0 {
		sortReplicaWidenResults(report.Results)
		return report, nil
	}

	profile, err := resolveHAProfile(ctx, p.deps)
	if err != nil {
		return report, fmt.Errorf("replica widen: resolve the HA profile: %w", err)
	}
	if !profile.WritesAllowed() {
		return report, errors.New("replica widen: the HA profile does not let the CPI write HA state")
	}
	jobs, err := p.api.ListJobs(ctx)
	if err != nil {
		return report, fmt.Errorf("replica widen: list the replication jobs: %w", err)
	}
	states, err := pve.ReadHAServiceStates(ctx, client.Cluster())
	if err != nil {
		return report, fmt.Errorf("replica widen: read the HA manager status: %w", err)
	}

	var candidates []replicaWidenCandidate
	statusReads := 0
	for _, a := range awaiting {
		vmid, node := int(a.row.VMID.Int()), a.row.Node
		skip := func(reason string) {
			report.Results = append(report.Results, ReplicaWidenResult{VMID: vmid, Node: node,
				Outcome: ReplicaWidenSkipped, Reason: reason})
		}
		if why := replicaWidenServiceHeld(states[haResourceSid(vmid)]); why != "" {
			skip(why)
			continue
		}
		job, _, err := guestReplicaJobs(jobs, vmid, p.owner)
		switch {
		case err != nil:
			skip(err.Error())
			continue
		case job == nil:
			skip("the VM has no replication job of this Director")
			continue
		case job.Disabled || job.RemoveJob != "":
			skip("the VM's replication job " + job.ID + " is disabled or marked for removal")
			continue
		}
		target := replicaOtherNode(*job, node)
		if target == "" {
			skip("the VM's replication job " + job.ID + " names no node other than " + node)
			continue
		}
		// A pin that records no change time gets one from the check, which
		// needs no sync, so only a recorded time is worth a status read. The
		// status read tests the pin's time alone, and the check adds the
		// journal's, which is never earlier, so a VM this read holds back the
		// check would hold back too.
		if a.stamped {
			if p.opts.MaxStatusReads > 0 && statusReads >= p.opts.MaxStatusReads {
				report.Deferred++
				continue
			}
			statusReads++
			st, err := p.api.Status(ctx, node, job.ID)
			if err != nil {
				report.Results = append(report.Results, ReplicaWidenResult{VMID: vmid, Node: node, Partner: target,
					Outcome: ReplicaWidenFailed, Reason: "read the status of replication job " + job.ID + ": " +
						pve.WrapError(err).Error()})
				continue
			}
			if ok, why := ha.SyncAfterChange(st, a.changedAt); !ok {
				skip("the replica on node " + target + " is not verified yet (job " + job.ID + ": " + why + ")")
				continue
			}
		}
		candidates = append(candidates, replicaWidenCandidate{vmid: vmid, node: node, target: target})
	}

	if p.shuffle != nil {
		p.shuffle(candidates)
	} else {
		rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	}
	widened, attempts := 0, 0
	for i, c := range candidates {
		if p.opts.DryRun {
			report.Results = append(report.Results, ReplicaWidenResult{VMID: c.vmid, Node: c.node, Partner: c.target,
				Outcome: ReplicaWouldWiden, Reason: "the job's last sync follows the pin's last change; the check " +
					"under the VM's claim decides"})
			continue
		}
		if (p.opts.MaxWidened > 0 && widened >= p.opts.MaxWidened) ||
			(p.opts.MaxAttempts > 0 && attempts >= p.opts.MaxAttempts) || ctx.Err() != nil {
			report.Deferred += len(candidates) - i
			break
		}
		attempts++
		res, halt := p.widen(ctx, c, profile)
		report.Results = append(report.Results, res)
		if res.Outcome == ReplicaWidened {
			widened++
		}
		if halt != "" {
			report.Halted = halt
			report.Deferred += len(candidates) - i - 1
			break
		}
	}
	sortReplicaWidenResults(report.Results)
	return report, nil
}

// listRows lists the guests once and keeps the QEMU VMs that carry the
// replicate tag and no tag that proves them another Director's, in VMID
// order.
func (p *replicaWidenPass) listRows(ctx context.Context) ([]replicaWidenRow, error) {
	guestType := "vm"
	resp, err := p.deps.PVE.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &guestType})
	if err != nil {
		return nil, fmt.Errorf("replica widen: list the cluster's VMs: %w", pve.WrapError(err))
	}
	if resp == nil {
		return nil, nil
	}
	var rows []replicaWidenRow
	for _, raw := range *resp {
		var row replicaWidenRow
		if json.Unmarshal(raw, &row) != nil {
			continue
		}
		if row.Type != resourceTypeQemu || row.VMID.Int() <= 0 || row.Node == "" || bool(row.Template) ||
			!hasReplicaTag(row.Tags) || stragglerRowForeign(row.Tags, p.owner) {
			continue
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b replicaWidenRow) int { return int(a.VMID.Int() - b.VMID.Int()) })
	return rows, nil
}

// replicaWidenServiceHeld returns why the HA manager's state of a VM keeps the
// pin as it is, or "" for a VM that the manager runs or keeps stopped where it
// is. A VM that migrates, relocates, recovers, fences, freezes, or is in error
// is in the middle of a move or a recovery that the pin must not change under.
func replicaWidenServiceHeld(state string) string {
	switch state {
	case "started", "stopped":
		return ""
	case "":
		return "the HA manager lists no state for the VM"
	}
	return "the HA manager reports the VM as " + strconv.Quote(state) + ", so a move or a recovery may be running"
}

// widen checks one VM under its VMID claim and widens its pin when the check
// passes. halt is non-empty when no later VM can be claimed either.
func (p *replicaWidenPass) widen(
	ctx context.Context, c replicaWidenCandidate, profile *ha.Profile,
) (res ReplicaWidenResult, halt string) {
	res = ReplicaWidenResult{VMID: c.vmid, Node: c.node, Partner: c.target, Outcome: ReplicaWidenSkipped}
	logger := p.logger.With(log.Int(metadataKeyVMID, c.vmid), log.String("node", c.node))
	var widened bool
	var reason string
	var workErr error
	// The pass takes claims of its own. A claim set the caller's context
	// carried would let withVMIDClaim reuse a claim the caller holds, so the
	// pass drops it, and a VM the caller claims reads as held.
	base := context.WithValue(ctx, vmidClaimSetKey{}, (*vmidClaimSet)(nil))
	claimErr := withVMIDClaim(base, p.deps, c.vmid, "replica-widen/"+strconv.Itoa(c.vmid), claimForSweep, logger,
		func(ctx context.Context) error {
			widened, reason, workErr = p.widenClaimed(ctx, c, profile, logger)
			return nil
		})
	switch {
	case claimErr == nil && workErr == nil:
		res.Reason = reason
		if widened {
			res.Outcome = ReplicaWidened
		}
	case claimErr == nil:
		res.Outcome, res.Reason = ReplicaWidenFailed, workErr.Error()
	case isVMIDClaimHeld(claimErr):
		res.Reason = "another call claims the VM, such as a create, a delete, or another pass"
	case pve.IsClusterLockPermissionRefused(claimErr):
		res.Outcome = ReplicaWidenFailed
		res.Reason = "PVE refused the VMID claim for lack of permission: " + claimErr.Error()
		halt = "PVE refused the VMID claim for lack of permission, so no VM can be claimed; grant Pool.Allocate " +
			"and Pool.Audit on /pool so the CPI can create bosh-lock-vm-<vmid>"
	default:
		res.Outcome, res.Reason = ReplicaWidenFailed, "claim the VM: "+claimErr.Error()
	}
	return res, halt
}

// widenClaimed is the check of one claimed VM. It reads the VM's config on
// the node the listing named, which proves the VM still runs there, and
// checks that the VM holds no PVE lock, still carries the replicate tag, is
// this Director's, and that its HA resource carries this Director's marker
// and is not held by an operator or by the narrowing barrier. The replica
// check then runs with the VM's journal record held, and its write is guarded
// by the same reads under the rules lock.
func (p *replicaWidenPass) widenClaimed(
	ctx context.Context, c replicaWidenCandidate, profile *ha.Profile, logger *log.Logger,
) (widened bool, reason string, err error) {
	cfg, why, err := p.readConfig(ctx, c)
	if err != nil || why != "" {
		return false, why, err
	}
	tags, _ := pve.ConfigString(cfg, jsonKeyTags)
	verdict, err := checkVMOwnership(ctx, p.deps, replicaWidenOp, vmcid.VMCID{VMID: c.vmid},
		pve.VMLocation{Node: c.node, Tags: tags, Found: true, Type: resourceTypeQemu}, cfg)
	if err != nil {
		return false, "", fmt.Errorf("check the VM's ownership: %w", err)
	}
	if !verdict.Owned {
		return false, "the VM is not this Director's (" + verdict.Reason + ")", nil
	}
	if why, err := p.resourceHeld(ctx, c.vmid); err != nil || why != "" {
		return false, why, err
	}
	err = p.withRecord(ctx, c.vmid, cfg, func(handle *aj.Handle) error {
		if handle != nil && vmRecordTerminal(handle.Record().State) {
			reason = "the VM's journal record ended while the pass waited for it"
			return nil
		}
		w := &replicaWiden{deps: p.deps, logger: logger, api: p.api, profile: profile, vmid: c.vmid, node: c.node,
			handle: handle, now: p.now, guard: func(ctx context.Context, proof replicaWidenProof) error {
				return p.guard(ctx, c, profile, proof)
			}}
		var err error
		widened, reason, err = w.check(ctx)
		return err
	})
	switch {
	case errors.Is(err, errVMRecordBusy):
		return false, "another call holds the VM's journal record", nil
	case errors.Is(err, errReplicaWidenRecordUnmatched):
		return false, err.Error(), nil
	case err != nil:
		return false, "", err
	}
	return widened, reason, nil
}

// readConfig reads the VM's config on the candidate's node. why is non-empty
// when the config says the pin must stay as it is.
func (p *replicaWidenPass) readConfig(
	ctx context.Context, c replicaWidenCandidate,
) (cfg map[string]any, why string, err error) {
	cfg, err = p.deps.PVE.QEMU().Config(ctx, c.node, c.vmid)
	if err != nil {
		if pve.IsNotFound(err) {
			return nil, "the VM's config is not on node " + c.node + " any more", nil
		}
		return nil, "", fmt.Errorf("read the config of VM %d on node %s: %w", c.vmid, c.node, pve.WrapError(err))
	}
	if !configHasReplicaTag(cfg) {
		return nil, "the VM no longer carries the " + haReplicaTag + " tag", nil
	}
	if lock, _ := pve.ConfigString(cfg, "lock"); strings.TrimSpace(lock) != "" {
		return nil, "the VM holds the PVE lock " + strconv.Quote(lock) + ", so a task is changing it", nil
	}
	return cfg, "", nil
}

// resourceHeld reads the VM's HA resource and the HA manager's state of it,
// and returns why they keep the pin as it is, or "".
func (p *replicaWidenPass) resourceHeld(ctx context.Context, vmid int) (string, error) {
	sid := haResourceSid(vmid)
	res, err := p.deps.PVE.Cluster().GetHaResources(ctx, sid)
	if err != nil {
		if pve.IsNotFound(err) || isHaNotFound(err) {
			return "the VM has no HA resource", nil
		}
		return "", fmt.Errorf("read HA resource %s: %w", sid, pve.WrapError(err))
	}
	switch {
	case res == nil || res.Comment == nil || haOwnershipOf(*res.Comment, p.owner) != haOwnedByUs:
		return "the VM's HA resource does not carry this Director's marker", nil
	case ha.NarrowPending(res.State, res.Comment):
		return "the narrowing barrier holds the VM's HA resource ignored while it changes the VM", nil
	case pve.HARequestedState(res.State) == "ignored":
		return "the VM's HA resource is set to ignored", nil
	}
	states, err := pve.ReadHAServiceStates(ctx, p.deps.PVE.Cluster())
	if err != nil {
		return "", fmt.Errorf("read the HA manager status: %w", err)
	}
	return replicaWidenServiceHeld(states[sid]), nil
}

// guard is the re-read under the rules lock, right before the widened pin is
// written. The rule writer checks the pin itself against the one the check
// read. The guard checks the rest: the HA resource and the manager's state,
// the VM's config on its node with its volumes, and the job the sync ran
// under. Anything that differs from what the check verified is a skip.
func (p *replicaWidenPass) guard(
	ctx context.Context, c replicaWidenCandidate, profile *ha.Profile, proof replicaWidenProof,
) error {
	if why, err := p.resourceHeld(ctx, c.vmid); err != nil || why != "" {
		if err != nil {
			return err
		}
		return &replicaWidenSkip{reason: why}
	}
	cfg, why, err := p.readConfig(ctx, c)
	if err != nil {
		return err
	}
	if why != "" {
		return &replicaWidenSkip{reason: why}
	}
	volids, gaps := ha.ReplicaVolumes(c.vmid, haVolumesOfConfig(cfg, c.node, nil), profile.Storages)
	if len(gaps) > 0 || !slices.Equal(normalizedVolIDs(volids), normalizedVolIDs(proof.volids)) {
		return &replicaWidenSkip{reason: "the VM's volumes changed since the sync was verified"}
	}
	jobs, err := p.api.ListJobs(ctx)
	if err != nil {
		return fmt.Errorf("list the replication jobs: %w", err)
	}
	job, _, err := guestReplicaJobs(jobs, c.vmid, p.owner)
	switch {
	case err != nil:
		return err
	case job == nil || job.ID != proof.jobID:
		return &replicaWidenSkip{reason: "the VM's replication job changed since the sync was verified"}
	case job.Disabled || job.RemoveJob != "":
		return &replicaWidenSkip{reason: "the VM's replication job " + job.ID + " was disabled or marked for " +
			"removal since the sync was verified"}
	case replicaOtherNode(*job, c.node) != proof.target:
		return &replicaWidenSkip{reason: "the VM's replication job " + job.ID + " no longer copies to node " +
			proof.target}
	}
	return nil
}

// withRecord runs fn with the VM's journal record held, waiting a bounded
// time for it.
func (p *replicaWidenPass) withRecord(
	ctx context.Context, vmid int, cfg map[string]any, fn func(*aj.Handle) error,
) error {
	if p.record != nil {
		return p.record(ctx, vmid, cfg, fn)
	}
	pick := func(records []aj.Record) (*aj.Record, error) { return replicaWidenRecordFor(records, vmid, cfg) }
	return withVMRecordWhere(ctx, p.deps, pick, replicaWidenRecordWait, fn)
}

// errReplicaWidenRecordUnmatched says the journal holds a live record for the
// VM's VMID that the pass cannot prove is this VM's.
var errReplicaWidenRecordUnmatched = errors.New("the journal's records for the VM do not match its config")

// replicaWidenRecordFor selects the VM's journal record by the allocation
// marker its config carries, since a pass has no CID from the Director. A
// live record for the VMID that the marker does not name, more than one that
// it names, or a marker with no live record is refused, because the check must
// never run without the record that may hold back the widening. A VM with no
// marker and no live record for its VMID has no record, which is how a VM the
// journal never knew is checked.
func replicaWidenRecordFor(records []aj.Record, vmid int, cfg map[string]any) (*aj.Record, error) {
	var live, named []aj.Record
	for i := range records {
		r := records[i]
		if r.Kind != "vm" || !vmcid.SameVM(r.CID, vmid) || vmRecordTerminal(r.State) {
			continue
		}
		live = append(live, r)
		if managedVMMarkerMatches(cfg, r) {
			named = append(named, r)
		}
	}
	_, marked, err := pve.ParseStorageAllocationMarker(pve.DescriptionFromConfig(cfg))
	switch {
	case len(named) == 1:
		return &named[0], nil
	case len(named) > 1:
		return nil, fmt.Errorf("%w: %d live records carry the VM's marker", errReplicaWidenRecordUnmatched, len(named))
	case len(live) > 0:
		return nil, fmt.Errorf("%w: a live record names VMID %d, and the VM's marker does not name it",
			errReplicaWidenRecordUnmatched, vmid)
	case err != nil:
		return nil, fmt.Errorf("%w: the VM's allocation marker does not parse", errReplicaWidenRecordUnmatched)
	case marked:
		return nil, fmt.Errorf("%w: the VM's allocation marker names no live record", errReplicaWidenRecordUnmatched)
	}
	return nil, nil
}

func sortReplicaWidenResults(results []ReplicaWidenResult) {
	slices.SortStableFunc(results, func(a, b ReplicaWidenResult) int { return a.VMID - b.VMID })
}

// replicaWidenTriggerKey is the context key of a request's replicaWidenTrigger.
type replicaWidenTriggerKey struct{}

// replicaWidenTrigger records that a request took the HA rules lock, and the
// deps of the first call that took it, which the sweep after the request runs
// with.
type replicaWidenTrigger struct {
	mu   sync.Mutex
	deps *Deps
	done bool
}

// noteHARulesLock records on the request's trigger that it took the HA rules
// lock with deps. A context with no trigger, such as a sweep's own, records
// nothing.
func noteHARulesLock(ctx context.Context, deps Deps) {
	t, _ := ctx.Value(replicaWidenTriggerKey{}).(*replicaWidenTrigger)
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done || t.deps != nil {
		return
	}
	t.deps = &deps
}

// take ends the trigger and returns the deps it recorded.
func (t *replicaWidenTrigger) take() (Deps, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.done = true
	if t.deps == nil {
		return Deps{}, false
	}
	return *t.deps, true
}

// withReplicaWidenSweep wraps a CPI handler so that a request that took the
// HA rules lock runs the replica widening sweep after its work, with the deps
// of the first call that took the lock. The handler's result and error are
// returned as they are, whatever the sweep does.
func withReplicaWidenSweep(h Handler) Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		trigger := &replicaWidenTrigger{}
		out, err := h.Handle(context.WithValue(ctx, replicaWidenTriggerKey{}, trigger), args, reqCtx)
		if deps, ok := trigger.take(); ok && ctx.Err() == nil {
			sweepAwaitingReplicaPins(ctx, deps)
		}
		return out, err
	})
}

// sweepAwaitingReplicaPins is the widening pass a call that took the HA rules
// lock runs after its work, so a replicate VM's pin names its partner soon
// after the first verified sync, without waiting for a deploy to touch the VM.
// It is best-effort: every failure is logged and never fails the call. It is
// bounded by the sweep's caps and by the smaller of its budget and half of
// what remains of the request's deadline.
func sweepAwaitingReplicaPins(ctx context.Context, deps Deps) {
	if deps.Config == nil || deps.Config.HAModeValue() == config.HAModeOff || deps.PVE == nil ||
		strings.TrimSpace(deps.RequestDirectorUUID) == "" {
		return
	}
	logger := deps.Log(ctx)
	budget := replicaWidenSweepBudget
	if dl, ok := ctx.Deadline(); ok {
		if half := time.Until(dl) / 2; half < budget {
			budget = half
		}
	}
	if budget <= 0 {
		return
	}
	sctx, cancel := context.WithTimeout(context.WithValue(ctx, replicaWidenTriggerKey{}, (*replicaWidenTrigger)(nil)),
		budget)
	defer cancel()
	pass := newReplicaWidenPass(deps, ReplicaWidenOptions{MaxWidened: replicaWidenSweepMaxWidened,
		MaxAttempts: replicaWidenSweepMaxAttempts, MaxStatusReads: replicaWidenSweepMaxStatusReads})
	pass.logger = logger
	report, err := pass.run(sctx)
	if err != nil {
		logger.Info(replicaWidenOp+": the sweep did not run; a later call that takes the HA rules lock tries again "+
			"(non-fatal)", log.Err(err))
		return
	}
	logReplicaWidenReport(logger, report)
}

// logReplicaWidenReport logs what a sweep did: each widening and failure, and
// a summary when the sweep considered any VM.
func logReplicaWidenReport(logger *log.Logger, report ReplicaWidenReport) {
	for _, r := range report.Results {
		fields := []log.Field{log.Int(metadataKeyVMID, r.VMID), log.String("node", r.Node),
			log.String("partner", r.Partner), log.String("reason", r.Reason)}
		switch r.Outcome {
		case ReplicaWidened:
			logger.Info(replicaWidenOp+": widened the VM's pin to its partner", fields...)
		case ReplicaWidenFailed:
			logger.Warn(replicaWidenOp+": the VM's replica could not be checked; a later sweep tries again "+
				"(non-fatal)", fields...)
		default:
			logger.Debug(replicaWidenOp+": the VM's pin keeps its source alone for now", fields...)
		}
	}
	if len(report.Results) == 0 && report.Deferred == 0 {
		return
	}
	fields := []log.Field{log.Int("widened", report.Count(ReplicaWidened)),
		log.Int("skipped", report.Count(ReplicaWidenSkipped)), log.Int("failed", report.Count(ReplicaWidenFailed)),
		log.Int("deferred", report.Deferred)}
	if report.Halted != "" {
		logger.Warn(replicaWidenOp+": the sweep stopped early: "+report.Halted, fields...)
		return
	}
	logger.Info(replicaWidenOp+": checked the replicate VMs whose pins name their source alone", fields...)
}
