package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// haPinRuleNamePrefix is the start of the name the CPI gives a VM's or a
// parker's node-affinity pin, bosh-na-<vmid>.
const haPinRuleNamePrefix = "bosh-na-"

// HAStateScanPolicy says whether a VMID allocation reads the cluster's HA
// state, and how it answers a read of it that fails.
//
// Under every policy, a list endpoint that PVE does not implement (HTTP 501,
// or 404 for the path) contributes nothing, and the scan goes on with the
// other list. PVE 8, and an entry node that a cluster upgrade has not reached
// yet, has no rules endpoint, and such a cluster holds no HA rule.
type HAStateScanPolicy int

const (
	// HAStateScanNone reads no HA state. It is the zero value.
	HAStateScanNone HAStateScanPolicy = iota
	// HAStateScanBestEffort reads the HA state, and when a read fails for
	// any reason it allocates with only what it read before the failure,
	// with a warning. pve.ha.mode off uses it, because nothing there promised to
	// read HA state before a park.
	HAStateScanBestEffort
	// HAStateScanToleratingDenial allocates without the HA state, with a
	// warning that names Sys.Audit on /, when PVE refuses the read for a
	// missing grant, and fails the allocation on any other failed read:
	// retriably when the failure may pass, and permanently when it is PVE's
	// verdict about the request. pve.ha.mode auto uses it, which parks as
	// if PVE HA managed no parker when the grant is missing.
	HAStateScanToleratingDenial
	// HAStateScanRequiringGrant fails the allocation on every failed read,
	// and a refusal for a missing grant fails it with a non-retriable error
	// that names Sys.Audit on /. pve.ha.mode managed uses it.
	HAStateScanRequiringGrant
)

// String names the policy for logs and test failures.
func (p HAStateScanPolicy) String() string {
	switch p {
	case HAStateScanNone:
		return "none"
	case HAStateScanBestEffort:
		return "best-effort"
	case HAStateScanToleratingDenial:
		return "tolerating-denial"
	case HAStateScanRequiringGrant:
		return "requiring-grant"
	}
	return "HAStateScanPolicy(" + strconv.Itoa(int(p)) + ")"
}

// haStateReadPrivilege is the privilege PVE checks for the HA resource and
// rule lists.
const haStateReadPrivilege = "Sys.Audit on /"

// WithHAStateScan makes NextVMID also treat as used every VMID the cluster's
// HA state names: the guest of every HA resource, every resource an HA rule
// lists, and the VMID in the name of every bosh-na-<vmid> pin. A VM or parker
// whose config is gone can leave its resource or its pin behind, and a guest
// created at that VMID would inherit them, so an allocator that must never
// adopt HA state it did not write asks for this scan. policy says how a read
// that fails is answered (HAStateScanPolicy), and HAStateScanNone leaves the
// option a no-op.
func WithHAStateScan(policy HAStateScanPolicy) AllocOption {
	return func(o *allocOpts) {
		o.haStateScan = policy
	}
}

// haStateScanWarned holds, per reason, whether this process has already
// warned that an allocation went on without the HA state. Each CPI call runs
// in its own process, so the operator sees one warning per call however many
// parkers the call allocates, and later ones log at debug level.
var haStateScanWarned struct {
	denied     atomic.Bool
	unreadable atomic.Bool
}

// warnHAStateScanOnce logs msg as a warning the first time this process logs
// it for flag, and at debug level after that.
func warnHAStateScanOnce(ctx context.Context, flag *atomic.Bool, msg string, fields ...log.Field) {
	logger := log.FromContext(ctx)
	if flag.CompareAndSwap(false, true) {
		logger.Warn(msg, fields...)
		return
	}
	logger.Debug(msg, fields...)
}

// haStateListMissing reports whether err says PVE does not implement the
// list endpoint at all: a 501, or a 404 for the path. A list answers an empty
// set rather than a 404, so a 404 can only mean the path is unknown.
func haStateListMissing(err error) bool {
	code, ok := apiHTTPCode(err)
	return ok && (code == 501 || code == 404)
}

