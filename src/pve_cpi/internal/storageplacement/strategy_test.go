package storageplacement

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strings"
	"testing"
)

func request(name string) RequestSnapshot {
	r := RequestSnapshot{Policy: Policy{Name: name, Version: 1}, Namespace: "ns", AllocationKey: "agent-1", AllocationGroup: "vm_bundle", RequestedBytes: 100, SeedSet: true}
	for i := range r.Seed {
		r.Seed[i] = byte(i)
	}
	return r
}

func fixture() []EligibleCandidate {
	return []EligibleCandidate{
		{StorageID: "a", BackingKey: "nfs://nas/a", DomainKey: "shared", Member: CapacityBudget{TotalBytes: 1000, AvailableBytes: 500, AllocationBytes: 100}, Domain: CapacityBudget{TotalBytes: 3000, AvailableBytes: 1500, ReserveBytes: 100, OutstandingBytes: 50, AllocationBytes: 100}},
		{StorageID: "b", BackingKey: "nfs://nas/b", DomainKey: "shared", Member: CapacityBudget{TotalBytes: 2000, AvailableBytes: 1000, AllocationBytes: 300}, Domain: CapacityBudget{TotalBytes: 3000, AvailableBytes: 1500, ReserveBytes: 100, OutstandingBytes: 50, AllocationBytes: 300}},
		{StorageID: "c", BackingKey: "nfs://nas/c", Member: CapacityBudget{TotalBytes: 5000, AvailableBytes: 3000, AllocationBytes: 500}},
	}
}

func names(ranked []RankedCandidate) []string {
	result := make([]string, len(ranked))
	for i := range ranked {
		result[i] = ranked[i].Candidate.StorageID
	}
	return result
}

func TestProtocolGoldenV1(t *testing.T) {
	t.Parallel()
	encoded, err := EncodeTuple("a", "é", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(encoded); got != "000000016100000002c3a900000000" {
		t.Fatalf("tuple protocol changed: %s", got)
	}
	a, err := EncodeTuple("ab", "c")
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncodeTuple("a", "bc")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Equal(a, b) {
		t.Fatal("ambiguous tuple boundaries")
	}
	if _, err := EncodeTuple(string([]byte{0xff})); err == nil {
		t.Fatal("accepted invalid UTF-8")
	}
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"spread", []string{"a", "b", "c"}},
		{"weighted_free_space", []string{"b", "a", "c"}},
		{"least_utilized", []string{"c", "a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Rank(request(tc.name), fixture())
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(names(got), tc.want) {
				t.Fatalf("ranking: got %v want %v", names(got), tc.want)
			}
		})
	}
	spreadResult, err := Rank(request("spread"), fixture())
	if err != nil {
		t.Fatal(err)
	}
	hashes := []string{"6d7a7840560a84f1fec8aa7c5003763f84b57ca9f9ad834da19f7f5c6e8e128c", "62f7c416b7e87a5735d4d35436042786ddc436cecbc601ba6fdbdc1aa6e3aaa1", "4306d07aa30d6bb87a71954fe2b018bc85b4031b3f0473437c8ed29cc16c714b"}
	for i, c := range spreadResult {
		if hex.EncodeToString(c.Rendezvous[:]) != hashes[i] {
			t.Fatalf("rendezvous protocol changed for %s", c.Candidate.StorageID)
		}
	}
	weightedResult, err := Rank(request("weighted_free_space"), fixture())
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]struct {
		member, domain     uint64
		score, domainScore float64
	}{
		"a": {400, 1050, 0.00436758431235306, 0.00019585958429145528},
		"b": {700, 1050, 0.0011047367053941173, 0.00019585958429145528},
		"c": {2500, 2500, 0.0002755257872865335, 0.00048725200694732057},
	}
	for _, c := range weightedResult {
		want := expected[c.Candidate.StorageID]
		if c.MemberWeight != want.member || c.DomainWeight != want.domain {
			t.Fatalf("candidate-specific clone weights changed: %+v", c)
		}
		if math.Abs(c.Score-want.score) > 1e-17 || math.Abs(c.DomainScore-want.domainScore) > 1e-17 {
			t.Fatalf("exponential race protocol changed: %+v", c)
		}
	}
}

