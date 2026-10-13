package handlers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// The owner tag, the evidence-checked backfill, and the instance identity
// check in set_vm_metadata. Every case drives the handler end to end against a
// VM config the test supplies, and reads the tags and name the handler writes.

const (
	ownerTestDirectorUUID = "5e1f0c2a-7d4b-4c8e-9a1f-2b3c4d5e6f70"
	ownerTestInstanceUUID = "2844c990-aef3-4de7-8bf3-1a2b3c4d5e6f"
	ownerTestOtherUUID    = "9f0e1d2c-3b4a-4596-8877-665544332211"
	ownerTestOwnerTag     = "bosh-owner--" + ownerTestDirectorUUID
)

// ownerTestMetadata is the metadata a Director sends for the ownerTestInstanceUUID
// instance of the "web" instance group.
func ownerTestMetadata() map[string]any {
	instanceUUID := ownerTestInstanceUUID
	return map[string]any{
		"director":       "bosh",
		"deployment":     "cf",
		"instance_group": "web",
		"job":            "web",
		"index":          "0",
		"id":             instanceUUID,
		"name":           "web/" + instanceUUID,
	}
}

// ownerTestDescription is the description buildDescription writes for the
// metadata above, which is what a VM records after its first sync.
func ownerTestDescription(instanceUUID string) string {
	return "deployment: cf\ndirector: bosh\nid: " + instanceUUID + "\nindex: 0\ninstance_group: web\njob: web\nname: web/" +
		instanceUUID + "\n"
}

// ownerTestRun is one set_vm_metadata call against a VM whose stored tags and
// description the caller chooses.
type ownerTestRun struct {
	tags         string
	description  string
	configErr    error
	directorUUID string
	metadata     map[string]any
}

type ownerTestResult struct {
	err         error
	updated     bool
	tags        string
	name        string
	description string
	obs         *log.Observer
}

func (r ownerTestRun) do(t *testing.T) ownerTestResult {
	t.Helper()
	qemuSvc := &mockQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			if r.configErr != nil {
				return nil, r.configErr
			}
			cfg := map[string]any{}
			if r.tags != "" {
				cfg["tags"] = r.tags
			}
			if r.description != "" {
				cfg["description"] = r.description
			}
			return cfg, nil
		},
	}
	var res ownerTestResult
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _, _ string, params *nodes.UpdateQemuConfigParams) error {
			res.updated = true
			if params.Tags != nil {
				res.tags = *params.Tags
			}
			if params.Name != nil {
				res.name = *params.Name
			}
			if params.Description != nil {
				res.description = *params.Description
			}
			return nil
		},
	}
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	deps := testDepsFoundVM(101, qemuSvc, nodesSvc, nil, &mockAgentService{})
	deps.Logger = logger
	h := handlers.HandleSetVMMetadata(deps)
	_, res.err = h.Handle(context.Background(), marshalArgs("101", r.metadata),
		jsonrpc.Context{DirectorUUID: r.directorUUID})
	res.obs = obs
	return res
}

// tagIndex returns the position of tag in a ";"-joined tag list, or -1.
func tagIndex(tags, tag string) int {
	for i, e := range strings.Split(tags, ";") {
		if e == tag {
			return i
		}
	}
	return -1
}

// assertRefusedUntouched checks that a refused call returned a non-retriable
// CloudError and wrote nothing at all, so the VM keeps its name, tags, and
// description.
func assertRefusedUntouched(t *testing.T, res ownerTestResult) {
	t.Helper()
	if res.err == nil {
		t.Fatalf("set_vm_metadata succeeded; want a refusal (wrote tags %q, name %q)", res.tags, res.name)
	}
	var cerr *cpierrors.Error
	if !errors.As(res.err, &cerr) {
		t.Fatalf("error is %T, want *cpierrors.Error: %v", res.err, res.err)
	}
	if cerr.Type() != cpierrors.TypeCloud || cerr.OkToRetry() {
		t.Errorf("error type=%s retriable=%v, want a non-retriable %s", cerr.Type(), cerr.OkToRetry(), cpierrors.TypeCloud)
	}
	if res.updated {
		t.Errorf("UpdateQemuConfig was called on a refused VM (tags %q, name %q)", res.tags, res.name)
	}
}

