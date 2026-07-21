package handlers

import (
	"regexp"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// poolIDCharsetRE matches the PVE poolid charset: letters, digits, '.', '_',
// and '-'. Any other rune (including '/') fails the match.
var poolIDCharsetRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// separatorRunRE collapses one or more consecutive '-' characters produced by
// renderPoolTemplate when an empty token substitution leaves adjacent
// hyphens (e.g. "{prefix}-{director}-..." with both prefix and director "").
var separatorRunRE = regexp.MustCompile(`-{2,}`)

// reservedPoolLockPrefix is the cluster-lock sentinel namespace the CPI uses
// internally (see internal/pve cluster_lock.go). A resolved pool name must
// never collide with it.
const reservedPoolLockPrefix = "bosh-lock-"

// newPoolResolver builds a layeredResolver dedicated to pool-name resolution.
// Its layer stack is [callCP, vmType.CloudProperties] only — the general
// purpose newLayeredResolver's stack additionally includes a disk_type layer
// between call and vm_type, which would let a disk_type profile's
// cloud_properties.pool silently outrank vm_type. Pool assignment is a
// VM-level concept; disk_type profiles have no vote in it (see plan §0/T2).
//
// Errors mirror newLayeredResolver: an unknown vm_type selector, or a
// non-string selector value, returns a non-retriable CloudError. nil callCP
// is treated as an empty map.
func newPoolResolver(callCP map[string]any, cfg *config.CPIConfig) (*layeredResolver, error) {
	if callCP == nil {
		callCP = map[string]any{}
	}

	layers := make([]map[string]any, 0, 2)
	layers = append(layers, callCP)

	vmTypeLayer, err := resolveProfileLayer(callCP, "vm_type", cfg.VMTypes)
	if err != nil {
		return nil, err
	}
	if vmTypeLayer != nil {
		layers = append(layers, vmTypeLayer)
	}

	return &layeredResolver{layers: layers}, nil
}

// resolvePoolName resolves the flat PVE pool name to assign a VM to, applying
// the call > vm_type > vm_pool_template > global precedence pipeline (plan
// §0/D-04). r must be constructed via newPoolResolver (its layer stack must
// exclude disk_type) — passing the general-purpose newLayeredResolver result
// would let a disk_type profile's "pool" key outrank vm_type.
//
// Returns ("", nil) when every layer resolves empty — no pool is assigned,
// byte-identical to pre-feature behavior. Returns a non-retriable CloudError
// when a resolved (non-empty) name fails validateResolvedPoolName; resolution
// stops at the first non-empty candidate and does not fall through to a lower
// layer on validation failure — an operator-set invalid name must surface,
// not be silently overridden by the global default.
func resolvePoolName(cfg *config.CPIConfig, r *layeredResolver, env map[string]any) (string, error) {
	if v, ok := r.String("pool"); ok {
		return validateResolvedPoolName(v)
	}

	if cfg.VMPoolTemplate != "" {
		if rendered := renderPoolTemplate(cfg, env); rendered != "" {
			return validateResolvedPoolName(rendered)
		}
		// Render collapsed to "" (every token empty): fall through to global.
	}

	if cfg.VMPool != "" {
		return validateResolvedPoolName(cfg.VMPool)
	}

	return "", nil
}

// renderPoolTemplate substitutes cfg.VMPoolTemplate's "{prefix}", "{director}",
// "{deployment}", and "{instance_group}" tokens and sanitizes the result:
// repeated '-' runs collapse to a single '-', leading/trailing '-' is
// trimmed. Returns "" when the sanitized result is empty (e.g. every token
// substituted to "" and no literal text remains) — the caller falls through
// to the global VMPool default in that case.
//
// Unknown "{...}" tokens are NOT this function's concern: validateVMPoolTemplate
// (internal/config) rejects them at config-load time, so any template reaching
// here is known to contain only the four supported tokens (or none).
func renderPoolTemplate(cfg *config.CPIConfig, env map[string]any) string {
	job := instanceGroupName(env)
	deployment := extractDeploymentFromEnv(env, job)
	if deployment == "" {
		deployment = cfg.CreateEnvDeployment
	}
	director := extractDirectorFromEnv(env, deployment, job)

	rendered := cfg.VMPoolTemplate
	rendered = strings.ReplaceAll(rendered, "{prefix}", cfg.VMPrefix)
	rendered = strings.ReplaceAll(rendered, "{director}", director)
	rendered = strings.ReplaceAll(rendered, "{deployment}", deployment)
	rendered = strings.ReplaceAll(rendered, "{instance_group}", job)

	rendered = separatorRunRE.ReplaceAllString(rendered, "-")
	rendered = strings.Trim(rendered, "-")
	return rendered
}

// extractDirectorFromEnv returns the BOSH director name embedded in
// env.bosh.group ("<director>-<deployment>-<job>"), or "" when it cannot be
// derived. Mirrors extractDeploymentFromEnv/extractJobNameFromEnv's suffix-
// trim approach: the director is whatever remains of group after trimming
// the "-<deployment>-<job>" suffix. Returns "" when env, deployment, or job
// is empty, or when the trim was a no-op (group did not end with that exact
// suffix — e.g. create-env paths where env.bosh.group is absent entirely).
func extractDirectorFromEnv(env map[string]any, deployment, job string) string {
	if deployment == "" || job == "" {
		return ""
	}
	boshRaw, ok := env["bosh"].(map[string]any)
	if !ok {
		return ""
	}
	group, _ := boshRaw["group"].(string)
	if group == "" {
		return ""
	}
	suffix := "-" + deployment + "-" + job
	director := strings.TrimSuffix(group, suffix)
	if director == group {
		return ""
	}
	return director
}

// validateResolvedPoolName enforces the flat-name + PVE poolid-charset +
// reserved-namespace rules on a resolved (non-empty) pool name. name must
// already be non-empty; callers only invoke this on a winning candidate.
// Returns (name, nil) when valid, or a non-retriable CloudError describing
// exactly which rule failed.
func validateResolvedPoolName(name string) (string, error) {
	if strings.Contains(name, "/") {
		return "", cpierrors.Cloud(
			"resolved pool name %q must be flat (contains '/'): the CPI does not create nested pools",
			name,
		)
	}
	if !poolIDCharsetRE.MatchString(name) {
		return "", cpierrors.Cloud(
			"resolved pool name %q contains characters invalid for a PVE poolid (allowed: letters, digits, '.', '_', '-')",
			name,
		)
	}
	if strings.HasPrefix(name, reservedPoolLockPrefix) {
		return "", cpierrors.Cloud(
			"resolved pool name %q uses the reserved cluster-lock namespace %q",
			name, reservedPoolLockPrefix,
		)
	}
	return name, nil
}
