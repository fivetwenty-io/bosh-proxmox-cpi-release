package pve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

const guardTestNamespace = "cf-prod"

// guardAllocVolume returns a full managed allocation volume name for VM vmid
// under namespace's locator.
func guardAllocVolume(vmid, namespace string) string {
	return "vm-" + vmid + "-bosh-" + AllocationNamespaceLocator(namespace) +
		"-alloc-0b0c8a52-6a1f-4c3e-9b1d-2f4e6a8c0d1e.raw"
}

func TestParkerBandConstantsMatchConfigDefaults(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{}
	if got := cfg.ParkedDiskVMIDRangeStartValue(); got != VMIDRangeParkerStart {
		t.Errorf("config parker start = %d, pve constant = %d", got, VMIDRangeParkerStart)
	}
	if got := cfg.ParkedDiskVMIDRangeEndValue(); got != VMIDRangeParkerEnd {
		t.Errorf("config parker end = %d, pve constant = %d", got, VMIDRangeParkerEnd)
	}
}

func TestVMIDBandsFromConfig(t *testing.T) {
	t.Parallel()
	defaults := DefaultVMIDBands()
	tests := []struct {
		name string
		cfg  *config.CPIConfig
		want VMIDBands
	}{
		{name: "nil config", cfg: nil, want: defaults},
		{name: "unset config", cfg: &config.CPIConfig{}, want: defaults},
		{
			name: "every band set",
			cfg: &config.CPIConfig{
				VMIDRangeStart: 1000, VMIDRangeEnd: 1999,
				DiskVMIDRangeStart: 2000, DiskVMIDRangeEnd: 2999,
				StemcellTemplateVMIDRangeStart: 3000, StemcellTemplateVMIDRangeEnd: 3099,
				ParkedDiskVMIDRangeStart: 4000, ParkedDiskVMIDRangeEnd: 4999,
			},
			want: VMIDBands{
				VM: VMIDBand{1000, 1999}, Disk: VMIDBand{2000, 2999},
				Template: VMIDBand{3000, 3099}, Parker: VMIDBand{4000, 4999},
			},
		},
		{
			name: "pairs WithRange ignores take the built-in band",
			cfg: &config.CPIConfig{
				DiskVMIDRangeStart: 12000, DiskVMIDRangeEnd: 0,
				StemcellTemplateVMIDRangeStart: 31000, StemcellTemplateVMIDRangeEnd: 30500,
			},
			want: defaults,
		},
		{
			name: "a VM start below 100 covers both readings",
			cfg:  &config.CPIConfig{VMIDRangeStart: 50, VMIDRangeEnd: 500},
			want: VMIDBands{VM: VMIDBand{100, 8999}, Disk: defaults.Disk, Template: defaults.Template, Parker: defaults.Parker},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := VMIDBandsFromConfig(tc.cfg); got != tc.want {
				t.Errorf("VMIDBandsFromConfig = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestVMIDBandHelpers(t *testing.T) {
	t.Parallel()
	b := VMIDBand{100, 200}
	if !b.Contains(100) || !b.Contains(200) || b.Contains(99) || b.Contains(201) {
		t.Error("Contains is not inclusive at both ends")
	}
	if !b.Overlaps(VMIDBand{200, 300}) || b.Overlaps(VMIDBand{201, 300}) || b.Overlaps(VMIDBand{300, 200}) {
		t.Error("Overlaps is wrong at the boundary or for an empty band")
	}
	if !DefaultVMIDBands().AllDefault() {
		t.Error("the default bands are not AllDefault")
	}
	changed := DefaultVMIDBands()
	changed.Parker = VMIDBand{80000, 80999}
	if changed.AllDefault() {
		t.Error("a moved parker band still reads as AllDefault")
	}
	if got := DefaultVMIDBands().String(); got != "vm 100-8999, disk 9000-29999, stemcell template 30000-30999, parker 90000-90999" {
		t.Errorf("String = %q", got)
	}
}

func TestForeignVolumeError(t *testing.T) {
	t.Parallel()
	bands := DefaultVMIDBands()
	tests := []struct {
		name    string
		volume  string
		foreign bool
	}{
		{name: "owner in the vm band", volume: "images/150/vm-150-disk-0.qcow2"},
		{name: "owner in the disk band", volume: "vm-9000-disk-0"},
		{name: "owner in the template band", volume: "base-30000-disk-0.qcow2"},
		{name: "owner in the parker band", volume: "images/90000/vm-90000-disk-1.raw"},
		{name: "owner outside every band", volume: "images/50000/vm-50000-disk-0.qcow2", foreign: true},
		{name: "owner too large to parse", volume: "vm-99999999999999999999-disk-0", foreign: true},
		{name: "our locator outside the bands", volume: guardAllocVolume("50000", guardTestNamespace)},
		{name: "another locator inside the bands", volume: guardAllocVolume("150", "other-foundation"), foreign: true},
		{name: "another locator with a malformed uuid", volume: "vm-150-bosh-" + AllocationNamespaceLocator("x") + "-alloc-nope.raw", foreign: true},
		{name: "no owner", volume: "iso/bosh-stemcell-import.img"},
		{name: "config-drive ISO in the vm band", volume: "iso/vm-150-config.iso"},
		{name: "config-drive ISO outside the bands", volume: "iso/vm-50000-config.iso", foreign: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ForeignVolumeError("nfs-shared", tc.volume, bands, guardTestNamespace)
			if (err != nil) != tc.foreign {
				t.Fatalf("ForeignVolumeError(%q) = %v, want foreign=%v", tc.volume, err, tc.foreign)
			}
			if err != nil {
				if !cpierrors.IsType(err, cpierrors.TypeCloud) {
					t.Errorf("refusal is not a permanent CloudError: %v", err)
				}
				if !strings.Contains(err.Error(), tc.volume) {
					t.Errorf("refusal %q does not name the volume", err)
				}
			}
		})
	}

	t.Run("refusal names the owner and the bands", func(t *testing.T) {
		t.Parallel()
		err := ForeignVolumeError("nfs-shared", "vm-50000-disk-0", bands, guardTestNamespace)
		for _, want := range []string{"VM 50000", bands.String(), "widen a band"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %v does not contain %q", err, want)
			}
		}
	})

	t.Run("an empty namespace owns no locator", func(t *testing.T) {
		t.Parallel()
		if ForeignVolumeError("s", guardAllocVolume("150", ""), bands, "") == nil {
			t.Error("a locator volume passed with no storage placement namespace configured")
		}
	})
}

// recordingStorage records every delete it is sent.
type recordingStorage struct {
	fakeStorageService
	deletes []string
}

func (r *recordingStorage) DeleteVolume(_ context.Context, _, _, volume string) error {
	r.deletes = append(r.deletes, "sync:"+volume)
	return nil
}

func (r *recordingStorage) DeleteVolumeIfExists(_ context.Context, _, _, volume string) (bool, error) {
	r.deletes = append(r.deletes, "ifexists:"+volume)
	return true, nil
}

func (r *recordingStorage) DeleteVolumeAsync(_ context.Context, _, _, volume string) (string, error) {
	r.deletes = append(r.deletes, "async:"+volume)
	return "UPID:x", nil
}

func (r *recordingStorage) DeleteVolumeIfExistsAsync(_ context.Context, _, _, volume string) (bool, string, error) {
	r.deletes = append(r.deletes, "ifexistsasync:"+volume)
	return true, "UPID:y", nil
}

func (r *recordingStorage) Upload(context.Context, string, string, string, string, io.Reader) (string, error) {
	return "uploaded", nil
}

func newGuardStorage(t *testing.T, lister *fakeLister) (*sharedVolumeGuardStorage, *recordingStorage) {
	t.Helper()
	inner := &recordingStorage{}
	cfg := &config.CPIConfig{StoragePlacementNamespace: guardTestNamespace}
	return newSharedVolumeGuardStorage(inner, lister, cfg), inner
}

func guardStorageIndex() *fakeLister {
	return &fakeLister{entries: []map[string]any{
		{"storage": "nfs-shared", "type": "nfs", "shared": 1, "server": "10.0.0.5", "export": "/exports/pve", "content": "images,iso"},
		{"storage": "local-lvm", "type": "lvmthin", "content": "images"},
	}}
}

// deleteVariants drives every delete method of the guard once.
func deleteVariants() map[string]func(s *sharedVolumeGuardStorage, ctx context.Context, storageName, volume string) error {
	return map[string]func(s *sharedVolumeGuardStorage, ctx context.Context, storageName, volume string) error{
		"sync": func(s *sharedVolumeGuardStorage, ctx context.Context, storageName, volume string) error {
			return s.DeleteVolume(ctx, "pve1", storageName, volume)
		},
		"ifexists": func(s *sharedVolumeGuardStorage, ctx context.Context, storageName, volume string) error {
			_, err := s.DeleteVolumeIfExists(ctx, "pve1", storageName, volume)
			return err
		},
		"async": func(s *sharedVolumeGuardStorage, ctx context.Context, storageName, volume string) error {
			_, err := s.DeleteVolumeAsync(ctx, "pve1", storageName, volume)
			return err
		},
		"ifexistsasync": func(s *sharedVolumeGuardStorage, ctx context.Context, storageName, volume string) error {
			_, _, err := s.DeleteVolumeIfExistsAsync(ctx, "pve1", storageName, volume)
			return err
		},
	}
}

func TestSharedVolumeGuardStorage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		storage   string
		volume    string
		lister    *fakeLister
		refused   bool
		wantReads int
	}{
		{name: "in-band owner on shared storage reads nothing", storage: "nfs-shared", volume: "nfs-shared:vm-150-disk-0", lister: guardStorageIndex()},
		{name: "foreign owner on shared storage is refused", storage: "nfs-shared", volume: "images/50000/vm-50000-disk-0.qcow2", lister: guardStorageIndex(), refused: true, wantReads: 1},
		{name: "foreign owner on local storage passes", storage: "local-lvm", volume: "vm-50000-disk-0", lister: guardStorageIndex(), wantReads: 1},
		{name: "our locator outside the bands passes", storage: "nfs-shared", volume: guardAllocVolume("50000", guardTestNamespace), lister: guardStorageIndex()},
		{name: "another locator in the bands is refused", storage: "nfs-shared", volume: guardAllocVolume("150", "other"), lister: guardStorageIndex(), refused: true, wantReads: 1},
		{name: "no owner passes", storage: "nfs-shared", volume: "iso/stemcell-import.img", lister: guardStorageIndex()},
		{name: "foreign config-drive ISO is refused", storage: "nfs-shared", volume: "iso/vm-50000-config.iso", lister: guardStorageIndex(), refused: true, wantReads: 1},
		{name: "storage missing from the index counts as shared", storage: "unknown", volume: "vm-50000-disk-0", lister: guardStorageIndex(), refused: true, wantReads: 1},
		{name: "unreadable index counts as shared", storage: "local-lvm", volume: "vm-50000-disk-0", lister: &fakeLister{err: errors.New("boom")}, refused: true, wantReads: 1},
	}
	for _, tc := range tests {
		for variant, call := range deleteVariants() {
			t.Run(tc.name+"/"+variant, func(t *testing.T) {
				t.Parallel()
				lister := &fakeLister{entries: tc.lister.entries, err: tc.lister.err}
				guard, inner := newGuardStorage(t, lister)
				err := call(guard, context.Background(), tc.storage, tc.volume)
				if tc.refused {
					if err == nil || !cpierrors.IsType(err, cpierrors.TypeCloud) {
						t.Fatalf("delete of %q returned %v, want a permanent refusal", tc.volume, err)
					}
					if len(inner.deletes) != 0 {
						t.Fatalf("refused delete still reached PVE: %v", inner.deletes)
					}
				} else {
					if err != nil {
						t.Fatalf("delete of %q returned %v, want it sent", tc.volume, err)
					}
					if len(inner.deletes) != 1 || inner.deletes[0] != variant+":"+tc.volume {
						t.Fatalf("deletes sent = %v, want one %s delete of %q", inner.deletes, variant, tc.volume)
					}
				}
				if lister.calls != tc.wantReads {
					t.Errorf("/storage reads = %d, want %d", lister.calls, tc.wantReads)
				}
			})
		}
	}
}

