package endpointcache

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// failedAttempt is an attempt at payloadNow that ended with failure and waits
// the usual hour.
func failedAttempt(failure DiscoveryFailure) *DiscoveryAttempt {
	return &DiscoveryAttempt{At: payloadNow, NextRefreshAfter: payloadNow.Add(RefreshInterval), Failure: failure}
}

// failureSection is the section a failed attempt at payloadNow creates.
func failureSection(failure DiscoveryFailure) *DiscoverySection {
	return &DiscoverySection{
		DiscoveredAt:     payloadNow,
		NextRefreshAfter: payloadNow.Add(RefreshInterval),
		Nodes:            []DiscoveredNode{},
		LastFailure:      failure,
	}
}

func TestDiscoveryFailureValid(t *testing.T) {
	for _, f := range []DiscoveryFailure{
		FailureNodesUnreadable, FailureNodesEmpty, FailureNodeNameInvalid, FailureRootCAUnreadable,
		FailureReadsTimedOut, FailureReadsFailed,
	} {
		if !f.Valid() {
			t.Errorf("%q is not valid", f)
		}
	}
	for _, f := range []DiscoveryFailure{"", "NODES_EMPTY", "nodes empty", "timeout"} {
		if f.Valid() {
			t.Errorf("%q is valid", f)
		}
	}
}

func TestMergeAttemptStoresItsFailure(t *testing.T) {
	key := mergeKey()

	// Over a stored result, the attempt moves the two times and stores the
	// failure, and the outcome, nodes, and pins stay as stored.
	stored := payloadFor(key, "pve2", "pve3")
	stored.Discovery.Outcome = OutcomeNoStatus
	next, _, err := Merge(stored, key, Change{Attempt: failedAttempt(FailureRootCAUnreadable)}, payloadNow)
	want := stored.clone()
	want.Discovery.DiscoveredAt, want.Discovery.NextRefreshAfter = payloadNow, payloadNow.Add(RefreshInterval)
	want.Discovery.LastFailure = FailureRootCAUnreadable
	if err != nil || !reflect.DeepEqual(next, want) {
		t.Fatalf("over a result: %v\n got %s\nwant %s", err, trustDump(next), trustDump(want))
	}

	// Another failure replaces the stored one, and an attempt without a
	// failure, which is what the trust rules record, leaves it.
	later := payloadNow.Add(time.Minute)
	again, _, err := Merge(next, key, Change{Attempt: &DiscoveryAttempt{
		At: later, NextRefreshAfter: later.Add(RefreshInterval), Failure: FailureNodesUnreadable,
	}}, later)
	if err != nil || again.Discovery.LastFailure != FailureNodesUnreadable {
		t.Errorf("a second failure: %+v, %v", again.Discovery, err)
	}
	kept, _, err := Merge(next, key, Change{Attempt: &DiscoveryAttempt{
		At: later, NextRefreshAfter: later.Add(MaxRefreshWait),
	}}, later)
	if err != nil || kept.Discovery.LastFailure != FailureRootCAUnreadable ||
		!kept.Discovery.NextRefreshAfter.Equal(later.Add(MaxRefreshWait)) {
		t.Errorf("an attempt without a failure: %+v, %v", kept.Discovery, err)
	}

	// With no file, the attempt creates a section that holds no nodes and no
	// outcome.
	created, drops, err := Merge(nil, key, Change{Attempt: failedAttempt(FailureNodesEmpty)}, payloadNow)
	wantCreated := &Payload{Key: key, Discovery: failureSection(FailureNodesEmpty)}
	if err != nil || len(drops) != 0 || !reflect.DeepEqual(created, wantCreated) {
		t.Fatalf("no file: %v, drops %v\n got %s\nwant %s", err, drops, trustDump(created), trustDump(wantCreated))
	}

	// Beside a stored identity with no section, the identity stays.
	identityOnly := &Payload{Key: key, Cluster: ptr(payloadCluster(payloadNow))}
	withIdentity, _, err := Merge(identityOnly, key, Change{Attempt: failedAttempt(FailureNodeNameInvalid)}, payloadNow)
	wantIdentity := identityOnly.clone()
	wantIdentity.Discovery = failureSection(FailureNodeNameInvalid)
	if err != nil || !reflect.DeepEqual(withIdentity, wantIdentity) {
		t.Errorf("an identity alone: %v\n got %s\nwant %s", err, trustDump(withIdentity), trustDump(wantIdentity))
	}

	// A stored hint stays, because a failure is not a result.
	hinted := &Payload{Key: key, ColdStart: &ColdStartHint{Name: "pve2", ProbedAt: payloadNow}}
	withHint, _, err := Merge(hinted, key, Change{Attempt: failedAttempt(FailureReadsTimedOut)}, payloadNow)
	if err != nil || withHint.ColdStart == nil || withHint.Discovery == nil ||
		withHint.Discovery.LastFailure != FailureReadsTimedOut {
		t.Errorf("beside a hint: %s, %v", trustDump(withHint), err)
	}

	// A section a reader hides for its time still records nothing.
	aheadFile := payloadFor(key, "pve2")
	aheadFile.Discovery.DiscoveredAt = payloadNow.Add(time.Hour)
	if next, _, err := Merge(aheadFile, key, Change{Attempt: failedAttempt(FailureReadsFailed)},
		payloadNow); err != nil || !reflect.DeepEqual(next, aheadFile) {
		t.Errorf("a section dated ahead: %s, %v", trustDump(next), err)
	}

	// A next refresh further ahead than MaxRefreshWait is dropped from a
	// created section too.
	far := failedAttempt(FailureNodesEmpty)
	far.NextRefreshAfter = payloadNow.Add(MaxRefreshWait + time.Minute)
	farFile, drops, err := Merge(nil, key, Change{Attempt: far}, payloadNow)
	if err != nil || farFile == nil || !farFile.Discovery.NextRefreshAfter.IsZero() ||
		!reflect.DeepEqual(drops, []Drop{{Record: "next_refresh_after", Reason: DropFuture}}) {
		t.Errorf("a far next refresh on a created section: %+v, %v, %v", farFile, drops, err)
	}

	// The created section passes validation and keeps its failure.
	usable, drops, err := Validate(created, key, payloadNow)
	if err != nil || len(drops) != 0 || !reflect.DeepEqual(usable.Discovery, failureSection(FailureNodesEmpty)) {
		t.Errorf("Validate of the created section: %+v, %v, %v", usable, drops, err)
	}
}

