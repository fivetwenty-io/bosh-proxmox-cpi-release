package pve_test

import (
	"context"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

func TestReplicaTagSanitizersAgree(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"pvuproxcf1_ns_1", "NFS.Fast", "a--b", "-lead", "trail-", "UP_per", "local"} {
		if got, want := pve.ReplicaStorageTagPrefix+config.ReplicaTagPart(in), pve.ReplicaStorageTagForStorage(in); got != want {
			t.Errorf("%q: config %q, pve %q", in, got, want)
		}
	}
}

func TestReplicaStorageTagForStorage(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"pvuproxcf1_ns_1": "bosh-stemcell-storage-pvuproxcf1-ns-1",
		"NFS.Fast":        "bosh-stemcell-storage-nfs-fast",
		"local":           "bosh-stemcell-storage-local",
	}
	for in, want := range cases {
		if got := pve.ReplicaStorageTagForStorage(in); got != want {
			t.Errorf("%q: want %q, got %q", in, want, got)
		}
	}
}

func TestTemplateRefReplicaKinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tags                string
		replica, storageRep bool
	}{
		{"bosh-cpi;bosh-stemcell-cache;bosh-stemcell-sha-abcd1234", false, false},
		{"bosh-stemcell-sha-abcd1234;bosh-stemcell-node-n2", true, false},
		{"bosh-stemcell-sha-abcd1234;bosh-stemcell-storage-ns-2", true, true},
		{"bosh-stemcell-node-n2;bosh-stemcell-storage-ns-2", true, true},
	}
	for _, tc := range cases {
		r := pve.TemplateRef{Tags: tc.tags}
		if r.IsReplica() != tc.replica || r.IsStorageReplica() != tc.storageRep {
			t.Errorf("%q: IsReplica=%v IsStorageReplica=%v", tc.tags, r.IsReplica(), r.IsStorageReplica())
		}
	}
}

func TestSelectStorageReplica(t *testing.T) {
	t.Parallel()
	refs := []pve.TemplateRef{
		{VMID: 30010, Node: "n1", Tags: "bosh-stemcell-cache;bosh-stemcell-sha-abcd1234"},
		{VMID: 30012, Node: "n1", Tags: "bosh-stemcell-cache;bosh-stemcell-sha-abcd1234;bosh-stemcell-storage-ns-2"},
		{VMID: 30011, Node: "n2", Tags: "bosh-stemcell-cache;bosh-stemcell-sha-abcd1234;bosh-stemcell-storage-ns-2"},
		{VMID: 30009, Node: "n1", Tags: "bosh-stemcell-cache;bosh-stemcell-sha-abcd1234;bosh-stemcell-storage-ns-2x"},
	}
	got, ok := pve.SelectStorageReplica(refs, "bosh-stemcell-storage-ns-2")
	if !ok || got.VMID != 30011 {
		t.Fatalf("want 30011 found, got %+v found=%v", got, ok)
	}
	if _, ok := pve.SelectStorageReplica(refs, "bosh-stemcell-storage-ns-3"); ok {
		t.Fatal("want not found for an unmatched tag")
	}
	if _, ok := pve.SelectStorageReplica(nil, "bosh-stemcell-storage-ns-2"); ok {
		t.Fatal("want not found for nil refs")
	}
	if _, ok := pve.SelectStorageReplica(refs, ""); ok {
		t.Fatal("want not found for an empty tag")
	}
}

func TestResolveTemplateVMIDForNodeSkipsStorageReplicas(t *testing.T) {
	t.Parallel()
	c := resolveTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponseRaw(
			`{"vmid":30012,"template":1,"tags":"bosh-stemcell-cache;bosh-stemcell-sha-abcd1234;bosh-stemcell-storage-ns-2"}`,
		), nil
	})
	_, found, err := pve.ResolveTemplateVMIDForNode(context.Background(), c, "pve1", "abcd1234")
	if err != nil || found {
		t.Fatalf("a storage replica must not resolve as the node primary, got found=%v err=%v", found, err)
	}
}

func TestResolveTemplateVMIDForNodePrefersPrimaryOverStorageReplica(t *testing.T) {
	t.Parallel()
	c := resolveTemplateClient(func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		return makeListQemuResponseRaw(
			`{"vmid":30009,"template":1,"tags":"bosh-stemcell-cache;bosh-stemcell-sha-abcd1234;bosh-stemcell-storage-ns-2"}`,
			`{"vmid":30010,"template":1,"tags":"bosh-stemcell-cache;bosh-stemcell-sha-abcd1234"}`,
		), nil
	})
	vmid, found, err := pve.ResolveTemplateVMIDForNode(context.Background(), c, "pve1", "abcd1234")
	if err != nil || !found || vmid != 30010 {
		t.Fatalf("want primary 30010, got vmid=%d found=%v err=%v", vmid, found, err)
	}
}
