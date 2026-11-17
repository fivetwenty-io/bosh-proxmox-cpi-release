package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// countingHA counts the per-node reads that reach the wrapped fake.
type countingHA struct {
	*fakeHA
	configReads  map[string]int
	statusReads  map[string]int
	volumeReads  map[string]int
	guestListing int
}

func newCountingHA(f *fakeHA) *countingHA {
	return &countingHA{fakeHA: f, configReads: map[string]int{}, statusReads: map[string]int{}, volumeReads: map[string]int{}}
}

func (c *countingHA) Guests(ctx context.Context) ([]pve.GuestRef, []string, error) {
	c.guestListing++
	return c.fakeHA.Guests(ctx)
}

func (c *countingHA) GuestConfig(ctx context.Context, node string, vmid int) (map[string]any, error) {
	c.configReads[node]++
	return c.fakeHA.GuestConfig(ctx, node, vmid)
}

func (c *countingHA) ReplicaStatus(ctx context.Context, node, id string) (ha.ReplicaSyncStatus, error) {
	c.statusReads[node]++
	return c.fakeHA.ReplicaStatus(ctx, node, id)
}

func (c *countingHA) StorageVolumes(ctx context.Context, node, storage string) (map[string]bool, error) {
	c.volumeReads[node]++
	return c.fakeHA.StorageVolumes(ctx, node, storage)
}

// haNoQuorumProfile is the healthy profile after n2 and n3 went away, as the
// survivor n1 reports it: not quorate, with both other members offline.
func haNoQuorumProfile() *ha.Profile {
	p := haHealthyProfile(config.HAModeAuto)
	p.Quorate = false
	for i := range p.Nodes {
		if p.Nodes[i].Name != "n1" {
			p.Nodes[i].Online = false
		}
	}
	return p
}

// lgUnreachable is the 595 the API node answers for a member it cannot reach.
func lgUnreachable() error {
	return sdkerrors.ParseAPIError(595, []byte(`{"message":"no route to host","code":595}`))
}

// findingWith returns the first finding whose detail contains text.
func findingWith(rep *haReport, text string) (haFinding, bool) {
	for _, f := range rep.Findings {
		if strings.Contains(f.Detail, text) {
			return f, true
		}
	}
	return haFinding{}, false
}

// TestLedgerReader_SkipsConfigOfVMOnOfflineNodeWithoutQuorum: the config of a
// VM whose node the non-quorate entry node lists offline is not read, and the
// doctor reports it unchecked with that reason.
func TestLedgerReader_SkipsConfigOfVMOnOfflineNodeWithoutQuorum(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile = haNoQuorumProfile()
	f.guests[0].Node = "n3"
	inner := newCountingHA(f)
	rep := collectHA(context.Background(), newLedgerHAReader(inner, newHANodeLedger()), haCfg(config.HAModeAuto, "none"),
		haDirector, haNow)
	if inner.configReads["n3"] != 0 {
		t.Fatalf("the config of a VM on offline n3 was read %d times, want 0", inner.configReads["n3"])
	}
	finding, ok := findingWith(rep, "node n3 was not asked, because the entry node does not report the cluster quorate and lists it offline")
	if !ok || finding.Kind != haKindUnchecked || finding.Subject != "vm:100" {
		t.Fatalf("want an unchecked finding for vm:100 naming the lost quorum, got %+v", rep.Findings)
	}
}

