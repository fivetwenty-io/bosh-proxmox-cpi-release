// Tests for the second opinion an empty content listing gets before the proof
// reads it as an absence: who is consulted, in what order, and what each source
// is allowed to conclude.
package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// corroborationNode is the node every case below observes from.
const corroborationNode = "pve-01"

// corroborationPeerNode is the other node in the cluster: the one whose disks
// say nothing about a local storage on corroborationNode.
const corroborationPeerNode = "pve-02"

// sharedProbe is the question a corroborator is asked about the nfs storage the
// proof cases run against, as seen from corroborationNode. nfs is shared, so a
// source may weigh evidence from anywhere in the cluster.
func sharedProbe() pve.EmptyListingProbe {
	return pve.EmptyListingProbe{
		Node:       corroborationNode,
		Storage:    absenceStorage,
		Volume:     absenceVolid,
		Info:       pve.StorageInfo{Name: absenceStorage, Type: pve.StorageTypeNFS},
		Classified: true,
	}
}

// localProbe is the same question about a node-local dir storage, where only
// what the probed node holds is evidence about the listing that node served.
func localProbe(node string) pve.EmptyListingProbe {
	return pve.EmptyListingProbe{
		Node:       node,
		Storage:    absenceStorage,
		Volume:     absenceVolid,
		Info:       pve.StorageInfo{Name: absenceStorage, Type: pve.StorageTypeDir, IsMountpoint: true},
		Classified: true,
	}
}

// unclassifiedProbe is the storage the proof could not identify. Nothing in
// Info was observed, so a source may not read it.
func unclassifiedProbe(node string) pve.EmptyListingProbe {
	return pve.EmptyListingProbe{Node: node, Storage: absenceStorage, Volume: absenceVolid}
}

// silentCorroborator has nothing to say and records that it was asked, which is
// how the order cases tell "consulted and passed" from "never reached".
func silentCorroborator(calls *int) pve.EmptyListingCorroborator {
	return pve.CorroboratorFunc("a quiet source",
		func(context.Context, pve.EmptyListingProbe) (pve.Corroboration, error) {
			*calls++
			return pve.Corroboration{}, nil
		})
}

// contradictingCorroborator contradicts the listing with a fixed detail.
func contradictingCorroborator(calls *int, source, detail string) pve.EmptyListingCorroborator {
	return pve.CorroboratorFunc(source, func(context.Context, pve.EmptyListingProbe) (pve.Corroboration, error) {
		*calls++
		return pve.Corroboration{Contradicted: true, Source: source, Detail: detail}, nil
	})
}

// failingCorroborator is a check that did not land.
func failingCorroborator(calls *int, err error) pve.EmptyListingCorroborator {
	return pve.CorroboratorFunc("a source that broke",
		func(context.Context, pve.EmptyListingProbe) (pve.Corroboration, error) {
			*calls++
			return pve.Corroboration{}, err
		})
}

// forbiddenCorroborator fails the test if it is ever consulted.
func forbiddenCorroborator(t *testing.T, why string) pve.EmptyListingCorroborator {
	t.Helper()
	return pve.CorroboratorFunc("a source that must not be asked",
		func(context.Context, pve.EmptyListingProbe) (pve.Corroboration, error) {
			t.Error(why)
			return pve.Corroboration{}, errors.New("unexpected corroboration call")
		})
}

// proveWithCorroborators runs the proof against an empty nfs listing, which is
// the one state corroboration is consulted in.
func proveWithCorroborators(t *testing.T, corroborators ...pve.EmptyListingCorroborator) (bool, error) {
	t.Helper()
	probe := noFormatProbe(listingOf(t))
	return pve.ProveVolumeAbsent(context.Background(), probe.client(), corroborationNode,
		absenceStorage, absenceVolid, nfsClassifier(), corroborators...)
}

func TestProveVolumeAbsent_EmptyListing_NoCorroborators_KeepsTodaysAnswer(t *testing.T) {
	t.Parallel()
	absent, err := proveWithCorroborators(t)
	if err != nil {
		t.Fatalf("a caller that passes no corroborators must keep the answer it had: %v", err)
	}
	if !absent {
		t.Fatal("an empty listing on nfs still proves absence when nothing corroborates it")
	}
}

// TestProveVolumeAbsent_EmptyListing_PassesTheVolumeUnderProof pins the
// argument the journal source depends on. A source that tracks volumes by name
// has to know which one is being proven absent, because that volume's own
// record is the thing in question and can never be evidence against the
// listing.
func TestProveVolumeAbsent_EmptyListing_PassesTheVolumeUnderProof(t *testing.T) {
	t.Parallel()
	var seen pve.EmptyListingProbe
	recorder := pve.CorroboratorFunc("a source that reads its arguments",
		func(_ context.Context, probe pve.EmptyListingProbe) (pve.Corroboration, error) {
			seen = probe
			return pve.Corroboration{}, nil
		})
	if _, err := proveWithCorroborators(t, recorder); err != nil {
		t.Fatalf("a silent corroborator leaves the answer alone: %v", err)
	}
	if seen.Node != corroborationNode || seen.Storage != absenceStorage {
		t.Errorf("the corroborator must be told where it is looking, got node %q storage %q", seen.Node, seen.Storage)
	}
	if seen.Volume != absenceVolid {
		t.Errorf("the corroborator must be told which volume is under proof, got %q", seen.Volume)
	}
	if !seen.Classified {
		t.Error("the proof classified the storage, so the probe must say so")
	}
	if seen.Info.Type != pve.StorageTypeNFS || !seen.Info.IsShared() {
		t.Errorf("the probe must carry the classification the proof made, got %+v", seen.Info)
	}
}

