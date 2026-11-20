package harebalance

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
)

const (
	// eps is the tolerance of every comparison of loads and cuts, so the
	// rounding of a float never decides a move.
	eps = 1e-9
	// minCut is the least a memory evening move must cut the sum of squared
	// loads by, two squared points, so a move that buys almost nothing is
	// never worth a live migration.
	minCut = 0.0002
	// maxSpreadMoves bounds memory evening. Every move cuts the sum of squared
	// loads by at least minCut, so it ends anyway, and the bound only keeps a
	// pathological model from planning for long.
	maxSpreadMoves = 1000
)

// Build plans the moves that bring the model into a good spread. It never
// changes m. The plan is the same for the same model whatever the order of its
// slices.
func Build(m Model, o Options) Plan {
	s := newState(m)
	p := Plan{Before: s.loads()}
	p.SpreadBefore = s.spread()

	s.separate()
	s.restore()
	if !o.RestoreOnly {
		p.Spreading = s.even(o)
	}

	p.Skipped = s.skips()
	p.After = s.loads()
	p.SpreadAfter = s.spread()
	p.Moves = s.moves
	if o.MaxMoves > 0 && len(p.Moves) > o.MaxMoves {
		p.Deferred = len(p.Moves) - o.MaxMoves
		p.Moves = p.Moves[:o.MaxMoves]
	}
	return p
}

// state is the model as the plan changes it, one move at a time.
type state struct {
	nodes    []Node
	node     map[string]Node
	eligible map[string]bool
	headroom string

	guests []Guest
	at     map[int]string
	mem    map[int]int64

	vms      []VM
	vm       map[int]*VM
	skip     map[int]Skip
	partners map[int]map[int]string
	groups   map[int][]string
	rules    []Rule

	moves []Move
}

func newState(m Model) *state {
	s := &state{
		node:     map[string]Node{},
		eligible: map[string]bool{},
		headroom: m.HeadroomMode,
		at:       map[int]string{},
		mem:      map[int]int64{},
		vm:       map[int]*VM{},
		skip:     map[int]Skip{},
		partners: map[int]map[int]string{},
		groups:   map[int][]string{},
	}
	for _, n := range m.Nodes {
		if _, dup := s.node[n.Name]; dup || n.Name == "" {
			continue
		}
		s.node[n.Name] = n
		s.nodes = append(s.nodes, n)
		if n.Online && !n.Maintenance && n.TotalMemBytes > 0 {
			s.eligible[n.Name] = true
		}
	}
	sort.Slice(s.nodes, func(i, j int) bool { return s.nodes[i].Name < s.nodes[j].Name })

	for _, g := range m.Guests {
		if _, dup := s.at[g.VMID]; dup {
			continue
		}
		g.Admissible = slices.Clone(g.Admissible)
		s.guests = append(s.guests, g)
		s.at[g.VMID], s.mem[g.VMID] = g.Node, g.MemBytes
	}
	for i := range m.VMs {
		v := m.VMs[i]
		if _, dup := s.vm[v.VMID]; dup {
			continue
		}
		if _, known := s.at[v.VMID]; !known {
			s.guests = append(s.guests, Guest{VMID: v.VMID, Node: v.Node, MemBytes: v.MemBytes, HA: true,
				Admissible: slices.Clone(v.Hard)})
			s.at[v.VMID], s.mem[v.VMID] = v.Node, v.MemBytes
		}
		s.vms = append(s.vms, v)
		s.vm[v.VMID] = nil
	}
	sort.Slice(s.guests, func(i, j int) bool { return s.guests[i].VMID < s.guests[j].VMID })
	sort.Slice(s.vms, func(i, j int) bool { return s.vms[i].VMID < s.vms[j].VMID })
	for i := range s.vms {
		v := &s.vms[i]
		s.vm[v.VMID] = v
		if v.Skip != "" {
			s.skip[v.VMID] = Skip{VMID: v.VMID, Name: v.Name, Node: s.at[v.VMID], Kind: v.Skip,
				Reason: v.SkipReason}
		}
	}

	s.indexRules(m.Rules)
	return s
}