// TestLedgerReader_SkipsOfflineNodeWithQuorum: a quorate cluster that lists a
// member offline gets the same skip, with a reason that does not mention
// quorum.
func TestLedgerReader_SkipsOfflineNodeWithQuorum(t *testing.T) {
	t.Parallel()
	f := haBaseFake()
	f.profile.Nodes[2].Online = false
	inner := newCountingHA(f)
	r := newLedgerHAReader(inner, newHANodeLedger())
	if _, err := r.Profile(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := r.ReplicaStatus(context.Background(), "n3", "100-0")
	var skipped *haSkippedReadError
	if !errors.As(err, &skipped) || err.Error() != "node n3 was not asked, because cluster status lists it offline" {
		t.Fatalf("want the read of offline n3 skipped, got %v", err)
	}
	if inner.statusReads["n3"] != 0 {
		t.Fatalf("offline n3 was asked %d times, want 0", inner.statusReads["n3"])
	}
}

// TestLedgerReader_SharesUnreachableNodeAcrossEntries: a transport fault on a
// node in one entry marks it for the rest of the run, so a second entry that
// reaches the same cluster does not ask it again and names the first failure.
func TestLedgerReader_SharesUnreachableNodeAcrossEntries(t *testing.T) {
	t.Parallel()
	ledger := newHANodeLedger()
	ctx := context.Background()

	first := newCountingHA(haBaseFake())
	first.volumesErr = map[string]error{"n2/local-zfs": lgUnreachable()}
	r1 := newLedgerHAReader(first, ledger)
	if _, err := r1.Profile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r1.StorageVolumes(ctx, "n2", "local-zfs"); err == nil {
		t.Fatal("the first read must fail as the fake says")
	}

	second := newCountingHA(haBaseFake())
	r2 := newLedgerHAReader(second, ledger)
	if _, err := r2.Profile(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := r2.ReplicaStatus(ctx, "n2", "100-0")
	if err == nil || !strings.Contains(err.Error(), "node n2 was not asked, because an earlier read of it in this run failed") ||
		!strings.Contains(err.Error(), "no route to host") {
		t.Fatalf("the second entry must skip n2 and name the first failure, got %v", err)
	}
	if second.statusReads["n2"] != 0 {
		t.Fatalf("n2 was asked %d times by the second entry, want 0", second.statusReads["n2"])
	}
	if _, err := r2.ReplicaStatus(ctx, "n1", "101-0"); err != nil {
		t.Fatalf("a node that answered must still be asked, got %v", err)
	}
}

// TestLedgerReader_APIAnswerDoesNotMarkNode: a failure PVE answered, such as a
// missing grant, says nothing about whether the node can answer, so the node
// is still asked by later reads.
func TestLedgerReader_APIAnswerDoesNotMarkNode(t *testing.T) {
	t.Parallel()
	ledger := newHANodeLedger()
	ctx := context.Background()
	inner := newCountingHA(haBaseFake())
	inner.statusErr = map[string]error{"100-0": sdkerrors.ParseAPIError(403, []byte(`{"message":"Permission check failed"}`))}
	r := newLedgerHAReader(inner, ledger)
	if _, err := r.Profile(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = r.ReplicaStatus(ctx, "n2", "100-0")
	if _, err := r.ReplicaStatus(ctx, "n2", "101-0"); err != nil {
		t.Fatalf("a 403 on one job must not skip the node, got %v", err)
	}
	if inner.statusReads["n2"] != 2 {
		t.Fatalf("n2 was asked %d times, want 2", inner.statusReads["n2"])
	}
}

// TestLedgerReader_SharesGuestFailureWithoutQuorum: once one entry's guest
// listing failed because the cluster has lost quorum, a second entry on the
// same cluster reuses that failure instead of listing again.
func TestLedgerReader_SharesGuestFailureWithoutQuorum(t *testing.T) {
	t.Parallel()
	ledger := newHANodeLedger()
	ctx := context.Background()
	listingErr := &pve.GuestEnumerationError{Nodes: []string{"n2", "n3"}, NoQuorum: []string{"n2", "n3"}}

	run := func() (*countingHA, *haReport) {
		f := haBaseFake()
		f.profile = haNoQuorumProfile()
		f.guestsErr = listingErr
		inner := newCountingHA(f)
		return inner, collectHA(ctx, newLedgerHAReader(inner, ledger), haCfg(config.HAModeAuto, "none"), haDirector, haNow)
	}
	first, _ := run()
	second, rep := run()
	if first.guestListing != 1 || second.guestListing != 0 {
		t.Fatalf("guest listings = %d then %d, want 1 then 0", first.guestListing, second.guestListing)
	}
	if _, ok := findingWith(rep, "an earlier entry in this run already failed to list them"); !ok {
		t.Fatalf("the second entry must report the shared failure, got %+v", rep.Findings)
	}
	if reason, ok := ledger.failure(haClusterKey("", haNoQuorumProfile().Nodes), "n2"); !ok ||
		!strings.Contains(reason, pve.ReasonNotQuorate) {
		t.Fatalf("n2 must be marked with the lost quorum, got %q", reason)
	}
}

// TestLedgerReader_DifferentClustersDoNotShare: a node name that a second,
// different cluster also uses is still asked there.
func TestLedgerReader_DifferentClustersDoNotShare(t *testing.T) {
	t.Parallel()
	ledger := newHANodeLedger()
	ctx := context.Background()
	first := newCountingHA(haBaseFake())
	first.statusErr = map[string]error{"100-0": lgUnreachable()}
	r1 := newLedgerHAReader(first, ledger)
	if _, err := r1.Profile(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = r1.ReplicaStatus(ctx, "n2", "100-0")

	other := haBaseFake()
	other.profile.Nodes = append(other.profile.Nodes, ha.NodeFacts{Name: "n4", Online: true})
	second := newCountingHA(other)
	r2 := newLedgerHAReader(second, ledger)
	if _, err := r2.Profile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r2.ReplicaStatus(ctx, "n2", "101-0"); err != nil {
		t.Fatalf("n2 of another cluster must still be asked, got %v", err)
	}
}

// TestRunHACPIConfig_SharesWhatOneEntryLearned runs the cpi-config form with
// two entries that reach the same cluster after it lost quorum. The guests are
// listed once, and the second entry reports the shared failure as unchecked.
func TestRunHACPIConfig_SharesWhatOneEntryLearned(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, "cpi.json", cpiConfigTwoClusters)
	entry := func(next pve.VMIDBand) *countingHA {
		f := sharedFake(next)
		f.profile = haNoQuorumProfile()
		f.guestsErr = &pve.GuestEnumerationError{Nodes: []string{"n2", "n3"}, NoQuorum: []string{"n2", "n3"}}
		return newCountingHA(f)
	}
	a := entry(pve.VMIDBand{Start: 200000, End: 200999})
	b := entry(pve.VMIDBand{Start: 210000, End: 210999})
	code, out, _ := runHATest(t, newHATestDeps(haCfg("auto", "none"), a, b), "--cpi-config", path)
	if code != exitError {
		t.Fatalf("a cluster without quorum must fail the run, code = %d\n%s", code, out)
	}
	if a.guestListing != 1 || b.guestListing != 0 {
		t.Fatalf("guest listings = %d then %d, want 1 then 0", a.guestListing, b.guestListing)
	}
	if !strings.Contains(out, "an earlier entry in this run already failed to list them") {
		t.Fatalf("the second entry must report the shared failure:\n%s", out)
	}
}

// identifiedHA is a counting fake that also names its cluster, as the
// production reader does.
type identifiedHA struct {
	*countingHA
	identity string
}

func (i *identifiedHA) ClusterIdentity(context.Context, string) string { return i.identity }

// TestLedgerReader_KeepsClustersWithSameMemberNamesApart: two clusters that
// name their nodes alike do not share a node mark, while two readers that name
// one cluster still do.
func TestLedgerReader_KeepsClustersWithSameMemberNamesApart(t *testing.T) {
	t.Parallel()
	ledger := newHANodeLedger()
	ctx := context.Background()

	a := &identifiedHA{countingHA: newCountingHA(haBaseFake()), identity: "pve-root-ca:aaaaaaaa"}
	a.volumesErr = map[string]error{"n2/local-zfs": lgUnreachable()}
	ra := newLedgerHAReader(a, ledger)
	if _, err := ra.Profile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ra.StorageVolumes(ctx, "n2", "local-zfs"); err == nil {
		t.Fatal("the first read must fail as the fake says")
	}

	b := &identifiedHA{countingHA: newCountingHA(haBaseFake()), identity: "pve-root-ca:bbbbbbbb"}
	rb := newLedgerHAReader(b, ledger)
	if _, err := rb.Profile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := rb.ReplicaStatus(ctx, "n2", "100-0"); err != nil && strings.Contains(err.Error(), "was not asked") {
		t.Fatalf("a different cluster must not inherit the mark, got %v", err)
	}
	if b.statusReads["n2"] != 1 {
		t.Fatalf("the second cluster's n2 was asked %d times, want 1", b.statusReads["n2"])
	}

	c := &identifiedHA{countingHA: newCountingHA(haBaseFake()), identity: "pve-root-ca:aaaaaaaa"}
	rc := newLedgerHAReader(c, ledger)
	if _, err := rc.Profile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := rc.ReplicaStatus(ctx, "n2", "100-0"); err == nil || !strings.Contains(err.Error(), "was not asked") {
		t.Fatalf("the same cluster must share the mark, got %v", err)
	}

	d := &identifiedHA{countingHA: newCountingHA(haBaseFake()), identity: ""}
	d.volumesErr = map[string]error{"n2/local-zfs": lgUnreachable()}
	rd := newLedgerHAReader(d, ledger)
	if _, err := rd.Profile(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = rd.StorageVolumes(ctx, "n2", "local-zfs")
	if _, ok := ledger.failure("", "n2"); ok {
		t.Fatal("a reader with no identity must record no claims")
	}
}
