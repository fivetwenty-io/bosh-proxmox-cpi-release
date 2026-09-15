package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// ProveVolumeAbsent — the listing is the proof, and the volume GET never is
// ---------------------------------------------------------------------------

// absenceVolid is the volid every case below probes for. It matches the storage
// named in liveVolumeSizeInfoNoFormat so the scripted PVE reply and the volume
// under test agree, the way they do on the wire.
const absenceVolid = "nfs-images:9999/vm-9999-disk-0.qcow2"

// absenceStorage is the storage part of absenceVolid.
const absenceStorage = "nfs-images"

// absenceStorageService is a storage.Service fake exposing only Exists. Every
// other call panics through the embedded nil interface, which is what we want:
// ProveVolumeAbsent must not reach for anything else.
type absenceStorageService struct {
	storage.Service
	existsFn func(ctx context.Context, node, storageName, volume string) (bool, error)
}

func (s *absenceStorageService) Exists(ctx context.Context, node, storageName, volume string) (bool, error) {
	return s.existsFn(ctx, node, storageName, volume)
}

// absenceNodesService is a nodes.Service fake exposing only ListStorageContent.
type absenceNodesService struct {
	nodes.Service
	listFn func(ctx context.Context, node, storageName string, params *nodes.ListStorageContentParams) (
		*nodes.ListStorageContentResponse, error)
}

func (n *absenceNodesService) ListStorageContent(
	ctx context.Context, node, storageName string, params *nodes.ListStorageContentParams,
) (*nodes.ListStorageContentResponse, error) {
	return n.listFn(ctx, node, storageName, params)
}

// absenceClient scripts the point probe and the content listing independently,
// which is the whole point: the two answers disagree on file storage, and the
// listing is the one that settles it.
type absenceClient struct {
	pve.Client
	storageSvc storage.Service
	nodesSvc   nodes.Service
}

func (c *absenceClient) Storage() storage.Service { return c.storageSvc }
func (c *absenceClient) Nodes() nodes.Service     { return c.nodesSvc }

// absenceVisibleClient answers the audit-visibility proof that
// ObserveStorageVolumeContent demands before it will call a volume absent.
// absenceClient deliberately does not implement it, so a case that needs a
// provable absence wraps its client and a case that needs the reduced-ACL
// outcome does not.
type absenceVisibleClient struct {
	*absenceClient
	visibilityErr error
}

func (c *absenceVisibleClient) StorageAuditVisibility(context.Context) error { return c.visibilityErr }

// listingOf builds a content listing from volids.
func listingOf(t *testing.T, volids ...string) *nodes.ListStorageContentResponse {
	t.Helper()
	out := make(nodes.ListStorageContentResponse, 0, len(volids))
	for _, volid := range volids {
		raw, err := json.Marshal(map[string]string{"volid": volid})
		if err != nil {
			t.Fatalf("marshal listing row: %v", err)
		}
		out = append(out, raw)
	}
	return &out
}

// absenceProbe scripts one point-probe answer and one listing answer.
type absenceProbe struct {
	existsFn func(ctx context.Context, node, storageName, volume string) (bool, error)
	listFn   func(ctx context.Context, node, storageName string, params *nodes.ListStorageContentParams) (
		*nodes.ListStorageContentResponse, error)
	visible       bool
	visibilityErr error
}

// client assembles the scripted services into the pve.Client the helper takes.
func (p absenceProbe) client() pve.Client {
	base := &absenceClient{
		storageSvc: &absenceStorageService{existsFn: p.existsFn},
		nodesSvc:   &absenceNodesService{listFn: p.listFn},
	}
	if !p.visible {
		return base
	}
	return &absenceVisibleClient{absenceClient: base, visibilityErr: p.visibilityErr}
}