func TestRankingPermutationRepeatabilityAndMembership(t *testing.T) {
	t.Parallel()
	for _, algorithm := range []string{"spread", "weighted_free_space", "least_utilized"} {
		r := request(algorithm)
		input := fixture()
		baseline, err := Rank(r, input)
		if err != nil {
			t.Fatal(err)
		}
		for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
			permuted := []EligibleCandidate{input[order[0]], input[order[1]], input[order[2]]}
			before := slices.Clone(permuted)
			got, err := Rank(r, permuted)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, baseline) {
				t.Fatalf("%s depends on input order", algorithm)
			}
			if !slices.Equal(permuted, before) {
				t.Fatalf("%s mutated caller input", algorithm)
			}
		}
	}
	// Rendezvous membership changes preserve the relative order of survivors.
	r := request("spread")
	input := fixture()
	before, err := Rank(r, input)
	if err != nil {
		t.Fatal(err)
	}
	added := EligibleCandidate{StorageID: "d", BackingKey: "nfs://nas/d", Member: input[0].Member}
	after, err := Rank(r, append(input, added))
	if err != nil {
		t.Fatal(err)
	}
	var survivors []RankedCandidate
	for _, c := range after {
		if c.Candidate.StorageID != "d" {
			survivors = append(survivors, c)
		}
	}
	if !slices.Equal(before, survivors) {
		t.Fatal("adding a backing remapped existing rendezvous order")
	}
	removed, err := Rank(r, input[1:])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(removed), []string{"b", "c"}) {
		t.Fatal("removing a backing remapped survivors")
	}
}

func TestCapacityAdmissionBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		budget         CapacityBudget
		residual, used uint64
		valid          bool
	}{
		{"exact fit", CapacityBudget{TotalBytes: 100, AvailableBytes: 20, AllocationBytes: 20}, 0, 100, true},
		{"reserve", CapacityBudget{TotalBytes: 100, AvailableBytes: 30, ReserveBytes: 10, AllocationBytes: 20}, 0, 90, true},
		{"ceiling", CapacityBudget{TotalBytes: 100, AvailableBytes: 50, MaxUtilizationPct: 70, OutstandingBytes: 5, AllocationBytes: 15}, 0, 70, true},
		{"round ceiling down", CapacityBudget{TotalBytes: 101, AvailableBytes: 101, MaxUtilizationPct: 1, AllocationBytes: 1}, 0, 1, true},
		{"maximum integer", CapacityBudget{TotalBytes: math.MaxUint64, AvailableBytes: math.MaxUint64, MaxUtilizationPct: 100, AllocationBytes: math.MaxUint64}, 0, math.MaxUint64, true},
		{"zero total", CapacityBudget{}, 0, 0, false},
		{"impossible available", CapacityBudget{TotalBytes: 1, AvailableBytes: 2}, 0, 0, false},
		{"over reserve", CapacityBudget{TotalBytes: 100, AvailableBytes: 10, ReserveBytes: 11}, 0, 0, false},
		{"over ceiling", CapacityBudget{TotalBytes: 100, AvailableBytes: 10, MaxUtilizationPct: 80}, 0, 0, false},
		{"aggregate charge", CapacityBudget{TotalBytes: 100, AvailableBytes: 30, OutstandingBytes: 20, AllocationBytes: 20}, 0, 0, false},
		{"charge overflow", CapacityBudget{TotalBytes: math.MaxUint64, AvailableBytes: math.MaxUint64, OutstandingBytes: math.MaxUint64, AllocationBytes: 1}, 0, 0, false},
		{"negative ceiling", CapacityBudget{TotalBytes: 100, AvailableBytes: 100, MaxUtilizationPct: -1}, 0, 0, false},
		{"large ceiling", CapacityBudget{TotalBytes: 100, AvailableBytes: 100, MaxUtilizationPct: 101}, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ProjectBudget(tc.budget)
			if !tc.valid {
				if err == nil {
					t.Fatal("accepted invalid capacity")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.ResidualBytes != tc.residual || p.ProjectedUsedBytes != tc.used || p.TotalBytes != tc.budget.TotalBytes {
				t.Fatalf("got %+v", p)
			}
		})
	}
	for _, algorithm := range []string{"spread", "weighted_free_space", "least_utilized"} {
		input := fixture()
		input[0].Domain.AllocationBytes = 2000
		if _, err := Rank(request(algorithm), input); err == nil {
			t.Fatalf("%s bypassed domain admission", algorithm)
		}
	}
	c := EligibleCandidate{StorageID: "full", BackingKey: "nfs://nas/full", Member: CapacityBudget{TotalBytes: 100, AvailableBytes: 20, AllocationBytes: 20}}
	got, err := Rank(request("weighted_free_space"), []EligibleCandidate{c})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].MemberWeight != 1 || got[0].DomainWeight != 1 {
		t.Fatal("exact fit lacks one-byte floor")
	}
}

