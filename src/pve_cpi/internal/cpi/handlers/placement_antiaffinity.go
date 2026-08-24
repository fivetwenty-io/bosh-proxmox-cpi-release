package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// aaOwnerSeq distinguishes successive lock acquisitions within one CPI process
// so the owner token stamped on a sentinel pool is unique per acquire (the pid
// alone would repeat across concurrent goroutines in the same process).
var aaOwnerSeq atomic.Uint64

// clusterLockOwner builds a process-and-request-unique owner token for a
// sentinel cluster lock: "<pid>-<seq>-<key>-<vmid>". It is stamped into the
// pool comment for diagnostics and to let an expired holder be distinguished
// from a live one. The token never needs to be parsed back.
func clusterLockOwner(key string, vmid int) string {
	return fmt.Sprintf("%d-%d-%s-%d", os.Getpid(), aaOwnerSeq.Add(1), key, vmid)
}

// haRuleNamePrefix namespaces every CPI-managed HA anti-affinity rule so a
// cluster-wide scan can find and clean them by vmid without knowing the group.
const haRuleNamePrefix = "bosh-aa-"

// haResourceType and haRuleAffinity are the fixed PVE values for negative
// (spread-apart) resource-affinity rules on PVE 9.x.
const (
	haRuleType     = "resource-affinity"
	haRuleAffinity = "negative"
)

// haRuleEntry is the subset of a GET /cluster/ha/rules list entry the CPI reads.
// Resources is decoded flexibly (see parseHaResources) because PVE may return
// it as a CSV string, an array, or an object keyed by resource id.
type haRuleEntry struct {
	Rule      string          `json:"rule"`
	Type      string          `json:"type"`
	Affinity  string          `json:"affinity"`
	Resources json.RawMessage `json:"resources"`
}

// haResourceSid renders the PVE HA resource id for a VMID, e.g. 100 -> "vm:100".
func haResourceSid(vmid int) string {
	return "vm:" + strconv.Itoa(vmid)
}

// haRuleNameFor renders the deterministic HA rule name for a sanitized instance
// group key, e.g. "web" -> "bosh-aa-web".
func haRuleNameFor(groupKey string) string {
	return haRuleNamePrefix + groupKey
}

// applyAntiAffinityMembership is the create_vm entry point for opt-in HA
// anti-affinity. It is a no-op unless anti_affinity.use_ha_rules is enabled and
// the VM carries an instance-group name. Generic HA failures (HA unconfigured,
// a transient rule-write hiccup) are logged non-fatally, preserving the §7.21
// best-effort intent; a TypeRetriableCloud error (cluster-lock timeout or
// read-after-write verify failure) is returned so the director re-drives rather
// than silently dropping the VM from its spread rule.
func applyAntiAffinityMembership(ctx context.Context, deps Deps, vmid int, env map[string]any, logger *log.Logger) error {
	if !deps.Config.AntiAffinityUseHaRulesEnabled() {
		return nil
	}
	groupKey := sanitizeTagValue(instanceGroupName(env))
	if groupKey == "" {
		return nil
	}
	if aaErr := ensureAntiAffinityMembership(ctx, deps, groupKey, vmid, logger); aaErr != nil {
		if cpierrors.IsType(aaErr, cpierrors.TypeRetriableCloud) {
			// Lock-timeout or verify-failure: propagate so the director re-drives.
			return aaErr
		}
		logger.Warn("create_vm: HA anti-affinity membership not fully applied (non-fatal)",
			log.Int(metadataKeyVMID, vmid), log.String("group", groupKey), log.Err(aaErr))
	}
	return nil
}

