package endpointcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
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
		{
			"an equal time takes the writer's mark",
			DownMark{DownAt: payloadNow.Add(-30 * time.Second), Class: MarkPin},
			MarkPin,
		},
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
	if len(next.Marks) != 2 || next.Marks["pve2.dc1.example.com:8006"].Class != MarkPin ||
		next.Marks[mergeSeed].Class != MarkDial {
		t.Errorf("marks %v", next.Marks)
	}
}

func ptr[T any](v T) *T { return &v }

func TestMergeDropsMarksThatExpired(t *testing.T) {
	stored := &Payload{Key: mergeKey(), Marks: map[string]DownMark{
		mergeSeed: {DownAt: payloadNow.Add(-markTTL - time.Second), Class: MarkDial},
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
		mergeSeed: true, "pve1b.dc1.example.com:8006": true,
		"pve2.dc1.example.com:8006": true, "cand.dc1.example.com:8006": true,
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
	expired := &Payload{
		Key:       mergeKey(),
		ColdStart: &ColdStartHint{Name: "pve3", ProbedAt: payloadNow.Add(-2 * time.Hour)},
	}
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
		"future discovery": func(r *DiscoveryResult) {
			r.Discovery.DiscoveredAt = payloadNow.Add(
				time.Hour,
			)
		},
		"nodes under standalone":         func(r *DiscoveryResult) { r.Discovery.Outcome = OutcomeStandalone },
		"nodes under no_candidates":      func(r *DiscoveryResult) { r.Discovery.Outcome = OutcomeNoCandidates },
		"observed_via outside the key":   func(r *DiscoveryResult) { r.Cluster.ObservedVia = "evil.example:1" },
		"observed_via not an endpoint":   func(r *DiscoveryResult) { r.Cluster.ObservedVia = ".:8006" },
		"a conflict carried in a result": func(r *DiscoveryResult) { r.Cluster.IdentityConflict = true },
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
	if len(next.Discovery.Nodes) != 2 || next.Discovery.Nodes[0].Name != "pve2" ||
		next.Discovery.Nodes[1].Name != "pve3" {
		t.Errorf("nodes %+v", next.Discovery.Nodes)
	}
	if !reflect.DeepEqual(drops, []Drop{{Record: "node 1", Reason: DropAmbiguousName}}) {
		t.Errorf("drops %v", drops)
	}
}

func TestMergeKeepsAStoredIdentityConflict(t *testing.T) {
	stored := payloadFor(mergeKey(), "pve2")
	stored.Cluster.IdentityConflict = true
	if next, _, err := Merge(
		stored,
		mergeKey(),
		Change{Result: mergeResult("pve3")},
		payloadNow,
	); !errors.Is(err, ErrIdentityConflict) || next != nil {
		t.Errorf("a result cleared the conflict: %+v, %v", next, err)
	}
	// A result that keeps the flag, or that carries the stored identity and
	// nodes unchanged, is refused too, so nothing replaces a conflicted file.
	conflicted := mergeResult("pve3")
	conflicted.Cluster.IdentityConflict = true
	if next, _, err := Merge(
		stored,
		mergeKey(),
		Change{Result: conflicted},
		payloadNow,
	); !errors.Is(err, ErrIdentityConflict) || next != nil {
		t.Errorf("a result that keeps the conflict: %+v, %v", next, err)
	}
	if next, _, err := Merge(
		stored,
		mergeKey(),
		Change{Result: mergeResult("pve2")},
		payloadNow,
	); !errors.Is(err, ErrIdentityConflict) || next != nil {
		t.Errorf("a result with the stored nodes: %+v, %v", next, err)
	}
	// Other changes pass over the flag and the stored nodes untouched.
	next := mustMerge(t, stored, Change{Marks: map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkDial}}})
	if !next.Cluster.IdentityConflict || !reflect.DeepEqual(next.Discovery, stored.Discovery) {
		t.Errorf("a mark write changed the conflicted file: %+v", next)
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
	next, drops, err := Merge(
		stored,
		mergeKey(),
		Change{Hint: &ColdStartHint{Name: "pve3", ProbedAt: payloadNow}},
		payloadNow,
	)
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
	next, _, err = Merge(
		other,
		mergeKey(),
		Change{Hint: &ColdStartHint{Name: "pve3", ProbedAt: payloadNow}},
		payloadNow,
	)
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
	res, err := s.UpdatePayload(context.Background(), now, func(Stored) (Change, error) {
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
	if _, _, err := s.ReadPayload(
		payloadNow,
		nil,
	); !errors.Is(err, ErrPayloadInvalid) || !errors.Is(err, ErrFileInvalid) {
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
		"key": s.Key(),
		"cluster": map[string]any{
			"root_ca_sha256":    payloadPin("3c"),
			"observed_at":       payloadNow,
			"observed_via":      "",
			"identity_conflict": false,
			"extra":             1,
		},
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
	_, err := s.UpdatePayload(context.Background(), payloadNow, func(Stored) (Change, error) {
		return Change{Hint: &ColdStartHint{Name: "pve3", ProbedAt: payloadNow}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Another entry sharing the key writes a mark. Its az_map plays no part in the write.
	later := payloadNow.Add(10 * time.Second)
	res, err := s.UpdatePayload(context.Background(), later, func(stored Stored) (Change, error) {
		if stored.Usable == nil || stored.Usable.ColdStart == nil || stored.Usable.ColdStart.Name != "pve3" {
			t.Errorf("build saw %+v, want the stored hint", stored.Usable)
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
	_, err := s.UpdatePayload(context.Background(), payloadNow, func(stored Stored) (Change, error) {
		if stored.Usable != nil || stored.Known != nil {
			t.Errorf("build saw %+v for a missing file", stored)
		}
		return Change{Result: mergeResult("pve2")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// 25 hours later the stored node is past its bound. The usable view leaves
	// it out, and the known view still holds it with its pins.
	_, err = s.UpdatePayload(context.Background(), payloadNow.Add(25*time.Hour), func(stored Stored) (Change, error) {
		if stored.Usable == nil || stored.Usable.Discovery == nil || len(stored.Usable.Discovery.Nodes) != 0 {
			t.Errorf("build saw %+v, want the payload without its expired node", stored.Usable)
		}
		if len(stored.Known) != 1 || stored.Known[0].Name != "pve2" || len(stored.Known[0].Pins) != 2 {
			t.Errorf("build knew %+v, want pve2 and its pins", stored.Known)
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
	res, err := s.UpdatePayload(context.Background(), payloadNow, func(stored Stored) (Change, error) {
		if stored.Usable != nil || stored.Known != nil {
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
	res, err := s.UpdatePayload(
		context.Background(),
		payloadNow,
		func(Stored) (Change, error) { return Change{}, boom },
	)
	if !errors.Is(err, boom) || res.Written || !res.Locked {
		t.Errorf("build error: %+v, %v", res, err)
	}
	bad := mergeResult("pve2")
	bad.Discovery.Nodes[0].Name = "api.attacker.example"
	res, err = s.UpdatePayload(context.Background(), payloadNow, func(Stored) (Change, error) {
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
	res, err = s.UpdatePayload(context.Background(), payloadNow, func(Stored) (Change, error) { return Change{}, nil })
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
	if _, err := s.UpdatePayload(
		context.Background(),
		payloadNow,
		func(Stored) (Change, error) { return Change{}, nil },
	); err == nil {
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
	res, err := s.UpdatePayload(context.Background(), payloadNow, func(Stored) (Change, error) {
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
	endpoints := []string{
		"a.dc1.example.com:8006",
		"b.dc1.example.com:8006",
		"c.dc1.example.com:8006",
		"d.dc1.example.com:8006",
	}
	var wg sync.WaitGroup
	for _, endpoint := range endpoints {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.UpdatePayload(context.Background(), payloadNow, func(Stored) (Change, error) {
				return Change{
					Marks: map[string]DownMark{endpoint: {DownAt: payloadNow, Class: MarkDial}},
					Pool:  endpoints,
				}, nil
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

// unverifiedKey is mergeKey under verify_ssl false.
func unverifiedKey() Key {
	key := mergeKey()
	key.VerifySSL = false
	return key
}

func TestMergeRefusesAResultAndAHintTogether(t *testing.T) {
	both := Change{Result: mergeResult("pve3"), Hint: &ColdStartHint{Name: "pve3", ProbedAt: payloadNow}}
	conflicted := payloadFor(unverifiedKey(), "pve2")
	conflicted.Cluster.IdentityConflict = true
	cases := map[string]struct {
		stored *Payload
		key    Key
	}{
		"no file":         {nil, mergeKey()},
		"a file":          {payloadFor(mergeKey(), "pve2"), mergeKey()},
		"a conflict file": {conflicted, unverifiedKey()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			next, _, err := Merge(tc.stored, tc.key, both, payloadNow)
			if !errors.Is(err, ErrInvalidChange) || next != nil {
				t.Errorf("Merge: %+v, %v, want ErrInvalidChange and nothing to write", next, err)
			}
		})
	}
}

func TestMergeRecordsAConflictAndNothingElse(t *testing.T) {
	key := unverifiedKey()
	stored := payloadFor(key, "pve2", "pve3")
	stored.Discovery.Nodes[1].ConfirmedAt = payloadNow.Add(-48 * time.Hour)
	stored.Marks = map[string]DownMark{mergeSeed: {DownAt: payloadNow.Add(-10 * time.Second), Class: MarkDial}}

	next, drops, err := Merge(stored, key, Change{Conflict: true}, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	want := stored.clone()
	want.Cluster.IdentityConflict = true
	if !reflect.DeepEqual(next, want) {
		t.Errorf("Merge changed more than the flag:\n got %+v\nwant %+v", next, want)
	}
	if !reflect.DeepEqual(drops, []Drop{{Record: "node 1", Reason: DropExpired}}) {
		t.Errorf("drops %v", drops)
	}
	// The recorded conflict refuses every later result and hides the nodes.
	if again, _, err := Merge(
		next,
		key,
		Change{Result: mergeResult("pve2")},
		payloadNow,
	); !errors.Is(err, ErrIdentityConflict) || again != nil {
		t.Errorf("a result over the recorded conflict: %+v, %v", again, err)
	}
	if got, _ := validatedPayload(t, next); len(got.Discovery.Nodes) != 0 || !got.Cluster.IdentityConflict {
		t.Errorf("a reader of the recorded conflict: %+v", got)
	}

	refused := map[string]struct {
		stored *Payload
		key    Key
		change Change
	}{
		"with a result":    {stored, key, Change{Conflict: true, Result: mergeResult("pve2")}},
		"under verify_ssl": {payloadFor(mergeKey(), "pve2"), mergeKey(), Change{Conflict: true}},
		"without a file":   {nil, key, Change{Conflict: true}},
		"without identity": {&Payload{Key: key, Marks: stored.Marks}, key, Change{Conflict: true}},
		"over invalid file": {
			&Payload{Key: key, Discovery: &DiscoverySection{Outcome: "x"}},
			key,
			Change{Conflict: true},
		},
	}
	for name, tc := range refused {
		t.Run(name, func(t *testing.T) {
			next, _, err := Merge(tc.stored, tc.key, tc.change, payloadNow)
			if !errors.Is(err, ErrInvalidChange) || next != nil {
				t.Errorf("Merge: %+v, %v, want ErrInvalidChange", next, err)
			}
		})
	}
}

func TestMergeRefusesAnIdentityChangeUnderVerifySSLFalse(t *testing.T) {
	key := unverifiedKey()
	stored := payloadFor(key, "pve2")
	changed := mergeResult("pve2")
	changed.Cluster.RootCASHA256 = payloadPin("ff")
	changed.Discovery.Nodes[0].Pins = []string{payloadPin("ee")}

	next, _, err := Merge(stored, key, Change{Result: changed}, payloadNow)
	var refusal *TrustRefusalError
	if !errors.As(err, &refusal) || !refusal.IdentityChanged || len(refusal.Nodes) != 0 || next != nil {
		t.Fatalf("Merge: %+v, %v, want an identity refusal and nothing to write", next, err)
	}
	if !errors.Is(err, ErrTrustRefused) || !errors.Is(err, ErrIdentityChanged) || errors.Is(err, ErrPinsChanged) ||
		errors.Is(err, ErrInvalidChange) {
		t.Errorf("%v matches the wrong sentinels", err)
	}
	if strings.Contains(err.Error(), payloadPin("ff")) || strings.Contains(err.Error(), "pve2") {
		t.Errorf("error %v echoes a value", err)
	}
	// The caller records it as a conflict.
	recorded, _, err := Merge(stored, key, Change{Conflict: refusal.IdentityChanged}, payloadNow)
	if err != nil || !recorded.Cluster.IdentityConflict || recorded.Cluster.RootCASHA256 != payloadPin("3c") {
		t.Errorf("recording the refusal: %+v, %v", recorded, err)
	}

	// An identity seen for the first time is recorded, with or without a file.
	for name, first := range map[string]*Payload{
		"no file":        nil,
		"marks only":     {Key: key, Marks: map[string]DownMark{mergeSeed: {DownAt: payloadNow, Class: MarkDial}}},
		"invalid stored": {Key: key, Cluster: &ClusterIdentity{RootCASHA256: "x"}},
	} {
		if next, _, err := Merge(
			first,
			key,
			Change{Result: changed},
			payloadNow,
		); err != nil || next.Cluster.RootCASHA256 != payloadPin("ff") {
			t.Errorf("%s: first use: %+v, %v", name, next, err)
		}
	}
	// Under verify_ssl true the changed identity replaces the stored one.
	next, _, err = Merge(payloadFor(mergeKey(), "pve2"), mergeKey(), Change{Result: changed}, payloadNow)
	if err != nil || next.Cluster.RootCASHA256 != payloadPin("ff") ||
		next.Discovery.Nodes[0].Pins[0] != payloadPin("ee") {
		t.Errorf("verify_ssl true: %+v, %v", next, err)
	}
}

func TestMergeRefusesAPinChangeForAKnownNodeUnderVerifySSLFalse(t *testing.T) {
	key := unverifiedKey()
	known := map[string]func(p *Payload){
		"a fresh node": func(*Payload) {},
		"a node past 24 hours": func(p *Payload) {
			p.Discovery.Nodes[0].ConfirmedAt = payloadNow.Add(
				-48 * time.Hour,
			)
		},
		"a node confirmed ahead": func(p *Payload) { p.Discovery.Nodes[0].ConfirmedAt = payloadNow.Add(time.Hour) },
		"a section dated ahead":  func(p *Payload) { p.Discovery.DiscoveredAt = payloadNow.Add(time.Hour) },
		"a node that no longer derives": func(p *Payload) {
			p.Discovery.Nodes[0].Address = "pve2.attacker.example"
		},
		"a name in another case": func(p *Payload) { p.Discovery.Nodes[0].Name = "PVE2" },
	}
	for name, mutate := range known {
		t.Run(name, func(t *testing.T) {
			stored := payloadFor(key, "pve2", "pve3")
			mutate(stored)
			result := mergeResult("pve4", "pve2", "pve3")
			result.Discovery.Nodes[1].Pins = []string{payloadPin("a1"), payloadPin("ff")}
			next, _, err := Merge(stored, key, Change{Result: result}, payloadNow)
			var refusal *TrustRefusalError
			if !errors.As(err, &refusal) || refusal.IdentityChanged ||
				!reflect.DeepEqual(refusal.Nodes, []string{"pve2"}) || next != nil {
				t.Fatalf("Merge: %+v, %v, want a pin refusal for pve2", next, err)
			}
			if !errors.Is(err, ErrTrustRefused) || !errors.Is(err, ErrPinsChanged) ||
				errors.Is(err, ErrIdentityChanged) {
				t.Errorf("%v matches the wrong sentinels", err)
			}
			if strings.Contains(err.Error(), "pve2") {
				t.Errorf("error %v echoes a node name", err)
			}
		})
	}

	// The same pins in another order and case, a new node, and a known node
	// the result leaves out are all accepted.
	stored := payloadFor(key, "pve2", "pve3")
	stored.Discovery.Nodes[0].ConfirmedAt = payloadNow.Add(-48 * time.Hour)
	result := mergeResult("pve2", "pve5")
	result.Discovery.Nodes[0].Pins = []string{strings.ToUpper(payloadPin("9e")), payloadPin("a1"), payloadPin("9E")}
	result.Discovery.Nodes[1].Pins = []string{payloadPin("55")}
	next, _, err := Merge(stored, key, Change{Result: result}, payloadNow)
	if err != nil || len(next.Discovery.Nodes) != 2 || next.Discovery.Nodes[1].Pins[0] != payloadPin("55") {
		t.Errorf("unchanged pins and a new node: %+v, %v", next, err)
	}
	// Under verify_ssl true a changed pin set replaces the old one.
	verified := payloadFor(mergeKey(), "pve2")
	changed := mergeResult("pve2")
	changed.Discovery.Nodes[0].Pins = []string{payloadPin("ff")}
	if next, _, err := Merge(verified, mergeKey(), Change{Result: changed}, payloadNow); err != nil ||
		!reflect.DeepEqual(next.Discovery.Nodes[0].Pins, []string{payloadPin("ff")}) {
		t.Errorf("verify_ssl true: %+v, %v", next, err)
	}
}

func TestMergeDropsAResultNodeTheSeedDoesNotDerive(t *testing.T) {
	result := mergeResult("pve2", "pve3")
	result.Discovery.Nodes[1].Address = "pve3.attacker.example"
	next, drops, err := Merge(nil, mergeKey(), Change{Result: result}, payloadNow)
	if err != nil || len(next.Discovery.Nodes) != 1 || next.Discovery.Nodes[0].Name != "pve2" {
		t.Fatalf("Merge: %+v, %v", next, err)
	}
	if !reflect.DeepEqual(drops, []Drop{{Record: "node 1", Reason: DropNotDerived}}) {
		t.Errorf("drops %v", drops)
	}
}

func TestMergeWritesTheStoredDiscoveryBackUnchanged(t *testing.T) {
	for _, step := range []time.Duration{-2 * time.Minute, 25 * time.Hour} {
		t.Run(step.String(), func(t *testing.T) {
			stored := payloadFor(mergeKey(), "pve2", "pve3", "localhost")
			stored.Discovery.Nodes[0].Address = "PVE2.dc1.example.com"
			stored.Discovery.Nodes[1].Pins = []string{strings.ToUpper(payloadPin("ab"))}
			stored.Discovery.Nodes[1].ConfirmedAt = payloadNow.Add(-25 * time.Hour)
			stored.Discovery.Nodes[2].Address = "localhost"
			stored.Discovery.NextRefreshAfter = payloadNow.Add(24 * time.Hour)
			before, err := json.Marshal(stored.Discovery)
			if err != nil {
				t.Fatal(err)
			}
			clock := payloadNow.Add(step)
			next, drops, err := Merge(stored, mergeKey(), Change{
				Marks: map[string]DownMark{
					mergeSeed:                   {DownAt: clock, Class: MarkDial},
					"pve2.dc1.example.com:8006": {DownAt: clock, Class: MarkPin},
				},
			}, clock)
			if err != nil || len(drops) == 0 {
				t.Fatalf("Merge: %v, drops %v", err, drops)
			}
			// The mark of a node written back in its stored spelling is kept.
			if len(next.Marks) != 2 {
				t.Errorf("marks %v, want the seed's and the node's", next.Marks)
			}
			after, err := json.Marshal(next.Discovery)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Errorf("a mark write changed the discovery section:\n got %s\nwant %s", after, before)
			}
		})
	}
}

func TestMergeCapsTheMarksOfOtherEndpoints(t *testing.T) {
	key := mergeKey()
	key.Endpoints = []string{mergeSeed, "pve1b.dc1.example.com:8006"}
	marks := map[string]DownMark{
		mergeSeed:                    {DownAt: payloadNow.Add(-100 * time.Second), Class: MarkDial},
		"pve1b.dc1.example.com:8006": {DownAt: payloadNow.Add(-100 * time.Second), Class: MarkDial},
	}
	pool := make([]string, 0, 70)
	for i := range 70 {
		endpoint := fmt.Sprintf("cand%02d.dc1.example.com:8006", i)
		pool = append(pool, endpoint)
		// The last seven share one time, and the endpoint that sorts first wins.
		marks[endpoint] = DownMark{DownAt: payloadNow.Add(-time.Duration(min(i, 63)) * time.Second), Class: MarkDNS}
	}
	next, _, err := Merge(nil, key, Change{Marks: marks, Pool: pool}, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Marks) != 2+MaxOtherMarks {
		t.Fatalf("%d marks, want the two configured endpoints and %d others", len(next.Marks), MaxOtherMarks)
	}
	for _, endpoint := range key.Endpoints {
		if _, ok := next.Marks[endpoint]; !ok {
			t.Errorf("the oldest mark, for configured %s, was dropped", endpoint)
		}
	}
	for i := range 70 {
		endpoint := fmt.Sprintf("cand%02d.dc1.example.com:8006", i)
		if _, ok := next.Marks[endpoint]; ok != (i < 64) {
			t.Errorf("mark for %s kept %v", endpoint, ok)
		}
	}
	// A merge under the cap keeps every mark.
	small := map[string]DownMark{
		mergeSeed:                     marks[mergeSeed],
		"cand00.dc1.example.com:8006": marks["cand00.dc1.example.com:8006"],
	}
	if next, _, err := Merge(
		nil,
		key,
		Change{Marks: small, Pool: pool},
		payloadNow,
	); err != nil || len(next.Marks) != 2 {
		t.Errorf("under the cap: %+v, %v", next, err)
	}
}

// openUnverifiedStore opens a store on dir for storeTestConfig under
// verify_ssl false, and closes it when the test ends.
func openUnverifiedStore(t *testing.T, dir string) *Store {
	t.Helper()
	cfg := storeTestConfig()
	cfg.VerifySSL = false
	return openStoreFor(t, dir, cfg)
}

// openStoreFor opens a store on dir for cfg, and closes it when the test
// ends.
func openStoreFor(t *testing.T, dir string, cfg KeyConfig) *Store {
	t.Helper()
	s, err := OpenStore(context.Background(), storeTestRoot(t, dir), cfg, storeTestCredential, StoreOptions{})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestStoreMarkWriteKeepsTheDiscoveryAfterTheClockMoves(t *testing.T) {
	for _, step := range []time.Duration{-2 * time.Minute, 25 * time.Hour} {
		t.Run(step.String(), func(t *testing.T) {
			dir := newStoreDir(t)
			s := openUnverifiedStore(t, dir)
			planted := payloadFor(s.Key(), "pve2", "pve3")
			planted.Cluster.ObservedVia = s.Key().Endpoints[0]
			planted.Discovery.Nodes[1].ConfirmedAt = payloadNow.Add(-25 * time.Hour)
			signedFile(t, s, planted)

			// A mark write at the moved clock, which sees the discovery as
			// ahead after the step back and every node as expired after 25
			// hours.
			clock := payloadNow.Add(step)
			res, err := s.UpdatePayload(context.Background(), clock, func(stored Stored) (Change, error) {
				if len(stored.Known) != 2 {
					t.Errorf("build knew %+v, want both nodes", stored.Known)
				}
				return Change{Marks: map[string]DownMark{s.Key().Endpoints[0]: {DownAt: clock, Class: MarkDial}}}, nil
			})
			if err != nil || !res.Written {
				t.Fatalf("mark write: %+v, %v", res, err)
			}
			var raw Payload
			if err := s.Read(&raw); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(raw.Discovery, planted.Discovery) {
				t.Errorf("the file's discovery changed:\n got %+v\nwant %+v", raw.Discovery, planted.Discovery)
			}
			got, _, err := s.ReadPayload(payloadNow, nil)
			if err != nil || got.Discovery == nil || len(got.Discovery.Nodes) != 1 ||
				got.Discovery.Nodes[0].Name != "pve2" {
				t.Fatalf("a read at the sane clock: %+v, %v", got, err)
			}

			// A discovery that changes the expired node's pins is refused, and
			// one that keeps them is accepted.
			later := payloadNow.Add(max(step, 0))
			changed := mergeResult("pve2", "pve3")
			changed.Cluster.ObservedVia = s.Key().Endpoints[0]
			changed.Discovery.DiscoveredAt = later
			changed.Discovery.Nodes[1].Pins = []string{payloadPin("ff")}
			for i := range changed.Discovery.Nodes {
				changed.Discovery.Nodes[i].ConfirmedAt = later
			}
			res, err = s.UpdatePayload(context.Background(), later, func(Stored) (Change, error) {
				return Change{Result: changed}, nil
			})
			if !errors.Is(err, ErrPinsChanged) || res.Written {
				t.Fatalf("a pin change for the expired node: %+v, %v", res, err)
			}
			kept := changed
			kept.Discovery.Nodes = append([]DiscoveredNode(nil), changed.Discovery.Nodes...)
			kept.Discovery.Nodes[1].Pins = []string{payloadPin("9e"), payloadPin("a1")}
			if res, err := s.UpdatePayload(context.Background(), later, func(Stored) (Change, error) {
				return Change{Result: kept}, nil
			}); err != nil || !res.Written {
				t.Errorf("a discovery with the stored pins: %+v, %v", res, err)
			}
		})
	}
}

func TestStoreSweepSparesTheWritersOwnKeyFile(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	putResult(t, s, payloadNow, mergeResult("pve2"))
	old := payloadNow.Add(-31 * 24 * time.Hour)
	other := "other.example.com-0123456789abcdef.json"
	writeStoreFile(t, filepath.Join(dir, other), []byte("{}"), 0o600)
	for _, name := range []string{s.FileName(), other} {
		if err := os.Chtimes(filepath.Join(dir, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	// build fails, so nothing is written and the key file stays only if the
	// sweep spared it.
	boom := errors.New("no change")
	res, err := s.UpdatePayload(
		context.Background(),
		payloadNow,
		func(Stored) (Change, error) { return Change{}, boom },
	)
	if !errors.Is(err, boom) || res.Written {
		t.Fatalf("UpdatePayload: %+v, %v", res, err)
	}
	if !reflect.DeepEqual(res.Swept.Removed, []string{other}) {
		t.Errorf("swept %v, want only the other key's file", res.Swept.Removed)
	}
	if _, err := os.Lstat(filepath.Join(dir, s.FileName())); err != nil {
		t.Errorf("the writer's own key file is gone: %v", err)
	}
}

func TestStoreWritesTheLargestLegalFile(t *testing.T) {
	// Every derived host is 253 characters, the longest DNS allows, which is
	// a node name of 63 characters under a 189-character registrable domain.
	// Every endpoint, node, and extra cold-start candidate is marked, so the
	// cap keeps 64 marks beside the configured ones. The key holds five
	// configured endpoints of 253 characters, the most that fit beside the
	// rest, because the key and its marks are the one part of the file whose
	// size the cache does not bound.
	const port = 65535
	domain := strings.Repeat("d", 63) + "." + strings.Repeat("e", 63) + "." + strings.Repeat("f", 57) + ".com"
	label := func(prefix string, i int) string {
		name := fmt.Sprintf("%s%02d", prefix, i)
		return name + strings.Repeat("x", MaxNodeNameLength-len(name))
	}
	endpoints := make([]string, 0, 5)
	endpoints = append(endpoints, net.JoinHostPort("pve1."+domain, strconv.Itoa(port)))
	for i := range 4 {
		endpoints = append(endpoints, net.JoinHostPort(label("c", i)+"."+domain, strconv.Itoa(port)))
	}
	cfg := storeTestConfig()
	cfg.Endpoints = endpoints
	dir := newStoreDir(t)
	s := openStoreFor(t, dir, cfg)

	// Every time carries nanoseconds, which is its longest spelling.
	at := payloadNow.Add(-123456789)
	result := &DiscoveryResult{Cluster: payloadCluster(at), Discovery: payloadDiscovery()}
	result.Cluster.ObservedVia = endpoints[0]
	result.Discovery.DiscoveredAt = at
	result.Discovery.NextRefreshAfter = at.Add(time.Hour)
	result.Discovery.Outcome = OutcomeNoStatus
	marks := make(map[string]DownMark)
	pool := make([]string, 0, 8)
	for _, endpoint := range endpoints {
		marks[endpoint] = DownMark{DownAt: at, Class: MarkTLSHandshake}
	}
	for i := range MaxNodes {
		name := label("n", i)
		address, err := DeriveAddress(endpoints[0], name)
		if err != nil {
			t.Fatal(err)
		}
		host, _, _ := net.SplitHostPort(address)
		if len(host) != 253 {
			t.Fatalf("derived host is %d characters, want 253", len(host))
		}
		result.Discovery.Nodes = append(result.Discovery.Nodes, DiscoveredNode{
			Name: name, Address: host, Port: port, Online: false, ConfirmedAt: at,
			Pins: []string{payloadPin(fmt.Sprintf("%02x", i)), payloadPin("a1"), payloadPin("9e")},
		})
		marks[address] = DownMark{DownAt: at, Class: MarkTLSHandshake}
	}
	for i := range 8 {
		endpoint := net.JoinHostPort(label("z", i)+"."+domain, strconv.Itoa(port))
		pool = append(pool, endpoint)
		marks[endpoint] = DownMark{DownAt: at.Add(time.Nanosecond), Class: MarkTLSHandshake}
	}

	res, err := s.UpdatePayload(context.Background(), payloadNow, func(Stored) (Change, error) {
		return Change{Result: result, Marks: marks, Pool: pool}, nil
	})
	if err != nil || !res.Written || len(res.Dropped) != 0 {
		t.Fatalf("UpdatePayload: %+v, %v", res, err)
	}
	info, err := os.Stat(filepath.Join(dir, s.FileName()))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 64*1024 {
		t.Errorf("the file is %d bytes, more than 64 KiB", info.Size())
	}
	got, drops, err := s.ReadPayload(payloadNow, nil)
	if err != nil || len(drops) != 0 {
		t.Fatalf("ReadPayload: %v, drops %v", err, drops)
	}
	if len(got.Discovery.Nodes) != MaxNodes || len(got.Marks) != len(endpoints)+MaxOtherMarks {
		t.Errorf(
			"%d nodes and %d marks, want %d and %d",
			len(got.Discovery.Nodes),
			len(got.Marks),
			MaxNodes,
			len(endpoints)+MaxOtherMarks,
		)
	}
}

// TestMergeAcceptsAPinSetThatKeepsEveryStoredPin shows the verify_ssl false
// pin rule for a known node. A result that keeps every stored pin, with or
// without more, is accepted, and one that leaves out any stored pin is
// refused for that node, whether the sets shrink, overlap, or share nothing.
// Under verify_ssl true each of them replaces the stored set.
func TestMergeAcceptsAPinSetThatKeepsEveryStoredPin(t *testing.T) {
	cases := map[string]struct {
		pins    []string
		refused bool
	}{
		"the same pins":          {[]string{payloadPin("9e"), payloadPin("a1")}, false},
		"a pin set that grows":   {[]string{payloadPin("a1"), payloadPin("9e"), payloadPin("55")}, false},
		"a pin set that shrinks": {[]string{payloadPin("a1")}, true},
		"a partial overlap":      {[]string{payloadPin("a1"), payloadPin("55")}, true},
		"a disjoint pin set":     {[]string{payloadPin("55"), payloadPin("56")}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for _, verify := range []bool{false, true} {
				key := mergeKey()
				key.VerifySSL = verify
				stored := payloadFor(key, "pve2", "pve3")
				result := mergeResult("pve2", "pve3")
				result.Discovery.Nodes[0].Pins = tc.pins
				next, _, err := Merge(stored, key, Change{Result: result}, payloadNow)
				var refusal *TrustRefusalError
				switch {
				case tc.refused && !verify:
					if !errors.As(err, &refusal) || !reflect.DeepEqual(refusal.Nodes, []string{"pve2"}) || next != nil {
						t.Errorf("verify_ssl false: %+v, %v, want a pin refusal for pve2", next, err)
					}
				case err != nil:
					t.Errorf("verify_ssl %v: %v", verify, err)
				case len(next.Discovery.Nodes[0].Pins) != len(tc.pins):
					t.Errorf("verify_ssl %v: pins %v, want %v", verify, next.Discovery.Nodes[0].Pins, tc.pins)
				}
			}
		})
	}
}

// TestMergeKeepsAKnownNodeWithItsStoredConfirmation shows that under
// verify_ssl false a result that carries a known node with its stored pins and
// its stored confirmation keeps that node in the file even when the
// confirmation is past NodeTTL or ahead, so the node stays known while a reader
// no longer loads it. Any other node past NodeTTL is dropped from the result.
// The stored copy of such a node is reported as a read drop either way.
func TestMergeKeepsAKnownNodeWithItsStoredConfirmation(t *testing.T) {
	old := payloadNow.Add(-48 * time.Hour)
	ahead := payloadNow.Add(time.Hour)
	cases := map[string]struct {
		verify    bool
		stored    time.Time
		confirmed time.Time
		pins      []string
		kept      bool
	}{
		"stored pins and confirmation past 24 hours": {stored: old, confirmed: old, kept: true},
		"stored pins and a confirmation ahead":       {stored: ahead, confirmed: ahead, kept: true},
		"stored pins and another old confirmation":   {stored: old, confirmed: old.Add(time.Hour)},
		"grown pins and the stored confirmation": {
			stored: old, confirmed: old, pins: []string{payloadPin("a1"), payloadPin("9e"), payloadPin("55")},
		},
		"under verify_ssl true": {verify: true, stored: old, confirmed: old},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			key := mergeKey()
			key.VerifySSL = tc.verify
			stored := payloadFor(key, "pve2", "pve3")
			stored.Discovery.Nodes[0].ConfirmedAt = tc.stored
			result := mergeResult("pve2", "pve3")
			result.Discovery.Nodes[0].ConfirmedAt = tc.confirmed
			if tc.pins != nil {
				result.Discovery.Nodes[0].Pins = tc.pins
			}
			next, drops, err := Merge(stored, key, Change{Result: result}, payloadNow)
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			names := trustNodeNames(next)
			if tc.kept {
				if !reflect.DeepEqual(names, []string{"pve2", "pve3"}) || len(drops) != 0 ||
					!next.Discovery.Nodes[0].ConfirmedAt.Equal(tc.stored) {
					t.Fatalf("nodes %v drops %v, want pve2 kept with its stored confirmation and no drop", names, drops)
				}
				state, _ := readState(next, key, payloadNow)
				if got := trustNodeNames(
					state.usable,
				); !reflect.DeepEqual(got, []string{"pve3"}) || len(state.known) != 2 {
					t.Errorf(
						"a reader loads %v and knows %d nodes, want pve3 loaded and both known",
						got,
						len(state.known),
					)
				}
				return
			}
			if !reflect.DeepEqual(names, []string{"pve3"}) || len(drops) != 2 || drops[1].Record != "node 0" {
				t.Errorf("nodes %v drops %v, want pve2 dropped", names, drops)
			}
		})
	}
	// A node the file does not know gets no such exemption.
	result := mergeResult("pve4")
	result.Discovery.Nodes[0].ConfirmedAt = old
	next, drops, err := Merge(payloadFor(unverifiedKey(), "pve2"), unverifiedKey(), Change{Result: result}, payloadNow)
	if err != nil || len(next.Discovery.Nodes) != 0 ||
		!reflect.DeepEqual(drops, []Drop{{Record: "node 0", Reason: DropExpired}}) {
		t.Errorf("an unknown node past 24 hours: %+v, %v, %v", next, drops, err)
	}
}

// TestMergeRecordsAnAttempt shows that an attempt moves the stored discovery
// section's two times and nothing else, while a conflict stands too, and that
// the attempt's own rules refuse what they must.
func TestMergeRecordsAnAttempt(t *testing.T) {
	key := unverifiedKey()
	stored := payloadFor(key, "pve2", "pve3")
	stored.Discovery.Nodes[1].ConfirmedAt = payloadNow.Add(-48 * time.Hour)
	stored.Discovery.Quorate = false
	stored.Discovery.Outcome = OutcomeNoStatus
	stored.Marks = map[string]DownMark{mergeSeed: {DownAt: payloadNow.Add(-10 * time.Second), Class: MarkDial}}
	attempt := &DiscoveryAttempt{At: payloadNow, NextRefreshAfter: payloadNow.Add(MaxRefreshWait)}
	moved := func(p *Payload) *Payload {
		want := p.clone()
		want.Discovery.DiscoveredAt, want.Discovery.NextRefreshAfter = payloadNow, payloadNow.Add(MaxRefreshWait)
		return want
	}

	next, drops, err := Merge(stored, key, Change{Attempt: attempt}, payloadNow)
	if err != nil || !reflect.DeepEqual(next, moved(stored)) {
		t.Fatalf("Merge: %v\n got %s\nwant %s", err, trustDump(next), trustDump(moved(stored)))
	}
	if !reflect.DeepEqual(drops, []Drop{{Record: "node 1", Reason: DropExpired}}) {
		t.Errorf("drops %v", drops)
	}

	// With a conflict, and over a stored conflict.
	next, _, err = Merge(stored, key, Change{Conflict: true, Attempt: attempt}, payloadNow)
	want := moved(stored)
	want.Cluster.IdentityConflict = true
	if err != nil || !reflect.DeepEqual(next, want) {
		t.Errorf("with a conflict: %v\n got %s\nwant %s", err, trustDump(next), trustDump(want))
	}
	if again, _, err := Merge(
		next,
		key,
		Change{Attempt: attempt},
		payloadNow,
	); err != nil || !reflect.DeepEqual(again, want) {
		t.Errorf("over a stored conflict: %+v, %v", again, err)
	}

	// A section a reader hides for its time stays as stored, and the attempt
	// records nothing, so it does not become visible.
	aheadFile := payloadFor(key, "pve2")
	aheadFile.Discovery.DiscoveredAt = payloadNow.Add(time.Hour)
	if next, _, err := Merge(aheadFile, key, Change{Attempt: attempt}, payloadNow); err != nil ||
		!reflect.DeepEqual(next, aheadFile) {
		t.Errorf("a section dated ahead: %+v, %v", next, err)
	}
	if p, _, err := Validate(aheadFile, key, payloadNow); err != nil || p.Discovery != nil {
		t.Fatalf("the section dated ahead is visible to a reader: %+v, %v", p, err)
	}
	// A section dated just inside FutureSkew is visible, and takes the
	// attempt's time.
	insideFile := payloadFor(key, "pve2")
	insideFile.Discovery.DiscoveredAt = payloadNow.Add(FutureSkew)
	if next, _, err := Merge(insideFile, key, Change{Attempt: attempt}, payloadNow); err != nil ||
		!reflect.DeepEqual(next, moved(insideFile)) {
		t.Errorf("a section dated inside FutureSkew: %+v, %v", next, err)
	}

	// A next refresh further ahead than MaxRefreshWait is dropped, so the
	// refresh is due at once.
	far := &DiscoveryAttempt{At: payloadNow, NextRefreshAfter: payloadNow.Add(MaxRefreshWait + time.Minute)}
	next, drops, err = Merge(payloadFor(key, "pve2"), key, Change{Attempt: far}, payloadNow)
	if err != nil || !next.Discovery.NextRefreshAfter.IsZero() || !next.Discovery.DiscoveredAt.Equal(payloadNow) ||
		!reflect.DeepEqual(drops, []Drop{{Record: "next_refresh_after", Reason: DropFuture}}) {
		t.Errorf("a far next refresh: %+v, %v, %v", next.Discovery, drops, err)
	}

	// With no stored section there is no refresh time to move.
	identityOnly := &Payload{Key: key, Cluster: ptr(payloadCluster(payloadNow))}
	if next, _, err := Merge(identityOnly, key, Change{Attempt: attempt}, payloadNow); err != nil ||
		!reflect.DeepEqual(next, identityOnly) {
		t.Errorf("an identity alone: %+v, %v", next, err)
	}
	if next, _, err := Merge(nil, key, Change{Attempt: attempt}, payloadNow); err != nil || next != nil {
		t.Errorf("no file: %+v, %v", next, err)
	}

	refused := map[string]Change{
		"with a result": {Attempt: attempt, Result: mergeResult("pve2")},
		"with a hint":   {Attempt: attempt, Hint: &ColdStartHint{Name: "pve2", ProbedAt: payloadNow}},
		"with no time":  {Attempt: &DiscoveryAttempt{NextRefreshAfter: payloadNow.Add(time.Hour)}},
		"with a time ahead": {Attempt: &DiscoveryAttempt{
			At: payloadNow.Add(FutureSkew + time.Second), NextRefreshAfter: payloadNow.Add(time.Hour),
		}},
		"with no next refresh": {Attempt: &DiscoveryAttempt{At: payloadNow}},
		"with a next refresh before the attempt": {Attempt: &DiscoveryAttempt{
			At: payloadNow, NextRefreshAfter: payloadNow.Add(-time.Nanosecond),
		}},
		"with a time before the stored discovery": {Attempt: &DiscoveryAttempt{
			At: stored.Discovery.DiscoveredAt.Add(
				-FutureSkew - time.Nanosecond,
			), NextRefreshAfter: payloadNow.Add(time.Hour),
		}},
		"with a time a year back": {Attempt: &DiscoveryAttempt{
			At: payloadNow.Add(-365 * 24 * time.Hour), NextRefreshAfter: payloadNow.Add(time.Hour),
		}},
	}
	for name, change := range refused {
		t.Run(name, func(t *testing.T) {
			if next, _, err := Merge(
				stored,
				key,
				change,
				payloadNow,
			); !errors.Is(err, ErrInvalidChange) || next != nil {
				t.Errorf("Merge: %+v, %v, want ErrInvalidChange", next, err)
			}
		})
	}
	// A time within FutureSkew ahead is accepted as the writer's clock.
	near := &DiscoveryAttempt{At: payloadNow.Add(FutureSkew), NextRefreshAfter: payloadNow.Add(time.Hour)}
	if _, _, err := Merge(stored, key, Change{Attempt: near}, payloadNow); err != nil {
		t.Errorf("an attempt within FutureSkew: %v", err)
	}
	// The limits themselves are accepted. A next refresh equal to the attempt
	// is due at once, and an attempt FutureSkew before the stored discovery
	// is the clock skew a visible section can carry.
	edges := map[string]*DiscoveryAttempt{
		"a next refresh equal to the attempt": {At: payloadNow, NextRefreshAfter: payloadNow},
		"a time FutureSkew before the stored discovery": {
			At: stored.Discovery.DiscoveredAt.Add(-FutureSkew), NextRefreshAfter: payloadNow.Add(time.Hour),
		},
	}
	for name, edge := range edges {
		if _, _, err := Merge(stored, key, Change{Attempt: edge}, payloadNow); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