func TestLeastUtilizedUsesExactRatios(t *testing.T) {
	t.Parallel()
	// These projected ratios both round to 1 as float64; only integer comparison
	// can distinguish the emptier backing, even when its hash loses the tie.
	input := []EligibleCandidate{
		{StorageID: "a", BackingKey: "nfs://nas/a", Member: CapacityBudget{TotalBytes: math.MaxUint64, AvailableBytes: 1}},
		{StorageID: "b", BackingKey: "nfs://nas/b", Member: CapacityBudget{TotalBytes: math.MaxUint64, AvailableBytes: 2}},
	}
	got, err := Rank(request("least_utilized"), input)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Candidate.StorageID != "b" {
		t.Fatal("float rounding overrode exact utilization")
	}
	for i := uint64(1); i <= 100; i++ {
		a := projection{used: math.MaxUint64 - i, total: math.MaxUint64}
		b := projection{used: math.MaxUint64 - 2*i, total: math.MaxUint64 - i}
		left := new(big.Int).Mul(new(big.Int).SetUint64(a.used), new(big.Int).SetUint64(b.total))
		right := new(big.Int).Mul(new(big.Int).SetUint64(b.used), new(big.Int).SetUint64(a.total))
		if compareRatio(a, b) != left.Cmp(right) {
			t.Fatal("128-bit comparison disagrees with arbitrary precision oracle")
		}
	}
	// Equal domain ratios defer to the member quota's projected ratio.
	input = fixture()[:2]
	input[1].Domain = input[0].Domain
	input[1].Member.AvailableBytes = 1800
	got, err = Rank(request("least_utilized"), input)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Candidate.StorageID != "b" {
		t.Fatal("member ratio did not break domain tie")
	}
}

