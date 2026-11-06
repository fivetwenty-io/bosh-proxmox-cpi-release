package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
)

// haMarkerPrefix opens the ownership marker the CPI writes into the comment of
// every HA rule and HA resource it creates. The version lets a later format
// change be told apart from this one.
const haMarkerPrefix = "bosh-cpi:v1"

// haHashLen is the number of hex characters of the group hash that name an
// anti-affinity rule and its member tag. PVE rule IDs follow the pve-configid
// format, so the name stays short.
const haHashLen = 12

// haUnsetField stands in for a marker field the caller cannot name, such as the
// deployment of a node-affinity pin, so every marker keeps all three fields.
const haUnsetField = "-"

// haIdentity names the BOSH instance group a rule serves and the Director that
// owns it. Owner is the Director UUID, and the CPI edits or deletes only the
// objects whose marker carries its own.
type haIdentity struct {
	Owner      string
	Deployment string
	Group      string
}

// haIdentityFor builds the identity of the instance group in env for the
// requesting Director. The group is sanitized the way the member tag always
// was. The deployment comes from the environment when the Director sent a
// group name that carries it, and is empty otherwise.
func haIdentityFor(deps Deps, env map[string]any) haIdentity {
	return haIdentity{
		Owner:      deps.RequestDirectorUUID,
		Deployment: sanitizeTagValue(extractDeploymentFromEnv(env, extractJobNameFromEnv(env))),
		Group:      sanitizeTagValue(instanceGroupName(env)),
	}
}

// hash returns the first 12 hex characters of a SHA-256 over the Director UUID,
// the deployment name, and the instance group name. The fields are separated by
// a byte that none of them can hold, so ("ab", "c") and ("a", "bc") differ.
func (i haIdentity) hash() string {
	sum := sha256.Sum256([]byte(i.Owner + "\x00" + i.Deployment + "\x00" + i.Group))
	return hex.EncodeToString(sum[:])[:haHashLen]
}

// ruleName is the anti-affinity rule ID, bosh-aa-<hash12>. It is also the
// member tag, so a VM that carries the tag belongs to the group.
func (i haIdentity) ruleName() string {
	return haRuleNamePrefix + i.hash()
}

// marker renders the ownership marker for the comment of a rule or resource.
func (i haIdentity) marker() string {
	return haMarkerPrefix + " owner=" + markerValue(i.Owner) +
		" deployment=" + markerValue(i.Deployment) + " group=" + markerValue(i.Group)
}

// valid reports whether the identity can own an anti-affinity rule. Without a
// Director UUID no rule could be marked as ours, and without a group there is
// nothing to spread.
func (i haIdentity) valid() bool {
	return i.Owner != "" && i.Group != ""
}

// markerValue makes a marker field one token. Whitespace would break the
// space-separated format, and an empty field would hide the key.
func markerValue(s string) string {
	s = strings.Join(strings.Fields(s), "_")
	if s == "" {
		return haUnsetField
	}
	return s
}

// haMarker is a parsed ownership marker.
type haMarker struct {
	Owner      string
	Deployment string
	Group      string
}

// parseHaMarker reads the ownership marker at the start of a comment. It
// reports false when the comment holds no marker, which is how a rule or
// resource from an older CPI looks.
func parseHaMarker(comment string) (haMarker, bool) {
	fields := strings.Fields(comment)
	if len(fields) == 0 || fields[0] != haMarkerPrefix {
		return haMarker{}, false
	}
	var m haMarker
	for _, f := range fields[1:] {
		key, val, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		if val == haUnsetField {
			val = ""
		}
		switch key {
		case "owner":
			m.Owner = val
		case "deployment":
			m.Deployment = val
		case "group":
			m.Group = val
		}
	}
	return m, true
}

// haOwnership says whose an HA rule or resource is, judged by its comment.
type haOwnership int

const (
	// haUnmarked is an object with no marker, written by an older CPI.
	haUnmarked haOwnership = iota
	// haOwnedByUs carries our Director's marker.
	haOwnedByUs
	// haOwnedByOther carries a marker that names another Director, or ours is
	// unknown, so the CPI must leave it alone.
	haOwnedByOther
)

// haOwnershipOf classifies a comment against the requesting Director. An empty
// Director UUID owns nothing, so every marked object is foreign to it.
func haOwnershipOf(comment, owner string) haOwnership {
	m, ok := parseHaMarker(comment)
	if !ok {
		return haUnmarked
	}
	if owner != "" && m.Owner == owner {
		return haOwnedByUs
	}
	return haOwnedByOther
}

// haRuleWriteGate refuses an HA rule write unless the cluster profile reports
// PVE 9 or later on every node. Deps.HAProfile supplies the profile when it is
// wired, and otherwise the gate probes the cluster itself.
func haRuleWriteGate(ctx context.Context, deps Deps) error {
	_, err := haRuleWriteProfile(ctx, deps)
	return err
}

// haRuleWriteProfile resolves the request's HA profile and applies the PVE 9
// rule-write gate to it, returning the profile the rule writer then plans
// against. It runs before the rules lock, so a cluster probe never eats into
// the lock's window.
func haRuleWriteProfile(ctx context.Context, deps Deps) (*ha.Profile, error) {
	profile, err := resolveHAProfile(ctx, deps)
	if err != nil {
		return nil, err
	}
	if err := profile.RuleWriteGate(); err != nil {
		return nil, err
	}
	return profile, nil
}

// probeHAProfile returns a resolver that probes the cluster deps targets for
// its HA profile.
func probeHAProfile(deps Deps) func(ctx context.Context) (*ha.Profile, error) {
	return func(ctx context.Context) (*ha.Profile, error) {
		return ha.Resolve(ctx, ha.DepsFromClient(deps.PVE, deps.Config, deps.Log(ctx)))
	}
}

// withRequestHAProfile returns deps whose HAProfile resolves the profile at
// most once, so every HA rule writer in one request shares one answer. It
// wraps the wired HAProfile, or a cluster probe when none is wired, and keeps
// the first result and error for the rest of the request. The probe reads
// cluster status, every node's version, the rules, and more, so a request that
// writes both an anti-affinity rule and a node-affinity pin pays for it once.
// The resolve itself stays lazy, so a request that writes no rule never
// probes.
func withRequestHAProfile(deps Deps) Deps {
	resolve := deps.HAProfile
	if resolve == nil {
		resolve = probeHAProfile(deps)
	}
	var (
		once    sync.Once
		profile *ha.Profile
		err     error
	)
	deps.HAProfile = func(ctx context.Context) (*ha.Profile, error) {
		once.Do(func() { profile, err = resolve(ctx) })
		return profile, err
	}
	return deps
}
