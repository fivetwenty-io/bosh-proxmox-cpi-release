package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// stampTestLive is the live cluster stamp stampTestNodes reports.
const stampTestLive = "0a1b2c3d"

// stampTestNodes answers the cluster stamp read with a root whose reported
// fingerprint starts with stampTestLive, or with err, and counts the reads.
type stampTestNodes struct {
	sdknodes.Service
	mu    sync.Mutex
	err   error
	reads int
}

func (n *stampTestNodes) ListCertificatesInfo(context.Context, string) (*sdknodes.ListCertificatesInfoResponse, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reads++
	if n.err != nil {
		return nil, n.err
	}
	fp := "0A:1B:2C:3D:" + strings.Repeat("EE:", 27) + "FF"
	raw, err := json.Marshal(map[string]any{"filename": "pve-root-ca.pem", "fingerprint": fp})
	if err != nil {
		return nil, err
	}
	return &sdknodes.ListCertificatesInfoResponse{raw}, nil
}

func (n *stampTestNodes) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.reads
}

// stampTestCluster is a one-member cluster whose index lists rows.
type stampTestCluster struct {
	ownTestCluster
	rows []map[string]any
}

func (c stampTestCluster) ListResources(context.Context, *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	out := make(sdkcluster.ListResourcesResponse, 0, len(c.rows))
	for _, row := range c.rows {
		raw, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return &out, nil
}

// stampTestClient is ownTestClient with a nodes service for the stamp read.
type stampTestClient struct {
	ownTestClient
	nodes *stampTestNodes
}

func (c stampTestClient) Nodes() sdknodes.Service { return c.nodes }

// stampTestDeps returns Deps whose cluster stamp is stampTestLive, or whose
// stamp read fails with readErr, with aliases declared.
func stampTestDeps(q *ownTestQEMU, readErr error, aliases ...string) (Deps, *stampTestNodes) {
	n := &stampTestNodes{err: readErr}
	deps := ownTestDeps(q)
	deps.PVE = stampTestClient{
		ownTestClient: ownTestClient{qemu: q, cluster: stampTestCluster{ownTestCluster: ownTestCluster{nodes: []string{"n1"}}}},
		nodes:         n,
	}
	deps.Config = &config.CPIConfig{Node: "n1", ClusterStampAliases: aliases}
	return deps, n
}

// TestCheckVMOwnership_StampedCIDWithoutClusterTag pins the live identity
// check. A stamped CID that reaches a VM with no cluster tag is ours only
// when its stamp is the live stamp or an alias, the live stamp is read at
// most once and only when the VM's tags cannot settle it, and a failed read
// is retriable rather than a verdict.
func TestCheckVMOwnership_StampedCIDWithoutClusterTag(t *testing.T) {
	t.Parallel()
	ownTags := ownershipTag + ";" + ownerTagFor(ownTestDirector) + ";" + cidStampTagPrefix + "4e5f6a7b"
	cases := []struct {
		name      string
		cid       string
		tags      string
		readErr   error
		aliases   []string
		wantReads int
		want      vmOwnershipVerdict
		// wantDetail is a phrase the verdict's detail must carry.
		wantDetail string
		wantErr    bool
	}{
		{name: "live stamp matches", cid: "101-" + stampTestLive + "-4e5f6a7b", tags: ownTags,
			wantReads: 1, want: vmOwnershipVerdict{Owned: true}},
		{name: "live stamp differs", cid: "101-99aabbcc-4e5f6a7b", tags: ownTags,
			wantReads: 1, want: vmOwnershipVerdict{Reason: ownershipReasonClusterMismatch}},
		{name: "alias matches with no read", cid: "101-99aabbcc-4e5f6a7b", tags: ownTags, aliases: []string{"99aabbcc"},
			wantReads: 0, want: vmOwnershipVerdict{Owned: true}},
		{name: "unknown stamp never matches and never reads", cid: "101-00000000-4e5f6a7b", tags: ownTags,
			wantReads: 0, want: vmOwnershipVerdict{Reason: ownershipReasonClusterMismatch},
			wantDetail: "the unknown cluster stamp 00000000"},
		{name: "cluster tag settles it with no read", cid: "101-" + stampTestLive + "-4e5f6a7b",
			tags: ownTags + ";" + clusterStampTagPrefix + stampTestLive, wantReads: 0, want: vmOwnershipVerdict{Owned: true}},
		{name: "failed read is retriable", cid: "101-" + stampTestLive + "-4e5f6a7b", tags: ownTags,
			readErr: errors.New("connection refused"), wantReads: 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &ownTestQEMU{}
			deps, n := stampTestDeps(q, tc.readErr, tc.aliases...)
			loc := pve.VMLocation{Node: "n1", Found: true, Tags: tc.tags, Type: resourceTypeQemu}
			got, err := checkVMOwnership(context.Background(), deps, "test", mustParseCID(t, tc.cid), loc,
				map[string]any{"tags": tc.tags})
			if n.count() != tc.wantReads {
				t.Errorf("stamp reads = %d, want %d", n.count(), tc.wantReads)
			}
			if tc.wantErr {
				if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
					t.Fatalf("checkVMOwnership = (%+v, %v), want a retriable error", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Owned != tc.want.Owned || got.Reason != tc.want.Reason {
				t.Errorf("verdict = %+v, want %+v", got, tc.want)
			}
			if !strings.Contains(got.Detail, tc.wantDetail) {
				t.Errorf("detail %q, want one containing %q", got.Detail, tc.wantDetail)
			}
		})
	}
}

// TestClusterStampProbe_ReadsOnce pins that a probe reads the live stamp once
// however often it is asked, and keeps a failure rather than reading again.
func TestClusterStampProbe_ReadsOnce(t *testing.T) {
	t.Parallel()
	deps, n := stampTestDeps(&ownTestQEMU{}, nil)
	probe := newClusterStampProbe(context.Background(), deps, "n1")
	for _, stamp := range []string{stampTestLive, "99aabbcc", stampTestLive} {
		if _, err := probe.matches(stamp); err != nil {
			t.Fatalf("matches(%q): %v", stamp, err)
		}
	}
	if n.count() != 1 {
		t.Errorf("stamp reads = %d, want 1", n.count())
	}

	failDeps, failNodes := stampTestDeps(&ownTestQEMU{}, errors.New("boom"))
	failing := newClusterStampProbe(context.Background(), failDeps, "n1")
	for range 2 {
		if same, err := failing.matches(stampTestLive); err == nil || same {
			t.Fatalf("matches on a failed read = (%v, %v), want an error", same, err)
		}
	}
	if failNodes.count() != 1 {
		t.Errorf("stamp reads after a failure = %d, want 1", failNodes.count())
	}

	noClient := newClusterStampProbe(context.Background(), Deps{Config: &config.CPIConfig{}}, "n1")
	if _, err := noClient.matches(stampTestLive); !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("matches with no PVE client = %v, want a retriable error", err)
	}
}

// TestAbsentVMVerdict pins the judgement of a CID whose VMID is absent.
func TestAbsentVMVerdict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		cid           string
		readErr       error
		aliases       []string
		wantReads     int
		wantAbsent    bool
		wantElsewhere bool
		wantErr       bool
	}{
		{name: "bare CID", cid: "101", wantAbsent: true},
		{name: "unknown stamp", cid: "101-00000000-4e5f6a7b", wantAbsent: true},
		{name: "unknown stamp with a broken read", cid: "101-00000000-4e5f6a7b", readErr: errors.New("boom"), wantAbsent: true},
		{name: "this cluster", cid: "101-" + stampTestLive + "-4e5f6a7b", wantReads: 1, wantAbsent: true},
		{name: "alias", cid: "101-99aabbcc-4e5f6a7b", aliases: []string{"99aabbcc"}, wantAbsent: true},
		{name: "other cluster", cid: "101-99aabbcc-4e5f6a7b", wantReads: 1, wantElsewhere: true},
		{name: "broken read", cid: "101-99aabbcc-4e5f6a7b", readErr: errors.New("boom"), wantReads: 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, n := stampTestDeps(&ownTestQEMU{}, tc.readErr, tc.aliases...)
			got, err := absentVMVerdict(context.Background(), deps, "test", mustParseCID(t, tc.cid))
			if n.count() != tc.wantReads {
				t.Errorf("stamp reads = %d, want %d", n.count(), tc.wantReads)
			}
			if tc.wantErr {
				if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
					t.Fatalf("absentVMVerdict = (%+v, %v), want a retriable error", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Absent != tc.wantAbsent || got.Elsewhere != tc.wantElsewhere {
				t.Errorf("verdict = %+v, want Absent=%v Elsewhere=%v", got, tc.wantAbsent, tc.wantElsewhere)
			}
			if tc.wantElsewhere {
				if got.Owned || got.Gone || got.Reason != ownershipReasonClusterMismatch || !got.foreign() {
					t.Errorf("verdict = %+v, want a foreign cluster-mismatch", got)
				}
				for _, want := range []string{"names cluster 99aabbcc", "this cluster's stamp is " + stampTestLive} {
					if !strings.Contains(got.Detail, want) {
						t.Errorf("detail %q does not contain %q", got.Detail, want)
					}
				}
			}
		})
	}
}

