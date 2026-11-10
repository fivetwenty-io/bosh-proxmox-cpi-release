package endpointcache

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

const mergeSeed = "pve1.dc1.example.com:8006"

func mergeKey() Key { return payloadTestKey() }

func mergeResult(names ...string) *DiscoveryResult {
	section := payloadDiscovery()
	for _, name := range names {
		section.Nodes = append(section.Nodes, payloadNode(name, payloadNow))
	}
	return &DiscoveryResult{Cluster: payloadCluster(payloadNow), Discovery: section}
}

func mustMerge(t *testing.T, stored *Payload, change Change) *Payload {
	t.Helper()
	next, _, err := Merge(stored, mergeKey(), change, payloadNow)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	return next
}

func TestMergeKeepsTheNewerMarkPerEndpoint(t *testing.T) {
	stored := &Payload{Key: mergeKey(), Marks: map[string]DownMark{
		mergeSeed: {DownAt: payloadNow.Add(-30 * time.Second), Class: MarkDial},
	}}
	cases := []struct {
		name     string
		incoming DownMark
		want     MarkClass
	}{
		{"a newer mark replaces", DownMark{DownAt: payloadNow.Add(-10 * time.Second), Class: MarkDNS}, MarkDNS},
		{"an older mark loses", DownMark{DownAt: payloadNow.Add(-60 * time.Second), Class: MarkTLSVerify}, MarkDial},
		{"an equal time takes the writer's mark", DownMark{DownAt: payloadNow.Add(-30 * time.Second), Class: MarkPin}, MarkPin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := mustMerge(t, stored, Change{Marks: map[string]DownMark{mergeSeed: tc.incoming}})
			if len(next.Marks) != 1 || next.Marks[mergeSeed].Class != tc.want {
				t.Errorf("marks %v, want class %s", next.Marks, tc.want)
			}
		})
	}
}

func TestMergeAddsMarksForOtherEndpoints(t *testing.T) {
	stored := &Payload{Key: mergeKey(), Marks: map[string]DownMark{
		mergeSeed: {DownAt: payloadNow.Add(-30 * time.Second), Class: MarkDial},
	}}
	stored.Discovery = &DiscoverySection{Outcome: OutcomeOK, Nodes: []DiscoveredNode{payloadNode("pve2", payloadNow)}}
	stored.Cluster = ptr(payloadCluster(payloadNow))
	next := mustMerge(t, stored, Change{Marks: map[string]DownMark{
		"PVE2.dc1.example.com:8006": {DownAt: payloadNow, Class: MarkPin},
	}})
	if len(next.Marks) != 2 || next.Marks["pve2.dc1.example.com:8006"].Class != MarkPin || next.Marks[mergeSeed].Class != MarkDial {
		t.Errorf("marks %v", next.Marks)
	}
}

func ptr[T any](v T) *T { return &v }

func TestMergeDropsMarksThatExpired(t *testing.T) {
	stored := &Payload{Key: mergeKey(), Marks: map[string]DownMark{
		mergeSeed: {DownAt: payloadNow.Add(-MarkTTL - time.Second), Class: MarkDial},
	}}
	// A change that holds something else keeps the file, and the stale mark is gone.
	next := mustMerge(t, stored, Change{Hint: &ColdStartHint{Name: "pve2", ProbedAt: payloadNow}})
	if next.Marks != nil {
		t.Errorf("an expired mark was kept: %v", next.Marks)
	}
	// An incoming mark that is already old, or ahead, never lands.
	old := DownMark{DownAt: payloadNow.Add(-5 * time.Minute), Class: MarkDial}
	ahead := DownMark{DownAt: payloadNow.Add(time.Hour), Class: MarkDial}
	next, drops, err := Merge(nil, mergeKey(), Change{
		Marks: map[string]DownMark{mergeSeed: old, "pve2.dc1.example.com:8006": ahead},
		Hint:  &ColdStartHint{Name: "pve2", ProbedAt: payloadNow},
	}, payloadNow)
	if err != nil || len(next.Marks) != 0 || len(drops) != 2 {
		t.Errorf("marks %v, drops %v, err %v", next.Marks, drops, err)
	}
}

