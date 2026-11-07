package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// Values of the pve.ha.mode key.
const (
	HAModeOff     = "off"
	HAModeAuto    = "auto"
	HAModeManaged = "managed"
)

// Values of the pve.ha.storage_topology key.
const (
	HAStorageTopologyAuto   = "auto"
	HAStorageTopologyShared = "shared"
	HAStorageTopologyLocal  = "local"
	HAStorageTopologyMixed  = "mixed"
)

// Values of the pve.ha.local_policy key.
const (
	HALocalPolicyNone      = "none"
	HALocalPolicyHold      = "hold"
	HALocalPolicyReplicate = "replicate"
)

// Values shared by pve.ha.anti_affinity and pve.ha.pin_az.
const (
	HARuleOff  = "off"
	HARuleAuto = "auto"
	HARuleOn   = "on"
)

// Values of the pve.ha.anti_affinity_overflow key.
const (
	HAOverflowSkip  = "skip"
	HAOverflowSplit = "split"
)

// Values of the pve.ha.headroom key.
const (
	HAHeadroomOff     = "off"
	HAHeadroomWarn    = "warn"
	HAHeadroomEnforce = "enforce"
)

// Compiled defaults of the pve.ha block. The block is never materialized by
// ApplyDefaults: an absent block and an absent key both read as these values
// through the accessors, so "set" stays detectable for validation and for the
// per-entry merge.
const (
	haDefaultDLBAZName          = "dlb"
	haDefaultMotionWaitSec      = 600
	haDefaultReplicationSched   = "*/5"
	haMaxMotionWaitSec          = 86400
	haMaxReplicationScheduleLen = 128
)

// HAChoice is a string-valued pve.ha key. It decodes a JSON string as is and a
// JSON boolean as "on" or "off", because YAML 1.1, which BOSH manifests and
// cpi-config entries are written in, reads an unquoted on or off as a boolean.
// Without this an operator who writes `mode: off` would see the Director hand
// the CPI a boolean and the key would fail to decode.
type HAChoice string

// UnmarshalJSON implements json.Unmarshaler.
func (c *HAChoice) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		*c = HAChoice(s)
		return nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		if b {
			*c = HAChoice(HARuleOn)
		} else {
			*c = HAChoice(HAModeOff)
		}
		return nil
	}
	return fmt.Errorf("expected a string, got %s", strings.TrimSpace(string(raw)))
}

// HAConfig is the pve.ha block: the single place the CPI is told whether to
// write PVE HA state and which HA features it may use. Every field is a
// pointer so that nil means "not set", which validation and the per-entry
// merge both depend on. The accessors on CPIConfig supply the defaults. A nil
// *HAConfig is the same as an empty block, and both mean mode off.
type HAConfig struct {
	// Mode is off (default), auto, or managed. Off makes no HA registrations,
	// auto probes the cluster and degrades when a prerequisite is missing, and
	// managed turns a missing prerequisite into an error.
	Mode *HAChoice `json:"mode,omitempty"`

	// StorageTopology is auto (default), shared, local, or mixed. It states
	// which disk scenario applies, overriding what the probe found.
	StorageTopology *HAChoice `json:"storage_topology,omitempty"`

	// LocalPolicy is none (default), hold, or replicate, and decides what
	// happens to a VM that has node-local volumes.
	LocalPolicy *HAChoice `json:"local_policy,omitempty"`

	// AntiAffinity is off, auto (default), or on. Under on, a group that
	// cannot get its rule is a warning in auto mode and an error in managed
	// mode.
	AntiAffinity *HAChoice `json:"anti_affinity,omitempty"`

	// AntiAffinityOverflow is skip (default) or split and decides what happens
	// when a group has more members than the cluster has nodes.
	AntiAffinityOverflow *HAChoice `json:"anti_affinity_overflow,omitempty"`

	// PinAZ is off, auto (default), or on. Auto folds a VM's AZ node set into
	// its node-affinity pin when the VM is HA-managed and placement.az_map
	// defines its AZ.
	PinAZ *HAChoice `json:"pin_az,omitempty"`

	// PinAZStrict decides whether the AZ part of the pin is strict. Default
	// true.
	PinAZStrict *bool `json:"pin_az_strict,omitempty"`

	// Headroom is off, warn (default), or enforce, and sets the N+1 memory
	// check.
	Headroom *HAChoice `json:"headroom,omitempty"`

	// DLB holds the PVE Dynamic Load Balancer keys.
	DLB *HADLBConfig `json:"dlb,omitempty"`

	// MotionWaitSec is the budget in seconds for a migrate-lock wait, the HA
	// state poll, and the delete grace window. Default 600.
	MotionWaitSec *int `json:"motion_wait_sec,omitempty"`

	// Replication holds the settings of each replication job that the
	// replicate local policy creates.
	Replication *HAReplicationConfig `json:"replication,omitempty"`
}

