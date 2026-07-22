package config

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// contextOverridePrefix marks a context key as a candidate pve.* per-request
// override (see ApplyContextOverrides). Any context key without this prefix
// is never treated as a config override candidate, known or unknown.
const contextOverridePrefix = "pve_"

// contextOverrideFieldOrder lists, in a stable/deterministic order, every
// context key ApplyContextOverrides honors. Only these pve.* properties may
// be overridden per dispatched request: the subset that drives PVE
// connection identity (host/port/auth/tls), node selection, storage-pool
// names, network bridge, VMID allocation range, agent bootstrap mode, and
// disk format — i.e. everything a single request needs to target the
// correct PVE cluster and place its VM/disk/stemcell correctly there once
// it does. Every other pve.* config knob (placement scoring, hooks, otel,
// disk-performance policy, HA/anti-affinity behavior, retry curves, etc.) is
// process-wide operating policy rather than a property of which cluster a
// request targets, and is deliberately NOT in this list — it continues to
// come from the job-level config unconditionally for every request,
// overridden or not.
var contextOverrideFieldOrder = []string{
	"pve_host",
	"pve_port",
	"pve_user",
	"pve_password",
	"pve_api_token",
	"pve_realm",
	"pve_node",
	"pve_vm_storage",
	"pve_disk_storage",
	"pve_stemcell_storage",
	"pve_iso_storage",
	"pve_network_bridge",
	"pve_verify_ssl",
	"pve_vmid_range_start",
	"pve_vmid_range_end",
	"pve_agent_mode",
	"pve_vm_disk_format",
	"pve_agent_mbus",
}

// contextOverrideFields maps each supported context key to the function that
// applies its coerced value onto an effective CPIConfig. Built once at
// package init from contextOverrideFieldOrder; every entry in that slice
// must have a matching entry here (asserted by TestContextOverrideFieldOrder
// in context_overrides_test.go) or ApplyContextOverrides would silently skip it.
var contextOverrideFields = map[string]func(*CPIConfig, any) error{
	"pve_host": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.Host = s
		return nil
	},
	"pve_port": func(c *CPIConfig, v any) error {
		n, err := coerceOverrideInt(v)
		if err != nil {
			return err
		}
		// Value-range/constraint checking (1-65535) is intentionally NOT done
		// here — it is delegated entirely to the eff.Validate() call at the
		// end of ApplyContextOverrides, the same validation the job-level
		// config must pass at CPI startup. A field-local check here previously
		// used a looser bound (0-65535) than Validate() (1-65535), silently
		// accepting pve_port=0; see the H1 finding in the A13 review. One
		// source of truth for value constraints avoids that class of drift.
		c.Port = n
		return nil
	},
	"pve_user": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.User = s
		return nil
	},
	"pve_password": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.Password = s
		return nil
	},
	"pve_api_token": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.APIToken = s
		return nil
	},
	"pve_realm": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.Realm = s
		return nil
	},
	"pve_node": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.Node = s
		return nil
	},
	"pve_vm_storage": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.VMStorage = s
		return nil
	},
	"pve_disk_storage": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.DiskStorage = s
		return nil
	},
	"pve_stemcell_storage": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.StemcellStorage = s
		return nil
	},
	"pve_iso_storage": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.ISOStorage = s
		return nil
	},
	"pve_network_bridge": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.NetworkBridge = s
		return nil
	},
	"pve_verify_ssl": func(c *CPIConfig, v any) error {
		b, err := coerceOverrideBool(v)
		if err != nil {
			return err
		}
		// Always allocate a NEW *bool. base's VerifySSL pointer (if any) is
		// shared by the shallow copy ApplyContextOverrides makes; writing
		// through the inherited pointer instead of replacing it would mutate
		// base's own config out from under every other in-flight/future
		// request that has not opted into this override.
		c.VerifySSL = &b
		return nil
	},
	"pve_vmid_range_start": func(c *CPIConfig, v any) error {
		n, err := coerceOverrideInt(v)
		if err != nil {
			return err
		}
		// Value constraints (>=100, ordering against pve_vmid_range_end, and
		// non-overlap against the disk/template/parker VMID bands) are
		// enforced by the eff.Validate() call at the end of
		// ApplyContextOverrides, not here — see the pve_port entry's comment
		// for why a single source of truth matters, and the H1 finding in the
		// A13 review for the specific single-bound-override inversion this
		// closes (this field-local check previously only rejected n<=0, and
		// the caller only cross-checked ordering when BOTH bounds were
		// overridden together, so overriding just this field above the
		// inherited end silently produced an empty/inverted range).
		c.VMIDRangeStart = n
		return nil
	},
	"pve_vmid_range_end": func(c *CPIConfig, v any) error {
		n, err := coerceOverrideInt(v)
		if err != nil {
			return err
		}
		// See pve_vmid_range_start above: value constraints are enforced by
		// eff.Validate(), not here.
		c.VMIDRangeEnd = n
		return nil
	},
	"pve_agent_mode": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.AgentMode = s
		return nil
	},
	"pve_vm_disk_format": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.VMDiskFormat = s
		return nil
	},
	"pve_agent_mbus": func(c *CPIConfig, v any) error {
		s, err := coerceOverrideString(v)
		if err != nil {
			return err
		}
		c.AgentMBus = s
		return nil
	},
}