func TestCanonicalTiesAndDomainTags(t *testing.T) {
	t.Parallel()
	a := preparedCandidate{candidate: EligibleCandidate{BackingKey: "nfs://nas/a"}, member: projection{total: 1}, domain: projection{total: 1}}
	b := a
	b.candidate.BackingKey = "nfs://nas/b"
	for _, kind := range []strategyKind{spread, weighted, least} {
		if comparePrepared(kind, a, b) >= 0 || comparePrepared(kind, b, a) <= 0 {
			t.Fatalf("noncanonical tie for strategy %d", kind)
		}
	}
	a.domainID = domainIdentity{class: "declared", key: "a"}
	b.domainID = domainIdentity{class: "declared", key: "b"}
	if comparePrepared(weighted, a, b) >= 0 {
		t.Fatal("domain score tie is not canonical")
	}
	r := request("weighted_free_space")
	member, err := weightedScore(r, 100, "member", "same")
	if err != nil {
		t.Fatal(err)
	}
	domain, err := weightedScore(r, 100, "domain", "declared", "same")
	if err != nil {
		t.Fatal(err)
	}
	implicit, err := weightedScore(r, 100, "domain", "backing", "same")
	if err != nil {
		t.Fatal(err)
	}
	if member == domain || domain == implicit || member == implicit {
		t.Fatal("identity classes share a weighted draw")
	}
	r.AllocationGroup = "root"
	other, err := weightedScore(r, 100, "member", "same")
	if err != nil {
		t.Fatal(err)
	}
	if member == other {
		t.Fatal("allocation group absent from draw")
	}
	if _, err := weightedScore(r, 0, "member", "same"); err == nil {
		t.Fatal("accepted zero weight")
	}
}

type strategyFunc func(RequestSnapshot, []EligibleCandidate) ([]RankedCandidate, error)

func (f strategyFunc) Rank(r RequestSnapshot, c []EligibleCandidate) ([]RankedCandidate, error) {
	return f(r, c)
}

func TestRegistryRejectsContractViolations(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		strategy strategyFunc
	}{
		{"missing", func(_ RequestSnapshot, c []EligibleCandidate) ([]RankedCandidate, error) {
			return []RankedCandidate{{Candidate: c[0]}}, nil
		}},
		{"duplicate", func(_ RequestSnapshot, c []EligibleCandidate) ([]RankedCandidate, error) {
			return []RankedCandidate{{Candidate: c[0]}, {Candidate: c[0]}, {Candidate: c[2]}}, nil
		}},
		{"foreign", func(_ RequestSnapshot, c []EligibleCandidate) ([]RankedCandidate, error) {
			c[0].BackingKey = "foreign"
			return wrap(c), nil
		}},
		{"mutated output", func(_ RequestSnapshot, c []EligibleCandidate) ([]RankedCandidate, error) {
			out := wrap(c)
			out[0].Candidate.Member.AvailableBytes++
			return out, nil
		}},
		{"mutated input", func(_ RequestSnapshot, c []EligibleCandidate) ([]RankedCandidate, error) {
			out := wrap(c)
			c[0].Member.AvailableBytes++
			return out, nil
		}},
		{"invalid score", func(_ RequestSnapshot, c []EligibleCandidate) ([]RankedCandidate, error) {
			out := wrap(c)
			out[0].Score = math.NaN()
			return out, nil
		}},
		{"invalid domain score", func(_ RequestSnapshot, c []EligibleCandidate) ([]RankedCandidate, error) {
			out := wrap(c)
			out[0].DomainScore = math.Inf(1)
			return out, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewRegistry(Registration{Policy: Policy{Name: "test", Version: 1}, Strategy: tc.strategy})
			if err != nil {
				t.Fatal(err)
			}
			req := request("test")
			input := fixture()
			before := slices.Clone(input)
			if _, err := r.Rank(req, input); err == nil {
				t.Fatal("accepted contract violation")
			}
			if !slices.Equal(input, before) {
				t.Fatal("custom strategy corrupted caller-owned input")
			}
		})
	}
	broken := errors.New("algorithm unavailable")
	r, err := NewRegistry(Registration{Policy: Policy{Name: "error", Version: 1}, Strategy: strategyFunc(func(RequestSnapshot, []EligibleCandidate) ([]RankedCandidate, error) { return nil, broken })})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Rank(request("error"), fixture()); !errors.Is(err, broken) {
		t.Fatalf("lost strategy error: %v", err)
	}
}

func wrap(c []EligibleCandidate) []RankedCandidate {
	out := make([]RankedCandidate, len(c))
	for i := range c {
		out[i].Candidate = c[i]
	}
	return out
}