// HADLBConfig holds the pve.ha.dlb keys.
type HADLBConfig struct {
	// Enabled registers every eligible VM for the PVE Dynamic Load Balancer.
	// Default false.
	Enabled *bool `json:"enabled,omitempty"`

	// AZName is the sentinel AZ name that opts a single VM into DLB. Default
	// "dlb". A non-nil pointer to "" disables the sentinel.
	AZName *string `json:"az_name,omitempty"`
}

// HAReplicationConfig holds the pve.ha.replication keys.
type HAReplicationConfig struct {
	// Schedule is the PVE schedule of each replication job. Default "*/5".
	Schedule *string `json:"schedule,omitempty"`

	// RateMbps limits each replication job, where 0 (default) means no limit.
	RateMbps *int `json:"rate_mbps,omitempty"`
}

// haEnumRule names one enum-valued key and the values it accepts.
type haEnumRule struct {
	key     string
	value   func(*HAConfig) *HAChoice
	allowed []string
}

// haEnumRules lists every enum-valued pve.ha key, in the order validation
// reports them.
var haEnumRules = []haEnumRule{
	{"mode", func(h *HAConfig) *HAChoice { return h.Mode }, []string{HAModeOff, HAModeAuto, HAModeManaged}},
	{"storage_topology", func(h *HAConfig) *HAChoice { return h.StorageTopology },
		[]string{HAStorageTopologyAuto, HAStorageTopologyShared, HAStorageTopologyLocal, HAStorageTopologyMixed}},
	{"local_policy", func(h *HAConfig) *HAChoice { return h.LocalPolicy },
		[]string{HALocalPolicyNone, HALocalPolicyHold, HALocalPolicyReplicate}},
	{"anti_affinity", func(h *HAConfig) *HAChoice { return h.AntiAffinity },
		[]string{HARuleOff, HARuleAuto, HARuleOn}},
	{"anti_affinity_overflow", func(h *HAConfig) *HAChoice { return h.AntiAffinityOverflow },
		[]string{HAOverflowSkip, HAOverflowSplit}},
	{"pin_az", func(h *HAConfig) *HAChoice { return h.PinAZ }, []string{HARuleOff, HARuleAuto, HARuleOn}},
	{"headroom", func(h *HAConfig) *HAChoice { return h.Headroom },
		[]string{HAHeadroomOff, HAHeadroomWarn, HAHeadroomEnforce}},
}

// haChoiceValue returns the value of p, or def when p is nil.
func haChoiceValue(p *HAChoice, def string) string {
	if p == nil {
		return def
	}
	return string(*p)
}

// haBlock returns the pve.ha block, or an empty block when none is set, so
// the accessors read one shape.
func (c *CPIConfig) haBlock() *HAConfig {
	if c == nil || c.HA == nil {
		return &HAConfig{}
	}
	return c.HA
}

// HAModeValue returns the effective pve.ha.mode: off, auto, or managed.
// Defaults to off when the key is unset.
func (c *CPIConfig) HAModeValue() string {
	return haChoiceValue(c.haBlock().Mode, HAModeOff)
}

// HAWritesEnabled reports whether the CPI may write HA state at all, which is
// true in auto and managed mode and false in off mode.
func (c *CPIConfig) HAWritesEnabled() bool {
	return c.HAModeValue() != HAModeOff
}