func TestSharedVolumeGuardStoragePassesOtherCalls(t *testing.T) {
	t.Parallel()
	guard, _ := newGuardStorage(t, guardStorageIndex())
	if got, err := guard.Upload(context.Background(), "pve1", "nfs-shared", "iso", "vm-50000-config.iso", nil); err != nil || got != "uploaded" {
		t.Errorf("Upload = %q, %v; want it passed through", got, err)
	}
}

func TestSharedVolumeGuardStorageWithoutLister(t *testing.T) {
	t.Parallel()
	inner := &recordingStorage{}
	guard := newSharedVolumeGuardStorage(inner, nil, nil)
	if err := guard.DeleteVolume(context.Background(), "pve1", "local", "vm-50000-disk-0"); err == nil {
		t.Fatal("a guard with no storage index let a foreign-owner delete through")
	}
	if err := guard.DeleteVolume(context.Background(), "pve1", "local", "vm-150-disk-0"); err != nil {
		t.Fatalf("in-band delete refused: %v", err)
	}
}

// nodeStorageListing builds a node storage listing response from rows.
func nodeStorageListing(t *testing.T, rows ...map[string]any) *nodes.ListStorageResponse {
	t.Helper()
	resp := make(nodes.ListStorageResponse, 0, len(rows))
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("marshal row: %v", err)
		}
		resp = append(resp, raw)
	}
	return &resp
}

