package handlers

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/aapattern"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// otherEntryRule returns why a plan's writes to a group most likely belong to
// another cpi_config entry on the cluster, or "". It looks past the remainder
// of a rule's number, which foreignNamedRule checks, because an entry with one
// AZ or none takes every number for the whole cluster's domain and so never
// fails that check. Three kinds of evidence mark a group this entry did not
// place. A rule this entry's numbering names that way only for higher
// indexes than its members carry, a rename to the same members that another
// AZ count explains, and members that all lie outside this entry's VM ID band.
// Each one fails safe, so the check and the fix leave the group to the entry
// that placed it, and --cpi-config judges it by that entry.
func otherEntryRule(cfg *config.CPIConfig, set *ha.RuleSet, ident haIdentity, in aapattern.Input,
	out aapattern.Output, rewritten map[string]bool, lapsed []AntiAffinityHold,
) string {
	if len(rewritten) == 0 {
		return ""
	}
	if reason := ruleBelowItsIndexes(set, ident, in, rewritten); reason != "" {
		return reason
	}
	if reason := renameOfAnotherNumbering(set, ident, in, out, rewritten, lapsed); reason != "" {
		return reason
	}
	return outOfBandGroup(cfg, in, rewritten)
}

// otherEntryRuleAdvice ends each reason of otherEntryRule.
const otherEntryRuleAdvice = "check the group under --cpi-config and fix it with that entry's request. A rule " +
	"written before 0.12.0, or before this entry's az_map changed, can also carry such a name, and the group's " +
	"next write by a member's set_vm_metadata or delete_vm renames it"

// blockSpread is the spread a block term numbers its rules by, the way the
// layout computes it.
func blockSpread(in aapattern.Input) int {
	spread := max(in.Spread, 1)
	for _, d := range in.Domains {
		spread = max(spread, d.Ord)
	}
	return spread
}

// liveMembers returns the members of in that are not reserved, by key.
func liveMembers(in aapattern.Input) map[string]aapattern.Member {
	out := make(map[string]aapattern.Member, len(in.Members))
	for _, m := range in.Members {
		if !m.Reserved {
			out[m.Key] = m
		}
	}
	return out
}

// chunkSlot returns the slot of a block rule numbered n among the rules of
// its members' domain, with the domain's key, or false when no member of sids
// is known or the number is not one this entry gives that domain.
func chunkSlot(in aapattern.Input, members map[string]aapattern.Member, sids []string, n int) (int, string, bool) {
	for _, sid := range sids {
		m, ok := members[sid]
		if !ok {
			continue
		}
		spread, pos := blockSpread(in), max(in.Domains[m.Domain].Ord-1, 0)
		if (n-1)%spread != pos {
			return 0, "", false
		}
		return (n - 1 - pos) / spread, m.Domain, true
	}
	return 0, "", false
}

// ruleBelowItsIndexes returns why a rewritten block rule names members whose
// indexes this entry's numbering never gives that rule, or "". The layout
// fills a domain's block rules with the members in index order, at least
// aapattern.MinBlockSize to a rule, and a member's place in that order is
// never above its index, so a rule in slot k only ever gets members of index
// 2k and up. A member that joined with no index can sit lower, and so can a
// layout from before 0.12.0, which filled rules in join order.
func ruleBelowItsIndexes(set *ha.RuleSet, ident haIdentity, in aapattern.Input, rewritten map[string]bool) string {
	if !in.Pattern.HasKind(aapattern.KindBlock) {
		return ""
	}
	members, base := liveMembers(in), ident.ruleName()
	for i := range set.Rules {
		r := &set.Rules[i]
		if !rewritten[r.ID] || r.Type != haRuleType {
			continue
		}
		n, ok := aapattern.ChunkIndex(base, r.ID)
		if !ok {
			continue
		}
		slot, _, ok := chunkSlot(in, members, r.Resources, n)
		if !ok || slot == 0 {
			continue
		}
		floor, low := aapattern.MinBlockSize*slot, -1
		for _, sid := range r.Resources {
			if m, ok := members[sid]; ok && m.Index >= 0 && m.Index < floor && (low < 0 || m.Index < low) {
				low = m.Index
			}
		}
		if low < 0 {
			continue
		}
		return fmt.Sprintf("the plan would delete or rewrite rule %s, which holds index %d, but this entry's "+
			"numbering gives that name only to a rule of indexes %d and up. Another cpi_config entry on this "+
			"cluster whose placement.az_map numbers the group's rules by another count of AZs most likely placed "+
			"the group and keeps the rule, so "+otherEntryRuleAdvice, r.ID, low, floor)
	}
	return ""
}

// chunkPair is a planned block rule whose members are exactly those of an
// existing rule of the group.
type chunkPair struct {
	slot     int
	have     int
	from, to string
	low      int
}