// ensureAntiAffinityMembership registers the VM as a PVE HA resource and adds it
// to the cluster-level negative resource-affinity rule for its BOSH instance
// group, so PVE enforces spreading at the hypervisor level. It is best-effort:
// every PVE failure (including "HA not configured on this cluster") is returned
// for the caller to log as a warning, never to fail create_vm.
//
// A negative resource-affinity rule is only meaningful with two or more members,
// so the rule is materialized from the live set of same-group guests plus this
// VM. When fewer than two members exist the rule is not created (and a stale
// single-member rule, if any, is removed).
func ensureAntiAffinityMembership(ctx context.Context, deps Deps, groupKey string, vmid int, logger *log.Logger) error {
	if groupKey == "" {
		return nil
	}

	// When the cross-process cluster lock is enabled, serialize the entire
	// read-modify-write on the shared bosh-aa-<group> rule against concurrent
	// create_vm invocations on other hosts. The lock is keyed on the group so
	// different instance groups never block each other. Acquire failure/timeout
	// surfaces as a retriable error (the caller logs it best-effort, the director
	// re-drives); the lock is always released on return, including on RMW error.
	if deps.Config.ClusterLockEnabled() {
		handle, lockErr := acquireAntiAffinityLock(ctx, deps, groupKey, vmid)
		if lockErr != nil {
			return lockErr
		}
		defer func() {
			// Detached + bounded for the same reason as withVMIDLock's
			// release: a cancelled request must not orphan the sentinel
			// pool, and the anti-affinity lock's longer TTL makes an orphan
			// proportionally more disruptive to concurrent create_vm calls.
			relCtx, relCancel := detachedContext(ctx, lockReleaseTimeout)
			defer relCancel()
			if relErr := handle.Release(relCtx); relErr != nil {
				logger.Warn("anti-affinity: release cluster lock failed (non-fatal)",
					log.String("group", groupKey), log.Err(relErr))
			}
		}()
	}

	return ensureAntiAffinityMembershipLocked(ctx, deps, groupKey, vmid, logger)
}

// acquireAntiAffinityLock acquires the per-group cross-process sentinel lock.
// Separated so the lock plumbing (owner token, timeout/TTL from config) stays
// out of the RMW body.
func acquireAntiAffinityLock(ctx context.Context, deps Deps, groupKey string, vmid int) (*pve.ClusterLockHandle, error) {
	poolSvc := deps.PVE.Pools()
	if poolSvc == nil {
		// No pool service wired: cannot lock. Treat as retriable so the operator
		// notices the misconfiguration rather than racing silently.
		return nil, cpierrors.WrapAs(
			cpierrors.Cloud("anti-affinity: cluster_lock_mode=pool but no pool service available"),
			cpierrors.TypeRetriableCloud, "anti-affinity: acquire cluster lock")
	}
	// TTL and timeout are deliberately decoupled: TTL is 2× the acquire
	// timeout so a holder whose RMW runs for the full timeout duration is not
	// stolen mid-flight by a concurrent waiter. A crashed holder (RMW aborted,
	// release never called) is reclaimed at 2×timeout — acceptable for the
	// advisory lock use-case. The 2× factor is a sane default; the exact ratio
	// is tunable by adjusting cluster_lock_timeout_sec (timeout) independently.
	timeout := time.Duration(deps.Config.ClusterLockTimeoutSecValue()) * time.Second
	ttl := 2 * timeout
	owner := clusterLockOwner(groupKey, vmid)
	return pve.AcquireClusterLock(ctx, poolSvc, "aa-"+groupKey, owner, ttl, timeout)
}