// TestAbsentVMVerdict_HintCountsTaggedGuests pins that only guests carrying
// the CID's cluster tag count toward the hint, and that a failed count leaves
// the verdict alone.
func TestAbsentVMVerdict_HintCountsTaggedGuests(t *testing.T) {
	t.Parallel()
	deps, _ := stampTestDeps(&ownTestQEMU{}, nil)
	client := deps.PVE.(stampTestClient)
	client.cluster = stampTestCluster{
		ownTestCluster: ownTestCluster{nodes: []string{"n1"}},
		rows: []map[string]any{
			{"vmid": 102, "type": "qemu", "tags": "bosh-cpi;bosh-cluster--99aabbcc"},
			{"vmid": 103, "type": "qemu", "tags": "bosh-cpi;bosh-cluster--" + stampTestLive},
		},
	}
	deps.PVE = client
	got, err := absentVMVerdict(context.Background(), deps, "test", mustParseCID(t, "101-99aabbcc-4e5f6a7b"))
	if err != nil || !got.Elsewhere {
		t.Fatalf("absentVMVerdict = (%+v, %v), want Elsewhere", got, err)
	}
	if !strings.Contains(got.Detail, "1 guests on this cluster carry the tag bosh-cluster--99aabbcc") {
		t.Errorf("detail %q does not count the one tagged guest", got.Detail)
	}

	noIndex, _ := stampTestDeps(&ownTestQEMU{}, nil)
	bare := noIndex.PVE.(stampTestClient)
	bare.cluster = nil
	noIndex.PVE = bare
	got, err = absentVMVerdict(context.Background(), noIndex, "test", mustParseCID(t, "101-99aabbcc-4e5f6a7b"))
	if err != nil || !got.Elsewhere || strings.Contains(got.Detail, "guests on this cluster") {
		t.Errorf("absentVMVerdict with no index = (%+v, %v), want Elsewhere and no hint", got, err)
	}
}