// A legacy VM, one with bosh-cpi and no owner tag, whose stored identity
// matches the metadata gets the owner tag right behind bosh-cpi.
func TestSetVMMetadata_BackfillsOwnerTagOnMatchingIdentity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		tags        string
		description string
		metadata    map[string]any
	}{
		{
			name:        "id line in the description",
			tags:        "bosh-cpi;deployment--cf;director--bosh;job--web;web--" + ownerTestInstanceUUID,
			description: ownerTestDescription(ownerTestInstanceUUID),
			metadata:    ownerTestMetadata(),
		},
		{
			name:     "name tag only, after an operator replaced the notes",
			tags:     "bosh-cpi;env--prod;job--web;web--" + ownerTestInstanceUUID,
			metadata: ownerTestMetadata(),
		},
		{
			// A renamed instance group changes the job part of the name and
			// keeps the UUID, so the old records still match.
			name:        "renamed instance group",
			tags:        "bosh-cpi;job--old-web;old-web--" + ownerTestInstanceUUID,
			description: "id: " + ownerTestInstanceUUID + "\njob: old-web\nname: old-web/" + ownerTestInstanceUUID + "\n",
			metadata: func() map[string]any {
				m := ownerTestMetadata()
				m["job"], m["instance_group"], m["name"] = "api", "api", "api/"+ownerTestInstanceUUID
				return m
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := ownerTestRun{
				tags:         tc.tags,
				description:  tc.description,
				directorUUID: ownerTestDirectorUUID,
				metadata:     tc.metadata,
			}.do(t)
			if res.err != nil {
				t.Fatalf("unexpected error: %v", res.err)
			}
			if got := tagIndex(res.tags, ownerTestOwnerTag); got != 1 {
				t.Errorf("owner tag at position %d in %q; want 1, right behind bosh-cpi", got, res.tags)
			}
			if tagIndex(res.tags, "bosh-cpi") != 0 {
				t.Errorf("bosh-cpi is not first in %q", res.tags)
			}
		})
	}
}

// A legacy VM with no stored identity is the state create_vm leaves before the
// Director's first sync, so it is backfilled.
func TestSetVMMetadata_BackfillsOwnerTagWithNoStoredIdentity(t *testing.T) {
	t.Parallel()

	res := ownerTestRun{
		tags:         "bosh-cpi;stemcell--bosh-pve-ubuntu-jammy-1-1",
		directorUUID: ownerTestDirectorUUID,
		metadata:     ownerTestMetadata(),
	}.do(t)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if got := tagIndex(res.tags, ownerTestOwnerTag); got != 1 {
		t.Errorf("owner tag at position %d in %q; want 1", got, res.tags)
	}
	if !strings.Contains(res.description, "id: "+ownerTestInstanceUUID) {
		t.Errorf("description not written: %q", res.description)
	}
}

// A bosh create-env VM's metadata carries no id key. Its identity is the index
// that trails the name, and a matching index backfills and syncs as before.
func TestSetVMMetadata_CreateEnvMetadataWithoutIDSyncsAndBackfills(t *testing.T) {
	t.Parallel()

	metadata := map[string]any{
		"director":       "bosh-init",
		"deployment":     "bosh",
		"instance_group": "bosh",
		"job":            "bosh",
		"index":          "0",
		"name":           "bosh/0",
	}
	res := ownerTestRun{
		tags:         "bosh-cpi;deployment--bosh;director--bosh-init;index--0;job--bosh;bosh--0",
		description:  "deployment: bosh\ndirector: bosh-init\nindex: 0\ninstance_group: bosh\njob: bosh\nname: bosh/0\n",
		directorUUID: ownerTestDirectorUUID,
		metadata:     metadata,
	}.do(t)
	if res.err != nil {
		t.Fatalf("create-env VM refused: %v", res.err)
	}
	if res.name != "bosh-bosh-0" {
		t.Errorf("name = %q, want bosh-bosh-0", res.name)
	}
	if got := tagIndex(res.tags, ownerTestOwnerTag); got != 1 {
		t.Errorf("owner tag at position %d in %q; want 1", got, res.tags)
	}
}