func TestProveVolumeAbsent_EmptyListing_NilCorroborator_IsSkipped(t *testing.T) {
	t.Parallel()
	absent, err := proveWithCorroborators(t, nil)
	if err != nil {
		t.Fatalf("a nil corroborator must be skipped rather than dereferenced: %v", err)
	}
	if !absent {
		t.Fatal("a nil corroborator contradicts nothing")
	}
}

func TestProveVolumeAbsent_EmptyListing_Contradicted_IsUnproven(t *testing.T) {
	t.Parallel()
	var calls int
	absent, err := proveWithCorroborators(t,
		contradictingCorroborator(&calls, pve.CorroborationSourceConfigs,
			"3 volumes on the storage are referenced by VM configs"))
	if err == nil {
		t.Fatal("a contradicted empty listing must be unproven")
	}
	if absent {
		t.Fatal("an unproven absence must not read as absent")
	}
	if calls != 1 {
		t.Fatalf("the corroborator must be consulted exactly once, got %d calls", calls)
	}
	for _, want := range []string{
		absenceStorage,
		pve.CorroborationSourceConfigs,
		"3 volumes on the storage are referenced by VM configs",
		"mounted from the wrong tree",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name %q for the operator, got: %v", want, err)
		}
	}
}

func TestProveVolumeAbsent_EmptyListing_ContradictionStopsTheRest(t *testing.T) {
	t.Parallel()
	var first int
	_, err := proveWithCorroborators(t,
		contradictingCorroborator(&first, pve.CorroborationSourceConfigs,
			"1 volume on the storage is referenced by VM configs"),
		forbiddenCorroborator(t, "a contradiction settles it; later sources must not be consulted"),
	)
	if err == nil {
		t.Fatal("the contradiction must be reported")
	}
	if first != 1 {
		t.Fatalf("the contradicting source must be consulted once, got %d", first)
	}
}

func TestProveVolumeAbsent_EmptyListing_CorroboratorErrorStopsTheRest(t *testing.T) {
	t.Parallel()
	var first int
	sentinel := errors.New("storage status read timed out")
	absent, err := proveWithCorroborators(t,
		failingCorroborator(&first, sentinel),
		forbiddenCorroborator(t, "a corroborator that errored settles it; later sources must not be consulted"),
	)
	if err == nil {
		t.Fatal("a corroborator that did not land must leave the absence unproven")
	}
	if absent {
		t.Fatal("an unproven absence must not read as absent")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("the corroborator's own error must be wrapped, got: %v", err)
	}
	if first != 1 {
		t.Fatalf("the failing source must be consulted once, got %d", first)
	}
	if !strings.Contains(err.Error(), absenceStorage) {
		t.Errorf("the refusal must name the storage, got: %v", err)
	}
}

func TestProveVolumeAbsent_EmptyListing_SilentSourcePassesToTheNext(t *testing.T) {
	t.Parallel()
	var quiet, loud int
	_, err := proveWithCorroborators(t,
		silentCorroborator(&quiet),
		contradictingCorroborator(&loud, pve.CorroborationSourceStorageStatus, "the storage reports 5 GiB in use"),
	)
	if err == nil {
		t.Fatal("the second source contradicted the listing, so the absence is unproven")
	}
	if quiet != 1 || loud != 1 {
		t.Fatalf("both sources must be consulted in order, got quiet=%d loud=%d", quiet, loud)
	}
}

func TestProveVolumeAbsent_EmptyListing_AllSilent_ProvesAbsence(t *testing.T) {
	t.Parallel()
	var first, second int
	absent, err := proveWithCorroborators(t, silentCorroborator(&first), silentCorroborator(&second))
	if err != nil {
		t.Fatalf("sources with nothing to say leave the existing rule standing: %v", err)
	}
	if !absent {
		t.Fatal("an empty listing on nfs proves absence when nothing contradicts it")
	}
	if first != 1 || second != 1 {
		t.Fatalf("every source must be consulted, got first=%d second=%d", first, second)
	}
}

func TestProveVolumeAbsent_ContradictionWithoutSourceOrDetail_StillReads(t *testing.T) {
	t.Parallel()
	bare := pve.CorroboratorFunc("", func(context.Context, pve.EmptyListingProbe) (pve.Corroboration, error) {
		return pve.Corroboration{Contradicted: true}, nil
	})
	_, err := proveWithCorroborators(t, bare)
	if err == nil {
		t.Fatal("a contradiction is a contradiction whether or not it named itself")
	}
	if !strings.Contains(err.Error(), "unnamed") || !strings.Contains(err.Error(), "no detail") {
		t.Errorf("the refusal must stay readable without a source or detail, got: %v", err)
	}
}

