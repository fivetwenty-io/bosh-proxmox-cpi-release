// disk_identity_copied_internal_test.go — white-box tests for the resolver's
// refusal of a disk whose serial or transfer record more than one guest
// carries, and for the transfer windows where it must not refuse.
package pve

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

const (
	copiedBirth = "data:vm-9001-disk-0"
	copiedToken = "bpd-0011223344556677"
	parkerTags  = "bosh-cpi;bosh-parker"
)

var copiedCfg = ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999, FallbackNode: "pve1", ParkedEnabled: true}

// copiedRecord is a parker description that keeps one transfer record of the
// disk under copiedToken, from source VM 700.
func copiedRecord(volid, slot string) string {
	return `<!--BOSH:{"bosh_parked_disks":{"` + copiedToken + `":{"disk_cid":"pvd-x","source_vm_cid":"700",` +
		`"parked_at":"2026-08-20T00:00:00Z","node":"pve1","volid":"` + volid + `","slot":"` + slot + `"}}}-->`
}

// copiedClient serves configs in the order rows lists them.
func copiedClient(configs map[int]map[string]any, vmids ...int) *scanFakeClient {
	c := newScanFakeClient(configs)
	for _, vmid := range vmids {
		tags, _ := configs[vmid][cfgKeyTags].(string)
		c.rows = append(c.rows, clusterRow(vmid, tags))
	}
	return c
}

// readHookClient runs after, under no lock, once each pending read of a guest
// has been served, with the number of reads of that guest so far. A hook
// changes the cluster between two of the scan's reads, the way a move that
// runs while the scan reads does.
type readHookClient struct {
	*scanFakeClient

	mu    sync.Mutex
	reads map[int]int
	after func(c *scanFakeClient, vmid, read int)
}

// Storage answers from the configs the hook has written so far, so a volume a
// config names is on storage and any other is gone.
func (c *readHookClient) Storage() storage.Service {
	return volumeStorage(c.scanFakeClient, nil)
}

func (c *readHookClient) Nodes() sdknodes.Service {
	inner := c.scanFakeClient.Nodes().(*fakeNodesService)
	passThrough := inner.listQemuPendingFn
	inner.listQemuPendingFn = func(ctx context.Context, node, vmidText string) (*sdknodes.ListQemuPendingResponse, error) {
		resp, err := passThrough(ctx, node, vmidText)
		vmid, _ := strconv.Atoi(vmidText)
		c.mu.Lock()
		if c.reads == nil {
			c.reads = map[int]int{}
		}
		c.reads[vmid]++
		read := c.reads[vmid]
		c.mu.Unlock()
		c.scanFakeClient.mu.Lock()
		c.after(c.scanFakeClient, vmid, read)
		c.scanFakeClient.mu.Unlock()
		return resp, err
	}
	return inner
}

// requireCopiedRefusal fails unless err is the permanent refusal that names
// every guest in want and the way to tell the copy from the disk.
func requireCopiedRefusal(t *testing.T, err error, want ...string) {
	t.Helper()
	if _, ok := IsDiskIdentityCopied(err); !ok {
		t.Fatalf("err = %v, want a DiskIdentityCopiedError", err)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.Type() != cpierrors.TypeCloud || typed.OkToRetry() {
		t.Fatalf("err = %v, want a permanent CloudError", err)
	}
	for _, s := range append(want, copiedToken, "a qmclone task under the VM it copied", "bosh instances --details", DiskIdentityCopiedRunbook) {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("err = %v\nwant it to contain %q", err, s)
		}
	}
}