// indexRules keeps the rules in ID order, and the partners and groups of each
// member.
func (s *state) indexRules(rules []Rule) {
	s.rules = make([]Rule, 0, len(rules))
	for _, r := range rules {
		r.Members = slices.Clone(r.Members)
		slices.Sort(r.Members)
		r.Members = slices.Compact(r.Members)
		if r.Group == "" {
			r.Group = r.ID
		}
		s.rules = append(s.rules, r)
	}
	sort.Slice(s.rules, func(i, j int) bool { return s.rules[i].ID < s.rules[j].ID })
	for _, r := range s.rules {
		for _, a := range r.Members {
			if !slices.Contains(s.groups[a], r.Group) {
				s.groups[a] = append(s.groups[a], r.Group)
			}
			for _, b := range r.Members {
				if a == b {
					continue
				}
				if s.partners[a] == nil {
					s.partners[a] = map[int]string{}
				}
				if _, seen := s.partners[a][b]; !seen {
					s.partners[a][b] = r.ID
				}
			}
		}
	}
}

// committed returns the memory the guests on node reserve, and how many guests
// it holds.
func (s *state) committed(node string) (int64, int) {
	var c int64
	var n int
	for _, g := range s.guests {
		if s.at[g.VMID] == node {
			c += s.mem[g.VMID]
			n++
		}
	}
	return c, n
}

// load returns node's committed memory as a fraction of its memory.
func (s *state) load(node string) float64 {
	total := s.node[node].TotalMemBytes
	if total <= 0 {
		return 0
	}
	c, _ := s.committed(node)
	return float64(c) / float64(total)
}

// loads returns the load of every node that can take work.
func (s *state) loads() map[string]NodeLoad {
	out := map[string]NodeLoad{}
	for _, n := range s.nodes {
		if !s.eligible[n.Name] {
			continue
		}
		_, count := s.committed(n.Name)
		out[n.Name] = NodeLoad{Committed: s.load(n.Name), Guests: count}
	}
	return out
}

// spread returns the highest load minus the lowest among the nodes that can
// take work, and 0 when fewer than two can.
func (s *state) spread() float64 {
	lo, hi, n := math.Inf(1), math.Inf(-1), 0
	for _, node := range s.nodes {
		if !s.eligible[node.Name] {
			continue
		}
		l := s.load(node.Name)
		lo, hi, n = math.Min(lo, l), math.Max(hi, l), n+1
	}
	if n < 2 {
		return 0
	}
	return hi - lo
}

// movable reports whether the plan may move v: it is not skipped and sits on a
// node that can take work, so PVE HA is not moving it already.
func (s *state) movable(v *VM) bool {
	_, skipped := s.skip[v.VMID]
	return !skipped && s.eligible[s.at[v.VMID]]
}