// A legacy VM whose stored identity differs is refused, and nothing is
// renamed, retagged, or rewritten.
func TestSetVMMetadata_RefusesLegacyVMWithDifferentIdentity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		tags        string
		description string
		metadata    map[string]any
	}{
		{
			name:        "id line differs",
			tags:        "bosh-cpi;job--web;web--" + ownerTestOtherUUID,
			description: ownerTestDescription(ownerTestOtherUUID),
			metadata:    ownerTestMetadata(),
		},
		{
			name:     "name tag differs and the notes are gone",
			tags:     "bosh-cpi;job--web;web--" + ownerTestOtherUUID,
			metadata: ownerTestMetadata(),
		},
		{
			name:        "create-env metadata reaches a Director's VM",
			tags:        "bosh-cpi;job--web;web--" + ownerTestOtherUUID,
			description: ownerTestDescription(ownerTestOtherUUID),
			metadata:    map[string]any{"director": "bosh-init", "job": "bosh", "index": "0", "name": "bosh/0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := ownerTestRun{
				tags:         tc.tags,
				description:  tc.description,
				directorUUID: ownerTestDirectorUUID,
				metadata:     tc.metadata,
			}.do(t)
			assertRefusedUntouched(t, res)
		})
	}
}

// The identity check runs on an owned VM too, because a bare CID repointed at
// another instance's VM that our own Director created carries our owner tag.
func TestSetVMMetadata_RefusesOwnedVMWithDifferentIdentity(t *testing.T) {
	t.Parallel()

	res := ownerTestRun{
		tags:         "bosh-cpi;" + ownerTestOwnerTag + ";job--web;web--" + ownerTestOtherUUID,
		description:  ownerTestDescription(ownerTestOtherUUID),
		directorUUID: ownerTestDirectorUUID,
		metadata:     ownerTestMetadata(),
	}.do(t)
	assertRefusedUntouched(t, res)
}

// An owned VM with a matching identity syncs as before and keeps exactly one
// owner tag at the head.
func TestSetVMMetadata_OwnedVMWithMatchingIdentitySyncs(t *testing.T) {
	t.Parallel()

	res := ownerTestRun{
		tags:         "bosh-cpi;" + ownerTestOwnerTag + ";job--web;web--" + ownerTestInstanceUUID,
		description:  ownerTestDescription(ownerTestInstanceUUID),
		directorUUID: ownerTestDirectorUUID,
		metadata:     ownerTestMetadata(),
	}.do(t)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if n := strings.Count(res.tags, "bosh-owner--"); n != 1 {
		t.Errorf("found %d owner tags in %q, want 1", n, res.tags)
	}
	if got := tagIndex(res.tags, ownerTestOwnerTag); got != 1 {
		t.Errorf("owner tag at position %d in %q; want 1", got, res.tags)
	}
}

// Metadata that names no instance skips the identity check. The write goes
// ahead, and a legacy VM whose stored identity could not be compared is not
// backfilled.
func TestSetVMMetadata_MetadataWithoutIdentitySkipsCheck(t *testing.T) {
	t.Parallel()

	res := ownerTestRun{
		tags:         "bosh-cpi;job--web;web--" + ownerTestOtherUUID,
		description:  ownerTestDescription(ownerTestOtherUUID),
		directorUUID: ownerTestDirectorUUID,
		metadata:     map[string]any{"director": "bosh", "deployment": "cf"},
	}.do(t)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if !res.updated {
		t.Fatal("UpdateQemuConfig was not called")
	}
	if strings.Contains(res.tags, "bosh-owner--") {
		t.Errorf("owner tag backfilled without a comparable identity: %q", res.tags)
	}
}

// A request without a director_uuid backfills nothing and still syncs.
func TestSetVMMetadata_NoDirectorUUIDBackfillsNothing(t *testing.T) {
	t.Parallel()

	res := ownerTestRun{
		tags:     "bosh-cpi",
		metadata: ownerTestMetadata(),
	}.do(t)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if strings.Contains(res.tags, "bosh-owner--") {
		t.Errorf("owner tag stamped without a director_uuid: %q", res.tags)
	}
}