type destroyRecorder struct {
	sent         []*nodes.DeleteQemuParams
	listCalls    int
	listParams   *nodes.ListStorageParams
	listResponse func() (*nodes.ListStorageResponse, error)
}

func (d *destroyRecorder) service() *fakeNodesService {
	return &fakeNodesService{
		deleteQemuFn: func(_ context.Context, _, _ string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			d.sent = append(d.sent, params)
			resp := nodes.DeleteQemuResponse(`"UPID:destroy"`)
			return &resp, nil
		},
		listStorageFn: func(_ context.Context, _ string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			d.listCalls++
			d.listParams = params
			return d.listResponse()
		},
	}
}

func boolPtrGuard(v bool) *bool { return &v }

func TestSharedVolumeGuardNodesDeleteQemu(t *testing.T) {
	sharedRow := map[string]any{"storage": "nfs-shared", "type": "nfs", "content": "images,iso", "shared": 1, "enabled": 1, "active": 1}
	inactiveSharedRow := map[string]any{"storage": "nfs-down", "type": "nfs", "content": "images", "shared": 1, "enabled": 1, "active": 0}
	localRow := map[string]any{"storage": "local-lvm", "type": "lvmthin", "content": "images", "enabled": 1, "active": 1}
	isoOnlyShared := map[string]any{"storage": "nfs-iso", "type": "nfs", "content": "iso", "shared": 1, "enabled": 1}
	disabledShared := map[string]any{"storage": "nfs-off", "type": "nfs", "content": "images", "shared": 1, "enabled": 0}
	sharedDir := map[string]any{"storage": "dir-shared", "type": "dir", "content": "images", "shared": 1, "enabled": 1}

	tests := []struct {
		name      string
		listing   func(t *testing.T) (*nodes.ListStorageResponse, error)
		wantSweep bool
	}{
		{name: "shared images storage turns the sweep off", listing: func(t *testing.T) (*nodes.ListStorageResponse, error) {
			return nodeStorageListing(t, localRow, sharedRow), nil
		}},
		{name: "an inactive shared storage still turns it off", listing: func(t *testing.T) (*nodes.ListStorageResponse, error) {
			return nodeStorageListing(t, localRow, inactiveSharedRow), nil
		}},
		{name: "a shared dir turns it off", listing: func(t *testing.T) (*nodes.ListStorageResponse, error) {
			return nodeStorageListing(t, sharedDir), nil
		}},
		{name: "local storage only keeps the sweep", wantSweep: true, listing: func(t *testing.T) (*nodes.ListStorageResponse, error) {
			return nodeStorageListing(t, localRow, isoOnlyShared, disabledShared), nil
		}},
		{name: "a failed listing turns it off", listing: func(*testing.T) (*nodes.ListStorageResponse, error) {
			return nil, errors.New("connection reset")
		}},
		{name: "a nil listing turns it off", listing: func(*testing.T) (*nodes.ListStorageResponse, error) {
			return nil, nil
		}},
		{name: "an undecodable row turns it off", listing: func(*testing.T) (*nodes.ListStorageResponse, error) {
			resp := nodes.ListStorageResponse{json.RawMessage(`["not","an","object"]`)}
			return &resp, nil
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer ResetSweepSuppressedWarnOnceForTest()()
			rec := &destroyRecorder{listResponse: func() (*nodes.ListStorageResponse, error) { return tc.listing(t) }}
			guard := &sharedVolumeGuardNodes{Service: rec.service()}
			caller := &nodes.DeleteQemuParams{DestroyUnreferencedDisks: boolPtrGuard(true), Purge: boolPtrGuard(true)}
			if _, err := guard.DeleteQemu(context.Background(), "pve1", "150", caller); err != nil {
				t.Fatalf("DeleteQemu: %v", err)
			}
			if rec.listCalls != 1 {
				t.Fatalf("storage listings = %d, want exactly one", rec.listCalls)
			}
			if rec.listParams == nil || rec.listParams.Content == nil || *rec.listParams.Content != "images" ||
				rec.listParams.Enabled == nil || !*rec.listParams.Enabled {
				t.Errorf("listing params = %+v, want content=images enabled=1", rec.listParams)
			}
			if len(rec.sent) != 1 {
				t.Fatalf("destroys sent = %d, want 1", len(rec.sent))
			}
			sent := rec.sent[0]
			if got := sent.DestroyUnreferencedDisks != nil && *sent.DestroyUnreferencedDisks; got != tc.wantSweep {
				t.Errorf("destroy-unreferenced-disks sent = %v, want %v", got, tc.wantSweep)
			}
			if sent.Purge == nil || !*sent.Purge {
				t.Error("purge was lost from the guarded destroy")
			}
			if !*caller.DestroyUnreferencedDisks {
				t.Error("the caller's params were modified")
			}
		})
	}
}

