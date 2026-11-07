package pve

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// twoParkerTransferClient returns a scan fake with two empty parkers on pve1
// and the given source VMs.
func twoParkerTransferClient(sources map[int]map[string]any) *scanFakeClient {
	configs := map[int]map[string]any{
		90000: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true},
		90001: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true},
	}
	rows := make([]map[string]any, 0, 2+len(sources))
	rows = append(rows, clusterRow(90000, "bosh-cpi;bosh-parker"), clusterRow(90001, "bosh-cpi;bosh-parker"))
	for vmid, cfg := range sources {
		configs[vmid] = cfg
		rows = append(rows, clusterRow(vmid, ""))
	}
	c := newScanFakeClient(configs)
	c.rows = rows
	return c
}

// recordsOf decodes the provenance records of one parker.
func (c *scanFakeClient) recordsOf(t *testing.T, vmid int) map[string]parkerProvEntry {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	desc, _ := c.configs[vmid]["description"].(string)
	_, disks, _ := parseParkerSentinel(desc)
	return disks
}

// TestTransferDiskToParker_AdmitRefusalLandsInNextParker proves a transfer
// passes over a parker the selection rule refuses before it writes an intent
// record there, and moves the disk into the next parker.
func TestTransferDiskToParker_AdmitRefusalLandsInNextParker(t *testing.T) {
	t.Parallel()
	c := twoParkerTransferClient(map[int]map[string]any{
		700: {"scsi1": "data:vm-700-disk-1,serial=" + transferStableID + ",size=10G"},
	})
	cfg := transferTestCfg
	var asked []int
	cfg.Admit = func(_ context.Context, node string, vmid int, volid string) (bool, error) {
		asked = append(asked, vmid)
		if node != "pve1" || volid != "data:vm-700-disk-1" {
			t.Errorf("Admit got node %q volid %q", node, volid)
		}
		return vmid != 90000, nil
	}
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}

	landed, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", cfg, pctx)
	if err != nil {
		t.Fatalf("TransferDiskToParker: %v", err)
	}
	if !strings.HasPrefix(landed, "data:vm-90001-disk-") {
		t.Fatalf("landed = %q, want a volume of parker 90001", landed)
	}
	if len(c.recordsOf(t, 90000)) != 0 || c.eventIndex("description:90000") >= 0 {
		t.Fatalf("the refused parker must carry no record of the disk; events %v", c.events)
	}
	if _, ok := c.recordsOf(t, 90001)[transferStableID]; !ok {
		t.Fatalf("parker 90001 must carry the disk's record; records %+v", c.recordsOf(t, 90001))
	}
	if len(asked) < 2 || asked[0] != 90000 || asked[1] != 90001 {
		t.Fatalf("the rule must be asked about 90000, then 90001; asked %v", asked)
	}
}

// TestTransferDiskToParker_AdmitKeepsRegisteredReach models the AB then AC
// case on the transfer path: the disk on storage ab lands in the registered
// parker 90000, whose reach is pve1 and pve2, and the disk on storage ac,
// which pve2 cannot reach, lands in 90001.
func TestTransferDiskToParker_AdmitKeepsRegisteredReach(t *testing.T) {
	t.Parallel()
	const stableAC = "bpd-aabbccdd00112233"
	c := twoParkerTransferClient(map[int]map[string]any{
		700: {"scsi1": "ab:vm-700-disk-1,serial=" + transferStableID + ",size=10G"},
		701: {"scsi1": "ac:vm-701-disk-1,serial=" + stableAC + ",size=10G"},
	})
	storageNodes := map[string][]string{"ab": {"pve1", "pve2"}, "ac": {"pve1", "pve3"}}
	cfg := transferTestCfg
	cfg.Admit = func(_ context.Context, _ string, vmid int, volid string) (bool, error) {
		if vmid != 90000 {
			return true, nil
		}
		storage, _, _ := strings.Cut(volid, ":")
		reaches := strings.Join(storageNodes[storage], ",")
		return strings.Contains(reaches, "pve1") && strings.Contains(reaches, "pve2"), nil
	}

	ctx := context.Background()
	pctxAB := ParkContext{DiskCID: "pvd-ab", SourceVMCID: "700", StableID: transferStableID}
	landedAB, err := TransferDiskToParker(ctx, c, nil, "pve1", 700, "ab:vm-700-disk-1", cfg, pctxAB)
	if err != nil {
		t.Fatalf("transfer ab: %v", err)
	}
	pctxAC := ParkContext{DiskCID: "pvd-ac", SourceVMCID: "701", StableID: stableAC}
	landedAC, err := TransferDiskToParker(ctx, c, nil, "pve1", 701, "ac:vm-701-disk-1", cfg, pctxAC)
	if err != nil {
		t.Fatalf("transfer ac: %v", err)
	}
	if !strings.HasPrefix(landedAB, "ab:vm-90000-disk-") {
		t.Errorf("the ab disk landed as %q, want it in the registered parker 90000", landedAB)
	}
	if !strings.HasPrefix(landedAC, "ac:vm-90001-disk-") {
		t.Errorf("the ac disk landed as %q, want it in parker 90001", landedAC)
	}
	if _, ok := c.recordsOf(t, 90000)[stableAC]; ok {
		t.Error("the registered parker must carry no record of the ac disk")
	}
}