func TestRegistryExtensionAndValidation(t *testing.T) {
	t.Parallel()
	custom := Registration{Policy: Policy{Name: "reverse", Version: 7}, Strategy: strategyFunc(func(_ RequestSnapshot, c []EligibleCandidate) ([]RankedCandidate, error) {
		slices.Reverse(c)
		return wrap(c), nil
	})}
	r, err := NewRegistry(custom)
	if err != nil {
		t.Fatal(err)
	}
	req := request("reverse")
	req.Policy.Version = 7
	got, err := r.Rank(req, fixture())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(got), []string{"c", "b", "a"}) {
		t.Fatal("compiled extension was not used")
	}
	for _, p := range []Policy{{Name: "spread", Version: 2}, {Name: "round_robin", Version: 1}, {Name: "spread", Version: 0}} {
		if err := ValidateStrategy(p.Name, p.Version); err == nil {
			t.Fatal("accepted unsupported policy")
		}
	}
	if err := ValidateStrategy("spread", 1); err != nil {
		t.Fatal(err)
	}
	for _, registrations := range [][]Registration{{custom, custom}, {{Policy: Policy{Name: "spread", Version: 1}, Strategy: custom.Strategy}}, {{Policy: Policy{Name: "nil", Version: 1}}}, {{Policy: Policy{Name: " ", Version: 1}, Strategy: custom.Strategy}}} {
		if _, err := NewRegistry(registrations...); err == nil {
			t.Fatal("accepted invalid registration")
		}
	}
	if err := (*Registry)(nil).ValidateStrategy("spread", 1); err == nil {
		t.Fatal("accepted nil registry")
	}
	req = request("weighted_free_space")
	req.SeedSet = false
	if _, err := Rank(req, fixture()); err == nil {
		t.Fatal("accepted missing entropy")
	}
	req.SeedSet = true
	req.Seed = [32]byte{}
	if _, err := Rank(req, fixture()); err != nil {
		t.Fatalf("zero injected seed is valid: %v", err)
	}
	for _, bad := range []RequestSnapshot{{Policy: Policy{Name: "spread", Version: 1}}, {Policy: Policy{Name: "spread", Version: 1}, Namespace: "ns", AllocationKey: "a", AllocationGroup: string([]byte{0xff})}} {
		if _, err := Rank(bad, fixture()); err == nil {
			t.Fatal("accepted malformed request")
		}
	}
	if got, err := Rank(request("spread"), nil); err != nil || len(got) != 0 {
		t.Fatalf("empty ranking: %v %v", got, err)
	}
}

func TestCandidateValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func([]EligibleCandidate)
	}{
		{"duplicate backing", func(c []EligibleCandidate) { c[1].BackingKey = c[0].BackingKey }},
		{"duplicate storage", func(c []EligibleCandidate) { c[1].StorageID = c[0].StorageID }},
		{"blank identity", func(c []EligibleCandidate) { c[0].StorageID = " " }},
		{"blank domain", func(c []EligibleCandidate) { c[0].DomainKey = " " }},
		{"invalid UTF8", func(c []EligibleCandidate) { c[0].BackingKey = string([]byte{0xff}) }},
		{"inconsistent domain", func(c []EligibleCandidate) { c[1].Domain.TotalBytes++ }},
		{"implicit domain override", func(c []EligibleCandidate) { c[2].Domain = c[0].Domain }},
		{"invalid member", func(c []EligibleCandidate) { c[0].Member.TotalBytes = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := fixture()
			tc.change(input)
			if _, err := Rank(request("spread"), input); err == nil {
				t.Fatal("accepted malformed candidates")
			}
		})
	}
}