// haStateScanFailure answers a failed read of the HA list what under policy.
// It returns nil when the allocation goes on without that list, and the error
// that fails the allocation otherwise.
func haStateScanFailure(ctx context.Context, policy HAStateScanPolicy, err error, what string) error {
	msg := "NextVMID: list the " + what
	if ctxErr := ctx.Err(); ctxErr != nil {
		return cpierrors.Wrap(WrapError(err), msg)
	}
	denied := IsPermissionDenied(err)
	switch {
	case denied && policy != HAStateScanRequiringGrant:
		warnHAStateScanOnce(ctx, &haStateScanWarned.denied,
			"pve: PVE refused the HA state read of a parker's VMID allocation, because the token lacks "+
				haStateReadPrivilege+"; the VMID is chosen as if no HA resource or rule named one, so a new parker "+
				"can land on a VMID that a gone parker's leftover HA resource or pin still names, until the "+
				"privilege is granted", log.String("list", what), log.Err(err))
		return nil
	case policy == HAStateScanBestEffort:
		warnHAStateScanOnce(ctx, &haStateScanWarned.unreadable,
			"pve: the HA state read of a parker's VMID allocation failed; the VMID is chosen without the "+
				"list that could not be read", log.String("list", what), log.Err(err))
		return nil
	case denied:
		return cpierrors.Wrap(WrapError(err), fmt.Sprintf(
			"%s: PVE refused the read, because the token lacks %s, which pve.ha.mode managed needs to keep a new "+
				"parker off a VMID that a gone parker's HA resource or pin still names; grant %s and retry",
			msg, haStateReadPrivilege, haStateReadPrivilege))
	case IsRequestVerdict(err):
		return cpierrors.Wrap(WrapError(err), msg)
	default:
		return cpierrors.WrapAs(WrapError(err), cpierrors.TypeRetriableCloud, msg)
	}
}

// readHAStateList runs one HA list read with the transient retries, and
// reports whether PVE does not implement the endpoint, which a retry never
// changes, so a 501 is never retried as a server fault.
func readHAStateList(ctx context.Context, label string, read func() error) (missing bool, err error) {
	err = RetryOnTransient(ctx, nil, label, 0, func() error {
		inner := read()
		if inner != nil && haStateListMissing(inner) {
			missing = true
			return nil
		}
		return inner
	})
	return missing, err
}

// listHAStateVMIDs returns the VMIDs the cluster's HA resources and rules
// name, as WithHAStateScan describes, answering a failed read by policy.
func listHAStateVMIDs(ctx context.Context, c Client, policy HAStateScanPolicy) (map[int]struct{}, error) {
	svc := c.Cluster()
	if svc == nil {
		if policy == HAStateScanBestEffort {
			return nil, nil
		}
		return nil, cpierrors.Cloud("NextVMID: the HA state scan has no cluster client")
	}
	s := &haStateScan{ctx: ctx, policy: policy, used: map[int]struct{}{}}
	if stop, err := s.scanResources(svc); stop || err != nil {
		return s.result(err)
	}
	return s.result(s.scanRules(svc))
}

// haStateScan collects the VMIDs one HA state scan finds.
type haStateScan struct {
	ctx    context.Context //nolint:containedctx // One scan's reads share the allocation's context.
	policy HAStateScanPolicy
	used   map[int]struct{}
}

const haStateResourcesList, haStateRulesList = "HA resources", "HA rules"

// result returns what the scan found, or err when the scan failed.
func (s *haStateScan) result(err error) (map[int]struct{}, error) {
	if err != nil {
		return nil, err
	}
	return s.used, nil
}

// addSID records the VMID a vm: or ct: service ID names.
func (s *haStateScan) addSID(sid string) {
	kind, id, ok := strings.Cut(strings.TrimSpace(sid), ":")
	if !ok || (kind != "vm" && kind != "ct") {
		return
	}
	if vmid, err := strconv.Atoi(id); err == nil && vmid > 0 {
		s.used[vmid] = struct{}{}
	}
}