// HAStorageTopologyValue returns the effective pve.ha.storage_topology.
// Defaults to auto.
func (c *CPIConfig) HAStorageTopologyValue() string {
	return haChoiceValue(c.haBlock().StorageTopology, HAStorageTopologyAuto)
}

// HALocalPolicyValue returns the effective pve.ha.local_policy. Defaults to
// none.
func (c *CPIConfig) HALocalPolicyValue() string {
	return haChoiceValue(c.haBlock().LocalPolicy, HALocalPolicyNone)
}

// HAAntiAffinityValue returns the effective pve.ha.anti_affinity. Defaults to
// auto.
func (c *CPIConfig) HAAntiAffinityValue() string {
	return haChoiceValue(c.haBlock().AntiAffinity, HARuleAuto)
}

// HAAntiAffinityOverflowValue returns the effective
// pve.ha.anti_affinity_overflow. Defaults to skip.
func (c *CPIConfig) HAAntiAffinityOverflowValue() string {
	return haChoiceValue(c.haBlock().AntiAffinityOverflow, HAOverflowSkip)
}

// HAPinAZValue returns the effective pve.ha.pin_az. Defaults to auto.
func (c *CPIConfig) HAPinAZValue() string {
	return haChoiceValue(c.haBlock().PinAZ, HARuleAuto)
}

// HAPinAZStrictValue returns the effective pve.ha.pin_az_strict. Defaults to
// true; only an explicit false returns false.
func (c *CPIConfig) HAPinAZStrictValue() bool {
	p := c.haBlock().PinAZStrict
	return p == nil || *p
}

// HAHeadroomValue returns the effective pve.ha.headroom. Defaults to warn.
func (c *CPIConfig) HAHeadroomValue() string {
	return haChoiceValue(c.haBlock().Headroom, HAHeadroomWarn)
}

// HADLBEnabledValue returns the effective pve.ha.dlb.enabled. Defaults to
// false; only an explicit true returns true.
func (c *CPIConfig) HADLBEnabledValue() bool {
	dlb := c.haBlock().DLB
	return dlb != nil && dlb.Enabled != nil && *dlb.Enabled
}

// HADLBAZNameValue returns the effective pve.ha.dlb.az_name. Defaults to
// "dlb"; an explicitly empty value returns "" and disables the sentinel.
func (c *CPIConfig) HADLBAZNameValue() string {
	dlb := c.haBlock().DLB
	if dlb == nil || dlb.AZName == nil {
		return haDefaultDLBAZName
	}
	return *dlb.AZName
}

// HAMotionWaitSecValue returns the effective pve.ha.motion_wait_sec in
// seconds. Defaults to 600.
func (c *CPIConfig) HAMotionWaitSecValue() int {
	p := c.haBlock().MotionWaitSec
	if p == nil {
		return haDefaultMotionWaitSec
	}
	return *p
}

// HAReplicationScheduleValue returns the effective pve.ha.replication.schedule.
// Defaults to "*/5".
func (c *CPIConfig) HAReplicationScheduleValue() string {
	r := c.haBlock().Replication
	if r == nil || r.Schedule == nil {
		return haDefaultReplicationSched
	}
	return *r.Schedule
}

// HAReplicationRateMbpsValue returns the effective pve.ha.replication.rate_mbps.
// Defaults to 0, which means no limit.
func (c *CPIConfig) HAReplicationRateMbpsValue() int {
	r := c.haBlock().Replication
	if r == nil || r.RateMbps == nil {
		return 0
	}
	return *r.RateMbps
}

// validateHA validates the optional pve.ha block. Nothing is checked for a key
// that is not set, so an absent block and an absent key behave as the defaults.
//
// Three combinations are rejected because each one would otherwise switch a
// feature off without a word. anti_affinity on, pin_az on, and dlb.enabled
// true all need an effective mode of auto or managed, and the error names
// pve.ha.mode. local_policy replicate is rejected as not yet available, so a
// manifest cannot ask for protection this build does not provide.
func (c *CPIConfig) validateHA(errs *[]string) {
	h := c.HA
	if h == nil {
		return
	}
	for _, rule := range haEnumRules {
		p := rule.value(h)
		if p == nil {
			continue
		}
		if !slices.Contains(rule.allowed, string(*p)) {
			*errs = append(*errs, fmt.Sprintf("ha.%s must be one of %s, got %q",
				rule.key, strings.Join(rule.allowed, ", "), string(*p)))
		}
	}
	c.validateHAUnavailablePolicy(errs)
	c.validateHAModeCombinations(errs)
	c.validateHAPinAZ(errs)
	c.validateHAMotionWait(errs)
	c.validateHAReplication(errs)
}

