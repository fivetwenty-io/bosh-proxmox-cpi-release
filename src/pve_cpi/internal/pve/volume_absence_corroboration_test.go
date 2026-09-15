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

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// corroborationNode is the node every case below observes from.
const corroborationNode = "pve-01"

// silentCorroborator has nothing to say and records that it was asked, which is
// how the order cases tell "consulted and passed" from "never reached".
func silentCorroborator(calls *int) pve.EmptyListingCorroborator {
	return pve.CorroboratorFunc("a quiet source", func(context.Context, string, string, string) (pve.Corroboration, error) {
		*calls++
		return pve.Corroboration{}, nil
	})
}

// contradictingCorroborator contradicts the listing with a fixed detail.
func contradictingCorroborator(calls *int, source, detail string) pve.EmptyListingCorroborator {
	return pve.CorroboratorFunc(source, func(context.Context, string, string, string) (pve.Corroboration, error) {
		*calls++
		return pve.Corroboration{Contradicted: true, Source: source, Detail: detail}, nil
	})
}

// failingCorroborator is a check that did not land.
func failingCorroborator(calls *int, err error) pve.EmptyListingCorroborator {
	return pve.CorroboratorFunc("a source that broke", func(context.Context, string, string, string) (pve.Corroboration, error) {
		*calls++
		return pve.Corroboration{}, err
	})
}