func TestDeterministicDistribution(t *testing.T) {
	t.Parallel()
	// Independent residual budgets have a 1:9 ratio. Fixed allocation keys and
	// SHA-derived injected seeds make this test exactly reproducible.
	input := []EligibleCandidate{
		{StorageID: "small", BackingKey: "nfs://nas/small", Member: CapacityBudget{TotalBytes: 1000, AvailableBytes: 110, AllocationBytes: 10}},
		{StorageID: "large", BackingKey: "nfs://nas/large", Member: CapacityBudget{TotalBytes: 1000, AvailableBytes: 910, AllocationBytes: 10}},
	}
	const attempts = 6000
	counts := map[string]int{}
	for _, algorithm := range []string{"spread", "weighted_free_space"} {
		for i := 0; i < attempts; i++ {
			r := request(algorithm)
			r.AllocationKey = fmt.Sprintf("allocation-%d", i)
			r.Seed = sha256.Sum256([]byte(r.AllocationKey))
			got, err := Rank(r, input)
			if err != nil {
				t.Fatal(err)
			}
			if got[0].Candidate.StorageID == "large" {
				counts[algorithm]++
			}
		}
	}
	if counts["spread"] < 2800 || counts["spread"] > 3200 {
		t.Fatalf("spread count imbalance: %v", counts)
	}
	if counts["weighted_free_space"] < 5200 || counts["weighted_free_space"] > 5600 {
		t.Fatalf("weighted did not follow capacity ratio: %v", counts)
	}
	t.Logf("large member wins across %d allocations: spread=%d weighted=%d", attempts, counts["spread"], counts["weighted_free_space"])
	// Duplicating an export as another distinct member of a declared domain must
	// not multiply that domain's chance of winning. Equal domain budgets stay 1:1.
	one := CapacityBudget{TotalBytes: 1000, AvailableBytes: 500, AllocationBytes: 100}
	input = []EligibleCandidate{{StorageID: "a", BackingKey: "nfs://nas/a", DomainKey: "shared", Member: one, Domain: one}, {StorageID: "b", BackingKey: "nfs://nas/b", DomainKey: "shared", Member: one, Domain: one}, {StorageID: "c", BackingKey: "nfs://nas/c", Member: one}}
	wins := 0
	for i := 0; i < attempts; i++ {
		r := request("weighted_free_space")
		r.AllocationKey = fmt.Sprint(i)
		r.Seed = sha256.Sum256([]byte(r.AllocationKey))
		got, err := Rank(r, input)
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Candidate.DomainKey == "shared" {
			wins++
		}
	}
	if wins < 2800 || wins > 3200 {
		t.Fatalf("shared domain was multiplied by member count: %d", wins)
	}
	t.Logf("two-member shared domain wins against equal independent domain: %d/%d", wins, attempts)
}

func TestSeedAndConcurrentRegistry(t *testing.T) {
	t.Parallel()
	if _, err := NewSeed(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			for _, name := range []string{"spread", "weighted_free_space", "least_utilized"} {
				if _, err := Rank(request(name), fixture()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func FuzzEncodeTuple(f *testing.F) {
	f.Add("a", "bc")
	f.Add("é", "")
	f.Fuzz(func(t *testing.T, a, b string) {
		if len(a)+len(b) > 1<<20 {
			t.Skip()
		}
		encoded, err := EncodeTuple(a, b)
		if err != nil {
			if !strings.Contains(err.Error(), "UTF-8") {
				t.Fatal(err)
			}
			return
		}
		// Decode independently using the documented wire protocol, including UTF-8
		// byte lengths, rather than comparing EncodeTuple with itself.
		for _, want := range []string{a, b} {
			if len(encoded) < 4 {
				t.Fatal("missing field length")
			}
			n := uint64(binary.BigEndian.Uint32(encoded[:4]))
			encoded = encoded[4:]
			if n > uint64(len(encoded)) {
				t.Fatal("field length exceeds payload")
			}
			if string(encoded[:int(n)]) != want {
				t.Fatal("tuple round-trip changed field")
			}
			encoded = encoded[int(n):]
		}
		if len(encoded) != 0 {
			t.Fatal("unexpected trailing bytes")
		}
	})
}