// coerceOverrideString requires v to already be a JSON string. Context
// property values arrive as whatever the BOSH director's manifest author
// wrote in cpi-config properties; a non-string value for a string-typed
// pve.* field (e.g. a number or object where a hostname is expected) is
// always an operator/manifest error, so this never attempts a lossy
// stringification — it fails loudly instead.
func coerceOverrideString(v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("expected string, got %T", v)
	}
	return s, nil
}

// coerceOverrideInt accepts a JSON number (decoded as float64 by
// encoding/json into `any`) or a numeric string ("8006"), matching the same
// tolerance the BOSH director's own property rendering can produce depending
// on manifest YAML typing. A non-integer float (e.g. 8006.5) is rejected
// rather than truncated, since a fractional PVE port/VMID bound can only be
// a manifest mistake. int/int64 are accepted too so tests and any future
// programmatic caller need not round-trip through JSON.
func coerceOverrideInt(v any) (int, error) {
	switch t := v.(type) {
	case float64:
		if t != math.Trunc(t) {
			return 0, fmt.Errorf("expected integer, got non-integer number %v", t)
		}
		return int(t), nil
	case int:
		return t, nil
	case int64:
		return int(t), nil
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, fmt.Errorf("expected integer, got empty string")
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("expected integer, got %q: %w", t, err)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("expected integer, got %T", v)
	}
}

// coerceOverrideBool accepts a JSON bool or a boolean-ish string ("true",
// "false", "1", "0", "TRUE", ...) via strconv.ParseBool, matching the same
// dual-typed tolerance coerceOverrideInt applies to numbers — BOSH manifest
// YAML can render a boolean property as either JSON type depending on how
// the operator quoted it.
func coerceOverrideBool(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		if err != nil {
			return false, fmt.Errorf("expected boolean, got %q: %w", t, err)
		}
		return b, nil
	default:
		return false, fmt.Errorf("expected boolean, got %T", v)
	}
}

