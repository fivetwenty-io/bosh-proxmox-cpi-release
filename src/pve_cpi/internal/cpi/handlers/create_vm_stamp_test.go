package handlers_test

// Pins for the cluster stamp and token create_vm writes and returns. The VM
// always carries both tags right after its ownership tags, and the CID the
// Director gets names them, unless vm_cid_format is bare, which returns the
// bare VMID and still writes the tags.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// runStampCreate runs one direct-import create_vm and returns its CID and the
// tags the VM was created with. edit changes the Deps before the call. On an
// error it also checks that no VM was created.
func runStampCreate(t *testing.T, rpc jsonrpc.Context, edit func(*handlers.Deps, *vmMockNodes)) (string, string, error) {
	t.Helper()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	deps := buildVMDeps(q, n, &vmMockCluster{}, &vmMockAgent{})
	if edit != nil {
		edit(&deps, n)
	}
	args := mkArgs("agent-uuid-1", testStemcellCID,
		map[string]any{"cores": 2, "memory": 1024},
		map[string]any{"default": map[string]any{
			"type": "manual", "ip": "10.0.0.5",
			"netmask": "255.255.255.0", "gateway": "10.0.0.1",
			"dns": []string{"8.8.8.8"}, "default": []string{"dns", "gateway"},
			"cloud_properties": map[string]any{"bridge": "vmbr0"},
		}},
		[]string{}, map[string]any{})
	result, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, rpc)
	if err != nil {
		if len(q.createCalls) != 0 {
			t.Errorf("create_vm failed with %v after creating %d VMs", err, len(q.createCalls))
		}
		return "", "", err
	}
	tuple, ok := result.([]any)
	if !ok || len(tuple) != 2 {
		t.Fatalf("create_vm result = %#v, want [vm_cid, networks]", result)
	}
	cid, _ := tuple[0].(string)
	if len(q.createCalls) != 1 {
		t.Fatalf("QEMU.Create calls = %d, want 1", len(q.createCalls))
	}
	tags, _ := q.createCalls[0].params["tags"].(string)
	return cid, tags, nil
}

// stampHead returns the first n entries of a tag list.
func stampHead(tags string, n int) []string {
	parts := strings.Split(tags, ";")
	if len(parts) > n {
		parts = parts[:n]
	}
	return parts
}

func TestCreateVM_StampTagsFollowTheOwnershipTags(t *testing.T) {
	t.Parallel()
	rpc := jsonrpc.Context{RequestID: "stamp-1", DirectorUUID: ownRequestDirector}
	cid, tags, err := runStampCreate(t, rpc, nil)
	if err != nil {
		t.Fatalf("create_vm: %v", err)
	}
	parsed, err := vmcid.Parse(cid)
	if err != nil || !parsed.Stamped() {
		t.Fatalf("vm_cid %q is not a stamped CID: %v", cid, err)
	}
	want := []string{
		"bosh-cpi",
		"bosh-owner--" + ownRequestDirector,
		"bosh-cluster--" + testClusterStamp,
		"bosh-cid--" + parsed.Token,
	}
	if got := stampHead(tags, 4); strings.Join(got, ";") != strings.Join(want, ";") {
		t.Errorf("tag head = %v, want %v (full list %q)", got, want, tags)
	}
	if parsed.Cluster != testClusterStamp {
		t.Errorf("vm_cid %q names cluster %q, want %q", cid, parsed.Cluster, testClusterStamp)
	}
}

func TestCreateVM_TokensDifferPerVM(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 4 {
		cid, _, err := runStampCreate(t, mkCtx("stamp-tokens"), nil)
		if err != nil {
			t.Fatalf("create_vm: %v", err)
		}
		parsed, err := vmcid.Parse(cid)
		if err != nil {
			t.Fatalf("vm_cid %q: %v", cid, err)
		}
		seen[parsed.Token] = true
	}
	if len(seen) != 4 {
		t.Errorf("four creates produced %d distinct tokens, want 4", len(seen))
	}
}

func TestCreateVM_BareFormatStillWritesTheTags(t *testing.T) {
	t.Parallel()
	cid, tags, err := runStampCreate(t, mkCtx("stamp-bare"), func(deps *handlers.Deps, _ *vmMockNodes) {
		deps.Config.VMCIDFormat = config.VMCIDFormatBare
	})
	if err != nil {
		t.Fatalf("create_vm: %v", err)
	}
	vmid, ok := vmcid.VMID(cid)
	if !ok || cid != vmcid.PathOf(vmid) {
		t.Fatalf("vm_cid %q under vm_cid_format: bare, want the bare VMID", cid)
	}
	_, token := withoutTokenTag(tags)
	if !strings.Contains(tags, "bosh-cluster--"+testClusterStamp) || !vmcid.IsStamp(token) {
		t.Errorf("tags %q under vm_cid_format: bare must still carry the cluster and token tags", tags)
	}
}

func TestCreateVM_UnreadableIdentityStampsTheUnknownCluster(t *testing.T) {
	t.Parallel()
	var logs lockedBuffer
	cid, tags, err := runStampCreate(t, mkCtx("stamp-unknown"), func(deps *handlers.Deps, n *vmMockNodes) {
		n.certificatesInfoFn = func(context.Context, string) (*sdknodes.ListCertificatesInfoResponse, error) {
			return nil, errors.New("connection refused")
		}
		logger, lerr := log.NewLogger("warn", &logs)
		if lerr != nil {
			t.Fatalf("logger: %v", lerr)
		}
		deps.Logger = logger
	})
	if err != nil {
		t.Fatalf("create_vm must not fail on an unreadable cluster identity: %v", err)
	}
	parsed, err := vmcid.Parse(cid)
	if err != nil || parsed.Cluster != vmcid.UnknownCluster || !vmcid.IsStamp(parsed.Token) {
		t.Fatalf("vm_cid %q, want the unknown cluster stamp %s and a token", cid, vmcid.UnknownCluster)
	}
	if !strings.Contains(tags, "bosh-cluster--"+vmcid.UnknownCluster) {
		t.Errorf("tags %q must carry the unknown cluster stamp", tags)
	}
	if logs.countAt(t, "WARN", "stamped with the unknown cluster") != 1 {
		t.Errorf("create_vm did not warn once about the unknown stamp: %v", logs.records(t))
	}
}

func TestCreateVM_RefusesWhenTheCapWouldDropAStampTag(t *testing.T) {
	t.Parallel()
	// A Director UUID this long fills the 350-byte tag cap before the stamp
	// tags, and a VM without them would get a CID its own checks refuse.
	rpc := jsonrpc.Context{RequestID: "stamp-cap", DirectorUUID: strings.Repeat("a", 330)}
	_, _, err := runStampCreate(t, rpc, nil)
	if err == nil {
		t.Fatal("create_vm succeeded with a tag list that drops a stamp tag")
	}
	if !strings.Contains(err.Error(), "350-byte cap") || !strings.Contains(err.Error(), "bosh-cluster--") {
		t.Errorf("error %q must name the cap and the dropped stamp tag", err.Error())
	}
}