// TestResumeDiskTransferToParker_AdmitRefusalDefers proves a resumed
// transfer whose recorded parker the selection rule now refuses moves
// nothing and defers with a retriable error, so the disk stays on its source.
func TestResumeDiskTransferToParker_AdmitRefusalDefers(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		700:   {"unused0": "data:vm-700-disk-1"},
		90000: {cfgKeyTags: "bosh-cpi;bosh-parker"},
	})
	cfg := transferTestCfg
	cfg.Admit = func(context.Context, string, int, string) (bool, error) { return false, nil }
	intent := DiskTransferIntent{
		ParkerVMID: 90000, ParkerNode: "pve1", Slot: "scsi4", Volid: "data:vm-700-disk-1", SourceVMCID: "700",
	}

	_, err := ResumeDiskTransferToParker(context.Background(), c, nil, intent, transferStableID, cfg, ParkContext{})
	if err == nil {
		t.Fatal("the resume must defer when the parker may not take the disk")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("the deferral must be retriable; got %v", err)
	}
	if !errors.Is(err, ErrParkerNotAdmitted) {
		t.Fatalf("the deferral must carry the refusal; got %v", err)
	}
	if got, _ := c.configs[700]["unused0"].(string); got != "data:vm-700-disk-1" {
		t.Fatalf("the source must keep the disk; source config %v", c.configs[700])
	}
	if _, ok := c.configs[90000]["scsi4"]; ok || c.eventIndex("move:") >= 0 {
		t.Fatalf("nothing may move into the refused parker; events %v", c.events)
	}
}

// TestResumeDiskTransferToParker_AdmitErrorKeepsItsType proves a resume whose
// selection rule cannot answer fails with the rule's error and moves nothing.
func TestResumeDiskTransferToParker_AdmitErrorKeepsItsType(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		700:   {"unused0": "data:vm-700-disk-1"},
		90000: {cfgKeyTags: "bosh-cpi;bosh-parker"},
	})
	cfg := transferTestCfg
	cfg.Admit = func(context.Context, string, int, string) (bool, error) {
		return false, cpierrors.Retriable("the HA rules read failed")
	}
	intent := DiskTransferIntent{
		ParkerVMID: 90000, ParkerNode: "pve1", Slot: "scsi4", Volid: "data:vm-700-disk-1", SourceVMCID: "700",
	}

	_, err := ResumeDiskTransferToParker(context.Background(), c, nil, intent, transferStableID, cfg, ParkContext{})
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || errors.Is(err, ErrParkerNotAdmitted) {
		t.Fatalf("the resume must fail with the rule's retriable error; got %v", err)
	}
	if c.eventIndex("move:") >= 0 {
		t.Fatalf("nothing may move; events %v", c.events)
	}
}

