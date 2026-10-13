package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// The predicate pins. Each row of the ownership table in the plan gets a case,
// in the order ownVM evaluates them, plus the edges the table leaves implicit:
// containers, two owner tags, an empty request UUID, the template flag from a
// scan row, mixed token tags, and the notes rules.

const (
	ownTestDirector = "11111111-2222-3333-4444-555555555555"
	ownTestOther    = "99999999-8888-7777-6666-555555555555"
	ownTestStamped  = "101-0a1b2c3d-4e5f6a7b"
)

// directorNotes is what set_vm_metadata writes into a VM's notes.
const directorNotes = "<!--BOSH:managed-->\ndirector: bosh\ndeployment: cf\nname: router/abc-123\nindex: 0\n"

func mustParseCID(t *testing.T, raw string) vmcid.VMCID {
	t.Helper()
	cid, err := vmcid.Parse(raw)
	if err != nil {
		t.Fatalf("vmcid.Parse(%q): %v", raw, err)
	}
	return cid
}

func notesFunc(desc string) func() (string, error) {
	return func() (string, error) { return desc, nil }
}

func failNotes(t *testing.T) func() (string, error) {
	return func() (string, error) {
		t.Helper()
		t.Error("ownVM read the notes, but the verdict should not depend on them")
		return "", nil
	}
}