// Only a VM with bosh-cpi is legacy. A VM without it, and a VM that already
// names a different owner, never get an owner tag from set_vm_metadata,
// because the ownership guard refuses both on the config it reads under the
// lock, before anything is written.
func TestSetVMMetadata_BackfillOnlyOnLegacyVMs(t *testing.T) {
	t.Parallel()

	const otherOwner = "bosh-owner--0a0b0c0d-1111-2222-3333-444455556666"
	cases := []struct {
		name, tags string
	}{
		{"no bosh-cpi tag", "env--prod"},
		{"another Director's owner tag", "bosh-cpi;" + otherOwner},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := ownerTestRun{
				tags:         tc.tags,
				directorUUID: ownerTestDirectorUUID,
				metadata:     ownerTestMetadata(),
			}.do(t)
			assertRefusedUntouched(t, res)
		})
	}

	// A VM without bosh-cpi whose notes carry the Director's lines passes the
	// guard as ours, so it reaches the backfill. Only bosh-cpi marks a VM as
	// one this CPI made, so it must not be claimed with an owner tag, even
	// when its recorded instance matches.
	t.Run("no bosh-cpi tag, Director notes", func(t *testing.T) {
		t.Parallel()
		res := ownerTestRun{
			tags:         "env--prod",
			description:  ownerTestDescription(ownerTestInstanceUUID),
			directorUUID: ownerTestDirectorUUID,
			metadata:     ownerTestMetadata(),
		}.do(t)
		if res.err != nil {
			t.Fatalf("set_vm_metadata refused a VM whose notes carry the Director's lines: %v", res.err)
		}
		if !res.updated {
			t.Fatal("UpdateQemuConfig was not called")
		}
		if strings.Contains(res.tags, "bosh-owner--") {
			t.Errorf("an owner tag was backfilled onto a VM without bosh-cpi: %q", res.tags)
		}
		if tagIndex(res.tags, "env--prod") < 0 {
			t.Errorf("the operator tag env--prod was dropped: %q", res.tags)
		}
	})
}

// An operator tag that shares the job's name is not identity evidence unless
// its value looks like an instance identity, so it never refuses the VM's own
// instance.
func TestSetVMMetadata_OperatorTagNamedLikeTheJobIsNotEvidence(t *testing.T) {
	t.Parallel()

	res := ownerTestRun{
		tags:         "bosh-cpi;job--web;web--prod",
		directorUUID: ownerTestDirectorUUID,
		metadata:     ownerTestMetadata(),
	}.do(t)
	if res.err != nil {
		t.Fatalf("own VM refused over an operator tag: %v", res.err)
	}
	if got := tagIndex(res.tags, ownerTestOwnerTag); got != 1 {
		t.Errorf("owner tag at position %d in %q; want 1", got, res.tags)
	}
}

// When the VM config cannot be read under the lock, neither the ownership
// recheck nor the identity check can run, so set_vm_metadata refuses
// retriably and writes nothing, whether or not the metadata names an
// instance.
func TestSetVMMetadata_UnreadableConfig(t *testing.T) {
	t.Parallel()

	readErr := errors.New("connection reset by peer")

	res := ownerTestRun{
		configErr:    readErr,
		directorUUID: ownerTestDirectorUUID,
		metadata:     ownerTestMetadata(),
	}.do(t)
	if res.err == nil {
		t.Fatal("set_vm_metadata succeeded without reading the VM config")
	}
	var cerr *cpierrors.Error
	if !errors.As(res.err, &cerr) {
		t.Fatalf("error is %T, want *cpierrors.Error: %v", res.err, res.err)
	}
	if cerr.Type() != cpierrors.TypeRetriableCloud || !cerr.OkToRetry() {
		t.Errorf("error type=%s retriable=%v, want a retriable %s", cerr.Type(), cerr.OkToRetry(),
			cpierrors.TypeRetriableCloud)
	}
	if res.updated {
		t.Errorf("UpdateQemuConfig was called on a VM whose identity was never read (tags %q)", res.tags)
	}

	// Metadata that names no instance is refused the same way, because the
	// ownership recheck under the lock never ran on this VM's config.
	res = ownerTestRun{
		configErr:    readErr,
		directorUUID: ownerTestDirectorUUID,
		metadata:     map[string]any{"director": "bosh", "deployment": "cf"},
	}.do(t)
	if res.err == nil {
		t.Fatal("set_vm_metadata wrote to a VM whose ownership was never checked against its config")
	}
	if !errors.As(res.err, &cerr) || cerr.Type() != cpierrors.TypeRetriableCloud || !cerr.OkToRetry() {
		t.Errorf("error = %v, want a retriable %s", res.err, cpierrors.TypeRetriableCloud)
	}
	if res.updated {
		t.Error("UpdateQemuConfig was called on a VM whose ownership was never checked")
	}
}

