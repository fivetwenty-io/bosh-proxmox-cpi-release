package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	pveerr "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// ============================================================
// Test doubles for registerStemcellDirectorRef / deregisterStemcellDirectorRef.
//
// Reuses refsQEMUSvc / refsNodesSvc / refsPoolSvc / refsClient from
// stemcell_refs_internal_test.go (same package), but records EVERY
// UpdateQemuConfig call in order (rather than only the last one) so tests can
// assert ordering — in particular, that no description write with an emptied
// ref set happens before a last-ref destroy call.
// ============================================================

// dirRefCall records one observed side effect during a
// register/deregisterStemcellDirectorRef call: either an UpdateQemuConfig
// write ("update", with the Description/Tags pointers that were set) or an
// invocation of the test-supplied destroy callback ("destroy").
type dirRefCall struct {
	kind        string
	description *string
	tags        *string
}

// buildDirectorRefsDeps constructs Deps backed by configFn for QEMU().Config
// reads, and returns a pointer to the ordered call log every UpdateQemuConfig
// invocation appends to.
func buildDirectorRefsDeps(
	configFn func(ctx context.Context, node string, vmid int) (map[string]any, error),
) (Deps, *[]dirRefCall) {
	calls := &[]dirRefCall{}
	deps := Deps{
		Config: &config.CPIConfig{
			Node:            "pve-node1",
			StemcellStorage: "local",
			VMStorage:       "local",
			DiskStorage:     "local",
		},
		PVE: &refsClient{
			q: &refsQEMUSvc{configFn: configFn},
			n: &refsNodesSvc{updateFn: func(_ context.Context, _, _ string, params *sdknodes.UpdateQemuConfigParams) error {
				*calls = append(*calls, dirRefCall{kind: "update", description: params.Description, tags: params.Tags})
				return nil
			}},
			p: &refsPoolSvc{},
		},
		Logger: log.NewNopLogger(),
	}
	return deps, calls
}

// recordingDestroy returns a destroy callback that appends a "destroy" entry
// to calls (the same ordered log buildDirectorRefsDeps returns) and then
// returns err — letting tests assert BOTH ordering (relative to update calls)
// and destroy-failure handling with a single spy.
func recordingDestroy(calls *[]dirRefCall, err error) func(context.Context) error {
	return func(_ context.Context) error {
		*calls = append(*calls, dirRefCall{kind: "destroy"})
		return err
	}
}

// ============================================================
// Tests: registerStemcellDirectorRef
// ============================================================