// TestProveVolumeAbsent_EmptyListing_LocalStorage_IgnoresOtherNodes is the
// whole finding end to end. The proof classifies the storage once and hands
// that classification to the sources, so a dir storage on the probed node is
// judged by what the probed node holds, and the disks another node keeps on its
// own storage of the same name cannot contradict an honest empty listing.
func TestProveVolumeAbsent_EmptyListing_LocalStorage_IgnoresOtherNodes(t *testing.T) {
	t.Parallel()
	probe := noFormatProbe(listingOf(t))
	refs := pve.StorageReferenceCounts{absenceStorage: {corroborationPeerNode: 3}}
	absent, err := pve.ProveVolumeAbsent(context.Background(), probe.client(), corroborationNode,
		absenceStorage, absenceVolid, classifierFor(pve.StorageTypeDir, true),
		pve.ConfigReferenceCorroborator(refs))
	if err != nil {
		t.Fatalf("another node's local storage is another tree and proves nothing here: %v", err)
	}
	if !absent {
		t.Fatal("an empty listing on the probed node's own storage still proves the volume gone")
	}
}

// TestProveVolumeAbsent_EmptyListing_SharedStorage_CountsOtherNodes is the same
// counts against a shared storage, where every node sees one tree and a
// reference from any of them is a volume the listing should have carried.
func TestProveVolumeAbsent_EmptyListing_SharedStorage_CountsOtherNodes(t *testing.T) {
	t.Parallel()
	probe := noFormatProbe(listingOf(t))
	refs := pve.StorageReferenceCounts{absenceStorage: {corroborationPeerNode: 3}}
	absent, err := pve.ProveVolumeAbsent(context.Background(), probe.client(), corroborationNode,
		absenceStorage, absenceVolid, nfsClassifier(), pve.ConfigReferenceCorroborator(refs))
	if err == nil {
		t.Fatal("on one shared export a reference from any node contradicts an empty listing")
	}
	if absent {
		t.Fatal("an unproven absence must not read as absent")
	}
	if !strings.Contains(err.Error(), pve.CorroborationSourceConfigs) {
		t.Errorf("the refusal must name the source, got: %v", err)
	}
}

func TestProveVolumeAbsent_NonEmptyListing_SkipsCorroboration(t *testing.T) {
	t.Parallel()
	probe := noFormatProbe(listingOf(t, "nfs-images:9000/vm-9000-disk-0.qcow2"))
	absent, err := pve.ProveVolumeAbsent(context.Background(), probe.client(), corroborationNode,
		absenceStorage, absenceVolid, nfsClassifier(),
		forbiddenCorroborator(t, "other volumes in the listing prove the tree is there; no corroboration is needed"))
	if err != nil {
		t.Fatalf("ProveVolumeAbsent: %v", err)
	}
	if !absent {
		t.Fatal("a listing carrying other volumes still proves this volume is gone")
	}
}

