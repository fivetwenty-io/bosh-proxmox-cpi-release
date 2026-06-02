package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
)

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
	members, scanErr := collectGroupMemberSids(ctx, deps, groupTag)
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
		return createNegativeRule(ctx, svc, ruleName, csv, groupKey, strict)
	}

	// 4b. Rule exists: recreate only when the membership actually changed
	// (PVE has no partial-edit for rules; recreate = delete + create).
	if sameSidSet(existing.Resources, members) {
		return nil
	}
	if err := svc.DeleteHaRules(ctx, ruleName); err != nil && !isHaNotFound(err) {
		return fmt.Errorf("anti-affinity: delete rule %q for recreate: %w", ruleName, err)
	}
	return createNegativeRule(ctx, svc, ruleName, csv, groupKey, strict)
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
// and returns their HA sids ("vm:<vmid>") as a set. The VM being created is not
// yet tagged, so the caller adds its own sid afterwards.
func collectGroupMemberSids(ctx context.Context, deps Deps, groupTag string) (map[string]struct{}, error) {
	resp, err := deps.PVE.Cluster().ListResources(ctx, &cluster.ListResourcesParams{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{})
	if resp == nil {
		return out, nil
	}
	for _, raw := range *resp {
		var ri struct {
			Type string `json:"type"`
			Vmid int64  `json:"vmid"`
			Tags string `json:"tags"`
		}
		if json.Unmarshal(raw, &ri) != nil {
			continue
		}
		if ri.Type != "qemu" || ri.Vmid == 0 {
			continue
		}
		if tagsContain(ri.Tags, groupTag) {
			out["vm:"+strconv.FormatInt(ri.Vmid, 10)] = struct{}{}
		}
	}
	return out, nil
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