func TestOwnVM_PredicateTable(t *testing.T) {
	t.Parallel()

	owner := ownerTagFor(ownTestDirector)
	other := ownerTagFor(ownTestOther)
	cluster := clusterStampTagPrefix + "0a1b2c3d"
	token := cidStampTagPrefix + "4e5f6a7b"

	cases := []struct {
		name       string
		cid        string
		request    string
		tags       []string
		template   bool
		typ        string
		notes      string
		readsNotes bool
		matches    func(string) (bool, error)
		want       vmOwnershipVerdict
	}{
		// Row 1: cluster.
		{name: "stamped, cluster tag differs", cid: ownTestStamped, request: ownTestDirector,
			tags: []string{ownershipTag, owner, clusterStampTagPrefix + "deadbeef", token},
			want: vmOwnershipVerdict{Reason: ownershipReasonClusterMismatch}},
		{name: "stamped, two cluster tags, one differs", cid: ownTestStamped, request: ownTestDirector,
			tags: []string{ownershipTag, owner, cluster, clusterStampTagPrefix + "deadbeef", token},
			want: vmOwnershipVerdict{Reason: ownershipReasonClusterMismatch}},
		{name: "stamped, no cluster tag, no live identity", cid: ownTestStamped, request: ownTestDirector,
			tags: []string{ownershipTag, owner, token},
			want: vmOwnershipVerdict{Reason: ownershipReasonClusterMismatch}},
		{name: "stamped, no cluster tag, live identity differs", cid: ownTestStamped, request: ownTestDirector,
			tags:    []string{ownershipTag, owner, token},
			matches: func(string) (bool, error) { return false, nil },
			want:    vmOwnershipVerdict{Reason: ownershipReasonClusterMismatch}},
		{name: "stamped, no cluster tag, live identity matches", cid: ownTestStamped, request: ownTestDirector,
			tags:    []string{ownershipTag, owner, token},
			matches: func(stamp string) (bool, error) { return stamp == "0a1b2c3d", nil },
			want:    vmOwnershipVerdict{Owned: true}},
		// Row 2: token mismatch on a proven cluster means the CID's VM is gone.
		{name: "stamped, token differs", cid: ownTestStamped, request: ownTestDirector,
			tags: []string{ownershipTag, owner, cluster, cidStampTagPrefix + "ffffffff"},
			want: vmOwnershipVerdict{Gone: true, Reason: ownershipReasonTokenMismatch}},
		{name: "stamped, token differs on a foreign VM is still gone", cid: ownTestStamped, request: ownTestDirector,
			tags: []string{other, cluster, cidStampTagPrefix + "ffffffff"},
			want: vmOwnershipVerdict{Gone: true, Reason: ownershipReasonTokenMismatch}},
		// Row 3: token missing.
		{name: "stamped, no token tag", cid: ownTestStamped, request: ownTestDirector,
			tags: []string{ownershipTag, owner, cluster},
			want: vmOwnershipVerdict{Reason: ownershipReasonTokenMissing}},
		{name: "stamped, mixed token tags", cid: ownTestStamped, request: ownTestDirector,
			tags: []string{ownershipTag, owner, cluster, token, cidStampTagPrefix + "ffffffff"},
			want: vmOwnershipVerdict{Reason: ownershipReasonTokenConflict}},
		{name: "stamped, everything matches", cid: ownTestStamped, request: ownTestDirector,
			tags: []string{ownershipTag, owner, cluster, token},
			want: vmOwnershipVerdict{Owned: true}},
		// Row 4: parker.
		{name: "parker", cid: "101", request: ownTestDirector,
			tags: []string{ownershipTag, owner, pve.ParkerTag},
			want: vmOwnershipVerdict{Reason: ownershipReasonParker}},
		{name: "stamped parker with matching stamps", cid: ownTestStamped, request: ownTestDirector,
			tags: []string{ownershipTag, owner, cluster, token, pve.ParkerTag},
			want: vmOwnershipVerdict{Reason: ownershipReasonParker}},
		// Row 5: template.
		{name: "template flag from the scan row", cid: "101", request: ownTestDirector,
			tags: []string{ownershipTag, owner}, template: true,
			want: vmOwnershipVerdict{Reason: ownershipReasonTemplate}},
		{name: "stemcell cache tag", cid: "101", request: ownTestDirector,
			tags: []string{ownershipTag, stemcellCacheTag},
			want: vmOwnershipVerdict{Reason: ownershipReasonTemplate}},
		{name: "stemcell marker tag", cid: "101", request: ownTestDirector,
			tags: []string{ownershipTag, stemcellMarkerTag},
			want: vmOwnershipVerdict{Reason: ownershipReasonTemplate}},
		// Row 6: owner.
		{name: "owner tag names another Director", cid: "101", request: ownTestDirector,
			tags: []string{ownershipTag, other},
			want: vmOwnershipVerdict{Reason: ownershipReasonOtherDirector}},
		{name: "owner tag names the requester", cid: "101", request: ownTestDirector,
			tags: []string{ownershipTag, owner},
			want: vmOwnershipVerdict{Owned: true}},
		{name: "owner tag matches without bosh-cpi", cid: "101", request: ownTestDirector,
			tags: []string{owner},
			want: vmOwnershipVerdict{Owned: true}},
		{name: "owner tag compared without regard to case", cid: "101", request: strings.ToUpper(ownTestDirector),
			tags: []string{ownershipTag, owner},
			want: vmOwnershipVerdict{Owned: true}},
		{name: "two owner tags, one is the requester", cid: "101", request: ownTestDirector,
			tags: []string{ownershipTag, owner, other},
			want: vmOwnershipVerdict{Reason: ownershipReasonOtherDirector}},
		{name: "two owner tags, empty request UUID", cid: "101", request: "",
			tags: []string{ownershipTag, owner, other},
			want: vmOwnershipVerdict{Reason: ownershipReasonOtherDirector}},
		{name: "one owner tag, empty request UUID", cid: "101", request: "",
			tags: []string{ownershipTag, other},
			want: vmOwnershipVerdict{Owned: true}},
		{name: "the same owner tag twice", cid: "101", request: ownTestDirector,
			tags: []string{ownershipTag, owner, owner},
			want: vmOwnershipVerdict{Owned: true}},
		// Row 7: not BOSH.
		{name: "no marker at all", cid: "101", request: ownTestDirector,
			tags: nil, notes: "a hand-made VM", readsNotes: true,
			want: vmOwnershipVerdict{Reason: ownershipReasonNotBOSH}},
		{name: "sentinel alone does not count", cid: "101", request: ownTestDirector,
			tags: []string{"web"}, notes: "<!--BOSH:managed-->\n", readsNotes: true,
			want: vmOwnershipVerdict{Reason: ownershipReasonNotBOSH}},
		{name: "partial Director lines do not count", cid: "101", request: ownTestDirector,
			notes: "director: bosh\ndeployment: cf\n", readsNotes: true,
			want: vmOwnershipVerdict{Reason: ownershipReasonNotBOSH}},
		{name: "name line without an instance id does not count", cid: "101", request: ownTestDirector,
			notes: "director: bosh\ndeployment: cf\nname: router\n", readsNotes: true,
			want: vmOwnershipVerdict{Reason: ownershipReasonNotBOSH}},
		{name: "LXC container", cid: "101", request: ownTestDirector,
			tags: []string{ownershipTag, owner}, typ: "lxc",
			want: vmOwnershipVerdict{Reason: ownershipReasonNotBOSH}},
		{name: "LXC container without tags", cid: "101", request: ownTestDirector,
			typ:  "lxc",
			want: vmOwnershipVerdict{Reason: ownershipReasonNotBOSH}},
		// Row 8: legacy.
		{name: "legacy bosh-cpi tag", cid: "101", request: ownTestDirector,
			tags: []string{ownershipTag},
			want: vmOwnershipVerdict{Owned: true, Legacy: true}},
		{name: "legacy Director notes", cid: "101", request: ownTestDirector,
			notes: directorNotes, readsNotes: true,
			want: vmOwnershipVerdict{Owned: true, Legacy: true}},
		{name: "legacy Director notes with CRLF", cid: "101", request: ownTestDirector,
			notes: strings.ReplaceAll(directorNotes, "\n", "\r\n"), readsNotes: true,
			want: vmOwnershipVerdict{Owned: true, Legacy: true}},
		{name: "legacy bosh-deleting is still judged by its tags", cid: "101", request: ownTestDirector,
			tags: []string{ownershipTag, "bosh-deleting"},
			want: vmOwnershipVerdict{Owned: true, Legacy: true}},
		{name: "another Director's bosh-deleting VM stays foreign", cid: "101", request: ownTestDirector,
			tags: []string{ownershipTag, other, "bosh-deleting"},
			want: vmOwnershipVerdict{Reason: ownershipReasonOtherDirector}},
		{name: "qemu type is not foreign", cid: "101", request: ownTestDirector,
			tags: []string{ownershipTag}, typ: resourceTypeQemu,
			want: vmOwnershipVerdict{Owned: true, Legacy: true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			view := vmOwnershipView{
				Tags:           tc.tags,
				Template:       tc.template,
				Type:           tc.typ,
				description:    failNotes(t),
				clusterMatches: tc.matches,
			}
			if tc.readsNotes {
				view.description = notesFunc(tc.notes)
			}
			got, err := ownVM(mustParseCID(t, tc.cid), tc.request, view)
			if err != nil {
				t.Fatalf("ownVM: unexpected error: %v", err)
			}
			if got.Owned != tc.want.Owned || got.Legacy != tc.want.Legacy || got.Gone != tc.want.Gone || got.Reason != tc.want.Reason {
				t.Errorf("ownVM = %+v, want Owned=%v Legacy=%v Gone=%v Reason=%q",
					got, tc.want.Owned, tc.want.Legacy, tc.want.Gone, tc.want.Reason)
			}
			if got.Reason != "" && got.Detail == "" {
				t.Errorf("verdict %q carries no detail", got.Reason)
			}
		})
	}
}