// ApplyContextOverrides returns a per-request effective CPIConfig built by
// overriding select fields of base with the "pve_"-prefixed keys present in
// extra (the jsonrpc.Context.Extra map decoded from one dispatched JSON-RPC
// request's context object).
//
// This exists to fix a defect: BOSH's cpi-config feature lets a director
// register multiple named CPI entries (e.g. two `type: pve` blocks pointing
// at distinct Proxmox clusters) that are all served by ONE running instance
// of this CPI binary. The director merges each entry's `properties:` hash
// into the JSON-RPC request context per dispatched request, but until this
// function existed nothing read those keys back out — every request ran
// against whichever pve.* config this process happened to be launched with
// (the job-level config), silently executing against the wrong PVE cluster
// for any cpi-config entry other than the one the process booted with.
//
// Only the keys enumerated in contextOverrideFieldOrder are applied — see
// that slice's doc comment for exactly which pve.* properties are in scope
// and why the rest (placement, hooks, otel, HA policy, retry curves, ...)
// are deliberately excluded from per-request override.
//
// base must be non-nil. extra may be nil or empty; that is the ordinary case
// (single-CPI deployments, or a cpi-config entry matching the job-level
// cluster) and returns base completely UNCHANGED — the same pointer, not a
// copy — so a request without any pve_* context keys is byte-identical in
// cost and behavior to every release before this function existed.
//
// Returns:
//   - effective: base when no override key is present; otherwise a shallow
//     copy of base with only the matched fields replaced. Every field NOT
//     named in contextOverrideFieldOrder — including every pointer, slice,
//     and map field other than VerifySSL — is shared with base by the
//     shallow copy: an overridden request inherits ALL other job-level
//     policy (placement, hooks, otel, retry curves, HA settings, ...)
//     unconditionally. VerifySSL is the one pointer field this function may
//     itself write, and it always allocates a fresh *bool rather than
//     writing through base's pointer (see the pve_verify_ssl entry in
//     contextOverrideFields), so base is never mutated by this call.
//   - applied: the sorted list of context keys that were recognized AND
//     successfully applied. Empty (nil) exactly when effective == base.
//   - unknown: the sorted list of "pve_"-prefixed keys present in extra that
//     are not in contextOverrideFieldOrder — i.e. real pve.* properties (or
//     typos) the director forwarded that this function does not support
//     overriding per-request. Returned rather than silently dropped so the
//     caller (handlers.Deps.WithRequestOverrides) can log a Warn naming
//     them; a director's cpi-config properties block commonly carries the
//     FULL pve.* property set for that entry, most of which is intentionally
//     job-level-only, so this is expected in normal operation and must never
//     fail the request.
//   - err: non-nil when a key IN contextOverrideFieldOrder is present with a
//     value that fails type coercion (e.g. pve_port: "not-a-number"), OR when
//     the fully-merged effective config fails the SAME eff.Validate() check
//     the job-level config must pass at CPI startup (see below) — required
//     fields cleared, invalid enum values, port/VMID-range bounds, VMID band
//     overlap, auth entirely absent, etc. This is deliberately hard-fail, not
//     fail-open: silently falling back to the job config on a malformed
//     override would reproduce the exact "request quietly runs against the
//     wrong cluster" defect class this function exists to close, and
//     silently ACCEPTING an invalid effective config (e.g. an inverted VMID
//     range, or a password cleared with no api_token to fall back to) would
//     surface only as a confusing downstream runtime failure — or worse, a
//     data-integrity violation (colliding VMID bands) that never surfaces at
//     all until two VMs collide. On error, effective is nil and must not be
//     used.
//
// Effective-config re-validation: after every matched key is applied, this
// function calls eff.Validate() — the identical validator CPI startup runs
// against the job-level config (internal/config/config.go's
// ValidateWithLogger: required fields, auth presence, enum fields, VMID
// range bounds/ordering/overlap against the disk/template/parker bands, port
// range, and every other startup invariant). eff is always a full copy of
// base, and base already passed this exact check when the process loaded its
// job config, so a validation failure here can only originate from a field
// this call just overrode (individually, or in combination with an inherited
// field it did not touch — e.g. overriding only pve_vmid_range_start above
// an inherited pve_vmid_range_end that was never itself overridden). This
// closes a class of gaps a narrower ad-hoc check (validating vmid range
// order only when BOTH bounds were overridden in the same request) missed
// entirely: single-bound range inversion, VMID band overlap with the
// persistent-disk/stemcell-template/parker bands, vmid_range_start below
// PVE's reserved floor (100), port=0, and an explicit empty-string override
// that clears a required field (host/user/vm_storage/disk_storage/
// network_bridge) or both auth credentials (password AND api_token) at once.
func ApplyContextOverrides(base *CPIConfig, extra map[string]any) (effective *CPIConfig, applied []string, unknown []string, err error) {
	if base == nil {
		return nil, nil, nil, fmt.Errorf("config: ApplyContextOverrides: base must not be nil")
	}
	if len(extra) == 0 {
		return base, nil, nil, nil
	}

	eff := *base // shallow copy — see doc comment above.
	var appliedKeys []string

	for _, key := range contextOverrideFieldOrder {
		raw, present := extra[key]
		if !present {
			continue
		}
		apply := contextOverrideFields[key]
		if apply == nil {
			// Invariant violation: every entry in contextOverrideFieldOrder
			// must have a matching contextOverrideFields entry (asserted by
			// TestContextOverrideFieldOrder). Unreachable in a correctly
			// built binary; guarded defensively rather than panicking on a
			// live request.
			return nil, nil, nil, fmt.Errorf("config: context override %q: no handler registered (internal error)", key)
		}
		if applyErr := apply(&eff, raw); applyErr != nil {
			return nil, nil, nil, fmt.Errorf("config: context override %q: %w", key, applyErr)
		}
		appliedKeys = append(appliedKeys, key)
	}

	var unknownKeys []string
	for k := range extra {
		if !strings.HasPrefix(k, contextOverridePrefix) {
			continue // not a pve.* override candidate at all (e.g. a future non-CPI context key)
		}
		if _, ok := contextOverrideFields[k]; !ok {
			unknownKeys = append(unknownKeys, k)
		}
	}

	sort.Strings(appliedKeys)
	sort.Strings(unknownKeys)

	if len(appliedKeys) == 0 {
		// No supported key matched (extra may still hold unrelated or
		// unknown pve_* keys) — return base unchanged, same byte-identical
		// contract as the empty-extra case above.
		return base, nil, unknownKeys, nil
	}

	// H1 (A13 review): re-run full startup validation against the merged
	// effective config. See the doc comment above for exactly what this
	// closes. A validation failure is returned as a plain error; the caller
	// (handlers.Deps.WithRequestOverrides) wraps it as a non-retriable
	// CloudError, matching every other coercion failure from this function —
	// a manifest/cpi-config authoring bug, never a transient condition.
	if validateErr := eff.Validate(); validateErr != nil {
		return nil, nil, nil, fmt.Errorf("config: effective override config failed validation: %w", validateErr)
	}

	return &eff, appliedKeys, unknownKeys, nil
}