// allowed returns the nodes v may run on that can take work, inside its
// targets and its AZ, in name order.
func (s *state) allowed(v *VM) []string {
	var out []string
	for _, n := range v.Targets {
		if !s.eligible[n] || (len(v.AZ) > 0 && !slices.Contains(v.AZ, n)) || slices.Contains(out, n) {
			continue
		}
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// facts returns the placement facts of every node as the plan stands, for v.
func (s *state) facts(v *VM) []placement.NodeFacts {
	out := make([]placement.NodeFacts, 0, len(s.nodes))
	for _, n := range s.nodes {
		c, count := s.committed(n.Name)
		same := 0
		for _, g := range s.guests {
			if g.VMID != v.VMID && s.at[g.VMID] == n.Name && s.sharesGroup(v.VMID, g.VMID) {
				same++
			}
		}
		out = append(out, placement.NodeFacts{
			Node:              n.Name,
			Online:            n.Online,
			InMaintenance:     n.Maintenance || n.TotalMemBytes <= 0,
			TotalMemBytes:     n.TotalMemBytes,
			CommittedMemBytes: c,
			FreeMemBytes:      n.TotalMemBytes - c,
			GuestCount:        count,
			SameGroupCount:    same,
		})
	}
	return out
}

func (s *state) sharesGroup(a, b int) bool {
	for _, g := range s.groups[a] {
		if slices.Contains(s.groups[b], g) {
			return true
		}
	}
	return false
}

// headroomCheck returns the headroom check for v as the plan stands, or nil
// when the mode does not refuse a node.
func (s *state) headroomCheck(v *VM) *placement.HeadroomCheck {
	switch s.headroom {
	case "", placement.HeadroomOff, placement.HeadroomWarn:
		return nil
	}
	guests := make([]placement.HeadroomGuest, 0, len(s.guests))
	for _, g := range s.guests {
		guests = append(guests, placement.HeadroomGuest{VMID: g.VMID, Node: s.at[g.VMID], MemBytes: s.mem[g.VMID],
			HA: g.HA, Admissible: g.Admissible})
	}
	return &placement.HeadroomCheck{Mode: s.headroom, Guests: guests,
		VM: placement.HeadroomVM{VMID: v.VMID, MemBytes: s.mem[v.VMID], Admissible: v.Hard}}
}

// targets returns the nodes of cands that can take v now, in name order, and
// why each other node of cands cannot. A node is out when it is the node v is
// on, when the pin the CPI would write there changes more than priorities,
// when it holds another member of one of v's anti-affinity rules, or when
// placement.Filter rejects it for memory or headroom.
func (s *state) targets(v *VM, cands []string) (pass []string, refused, reasons map[string]string) {
	refused, reasons = map[string]string{}, map[string]string{}
	from := s.at[v.VMID]
	var left []string
	for _, n := range cands {
		if n == from {
			continue
		}
		if why, ok := v.PinRefused[n]; ok {
			refused[n], reasons[n] = why, why
			continue
		}
		if partner, rule := s.partnerOn(v.VMID, n); partner != 0 {
			reasons[n] = fmt.Sprintf("it holds VM %d of the anti-affinity rule %s", partner, rule)
			continue
		}
		left = append(left, n)
	}
	if len(left) == 0 {
		return nil, refused, reasons
	}
	facts := s.facts(v)
	ok, rejected := placement.Filter(facts, placement.Request{RequiredMemBytes: s.mem[v.VMID], CandidateNodes: left,
		Headroom: s.headroomCheck(v)})
	for i := range ok {
		pass = append(pass, ok[i].Node)
	}
	for _, n := range left {
		if why, out := rejected[n]; out {
			reasons[n] = why
		}
	}
	slices.Sort(pass)
	return pass, refused, reasons
}

// partnerOn returns the lowest VMID that shares an anti-affinity rule with
// vmid and sits on node, with the rule, or 0.
func (s *state) partnerOn(vmid int, node string) (int, string) {
	best, rule := 0, ""
	for other, r := range s.partners[vmid] {
		if s.at[other] == node && (best == 0 || other < best) {
			best, rule = other, r
		}
	}
	return best, rule
}

// score returns placement.Score's score of node for v as the plan stands.
func (s *state) score(v *VM, node string) float64 {
	w := placement.DefaultWeights()
	w.MemorySignal = placement.MemorySignalReserved
	facts := s.facts(v)
	for i := range facts {
		if facts[i].Node == node {
			return placement.Score(facts[i:i+1], w, nil)[0].Score
		}
	}
	return 0
}

// lowest returns the node of pass with the lowest load, breaking a tie by the
// higher score and then by name.
func (s *state) lowest(v *VM, pass []string) string {
	best := ""
	for _, n := range pass {
		if best == "" || s.better(v, n, best) {
			best = n
		}
	}
	return best
}

func (s *state) better(v *VM, n, than string) bool {
	ln, lt := s.load(n), s.load(than)
	if math.Abs(ln-lt) > eps {
		return ln < lt
	}
	sn, st := s.score(v, n), s.score(v, than)
	if math.Abs(sn-st) > eps {
		return sn > st
	}
	return n < than
}

// move records the move of v to node.
func (s *state) move(v *VM, to string, phase Phase, reason string) {
	mv := Move{VMID: v.VMID, Name: v.Name, From: s.at[v.VMID], To: to, Phase: phase, Reason: reason,
		PinRewritten: v.Rewrite[to]}
	if mv.PinRewritten {
		mv.Reason += fmt.Sprintf("; its pin is rewritten to prefer %s", to)
	}
	s.moves = append(s.moves, mv)
	s.at[v.VMID] = to
}

// byMemory orders vmids by memory, the largest first, then by VMID.
func (s *state) byMemory(vmids []int) {
	sort.Slice(vmids, func(i, j int) bool {
		if s.mem[vmids[i]] != s.mem[vmids[j]] {
			return s.mem[vmids[i]] > s.mem[vmids[j]]
		}
		return vmids[i] < vmids[j]
	})
}

// separate moves members of one anti-affinity rule off a node they share, and
// then holds every anti-affinity group to at most ceil(members / nodes) on a
// node.
func (s *state) separate() {
	for _, r := range s.rules {
		on := map[string][]int{}
		for _, id := range r.Members {
			if node, ok := s.at[id]; ok {
				on[node] = append(on[node], id)
			}
		}
		for _, node := range sortedKeys(on) {
			members := on[node]
			s.byMemory(members)
			left := len(members)
			for _, id := range members {
				if left < 2 {
					break
				}
				v := s.vm[id]
				if v == nil || !s.movable(v) {
					continue
				}
				pass, _, _ := s.targets(v, s.allowed(v))
				if to := s.lowest(v, pass); to != "" {
					partner, _ := s.partnerOn(id, node)
					s.move(v, to, PhaseSeparate, fmt.Sprintf("VM %d shares %s with VM %d of the anti-affinity rule %s",
						id, node, partner, r.ID))
					left--
				}
			}
		}
	}
	for _, group := range s.groupNames() {
		s.balanceGroup(group)
	}
}

func (s *state) groupNames() []string {
	var out []string
	for _, r := range s.rules {
		if !slices.Contains(out, r.Group) {
			out = append(out, r.Group)
		}
	}
	slices.Sort(out)
	return out
}

// balanceGroup moves members of group off a node that holds more than its
// share, ceil(members / nodes), onto a node that holds less.
func (s *state) balanceGroup(group string) {
	var members []int
	for _, r := range s.rules {
		if r.Group != group {
			continue
		}
		for _, id := range r.Members {
			if _, ok := s.at[id]; ok && !slices.Contains(members, id) {
				members = append(members, id)
			}
		}
	}
	nodes := map[string]bool{}
	for _, id := range members {
		if v := s.vm[id]; v != nil {
			for _, n := range s.allowed(v) {
				nodes[n] = true
			}
		}
	}
	if len(members) == 0 || len(nodes) == 0 {
		return
	}
	ceil := (len(members) + len(nodes) - 1) / len(nodes)
	s.byMemory(members)
	for range len(members) * len(s.nodes) {
		count := map[string]int{}
		for _, id := range members {
			count[s.at[id]]++
		}
		if !s.balanceOnce(group, members, count, ceil) {
			return
		}
	}
}

func (s *state) balanceOnce(group string, members []int, count map[string]int, ceil int) bool {
	for _, node := range sortedKeys(count) {
		if count[node] <= ceil {
			continue
		}
		for _, id := range members {
			v := s.vm[id]
			if s.at[id] != node || v == nil || !s.movable(v) {
				continue
			}
			var under []string
			for _, n := range s.allowed(v) {
				if count[n] < ceil {
					under = append(under, n)
				}
			}
			pass, _, _ := s.targets(v, under)
			if to := s.lowest(v, pass); to != "" {
				s.move(v, to, PhaseSeparate, fmt.Sprintf("%s holds %d members of the anti-affinity group %s, "+
					"above its share of %d", node, count[node], group, ceil))
				return true
			}
		}
	}
	return false
}

// restore returns every VM outside its AZ to the node of its AZ with the
// lowest load, the VMs on the busiest nodes first.
func (s *state) restore() {
	var away []*VM
	for i := range s.vms {
		v := &s.vms[i]
		if len(v.AZ) > 0 && s.movable(v) && !slices.Contains(v.AZ, s.at[v.VMID]) {
			away = append(away, v)
		}
	}
	load := map[int]float64{}
	for _, v := range away {
		load[v.VMID] = s.load(s.at[v.VMID])
	}
	sort.SliceStable(away, func(i, j int) bool {
		a, b := away[i], away[j]
		if math.Abs(load[a.VMID]-load[b.VMID]) > eps {
			return load[a.VMID] > load[b.VMID]
		}
		if s.mem[a.VMID] != s.mem[b.VMID] {
			return s.mem[a.VMID] > s.mem[b.VMID]
		}
		return a.VMID < b.VMID
	})
	for _, v := range away {
		s.restoreOne(v)
	}
}

func (s *state) restoreOne(v *VM) {
	from := s.at[v.VMID]
	az := "its AZ is " + strings.Join(v.AZ, ", ")
	if v.AZName != "" {
		az = fmt.Sprintf("its AZ %s is %s", v.AZName, strings.Join(v.AZ, ", "))
	}
	cands := s.allowed(v)
	pass, refused, reasons := s.targets(v, cands)
	if to := s.lowest(v, pass); to != "" {
		s.move(v, to, PhaseRestore, fmt.Sprintf("VM %d is on %s, and %s", v.VMID, from, az))
		return
	}
	kind := SkipNoTarget
	why := fmt.Sprintf("no node of its AZ can take it now (%s)", az)
	if len(cands) == 0 {
		why = fmt.Sprintf("no node of its AZ can take work now (%s)", az)
	}
	if len(refused) > 0 {
		kind = SkipPinRewrite
		why = fmt.Sprintf("a move into its AZ (%s) needs a pin that changes more than priorities", az)
	}
	var parts []string
	for _, n := range sortedKeys(reasons) {
		parts = append(parts, n+": "+reasons[n])
	}
	if len(parts) > 0 {
		why += ": " + strings.Join(parts, "; ")
	}
	s.skip[v.VMID] = Skip{VMID: v.VMID, Name: v.Name, Node: from, Kind: kind, Reason: why}
}

// even moves VMs between the nodes of their AZ while the spread is wide, the
// move that cuts the sum of squared loads the most first. It reports whether
// memory evening ran.
func (s *state) even(o Options) bool {
	d := s.spread()
	if d <= o.StartThreshold+eps && (!o.Spreading || d <= o.StopThreshold+eps) {
		return false
	}
	for range maxSpreadMoves {
		if s.spread() <= o.StopThreshold+eps {
			break
		}
		best, ok := s.bestSpreadMove()
		if !ok {
			break
		}
		from := s.at[best.vm.VMID]
		s.move(best.vm, best.to, PhaseSpread, fmt.Sprintf("%s holds %.0f%% of its memory and %s holds %.0f%%, "+
			"and the move narrows the spread of %.0f points", from, 100*s.load(from), best.to, 100*s.load(best.to),
			100*s.spread()))
	}
	return true
}

type candidate struct {
	vm    *VM
	to    string
	cut   float64
	score float64
}

func (s *state) bestSpreadMove() (candidate, bool) {
	var best candidate
	found := false
	for i := range s.vms {
		v := &s.vms[i]
		if !s.movable(v) {
			continue
		}
		cands := s.allowed(v)
		if len(cands) < 2 {
			continue
		}
		from := s.at[v.VMID]
		m := s.mem[v.VMID]
		if m <= 0 {
			continue
		}
		srcTotal := float64(s.node[from].TotalMemBytes)
		srcC, _ := s.committed(from)
		ls, ls2 := float64(srcC)/srcTotal, float64(srcC-m)/srcTotal
		var lower []string
		for _, n := range cands {
			if n == from {
				continue
			}
			if s.load(n)+float64(m)/float64(s.node[n].TotalMemBytes) < ls-eps {
				lower = append(lower, n)
			}
		}
		if len(lower) == 0 {
			continue
		}
		pass, _, _ := s.targets(v, lower)
		for _, n := range pass {
			lt := s.load(n)
			lt2 := lt + float64(m)/float64(s.node[n].TotalMemBytes)
			cut := ls*ls + lt*lt - ls2*ls2 - lt2*lt2
			if cut < minCut-eps {
				continue
			}
			c := candidate{vm: v, to: n, cut: cut, score: s.score(v, n)}
			if !found || c.beats(best) {
				best, found = c, true
			}
		}
	}
	return best, found
}

func (c candidate) beats(b candidate) bool {
	if math.Abs(c.cut-b.cut) > eps {
		return c.cut > b.cut
	}
	if math.Abs(c.score-b.score) > eps {
		return c.score > b.score
	}
	if c.to != b.to {
		return c.to < b.to
	}
	return c.vm.VMID < b.vm.VMID
}

// skips returns the VMs the plan leaves where they are, in VMID order.
func (s *state) skips() []Skip {
	var out []Skip
	for i := range s.vms {
		if sk, ok := s.skip[s.vms[i].VMID]; ok {
			out = append(out, sk)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