// TestOwnVM_ErrorsAreNotVerdicts proves a notes read or a cluster identity
// failure surfaces as an error instead of a foreign or owned answer.
func TestOwnVM_ErrorsAreNotVerdicts(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")

	_, err := ownVM(mustParseCID(t, "101"), ownTestDirector, vmOwnershipView{
		description: func() (string, error) { return "", boom },
	})
	if !errors.Is(err, boom) {
		t.Errorf("notes failure: err = %v, want boom", err)
	}

	_, err = ownVM(mustParseCID(t, ownTestStamped), ownTestDirector, vmOwnershipView{
		Tags:           []string{ownershipTag, cidStampTagPrefix + "4e5f6a7b"},
		description:    failNotes(t),
		clusterMatches: func(string) (bool, error) { return false, boom },
	})
	if !errors.Is(err, boom) {
		t.Errorf("cluster identity failure: err = %v, want boom", err)
	}
	// The cluster identity read is a PVE call once U9 plugs it in, and an
	// unclassified error there would surface as a permanent failure.
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.Type() != cpierrors.TypeRetriableCloud || !typed.OkToRetry() {
		t.Errorf("cluster identity failure: err = %v, want a retriable cloud error", err)
	}
}

func TestHasDirectorMetadataLines(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		directorNotes: true,
		"director: bosh\ndeployment: cf\nname: router/0\n": true,
		"director: bosh\ndeployment: cf\nname: router/\n":  false,
		"director: bosh\ndeployment: \nname: router/0\n":   false,
		"director:bosh\ndeployment:cf\nname:router/0\n":    false,
		"<!--BOSH:director: bosh deployment: cf-->":        false,
		"":                                 false,
		"deployment: cf\nname: router/0\n": false,
		"director: bosh\nname: router/0\ndeployment: cf\n\n": true,
	}
	for desc, want := range cases {
		if got := hasDirectorMetadataLines(desc); got != want {
			t.Errorf("hasDirectorMetadataLines(%q) = %v, want %v", desc, got, want)
		}
	}
}

// ownTestQEMU answers Config with a fixed map and counts the reads, so the
// lazy-read rules of checkVMOwnership can be pinned. byNode, when set, gives
// some nodes their own answer, for the re-locate after a not-found.
type ownTestQEMU struct {
	qemu.Service
	cfg    map[string]any
	err    error
	byNode map[string]ownTestAnswer
	reads  int
}

type ownTestAnswer struct {
	cfg map[string]any
	err error
}

func (q *ownTestQEMU) Config(_ context.Context, node string, _ int) (map[string]any, error) {
	q.reads++
	if a, ok := q.byNode[node]; ok {
		return a.cfg, a.err
	}
	return q.cfg, q.err
}

// ownTestCluster lists a fixed membership with every member online, which is
// all FindVMByConfigProbe needs.
type ownTestCluster struct {
	sdkcluster.Service
	nodes []string
}