// TestAdmitIntoParker covers the three answers of the rule wrapper.
func TestAdmitIntoParker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if err := admitIntoParker(ctx, ParkerConfig{}, "pve1", 90000, "nfs:vm-1-disk-0"); err != nil {
		t.Fatalf("a nil rule must admit; got %v", err)
	}
	refuse := ParkerConfig{Admit: func(context.Context, string, int, string) (bool, error) { return false, nil }}
	err := admitIntoParker(ctx, refuse, "pve1", 90000, "nfs:vm-1-disk-0")
	if !errors.Is(err, ErrParkerNotAdmitted) || !isParkerSkipError(err, ParkerConfig{}) {
		t.Fatalf("a refusal must be a skip error; got %v", err)
	}
	fail := ParkerConfig{Admit: func(context.Context, string, int, string) (bool, error) {
		return true, errors.New("boom")
	}}
	err = admitIntoParker(ctx, fail, "pve1", 90000, "nfs:vm-1-disk-0")
	if err == nil || isParkerSkipError(err, ParkerConfig{}) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("an unanswered rule must fail and never skip; got %v", err)
	}
}

// vanishThenMoveClient serves a disk on parker 90000 to the first holder
// scan, and answers the scan's follow-up config read of the parker with a
// missing config, after it moved the disk onto VM 700 and removed the parker,
// the way a retirement of the parker right after the disk left it looks.
type vanishThenMoveClient struct {
	*scanFakeClient
	volid     string
	mu        sync.Mutex
	vanished  bool
	followUps int
}

func (c *vanishThenMoveClient) QEMU() qemu.Service {
	svc := c.scanFakeClient.QEMU()
	inner, ok := svc.(*fakeQEMUService)
	if !ok {
		panic("scanFakeClient.QEMU is not a *fakeQEMUService")
	}
	passThrough := inner.configFn
	inner.configFn = func(ctx context.Context, node string, vmid int) (map[string]any, error) {
		if vmid == 90000 {
			c.mu.Lock()
			c.followUps++
			first := !c.vanished
			c.vanished = true
			c.mu.Unlock()
			if first {
				c.scanFakeClient.mu.Lock()
				delete(c.configs, 90000)
				c.rows = []map[string]any{clusterRow(700, "")}
				c.configs[700]["scsi1"] = c.volid
				c.scanFakeClient.mu.Unlock()
			}
			return nil, errors.New("Configuration file 'nodes/pve1/qemu-server/90000.conf' does not exist")
		}
		return passThrough(ctx, node, vmid)
	}
	return svc
}

// TestResolveDiskHolder_RescanFindsTheNewHolder proves a holder scan whose
// in-band holder vanished scans once more and answers the disk's new holder,
// even under the strict anchor invariant.
func TestResolveDiskHolder_RescanFindsTheNewHolder(t *testing.T) {
	t.Parallel()
	const volid = "data:vm-9001-disk-0"
	inner := newScanFakeClient(map[int]map[string]any{
		90000: {cfgKeyTags: "bosh-cpi;bosh-parker", "scsi0": volid},
		700:   {},
	})
	inner.rows = []map[string]any{clusterRow(90000, "bosh-cpi;bosh-parker"), clusterRow(700, "")}
	c := &vanishThenMoveClient{scanFakeClient: inner, volid: volid}
	cfg := transferTestCfg
	cfg.AnchorStrict = true
	cfg.RetiresParkers = true

	holder, err := resolveDiskHolder(context.Background(), c, nil, volid, cfg)
	if err != nil {
		t.Fatalf("resolveDiskHolder: %v", err)
	}
	if !holder.found || holder.vmid != 700 || holder.isParker {
		t.Fatalf("the rescan must find the disk on VM 700; got %+v", holder)
	}
	if c.followUps != 1 {
		t.Fatalf("the parker's config must be read once, by the first scan's follow-up; read %d times", c.followUps)
	}
}

// TestResolveDiskHolder_NoRescanWhenParkersAreNeverRetired proves that, with
// parker retirement off, a holder scan whose in-band holder vanished answers
// as it always has, without a second scan.
func TestResolveDiskHolder_NoRescanWhenParkersAreNeverRetired(t *testing.T) {
	t.Parallel()
	const volid = "data:vm-9001-disk-0"
	inner := newScanFakeClient(map[int]map[string]any{
		90000: {cfgKeyTags: "bosh-cpi;bosh-parker", "scsi0": volid},
		700:   {},
	})
	inner.rows = []map[string]any{clusterRow(90000, "bosh-cpi;bosh-parker"), clusterRow(700, "")}
	c := &vanishThenMoveClient{scanFakeClient: inner, volid: volid}
	cfg := transferTestCfg
	cfg.AnchorStrict = true

	holder, err := resolveDiskHolder(context.Background(), c, nil, volid, cfg)
	if err == nil && holder.found && holder.vmid == 700 {
		t.Fatalf("without parker retirement the scan must not rescan; got holder %+v", holder)
	}
	if c.followUps != 1 {
		t.Fatalf("the parker's config must be read once; read %d times", c.followUps)
	}
}