// validateHAUnavailablePolicy rejects the local policy this build does not
// implement, which is replicate.
func (c *CPIConfig) validateHAUnavailablePolicy(errs *[]string) {
	if c.HA.LocalPolicy != nil && string(*c.HA.LocalPolicy) == HALocalPolicyReplicate {
		*errs = append(*errs, fmt.Sprintf("ha.local_policy %q is not available in this build; use %q or %q",
			HALocalPolicyReplicate, HALocalPolicyNone, HALocalPolicyHold))
	}
}

// validateHAModeCombinations rejects the keys that need HA writes while the
// effective mode is off. An invalid mode is reported by the enum check, so
// this stays quiet about it.
func (c *CPIConfig) validateHAModeCombinations(errs *[]string) {
	h := c.HA
	if h.Mode != nil && !slices.Contains([]string{HAModeOff, HAModeAuto, HAModeManaged}, string(*h.Mode)) {
		return
	}
	if c.HAModeValue() != HAModeOff {
		return
	}
	if h.AntiAffinity != nil && string(*h.AntiAffinity) == HARuleOn {
		*errs = append(*errs, "ha.anti_affinity \"on\" requires pve.ha.mode to be \"auto\" or \"managed\"")
	}
	if h.PinAZ != nil && string(*h.PinAZ) == HARuleOn {
		*errs = append(*errs, "ha.pin_az \"on\" requires pve.ha.mode to be \"auto\" or \"managed\"")
	}
	if h.DLB != nil && h.DLB.Enabled != nil && *h.DLB.Enabled {
		*errs = append(*errs, "ha.dlb.enabled requires pve.ha.mode to be \"auto\" or \"managed\"")
	}
}

// validateHAPinAZ enforces the cross-field rules of pin_az "on". It needs a
// non-empty placement.az_map to pin against, and it is incompatible with the
// DLB sentinel AZ in that map, because DLB intentionally un-pins guests and a
// durable pin would fight the rebalancer.
func (c *CPIConfig) validateHAPinAZ(errs *[]string) {
	if c.HA.PinAZ == nil || string(*c.HA.PinAZ) != HARuleOn {
		return
	}
	if c.Placement == nil || len(c.Placement.AZMap) == 0 {
		*errs = append(*errs, "ha.pin_az \"on\" requires a non-empty placement.az_map to pin against")
		return
	}
	sentinel := c.DLBAZName()
	if sentinel == "" {
		return
	}
	if _, ok := c.Placement.AZMap[sentinel]; ok {
		*errs = append(*errs, fmt.Sprintf(
			"ha.pin_az \"on\" is incompatible with the DLB sentinel AZ %q in placement.az_map; "+
				"DLB intentionally un-pins guests", sentinel))
	}
}

// validateHAMotionWait checks that motion_wait_sec, when set, is a positive
// budget that stays well inside the range of a time.Duration.
func (c *CPIConfig) validateHAMotionWait(errs *[]string) {
	p := c.HA.MotionWaitSec
	if p == nil {
		return
	}
	if *p < 1 || *p > haMaxMotionWaitSec {
		*errs = append(*errs, fmt.Sprintf("ha.motion_wait_sec must be between 1 and %d, got %d",
			haMaxMotionWaitSec, *p))
	}
}