func (c ownTestCluster) ListConfigNodes(context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	resp := make(sdkcluster.ListConfigNodesResponse, 0, len(c.nodes))
	for _, n := range c.nodes {
		b, _ := json.Marshal(map[string]any{"name": n})
		resp = append(resp, b)
	}
	return &resp, nil
}

func (ownTestCluster) ListStatus(context.Context) (*sdkcluster.ListStatusResponse, error) {
	return &sdkcluster.ListStatusResponse{}, nil
}

// ownTestClient has no cluster service unless one is set, so a re-locate
// after a not-found cannot prove anything and the read stays retriable.
type ownTestClient struct {
	pve.Client
	qemu    *ownTestQEMU
	cluster sdkcluster.Service
}

func (c ownTestClient) QEMU() qemu.Service          { return c.qemu }
func (c ownTestClient) Cluster() sdkcluster.Service { return c.cluster }

func ownTestDeps(q *ownTestQEMU) Deps {
	return Deps{
		PVE:                 ownTestClient{qemu: q},
		Config:              &config.CPIConfig{},
		Logger:              log.NewNopLogger(),
		RequestDirectorUUID: ownTestDirector,
	}
}

// ownTestDepsWithMembers is ownTestDeps on a cluster with the given members.
func ownTestDepsWithMembers(q *ownTestQEMU, nodes ...string) Deps {
	deps := ownTestDeps(q)
	deps.PVE = ownTestClient{qemu: q, cluster: ownTestCluster{nodes: nodes}}
	return deps
}