// forbiddenCorroborator fails the test if it is ever consulted.
func forbiddenCorroborator(t *testing.T, why string) pve.EmptyListingCorroborator {
	t.Helper()
	return pve.CorroboratorFunc("a source that must not be asked",
		func(context.Context, string, string, string) (pve.Corroboration, error) {
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
	var sawNode, sawStorage, sawVolume string
	recorder := pve.CorroboratorFunc("a source that reads its arguments",
		func(_ context.Context, node, storage, volume string) (pve.Corroboration, error) {
			sawNode, sawStorage, sawVolume = node, storage, volume
			return pve.Corroboration{}, nil
		})
	if _, err := proveWithCorroborators(t, recorder); err != nil {
		t.Fatalf("a silent corroborator leaves the answer alone: %v", err)
	}
	if sawNode != corroborationNode || sawStorage != absenceStorage {
		t.Errorf("the corroborator must be told where it is looking, got node %q storage %q", sawNode, sawStorage)
	}
	if sawVolume != absenceVolid {
		t.Errorf("the corroborator must be told which volume is under proof, got %q", sawVolume)
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
	bare := pve.CorroboratorFunc("", func(context.Context, string, string, string) (pve.Corroboration, error) {
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
		CorroborateEmptyListing(context.Background(), corroborationNode, absenceStorage, absenceVolid)
	if err != nil {
		t.Fatalf("a caller that never scanned is not an error: %v", err)
	}
	if verdict.Contradicted {
		t.Fatal("no scan means no evidence, which is not a contradiction")
	}
}

func TestConfigReferenceCorroborator_ZeroCount_SaysNothing(t *testing.T) {
	t.Parallel()
	refs := pve.StorageReferenceCounts{"other-storage": 4}
	verdict, err := pve.ConfigReferenceCorroborator(refs).
		CorroborateEmptyListing(context.Background(), corroborationNode, absenceStorage, absenceVolid)
	if err != nil {
		t.Fatalf("ConfigReferenceCorroborator: %v", err)
	}
	if verdict.Contradicted {
		t.Fatal("references on a different storage say nothing about this one")
	}
}

func TestConfigReferenceCorroborator_ReferencedStorage_Contradicts(t *testing.T) {
	t.Parallel()
	refs := pve.StorageReferenceCounts{absenceStorage: 3}
	verdict, err := pve.ConfigReferenceCorroborator(refs).
		CorroborateEmptyListing(context.Background(), corroborationNode, absenceStorage, absenceVolid)
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
	refs := pve.StorageReferenceCounts{absenceStorage: 1}
	verdict, err := pve.ConfigReferenceCorroborator(refs).
		CorroborateEmptyListing(context.Background(), corroborationNode, absenceStorage, absenceVolid)
	if err != nil {
		t.Fatalf("ConfigReferenceCorroborator: %v", err)
	}
	if verdict.Detail != "1 volume on the storage is referenced by VM configs" {
		t.Errorf("a single reference must read as a sentence, got %q", verdict.Detail)
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
		CorroborateEmptyListing(context.Background(), corroborationNode, absenceStorage, absenceVolid)
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
		CorroborateEmptyListing(context.Background(), corroborationNode, absenceStorage, absenceVolid)
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
		CorroborateEmptyListing(context.Background(), corroborationNode, absenceStorage, absenceVolid)
	if err != nil {
		t.Fatalf("StorageStatusCorroborator: %v", err)
	}
	if !verdict.Contradicted {
		t.Fatal("a status that never said the storage is active is not a status that agreed")
	}
}

func TestStorageStatusCorroborator_UsedAtFloor_Contradicts(t *testing.T) {
	t.Parallel()
	status := statusFromJSON(t, `{"active":1,"used":1073741824,"total":10737418240,"type":"nfs"}`)
	verdict, err := pve.StorageStatusCorroborator(statusClient(status, nil)).
		CorroborateEmptyListing(context.Background(), corroborationNode, absenceStorage, absenceVolid)
	if err != nil {
		t.Fatalf("StorageStatusCorroborator: %v", err)
	}
	if !verdict.Contradicted {
		t.Fatalf("used at the floor contradicts a listing that showed nothing, got %+v", verdict)
	}
	if !strings.Contains(verdict.Detail, "1073741824") {
		t.Errorf("the detail must name the used bytes, got %q", verdict.Detail)
	}
	if !strings.Contains(verdict.Detail, "10737418240") {
		t.Errorf("the detail must name the capacity the used figure sits against, got %q", verdict.Detail)
	}
}

func TestStorageStatusCorroborator_UsedBelowFloor_SaysNothing(t *testing.T) {
	t.Parallel()
	status := statusFromJSON(t, `{"active":1,"used":1073741823,"total":10737418240,"type":"nfs"}`)
	verdict, err := pve.StorageStatusCorroborator(statusClient(status, nil)).
		CorroborateEmptyListing(context.Background(), corroborationNode, absenceStorage, absenceVolid)
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
	status := statusFromJSON(t, `{"active":"1","used":"2147483648","total":"10737418240","type":"nfs"}`)
	verdict, err := pve.StorageStatusCorroborator(statusClient(status, nil)).
		CorroborateEmptyListing(context.Background(), corroborationNode, absenceStorage, absenceVolid)
	if err != nil {
		t.Fatalf("StorageStatusCorroborator: %v", err)
	}
	if !verdict.Contradicted {
		t.Fatal("a string-typed used figure above the floor contradicts the listing just as an integer one does")
	}
	if !strings.Contains(verdict.Detail, "2147483648") {
		t.Errorf("the string-typed used figure must decode, got %q", verdict.Detail)
	}
}

func TestStorageStatusCorroborator_StringTypedInactive_Decodes(t *testing.T) {
	t.Parallel()
	status := statusFromJSON(t, `{"active":"0","used":"0","total":"0","type":"nfs"}`)
	verdict, err := pve.StorageStatusCorroborator(statusClient(status, nil)).
		CorroborateEmptyListing(context.Background(), corroborationNode, absenceStorage, absenceVolid)
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
		CorroborateEmptyListing(context.Background(), corroborationNode, absenceStorage, absenceVolid)
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
		CorroborateEmptyListing(context.Background(), corroborationNode, absenceStorage, absenceVolid)
	if err == nil {
		t.Fatal("a status with no payload establishes nothing and must fail closed")
	}
}

func TestStorageStatusCorroborator_MissingNodesService_IsAnError(t *testing.T) {
	t.Parallel()
	_, err := pve.StorageStatusCorroborator(&corroborationStatusClient{}).
		CorroborateEmptyListing(context.Background(), corroborationNode, absenceStorage, absenceVolid)
	if err == nil {
		t.Fatal("a client with no nodes service cannot corroborate anything")
	}
}

func TestStorageStatusCorroborator_MissingArguments_AreErrors(t *testing.T) {
	t.Parallel()
	corroborator := pve.StorageStatusCorroborator(statusClient(nil, nil))
	if _, err := corroborator.CorroborateEmptyListing(context.Background(), "", absenceStorage, absenceVolid); err == nil {
		t.Error("an empty node name must not reach the API")
	}
	if _, err := corroborator.CorroborateEmptyListing(context.Background(), corroborationNode, "", absenceVolid); err == nil {
		t.Error("an empty storage name must not reach the API")
	}
	if _, err := pve.StorageStatusCorroborator(nil).
		CorroborateEmptyListing(context.Background(), corroborationNode, absenceStorage, absenceVolid); err == nil {
		t.Error("a nil client must not reach the API")
	}
}

func TestCorroboratorFunc_NilFunction_IsAnError(t *testing.T) {
	t.Parallel()
	_, err := pve.CorroboratorFunc("a source with no implementation", nil).
		CorroborateEmptyListing(context.Background(), corroborationNode, absenceStorage, absenceVolid)
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
	if got := holder.StorageReferences["nfs-images"]; got != 2 {
		t.Errorf("nfs-images: want 2 disk references (cloud-init and ISO excluded), got %d", got)
	}
	if got := holder.StorageReferences["local-lvm"]; got != 1 {
		t.Errorf("local-lvm: want 1 disk reference, got %d", got)
	}
	if got := len(holder.StorageReferences); got != 2 {
		t.Errorf("only the two real storages must be counted, got %d entries: %v", got, holder.StorageReferences)
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
	if got := holder.StorageReferences["local-lvm"]; got != 2 {
		t.Errorf("local-lvm: want 2 disk references on the holder's own config, got %d", got)
	}
}