// ensureAntiAffinityMembershipLocked is the read-modify-write body, run under
// the cluster lock when enabled. It registers the HA resource, recomputes the
// member set, and recreates the bosh-aa-<group> rule when membership changed.
func ensureAntiAffinityMembershipLocked(
	ctx context.Context, deps Deps, groupKey string, vmid int, logger *log.Logger,
) error {
	svc := deps.PVE.Cluster()
	sid := haResourceSid(vmid)
	ruleName := haRuleNameFor(groupKey)
	groupTag := "job--" + groupKey

	// 1. Register the VM as an HA resource (idempotent; "already defined" is fine).
	if err := svc.CreateHaResources(ctx, &cluster.CreateHaResourcesParams{Sid: sid}); err != nil {
		if !isHaAlreadyExists(err) {
			logger.Warn("anti-affinity: register HA resource failed (continuing best-effort)",
				log.String("sid", sid), log.Err(err))
		}
	}

	// 2. Compute the full member set: existing same-group guests + this VM.
	members, live, liveComplete, scanErr := collectGroupMemberSids(ctx, deps, groupTag)
	if scanErr != nil {
		// Without the member set we cannot safely recreate the rule. Warn and stop.
		return fmt.Errorf("anti-affinity: scan group members: %w", scanErr)
	}
	members[sid] = struct{}{}

	// 3. Locate any existing rule for this group.
	existing, listErr := findHaRule(ctx, svc, ruleName)
	if listErr != nil {
		return fmt.Errorf("anti-affinity: list HA rules: %w", listErr)
	}

	// This ensure path only ever grows a rule: union the existing rule's
	// members into the freshly scanned set instead of replacing them. However
	// authoritative the scan, its TAG view can still trail reality (a
	// same-group twin mid-create has no tag yet), and the delete path
	// (removeAntiAffinityMembership) is the sole authority for shrinking, so
	// a smaller scan here must never drop members another create registered.
	// The one exception is a sid whose GUEST no longer exists anywhere in the
	// fleet (destroyed outside the CPI, or a failed delete_vm HA cleanup):
	// the enumeration in hand is authoritative for existence, and recreating
	// the rule around a dangling sid would name a resource PVE no longer
	// has, wedging every later create in the group. That drop is gated on
	// liveComplete: when the enumeration excluded offline members, a sid
	// missing from live may simply sit on an unenumerated node, and dropping
	// it would silently void the spread guarantee for the reboot window, so
	// every inherited sid is kept instead.
	if existing != nil {
		for s := range parseHaResources(existing.Resources) {
			if _, ok := live[s]; !ok && liveComplete {
				logger.Info("anti-affinity: dropping rule member with no live guest",
					log.String("rule", ruleName), log.String("sid", s))
				continue
			}
			members[s] = struct{}{}
		}
	}

	// Fewer than two members: a negative-affinity rule is meaningless. Remove a
	// stale rule if one survived from an earlier larger membership.
	if len(members) < 2 {
		if existing != nil {
			if err := svc.DeleteHaRules(ctx, ruleName); err != nil && !isHaNotFound(err) {
				return fmt.Errorf("anti-affinity: delete single-member rule %q: %w", ruleName, err)
			}
		}
		logger.Debug("anti-affinity: fewer than two group members; no HA rule needed",
			log.String("rule", ruleName), log.Int("members", len(members)))
		return nil
	}

	csv := sidsCSV(members)
	strict := deps.Config.AntiAffinityStrict()

	// 4a. No rule yet: create it.
	if existing == nil {
		if err := createNegativeRule(ctx, svc, ruleName, csv, groupKey, strict); err != nil {
			return err
		}
		return verifyAntiAffinityMember(ctx, deps, ruleName, sid, logger)
	}

	// 4b. Rule exists: recreate only when the membership actually changed
	// (PVE has no partial-edit for rules; recreate = delete + create).
	if sameSidSet(existing.Resources, members) {
		return nil
	}
	if err := svc.DeleteHaRules(ctx, ruleName); err != nil && !isHaNotFound(err) {
		return fmt.Errorf("anti-affinity: delete rule %q for recreate: %w", ruleName, err)
	}
	if err := createNegativeRule(ctx, svc, ruleName, csv, groupKey, strict); err != nil {
		return err
	}
	return verifyAntiAffinityMember(ctx, deps, ruleName, sid, logger)
}

// verifyAntiAffinityMember performs the opt-in read-after-write check: it
// re-lists the HA rules and asserts that sid is present in ruleName's member
// set. A concurrent writer that recreated the rule without this VM (lost-update)
// is caught here and surfaced as a retriable error so the director re-drives,
// rather than silently losing the spread guarantee. A no-op when
// antiaffinity_verify is off.
//
// A single re-read is used here. PVE HA-rule reads are strongly consistent
// within the same cluster because pmxcfs serializes writes; a false-negative
// (rule absent) is therefore a real concurrent drop, not read lag, so a single
// re-read is sufficient. A false-negative triggers a bounded director re-drive
// (not a storm): the director re-issues create_vm once before declaring failure.
func verifyAntiAffinityMember(ctx context.Context, deps Deps, ruleName, sid string, logger *log.Logger) error {
	if !deps.Config.AntiAffinityVerifyEnabled() {
		return nil
	}
	svc := deps.PVE.Cluster()
	rule, err := findHaRule(ctx, svc, ruleName)
	if err != nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("anti-affinity: verify re-list rule %q", ruleName))
	}
	if rule == nil {
		return cpierrors.WrapAs(
			cpierrors.Cloud("anti-affinity: rule %q absent after recreate (concurrent drop)", ruleName),
			cpierrors.TypeRetriableCloud, "anti-affinity: verify membership")
	}
	members := parseHaResources(rule.Resources)
	if _, ok := members[sid]; !ok {
		return cpierrors.WrapAs(
			cpierrors.Cloud("anti-affinity: %s missing from rule %q after recreate (concurrent drop)", sid, ruleName),
			cpierrors.TypeRetriableCloud, "anti-affinity: verify membership")
	}
	logger.Debug("anti-affinity: read-after-write verify passed",
		log.String("rule", ruleName), log.String("sid", sid))
	return nil
}