func TestMergePrunesMarksForEndpointsOutsideThePool(t *testing.T) {
	key := mergeKey()
	key.Endpoints = []string{mergeSeed, "pve1b.dc1.example.com:8006"}
	stored := &Payload{Key: key, Marks: map[string]DownMark{
		mergeSeed:                      {DownAt: payloadNow, Class: MarkDial},
		"pve1b.dc1.example.com:8006":   {DownAt: payloadNow, Class: MarkDial},
		"pve2.dc1.example.com:8006":    {DownAt: payloadNow, Class: MarkDial},
		"gone.dc1.example.com:8006":    {DownAt: payloadNow, Class: MarkDial},
		"cand.dc1.example.com:8006":    {DownAt: payloadNow, Class: MarkDial},
		"pve1.dc1.example.com:8007":    {DownAt: payloadNow, Class: MarkDial},
		"10.0.0.9:8006":                {DownAt: payloadNow, Class: MarkDial},
		"pve9.other.example.org:8006":  {DownAt: payloadNow, Class: MarkDial},
		"pve10.other.example.org:8006": {DownAt: payloadNow, Class: MarkDial},
	}}
	stored.Cluster = ptr(payloadCluster(payloadNow))
	stored.Discovery = &DiscoverySection{Outcome: OutcomeOK, Nodes: []DiscoveredNode{payloadNode("pve2", payloadNow)}}

	next, _, err := Merge(stored, key, Change{Pool: []string{"CAND.dc1.example.com:8006"}}, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	kept := make([]string, 0, len(next.Marks))
	for endpoint := range next.Marks {
		kept = append(kept, endpoint)
	}
	want := map[string]bool{
		mergeSeed: true, "pve1b.dc1.example.com:8006": true, "pve2.dc1.example.com:8006": true, "cand.dc1.example.com:8006": true,
	}
	if len(kept) != len(want) {
		t.Fatalf("kept marks %v, want %v", kept, want)
	}
	for _, endpoint := range kept {
		if !want[endpoint] {
			t.Errorf("mark for %s survived pruning", endpoint)
		}
	}

	// A result that replaces the node list prunes the marks of the nodes it removes.
	next, _, err = Merge(stored, key, Change{Result: mergeResult("pve3")}, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := next.Marks["pve2.dc1.example.com:8006"]; ok {
		t.Error("the mark of a node the new result dropped survived")
	}
}

func TestMergePrunesEveryMarkWhenNothingRemains(t *testing.T) {
	stored := &Payload{Key: mergeKey(), Marks: map[string]DownMark{
		"gone.dc1.example.com:8006": {DownAt: payloadNow, Class: MarkDial},
	}}
	next := mustMerge(t, stored, Change{})
	if next == nil || next.Marks != nil {
		t.Errorf("got %+v, want a payload with no marks", next)
	}
}

func TestMergeReplacesTheNodeListOnlyWithAResult(t *testing.T) {
	stored := payloadFor(mergeKey(), "pve2", "pve3")
	stored.Marks = map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkDial}}

	same := mustMerge(t, stored, Change{Marks: map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkDNS}}})
	if len(same.Discovery.Nodes) != 2 || same.Cluster == nil {
		t.Errorf("a marks-only change touched the node list: %+v", same.Discovery)
	}

	next := mustMerge(t, stored, Change{Result: mergeResult("pve4")})
	if len(next.Discovery.Nodes) != 1 || next.Discovery.Nodes[0].Name != "pve4" {
		t.Errorf("nodes %+v, want only pve4", next.Discovery.Nodes)
	}
	if !next.Discovery.Nodes[0].ConfirmedAt.Equal(payloadNow) || next.Cluster.RootCASHA256 != payloadPin("3c") {
		t.Errorf("result not stored: %+v %+v", next.Discovery, next.Cluster)
	}
	if next.Marks[mergeSeed].Class != MarkDial {
		t.Errorf("a result lost the seed's mark: %v", next.Marks)
	}
}

func TestMergeADiscoveryRemovesTheHint(t *testing.T) {
	stored := &Payload{Key: mergeKey(), ColdStart: &ColdStartHint{Name: "pve2", ProbedAt: payloadNow.Add(-time.Minute)}}
	next := mustMerge(t, stored, Change{Result: mergeResult("pve2")})
	if next.ColdStart != nil {
		t.Errorf("the next discovery kept the hint: %+v", next.ColdStart)
	}
}