func TestDirectorVMCID(t *testing.T) {
	t.Parallel()
	stamped := &config.CPIConfig{}
	bare := &config.CPIConfig{VMCIDFormat: config.VMCIDFormatBare}
	cases := []struct {
		name string
		cfg  *config.CPIConfig
		tags string
		want string
	}{
		{"stamped from tags", stamped, "bosh-cpi;bosh-cluster--0a1b2c3d;bosh-cid--4e5f6a7b", "101-0a1b2c3d-4e5f6a7b"},
		{"tag case is ignored", stamped, "bosh-cpi;BOSH-CLUSTER--0A1B2C3D;bosh-cid--4E5F6A7B", "101-0a1b2c3d-4e5f6a7b"},
		{"duplicate equal tags", stamped, "bosh-cluster--0a1b2c3d;bosh-cluster--0a1b2c3d;bosh-cid--4e5f6a7b", "101-0a1b2c3d-4e5f6a7b"},
		{"nil config is stamped", nil, "bosh-cluster--0a1b2c3d;bosh-cid--4e5f6a7b", "101-0a1b2c3d-4e5f6a7b"},
		{"bare format", bare, "bosh-cluster--0a1b2c3d;bosh-cid--4e5f6a7b", "101"},
		{"no tags", stamped, "", "101"},
		{"no token tag", stamped, "bosh-cluster--0a1b2c3d", "101"},
		{"no cluster tag", stamped, "bosh-cid--4e5f6a7b", "101"},
		{"two cluster values", stamped, "bosh-cluster--0a1b2c3d;bosh-cluster--99aabbcc;bosh-cid--4e5f6a7b", "101"},
		{"two token values", stamped, "bosh-cluster--0a1b2c3d;bosh-cid--4e5f6a7b;bosh-cid--11111111", "101"},
		{"invalid stamp value", stamped, "bosh-cluster--0a1b2c3;bosh-cid--4e5f6a7b", "101"},
		{"invalid token value", stamped, "bosh-cluster--0a1b2c3d;bosh-cid--4e5f6a7z", "101"},
	}
	for _, tc := range cases {
		if got := directorVMCID(tc.cfg, 101, tc.tags); got != tc.want {
			t.Errorf("%s: directorVMCID = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestResumedVMCID(t *testing.T) {
	t.Parallel()
	deps := Deps{Config: &config.CPIConfig{}}
	cfg := map[string]any{"tags": "bosh-cpi;bosh-cluster--0a1b2c3d;bosh-cid--4e5f6a7b"}
	if got := resumedVMCID(deps, "101", cfg); got != "101-0a1b2c3d-4e5f6a7b" {
		t.Errorf("resumedVMCID = %q, want the stamped CID rebuilt from the tags", got)
	}
	if got := resumedVMCID(deps, "101", map[string]any{"tags": "bosh-cpi"}); got != "101" {
		t.Errorf("resumedVMCID for an unstamped VM = %q, want 101", got)
	}
	bareDeps := Deps{Config: &config.CPIConfig{VMCIDFormat: config.VMCIDFormatBare}}
	if got := resumedVMCID(bareDeps, "101", cfg); got != "101" {
		t.Errorf("resumedVMCID under bare = %q, want 101", got)
	}
	if got := resumedVMCID(deps, "not-a-vmid", cfg); got != "not-a-vmid" {
		t.Errorf("resumedVMCID for a non-VMID record = %q, want it unchanged", got)
	}
}

func TestRequireStampTags(t *testing.T) {
	t.Parallel()
	parsed := &createVMParsedArgs{clusterStamp: "0a1b2c3d", cidToken: "4e5f6a7b"}
	if err := requireStampTags(parsed, "bosh-cpi;bosh-cluster--0a1b2c3d;bosh-cid--4e5f6a7b;web"); err != nil {
		t.Errorf("both stamp tags kept: %v", err)
	}
	for _, kept := range []string{"bosh-cpi;bosh-cluster--0a1b2c3d", "bosh-cpi", "bosh-cpi;bosh-cluster--0a1b2c3d;bosh-cid--4e5f6a7"} {
		err := requireStampTags(parsed, kept)
		if err == nil || !cpierrors.IsType(err, cpierrors.TypeCloud) {
			t.Errorf("requireStampTags(%q) = %v, want a Cloud error", kept, err)
		}
	}
	if err := requireStampTags(&createVMParsedArgs{}, "bosh-cpi"); err != nil {
		t.Errorf("an unstamped shape has no tags to require: %v", err)
	}
}

func TestInitialTagHead_Order(t *testing.T) {
	t.Parallel()
	deps := Deps{Config: &config.CPIConfig{}, RequestDirectorUUID: ownTestDirector}
	parsed := &createVMParsedArgs{clusterStamp: "0a1b2c3d", cidToken: "4e5f6a7b"}
	want := []string{ownershipTag, ownerTagFor(ownTestDirector), clusterStampTagPrefix + "0a1b2c3d", cidStampTagPrefix + "4e5f6a7b"}
	if got := initialTagHead(deps, parsed); strings.Join(got, ";") != strings.Join(want, ";") {
		t.Errorf("initialTagHead = %v, want %v", got, want)
	}
	deps.RequestDirectorUUID = ""
	if got := initialTagHead(deps, parsed); strings.Join(got, ";") != strings.Join(want[:1], ";")+";"+strings.Join(want[2:], ";") {
		t.Errorf("initialTagHead with no Director UUID = %v", got)
	}
	if got := initialTagHead(deps, nil); len(got) != 1 || got[0] != ownershipTag {
		t.Errorf("initialTagHead with no stamp = %v, want only %s", got, ownershipTag)
	}
}

func TestResolveCreateStamp(t *testing.T) {
	t.Parallel()
	deps, _ := stampTestDeps(&ownTestQEMU{}, nil)
	parsed := &createVMParsedArgs{}
	if err := resolveCreateStamp(context.Background(), deps, parsed, log.NewNopLogger()); err != nil {
		t.Fatalf("resolveCreateStamp: %v", err)
	}
	if parsed.clusterStamp != stampTestLive || !vmcid.IsStamp(parsed.cidToken) {
		t.Errorf("stamp, token = %q, %q, want %q and a token", parsed.clusterStamp, parsed.cidToken, stampTestLive)
	}

	failing, _ := stampTestDeps(&ownTestQEMU{}, errors.New("boom"))
	parsed = &createVMParsedArgs{}
	if err := resolveCreateStamp(context.Background(), failing, parsed, log.NewNopLogger()); err != nil {
		t.Fatalf("resolveCreateStamp with a broken read: %v", err)
	}
	if parsed.clusterStamp != vmcid.UnknownCluster || !vmcid.IsStamp(parsed.cidToken) {
		t.Errorf("stamp, token = %q, %q, want the unknown stamp and a token", parsed.clusterStamp, parsed.cidToken)
	}

	parsed = &createVMParsedArgs{}
	if err := resolveCreateStamp(context.Background(), Deps{}, parsed, log.NewNopLogger()); err != nil {
		t.Fatalf("resolveCreateStamp with no client: %v", err)
	}
	if parsed.clusterStamp != vmcid.UnknownCluster {
		t.Errorf("stamp with no client = %q, want the unknown stamp", parsed.clusterStamp)
	}

	if err := resolveCreateStamp(context.Background(), deps, nil, log.NewNopLogger()); err == nil {
		t.Error("resolveCreateStamp(nil) succeeded")
	}
}

// TestRefuseUnownedStableIDDetach_UnusedEntryOnAbsentVM pins the answer a
// stable-ID detach gives when the disk is only an unused entry and the VM the
// CID names is gone. The index can miss the VMID, or list it while every
// member's config answers not-found, and in both cases a CID naming another
// cluster is refused, because the disk may be attached to its VM there. A CID
// naming this cluster or no cluster has nothing to edit and goes on.
func TestRefuseUnownedStableIDDetach_UnusedEntryOnAbsentVM(t *testing.T) {
	t.Parallel()
	rd := resolvedDisk{unused: []pve.VolumeReference{{}}}
	for _, listed := range []bool{false, true} {
		for _, tc := range []struct {
			cid     string
			refused bool
		}{
			{cid: "101-99aabbcc-4e5f6a7b", refused: true},
			{cid: "101-" + stampTestLive + "-4e5f6a7b"},
			{cid: "101"},
		} {
			deps, _ := stampTestDeps(&ownTestQEMU{err: sdkerrors.ErrNotFound}, nil)
			if listed {
				client := deps.PVE.(stampTestClient)
				client.cluster = stampTestCluster{
					ownTestCluster: ownTestCluster{nodes: []string{"n1"}},
					rows:           []map[string]any{{"vmid": 101, "type": "qemu", "node": "n1", "tags": ownershipTag}},
				}
				deps.PVE = client
			}
			err := refuseUnownedStableIDDetach(context.Background(), deps, tc.cid, mustParseCID(t, tc.cid), rd)
			switch {
			case tc.refused && (!cpierrors.IsType(err, cpierrors.TypeCloud) || !strings.Contains(err.Error(), "cluster-mismatch")):
				t.Errorf("listed=%v cid %s: err = %v, want the cluster-mismatch refusal", listed, tc.cid, err)
			case !tc.refused && err != nil:
				t.Errorf("listed=%v cid %s: unexpected error: %v", listed, tc.cid, err)
			}
		}
	}
}