func TestRegisterStemcellDirectorRef_AddsUUID_Idempotent(t *testing.T) {
	t.Parallel()

	const vmid = int64(6042)
	const node = "pve-node1"
	desc := `{"name":"test","version":"1.0","sha8":"ab12cd34","created":"2026-08-03T00:00:00Z","kind":"heavy","cid":":heavy:local:import/x.qcow2","director_refs":[]}`

	deps, calls := buildDirectorRefsDeps(func(_ context.Context, _ string, _ int) (map[string]any, error) {
		return map[string]any{"description": desc}, nil
	})

	if err := registerStemcellDirectorRef(context.Background(), deps, deps.Logger, node, vmid, "director-a"); err != nil {
		t.Fatalf("first register: unexpected error: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("first register: got %d calls, want 1: %+v", len(*calls), *calls)
	}
	firstCall := (*calls)[0]
	if firstCall.description == nil {
		t.Fatal("first register: expected description write when ref is newly added")
	}
	prov, ok := parseStemcellProvenanceFromDescription(*firstCall.description)
	if !ok {
		t.Fatalf("first register: written description is not valid JSON: %q", *firstCall.description)
	}
	if len(prov.DirectorRefs) != 1 || prov.DirectorRefs[0] != "director-a" {
		t.Errorf("first register: DirectorRefs = %v, want [director-a]", prov.DirectorRefs)
	}
	if firstCall.tags == nil || !strings.Contains(*firstCall.tags, "director--director-a") {
		t.Errorf("first register: tags = %v, want to contain director--director-a", firstCall.tags)
	}

	// Idempotent re-registration against the state the first call produced:
	// no ref addition, no tag addition, no pending tag to clear — expect
	// zero further API calls.
	deps2, calls2 := buildDirectorRefsDeps(func(_ context.Context, _ string, _ int) (map[string]any, error) {
		return map[string]any{"description": *firstCall.description, "tags": *firstCall.tags}, nil
	})
	if err := registerStemcellDirectorRef(context.Background(), deps2, deps2.Logger, node, vmid, "director-a"); err != nil {
		t.Fatalf("second register: unexpected error: %v", err)
	}
	if len(*calls2) != 0 {
		t.Errorf("second register: expected 0 calls (idempotent), got %d: %+v", len(*calls2), *calls2)
	}
}

func TestRegisterStemcellDirectorRef_MissingTemplate_ReturnsErrGone(t *testing.T) {
	t.Parallel()

	deps, calls := buildDirectorRefsDeps(func(_ context.Context, _ string, _ int) (map[string]any, error) {
		return nil, pveerr.ErrNotFound
	})

	err := registerStemcellDirectorRef(context.Background(), deps, deps.Logger, "pve-node1", 6042, "director-a")
	if !errors.Is(err, ErrStemcellTemplateGone) {
		t.Fatalf("expected ErrStemcellTemplateGone, got %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("expected no writes when template is gone, got %+v", *calls)
	}
}

func TestRegisterStemcellDirectorRef_ClearsPendingTag(t *testing.T) {
	t.Parallel()

	desc := `{"name":"test","version":"1.0","sha8":"ab12cd34","created":"2026-08-03T00:00:00Z","director_refs":["director-a"]}`
	deps, calls := buildDirectorRefsDeps(func(_ context.Context, _ string, _ int) (map[string]any, error) {
		return map[string]any{
			"description": desc,
			"tags":        "director--director-a;bosh-destroy-pending",
		}, nil
	})

	if err := registerStemcellDirectorRef(context.Background(), deps, deps.Logger, "pve-node1", 6042, "director-a"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 call (pending-tag removal), got %d: %+v", len(*calls), *calls)
	}
	got := (*calls)[0]
	if got.tags == nil {
		t.Fatal("expected tags to be rewritten")
	}
	if strings.Contains(*got.tags, stemcellDestroyPendingTag) {
		t.Errorf("pending tag not removed: %q", *got.tags)
	}
	if !strings.Contains(*got.tags, "director--director-a") {
		t.Errorf("existing director tag lost: %q", *got.tags)
	}
	// The ref was already present — description content is unchanged, so it
	// must not be rewritten at all (only the tag field changed).
	if got.description != nil {
		t.Errorf("expected no description write (ref already present), got %q", *got.description)
	}
}

func TestRegisterStemcellDirectorRef_EmptyUUID_UsesSentinelAndWarns(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger, err := log.NewLogger("warn", &buf)
	if err != nil {
		t.Fatalf("log.NewLogger: %v", err)
	}

	desc := `{"name":"test","version":"1.0","sha8":"ab12cd34","created":"2026-08-03T00:00:00Z","director_refs":[]}`
	deps, calls := buildDirectorRefsDeps(func(_ context.Context, _ string, _ int) (map[string]any, error) {
		return map[string]any{"description": desc}, nil
	})

	if err := registerStemcellDirectorRef(context.Background(), deps, logger, "pve-node1", 6042, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "director UUID") {
		t.Errorf("expected a Warn log about the missing director UUID, got: %s", buf.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %+v", len(*calls), *calls)
	}
	if (*calls)[0].description == nil {
		t.Fatal("expected description write")
	}
	prov, ok := parseStemcellProvenanceFromDescription(*(*calls)[0].description)
	if !ok || len(prov.DirectorRefs) != 1 || prov.DirectorRefs[0] != unknownDirectorRef {
		t.Errorf("expected DirectorRefs=[%s], got %+v (ok=%v)", unknownDirectorRef, prov.DirectorRefs, ok)
	}
}

func TestRegisterStemcellDirectorRef_InvalidVMID(t *testing.T) {
	t.Parallel()
	deps, _ := buildDirectorRefsDeps(nil)
	if err := registerStemcellDirectorRef(context.Background(), deps, deps.Logger, "pve-node1", 0, "director-a"); err == nil {
		t.Fatal("expected error for vmid <= 0")
	}
}

func TestRegisterStemcellDirectorRef_EmptyNode(t *testing.T) {
	t.Parallel()
	deps, _ := buildDirectorRefsDeps(nil)
	if err := registerStemcellDirectorRef(context.Background(), deps, deps.Logger, "", 6042, "director-a"); err == nil {
		t.Fatal("expected error for empty node")
	}
}

// ============================================================
// Tests: deregisterStemcellDirectorRef
// ============================================================

func TestDeregisterStemcellDirectorRef_RefsRemain_NoDestroy(t *testing.T) {
	t.Parallel()

	desc := `{"name":"test","version":"1.0","sha8":"ab12cd34","created":"2026-08-03T00:00:00Z","director_refs":["director-a","director-b"]}`
	deps, calls := buildDirectorRefsDeps(func(_ context.Context, _ string, _ int) (map[string]any, error) {
		return map[string]any{"description": desc}, nil
	})
	destroyCalled := false
	destroy := func(_ context.Context) error { destroyCalled = true; return nil }

	destroyed, remaining, err := deregisterStemcellDirectorRef(context.Background(), deps, deps.Logger, "pve-node1", 6042, "director-a", destroy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if destroyed {
		t.Error("expected destroyed=false when refs remain")
	}
	if destroyCalled {
		t.Error("destroy must not be called when refs remain")
	}
	if len(remaining) != 1 || remaining[0] != "director-b" {
		t.Errorf("remaining = %v, want [director-b]", remaining)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 update call, got %d: %+v", len(*calls), *calls)
	}
	if (*calls)[0].description == nil {
		t.Fatal("expected a description write")
	}
	writtenProv, ok := parseStemcellProvenanceFromDescription(*(*calls)[0].description)
	if !ok || len(writtenProv.DirectorRefs) != 1 || writtenProv.DirectorRefs[0] != "director-b" {
		t.Errorf("written description DirectorRefs = %+v (ok=%v), want [director-b]", writtenProv.DirectorRefs, ok)
	}
}

func TestDeregisterStemcellDirectorRef_LastRef_StampsPendingTagThenDestroys(t *testing.T) {
	t.Parallel()

	desc := `{"name":"test","version":"1.0","sha8":"ab12cd34","created":"2026-08-03T00:00:00Z","director_refs":["director-a"]}`
	deps, calls := buildDirectorRefsDeps(func(_ context.Context, _ string, _ int) (map[string]any, error) {
		return map[string]any{"description": desc}, nil
	})
	destroy := recordingDestroy(calls, nil)

	destroyed, remaining, err := deregisterStemcellDirectorRef(context.Background(), deps, deps.Logger, "pve-node1", 6042, "director-a", destroy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !destroyed {
		t.Error("expected destroyed=true when last ref removed")
	}
	if remaining != nil {
		t.Errorf("remaining = %v, want nil", remaining)
	}
	// Ordering: the pending-destroy tag is stamped (inside the lock) BEFORE
	// destroy runs (outside it, once the lock releases) — but the
	// description (director_refs included) is never written at all, emptied
	// or otherwise. That is the trapdoor fix: the ref set is only ever
	// implicitly retired by a successful destroy, never separately persisted
	// as empty.
	if len(*calls) != 2 {
		t.Fatalf("expected 2 recorded calls (tag stamp, then destroy), got %+v", *calls)
	}
	stampCall := (*calls)[0]
	if stampCall.kind != "update" {
		t.Fatalf("first recorded call = %q, want update (pending-tag stamp)", stampCall.kind)
	}
	if stampCall.tags == nil || !strings.Contains(*stampCall.tags, stemcellDestroyPendingTag) {
		t.Errorf("expected pending-tag stamp write, got tags=%v", stampCall.tags)
	}
	if stampCall.description != nil {
		t.Errorf("expected no description write, ever, got %q", *stampCall.description)
	}
	if (*calls)[1].kind != "destroy" {
		t.Fatalf("second recorded call = %q, want destroy", (*calls)[1].kind)
	}
}

func TestDeregisterStemcellDirectorRef_DestroyFailure_TagAlreadyStamped_PreservesRefs(t *testing.T) {
	t.Parallel()

	desc := `{"name":"test","version":"1.0","sha8":"ab12cd34","created":"2026-08-03T00:00:00Z","director_refs":["director-a"]}`
	deps, calls := buildDirectorRefsDeps(func(_ context.Context, _ string, _ int) (map[string]any, error) {
		return map[string]any{"description": desc}, nil
	})
	destroyErr := errors.New("linked clone present")
	destroy := recordingDestroy(calls, destroyErr)

	// Deliberately a different node/vmid than the other deregister tests so
	// no argument to deregisterStemcellDirectorRef is a package-wide constant
	// across every call site.
	destroyed, remaining, err := deregisterStemcellDirectorRef(context.Background(), deps, deps.Logger, "pve-node2", 7000, "director-a", destroy)
	if err == nil {
		t.Fatal("expected error propagated from destroy failure")
	}
	if !errors.Is(err, destroyErr) && !strings.Contains(err.Error(), destroyErr.Error()) {
		t.Errorf("error %v does not wrap/mention destroy failure %v", err, destroyErr)
	}
	if destroyed {
		t.Error("expected destroyed=false on destroy failure")
	}
	if remaining != nil {
		t.Errorf("remaining = %v, want nil", remaining)
	}
	// Ordering: the pending-destroy tag is stamped INSIDE the lock, BEFORE
	// destroy runs outside it — so on a destroy failure the stamp is already
	// in place and no second write follows it.
	if len(*calls) != 2 {
		t.Fatalf("expected 2 calls (pending-tag stamp, then the failed destroy attempt), got %+v", *calls)
	}
	stampCall := (*calls)[0]
	if stampCall.kind != "update" {
		t.Fatalf("first recorded call = %q, want update (pending-tag stamp)", stampCall.kind)
	}
	if stampCall.tags == nil || !strings.Contains(*stampCall.tags, stemcellDestroyPendingTag) {
		t.Errorf("expected pending-tag stamp write, got tags=%v", stampCall.tags)
	}
	// Refs are never separately rewritten as empty — no description write
	// happens at all, on either the stamp or the failed destroy.
	if stampCall.description != nil {
		t.Errorf("expected no description write, got %q", *stampCall.description)
	}
	if (*calls)[1].kind != "destroy" {
		t.Errorf("second recorded call = %q, want destroy", (*calls)[1].kind)
	}
}

func TestDeregisterStemcellDirectorRef_PendingTag_ResumesDestroy(t *testing.T) {
	t.Parallel()

	// Description still reports the ref as present (the earlier destroy
	// attempt failed before any refs write), but the pending tag says a
	// destroy is already in flight — resume must skip refs logic entirely
	// and call destroy directly.
	desc := `{"name":"test","version":"1.0","sha8":"ab12cd34","created":"2026-08-03T00:00:00Z","director_refs":["director-a"]}`
	deps, calls := buildDirectorRefsDeps(func(_ context.Context, _ string, _ int) (map[string]any, error) {
		return map[string]any{"description": desc, "tags": stemcellDestroyPendingTag}, nil
	})
	destroy := recordingDestroy(calls, nil)

	destroyed, remaining, err := deregisterStemcellDirectorRef(context.Background(), deps, deps.Logger, "pve-node1", 6042, "director-a", destroy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !destroyed {
		t.Error("expected destroyed=true on pending-tag resume")
	}
	if remaining != nil {
		t.Errorf("remaining = %v, want nil", remaining)
	}
	if len(*calls) != 1 || (*calls)[0].kind != "destroy" {
		t.Fatalf("expected exactly one call (destroy; refs never touched), got %+v", *calls)
	}
}

func TestDeregisterStemcellDirectorRef_PendingTag_ResumeFailure(t *testing.T) {
	t.Parallel()

	desc := `{"name":"test","version":"1.0","sha8":"ab12cd34","created":"2026-08-03T00:00:00Z","director_refs":["director-a"]}`
	deps, calls := buildDirectorRefsDeps(func(_ context.Context, _ string, _ int) (map[string]any, error) {
		return map[string]any{"description": desc, "tags": stemcellDestroyPendingTag}, nil
	})
	destroyErr := errors.New("still busy")
	destroy := recordingDestroy(calls, destroyErr)

	destroyed, remaining, err := deregisterStemcellDirectorRef(context.Background(), deps, deps.Logger, "pve-node1", 6042, "director-a", destroy)
	if err == nil {
		t.Fatal("expected error propagated from resume destroy failure")
	}
	if destroyed {
		t.Error("expected destroyed=false on resume failure")
	}
	if remaining != nil {
		t.Errorf("remaining = %v, want nil", remaining)
	}
	// The pending tag is already present — no second stamp write should occur.
	if len(*calls) != 1 || (*calls)[0].kind != "destroy" {
		t.Fatalf("expected exactly one call (destroy attempt; tag already present), got %+v", *calls)
	}
}

func TestDeregisterStemcellDirectorRef_ConservativeNonJSON(t *testing.T) {
	t.Parallel()

	deps, calls := buildDirectorRefsDeps(func(_ context.Context, _ string, _ int) (map[string]any, error) {
		return map[string]any{"description": "director: cf\ndeployment: prod\n"}, nil
	})
	destroyCalled := false
	destroy := func(_ context.Context) error { destroyCalled = true; return nil }

	destroyed, remaining, err := deregisterStemcellDirectorRef(context.Background(), deps, deps.Logger, "pve-node1", 6042, "director-a", destroy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if destroyed {
		t.Error("expected destroyed=false for non-JSON description (conservative rule)")
	}
	if destroyCalled {
		t.Error("destroy must not be called on an unparseable description")
	}
	if remaining != nil {
		t.Errorf("remaining = %v, want nil", remaining)
	}
	if len(*calls) != 0 {
		t.Errorf("expected no writes on the conservative path, got %+v", *calls)
	}
}

func TestDeregisterStemcellDirectorRef_TemplateGone_Idempotent(t *testing.T) {
	t.Parallel()

	deps, _ := buildDirectorRefsDeps(func(_ context.Context, _ string, _ int) (map[string]any, error) {
		return nil, pveerr.ErrNotFound
	})
	destroyCalled := false
	destroy := func(_ context.Context) error { destroyCalled = true; return nil }

	destroyed, remaining, err := deregisterStemcellDirectorRef(context.Background(), deps, deps.Logger, "pve-node1", 6042, "director-a", destroy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !destroyed {
		t.Error("expected destroyed=true for an already-gone template (idempotent)")
	}
	if remaining != nil {
		t.Errorf("remaining = %v, want nil", remaining)
	}
	if destroyCalled {
		t.Error("destroy must not be called when the template is already gone")
	}
}

func TestDeregisterStemcellDirectorRef_EmptyUUID_UsesSentinel(t *testing.T) {
	t.Parallel()

	desc := fmt.Sprintf(`{"name":"test","version":"1.0","sha8":"ab12cd34","created":"2026-08-03T00:00:00Z","director_refs":[%q]}`, unknownDirectorRef)
	deps, calls := buildDirectorRefsDeps(func(_ context.Context, _ string, _ int) (map[string]any, error) {
		return map[string]any{"description": desc}, nil
	})
	destroy := recordingDestroy(calls, nil)

	destroyed, remaining, err := deregisterStemcellDirectorRef(context.Background(), deps, deps.Logger, "pve-node1", 6042, "", destroy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !destroyed {
		t.Error("expected destroyed=true — empty UUID collapses to unknownDirectorRef, matching the only ref present")
	}
	if remaining != nil {
		t.Errorf("remaining = %v, want nil", remaining)
	}
}

// TestBuildStemcellProvenanceNotesPath_EmptyDirectorUUID_DeregisterEmptiesSet
// is the regression test for the empty-UUID seed bug: buildStemcellProvenanceNotesPath
// used to seed DirectorRefs with the raw empty string when
// creatingDirectorUUID was "", while registerStemcellDirectorRef/
// deregisterStemcellDirectorRef both resolve a UUID-less caller to the
// unknownDirectorRef sentinel — "" != unknownDirectorRef, so the seeded ""
// entry could never be matched and removed, leaving the template's
// DirectorRefs permanently non-empty and the template permanently
// un-destroyable. Exercises the REAL builder (not a hand-written description
// fixture) end to end: seed with an empty director UUID, then deregister the
// same empty UUID and assert the set actually empties and destroy fires.
func TestBuildStemcellProvenanceNotesPath_EmptyDirectorUUID_DeregisterEmptiesSet(t *testing.T) {
	t.Parallel()

	cp := stemcellCloudProps{Name: "bosh-ubuntu-noble", Version: "1.0"}
	now := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	notes, buildErr := buildStemcellProvenanceNotesPath(cp, pve.StemcellKindHeavy,
		":heavy:local:import/x.qcow2", "", "", "", now, nil)
	if buildErr != nil {
		t.Fatalf("buildStemcellProvenanceNotesPath: unexpected error: %v", buildErr)
	}

	// Sanity: the seed itself must already be the sentinel, not "".
	seeded, ok := parseStemcellProvenanceFromDescription(notes)
	if !ok {
		t.Fatalf("seeded notes not parseable: %s", notes)
	}
	if len(seeded.DirectorRefs) != 1 || seeded.DirectorRefs[0] != unknownDirectorRef {
		t.Fatalf("seeded DirectorRefs = %v, want [%s]", seeded.DirectorRefs, unknownDirectorRef)
	}

	deps, calls := buildDirectorRefsDeps(func(_ context.Context, _ string, _ int) (map[string]any, error) {
		return map[string]any{"description": notes}, nil
	})
	destroy := recordingDestroy(calls, nil)

	destroyed, remaining, err := deregisterStemcellDirectorRef(context.Background(), deps, deps.Logger, "pve-node1", 6042, "", destroy)
	if err != nil {
		t.Fatalf("deregisterStemcellDirectorRef: unexpected error: %v", err)
	}
	if !destroyed {
		t.Error("expected destroyed=true — the sentinel-seeded ref must be fully removable by a matching empty-UUID deregister")
	}
	if remaining != nil {
		t.Errorf("remaining = %v, want nil", remaining)
	}
}

func TestDeregisterStemcellDirectorRef_NilDestroyCallback_Errors(t *testing.T) {
	t.Parallel()
	deps, _ := buildDirectorRefsDeps(nil)
	if _, _, err := deregisterStemcellDirectorRef(context.Background(), deps, deps.Logger, "pve-node1", 6042, "director-a", nil); err == nil {
		t.Fatal("expected error for nil destroy callback")
	}
}

// ============================================================
// Tests: mergeProvenanceIntoDescription
// ============================================================

func TestMergeProvenanceIntoDescription_PreservesForeignKeys(t *testing.T) {
	t.Parallel()

	desc := `{"operator_note":"do not delete","name":"old-name","director_refs":["stale"]}`
	prov := stemcellProvenance{
		Name: "new-name", Version: "1.0", SHA8: "ab12cd34",
		Created: "2026-08-03T00:00:00Z", DirectorRefs: []string{"director-a"},
	}

	merged, err := mergeProvenanceIntoDescription(desc, prov)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]any
	if jsonErr := json.Unmarshal([]byte(merged), &m); jsonErr != nil {
		t.Fatalf("merged output is not valid JSON: %v (%s)", jsonErr, merged)
	}
	if m["operator_note"] != "do not delete" {
		t.Errorf("foreign key operator_note dropped: %s", merged)
	}
	if m["name"] != "new-name" {
		t.Errorf("name = %v, want new-name (owned key must be overwritten)", m["name"])
	}
	refs, _ := m["director_refs"].([]any)
	if len(refs) != 1 || refs[0] != "director-a" {
		t.Errorf("director_refs = %v, want [director-a]", m["director_refs"])
	}
}

func TestMergeProvenanceIntoDescription_ClearsEmptiedOwnedKey(t *testing.T) {
	t.Parallel()

	desc := `{"director_refs":["stale-a","stale-b"],"operator_note":"keep me"}`
	// prov.DirectorRefs left empty (omitempty) — the merge must not leave the
	// base map's stale director_refs value behind.
	prov := stemcellProvenance{Name: "n", Version: "v", SHA8: "ab12cd34", Created: "2026-08-03T00:00:00Z"}

	merged, err := mergeProvenanceIntoDescription(desc, prov)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(merged, "director_refs") {
		t.Errorf("stale director_refs must be cleared when prov.DirectorRefs is empty: %s", merged)
	}
	if !strings.Contains(merged, "keep me") {
		t.Errorf("foreign key operator_note dropped: %s", merged)
	}
}

func TestMergeProvenanceIntoDescription_NonObjectJSON_Errors(t *testing.T) {
	t.Parallel()

	prov := stemcellProvenance{Name: "n", Version: "v", SHA8: "ab12cd34", Created: "2026-08-03T00:00:00Z"}
	cases := []string{`[1,2]`, `"just a string"`, `42`, `not json at all`}
	for _, desc := range cases {
		desc := desc
		t.Run(desc, func(t *testing.T) {
			t.Parallel()
			if _, err := mergeProvenanceIntoDescription(desc, prov); err == nil {
				t.Errorf("mergeProvenanceIntoDescription(%q): expected error, got nil", desc)
			}
		})
	}
}

func TestMergeProvenanceIntoDescription_EmptyDescription_StartsFresh(t *testing.T) {
	t.Parallel()

	prov := stemcellProvenance{
		Name: "n", Version: "v", SHA8: "ab12cd34",
		Created: "2026-08-03T00:00:00Z", DirectorRefs: []string{"director-a"},
	}
	merged, err := mergeProvenanceIntoDescription("", prov)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := parseStemcellProvenanceFromDescription(merged)
	if !ok {
		t.Fatalf("merged output not parseable: %s", merged)
	}
	if got.Name != "n" || len(got.DirectorRefs) != 1 || got.DirectorRefs[0] != "director-a" {
		t.Errorf("got %+v", got)
	}
}