func TestMergeKeepsAStoredHintAndReplacesItOnlyWithAnother(t *testing.T) {
	stored := &Payload{Key: mergeKey(), ColdStart: &ColdStartHint{Name: "pve3", ProbedAt: payloadNow.Add(-time.Minute)}}

	// The writer has no az_map input at all, and the stored hint stays.
	next := mustMerge(t, stored, Change{Marks: map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkDial}}})
	if next.ColdStart == nil || next.ColdStart.Name != "pve3" {
		t.Errorf("hint %+v, want pve3 kept", next.ColdStart)
	}
	next = mustMerge(t, stored, Change{})
	if next.ColdStart == nil || next.ColdStart.Name != "pve3" {
		t.Errorf("an empty change lost the hint: %+v", next.ColdStart)
	}

	// A hint of its own replaces it.
	next = mustMerge(t, stored, Change{Hint: &ColdStartHint{Name: "pve4", ProbedAt: payloadNow}})
	if next.ColdStart == nil || next.ColdStart.Name != "pve4" || !next.ColdStart.ProbedAt.Equal(payloadNow) {
		t.Errorf("hint %+v, want pve4", next.ColdStart)
	}

	// A stored hint that fails a check other than the az_map is not kept.
	expired := &Payload{Key: mergeKey(), ColdStart: &ColdStartHint{Name: "pve3", ProbedAt: payloadNow.Add(-2 * time.Hour)}}
	next = mustMerge(t, expired, Change{Marks: map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkDial}}})
	if next.ColdStart != nil {
		t.Errorf("an expired hint was kept: %+v", next.ColdStart)
	}
}

func TestMergeRefusesAHintItCannotWrite(t *testing.T) {
	cases := map[string]ColdStartHint{
		"name with dots": {Name: "pve2.example.com", ProbedAt: payloadNow},
		"loopback alias": {Name: "localhost", ProbedAt: payloadNow},
		"all digits":     {Name: "42", ProbedAt: payloadNow},
		"ahead":          {Name: "pve2", ProbedAt: payloadNow.Add(time.Hour)},
		"expired":        {Name: "pve2", ProbedAt: payloadNow.Add(-2 * time.Hour)},
	}
	for name, hint := range cases {
		t.Run(name, func(t *testing.T) {
			next, _, err := Merge(nil, mergeKey(), Change{Hint: &hint}, payloadNow)
			if !errors.Is(err, ErrInvalidChange) || next != nil {
				t.Errorf("Merge: %+v, %v, want ErrInvalidChange", next, err)
			}
		})
	}
}

func TestMergeRefusesAResultItCannotWrite(t *testing.T) {
	cases := map[string]func(r *DiscoveryResult){
		"invalid node name": func(r *DiscoveryResult) { r.Discovery.Nodes[0].Name = "api.attacker.example" },
		"bad pin":           func(r *DiscoveryResult) { r.Discovery.Nodes[0].Pins = []string{"abc"} },
		"no pins":           func(r *DiscoveryResult) { r.Discovery.Nodes[0].Pins = nil },
		"unknown outcome":   func(r *DiscoveryResult) { r.Discovery.Outcome = "failed" },
		"no cluster digest": func(r *DiscoveryResult) { r.Cluster.RootCASHA256 = "" },
		"future discovery":  func(r *DiscoveryResult) { r.Discovery.DiscoveredAt = payloadNow.Add(time.Hour) },
		"too many nodes": func(r *DiscoveryResult) {
			for range MaxNodes {
				r.Discovery.Nodes = append(r.Discovery.Nodes, payloadNode("pve9", payloadNow))
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			stored := payloadFor(mergeKey(), "pve2")
			result := mergeResult("pve3")
			mutate(result)
			next, _, err := Merge(stored, mergeKey(), Change{Result: result}, payloadNow)
			if !errors.Is(err, ErrInvalidChange) || next != nil {
				t.Errorf("Merge: %+v, %v, want ErrInvalidChange and nothing to write", next, err)
			}
		})
	}
}

func TestMergeDropsAnAmbiguousResultNodeAlone(t *testing.T) {
	result := mergeResult("pve2", "localhost", "pve3")
	result.Discovery.Nodes[1].Address = "localhost"
	next, drops, err := Merge(nil, mergeKey(), Change{Result: result}, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Discovery.Nodes) != 2 || next.Discovery.Nodes[0].Name != "pve2" || next.Discovery.Nodes[1].Name != "pve3" {
		t.Errorf("nodes %+v", next.Discovery.Nodes)
	}
	if !reflect.DeepEqual(drops, []Drop{{Record: "node 1", Reason: DropAmbiguousName}}) {
		t.Errorf("drops %v", drops)
	}
}