func TestResolveDiskIdentity_CopiedSerialRefuses(t *testing.T) {
	t.Parallel()

	t.Run("a cloned holder on a parker that sorts first", func(t *testing.T) {
		t.Parallel()
		// VM 700 holds the disk. Parker 90002 is a clone of a parker that
		// held it once, so it carries the serial on a volume of its own, and
		// the scan reads it first. Before the fix the resolver handed out the
		// clone's volume, and attach_disk moved it onto the new VM.
		c := copiedClient(map[int]map[string]any{
			90002: {"scsi0": "data:vm-90002-disk-0,serial=" + copiedToken, cfgKeyTags: parkerTags},
			700:   {"scsi1": "data:vm-700-disk-1,serial=" + copiedToken + ",size=10G"},
		}, 90002, 700)
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err,
			"slot scsi0 of parker VM 90002 on node pve1 with volume data:vm-90002-disk-0",
			"slot scsi1 of VM 700 on node pve1 with volume data:vm-700-disk-1")
		copied, _ := IsDiskIdentityCopied(err)
		if len(copied.Holders) != 2 || copied.Holders[0].VMID != 90002 || !copied.Holders[0].Parker || copied.Holders[1].VMID != 700 {
			t.Fatalf("holders = %+v", copied.Holders)
		}
	})

	t.Run("a cloned parker ahead of the parker that holds the disk", func(t *testing.T) {
		t.Parallel()
		c := copiedClient(map[int]map[string]any{
			90002: {"scsi3": "data:vm-90002-disk-2,serial=" + copiedToken, cfgKeyTags: parkerTags},
			90010: {"scsi3": "data:vm-90010-disk-2,serial=" + copiedToken, cfgKeyTags: parkerTags},
		}, 90002, 90010)
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err, "parker VM 90002 on node pve1", "parker VM 90010 on node pve1")
	})

	t.Run("a cloned holder on a VM that isn't a parker", func(t *testing.T) {
		t.Parallel()
		// A backup of VM 800 restored as VM 700 carries 800's drive line,
		// serial included, on 700's own volume, and the scan reads 700 first.
		c := copiedClient(map[int]map[string]any{
			700: {"scsi1": "data:vm-700-disk-0,serial=" + copiedToken},
			800: {"scsi1": "data:vm-800-disk-0,serial=" + copiedToken},
		}, 700, 800)
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err, "slot scsi1 of VM 700 on node pve1", "slot scsi1 of VM 800 on node pve1")
	})

	t.Run("a copy whose slot only the current view shows", func(t *testing.T) {
		t.Parallel()
		// The copy's slot has a pending delete, so only the running guest
		// still has it, and the scan counts it as it counts any holder.
		c := copiedClient(map[int]map[string]any{
			700: {},
			800: {"scsi1": "data:vm-800-disk-0,serial=" + copiedToken},
		}, 700, 800)
		c.held = map[int]map[string]any{700: {"scsi1": "data:vm-700-disk-0,serial=" + copiedToken}}
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err, "slot scsi1 of VM 700 on node pve1", "slot scsi1 of VM 800 on node pve1")
	})

	t.Run("the name-matching variant refuses too", func(t *testing.T) {
		t.Parallel()
		c := copiedClient(map[int]map[string]any{
			700: {"scsi1": "data:vm-700-disk-0,serial=" + copiedToken},
			800: {"scsi1": "data:vm-800-disk-0,serial=" + copiedToken},
		}, 700, 800)
		_, err := ResolveDiskIdentityMatchingName(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err, "VM 700", "VM 800")
	})

	t.Run("two parkers keep a record and no slot carries the serial", func(t *testing.T) {
		t.Parallel()
		// A clone of parker 90000 taken while the disk's transfer was
		// unfinished copies the record. Before the fix the resolver resumed
		// whichever record it read first.
		c := copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "description": copiedRecord("data:vm-700-disk-1", "scsi4")},
			90003: {cfgKeyTags: parkerTags, "description": copiedRecord("data:vm-700-disk-1", "scsi4")},
			700:   {"unused0": "data:vm-700-disk-1"},
		}, 90000, 90003, 700)
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err, "parker VM 90000 on node pve1", "parker VM 90003 on node pve1",
			"each keep a record of its transfer")
		copied, _ := IsDiskIdentityCopied(err)
		if len(copied.Records) != 2 || len(copied.Holders) != 0 {
			t.Fatalf("refusal = %+v", copied)
		}
	})
}