// goneAfterListClient is the two-parker fake whose parker 90000 answers its
// first config read, the listing's, and then reads as missing, the way a
// parker a retirement destroyed between the listing and the transfer's first
// read looks.
type goneAfterListClient struct {
	*scanFakeClient
	mu    sync.Mutex
	reads int
}

func (c *goneAfterListClient) QEMU() qemu.Service {
	svc := c.scanFakeClient.QEMU()
	inner, ok := svc.(*fakeQEMUService)
	if !ok {
		panic("scanFakeClient.QEMU is not a *fakeQEMUService")
	}
	passThrough := inner.configFn
	inner.configFn = func(ctx context.Context, node string, vmid int) (map[string]any, error) {
		if vmid == 90000 {
			c.mu.Lock()
			c.reads++
			first := c.reads == 1
			c.mu.Unlock()
			if !first {
				return nil, errors.New("Configuration file 'nodes/pve1/qemu-server/90000.conf' does not exist")
			}
		}
		return passThrough(ctx, node, vmid)
	}
	return svc
}

func goneParkerTransferClient() *goneAfterListClient {
	return &goneAfterListClient{scanFakeClient: twoParkerTransferClient(map[int]map[string]any{
		700: {"scsi1": "data:vm-700-disk-1,serial=" + transferStableID + ",size=10G"},
	})}
}

// TestTransferDiskToParker_AGoneParkerIsSkipped proves a transfer passes over
// a listed parker whose config is gone when parkers may be retired, and moves
// the disk into the next parker.
func TestTransferDiskToParker_AGoneParkerIsSkipped(t *testing.T) {
	t.Parallel()
	c := goneParkerTransferClient()
	cfg := transferTestCfg
	cfg.RetiresParkers = true
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}

	landed, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", cfg, pctx)
	if err != nil {
		t.Fatalf("TransferDiskToParker: %v", err)
	}
	if !strings.HasPrefix(landed, "data:vm-90001-disk-") {
		t.Fatalf("landed = %q, want a volume of parker 90001", landed)
	}
}

// TestTransferDiskToParker_AGoneParkerFailsWithoutRetirement proves that,
// with parker retirement off, a listed parker whose config is gone fails the
// transfer as it always has.
func TestTransferDiskToParker_AGoneParkerFailsWithoutRetirement(t *testing.T) {
	t.Parallel()
	c := goneParkerTransferClient()
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}

	landed, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1",
		transferTestCfg, pctx)
	if err == nil {
		t.Fatalf("want the gone parker to fail the transfer; landed %q", landed)
	}
	if !errors.Is(err, ErrParkerGone) || isParkerSkipError(err, transferTestCfg) {
		t.Fatalf("without retirement a gone parker must fail the transfer, not be skipped: %v", err)
	}
	if c.reads < 2 {
		t.Fatalf("the transfer must have read the listed parker; reads %d", c.reads)
	}
}

// TestParkerGoneMark proves only a missing config marks a parker gone, and
// that the mark keeps the error's text and type.
func TestParkerGoneMark(t *testing.T) {
	t.Parallel()
	missing := errors.New("Configuration file 'nodes/pve1/qemu-server/90000.conf' does not exist")
	wrapped := cpierrors.Wrap(missing, "read parker")
	marked := markParkerGone(wrapped, missing)
	if !errors.Is(marked, ErrParkerGone) || marked.Error() != wrapped.Error() {
		t.Fatalf("a missing config must mark the parker gone and keep the text; got %v", marked)
	}
	if !isParkerSkipError(marked, ParkerConfig{RetiresParkers: true}) || isParkerSkipError(marked, ParkerConfig{}) {
		t.Fatal("a gone parker is skipped only when parkers may be retired")
	}
	other := errors.New("500 pmxcfs timeout")
	if errors.Is(markParkerGone(other, other), ErrParkerGone) {
		t.Fatal("a server error must not mark the parker gone")
	}
	if markParkerGone(nil, missing) != nil {
		t.Fatal("a nil error stays nil")
	}
}