func TestMergeKeepsAStoredIdentityConflict(t *testing.T) {
	stored := payloadFor(mergeKey(), "pve2")
	stored.Cluster.IdentityConflict = true
	if next, _, err := Merge(stored, mergeKey(), Change{Result: mergeResult("pve3")}, payloadNow); !errors.Is(err, ErrIdentityConflict) || next != nil {
		t.Errorf("a result cleared the conflict: %+v, %v", next, err)
	}
	conflicted := mergeResult("pve3")
	conflicted.Cluster.IdentityConflict = true
	next, _, err := Merge(stored, mergeKey(), Change{Result: conflicted}, payloadNow)
	if err != nil || !next.Cluster.IdentityConflict {
		t.Errorf("a result that keeps the conflict: %+v, %v", next, err)
	}
	// Other changes pass over the flag untouched.
	next = mustMerge(t, stored, Change{Marks: map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkDial}}})
	if !next.Cluster.IdentityConflict {
		t.Error("a mark write cleared the conflict")
	}
}

func TestMergeWritesNothingForAnEmptyChangeIntoAMissingFile(t *testing.T) {
	next, drops, err := Merge(nil, mergeKey(), Change{}, payloadNow)
	if next != nil || len(drops) != 0 || err != nil {
		t.Errorf("got %+v, %v, %v", next, drops, err)
	}
	next, _, err = Merge(nil, mergeKey(), Change{Pool: []string{mergeSeed}}, payloadNow)
	if next != nil || err != nil {
		t.Errorf("a pool alone wrote a file: %+v, %v", next, err)
	}
}

func TestMergeTreatsAnInvalidStoredPayloadAsMissing(t *testing.T) {
	stored := payloadFor(mergeKey(), "pve2")
	stored.Discovery.Nodes[0].Name = "api.attacker.example"
	next, drops, err := Merge(stored, mergeKey(), Change{Hint: &ColdStartHint{Name: "pve3", ProbedAt: payloadNow}}, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	if next.Discovery != nil || next.Cluster != nil || next.ColdStart == nil {
		t.Errorf("the invalid file's content survived: %+v", next)
	}
	if !reflect.DeepEqual(drops, []Drop{{Record: "file", Reason: DropUnusable}}) {
		t.Errorf("drops %v", drops)
	}
	// With nothing to record, an invalid stored file is left alone.
	if next, _, err := Merge(stored, mergeKey(), Change{}, payloadNow); next != nil || err != nil {
		t.Errorf("an empty change rewrote an invalid file: %+v, %v", next, err)
	}
	// A stored payload for another key is not merged into this one.
	other := payloadFor(mergeKey(), "pve2")
	other.Key.CredentialID = "ffffffffffffffff"
	next, _, err = Merge(other, mergeKey(), Change{Hint: &ColdStartHint{Name: "pve3", ProbedAt: payloadNow}}, payloadNow)
	if err != nil || next.Discovery != nil || !next.Key.Equal(mergeKey()) {
		t.Errorf("got %+v, %v", next, err)
	}
}

func TestMergeDoesNotChangeItsInputs(t *testing.T) {
	stored := payloadFor(mergeKey(), "pve2")
	stored.Marks = map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkDial}}
	before := stored.clone()
	change := Change{
		Marks: map[string]DownMark{"pve2.dc1.example.com:8006": {DownAt: payloadNow, Class: MarkPin}},
		Hint:  &ColdStartHint{Name: "pve3", ProbedAt: payloadNow},
	}
	next := mustMerge(t, stored, change)
	next.Discovery.Nodes[0].Pins[0] = "changed"
	next.Marks["x"] = DownMark{}
	if !reflect.DeepEqual(stored, before) {
		t.Errorf("Merge changed or shared its stored payload: %+v", stored)
	}
	if len(change.Marks) != 1 {
		t.Error("Merge changed the change")
	}
}

// storeWithNodes writes a discovery for names through UpdatePayload.
func putResult(t *testing.T, s *Store, now time.Time, result *DiscoveryResult) UpdateResult {
	t.Helper()
	res, err := s.UpdatePayload(context.Background(), now, func(*Payload) (Change, error) {
		return Change{Result: result}, nil
	})
	if err != nil || !res.Written {
		t.Fatalf("UpdatePayload: %+v, %v", res, err)
	}
	return res
}

