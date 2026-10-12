package handlers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// create_vm stamps the owner tag right after bosh-cpi and keeps operator tags
// out of every CPI-owned prefix. These tests drive HandleCreateVM end to end
// and read the tags param of the captured QEMU.Create call.

func createVMOwnerTagRun(t *testing.T, directorUUID string, cloudProps map[string]any) (string, *log.Observer) {
	t.Helper()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	deps := buildVMDeps(q, n, c, a)
	deps.Logger = logger
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-owner-tag", testStemcellCID, cloudProps, defaultNetMap(), []string{}, map[string]any{})
	reqCtx := jsonrpc.Context{RequestID: "owner-tag", DirectorUUID: directorUUID}
	if _, err := h.Handle(context.Background(), args, reqCtx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	tags, _ := q.createCalls[0].params["tags"].(string)
	return tags, obs
}

// The owner tag sits second, right after bosh-cpi and ahead of the stemcell,
// retention, and operator tags, so the byte cap can never reach it first.
func TestCreateVM_OwnerTagFollowsBoshCPI(t *testing.T) {
	t.Parallel()

	tags, _ := createVMOwnerTagRun(t, ownerTestDirectorUUID, map[string]any{
		"cores": 1, "memory": 512, "retain_ephemeral_on_delete": true,
		"tags": map[string]any{"env": "prod", "team": "platform"},
	})
	entries := strings.Split(tags, ";")
	if len(entries) < 3 || entries[0] != "bosh-cpi" || entries[1] != ownerTestOwnerTag {
		t.Fatalf("tags = %q; want bosh-cpi then %s at the head", tags, ownerTestOwnerTag)
	}
	for _, want := range []string{"env--prod", "team--platform", handlers.TagRetainEphemeral} {
		if tagIndex(tags, want) < 2 {
			t.Errorf("tag %q missing or ahead of the owner tag in %q", want, tags)
		}
	}
}

// A request without a director_uuid stamps no owner tag and says so.
func TestCreateVM_NoDirectorUUIDStampsNoOwnerTag(t *testing.T) {
	t.Parallel()

	tags, obs := createVMOwnerTagRun(t, "", map[string]any{"cores": 1, "memory": 512})
	if strings.Contains(tags, "bosh-owner--") {
		t.Errorf("tags = %q; want no owner tag without a director_uuid", tags)
	}
	if !strings.HasPrefix(tags, "bosh-cpi") {
		t.Errorf("tags = %q; want bosh-cpi first", tags)
	}
	if got := countWarnsContaining(obs, "no director_uuid"); got != 1 {
		t.Errorf("got %d warnings about the missing director_uuid, want exactly 1: %+v", got, obs.All())
	}
}

// A cloud property tag cannot claim a CPI-owned prefix. Before the filter,
// {bosh-owner: x} rendered "bosh-owner--x" and named a Director that never
// created the VM.
func TestCreateVM_CloudPropertyTagsCannotClaimCPIPrefixes(t *testing.T) {
	t.Parallel()

	tags, obs := createVMOwnerTagRun(t, ownerTestDirectorUUID, map[string]any{
		"cores": 1, "memory": 512,
		"tags": map[string]any{
			"bosh-owner":   "0a0b0c0d-1111-2222-3333-444455556666",
			"Bosh_Cid":     "pve-1-101",
			"bosh-cluster": "deadbeef",
			"stemcell":     "fake",
			"env":          "prod",
		},
	})
	if n := strings.Count(tags, "bosh-owner--"); n != 1 || tagIndex(tags, ownerTestOwnerTag) != 1 {
		t.Errorf("tags = %q; want exactly the requesting Director's owner tag", tags)
	}
	for _, bad := range []string{"bosh-owner--0a0b0c0d", "Bosh-Cid--", "bosh-cid--", "bosh-cluster--", "stemcell--fake"} {
		if strings.Contains(tags, bad) {
			t.Errorf("tags = %q; operator tag claimed %q", tags, bad)
		}
	}
	if tagIndex(tags, "env--prod") < 0 {
		t.Errorf("tags = %q; the ordinary operator tag was dropped", tags)
	}
	if got := countWarnsContaining(obs, "reserved by the CPI"); got != 4 {
		t.Errorf("got %d reserved-key warnings, want 4: %+v", got, obs.All())
	}
}

// The owner tag create_vm writes survives the first set_vm_metadata, which
// strips and rebuilds every reserved prefix, and stays at the head.
func TestCreateVM_OwnerTagSurvivesFirstMetadataSync(t *testing.T) {
	t.Parallel()

	created, _ := createVMOwnerTagRun(t, ownerTestDirectorUUID, map[string]any{
		"cores": 1, "memory": 512, "tags": map[string]any{"env": "prod"},
	})
	res := ownerTestRun{
		tags:         created,
		directorUUID: ownerTestDirectorUUID,
		metadata:     ownerTestMetadata(),
	}.do(t)
	if res.err != nil {
		t.Fatalf("set_vm_metadata: %v", res.err)
	}
	if tagIndex(res.tags, ownerTestOwnerTag) != 1 {
		t.Errorf("after the first sync tags = %q; want the owner tag right after bosh-cpi", res.tags)
	}
}

// The owner tag is lowercased, so an uppercase director_uuid still yields the
// one canonical form every reader compares against.
func TestCreateVM_OwnerTagIsLowercase(t *testing.T) {
	t.Parallel()

	tags, _ := createVMOwnerTagRun(t, strings.ToUpper(ownerTestDirectorUUID), map[string]any{"cores": 1, "memory": 512})
	if got := tagIndex(tags, ownerTestOwnerTag); got != 1 {
		t.Errorf("tags = %q; want the lowercase %s at position 1", tags, ownerTestOwnerTag)
	}
}