// validateHAReplication checks the replication keys that are set. PVE parses
// the schedule itself, so only an empty, oversized, or control-character
// value, none of which PVE could accept, is rejected here.
func (c *CPIConfig) validateHAReplication(errs *[]string) {
	r := c.HA.Replication
	if r == nil {
		return
	}
	if r.Schedule != nil {
		s := *r.Schedule
		switch {
		case strings.TrimSpace(s) == "":
			*errs = append(*errs, "ha.replication.schedule must not be empty")
		case len(s) > haMaxReplicationScheduleLen:
			*errs = append(*errs, fmt.Sprintf("ha.replication.schedule must be at most %d characters",
				haMaxReplicationScheduleLen))
		case strings.IndexFunc(s, unicode.IsControl) >= 0:
			*errs = append(*errs, "ha.replication.schedule must not contain control characters")
		}
	}
	if r.RateMbps != nil && *r.RateMbps < 0 {
		*errs = append(*errs, fmt.Sprintf("ha.replication.rate_mbps must be 0 or greater, got %d", *r.RateMbps))
	}
}

// merged returns a new block holding h with every key that over sets replacing
// the same key from h, one key at a time, including the keys of the dlb and
// replication sub-blocks. Neither input is modified and the result shares no
// pointer with either, so writing through it cannot reach the job-level
// config that other requests are reading. It returns nil when both are nil.
func (h *HAConfig) merged(over *HAConfig) *HAConfig {
	if h == nil && over == nil {
		return nil
	}
	if h == nil {
		h = &HAConfig{}
	}
	if over == nil {
		over = &HAConfig{}
	}
	out := &HAConfig{
		Mode:                 haPick(over.Mode, h.Mode),
		StorageTopology:      haPick(over.StorageTopology, h.StorageTopology),
		LocalPolicy:          haPick(over.LocalPolicy, h.LocalPolicy),
		AntiAffinity:         haPick(over.AntiAffinity, h.AntiAffinity),
		AntiAffinityOverflow: haPick(over.AntiAffinityOverflow, h.AntiAffinityOverflow),
		PinAZ:                haPick(over.PinAZ, h.PinAZ),
		PinAZStrict:          haPick(over.PinAZStrict, h.PinAZStrict),
		Headroom:             haPick(over.Headroom, h.Headroom),
		MotionWaitSec:        haPick(over.MotionWaitSec, h.MotionWaitSec),
	}
	if h.DLB != nil || over.DLB != nil {
		base, top := h.DLB, over.DLB
		if base == nil {
			base = &HADLBConfig{}
		}
		if top == nil {
			top = &HADLBConfig{}
		}
		out.DLB = &HADLBConfig{
			Enabled: haPick(top.Enabled, base.Enabled),
			AZName:  haPick(top.AZName, base.AZName),
		}
	}
	if h.Replication != nil || over.Replication != nil {
		base, top := h.Replication, over.Replication
		if base == nil {
			base = &HAReplicationConfig{}
		}
		if top == nil {
			top = &HAReplicationConfig{}
		}
		out.Replication = &HAReplicationConfig{
			Schedule: haPick(top.Schedule, base.Schedule),
			RateMbps: haPick(top.RateMbps, base.RateMbps),
		}
	}
	return out
}

// haPick returns a copy of over when it is set and a copy of base otherwise.
func haPick[T any](over, base *T) *T {
	if over != nil {
		return clonePlacementPointer(over)
	}
	return clonePlacementPointer(base)
}

// coerceOverrideHA decodes the pve_ha value of a cpi-config entry into an
// HAConfig holding only the keys the entry names. The caller merges it over
// the job-level block one key at a time, so an entry can change the mode and
// inherit everything else.
//
// Decoding goes through JSON with DisallowUnknownFields, at every level, so a
// misspelled key is rejected rather than ignored, because an ignored
// misspelling would leave a feature at the default the operator meant to
// change. A value that is not an object is rejected for the same reason. An
// explicit null names no key and inherits the whole job-level block.
func coerceOverrideHA(v any) (*HAConfig, error) {
	if v == nil {
		return nil, nil
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected an ha object, got %T", v)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("cannot re-encode ha block: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.DisallowUnknownFields()
	var h HAConfig
	if err := dec.Decode(&h); err != nil {
		return nil, fmt.Errorf("cannot decode ha block: %w", err)
	}
	return &h, nil
}