func TestStorePayloadRoundTrip(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	if _, _, err := s.ReadPayload(payloadNow, nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadPayload before any write: %v", err)
	}
	result := mergeResult("pve2", "pve3")
	result.Cluster.ObservedVia = s.Key().Endpoints[0]
	put := putResult(t, s, payloadNow, result)
	if !put.Locked || len(put.Dropped) != 0 {
		t.Errorf("update result %+v", put)
	}
	got, drops, err := s.ReadPayload(payloadNow, nil)
	if err != nil || len(drops) != 0 {
		t.Fatalf("ReadPayload: %v, drops %v", err, drops)
	}
	if !got.Key.Equal(s.Key()) || len(got.Discovery.Nodes) != 2 || got.Cluster.RootCASHA256 != payloadPin("3c") {
		t.Errorf("read %+v", got)
	}
	if got.Discovery.Nodes[0].Name != "pve2" || got.Discovery.Outcome != OutcomeOK || !got.Discovery.Quorate {
		t.Errorf("discovery %+v", got.Discovery)
	}
}

func TestStoreReadPayloadRefusesAnInvalidSignedPayload(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	p := payloadFor(s.Key(), "pve2")
	p.Discovery.Nodes[0].Name = "api.attacker.example"
	signedFile(t, s, p)
	if _, _, err := s.ReadPayload(payloadNow, nil); !errors.Is(err, ErrPayloadInvalid) || !errors.Is(err, ErrFileInvalid) {
		t.Errorf("ReadPayload: %v, want ErrPayloadInvalid", err)
	}
	// The next successful write replaces the file.
	putResult(t, s, payloadNow, mergeResult("pve2"))
	if _, _, err := s.ReadPayload(payloadNow, nil); err != nil {
		t.Errorf("ReadPayload after a rewrite: %v", err)
	}
}

func TestStoreReadPayloadRefusesAHintThatCarriesMore(t *testing.T) {
	for field, value := range map[string]any{
		"address": "pve2.dc1.example.com",
		"port":    8006,
		"pins":    []string{payloadPin("a1")},
		"pin":     payloadPin("a1"),
	} {
		t.Run(field, func(t *testing.T) {
			dir := newStoreDir(t)
			s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
			signedFile(t, s, map[string]any{
				"key": s.Key(),
				"cold_start": map[string]any{
					"name": "pve2", "probed_at": payloadNow.Format(time.RFC3339), field: value,
				},
			})
			if _, _, err := s.ReadPayload(payloadNow, []string{"pve2"}); !errors.Is(err, ErrFileInvalid) {
				t.Errorf("a hint with %s: %v, want ErrFileInvalid", field, err)
			}
		})
	}
}

func TestStoreReadPayloadRefusesUnknownSections(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	signedFile(t, s, map[string]any{"key": s.Key(), "audit": map[string]any{"who": "me"}})
	if _, _, err := s.ReadPayload(payloadNow, nil); !errors.Is(err, ErrFileInvalid) {
		t.Errorf("an unknown member: %v", err)
	}
	signedFile(t, s, map[string]any{
		"key":     s.Key(),
		"cluster": map[string]any{"root_ca_sha256": payloadPin("3c"), "observed_at": payloadNow, "observed_via": "", "identity_conflict": false, "extra": 1},
	})
	if _, _, err := s.ReadPayload(payloadNow, nil); !errors.Is(err, ErrFileInvalid) {
		t.Errorf("an unknown cluster member: %v", err)
	}
}

func TestStoreRestoredOldFileLosesItsUnconfirmedNodes(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	wrote := payloadNow.Add(-26 * time.Hour)
	result := mergeResult("pve2", "pve3")
	for i := range result.Discovery.Nodes {
		result.Discovery.Nodes[i].ConfirmedAt = wrote
	}
	result.Discovery.DiscoveredAt = wrote
	result.Discovery.NextRefreshAfter = wrote.Add(time.Hour)
	result.Cluster.ObservedAt = wrote
	putResult(t, s, wrote, result)
	genuine := readStoreFile(t, filepath.Join(dir, s.FileName()))

	// Put the genuine file back, as someone who can write the directory could.
	writeStoreFile(t, filepath.Join(dir, s.FileName()), genuine, 0o600)
	got, drops, err := s.ReadPayload(payloadNow, nil)
	if err != nil {
		t.Fatalf("a genuine file is still a valid file: %v", err)
	}
	if len(got.Discovery.Nodes) != 0 {
		t.Errorf("nodes %+v survived 26 hours without a confirmation", got.Discovery.Nodes)
	}
	if len(drops) != 2 || drops[0].Reason != DropExpired {
		t.Errorf("drops %v", drops)
	}
	if got.Cluster == nil {
		t.Error("the identity was dropped with the nodes")
	}
	// Read an hour after writing, the same file still holds its nodes.
	if fresh, _, err := s.ReadPayload(wrote.Add(time.Hour), nil); err != nil || len(fresh.Discovery.Nodes) != 2 {
		t.Errorf("an hour after the write: %+v, %v", fresh, err)
	}
}