// noFormatProbe scripts PVE's file-storage reply for a stat that did not work,
// which is what every classification case starts from.
func noFormatProbe(listing *nodes.ListStorageContentResponse) absenceProbe {
	return absenceProbe{
		existsFn: func(context.Context, string, string, string) (bool, error) {
			return false, makeAPIErr(500, liveVolumeSizeInfoNoFormat)
		},
		listFn: func(context.Context, string, string, *nodes.ListStorageContentParams) (
			*nodes.ListStorageContentResponse, error) {
			return listing, nil
		},
		visible: true,
	}
}

// classifierFor returns a classifier answering with one fixed storage.
func classifierFor(storageType string, isMountpoint bool) pve.StorageClassifier {
	return func(context.Context) (pve.StorageInfo, bool) {
		return pve.StorageInfo{Name: absenceStorage, Type: storageType, IsMountpoint: isMountpoint}, true
	}
}

// nfsClassifier is the common case: a storage PVE refuses to activate when its
// export is unreachable, so a missing volid in a listing is an absence.
func nfsClassifier() pve.StorageClassifier { return classifierFor(pve.StorageTypeNFS, false) }

func TestProveVolumeAbsent_PointProbe404_ProvesAbsenceWithoutListing(t *testing.T) {
	t.Parallel()
	probe := absenceProbe{
		existsFn: func(context.Context, string, string, string) (bool, error) {
			return false, makeAPIErr(404, "no such volume")
		},
		listFn: func(context.Context, string, string, *nodes.ListStorageContentParams) (
			*nodes.ListStorageContentResponse, error) {
			t.Error("a 404 from the point probe already answers; the listing must not be read")
			return nil, errors.New("unexpected listing call")
		},
	}
	absent, err := pve.ProveVolumeAbsent(
		context.Background(), probe.client(), "pve-01", absenceStorage, absenceVolid, nfsClassifier())
	if err != nil {
		t.Fatalf("ProveVolumeAbsent: %v", err)
	}
	if !absent {
		t.Fatal("a 404 from the point probe proves the volume is not there")
	}
}

func TestProveVolumeAbsent_PresentVolume_AnswersPresentWithoutListing(t *testing.T) {
	t.Parallel()
	probe := absenceProbe{
		existsFn: func(context.Context, string, string, string) (bool, error) { return true, nil },
		listFn: func(context.Context, string, string, *nodes.ListStorageContentParams) (
			*nodes.ListStorageContentResponse, error) {
			t.Error("a clean point probe already answers; the listing must not be read")
			return nil, errors.New("unexpected listing call")
		},
	}
	absent, err := pve.ProveVolumeAbsent(
		context.Background(), probe.client(), "pve-01", absenceStorage, absenceVolid, nfsClassifier())
	if err != nil {
		t.Fatalf("ProveVolumeAbsent: %v", err)
	}
	if absent {
		t.Fatal("the point probe found the volume, so it must not read as absent")
	}
}

func TestProveVolumeAbsent_NoFormatThenCleanListing_ProvesAbsence(t *testing.T) {
	t.Parallel()
	probe := noFormatProbe(listingOf(t, "nfs-images:9000/vm-9000-disk-0.qcow2"))
	absent, err := pve.ProveVolumeAbsent(
		context.Background(), probe.client(), "pve-01", absenceStorage, absenceVolid, nfsClassifier())
	if err != nil {
		t.Fatalf("ProveVolumeAbsent: %v", err)
	}
	if !absent {
		t.Fatal("a listing that does not carry the volid proves the volume is gone on nfs")
	}
}

func TestProveVolumeAbsent_NoFormatThenListingCarriesVolid_AnswersPresent(t *testing.T) {
	t.Parallel()
	probe := noFormatProbe(listingOf(t, absenceVolid, "nfs-images:9000/vm-9000-disk-0.qcow2"))
	absent, err := pve.ProveVolumeAbsent(
		context.Background(), probe.client(), "pve-01", absenceStorage, absenceVolid, nfsClassifier())
	if err != nil {
		t.Fatalf("ProveVolumeAbsent: %v", err)
	}
	if absent {
		t.Fatal("the volid is in the listing, so the volume is there whatever the point probe said")
	}
}