func TestMergeRefusesAnUnknownFailure(t *testing.T) {
	key := mergeKey()
	for name, stored := range map[string]*Payload{"no file": nil, "a stored result": payloadFor(key, "pve2")} {
		_, _, err := Merge(stored, key, Change{Attempt: failedAttempt("timeout")}, payloadNow)
		if !errors.Is(err, ErrInvalidChange) || !strings.Contains(err.Error(), "failure is not one of") {
			t.Errorf("%s: %v, want ErrInvalidChange", name, err)
		}
	}
	// A failure beside a result in the same change is refused.
	_, _, err := Merge(nil, key, Change{Attempt: failedAttempt(FailureNodesEmpty), Result: mergeResult("pve2")},
		payloadNow)
	if !errors.Is(err, ErrInvalidChange) {
		t.Errorf("a failure beside a result: %v, want ErrInvalidChange", err)
	}
}

func TestMergeResultClearsTheLastFailure(t *testing.T) {
	key := mergeKey()
	failed := mustMerge(t, payloadFor(key, "pve2"), Change{Attempt: failedAttempt(FailureRootCAUnreadable)})
	next := mustMerge(t, failed, Change{Result: mergeResult("pve2", "pve3")})
	if next.Discovery.LastFailure != "" || len(next.Discovery.Nodes) != 2 || next.Discovery.Outcome != OutcomeOK {
		t.Errorf("a result over a failure: %+v", next.Discovery)
	}
	onlyFailure := mustMerge(t, nil, Change{Attempt: failedAttempt(FailureNodesUnreadable)})
	if next := mustMerge(t, onlyFailure, Change{Result: mergeResult("pve2")}); next.Discovery.LastFailure != "" ||
		next.Discovery.Outcome != OutcomeOK || next.Cluster == nil {
		t.Errorf("a result over a created section: %s", trustDump(next))
	}

	// A result that carries a failure of its own is refused.
	result := mergeResult("pve2")
	result.Discovery.LastFailure = FailureNodesEmpty
	if _, _, err := Merge(failed, key, Change{Result: result}, payloadNow); !errors.Is(err, ErrInvalidChange) ||
		!strings.Contains(err.Error(), "carries a last_failure") {
		t.Errorf("a result with a last_failure: %v, want ErrInvalidChange", err)
	}
	result.Discovery.Outcome = ""
	if _, _, err := Merge(nil, key, Change{Result: result}, payloadNow); !errors.Is(err, ErrInvalidChange) {
		t.Errorf("a result with no outcome and a last_failure: %v, want ErrInvalidChange", err)
	}
}