// The owner tag survives the reserved-prefix rebuild when PVE hands the tags
// back sorted and long operator tags come ahead of it, because set_vm_metadata
// puts it back at the head before the byte cap truncates the tail.
func TestSetVMMetadata_OwnerTagSurvivesRebuildOfALongTagList(t *testing.T) {
	t.Parallel()

	// Sorted the way PVE stores tags, so every "a..." operator tag precedes
	// bosh-cpi and the owner tag, and together they pass the 350-byte cap.
	long := strings.Repeat("x", 60)
	stored := []string{
		"a1--" + long, "a2--" + long, "a3--" + long, "a4--" + long, "a5--" + long,
		"bosh-cpi", ownerTestOwnerTag,
		"deployment--old", "director--old", "job--web", "web--" + ownerTestInstanceUUID,
	}
	metadata := ownerTestMetadata()
	metadata["deployment"] = "a-deployment-name-long-enough-to-fill-the-tag-list-" + long
	res := ownerTestRun{
		tags:         strings.Join(stored, ";"),
		description:  ownerTestDescription(ownerTestInstanceUUID),
		directorUUID: ownerTestDirectorUUID,
		metadata:     metadata,
	}.do(t)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if len(res.tags) > 350 {
		t.Fatalf("tags are %d bytes, over the cap", len(res.tags))
	}
	if !strings.HasPrefix(res.tags, "bosh-cpi;"+ownerTestOwnerTag+";") {
		t.Errorf("tags do not open with bosh-cpi and the owner tag: %q", res.tags)
	}
	if strings.Contains(res.tags, "director--old") || strings.Contains(res.tags, "deployment--old") {
		t.Errorf("stale reserved tags survived the rebuild: %q", res.tags)
	}
}

// An instance group named after a create-time prefix would otherwise write a
// name tag that reads as an owner record and survives every later sync.
func TestSetVMMetadata_NameTagCannotClaimOwnerPrefix(t *testing.T) {
	t.Parallel()

	metadata := ownerTestMetadata()
	metadata["job"], metadata["instance_group"], metadata["name"] = "bosh-owner", "bosh-owner", "bosh-owner/"+ownerTestOtherUUID
	delete(metadata, "id")
	res := ownerTestRun{
		tags:     "bosh-cpi;env--prod",
		metadata: metadata,
	}.do(t)
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}
	if strings.Contains(res.tags, "bosh-owner--") {
		t.Errorf("instance name wrote an owner-shaped tag: %q", res.tags)
	}
	if countWarnsContaining(res.obs, "instance name tag") != 1 {
		t.Errorf("expected one warning about the refused name tag, got entries %+v", res.obs.All())
	}
}

// Index-only evidence, such as an operator tag {web: 7} that renders "web--7",
// is compared only with a create-env index. A Director's UUID request against
// it is not comparable, so the VM is neither refused nor backfilled.
func TestSetVMMetadata_IndexEvidenceIsNotComparedWithAUUID(t *testing.T) {
	t.Parallel()

	res := ownerTestRun{
		tags:         "bosh-cpi;job--web;web--7",
		directorUUID: ownerTestDirectorUUID,
		metadata:     ownerTestMetadata(),
	}.do(t)
	if res.err != nil {
		t.Fatalf("own VM refused over a digit-valued operator tag: %v", res.err)
	}
	if !res.updated {
		t.Fatal("UpdateQemuConfig was not called")
	}
	if strings.Contains(res.tags, ownerTestOwnerTag) {
		t.Errorf("owner tag backfilled on evidence that could not be compared: %q", res.tags)
	}
}

// A create-env index is still compared with another index, so create-env
// metadata for instance 0 that reaches the VM of instance 1 is refused.
func TestSetVMMetadata_RefusesDifferentCreateEnvIndex(t *testing.T) {
	t.Parallel()

	res := ownerTestRun{
		tags:         "bosh-cpi;job--bosh;bosh--1",
		description:  "deployment: bosh\ndirector: bosh-init\nindex: 1\njob: bosh\nname: bosh/1\n",
		directorUUID: ownerTestDirectorUUID,
		metadata:     map[string]any{"director": "bosh-init", "job": "bosh", "index": "0", "name": "bosh/0"},
	}.do(t)
	assertRefusedUntouched(t, res)
}

// A UUID-shaped name tag that no job record ties to the VM still shows the VM
// records some instance. With the job tags and the notes gone, the VM is
// neither refused nor backfilled, because we can't tell whose instance it is.
func TestSetVMMetadata_UntiedUUIDTagBlocksBackfill(t *testing.T) {
	t.Parallel()

	res := ownerTestRun{
		tags:         "bosh-cpi;deployment--cf;director--bosh;web--" + ownerTestOtherUUID,
		description:  "operator notes",
		directorUUID: ownerTestDirectorUUID,
		metadata:     ownerTestMetadata(),
	}.do(t)
	if res.err != nil {
		t.Fatalf("unexpected refusal: %v", res.err)
	}
	if strings.Contains(res.tags, ownerTestOwnerTag) {
		t.Errorf("owner tag backfilled beside another instance's name tag: %q", res.tags)
	}
}