// fail answers a list that could not be used. A nil answer lets the
// allocation go on with what the scan has read so far.
func (s *haStateScan) fail(err error, what string) error {
	return haStateScanFailure(s.ctx, s.policy, err, what)
}

// malformed answers a row that does not parse. A best-effort scan answers it
// as any failed read, and every other scan fails the allocation.
func (s *haStateScan) malformed(err error, what string, format string, args ...any) error {
	if s.policy == HAStateScanBestEffort {
		return s.fail(err, what)
	}
	return cpierrors.Cloud(format, args...)
}

// scanResources records the VMIDs the HA resources name. It reports stop when
// the scan answered a failed read and must not read the rules.
func (s *haStateScan) scanResources(svc sdkcluster.Service) (stop bool, err error) {
	var resources *sdkcluster.ListHaResourcesResponse
	missing, err := readHAStateList(s.ctx, "vmid_ha_resources_scan", func() error {
		var inner error
		resources, inner = svc.ListHaResources(s.ctx, nil)
		return inner
	})
	switch {
	case err != nil:
		return true, s.fail(err, haStateResourcesList)
	case missing:
		log.FromContext(s.ctx).Debug("pve: PVE does not implement the HA resources list, so the VMID allocation " +
			"scans no HA resource")
		return false, nil
	case resources == nil:
		return true, s.fail(cpierrors.Retriable("no rows returned"), haStateResourcesList)
	}
	for i, raw := range *resources {
		var row struct {
			SID string `json:"sid"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return true, s.malformed(err, haStateResourcesList, "NextVMID: HA resources row %d does not parse: %v", i, err)
		}
		s.addSID(row.SID)
	}
	return false, nil
}

// scanRules records the VMIDs the HA rules name, by their resources and by a
// node pin's name.
func (s *haStateScan) scanRules(svc sdkcluster.Service) error {
	var rules *sdkcluster.ListHaRulesResponse
	missing, err := readHAStateList(s.ctx, "vmid_ha_rules_scan", func() error {
		var inner error
		rules, inner = svc.ListHaRules(s.ctx, &sdkcluster.ListHaRulesParams{})
		return inner
	})
	switch {
	case err != nil:
		return s.fail(err, haStateRulesList)
	case missing:
		log.FromContext(s.ctx).Debug("pve: PVE does not implement the HA rules list, as on PVE 8, so the VMID " +
			"allocation scans the HA resources alone")
		return nil
	case rules == nil:
		return s.fail(cpierrors.Retriable("no rows returned"), haStateRulesList)
	}
	for i, raw := range *rules {
		var row struct {
			Rule      string          `json:"rule"`
			Resources json.RawMessage `json:"resources"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return s.malformed(err, haStateRulesList, "NextVMID: HA rules row %d does not parse: %v", i, err)
		}
		if id, ok := strings.CutPrefix(row.Rule, haPinRuleNamePrefix); ok {
			if vmid, err := strconv.Atoi(id); err == nil && vmid > 0 {
				s.used[vmid] = struct{}{}
			}
		}
		sids, err := haRuleResourceSIDs(row.Resources)
		if err != nil {
			return s.malformed(err, haStateRulesList,
				"NextVMID: HA rule %q lists resources that do not parse: %v", row.Rule, err)
		}
		for _, sid := range sids {
			s.addSID(sid)
		}
	}
	return nil
}

// haRuleResourceSIDs reads a rule's resources field, which PVE sends as one
// comma-separated string and a client may see as an array of strings. An
// absent or null field lists none.
func haRuleResourceSIDs(raw json.RawMessage) ([]string, error) {
	if strings.TrimSpace(string(raw)) == "" {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	var list []string
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		for _, sid := range strings.Split(v, ",") {
			if sid = strings.TrimSpace(sid); sid != "" {
				list = append(list, sid)
			}
		}
	case []any:
		for _, item := range v {
			sid, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("a rule resource is %T, not a string", item)
			}
			list = append(list, sid)
		}
	default:
		return nil, fmt.Errorf("the rule resources field is %T, not a string or a list", value)
	}
	return list, nil
}