func TestCheckVMOwnership_ReadsConfigOnlyWhenNeeded(t *testing.T) {
	t.Parallel()
	stampCluster := clusterStampTagPrefix + "0a1b2c3d"

	cases := []struct {
		name      string
		cid       string
		loc       pve.VMLocation
		cfg       map[string]any
		wantReads int
		want      vmOwnershipVerdict
	}{
		{name: "row owner tag settles it",
			loc:       pve.VMLocation{Node: "n1", Found: true, Tags: ownerTagFor(ownTestDirector), Type: resourceTypeQemu},
			cfg:       map[string]any{"tags": "web"},
			wantReads: 0, want: vmOwnershipVerdict{Owned: true}},
		{name: "empty row tags read the config",
			loc:       pve.VMLocation{Node: "n1", Found: true, Type: resourceTypeQemu},
			cfg:       map[string]any{"tags": ownershipTag},
			wantReads: 1, want: vmOwnershipVerdict{Owned: true, Legacy: true}},
		{name: "notes read once with untagged row",
			loc:       pve.VMLocation{Node: "n1", Found: true},
			cfg:       map[string]any{"description": directorNotes},
			wantReads: 1, want: vmOwnershipVerdict{Owned: true, Legacy: true}},
		{name: "template flag from the config",
			loc:       pve.VMLocation{Node: "n1", Found: true},
			cfg:       map[string]any{"tags": ownershipTag, "template": float64(1)},
			wantReads: 1, want: vmOwnershipVerdict{Reason: ownershipReasonTemplate}},
		{name: "template row confirmed by the config",
			loc:       pve.VMLocation{Node: "n1", Found: true, Tags: ownershipTag, Template: true, Type: resourceTypeQemu},
			cfg:       map[string]any{"tags": ownershipTag, "template": float64(1)},
			wantReads: 1, want: vmOwnershipVerdict{Reason: ownershipReasonTemplate}},
		{name: "template row over a plain config is ours",
			loc:       pve.VMLocation{Node: "n1", Found: true, Tags: ownershipTag, Template: true, Type: resourceTypeQemu},
			cfg:       map[string]any{"tags": ownershipTag},
			wantReads: 1, want: vmOwnershipVerdict{Owned: true, Legacy: true}},
		{name: "foreign row read once and the config decides",
			loc:       pve.VMLocation{Node: "n1", Found: true, Tags: ownershipTag + ";" + ownerTagFor(ownTestOther), Type: resourceTypeQemu},
			cfg:       map[string]any{"tags": ownershipTag + ";" + ownerTagFor(ownTestDirector)},
			wantReads: 1, want: vmOwnershipVerdict{Owned: true}},
		{name: "foreign row confirmed by the config",
			loc:       pve.VMLocation{Node: "n1", Found: true, Tags: ownershipTag + ";" + ownerTagFor(ownTestOther), Type: resourceTypeQemu},
			cfg:       map[string]any{"tags": ownershipTag + ";" + ownerTagFor(ownTestOther)},
			wantReads: 1, want: vmOwnershipVerdict{Reason: ownershipReasonOtherDirector}},
		{name: "notes read and re-judge share one config read",
			loc:       pve.VMLocation{Node: "n1", Found: true, Tags: "web", Type: resourceTypeQemu},
			cfg:       map[string]any{"tags": "web"},
			wantReads: 1, want: vmOwnershipVerdict{Reason: ownershipReasonNotBOSH}},
		{name: "container never reads the QEMU config",
			loc:       pve.VMLocation{Node: "n1", Found: true, Type: "lxc"},
			wantReads: 0, want: vmOwnershipVerdict{Reason: ownershipReasonNotBOSH}},
		{name: "stale token-mismatch row, config carries the CID's token",
			cid:       ownTestStamped,
			loc:       pve.VMLocation{Node: "n1", Found: true, Tags: stampCluster + ";" + cidStampTagPrefix + "deadbeef", Type: resourceTypeQemu},
			cfg:       map[string]any{"tags": ownershipTag + ";" + ownerTagFor(ownTestDirector) + ";" + stampCluster + ";" + cidStampTagPrefix + "4e5f6a7b"},
			wantReads: 1, want: vmOwnershipVerdict{Owned: true}},
		{name: "stale token-mismatch row, config carries no token",
			cid:       ownTestStamped,
			loc:       pve.VMLocation{Node: "n1", Found: true, Tags: stampCluster + ";" + cidStampTagPrefix + "deadbeef", Type: resourceTypeQemu},
			cfg:       map[string]any{"tags": ownershipTag + ";" + ownerTagFor(ownTestDirector) + ";" + stampCluster},
			wantReads: 1, want: vmOwnershipVerdict{Reason: ownershipReasonTokenMissing}},
		{name: "probe-path location is judged as it stands",
			loc:       pve.VMLocation{Node: "n1", Found: true, FromConfig: true, Tags: ownershipTag + ";" + ownerTagFor(ownTestOther), Type: resourceTypeQemu},
			cfg:       map[string]any{"tags": ownershipTag + ";" + ownerTagFor(ownTestDirector)},
			wantReads: 0, want: vmOwnershipVerdict{Reason: ownershipReasonOtherDirector}},
		{name: "untagged probe-path location reads only the notes",
			loc:       pve.VMLocation{Node: "n1", Found: true, FromConfig: true, Type: resourceTypeQemu},
			cfg:       map[string]any{"description": directorNotes},
			wantReads: 1, want: vmOwnershipVerdict{Owned: true, Legacy: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := tc.cid
			if raw == "" {
				raw = "101"
			}
			q := &ownTestQEMU{cfg: tc.cfg}
			got, err := checkVMOwnership(context.Background(), ownTestDeps(q), "test", mustParseCID(t, raw), tc.loc, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if q.reads != tc.wantReads {
				t.Errorf("config reads = %d, want %d", q.reads, tc.wantReads)
			}
			if got.Owned != tc.want.Owned || got.Legacy != tc.want.Legacy || got.Reason != tc.want.Reason {
				t.Errorf("verdict = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestCheckVMOwnership_HandedConfigIsReused proves a handler that already
// read the config pays no second read, and that its template flag counts.
func TestCheckVMOwnership_HandedConfigIsReused(t *testing.T) {
	t.Parallel()
	q := &ownTestQEMU{}
	got, err := checkVMOwnership(context.Background(), ownTestDeps(q), "test", mustParseCID(t, "101"),
		pve.VMLocation{Node: "n1", Found: true, Tags: ownershipTag},
		map[string]any{"tags": ownershipTag, "template": "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if q.reads != 0 || got.Reason != ownershipReasonTemplate {
		t.Errorf("reads=%d verdict=%+v, want no read and a template verdict", q.reads, got)
	}
}

// TestCheckVMOwnership_HandedConfigOverridesTheRow proves a config the
// handler read decides the verdict over the scan row, which can lag a VMID
// that changed hands. Its tags and its template flag both replace the row's,
// and no second read is paid.
func TestCheckVMOwnership_HandedConfigOverridesTheRow(t *testing.T) {
	t.Parallel()
	ours := ownershipTag + ";" + ownerTagFor(ownTestDirector)
	foreign := ownershipTag + ";" + ownerTagFor(ownTestOther)

	cases := []struct {
		name string
		loc  pve.VMLocation
		cfg  map[string]any
		want vmOwnershipVerdict
	}{
		{name: "row ours, config foreign",
			loc:  pve.VMLocation{Node: "n1", Found: true, Tags: ours, Type: resourceTypeQemu},
			cfg:  map[string]any{"tags": foreign},
			want: vmOwnershipVerdict{Reason: ownershipReasonOtherDirector}},
		{name: "row foreign, config ours",
			loc:  pve.VMLocation{Node: "n1", Found: true, Tags: foreign, Type: resourceTypeQemu},
			cfg:  map[string]any{"tags": ours},
			want: vmOwnershipVerdict{Owned: true}},
		{name: "row ours, config untagged and without notes",
			loc:  pve.VMLocation{Node: "n1", Found: true, Tags: ours, Type: resourceTypeQemu},
			cfg:  map[string]any{"name": "hand-made"},
			want: vmOwnershipVerdict{Reason: ownershipReasonNotBOSH}},
		{name: "row template, config a plain VM",
			loc:  pve.VMLocation{Node: "n1", Found: true, Tags: ownershipTag, Template: true, Type: resourceTypeQemu},
			cfg:  map[string]any{"tags": ownershipTag},
			want: vmOwnershipVerdict{Owned: true, Legacy: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &ownTestQEMU{}
			got, err := checkVMOwnership(context.Background(), ownTestDeps(q), "test", mustParseCID(t, "101"), tc.loc, tc.cfg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if q.reads != 0 {
				t.Errorf("config reads = %d, want 0 with a handed config", q.reads)
			}
			if got.Owned != tc.want.Owned || got.Legacy != tc.want.Legacy || got.Reason != tc.want.Reason {
				t.Errorf("verdict = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestCheckVMOwnership_ReadFailureIsRetriable proves a config read that fails
// is a retriable error and never a verdict, on every branch that reads: a row
// with no tags, a foreign row that the config must confirm, and a stamped row
// whose token says the VM is gone. A not-found is retriable too when locating
// the VM again proves nothing, which here is a client with no cluster service.
func TestCheckVMOwnership_ReadFailureIsRetriable(t *testing.T) {
	t.Parallel()
	stampedRow := clusterStampTagPrefix + "0a1b2c3d;" + cidStampTagPrefix + "deadbeef"
	rows := []struct {
		name string
		cid  string
		loc  pve.VMLocation
	}{
		{"untagged row", "101", pve.VMLocation{Node: "n1", Found: true}},
		{"foreign row", "101", pve.VMLocation{Node: "n1", Found: true, Type: resourceTypeQemu,
			Tags: ownershipTag + ";" + ownerTagFor(ownTestOther)}},
		{"stamped token-mismatch row", ownTestStamped, pve.VMLocation{Node: "n1", Found: true, Type: resourceTypeQemu,
			Tags: stampedRow}},
	}
	for _, row := range rows {
		for _, readErr := range []error{errors.New("connection reset"), sdkerrors.ErrNotFound} {
			t.Run(row.name+"/"+readErr.Error(), func(t *testing.T) {
				t.Parallel()
				q := &ownTestQEMU{err: readErr}
				got, err := checkVMOwnership(context.Background(), ownTestDeps(q), "test", mustParseCID(t, row.cid), row.loc, nil)
				if err == nil {
					t.Fatalf("got verdict %+v and no error, want a retriable error", got)
				}
				if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
					t.Errorf("err = %#v, want TypeRetriableCloud", err)
				}
				if got != (vmOwnershipVerdict{}) {
					t.Errorf("verdict = %+v, want the zero verdict alongside the error", got)
				}
			})
		}
	}
}

// TestCheckVMOwnership_NotFoundLocatesTheVMAgain pins the re-locate after a
// config read that answers not-found. Every member answering not-found proves
// absence. A hit on another node is judged from that node's config. A member
// whose probe fails leaves absence unproven, so the answer is retriable.
func TestCheckVMOwnership_NotFoundLocatesTheVMAgain(t *testing.T) {
	t.Parallel()
	foreignRow := pve.VMLocation{Node: "n1", Found: true, Type: resourceTypeQemu, Tags: ownershipTag + ";" + ownerTagFor(ownTestOther)}
	cases := []struct {
		name          string
		byNode        map[string]ownTestAnswer
		want          vmOwnershipVerdict
		wantRetriable bool
	}{
		{name: "absent on every member",
			byNode: map[string]ownTestAnswer{"n1": {err: sdkerrors.ErrNotFound}, "n2": {err: sdkerrors.ErrNotFound}},
			want:   vmOwnershipVerdict{Absent: true}},
		{name: "moved to a member where it is ours",
			byNode: map[string]ownTestAnswer{"n1": {err: sdkerrors.ErrNotFound},
				"n2": {cfg: map[string]any{"tags": ownershipTag + ";" + ownerTagFor(ownTestDirector)}}},
			want: vmOwnershipVerdict{Owned: true}},
		{name: "moved to a member where it is foreign",
			byNode: map[string]ownTestAnswer{"n1": {err: sdkerrors.ErrNotFound},
				"n2": {cfg: map[string]any{"tags": ownershipTag + ";" + ownerTagFor(ownTestOther)}}},
			want: vmOwnershipVerdict{Reason: ownershipReasonOtherDirector}},
		{name: "a member's probe fails",
			byNode:        map[string]ownTestAnswer{"n1": {err: sdkerrors.ErrNotFound}, "n2": {err: errors.New("connection reset")}},
			wantRetriable: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &ownTestQEMU{byNode: tc.byNode}
			got, err := checkVMOwnership(context.Background(), ownTestDepsWithMembers(q, "n1", "n2"), "test",
				mustParseCID(t, "101"), foreignRow, nil)
			if tc.wantRetriable {
				if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || got != (vmOwnershipVerdict{}) {
					t.Fatalf("verdict=%+v err=%v, want a retriable error and no verdict", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Owned != tc.want.Owned || got.Absent != tc.want.Absent || got.Reason != tc.want.Reason {
				t.Errorf("verdict = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestCheckVMOwnershipByConfig_ReadCounts pins the read the attach guard pays.
// A location from the index costs one config read, and one the lookup's
// config probe produced costs none, because it already is that config.
func TestCheckVMOwnershipByConfig_ReadCounts(t *testing.T) {
	t.Parallel()
	ours := ownershipTag + ";" + ownerTagFor(ownTestDirector)
	for _, tc := range []struct {
		name      string
		loc       pve.VMLocation
		wantReads int
	}{
		{"index row", pve.VMLocation{Node: "n1", Found: true, Tags: ours, Type: resourceTypeQemu}, 1},
		{"config probe", pve.VMLocation{Node: "n1", Found: true, Tags: ours, Type: resourceTypeQemu, FromConfig: true}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &ownTestQEMU{cfg: map[string]any{"tags": ours}}
			got, err := checkVMOwnershipByConfig(context.Background(), ownTestDeps(q), "attach_disk", mustParseCID(t, "101"), tc.loc)
			if err != nil || !got.Owned {
				t.Fatalf("verdict=%+v err=%v, want owned", got, err)
			}
			if q.reads != tc.wantReads {
				t.Errorf("config reads = %d, want %d", q.reads, tc.wantReads)
			}
		})
	}
}

// TestRefuseUnownedVMByConfig_ReadFailureIsRetriable pins the attach guard's
// failed read: it is retriable and never lets the handler proceed.
func TestRefuseUnownedVMByConfig_ReadFailureIsRetriable(t *testing.T) {
	t.Parallel()
	q := &ownTestQEMU{err: errors.New("connection reset")}
	loc := pve.VMLocation{Node: "n1", Found: true, Tags: ownershipTag + ";" + ownerTagFor(ownTestDirector), Type: resourceTypeQemu}
	err := refuseUnownedVMByConfig(context.Background(), ownTestDeps(q), "attach_disk", "101", mustParseCID(t, "101"), loc, log.NewNopLogger())
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want TypeRetriableCloud", err)
	}
}

func TestForeignVMError_NamesReasonIdentitiesAndEscapes(t *testing.T) {
	t.Parallel()
	v := vmOwnershipVerdict{Reason: ownershipReasonOtherDirector, Detail: "the VM is owned by Director a, and this request comes from Director b"}
	err := foreignVMError("delete_vm", "101", 101, v)
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Fatalf("err = %#v, want a non-retriable CloudError", err)
	}
	for _, want := range []string{"delete_vm", "VM 101", ownershipReasonOtherDirector, "Director a", "Director b", "Delete VM reference", "delete-deployment --force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// TestDetachStableID_RefusesAnotherDirectorsVM pins the guard on the
// stable-ID detach path, which locates the VM from the disk's holder rather
// than through the legacy slot resolution. A bare CID that reaches another
// Director's VM is refused, and the disk stays plugged in.
func TestDetachStableID_RefusesAnotherDirectorsVM(t *testing.T) {
	t.Parallel()
	c := birthHeldClient(t, nil)
	c.configs[birthHeldVMID]["tags"] = ownershipTag + ";" + ownerTagPrefix + ownTestOther

	deps := Deps{Config: &config.CPIConfig{Node: "pve1", DiskStorage: "data"}, PVE: c, Logger: log.NewNopLogger()}
	vmArg, _ := json.Marshal("700")
	diskArg, _ := json.Marshal(birthHeldCID(t, birthHeldBirth))
	_, err := HandleDetachDisk(deps).Handle(context.Background(), []json.RawMessage{vmArg, diskArg},
		jsonrpc.Context{DirectorUUID: ownTestDirector})
	if !cpierrors.IsType(err, cpierrors.TypeCloud) || !strings.Contains(err.Error(), "("+ownershipReasonOtherDirector+")") {
		t.Fatalf("detach_disk error = %v, want the other-director refusal", err)
	}
	if _, present := c.configs[birthHeldVMID]["scsi1"]; !present {
		t.Error("detach_disk unplugged the disk from another Director's VM")
	}
}

// TestDetachStableID_RefusesAnotherDirectorsVMHoldingAnUnusedEntry covers the
// second branch of the stable-ID guard. No slot carries the disk, and the VM
// the CID names holds it only as an unused entry, which the detach would
// otherwise park. Another Director's VM is refused, and the entry stays.
func TestDetachStableID_RefusesAnotherDirectorsVMHoldingAnUnusedEntry(t *testing.T) {
	t.Parallel()
	c := newIDFakeClient(map[int]map[string]any{
		birthHeldVMID: {
			"tags":        ownershipTag + ";" + ownerTagPrefix + ownTestOther,
			"unused0":     birthHeldBirth,
			"description": birthHeldDescription(t, idTestToken),
		},
		birthHeldParker: {"tags": "bosh-cpi;bosh-parker", "protection": true},
	})

	deps := Deps{Config: &config.CPIConfig{Node: "pve1", DiskStorage: "data"}, PVE: c, Logger: log.NewNopLogger()}
	vmArg, _ := json.Marshal(strconv.Itoa(birthHeldVMID))
	diskArg, _ := json.Marshal(birthHeldCID(t, birthHeldBirth))
	_, err := HandleDetachDisk(deps).Handle(context.Background(), []json.RawMessage{vmArg, diskArg},
		jsonrpc.Context{DirectorUUID: ownTestDirector})
	if !cpierrors.IsType(err, cpierrors.TypeCloud) || !strings.Contains(err.Error(), "("+ownershipReasonOtherDirector+")") {
		t.Fatalf("detach_disk error = %v, want the other-director refusal", err)
	}
	if got, _ := c.configs[birthHeldVMID]["unused0"].(string); got != birthHeldBirth {
		t.Errorf("unused0 = %q, want %q left on another Director's VM", got, birthHeldBirth)
	}
}

// TestDetachStableID_StaleOwnRowDoesNotCoverAnUnusedEntry is the stale-row
// case of the unused-entry branch. The index row still shows the VM as ours,
// and the config shows another Director, so the config decides, the detach is
// refused, and the entry stays.
func TestDetachStableID_StaleOwnRowDoesNotCoverAnUnusedEntry(t *testing.T) {
	t.Parallel()
	c := newIDFakeClient(map[int]map[string]any{
		birthHeldVMID: {
			"tags":        ownershipTag + ";" + ownerTagPrefix + ownTestOther,
			"unused0":     birthHeldBirth,
			"description": birthHeldDescription(t, idTestToken),
		},
		birthHeldParker: {"tags": "bosh-cpi;bosh-parker", "protection": true},
	})
	c.rowTags = map[int]string{birthHeldVMID: ownershipTag + ";" + ownerTagPrefix + ownTestDirector}

	deps := Deps{Config: &config.CPIConfig{Node: "pve1", DiskStorage: "data"}, PVE: c, Logger: log.NewNopLogger()}
	vmArg, _ := json.Marshal(strconv.Itoa(birthHeldVMID))
	diskArg, _ := json.Marshal(birthHeldCID(t, birthHeldBirth))
	_, err := HandleDetachDisk(deps).Handle(context.Background(), []json.RawMessage{vmArg, diskArg},
		jsonrpc.Context{DirectorUUID: ownTestDirector})
	if !cpierrors.IsType(err, cpierrors.TypeCloud) || !strings.Contains(err.Error(), "("+ownershipReasonOtherDirector+")") {
		t.Fatalf("detach_disk error = %v, want the other-director refusal", err)
	}
	if got, _ := c.configs[birthHeldVMID]["unused0"].(string); got != birthHeldBirth {
		t.Errorf("unused0 = %q, want %q left on another Director's VM", got, birthHeldBirth)
	}
}

// TestOwnerTagBackfill_OnlyLegacyInstanceVMs pins ownerTagBackfill's own
// exclusions. The guard refuses templates and parkers before set_vm_metadata
// gets this far, so these branches are defence in depth, and only a direct
// call reaches them.
func TestOwnerTagBackfill_OnlyLegacyInstanceVMs(t *testing.T) {
	t.Parallel()
	matched := vmMetadataIdentity{incoming: "x", stored: []string{"x"}, compared: []string{"x"}, matched: true}
	cases := []struct {
		name string
		tags []string
		want string
	}{
		{"legacy instance VM", []string{ownershipTag}, ownerTagFor(ownTestDirector)},
		{"no bosh-cpi tag", []string{"env--prod"}, ""},
		{"already owned", []string{ownershipTag, ownerTagFor(ownTestOther)}, ""},
		{"stemcell template", []string{ownershipTag, stemcellMarkerTag}, ""},
		{"stemcell cache template", []string{ownershipTag, stemcellCacheTag}, ""},
		{"disk parker", []string{ownershipTag, pve.ParkerTag}, ""},
		{"parker tag in another case", []string{ownershipTag, strings.ToUpper(pve.ParkerTag)}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ownerTagBackfill(matched, tc.tags, ownTestDirector, log.NewNopLogger()); got != tc.want {
				t.Errorf("ownerTagBackfill(%v) = %q, want %q", tc.tags, got, tc.want)
			}
		})
	}
}