func TestProveVolumeAbsent_PlainDirRuleWinsBeforeCorroboration(t *testing.T) {
	t.Parallel()
	probe := noFormatProbe(listingOf(t))
	absent, err := pve.ProveVolumeAbsent(context.Background(), probe.client(), corroborationNode,
		absenceStorage, absenceVolid, classifierFor(pve.StorageTypeDir, false),
		forbiddenCorroborator(t, "the plain-dir rule already refused; corroboration must not run"))
	if err == nil {
		t.Fatal("a plain dir with no is_mountpoint and an empty listing proves nothing")
	}
	if absent {
		t.Fatal("an unproven absence must not read as absent")
	}
	if !strings.Contains(err.Error(), "is_mountpoint") {
		t.Errorf("the plain-dir refusal must keep its own wording, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ConfigReferenceCorroborator
// ---------------------------------------------------------------------------

func TestConfigReferenceCorroborator_NilCounts_SaysNothing(t *testing.T) {
	t.Parallel()
	verdict, err := pve.ConfigReferenceCorroborator(nil).
		CorroborateEmptyListing(context.Background(), sharedProbe())
	if err != nil {
		t.Fatalf("a caller that never scanned is not an error: %v", err)
	}
	if verdict.Contradicted {
		t.Fatal("no scan means no evidence, which is not a contradiction")
	}
}

// referenceCounts builds the nested shape the scan produces: per storage, then
// per node of the guest whose config carried the reference.
func referenceCounts(storage, node string, count int) pve.StorageReferenceCounts {
	return pve.StorageReferenceCounts{storage: {node: count}}
}

func TestConfigReferenceCorroborator_ZeroCount_SaysNothing(t *testing.T) {
	t.Parallel()
	refs := referenceCounts("other-storage", corroborationNode, 4)
	verdict, err := pve.ConfigReferenceCorroborator(refs).
		CorroborateEmptyListing(context.Background(), sharedProbe())
	if err != nil {
		t.Fatalf("ConfigReferenceCorroborator: %v", err)
	}
	if verdict.Contradicted {
		t.Fatal("references on a different storage say nothing about this one")
	}
}

func TestConfigReferenceCorroborator_ReferencedStorage_Contradicts(t *testing.T) {
	t.Parallel()
	refs := referenceCounts(absenceStorage, corroborationNode, 3)
	verdict, err := pve.ConfigReferenceCorroborator(refs).
		CorroborateEmptyListing(context.Background(), sharedProbe())
	if err != nil {
		t.Fatalf("ConfigReferenceCorroborator: %v", err)
	}
	if !verdict.Contradicted {
		t.Fatal("configs still naming volumes on the storage contradict an empty listing")
	}
	if verdict.Source != pve.CorroborationSourceConfigs {
		t.Errorf("source: want %q, got %q", pve.CorroborationSourceConfigs, verdict.Source)
	}
	if verdict.Detail != "3 volumes on the storage are referenced by VM configs" {
		t.Errorf("detail reads wrong for the operator: %q", verdict.Detail)
	}
}

func TestConfigReferenceCorroborator_SingleReference_ReadsAsOneVolume(t *testing.T) {
	t.Parallel()
	refs := referenceCounts(absenceStorage, corroborationNode, 1)
	verdict, err := pve.ConfigReferenceCorroborator(refs).
		CorroborateEmptyListing(context.Background(), sharedProbe())
	if err != nil {
		t.Fatalf("ConfigReferenceCorroborator: %v", err)
	}
	if verdict.Detail != "1 volume on the storage is referenced by VM configs" {
		t.Errorf("a single reference must read as a sentence, got %q", verdict.Detail)
	}
}

// TestConfigReferenceCorroborator_SharedStorage_CountsEveryNode is the whole
// point of the shared reading: one nfs export is one tree, so a disk any node
// still references is a disk the listing should have carried.
func TestConfigReferenceCorroborator_SharedStorage_CountsEveryNode(t *testing.T) {
	t.Parallel()
	refs := pve.StorageReferenceCounts{absenceStorage: {corroborationPeerNode: 2}}
	verdict, err := pve.ConfigReferenceCorroborator(refs).
		CorroborateEmptyListing(context.Background(), sharedProbe())
	if err != nil {
		t.Fatalf("ConfigReferenceCorroborator: %v", err)
	}
	if !verdict.Contradicted {
		t.Fatal("on a shared storage a reference from another node is a reference to the same tree")
	}
	if verdict.Detail != "2 volumes on the storage are referenced by VM configs" {
		t.Errorf("a shared storage names no node, got %q", verdict.Detail)
	}
}

// TestConfigReferenceCorroborator_LocalStorage_IgnoresOtherNodes is the defect
// this split closed. PVE gives every node a dir storage called "local", so ten
// disks on one node's "local" would otherwise contradict an honest empty
// listing another node served for its own.
func TestConfigReferenceCorroborator_LocalStorage_IgnoresOtherNodes(t *testing.T) {
	t.Parallel()
	refs := pve.StorageReferenceCounts{absenceStorage: {corroborationPeerNode: 10}}
	verdict, err := pve.ConfigReferenceCorroborator(refs).
		CorroborateEmptyListing(context.Background(), localProbe(corroborationNode))
	if err != nil {
		t.Fatalf("ConfigReferenceCorroborator: %v", err)
	}
	if verdict.Contradicted {
		t.Fatalf("another node's local storage is another tree, got %+v", verdict)
	}
}

// TestConfigReferenceCorroborator_LocalStorage_CountsTheProbedNode is the other
// half: a reference on the node we probed is a volume that node's own listing
// should have carried.
func TestConfigReferenceCorroborator_LocalStorage_CountsTheProbedNode(t *testing.T) {
	t.Parallel()
	refs := pve.StorageReferenceCounts{absenceStorage: {
		corroborationNode:     1,
		corroborationPeerNode: 10,
	}}
	verdict, err := pve.ConfigReferenceCorroborator(refs).
		CorroborateEmptyListing(context.Background(), localProbe(corroborationNode))
	if err != nil {
		t.Fatalf("ConfigReferenceCorroborator: %v", err)
	}
	if !verdict.Contradicted {
		t.Fatal("a reference on the probed node contradicts that node's empty listing")
	}
	if verdict.Detail != "1 volume on the storage is referenced by VM configs on node "+corroborationNode {
		t.Errorf("the detail must say which node the reference lives on, got %q", verdict.Detail)
	}
}

// TestConfigReferenceCorroborator_Unclassified_ReadsOnlyTheProbedNode pins the
// conservative reading. An unclassified storage may be local, so counting
// another node's references could manufacture a contradiction out of a storage
// the probed node never shared.
func TestConfigReferenceCorroborator_Unclassified_ReadsOnlyTheProbedNode(t *testing.T) {
	t.Parallel()
	refs := pve.StorageReferenceCounts{absenceStorage: {corroborationPeerNode: 4}}
	verdict, err := pve.ConfigReferenceCorroborator(refs).
		CorroborateEmptyListing(context.Background(), unclassifiedProbe(corroborationNode))
	if err != nil {
		t.Fatalf("ConfigReferenceCorroborator: %v", err)
	}
	if verdict.Contradicted {
		t.Fatalf("a storage nobody classified cannot be assumed shared, got %+v", verdict)
	}
}

// TestStorageReferenceCounts_ReadingsOfOneScan pins the two readings against
// one another on the same counts, including the nil map every caller that never
// scanned passes.
func TestStorageReferenceCounts_ReadingsOfOneScan(t *testing.T) {
	t.Parallel()
	refs := pve.StorageReferenceCounts{absenceStorage: {
		corroborationNode:     2,
		corroborationPeerNode: 3,
	}}
	if got := refs.OnNode(absenceStorage, corroborationNode); got != 2 {
		t.Errorf("OnNode: want 2, got %d", got)
	}
	if got := refs.Anywhere(absenceStorage); got != 5 {
		t.Errorf("Anywhere: want 5, got %d", got)
	}
	if got := refs.OnNode(absenceStorage, "pve-99"); got != 0 {
		t.Errorf("a node nothing was counted on reads zero, got %d", got)
	}
	if got := refs.Anywhere("other-storage"); got != 0 {
		t.Errorf("a storage nothing was counted on reads zero, got %d", got)
	}
	var unscanned pve.StorageReferenceCounts
	if got := unscanned.OnNode(absenceStorage, corroborationNode); got != 0 {
		t.Errorf("a scan that never ran counts nothing, got %d", got)
	}
	if got := unscanned.Anywhere(absenceStorage); got != 0 {
		t.Errorf("a scan that never ran counts nothing, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// StorageStatusCorroborator
// ---------------------------------------------------------------------------

// corroborationNodesService exposes only ListStorageStatus; every other node
// call panics through the embedded nil interface, which is the point.
type corroborationNodesService struct {
	nodes.Service
	statusFn func(ctx context.Context, node, storage string) (*nodes.ListStorageStatusResponse, error)
}

func (n *corroborationNodesService) ListStorageStatus(
	ctx context.Context, node, storage string,
) (*nodes.ListStorageStatusResponse, error) {
	return n.statusFn(ctx, node, storage)
}

// corroborationStatusClient wires one scripted status answer into a pve.Client.
type corroborationStatusClient struct {
	pve.Client
	nodesSvc nodes.Service
}

func (c *corroborationStatusClient) Nodes() nodes.Service { return c.nodesSvc }

// statusClient builds a client answering the status read with resp/err.
func statusClient(resp *nodes.ListStorageStatusResponse, err error) pve.Client {
	return &corroborationStatusClient{
		nodesSvc: &corroborationNodesService{
			statusFn: func(context.Context, string, string) (*nodes.ListStorageStatusResponse, error) {
				return resp, err
			},
		},
	}
}

// statusFromJSON decodes a PVE status payload the way the wire delivers it, so
// a case can script integer-typed or string-typed fields without hand-building
// the wire scalars.
func statusFromJSON(t *testing.T, payload string) *nodes.ListStorageStatusResponse {
	t.Helper()
	out := &nodes.ListStorageStatusResponse{}
	if err := json.Unmarshal([]byte(payload), out); err != nil {
		t.Fatalf("decode storage status fixture: %v", err)
	}
	return out
}

func TestStorageStatusCorroborator_ActiveAndEmpty_SaysNothing(t *testing.T) {
	t.Parallel()
	status := statusFromJSON(t, `{"active":1,"enabled":1,"used":131072,"total":10737418240,"type":"nfs"}`)
	verdict, err := pve.StorageStatusCorroborator(statusClient(status, nil)).
		CorroborateEmptyListing(context.Background(), sharedProbe())
	if err != nil {
		t.Fatalf("StorageStatusCorroborator: %v", err)
	}
	if verdict.Contradicted {
		t.Fatalf("an active storage with nothing in it agrees with the listing, got %+v", verdict)
	}
}

func TestStorageStatusCorroborator_Inactive_Contradicts(t *testing.T) {
	t.Parallel()
	status := statusFromJSON(t, `{"active":0,"enabled":1,"used":0,"total":0,"type":"nfs"}`)
	verdict, err := pve.StorageStatusCorroborator(statusClient(status, nil)).
		CorroborateEmptyListing(context.Background(), sharedProbe())
	if err != nil {
		t.Fatalf("StorageStatusCorroborator: %v", err)
	}
	if !verdict.Contradicted {
		t.Fatal("a storage PVE calls inactive cannot have proved anything absent")
	}
	if verdict.Source != pve.CorroborationSourceStorageStatus {
		t.Errorf("source: want %q, got %q", pve.CorroborationSourceStorageStatus, verdict.Source)
	}
	if !strings.Contains(verdict.Detail, corroborationNode) {
		t.Errorf("the detail must name the node, got %q", verdict.Detail)
	}
}

func TestStorageStatusCorroborator_MissingActive_Contradicts(t *testing.T) {
	t.Parallel()
	status := statusFromJSON(t, `{"enabled":1,"type":"nfs"}`)
	verdict, err := pve.StorageStatusCorroborator(statusClient(status, nil)).
		CorroborateEmptyListing(context.Background(), sharedProbe())
	if err != nil {
		t.Fatalf("StorageStatusCorroborator: %v", err)
	}
	if !verdict.Contradicted {
		t.Fatal("a status that never said the storage is active is not a status that agreed")
	}
}

// TestStorageStatusCorroborator_UsedAtFloor_WarnsAndSaysNothing pins the
// figure's demotion to advice. PVE answers used from a statfs of the whole
// filesystem, so one NFS export carrying a backup storage's dumps beside an
// images storage crosses the floor while both storages are telling the truth,
// and has_disk and the orphan sweeps have no way to override a refusal.
func TestStorageStatusCorroborator_UsedAtFloor_WarnsAndSaysNothing(t *testing.T) {
	t.Parallel()
	logger, observer := log.NewObservedLogger(log.LevelWarn)
	ctx := log.IntoContext(context.Background(), logger)
	status := statusFromJSON(t, `{"active":1,"used":1073741824,"total":10737418240,"type":"nfs"}`)
	verdict, err := pve.StorageStatusCorroborator(statusClient(status, nil)).
		CorroborateEmptyListing(ctx, sharedProbe())
	if err != nil {
		t.Fatalf("StorageStatusCorroborator: %v", err)
	}
	if verdict.Contradicted {
		t.Fatalf("a used figure is not a listing, so it may not contradict one, got %+v", verdict)
	}
	entry, found := warnedAboutUsedBytes(observer)
	if !found {
		t.Fatalf("the operator must still be told what PVE reported, got %+v", observer.All())
	}
	if got := entry.Attrs["used_bytes"]; got != int64(1073741824) {
		t.Errorf("used_bytes: want 1073741824, got %v", got)
	}
	if got := entry.Attrs["total_bytes"]; got != int64(10737418240) {
		t.Errorf("total_bytes: want 10737418240, got %v", got)
	}
	if got := entry.Attrs["storage"]; got != absenceStorage {
		t.Errorf("storage: want %q, got %v", absenceStorage, got)
	}
	if got := entry.Attrs["node"]; got != corroborationNode {
		t.Errorf("node: want %q, got %v", corroborationNode, got)
	}
}

// warnedAboutUsedBytes finds the warning the used figure is now reported
// through, which is the only trace it leaves.
func warnedAboutUsedBytes(observer *log.Observer) (log.Entry, bool) {
	for _, entry := range observer.All() {
		if entry.Level == log.LevelWarn && strings.Contains(entry.Message, "reports bytes in use") {
			return entry, true
		}
	}
	return log.Entry{}, false
}

// TestStorageStatusCorroborator_UsedAboveFloor_StillSaysNothing covers the
// figure an operator would call obviously wrong: a storage reporting gigabytes
// against an empty listing. It is the same statfs reading, so it is the same
// advice.
func TestStorageStatusCorroborator_UsedAboveFloor_StillSaysNothing(t *testing.T) {
	t.Parallel()
	status := statusFromJSON(t, `{"active":1,"used":21474836480,"total":10737418240,"type":"nfs"}`)
	verdict, err := pve.StorageStatusCorroborator(statusClient(status, nil)).
		CorroborateEmptyListing(context.Background(), sharedProbe())
	if err != nil {
		t.Fatalf("StorageStatusCorroborator: %v", err)
	}
	if verdict.Contradicted {
		t.Fatalf("the used figure is advisory whatever it reads, got %+v", verdict)
	}
}

// TestStorageStatusCorroborator_TransientError_IsRetried pins the retry the one
// read now rides. A pvedaemon worker recycling during it would otherwise turn
// into a permanent refusal on a delete path.
func TestStorageStatusCorroborator_TransientError_IsRetried(t *testing.T) {
	t.Parallel()
	attempts := 0
	client := &corroborationStatusClient{
		nodesSvc: &corroborationNodesService{
			statusFn: func(context.Context, string, string) (*nodes.ListStorageStatusResponse, error) {
				attempts++
				if attempts == 1 {
					return nil, makeAPIErr(596, "pvedaemon worker recycled")
				}
				return statusFromJSON(t, `{"active":1,"used":0,"total":10737418240,"type":"nfs"}`), nil
			},
		},
	}
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	verdict, err := pve.StorageStatusCorroborator(client).CorroborateEmptyListing(ctx, sharedProbe())
	if err != nil {
		t.Fatalf("a worker recycle must not become a permanent refusal: %v", err)
	}
	if verdict.Contradicted {
		t.Fatalf("the second attempt answered, and it agreed with the listing, got %+v", verdict)
	}
	if attempts != 2 {
		t.Errorf("want one retry after the transient failure, got %d attempts", attempts)
	}
}

// TestStorageStatusCorroborator_PermanentError_IsNotRetried keeps the retry
// narrow: a verdict that will never change is returned on the first read, so a
// misconfigured grant does not spend the whole ladder.
func TestStorageStatusCorroborator_PermanentError_IsNotRetried(t *testing.T) {
	t.Parallel()
	attempts := 0
	client := &corroborationStatusClient{
		nodesSvc: &corroborationNodesService{
			statusFn: func(context.Context, string, string) (*nodes.ListStorageStatusResponse, error) {
				attempts++
				return nil, makeAPIErr(403, "Permission check failed")
			},
		},
	}
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	if _, err := pve.StorageStatusCorroborator(client).CorroborateEmptyListing(ctx, sharedProbe()); err == nil {
		t.Fatal("a denied read did not land, so it cannot clear an empty listing")
	}
	if attempts != 1 {
		t.Errorf("a permanent answer owes no retry, got %d attempts", attempts)
	}
}

func TestStorageStatusCorroborator_UsedBelowFloor_SaysNothing(t *testing.T) {
	t.Parallel()
	status := statusFromJSON(t, `{"active":1,"used":1073741823,"total":10737418240,"type":"nfs"}`)
	verdict, err := pve.StorageStatusCorroborator(statusClient(status, nil)).
		CorroborateEmptyListing(context.Background(), sharedProbe())
	if err != nil {
		t.Fatalf("StorageStatusCorroborator: %v", err)
	}
	if verdict.Contradicted {
		t.Fatalf("a used figure below the floor is a storage's own overhead, got %+v", verdict)
	}
}

func TestStorageStatusCorroborator_StringTypedFields_Decode(t *testing.T) {
	t.Parallel()
	// PVE answers these as strings on some versions and endpoints, which is why
	// nothing here decodes into a plain int.
	logger, observer := log.NewObservedLogger(log.LevelWarn)
	ctx := log.IntoContext(context.Background(), logger)
	status := statusFromJSON(t, `{"active":"1","used":"2147483648","total":"10737418240","type":"nfs"}`)
	verdict, err := pve.StorageStatusCorroborator(statusClient(status, nil)).
		CorroborateEmptyListing(ctx, sharedProbe())
	if err != nil {
		t.Fatalf("StorageStatusCorroborator: %v", err)
	}
	if verdict.Contradicted {
		t.Fatalf("the used figure is advisory whichever way PVE typed it, got %+v", verdict)
	}
	entry, found := warnedAboutUsedBytes(observer)
	if !found {
		t.Fatal("a string-typed used figure above the floor must still reach the operator")
	}
	if got := entry.Attrs["used_bytes"]; got != int64(2147483648) {
		t.Errorf("the string-typed used figure must decode, got %v", got)
	}
}

func TestStorageStatusCorroborator_StringTypedInactive_Decodes(t *testing.T) {
	t.Parallel()
	status := statusFromJSON(t, `{"active":"0","used":"0","total":"0","type":"nfs"}`)
	verdict, err := pve.StorageStatusCorroborator(statusClient(status, nil)).
		CorroborateEmptyListing(context.Background(), sharedProbe())
	if err != nil {
		t.Fatalf("StorageStatusCorroborator: %v", err)
	}
	if !verdict.Contradicted {
		t.Fatal("a string-typed inactive flag must read as inactive")
	}
}

func TestStorageStatusCorroborator_TransportError_IsReturned(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("pveproxy backend gone (code: 596)")
	_, err := pve.StorageStatusCorroborator(statusClient(nil, sentinel)).
		CorroborateEmptyListing(context.Background(), sharedProbe())
	if err == nil {
		t.Fatal("a status read that did not land is not a status read that agreed")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("the transport error must be wrapped, got: %v", err)
	}
	if !strings.Contains(err.Error(), absenceStorage) || !strings.Contains(err.Error(), corroborationNode) {
		t.Errorf("the error must name the storage and the node, got: %v", err)
	}
}

func TestStorageStatusCorroborator_EmptyResponse_IsAnError(t *testing.T) {
	t.Parallel()
	_, err := pve.StorageStatusCorroborator(statusClient(nil, nil)).
		CorroborateEmptyListing(context.Background(), sharedProbe())
	if err == nil {
		t.Fatal("a status with no payload establishes nothing and must fail closed")
	}
}

func TestStorageStatusCorroborator_MissingNodesService_IsAnError(t *testing.T) {
	t.Parallel()
	_, err := pve.StorageStatusCorroborator(&corroborationStatusClient{}).
		CorroborateEmptyListing(context.Background(), sharedProbe())
	if err == nil {
		t.Fatal("a client with no nodes service cannot corroborate anything")
	}
}

func TestStorageStatusCorroborator_MissingArguments_AreErrors(t *testing.T) {
	t.Parallel()
	corroborator := pve.StorageStatusCorroborator(statusClient(nil, nil))
	nodeless := sharedProbe()
	nodeless.Node = ""
	if _, err := corroborator.CorroborateEmptyListing(context.Background(), nodeless); err == nil {
		t.Error("an empty node name must not reach the API")
	}
	storageless := sharedProbe()
	storageless.Storage = ""
	if _, err := corroborator.CorroborateEmptyListing(context.Background(), storageless); err == nil {
		t.Error("an empty storage name must not reach the API")
	}
	if _, err := pve.StorageStatusCorroborator(nil).
		CorroborateEmptyListing(context.Background(), sharedProbe()); err == nil {
		t.Error("a nil client must not reach the API")
	}
}

func TestCorroboratorFunc_NilFunction_IsAnError(t *testing.T) {
	t.Parallel()
	_, err := pve.CorroboratorFunc("a source with no implementation", nil).
		CorroborateEmptyListing(context.Background(), sharedProbe())
	if err == nil {
		t.Fatal("a corroborator with no implementation is a wiring fault, not a source with nothing to say")
	}
}

// ---------------------------------------------------------------------------
// The counts the holder scan carries out
// ---------------------------------------------------------------------------

// TestResolveDiskHolder_CountsStorageReferencesAcrossGuests pins what the scan
// records on its way past the guests that do not hold the disk: one count per
// storage, cloud-init and cdrom entries left out, and an answer even when
// nothing holds the volume.
func TestResolveDiskHolder_CountsStorageReferencesAcrossGuests(t *testing.T) {
	t.Parallel()
	configs := map[int]map[string]any{
		100: {
			"scsi0": "nfs-images:100/vm-100-disk-0.qcow2,size=32G",
			"scsi1": "local-lvm:vm-100-disk-1,iothread=1",
			// The cloud-init drive sits on a storage under test and must not
			// be counted: it is not a volume anyone proves an absence for.
			"ide2": "nfs-images:100/vm-100-cloudinit.qcow2,media=cdrom",
			// Neither is an ISO mounted from the same storage.
			"ide0": "nfs-images:iso/ubuntu.iso,media=cdrom",
		},
		200: {
			"scsi0":  "nfs-images:200/vm-200-disk-0.qcow2,size=64G",
			"virtio": "not-a-disk-key",
			"ide2":   "none,media=cdrom",
			// A detached volume PVE demoted to an unused slot is still a
			// reference: destroying the VM would take it with it.
			"unused0": "local-lvm:vm-200-disk-3",
		},
	}

	c := &diskClusterClient{
		clusterSvc: &diskFakeCluster{
			listFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return diskClusterResp(
					map[string]any{"vmid": int64(100), "node": "pve-01"},
					map[string]any{"vmid": int64(200), "node": "pve-02"},
				), nil
			},
		},
		qemuSvc: &diskFakeQEMUFn{
			fn: func(_ string, vmid int) (map[string]any, error) {
				cfg, ok := configs[vmid]
				if !ok {
					t.Errorf("unexpected config read for vmid %d", vmid)
				}
				return cfg, nil
			},
		},
	}

	holder, err := pve.ResolveDiskHolder(
		context.Background(), c, nopLogger(), "nfs-images:999/vm-999-disk-0.qcow2", parkerTestCfg())
	if err != nil {
		t.Fatalf("ResolveDiskHolder: %v", err)
	}
	if holder.Found {
		t.Fatal("nothing holds the probed volume")
	}
	if holder.StorageReferences == nil {
		t.Fatal("a scan that ran must carry its counts out, even when it found no holder")
	}
	if got := holder.StorageReferences.Anywhere("nfs-images"); got != 2 {
		t.Errorf("nfs-images: want 2 disk references (cloud-init and ISO excluded), got %d", got)
	}
	if got := holder.StorageReferences.OnNode("nfs-images", "pve-01"); got != 1 {
		t.Errorf("nfs-images on pve-01: want the one reference VM 100 carries, got %d", got)
	}
	if got := holder.StorageReferences.OnNode("nfs-images", "pve-02"); got != 1 {
		t.Errorf("nfs-images on pve-02: want the one reference VM 200 carries, got %d", got)
	}
	if got := holder.StorageReferences.OnNode("local-lvm", "pve-01"); got != 1 {
		t.Errorf("local-lvm on pve-01: want 1 disk reference, got %d", got)
	}
	if got := holder.StorageReferences.OnNode("local-lvm", "pve-02"); got != 1 {
		t.Errorf("local-lvm on pve-02: want the unused-slot reference to count, got %d", got)
	}
	if got := len(holder.StorageReferences); got != 2 {
		t.Errorf("only the two real storages must be counted, got %d entries: %v", got, holder.StorageReferences)
	}
}

// TestResolveDiskHolder_CountsUnusedSlotReferences is the unused slot on its
// own. A disk PVE demoted out of its bus slot is exactly the reference an empty
// listing would otherwise let delete_disk remove out from under the VM that
// still holds it.
func TestResolveDiskHolder_CountsUnusedSlotReferences(t *testing.T) {
	t.Parallel()

	c := &diskClusterClient{
		clusterSvc: &diskFakeCluster{
			listFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return diskClusterResp(map[string]any{"vmid": int64(400), "node": "pve-03"}), nil
			},
		},
		qemuSvc: &diskFakeQEMUFn{
			fn: func(_ string, _ int) (map[string]any, error) {
				return map[string]any{
					"unused0": "nfs-images:400/vm-400-disk-0.qcow2",
					"unused1": "nfs-images:400/vm-400-disk-1.qcow2,replicate=0",
					// A cloud-init drive parked in an unused slot is still not
					// a volume anyone proves an absence for.
					"unused2": "nfs-images:400/vm-400-cloudinit.qcow2",
					// A stale unused slot naming the volume the scan is looking
					// for is the one entry that may never count: the scan does
					// not read it as a holder, so the caller goes on to prove
					// that volume absent, and its own stale entry cannot be
					// evidence against that.
					"unused3": "nfs-images:999/vm-999-disk-0.qcow2",
				}, nil
			},
		},
	}

	holder, err := pve.ResolveDiskHolder(
		context.Background(), c, nopLogger(), "nfs-images:999/vm-999-disk-0.qcow2", parkerTestCfg())
	if err != nil {
		t.Fatalf("ResolveDiskHolder: %v", err)
	}
	if got := holder.StorageReferences.OnNode("nfs-images", "pve-03"); got != 2 {
		t.Errorf("nfs-images on pve-03: want both unused disks counted and the cloud-init drive and the "+
			"scan's own target left out, got %d", got)
	}
}

// TestResolveDiskHolder_CountsRideOutOnTheHolder covers the other half: a scan
// that found a holder still reports what it counted on the way there, which is
// what the attach paths read.
func TestResolveDiskHolder_CountsRideOutOnTheHolder(t *testing.T) {
	t.Parallel()
	volid := "local-lvm:vm-300-disk-0"

	c := &diskClusterClient{
		clusterSvc: &diskFakeCluster{
			listFn: func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
				return diskClusterResp(map[string]any{"vmid": int64(300), "node": "pve-01"}), nil
			},
		},
		qemuSvc: &diskFakeQEMUFn{
			fn: func(_ string, _ int) (map[string]any, error) {
				return map[string]any{"scsi0": volid, "scsi1": "local-lvm:vm-300-disk-1"}, nil
			},
		},
	}

	holder, err := pve.ResolveDiskHolder(context.Background(), c, nopLogger(), volid, parkerTestCfg())
	if err != nil {
		t.Fatalf("ResolveDiskHolder: %v", err)
	}
	if !holder.Found || holder.VMID != 300 {
		t.Fatalf("holder: want vmid 300, got %+v", holder)
	}
	// The disk the scan was looking for is left out of its own counts, so the
	// holder's config contributes only its other disk.
	if got := holder.StorageReferences.OnNode("local-lvm", "pve-01"); got != 1 {
		t.Errorf("local-lvm: want the holder's other disk counted and the scan target left out, got %d", got)
	}
}