func TestResolveDiskIdentity_OneGuestIsOneHolder(t *testing.T) {
	t.Parallel()

	t.Run("both views of one slot carry the serial", func(t *testing.T) {
		t.Parallel()
		c := copiedClient(map[int]map[string]any{
			700: {"scsi1": "data:vm-700-disk-1,serial=" + copiedToken + ",cache=writeback"},
		}, 700)
		c.replaced = map[int]map[string]any{700: {"scsi1": "data:vm-700-disk-1,serial=" + copiedToken}}
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ident.Holder.Found || ident.Holder.VMID != 700 || ident.Volid != "data:vm-700-disk-1" {
			t.Fatalf("identity = %+v", ident)
		}
	})

	t.Run("a pending delete and an applied slot on one guest", func(t *testing.T) {
		t.Parallel()
		// The running guest still has scsi1, whose delete is pending, and its
		// applied view has the disk on scsi2. That is one guest, and the
		// copy refusal is for two.
		c := copiedClient(map[int]map[string]any{
			700: {"scsi2": "data:vm-700-disk-1,serial=" + copiedToken},
		}, 700)
		c.held = map[int]map[string]any{700: {"scsi1": "data:vm-700-disk-1,serial=" + copiedToken}}
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// The applied view wins, so no pending slot rides out.
		if ident.Holder.VMID != 700 || ident.Holder.PendingSlot != "" {
			t.Fatalf("identity = %+v", ident)
		}
	})

	t.Run("a guest the listing names twice", func(t *testing.T) {
		t.Parallel()
		c := copiedClient(map[int]map[string]any{
			700: {"scsi1": "data:vm-700-disk-1,serial=" + copiedToken},
		}, 700, 700)
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 700 {
			t.Fatalf("identity = %+v, err = %v", ident, err)
		}
	})

	t.Run("a parker the listing names twice keeps one record", func(t *testing.T) {
		t.Parallel()
		c := copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "description": copiedRecord("data:vm-700-disk-1", "scsi4")},
		}, 90000, 90000)
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Intent == nil || ident.Intent.ParkerVMID != 90000 {
			t.Fatalf("identity = %+v, err = %v", ident, err)
		}
	})
}