// renameOfAnotherNumbering returns why the plan renames a block rule to one
// with the same members when another count of AZs explains every such rule's
// name, or "". The layout numbers slot k of a domain at position pos k*spread
// + pos + 1, so when some spread and position other than this entry's give
// each existing rule the number it carries, an entry with that spread most
// likely placed the group. A hold that lapsed below a renamed rule's members
// shifts this entry's own slots, so a domain with such a hold is left to the
// plan.
func renameOfAnotherNumbering(set *ha.RuleSet, ident haIdentity, in aapattern.Input, out aapattern.Output,
	rewritten map[string]bool, lapsed []AntiAffinityHold,
) string {
	if !in.Pattern.HasKind(aapattern.KindBlock) {
		return ""
	}
	members, base := liveMembers(in), ident.ruleName()
	existing := map[string]string{}
	for i := range set.Rules {
		r := &set.Rules[i]
		if _, ok := aapattern.ChunkIndex(base, r.ID); ok && r.Type == haRuleType {
			existing[memberSetKey(r.Resources)] = r.ID
		}
	}
	pairs := map[string][]chunkPair{}
	for _, name := range slices.Sorted(maps.Keys(out.Rules)) {
		n, ok := aapattern.ChunkIndex(base, name)
		if !ok {
			continue
		}
		from, ok := existing[memberSetKey(out.Rules[name])]
		if !ok {
			continue
		}
		slot, dom, ok := chunkSlot(in, members, out.Rules[name], n)
		if !ok {
			continue
		}
		have, _ := aapattern.ChunkIndex(base, from)
		low := -1
		for _, sid := range out.Rules[name] {
			if m, ok := members[sid]; ok && m.Index >= 0 && (low < 0 || m.Index < low) {
				low = m.Index
			}
		}
		pairs[dom] = append(pairs[dom], chunkPair{slot: slot, have: have, from: from, to: name, low: low})
	}
	spread := blockSpread(in)
	for _, dom := range slices.Sorted(maps.Keys(pairs)) {
		renamed := slices.IndexFunc(pairs[dom], func(p chunkPair) bool { return p.from != p.to && rewritten[p.from] })
		if renamed < 0 || lapsedBelow(lapsed, pairs[dom][renamed].low) {
			continue
		}
		pos := max(in.Domains[dom].Ord-1, 0)
		if azs, ok := explainingSpread(pairs[dom], spread, pos); ok {
			p := pairs[dom][renamed]
			return fmt.Sprintf("the plan would rename rule %s to %s with the same members, but a numbering of the "+
				"group's rules over %s gives each rule of the group the name it carries. Another cpi_config entry "+
				"on this cluster whose placement.az_map numbers them that way most likely placed the group and "+
				"keeps the rule, so "+otherEntryRuleAdvice, p.from, p.to, azs)
		}
	}
	return ""
}

// explainingSpread returns, in words, the count of AZs of a numbering other
// than spread and pos that gives every pair's existing rule its number.
func explainingSpread(pairs []chunkPair, spread, pos int) (string, bool) {
	top := 0
	for _, p := range pairs {
		top = max(top, p.have)
	}
	for s := 1; s <= top; s++ {
		for q := range s {
			if s == spread && q == pos {
				continue
			}
			if !slices.ContainsFunc(pairs, func(p chunkPair) bool { return p.slot*s+q+1 != p.have }) {
				if s == 1 {
					return "one AZ or none", true
				}
				return strconv.Itoa(s) + " AZs", true
			}
		}
	}
	return "", false
}

// lapsedBelow reports whether a lapsed hold that no member took keeps an index
// below low, which moves every higher index down one place in the layout.
func lapsedBelow(lapsed []AntiAffinityHold, low int) bool {
	return slices.ContainsFunc(lapsed, func(h AntiAffinityHold) bool { return !h.Consumed && h.Index < low })
}

// memberSetKey keys a rule's members independent of their order.
func memberSetKey(sids []string) string {
	return strings.Join(slices.Sorted(slices.Values(sids)), ",")
}

// outOfBandGroup returns why the plan's writes belong to another entry when
// no member of the group has a VM ID in this entry's VM ID band, or "". The
// CPI creates every VM of an entry in that entry's band, so such a group was
// created by another entry.
func outOfBandGroup(cfg *config.CPIConfig, in aapattern.Input, rewritten map[string]bool) string {
	band, seen := pve.VMIDBandsFromConfig(cfg).VM, false
	for _, m := range in.Members {
		if m.Reserved {
			continue
		}
		rest, ok := strings.CutPrefix(m.Key, "vm:")
		vmid, err := strconv.Atoi(rest)
		if !ok || err != nil {
			continue
		}
		if band.Contains(vmid) {
			return ""
		}
		seen = true
	}
	if !seen {
		return ""
	}
	return fmt.Sprintf("the plan would delete or rewrite rule %s, but no member of the group has a VM ID in this "+
		"entry's VM ID band %d-%d. Another cpi_config entry on this cluster most likely created the members and "+
		"keeps the group's rules, so "+otherEntryRuleAdvice,
		slices.Sorted(maps.Keys(rewritten))[0], band.Start, band.End)
}
