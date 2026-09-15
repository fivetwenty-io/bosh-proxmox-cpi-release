package storageinventory

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// siblingFixture builds a two-member snapshot whose members share one declared
// capacity domain, so both sibling maps have a key to land on.
func siblingFixture(t *testing.T) (*Snapshot, string, string) {
	t.Helper()
	source := sourceFor(t, "a", "b")
	source.set("n1", status(t, "a", 2000, 1000), status(t, "b", 2000, 1000))
	cfg := policy("a", "b")
	cfg.StorageCapacityDomains = map[string]config.StorageCapacityDomain{"nas": {Members: []string{"a", "b"}}}
	s := discover(t, collector(t, source, newClock()), cfg, "n1")
	base, err := NewLedger().Candidate(s, "n1", "a", 1, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if base.BackingKey == "" || base.DomainKey == "" {
		t.Fatalf("fixture lacks a capacity key or a domain key: %+v", base)
	}
	return s, base.BackingKey, base.DomainKey
}

func TestLedgerSiblingBytesChargeMemberAndDomain(t *testing.T) {
	t.Parallel()
	s, memberKey, domainKey := siblingFixture(t)
	seeded := NewLedgerWithSiblings(map[string]uint64{memberKey: 400}, map[string]uint64{domainKey: 700})
	candidate, err := seeded.Candidate(s, "n1", "a", 100, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Member.OutstandingBytes != 400 || candidate.Domain.OutstandingBytes != 700 {
		t.Fatalf("sibling bytes missing from the projection: %+v", candidate)
	}
	if len(seeded.Records()) != 0 {
		t.Fatalf("sibling bytes became ledger entries: %+v", seeded.Records())
	}
	// A sibling on another member charges the domain but leaves this member alone.
	other := NewLedgerWithSiblings(nil, map[string]uint64{domainKey: 500})
	candidate, err = other.Candidate(s, "n1", "a", 100, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Member.OutstandingBytes != 0 || candidate.Domain.OutstandingBytes != 500 {
		t.Fatalf("domain-only sibling leaked into the member: %+v", candidate)
	}
	// An unrelated capacity key never charges this candidate.
	unrelated := NewLedgerWithSiblings(map[string]uint64{memberKey + "-elsewhere": 900}, nil)
	candidate, err = unrelated.Candidate(s, "n1", "a", 100, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Member.OutstandingBytes != 0 {
		t.Fatalf("sibling bytes charged the wrong capacity key: %+v", candidate)
	}
}

func TestLedgerSiblingBytesSurviveCloneAndStayOutOfRecords(t *testing.T) {
	t.Parallel()
	s, memberKey, domainKey := siblingFixture(t)
	seeded := NewLedgerWithSiblings(map[string]uint64{memberKey: 400}, map[string]uint64{domainKey: 400})
	next, err := seeded.WithPlanned(s, Charge{ID: "root", Role: "root", Node: "n1", StorageID: "a", Bytes: 300})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := next.Candidate(s, "n1", "a", 100, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Member.OutstandingBytes != 700 || candidate.Domain.OutstandingBytes != 700 {
		t.Fatalf("clone dropped the sibling bytes: %+v", candidate)
	}
	records := next.Records()
	if len(records) != 1 || records[0].Charge.ID != "root" {
		t.Fatalf("sibling bytes reached the plan charges: %+v", records)
	}
	// Submit clones as well, so the maps have to survive every state change.
	submitted, err := next.Submit("root")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err = submitted.Candidate(s, "n1", "a", 100, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Member.OutstandingBytes != 700 {
		t.Fatalf("submit dropped the sibling bytes: %+v", candidate)
	}
}

func TestLedgerSiblingMapsAreCopiedAtConstruction(t *testing.T) {
	t.Parallel()
	s, memberKey, domainKey := siblingFixture(t)
	member := map[string]uint64{memberKey: 400}
	domain := map[string]uint64{domainKey: 700}
	seeded := NewLedgerWithSiblings(member, domain)
	member[memberKey] = 900
	domain[domainKey] = 1900
	delete(member, memberKey)
	candidate, err := seeded.Candidate(s, "n1", "a", 100, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Member.OutstandingBytes != 400 || candidate.Domain.OutstandingBytes != 700 {
		t.Fatalf("caller mutated a live ledger: %+v", candidate)
	}
}

func TestLedgerSiblingBytesDecideAdmission(t *testing.T) {
	t.Parallel()
	s, memberKey, domainKey := siblingFixture(t)
	// The member has 1000 usable bytes and the domain has 2000.
	if _, err := NewLedger().Candidate(s, "n1", "a", 900, Limits{}); err != nil {
		t.Fatalf("member rejected an allocation that fits: %v", err)
	}
	crowded := NewLedgerWithSiblings(map[string]uint64{memberKey: 200}, nil)
	_, err := crowded.Candidate(s, "n1", "a", 900, Limits{})
	if err == nil || !strings.Contains(err.Error(), "member a admission") {
		t.Fatalf("member admitted an allocation a sibling already claimed: %v", err)
	}
	crowdedDomain := NewLedgerWithSiblings(nil, map[string]uint64{domainKey: 1500})
	_, err = crowdedDomain.Candidate(s, "n1", "a", 900, Limits{})
	if err == nil || !strings.Contains(err.Error(), "admission") {
		t.Fatalf("domain admitted an allocation a sibling already claimed: %v", err)
	}
	// The same sibling bytes block WithPlanned, which ranks through Candidate.
	if _, err := crowded.WithPlanned(s, Charge{ID: "root", Role: "root", Node: "n1", StorageID: "a", Bytes: 900}); err == nil {
		t.Fatal("planning ignored the sibling claim")
	}
}

func TestLedgerNilSiblingMapsMatchNewLedger(t *testing.T) {
	t.Parallel()
	s, _, _ := siblingFixture(t)
	empty := NewLedgerWithSiblings(nil, nil)
	if empty.siblingMember != nil || empty.siblingDomain != nil {
		t.Fatalf("nil maps materialized: %+v %+v", empty.siblingMember, empty.siblingDomain)
	}
	if !reflect.DeepEqual(empty, NewLedger()) {
		t.Fatalf("nil-seeded ledger differs from an empty one: %+v", empty)
	}
	blank := NewLedgerWithSiblings(map[string]uint64{}, map[string]uint64{})
	if !reflect.DeepEqual(blank, NewLedger()) {
		t.Fatalf("empty maps materialized: %+v", blank)
	}
	fromNil, err := empty.Candidate(s, "n1", "a", 600, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	fromPlain, err := NewLedger().Candidate(s, "n1", "a", 600, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromNil, fromPlain) {
		t.Fatalf("nil sibling maps changed the candidate: %+v %+v", fromNil, fromPlain)
	}
}

func TestLedgerSiblingByteSumsAreOverflowChecked(t *testing.T) {
	t.Parallel()
	s, memberKey, domainKey := siblingFixture(t)
	planned, err := NewLedger().WithPlanned(s, Charge{ID: "root", Role: "root", Node: "n1", StorageID: "a", Bytes: 300})
	if err != nil {
		t.Fatal(err)
	}
	// The maps are unexported and immutable by contract, so an overflowing sum
	// can only be built from inside the package.
	member := planned
	member.siblingMember = map[string]uint64{memberKey: math.MaxUint64}
	if _, err := member.Candidate(s, "n1", "a", 1, Limits{}); err == nil || !strings.Contains(err.Error(), "member a sibling bytes") {
		t.Fatalf("member sibling overflow accepted: %v", err)
	}
	domain := planned
	domain.siblingDomain = map[string]uint64{domainKey: math.MaxUint64}
	if _, err := domain.Candidate(s, "n1", "a", 1, Limits{}); err == nil || !strings.Contains(err.Error(), "sibling bytes") {
		t.Fatalf("domain sibling overflow accepted: %v", err)
	}
}