// TestResolveDiskIdentity_TransferWindowsDoNotRefuse walks the windows in
// which a parker and the disk's source or target both name the disk for a
// while. None of them leaves two guests carrying the serial, so none refuses.
func TestResolveDiskIdentity_TransferWindowsDoNotRefuse(t *testing.T) {
	t.Parallel()

	t.Run("detach, intent written and source slot not yet deleted", func(t *testing.T) {
		t.Parallel()
		c := copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "description": copiedRecord("data:vm-700-disk-1", "scsi4")},
			700:   {"scsi1": "data:vm-700-disk-1,serial=" + copiedToken},
		}, 90000, 700)
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 700 || ident.Intent != nil {
			t.Fatalf("identity = %+v, err = %v", ident, err)
		}
	})

	t.Run("detach, the move landed before the serial write", func(t *testing.T) {
		t.Parallel()
		c := copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "scsi4": "data:vm-90000-disk-0",
				"description": copiedRecord("data:vm-700-disk-1", "scsi4")},
			700: {},
		}, 90000, 700)
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Intent == nil || ident.Intent.ParkerVMID != 90000 {
			t.Fatalf("identity = %+v, err = %v", ident, err)
		}
	})

	t.Run("attach, landed on the VM before the parker's record is removed", func(t *testing.T) {
		t.Parallel()
		// The attach's move renamed the recorded volume to VM 700's name, so
		// storage no longer holds the name the record gives.
		c := &storageFakeClient{scanFakeClient: copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "description": copiedRecord("data:vm-90000-disk-0", "scsi4")},
			700:   {"scsi1": "data:vm-700-disk-2,serial=" + copiedToken},
		}, 90000, 700)}
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 700 {
			t.Fatalf("identity = %+v, err = %v", ident, err)
		}
	})

	t.Run("detach, the serial written on the parker before the record's finalize", func(t *testing.T) {
		t.Parallel()
		// The parker's slot carries the serial on the landed volume, and its
		// own record still names the volume the source held. That record is
		// the slot carrier's own, so the name it gives is only out of date,
		// even while another disk's move has landed on scsi5 without its
		// serial yet.
		c := copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "scsi4": "data:vm-90000-disk-0,serial=" + copiedToken,
				"scsi5": "data:vm-90000-disk-1", "description": copiedRecord("data:vm-700-disk-1", "scsi4")},
			700: {},
		}, 90000, 700)
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 90000 || ident.Volid != "data:vm-90000-disk-0" || ident.Intent != nil {
			t.Fatalf("identity = %+v, err = %v", ident, err)
		}
	})

	t.Run("a record left behind whose volume name another disk took since", func(t *testing.T) {
		t.Parallel()
		// The attach that moved the disk to VM 700 couldn't remove parker
		// 90000's record. PVE then gave the freed name to the next disk it
		// renamed onto the parker, and that slot carries the other disk's
		// serial, so the name in the record is no longer this disk's.
		// The storage answers as node-local, because a storage the CPI can't
		// classify makes the call retriable while another disk's key names the
		// volume.
		c := &storageFakeClient{scanFakeClient: copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "scsi4": "data:vm-90000-disk-0,serial=bpd-8899aabbccddeeff",
				"description": copiedRecord("data:vm-90000-disk-0", "scsi4")},
			700: {"scsi1": "data:vm-700-disk-2,serial=" + copiedToken},
		}, 90000, 700)}
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 700 || ident.Volid != "data:vm-700-disk-2" {
			t.Fatalf("identity = %+v, err = %v", ident, err)
		}
	})

	t.Run("a move between the scan's reads of the parker and the VM", func(t *testing.T) {
		t.Parallel()
		// The scan reads parker 90000 while it holds the disk, and the move
		// to VM 700 lands before the scan reads 700. The first pass sees
		// both, and the second reads the disk on 700 alone.
		fake := copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "scsi4": "data:vm-90000-disk-0,serial=" + copiedToken},
			700:   {},
		}, 90000, 700)
		c := &readHookClient{scanFakeClient: fake, after: func(s *scanFakeClient, vmid, read int) {
			if vmid == 90000 && read == 1 {
				delete(s.configs[90000], "scsi4")
				s.configs[700]["scsi1"] = "data:vm-700-disk-0,serial=" + copiedToken
			}
		}}
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 700 || ident.Volid != "data:vm-700-disk-0" {
			t.Fatalf("identity = %+v, err = %v", ident, err)
		}
	})

	t.Run("a mover hand-off caught between its two config writes", func(t *testing.T) {
		t.Parallel()
		// Mover 90001 has written its intent and PVE has written parker
		// 90000's config without the slot but not yet the mover's, so no slot
		// carries the serial and both keep a record. By the second pass the
		// mover's slot has it.
		fake := copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "description": copiedRecord("data:vm-90000-disk-0", "scsi1")},
			90001: {cfgKeyTags: parkerTags + ";bosh-disk-mover", "description": copiedRecord("data:vm-90000-disk-0", "scsi0")},
		}, 90000, 90001)
		c := &readHookClient{scanFakeClient: fake, after: func(s *scanFakeClient, vmid, read int) {
			if vmid == 90000 && read == 2 {
				s.configs[90001]["scsi0"] = "data:vm-90001-disk-0,serial=" + copiedToken
			}
		}}
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Intent != nil || ident.Holder.VMID != 90001 || !ident.Holder.IsParker {
			t.Fatalf("identity = %+v, err = %v", ident, err)
		}
	})

	t.Run("a mover hand-off after the move, before the parker's record is removed", func(t *testing.T) {
		t.Parallel()
		// The mover's move renamed the shared parker's recorded volume, so
		// storage no longer holds the name parker 90000's record gives.
		c := &storageFakeClient{scanFakeClient: copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "description": copiedRecord("data:vm-90000-disk-0", "scsi1")},
			90001: {cfgKeyTags: parkerTags + ";bosh-disk-mover", "scsi0": "data:vm-90001-disk-0,serial=" + copiedToken,
				"description": copiedRecord("data:vm-90001-disk-0", "scsi0")},
		}, 90000, 90001)}
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 90001 {
			t.Fatalf("identity = %+v, err = %v", ident, err)
		}
	})
}