// removeAntiAffinityMembership removes a VM from any CPI-managed HA anti-affinity
// rule and deregisters its HA resource. It is keyed solely on vmid because
// delete_vm has no access to env.bosh (the group name is unknown at delete
// time): it scans every bosh-aa-* rule for the VM's sid. All failures are
// returned for the caller to log; cleanup never blocks VM deletion.
func removeAntiAffinityMembership(ctx context.Context, deps Deps, vmid int, logger *log.Logger) error {
	svc := deps.PVE.Cluster()
	sid := haResourceSid(vmid)

	rules, listErr := listHaRules(ctx, svc)
	if listErr != nil {
		return fmt.Errorf("anti-affinity: list HA rules for cleanup: %w", listErr)
	}

	var firstErr error
	for i := range rules {
		r := rules[i]
		if !strings.HasPrefix(r.Rule, haRuleNamePrefix) {
			continue
		}
		sids := parseHaResources(r.Resources)
		if _, member := sids[sid]; !member {
			continue
		}
		delete(sids, sid)
		if len(sids) < 2 {
			// Empty or now single-member: the rule no longer spreads anything.
			if err := svc.DeleteHaRules(ctx, r.Rule); err != nil && !isHaNotFound(err) && firstErr == nil {
				firstErr = fmt.Errorf("anti-affinity: delete rule %q: %w", r.Rule, err)
			}
			continue
		}
		// Recreate with the remaining members.
		groupKey := strings.TrimPrefix(r.Rule, haRuleNamePrefix)
		if err := svc.DeleteHaRules(ctx, r.Rule); err != nil && !isHaNotFound(err) {
			if firstErr == nil {
				firstErr = fmt.Errorf("anti-affinity: delete rule %q for recreate: %w", r.Rule, err)
			}
			continue
		}
		if err := createNegativeRule(ctx, svc, r.Rule, sidsCSV(sids), groupKey, deps.Config.AntiAffinityStrict()); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Deregister the HA resource. Purge removes it from any remaining rules too.
	purge := true
	if err := svc.DeleteHaResources(ctx, sid, &cluster.DeleteHaResourcesParams{Purge: &purge}); err != nil {
		if !isHaNotFound(err) {
			logger.Debug("anti-affinity: deregister HA resource (non-fatal)",
				log.String("sid", sid), log.Err(err))
		}
	}
	return firstErr
}

// createNegativeRule creates a negative resource-affinity rule. When strict is
// true PVE enforces hard node-separation for the rule members; when false
// (the default) the rule is advisory only. See AntiAffinityConfig.Strict for
// the small-cluster hazard of enabling strict mode.
func createNegativeRule(ctx context.Context, svc cluster.Service, ruleName, csv, groupKey string, strict bool) error {
	affinity := haRuleAffinity
	comment := "BOSH anti-affinity for instance group " + groupKey
	if err := svc.CreateHaRules(ctx, &cluster.CreateHaRulesParams{
		Rule:      ruleName,
		Type:      haRuleType,
		Affinity:  &affinity,
		Strict:    &strict,
		Resources: csv,
		Comment:   &comment,
	}); err != nil {
		return fmt.Errorf("anti-affinity: create rule %q: %w", ruleName, err)
	}
	return nil
}

// collectGroupMemberSids scans the cluster for QEMU guests tagged with groupTag
// and returns their HA sids ("vm:<vmid>") as a set, plus the sids of EVERY
// live guest regardless of tag (the union step uses the latter to drop an
// inherited rule member whose guest is provably gone), plus liveComplete,
// which is true only when the enumeration covered every cluster member.
// When it is false the live set proves nothing about absence: a guest on an
// offline-excluded member is unenumerated, not gone, so the union step must
// keep every inherited sid. The VM being created is not yet tagged, so the
// caller adds its own sid afterwards.
//
// The scan reads authoritative per-node listings, not the /cluster/resources
// index: the index lags by minutes, and a same-group VM created moments ago
// would be invisible to it, so a rule recomputed from the index could drop a
// live member. Tolerant form so an offline member does not block every
// create in the group; an enumeration failure is still returned (retriable)
// rather than answered from a partial fleet.
func collectGroupMemberSids(ctx context.Context, deps Deps, groupTag string) (members, live map[string]struct{}, liveComplete bool, err error) {
	guests, excluded, err := pve.ListGuestsAuthoritativeTolerant(ctx, deps.PVE, nil)
	if err != nil {
		return nil, nil, false, err
	}
	members = make(map[string]struct{})
	live = make(map[string]struct{}, len(guests))
	for _, g := range guests {
		sid := "vm:" + strconv.Itoa(g.VMID)
		live[sid] = struct{}{}
		if tagsContain(g.Tags, groupTag) {
			members[sid] = struct{}{}
		}
	}
	return members, live, len(excluded) == 0, nil
}

// findHaRule returns the rule named ruleName, or nil when it does not exist.
func findHaRule(ctx context.Context, svc cluster.Service, ruleName string) (*haRuleEntry, error) {
	rules, err := listHaRules(ctx, svc)
	if err != nil {
		return nil, err
	}
	for i := range rules {
		if rules[i].Rule == ruleName {
			return &rules[i], nil
		}
	}
	return nil, nil
}

// listHaRules fetches and decodes every HA rule. A nil response is treated as
// an empty list (HA may be unconfigured).
func listHaRules(ctx context.Context, svc cluster.Service) ([]haRuleEntry, error) {
	resp, err := svc.ListHaRules(ctx, &cluster.ListHaRulesParams{})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}
	out := make([]haRuleEntry, 0, len(*resp))
	for _, raw := range *resp {
		var e haRuleEntry
		if json.Unmarshal(raw, &e) != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// parseHaResources decodes a rule's "resources" field, which PVE may serialize
// as a CSV string ("vm:100,vm:101"), a JSON array, or an object keyed by sid.
// It returns the set of resource ids.
func parseHaResources(raw json.RawMessage) map[string]struct{} {
	out := make(map[string]struct{})
	if len(raw) == 0 {
		return out
	}
	// String CSV.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		addCSVSids(out, s)
		return out
	}
	// Array of strings.
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		for _, v := range arr {
			if v = strings.TrimSpace(v); v != "" {
				out[v] = struct{}{}
			}
		}
		return out
	}
	// Object keyed by sid.
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for k := range obj {
			if k = strings.TrimSpace(k); k != "" {
				out[k] = struct{}{}
			}
		}
	}
	return out
}