func TestProveVolumeAbsent_NoFormatThenListingFails_IsUnproven(t *testing.T) {
	t.Parallel()
	probe := absenceProbe{
		existsFn: func(context.Context, string, string, string) (bool, error) {
			return false, makeAPIErr(500, liveVolumeSizeInfoNoFormat)
		},
		listFn: func(context.Context, string, string, *nodes.ListStorageContentParams) (
			*nodes.ListStorageContentResponse, error) {
			return nil, makeAPIErr(596, "connection refused")
		},
		visible: true,
	}
	absent, err := pve.ProveVolumeAbsent(
		context.Background(), probe.client(), "pve-01", absenceStorage, absenceVolid, nfsClassifier())
	if err == nil {
		t.Fatal("a listing that did not come back proves nothing; the caller has to fail closed")
	}
	if absent {
		t.Fatal("an unproven outcome must never read as absent")
	}
	if strings.Contains(err.Error(), "connection refused") {
		t.Errorf("the observation error is scrubbed to a diagnostic category; got %q", err.Error())
	}
}

func TestProveVolumeAbsent_NoFormatWithoutVisibilityReader_IsUnproven(t *testing.T) {
	t.Parallel()
	probe := noFormatProbe(listingOf(t))
	probe.visible = false
	absent, err := pve.ProveVolumeAbsent(
		context.Background(), probe.client(), "pve-01", absenceStorage, absenceVolid, nfsClassifier())
	if err == nil {
		t.Fatal("a client that cannot prove audit visibility cannot prove an absence")
	}
	if absent {
		t.Fatal("an unproven outcome must never read as absent")
	}
}

func TestProveVolumeAbsent_NoFormatWithFailingVisibility_IsUnproven(t *testing.T) {
	t.Parallel()
	probe := noFormatProbe(listingOf(t))
	probe.visibilityErr = errors.New("allocation audit requires Sys.Audit at /access to inspect unfiltered ACL paths")
	absent, err := pve.ProveVolumeAbsent(
		context.Background(), probe.client(), "pve-01", absenceStorage, absenceVolid, nfsClassifier())
	if err == nil {
		t.Fatal("a reduced-ACL token cannot prove an absence, and that must surface as unproven")
	}
	if absent {
		t.Fatal("an unproven outcome must never read as absent")
	}
}

// TestProveVolumeAbsent_UnrelatedProbeError_FallsThroughToTheListing pins that
// the fall-through is not gated on the "no format" wording. The listing is the
// stronger observation on every backend, and gating on one plugin's phrasing
// would leave the same hole open for the next plugin that invents its own 500.
func TestProveVolumeAbsent_UnrelatedProbeError_FallsThroughToTheListing(t *testing.T) {
	t.Parallel()
	probe := noFormatProbe(listingOf(t, "nfs-images:9000/vm-9000-disk-0.qcow2"))
	probe.existsFn = func(context.Context, string, string, string) (bool, error) {
		return false, errors.New("unexpected EOF")
	}
	absent, err := pve.ProveVolumeAbsent(
		context.Background(), probe.client(), "pve-01", absenceStorage, absenceVolid, nfsClassifier())
	if err != nil {
		t.Fatalf("ProveVolumeAbsent: %v", err)
	}
	if !absent {
		t.Fatal("a clean listing proves the absence whatever shape the point probe failed with")
	}
}

// ---------------------------------------------------------------------------
// Storage classification — which listings can carry a proof at all
// ---------------------------------------------------------------------------