// TestResolveDiskIdentity_CopyBesideARecordedTransfer covers a copy whose slot
// carries the serial while the disk itself sits in a transfer with no slot
// carrying it. One guest carries the serial, so only the parker's record shows
// that another volume is the disk. A record that names the slot carrier's own
// volume is a transfer that hasn't deleted the source slot yet, and it never
// refuses.
func TestResolveDiskIdentity_CopyBesideARecordedTransfer(t *testing.T) {
	t.Parallel()

	t.Run("the disk waits on the source's unused entry for its move", func(t *testing.T) {
		t.Parallel()
		// The detach from VM 700 deleted the slot, so the volume sits on
		// unused0, and parker 90000's record names it. VM 800 is a clone of
		// 700 taken before the detach, and its slot carries the serial on a
		// volume of its own. Before the fix the resolver took 800's slot.
		c := copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "description": copiedRecord("data:vm-700-disk-1", "scsi4")},
			700:   {"unused0": "data:vm-700-disk-1"},
			800:   {"scsi1": "data:vm-800-disk-0,serial=" + copiedToken},
		}, 90000, 700, 800)
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err,
			"slot scsi1 of VM 800 on node pve1 with volume data:vm-800-disk-0",
			"parker VM 90000 on node pve1 keeps a record of the disk's transfer that names volume data:vm-700-disk-1",
			"unused0 of VM 700 on node pve1 still names that volume",
			"removes the record from the parker")
		copied, _ := IsDiskIdentityCopied(err)
		if len(copied.Holders) != 1 || copied.Holders[0].VMID != 800 || len(copied.Records) != 1 || copied.Records[0].ParkerVMID != 90000 {
			t.Fatalf("refusal = %+v", copied)
		}
	})

	t.Run("the disk's move landed on the parker before its serial write", func(t *testing.T) {
		t.Parallel()
		// The move renamed the volume onto parker 90000's scsi4, so nothing
		// names the recorded volume any more, and the landing has no serial
		// yet. The clone on VM 800 is still the only slot with the serial.
		c := copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "scsi4": "data:vm-90000-disk-0",
				"description": copiedRecord("data:vm-700-disk-1", "scsi4")},
			700: {},
			800: {"scsi1": "data:vm-800-disk-0,serial=" + copiedToken},
		}, 800, 90000, 700)
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err,
			"slot scsi1 of VM 800 on node pve1",
			"names volume data:vm-700-disk-1",
			"the parker holds volume data:vm-90000-disk-0 on scsi4 with no serial")
	})

	t.Run("a record that names the volume the slot carries", func(t *testing.T) {
		t.Parallel()
		// The detach wrote its record but hasn't deleted VM 700's slot, so the
		// record and the slot name the same volume, and an unused entry
		// elsewhere that names it changes nothing.
		c := copiedClient(map[int]map[string]any{
			700:   {"scsi1": "data:vm-700-disk-1,serial=" + copiedToken},
			90000: {cfgKeyTags: parkerTags, "description": copiedRecord("data:vm-700-disk-1", "scsi4")},
			701:   {"unused3": "data:vm-700-disk-1"},
		}, 700, 90000, 701)
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 700 || ident.Volid != "data:vm-700-disk-1" || ident.Intent != nil {
			t.Fatalf("identity = %+v, err = %v", ident, err)
		}
	})
}

// TestResolveDiskIdentity_ReadFailureAfterTheHolderFailsClosed pins the rule
// that a lookup by serial reads every guest. A guest whose config read fails
// after the holder was found could be a copy, so the lookup fails with that
// read's error rather than answering with the holder.
func TestResolveDiskIdentity_ReadFailureAfterTheHolderFailsClosed(t *testing.T) {
	t.Parallel()
	c := copiedClient(map[int]map[string]any{
		700: {"scsi1": "data:vm-700-disk-1,serial=" + copiedToken},
		800: {},
	}, 700, 800)
	c.configErr = map[int]error{800: errors.New("500 Internal Server Error")}
	ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
	if err == nil {
		t.Fatalf("identity = %+v, want the read failure of VM 800", ident)
	}
	if _, copied := IsDiskIdentityCopied(err); copied {
		t.Fatalf("err = %v, want the read failure, not a copy refusal", err)
	}
	if ident.Holder.Found || ident.Volid != "" || !strings.Contains(err.Error(), "Config error for vm 800 on node pve1") {
		t.Fatalf("identity = %+v, err = %v; want no identity and the error that names vm 800 on node pve1", ident, err)
	}
}