func TestValidateLastFailure(t *testing.T) {
	key := mergeKey()
	section := func(outcome DiscoveryOutcome, failure DiscoveryFailure, names ...string) *Payload {
		p := payloadFor(key, names...)
		p.Discovery.Outcome, p.Discovery.LastFailure = outcome, failure
		return p
	}
	invalid := map[string]*Payload{
		"an unknown failure beside an outcome": section(OutcomeOK, "timeout", "pve2"),
		"an unknown failure with no outcome":   section("", "nodes empty"),
		"no outcome and no failure":            section("", ""),
		"no outcome with nodes":                section("", FailureNodesEmpty, "pve2"),
		"an unknown outcome with a failure":    section("broken", FailureNodesEmpty),
	}
	for name, p := range invalid {
		if _, _, err := Validate(p, key, payloadNow); !errors.Is(err, ErrPayloadInvalid) {
			t.Errorf("%s: %v, want ErrPayloadInvalid", name, err)
		}
	}
	for name, p := range map[string]*Payload{
		"a failure beside a result": section(OutcomeOK, FailureRootCAUnreadable, "pve2"),
		"a failure alone":           section("", FailureNodesUnreadable),
	} {
		got, _, err := Validate(p, key, payloadNow)
		if err != nil || got.Discovery.LastFailure != p.Discovery.LastFailure ||
			got.Discovery.Outcome != p.Discovery.Outcome || len(got.Discovery.Nodes) != len(p.Discovery.Nodes) {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
}

func TestRefreshDueWaitsForAFailedAttempt(t *testing.T) {
	key := mergeKey()
	p := mustMerge(t, nil, Change{Attempt: failedAttempt(FailureNodesUnreadable)})
	usable, _, err := Validate(p, key, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	next := payloadNow.Add(RefreshInterval)
	cases := []struct {
		name      string
		now       time.Time
		pinFailed bool
		want      bool
	}{
		{"just after the attempt", payloadNow.Add(time.Second), false, false},
		{"just before the next refresh", next.Add(-time.Second), false, false},
		{"a pin failure counts for nothing without nodes", payloadNow.Add(PinRefreshFloor + time.Minute), true, false},
		{"at the next refresh", next, false, true},
		{"after the next refresh", next.Add(time.Minute), false, true},
	}
	for _, tc := range cases {
		if got := RefreshDue(usable, tc.now, tc.pinFailed); got != tc.want {
			t.Errorf("%s: RefreshDue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStoreRecordsAFailedAttemptOnAMissingFile(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, "secret-a", StoreOptions{})
	res, err := s.UpdatePayload(context.Background(), payloadNow, func(Stored) (Change, error) {
		return Change{Attempt: failedAttempt(FailureRootCAUnreadable)}, nil
	})
	if err != nil || !res.Written {
		t.Fatalf("UpdatePayload: %+v, %v", res, err)
	}
	got, drops, err := s.ReadPayload(payloadNow, nil)
	if err != nil || len(drops) != 0 || !reflect.DeepEqual(got.Discovery, failureSection(FailureRootCAUnreadable)) {
		t.Fatalf("ReadPayload: %+v, %v, %v", got, drops, err)
	}
	if RefreshDue(got, payloadNow.Add(time.Minute), false) {
		t.Error("the refresh is due a minute after a failed attempt")
	}
	data := string(readStoreFile(t, filepath.Join(dir, s.FileName())))
	if !strings.Contains(data, `\"last_failure\":\"root_ca_unreadable\"`) &&
		!strings.Contains(data, `"last_failure":"root_ca_unreadable"`) {
		t.Errorf("the file does not carry last_failure: %s", data)
	}

	// A result written next clears the failure, and the field leaves the file.
	if _, err := s.UpdatePayload(context.Background(), payloadNow.Add(time.Minute), func(Stored) (Change, error) {
		result := mergeResult("pve2")
		result.Discovery.DiscoveredAt = payloadNow.Add(time.Minute)
		return Change{Result: result}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if data := string(readStoreFile(t, filepath.Join(dir, s.FileName()))); strings.Contains(data, "last_failure") {
		t.Errorf("the file still carries last_failure after a result: %s", data)
	}
}
