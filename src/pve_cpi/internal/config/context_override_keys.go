package config

import (
	"fmt"
	"sort"
	"strings"
)

// ContextMisnestHint is the advice appended to every error that comes from a
// cpi-config entry whose properties sit at the wrong level.
const ContextMisnestHint = "cpi_config properties must sit under pve: and agent:"

// specKeys holds the names the context-key checks match against, derived
// once from jobSpecPropertyNames. A cpi-config entry writes a spec name such
// as pve.placement.az_map as a nested map, so an operator's top-level key
// under pve: or agent: is the first segment of a spec name ("placement"). The
// full dotted remainder ("placement.az_map") is accepted too, because
// operators copy names out of the spec. A flat pve_ key joins the segments
// with underscores ("pve_placement_enabled"), so pveFlat holds that spelling.
type specKeys struct {
	pve     map[string]struct{}
	pveFlat map[string]struct{}
	agent   map[string]struct{}
}

var jobSpecKeys = newSpecKeys(jobSpecPropertyNames)

func newSpecKeys(names []string) specKeys {
	s := specKeys{pve: map[string]struct{}{}, pveFlat: map[string]struct{}{}, agent: map[string]struct{}{}}
	for _, name := range names {
		scope, rest, ok := strings.Cut(name, ".")
		if !ok {
			continue
		}
		var set map[string]struct{}
		switch scope {
		case "pve":
			set = s.pve
		case "agent":
			set = s.agent
		default:
			continue
		}
		set[rest] = struct{}{}
		if scope == "pve" {
			s.pveFlat[strings.ReplaceAll(rest, ".", "_")] = struct{}{}
		}
		if first, _, nested := strings.Cut(rest, "."); nested {
			set[first] = struct{}{}
		}
	}
	return s
}

// ContextKeyFindings is what InspectContextKeys reports about a request
// context that did not make the request fail.
type ContextKeyFindings struct {
	// MisnestedSpecKeys are top-level context keys whose name matches a
	// job spec name with the pve. or agent. prefix removed, such as a bare
	// "node". The CPI ignores them, so the caller warns.
	MisnestedSpecKeys []string
	// UnsupportedAgentKeys are agent.<key> names the spec defines and no
	// request override covers, such as "agent.blobstore". The CPI ignores
	// them, so the caller warns.
	UnsupportedAgentKeys []string
	// OtherTopLevelKeys are the remaining top-level keys the CPI does not
	// read. A Director may add any of them, so the caller logs them at
	// debug level and nothing more.
	OtherTopLevelKeys []string
}

// InspectContextKeys checks the top-level shape of a request context's extra
// keys against the job spec's property names. It reads names only, never
// values, so nothing it returns or errors with can leak a credential.
//
// It fails in two cases, and neither can happen on a deployment without
// cpi_config, because a Director never puts credentials or pve:/agent: maps
// in the context:
//
//   - The context has no pve key and no pve_* key, and its top-level keys
//     include host together with user, password, or api_token. That is an
//     entry whose connection properties were written without the pve: level,
//     which would otherwise run against the job-level cluster without a word.
//   - A key under the pve or agent map, or a flat pve_* key, is not a spec
//     name. That is a typo, and silently using the job value in its place
//     targets the wrong settings.
//
// A key the spec defines but no request override covers (pve.hooks, say) is
// not an error, because operators copy whole property blocks into an entry.
func InspectContextKeys(extra map[string]any) (ContextKeyFindings, error) {
	var f ContextKeyFindings
	if len(extra) == 0 {
		return f, nil
	}

	pveMap, _ := extra["pve"].(map[string]any)
	agentMap, _ := extra["agent"].(map[string]any)
	// A pve value that is null or not a map names no cluster, so it must not
	// disarm the misnest rule (a templated variable that rendered empty gives
	// pve: null).
	hasPVEKey := pveMap != nil
	hasFlatKey := false
	for k := range extra {
		if strings.HasPrefix(k, contextOverridePrefix) {
			hasFlatKey = true
			break
		}
	}

	if !hasPVEKey && !hasFlatKey {
		if keys := misnestedCredentialKeys(extra); len(keys) > 0 {
			return f, fmt.Errorf("context carries %s at the top level; %s",
				strings.Join(quoteAll(keys), ", "), ContextMisnestHint)
		}
	}

	var typos []string
	for k := range pveMap {
		if !specKnownPVEKey(k) {
			typos = append(typos, "pve."+k)
		}
	}
	for k := range agentMap {
		if _, ok := jobSpecKeys.agent[k]; !ok {
			typos = append(typos, "agent."+k)
			continue
		}
		if _, overridable := contextOverrideFields[contextOverridePrefix+"agent_"+k]; !overridable {
			f.UnsupportedAgentKeys = append(f.UnsupportedAgentKeys, "agent."+k)
		}
	}
	for k := range extra {
		if !strings.HasPrefix(k, contextOverridePrefix) {
			continue
		}
		name := strings.TrimPrefix(k, contextOverridePrefix)
		if _, flat := jobSpecKeys.pveFlat[name]; !flat && !specKnownPVEKey(name) {
			typos = append(typos, k)
		}
	}
	if len(typos) > 0 {
		sort.Strings(typos)
		return f, fmt.Errorf("context carries %s, which the pve_cpi job spec does not define; check the spelling",
			strings.Join(quoteAll(typos), ", "))
	}

	for k := range extra {
		switch {
		case k == "pve", k == "agent", strings.HasPrefix(k, contextOverridePrefix):
			// Handled above, or by ApplyContextOverrides.
		case topLevelMatchesSpec(k):
			f.MisnestedSpecKeys = append(f.MisnestedSpecKeys, k)
		default:
			f.OtherTopLevelKeys = append(f.OtherTopLevelKeys, k)
		}
	}
	sort.Strings(f.MisnestedSpecKeys)
	sort.Strings(f.UnsupportedAgentKeys)
	sort.Strings(f.OtherTopLevelKeys)
	return f, nil
}

// specKnownPVEKey reports whether key, written under pve: or after a pve_
// prefix, is a request override or a job spec name.
func specKnownPVEKey(key string) bool {
	if _, ok := contextOverrideFields[contextOverridePrefix+key]; ok {
		return true
	}
	_, ok := jobSpecKeys.pve[key]
	return ok
}

// topLevelMatchesSpec reports whether a top-level context key is a job spec
// name with the pve. or agent. prefix removed.
func topLevelMatchesSpec(key string) bool {
	if _, ok := jobSpecKeys.pve[key]; ok {
		return true
	}
	_, ok := jobSpecKeys.agent[key]
	return ok
}

// misnestedCredentialKeys returns the sorted top-level keys that make up the
// endpoint and credential set: host plus at least one of user, password, or
// api_token. It returns nil when the set is not complete.
func misnestedCredentialKeys(extra map[string]any) []string {
	if _, ok := extra["host"]; !ok {
		return nil
	}
	keys := []string{"host"}
	for _, k := range []string{"user", "password", "api_token"} {
		if _, ok := extra[k]; ok {
			keys = append(keys, k)
		}
	}
	if len(keys) == 1 {
		return nil
	}
	sort.Strings(keys)
	return keys
}

func quoteAll(keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = fmt.Sprintf("%q", k)
	}
	return out
}