func TestStoreHintSurvivesAWriterWhoseMapDoesNotNameIt(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	_, err := s.UpdatePayload(context.Background(), payloadNow, func(*Payload) (Change, error) {
		return Change{Hint: &ColdStartHint{Name: "pve3", ProbedAt: payloadNow}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Another entry sharing the key writes a mark. Its az_map plays no part in the write.
	later := payloadNow.Add(10 * time.Second)
	res, err := s.UpdatePayload(context.Background(), later, func(stored *Payload) (Change, error) {
		if stored == nil || stored.ColdStart == nil || stored.ColdStart.Name != "pve3" {
			t.Errorf("build saw %+v, want the stored hint", stored)
		}
		return Change{Marks: map[string]DownMark{s.Key().Endpoints[0]: {DownAt: later, Class: MarkDial}}}, nil
	})
	if err != nil || !res.Written {
		t.Fatalf("UpdatePayload: %+v, %v", res, err)
	}

	// The reader whose map does not name pve3 ignores the hint and keeps the rest.
	got, drops, err := s.ReadPayload(later, []string{"pve2"})
	if err != nil || got.ColdStart != nil || len(got.Marks) != 1 {
		t.Fatalf("reader without the node: %+v, %v", got, err)
	}
	if !reflect.DeepEqual(drops, []Drop{{Record: "cold_start", Reason: DropNotInAZMap}}) {
		t.Errorf("drops %v", drops)
	}
	// The file still holds the hint for a reader whose map names it.
	got, drops, err = s.ReadPayload(later, []string{"pve3", "pve4"})
	if err != nil || got.ColdStart == nil || got.ColdStart.Name != "pve3" || len(drops) != 0 {
		t.Errorf("reader with the node: %+v, %v, %v", got, drops, err)
	}
}

func TestStoreUpdatePayloadHandsBuildTheValidatedPayload(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	_, err := s.UpdatePayload(context.Background(), payloadNow, func(stored *Payload) (Change, error) {
		if stored != nil {
			t.Errorf("build saw %+v for a missing file", stored)
		}
		return Change{Result: mergeResult("pve2")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// An hour later the stored nodes are 1 hour old and the mark is gone.
	_, err = s.UpdatePayload(context.Background(), payloadNow.Add(25*time.Hour), func(stored *Payload) (Change, error) {
		if stored == nil || stored.Discovery == nil || len(stored.Discovery.Nodes) != 0 {
			t.Errorf("build saw %+v, want the payload without its expired node", stored)
		}
		return Change{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// An invalid file reaches build as nil and is replaced.
	p := payloadFor(s.Key(), "pve2")
	p.Discovery.Nodes[0].Port = 0
	signedFile(t, s, p)
	res, err := s.UpdatePayload(context.Background(), payloadNow, func(stored *Payload) (Change, error) {
		if stored != nil {
			t.Errorf("build saw %+v for an invalid file", stored)
		}
		return Change{Hint: &ColdStartHint{Name: "pve2", ProbedAt: payloadNow}}, nil
	})
	if err != nil || !res.Written || !reflect.DeepEqual(res.Dropped, []Drop{{Record: "file", Reason: DropUnusable}}) {
		t.Fatalf("UpdatePayload over an invalid file: %+v, %v", res, err)
	}
	if got, _, err := s.ReadPayload(payloadNow, []string{"pve2"}); err != nil || got.ColdStart == nil {
		t.Errorf("the replacement is not readable: %+v, %v", got, err)
	}
}

func TestStoreUpdatePayloadErrorsWriteNothing(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	boom := errors.New("trust refused")
	res, err := s.UpdatePayload(context.Background(), payloadNow, func(*Payload) (Change, error) { return Change{}, boom })
	if !errors.Is(err, boom) || res.Written || !res.Locked {
		t.Errorf("build error: %+v, %v", res, err)
	}
	bad := mergeResult("pve2")
	bad.Discovery.Nodes[0].Name = "api.attacker.example"
	res, err = s.UpdatePayload(context.Background(), payloadNow, func(*Payload) (Change, error) {
		return Change{Result: bad}, nil
	})
	if !errors.Is(err, ErrInvalidChange) || res.Written {
		t.Errorf("merge error: %+v, %v", res, err)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, s.FileName())); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("a failed update left a file: %v", statErr)
	}
	if _, err := s.UpdatePayload(context.Background(), payloadNow, nil); err == nil {
		t.Error("a nil build function was accepted")
	}
	// An empty change into a missing file writes nothing and still succeeds.
	res, err = s.UpdatePayload(context.Background(), payloadNow, func(*Payload) (Change, error) { return Change{}, nil })
	if err != nil || res.Written || !res.Locked {
		t.Errorf("empty change: %+v, %v", res, err)
	}
}

func TestStoreUpdatePayloadRefusesAClosedStore(t *testing.T) {
	dir := newStoreDir(t)
	s, err := openStoreOn(t, dir, storeTestCredential, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdatePayload(context.Background(), payloadNow, func(*Payload) (Change, error) { return Change{}, nil }); err == nil {
		t.Error("UpdatePayload on a closed store succeeded")
	}
	if _, _, err := s.ReadPayload(payloadNow, nil); err == nil {
		t.Error("ReadPayload on a closed store succeeded")
	}
	var none *Store
	if _, _, err := none.ReadPayload(payloadNow, nil); err == nil {
		t.Error("ReadPayload on a nil store succeeded")
	}
}

func TestStoreUpdatePayloadSkipsTheWriteWhileAnotherProcessHoldsTheLock(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	s.lockWait = 150 * time.Millisecond
	holder := startStoreChild(t, storeChildLock, dir, storeChildLockEnv+"="+s.Key().LockName())
	if got := holder.line(t); got != "locked" {
		t.Fatalf("child said %q, want locked", got)
	}
	called := false
	res, err := s.UpdatePayload(context.Background(), payloadNow, func(*Payload) (Change, error) {
		called = true
		return Change{Result: mergeResult("pve2")}, nil
	})
	if err != nil || res.Locked || res.Written || called {
		t.Errorf("under a held lock: %+v, %v, build called %v", res, err, called)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, s.FileName())); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("a skipped write left a file: %v", statErr)
	}
	holder.finish(t)
}

func TestStoreUpdatePayloadSweepsUnderTheLock(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	old := payloadNow.Add(-2 * time.Hour)
	temp := filepath.Join(dir, ".tmp-0123456789abcdef")
	writeStoreFile(t, temp, []byte("x"), 0o600)
	if err := os.Chtimes(temp, old, old); err != nil {
		t.Fatal(err)
	}
	res := putResult(t, s, payloadNow, mergeResult("pve2"))
	if !reflect.DeepEqual(res.Swept.Removed, []string{".tmp-0123456789abcdef"}) || res.Swept.Err != nil {
		t.Errorf("swept %+v", res.Swept)
	}
	names := storeDirNames(t, dir)
	for _, want := range []string{"secret", "secret.lock", s.FileName(), s.Key().LockName()} {
		found := false
		for _, name := range names {
			found = found || name == want
		}
		if !found {
			t.Errorf("%s is missing from %v", want, names)
		}
	}
}

func TestStoreUpdatePayloadSerializesWriters(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	s.lockWait = 10 * time.Second
	endpoints := []string{"a.dc1.example.com:8006", "b.dc1.example.com:8006", "c.dc1.example.com:8006", "d.dc1.example.com:8006"}
	var wg sync.WaitGroup
	for _, endpoint := range endpoints {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.UpdatePayload(context.Background(), payloadNow, func(*Payload) (Change, error) {
				return Change{Marks: map[string]DownMark{endpoint: {DownAt: payloadNow, Class: MarkDial}}, Pool: endpoints}, nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, _, err := s.ReadPayload(payloadNow, nil)
	if err != nil || len(got.Marks) != len(endpoints) {
		t.Errorf("marks %v, err %v, want every writer's mark", got, err)
	}
}