func TestSharedVolumeGuardNodesDeleteQemuWithoutSweep(t *testing.T) {
	t.Parallel()
	for name, params := range map[string]*nodes.DeleteQemuParams{
		"nil params":  nil,
		"flag absent": {Purge: boolPtrGuard(true)},
		"flag false":  {DestroyUnreferencedDisks: boolPtrGuard(false)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := &destroyRecorder{listResponse: func() (*nodes.ListStorageResponse, error) {
				t.Error("a destroy without the sweep listed storage")
				return nil, nil
			}}
			guard := &sharedVolumeGuardNodes{Service: rec.service()}
			if _, err := guard.DeleteQemu(context.Background(), "pve1", "150", params); err != nil {
				t.Fatalf("DeleteQemu: %v", err)
			}
			if len(rec.sent) != 1 || rec.sent[0] != params {
				t.Errorf("destroy was not passed through unchanged: %v", rec.sent)
			}
		})
	}
}

func TestSharedVolumeGuardNodesWarnsOnce(t *testing.T) {
	defer ResetSweepSuppressedWarnOnceForTest()()
	ctx, buf := testLoggerCtx(t)
	rec := &destroyRecorder{listResponse: func() (*nodes.ListStorageResponse, error) { return nil, errors.New("down") }}
	guard := &sharedVolumeGuardNodes{Service: rec.service()}
	for range 3 {
		params := &nodes.DeleteQemuParams{DestroyUnreferencedDisks: boolPtrGuard(true)}
		if _, err := guard.DeleteQemu(ctx, "pve1", "150", params); err != nil {
			t.Fatalf("DeleteQemu: %v", err)
		}
	}
	out := buf.String()
	if n := strings.Count(out, "pve.destroy_unreferenced_disks"); n != 1 {
		t.Errorf("warning naming pve.destroy_unreferenced_disks logged %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, `"level":"WARN"`) && !strings.Contains(out, "level=WARN") {
		t.Errorf("the first suppressed sweep was not logged as a warning:\n%s", out)
	}
}