// Stemcell templates and disk parkers carry bosh-cpi and record no instance,
// and several Directors may share them, so they never get an owner tag. The
// ownership guard refuses them outright, so nothing is written at all.
func TestSetVMMetadata_NoBackfillOnTemplatesOrParkers(t *testing.T) {
	t.Parallel()

	for _, marker := range []string{"bosh-stemcell", "bosh-stemcell-cache", "bosh-parker", "BOSH-Parker"} {
		t.Run(marker, func(t *testing.T) {
			t.Parallel()
			res := ownerTestRun{
				tags:         "bosh-cpi;" + marker,
				directorUUID: ownerTestDirectorUUID,
				metadata:     ownerTestMetadata(),
			}.do(t)
			assertRefusedUntouched(t, res)
		})
	}
}

// A refused call stops before the pool reconcile, so the VM stays in the pool
// it was in. The matching control shows the same fixture does move the VM.
func TestSetVMMetadata_RefusalSkipsPoolReconcile(t *testing.T) {
	t.Parallel()

	sentinel := sentinelDesc(t, &pve.PoolMembership{
		Name: "bosh", Layer: pve.PoolLayerTemplate,
		Director: "d1", Deployment: "dep1", InstanceGroup: "web",
	})
	metadata := standardMetadata()
	metadata["id"] = ownerTestInstanceUUID

	refused := newReconcileFixture(t, "id: "+ownerTestOtherUUID+"\nname: web/"+ownerTestOtherUUID+"\n"+sentinel, "bosh")
	h := handlers.HandleSetVMMetadata(refused.deps)
	if _, err := h.Handle(context.Background(), marshalArgs("101", metadata), jsonrpc.Context{}); err == nil {
		t.Fatal("set_vm_metadata succeeded on a VM that records a different instance")
	}
	if moves := refused.moveEvents(); len(moves) != 0 {
		t.Errorf("a refused call moved the VM between pools: %v", moves)
	}
	if len(refused.descWrites) != 0 {
		t.Errorf("a refused call rewrote the description: %v", refused.descWrites)
	}

	matched := newReconcileFixture(t, "id: "+ownerTestInstanceUUID+"\nname: web/"+ownerTestInstanceUUID+"\n"+sentinel, "bosh")
	matched.run(t, metadata)
	if moves := matched.moveEvents(); len(moves) != 1 {
		t.Errorf("control: want one pool move for the matching instance, got %v", moves)
	}
}

// An index the VM's Notes record comes from a real create-env sync, so it stays
// comparable with a Director's UUID. A Director CID repointed at a create-env
// VM is refused, and the VM keeps its name, tags, and Notes.
func TestSetVMMetadata_RefusesDirectorMetadataOnACreateEnvVM(t *testing.T) {
	t.Parallel()

	res := ownerTestRun{
		tags: "bosh-cpi;bosh-owner--aaaaaaaa-0000-0000-0000-000000000000;deployment--bosh;director--bosh-init;" +
			"index--0;job--bosh;bosh--0",
		description:  "director: bosh-init\nname: bosh/0\n",
		directorUUID: ownerTestDirectorUUID,
		metadata:     ownerTestMetadata(),
	}.do(t)
	assertRefusedUntouched(t, res)
}

// A UUID name tag that no job record ties to the VM, including one under a
// different job than the VM's job tag, may be another instance's. The call is
// neither refused nor backfilled, even when other evidence matches.
func TestSetVMMetadata_UntiedNameTagUnderAnotherJobBlocksBackfill(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, tags string }{
		{"no comparable evidence", "bosh-cpi;job--api;web--" + ownerTestOtherUUID},
		{"matching evidence beside it", "bosh-cpi;job--web;web--" + ownerTestInstanceUUID + ";api--" + ownerTestOtherUUID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := ownerTestRun{
				tags:         tc.tags,
				description:  "operator notes",
				directorUUID: ownerTestDirectorUUID,
				metadata:     ownerTestMetadata(),
			}.do(t)
			if res.err != nil {
				t.Fatalf("unexpected refusal: %v", res.err)
			}
			if !res.updated {
				t.Fatal("UpdateQemuConfig was not called")
			}
			if strings.Contains(res.tags, ownerTestOwnerTag) {
				t.Errorf("owner tag backfilled beside another instance's name tag: %q", res.tags)
			}
		})
	}
}