// addCSVSids splits a comma-separated sid list into out.
func addCSVSids(out map[string]struct{}, csv string) {
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = struct{}{}
		}
	}
}

// sidsCSV renders a sid set as a deterministic (sorted) comma-separated string.
func sidsCSV(sids map[string]struct{}) string {
	keys := make([]string, 0, len(sids))
	for k := range sids {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// sameSidSet reports whether the rule's existing resources equal want.
func sameSidSet(existing json.RawMessage, want map[string]struct{}) bool {
	have := parseHaResources(existing)
	if len(have) != len(want) {
		return false
	}
	for k := range want {
		if _, ok := have[k]; !ok {
			return false
		}
	}
	return true
}

// tagsContain reports whether the PVE tags field contains an exact match for
// want, honoring PVE's ";"/"," separators (reuses parseTagsField).
func tagsContain(tags, want string) bool {
	if tags == "" || want == "" {
		return false
	}
	for _, t := range parseTagsField(tags) {
		if t == want {
			return true
		}
	}
	return false
}

// isHaAlreadyExists reports whether err indicates the HA resource is already
// defined (an idempotent no-op for ensureAntiAffinityMembership).
func isHaAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already defined") || strings.Contains(msg, "already exists")
}

// isHaNotFound reports whether err indicates the HA rule/resource is absent
// (an idempotent no-op for delete paths).
func isHaNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "not defined") ||
		strings.Contains(msg, "404")
}