// TestProveVolumeAbsent_StorageClassification walks the storage types from the
// "no format" reply and a listing that does not carry the volid, which is the
// shape an operator meets after a parker and its disk were removed out of band.
func TestProveVolumeAbsent_StorageClassification(t *testing.T) {
	t.Parallel()
	other := []string{"nfs-images:9000/vm-9000-disk-0.qcow2", "nfs-images:9001/vm-9001-disk-0.qcow2"}
	cases := []struct {
		name       string
		classify   pve.StorageClassifier
		listing    []string
		wantAbsent bool
		wantErr    bool
	}{
		{"nfs with an empty listing", classifierFor(pve.StorageTypeNFS, false), nil, true, false},
		{"cifs with an empty listing", classifierFor(pve.StorageTypeCIFS, false), nil, true, false},
		{"lvmthin with an empty listing", classifierFor(pve.StorageTypeLVMThin, false), nil, true, false},
		{"dir carrying is_mountpoint", classifierFor(pve.StorageTypeDir, true), nil, true, false},
		{"plain dir with an empty listing", classifierFor(pve.StorageTypeDir, false), nil, false, true},
		{"plain dir with other volumes", classifierFor(pve.StorageTypeDir, false), other, true, false},
		{"btrfs carrying is_mountpoint", classifierFor(pve.StorageTypeBTRFS, true), nil, true, false},
		{"plain btrfs with an empty listing", classifierFor(pve.StorageTypeBTRFS, false), nil, false, true},
		{"plain btrfs with other volumes", classifierFor(pve.StorageTypeBTRFS, false), other, true, false},
		{"an unrecognized type with an empty listing", classifierFor("some-future-plugin", false), nil, false, true},
		{
			"a classifier that cannot identify the storage",
			func(context.Context) (pve.StorageInfo, bool) { return pve.StorageInfo{}, false },
			nil, false, true,
		},
		{"no classifier at all", nil, nil, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe := noFormatProbe(listingOf(t, tc.listing...))
			absent, err := pve.ProveVolumeAbsent(
				context.Background(), probe.client(), "pve-01", absenceStorage, absenceVolid, tc.classify)
			if tc.wantErr && err == nil {
				t.Fatal("expected the absence to read as unproven")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ProveVolumeAbsent: %v", err)
			}
			if absent != tc.wantAbsent {
				t.Fatalf("absent = %v, want %v", absent, tc.wantAbsent)
			}
		})
	}
}

// TestProveVolumeAbsent_PlainDirEmptyListing_NamesTheFix pins the operator-facing
// half of the dir rule. The message has to name the storage and the flag,
// because setting is_mountpoint is what turns a dropped mount into an honest
// PVE failure instead of an empty array.
func TestProveVolumeAbsent_PlainDirEmptyListing_NamesTheFix(t *testing.T) {
	t.Parallel()
	probe := noFormatProbe(listingOf(t))
	_, err := pve.ProveVolumeAbsent(context.Background(), probe.client(), "pve-01",
		absenceStorage, absenceVolid, classifierFor(pve.StorageTypeDir, false))
	if err == nil {
		t.Fatal("an empty listing on a plain dir storage proves nothing")
	}
	for _, want := range []string{absenceStorage, "is_mountpoint", pve.StorageTypeDir} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// ---------------------------------------------------------------------------
// Input guards
// ---------------------------------------------------------------------------

func TestProveVolumeAbsent_RejectsMissingInputs(t *testing.T) {
	t.Parallel()
	probe := noFormatProbe(listingOf(t))
	cases := []struct {
		name    string
		client  pve.Client
		node    string
		storage string
		volume  string
	}{
		{"no client", nil, "pve-01", absenceStorage, absenceVolid},
		{"no node", probe.client(), "", absenceStorage, absenceVolid},
		{"no storage", probe.client(), "pve-01", "", absenceVolid},
		{"no volume", probe.client(), "pve-01", absenceStorage, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			absent, err := pve.ProveVolumeAbsent(
				context.Background(), tc.client, tc.node, tc.storage, tc.volume, nfsClassifier())
			if err == nil {
				t.Fatal("expected an error rather than an answer about the volume")
			}
			if absent {
				t.Fatal("a rejected call must never read as absent")
			}
		})
	}
}
