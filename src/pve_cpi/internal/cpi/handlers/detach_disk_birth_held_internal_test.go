// detach_disk_birth_held_internal_test.go covers the notes cleanup after a
// stable-ID disk's detach. A disk keeps its create_disk volume name in its CID
// as a birth record, and once a move renames the volume, a legacy disk created
// later can carry that same name and key its notes under it. The cleanup must
// leave a name the source VM still holds to the disk that holds it.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const (
	birthHeldVMID     = 700
	birthHeldParker   = 90000
	birthHeldBirth    = "data:vm-9001-disk-0"
	birthHeldAttached = "data:vm-700-disk-1"
)

// birthHeldDescription builds a description whose attached-disk notes and
// drive-option overlays carry one entry per key in keys.
func birthHeldDescription(t *testing.T, keys ...string) string {
	t.Helper()
	notes := map[string]string{}
	overlays := map[string]map[string]string{}
	for _, key := range keys {
		notes[key] = "cid-for-" + key
		overlays[key] = map[string]string{"cache": "unsafe", "mbps_rd": key}
	}
	rawNotes, err := json.Marshal(notes)
	if err != nil {
		t.Fatal(err)
	}
	rawOverlays, err := json.Marshal(overlays)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := pve.RenderSentinel("operator text", map[string]json.RawMessage{
		"bosh_attached_disks":          rawNotes,
		pve.DiskOptOverlaysSentinelKey: rawOverlays,
	})
	if err != nil {
		t.Fatal(err)
	}
	return desc
}

// birthHeldEntries returns the raw attached-disk note and overlay entries a
// description carries, keyed the way the description keys them.
func birthHeldEntries(t *testing.T, desc string) (notes, overlays map[string]json.RawMessage) {
	t.Helper()
	_, raw := pve.ParseSentinel(desc)
	notes = map[string]json.RawMessage{}
	overlays = map[string]json.RawMessage{}
	if body, ok := raw["bosh_attached_disks"]; ok {
		if err := json.Unmarshal(body, &notes); err != nil {
			t.Fatalf("parse attached-disk notes: %v", err)
		}
	}
	if body, ok := raw[pve.DiskOptOverlaysSentinelKey]; ok {
		if err := json.Unmarshal(body, &overlays); err != nil {
			t.Fatalf("parse overlays: %v", err)
		}
	}
	return notes, overlays
}

// birthHeldPlacement is where the legacy disk B holds the stable-ID disk's
// birth name on the source VM. key is the config key, and value is what the
// key holds. A pending delete leaves the key out of the config the way PVE's
// config endpoint does, and the pending model keeps its current value. A
// pending add puts the key in the config as a pending value with no current
// one.
type birthHeldPlacement struct {
	name          string
	key           string
	value         string
	pendingDelete bool
	pendingAdd    bool
}

var (
	// birthHeldOnASlot is the placement every earlier row used.
	birthHeldOnASlot = &birthHeldPlacement{name: "slot", key: "scsi2", value: birthHeldBirth + ",size=5G"}
	// birthHeldOnUnused is a legacy disk the VM keeps on an unused entry.
	birthHeldOnUnused = &birthHeldPlacement{name: "unused entry", key: "unused2", value: birthHeldBirth}
	// birthHeldPendingDelete is a slot whose delete is pending on a VM that
	// still has the disk plugged in.
	birthHeldPendingDelete = &birthHeldPlacement{name: "pending delete", key: "scsi2", value: birthHeldBirth + ",size=5G", pendingDelete: true}
	// birthHeldPendingAdd is a slot that exists only as a pending value.
	birthHeldPendingAdd = &birthHeldPlacement{name: "pending add", key: "scsi2", value: birthHeldBirth + ",size=5G", pendingAdd: true}

	birthHeldPlacements = []*birthHeldPlacement{birthHeldOnASlot, birthHeldOnUnused, birthHeldPendingDelete, birthHeldPendingAdd}
)

// birthHeldClient seeds the source VM with the stable-ID disk on scsi1 and,
// when legacy isn't nil, a legacy disk placed as legacy says, named for the
// stable-ID disk's birth volume. The description carries notes and overlays
// under the stable ID, the stable-ID disk's name on the VM, and the birth
// name.
func birthHeldClient(t *testing.T, legacy *birthHeldPlacement) *idFakeClient {
	t.Helper()
	return birthHeldClientFor(t, birthHeldBirth, legacy)
}

// birthHeldClientFor is birthHeldClient for a disk whose birth name is birth.
func birthHeldClientFor(t *testing.T, birth string, legacy *birthHeldPlacement) *idFakeClient {
	t.Helper()
	source := map[string]any{
		"scsi1":       birthHeldAttached + ",serial=" + idTestToken + ",size=10G",
		"description": birthHeldDescription(t, idTestToken, birthHeldAttached, birth),
	}
	c := newIDFakeClient(map[int]map[string]any{
		birthHeldVMID:   source,
		birthHeldParker: {"tags": "bosh-cpi;bosh-parker", "protection": true},
	})
	if legacy == nil {
		return c
	}
	switch {
	case legacy.pendingDelete:
		c.pending = newFakePendingModel()
		source[legacy.key] = legacy.value
		c.pending.holdDelete(birthHeldVMID, source, legacy.key)
	case legacy.pendingAdd:
		c.pending = newFakePendingModel()
		c.pending.holdReplacement(birthHeldVMID, source, legacy.key, legacy.value)
	default:
		source[legacy.key] = legacy.value
	}
	return c
}

// birthHeldCID is the CID of the stable-ID disk, whose birth name is birth.
func birthHeldCID(t *testing.T, birth string) string {
	t.Helper()
	return overlayCID(t, birth, &pve.DiskCIDMeta{ID: idTestToken, Anchor: true})
}

// detachBirthHeld runs detach_disk for the stable-ID disk on the source VM.
func detachBirthHeld(ctx context.Context, t *testing.T, client pve.Client, logger *log.Logger) error {
	t.Helper()
	return detachBirthHeldFor(ctx, t, client, logger, birthHeldBirth)
}

// detachBirthHeldFor is detachBirthHeld for a disk whose birth name is birth.
func detachBirthHeldFor(ctx context.Context, t *testing.T, client pve.Client, logger *log.Logger, birth string) error {
	t.Helper()
	deps := Deps{Config: &config.CPIConfig{Node: "pve1", DiskStorage: "data"}, PVE: client, Logger: logger}
	diskCID := birthHeldCID(t, birth)
	vmArg, err := json.Marshal("700")
	if err != nil {
		t.Fatal(err)
	}
	diskArg, err := json.Marshal(diskCID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = HandleDetachDisk(deps).Handle(ctx, []json.RawMessage{vmArg, diskArg}, jsonrpc.Context{})
	return err
}

// requireLegacyDiskUntouched checks that the legacy disk, placed as legacy
// says, and its note and overlay come through the detach byte for byte.
func requireLegacyDiskUntouched(t *testing.T, c *idFakeClient, legacy *birthHeldPlacement, wantNotes, wantOverlays map[string]json.RawMessage) {
	t.Helper()
	cfg := c.configs[birthHeldVMID]
	if legacy.pendingDelete {
		if _, present := cfg[legacy.key]; present || !c.pending.pendingDelete(birthHeldVMID, legacy.key) {
			t.Fatalf("%s = %v with a pending delete = %v, want the delete still pending", legacy.key, cfg[legacy.key], c.pending.pendingDelete(birthHeldVMID, legacy.key))
		}
	} else if got, _ := cfg[legacy.key].(string); got != legacy.value {
		t.Fatalf("%s = %q, want the legacy disk left in place as %q", legacy.key, got, legacy.value)
	}
	notes, overlays := birthHeldEntries(t, pve.DescriptionFromConfig(cfg))
	if string(notes[birthHeldBirth]) != string(wantNotes[birthHeldBirth]) {
		t.Errorf("legacy disk's note = %s, want %s unchanged", notes[birthHeldBirth], wantNotes[birthHeldBirth])
	}
	if string(overlays[birthHeldBirth]) != string(wantOverlays[birthHeldBirth]) {
		t.Errorf("legacy disk's overlay = %s, want %s unchanged", overlays[birthHeldBirth], wantOverlays[birthHeldBirth])
	}
}

// requireDeferredPark checks that the detach left the disk's volume on an
// unused entry of the source VM under its old name, which is the state a
// snapshot refusal leaves for the park to finish later.
func requireDeferredPark(t *testing.T, cfg map[string]any) {
	t.Helper()
	if _, present := cfg["scsi1"]; present {
		t.Fatal("scsi1 is still there, want the disk off the bus")
	}
	for _, volid := range pve.FindUnusedDiskEntries(cfg) {
		if volid == birthHeldAttached {
			return
		}
	}
	t.Fatalf("no unused entry names %s, want the deferred park's volume left on the VM", birthHeldAttached)
}

// birthHeldPaths are the two ways the transfer ends. It either lands or meets
// PVE's snapshot refusal, which defers the park and leaves the volume on the
// disk's own unused entry.
var birthHeldPaths = []struct {
	name    string
	moveErr error
}{
	{name: "transfer lands"},
	{name: "snapshot refuses the reassignment", moveErr: errors.New("API request failed: Can't move disk used by a snapshot to another VM")},
}

// TestDetachStableIDKeepsNotesOnAHeldBirthName covers a detach whose disk's
// birth name now names a legacy disk on the same VM, whose notes are keyed by
// that name. The legacy disk holds the name on a bus slot, on an unused entry,
// on a slot whose delete is pending, or on a slot that exists only as a pending
// value. The detached disk's own notes, under its stable ID and under the name
// it had on the VM, come off, and the legacy disk's note and overlay come
// through byte for byte. The cleanup runs on both ends of the transfer.
func TestDetachStableIDKeepsNotesOnAHeldBirthName(t *testing.T) {
	t.Parallel()
	for _, place := range birthHeldPlacements {
		for _, path := range birthHeldPaths {
			t.Run(place.name+"/"+path.name, func(t *testing.T) {
				t.Parallel()
				c := birthHeldClient(t, place)
				c.moveErr = path.moveErr
				wantNotes, wantOverlays := birthHeldEntries(t, pve.DescriptionFromConfig(c.configs[birthHeldVMID]))
				logger, observed := log.NewObservedLogger(log.LevelWarn)

				if err := detachBirthHeld(context.Background(), t, c, logger); err != nil {
					t.Fatalf("detach: %v", err)
				}

				requireNoLeftKeysWarning(t, observed)
				requireDetachedDiskNotesGone(t, c, place, path.moveErr != nil, wantNotes, wantOverlays)
			})
		}
	}
}

// requireDetachedDiskNotesGone checks the outcome every held-name row shares.
// The legacy disk and its entries come through byte for byte, and the detached
// disk's own notes are gone under its stable ID and under its old name. A
// deferred park leaves the volume on the disk's own unused entry under its old
// name, which no other disk can hold, so the notes under that name come off on
// both paths.
func requireDetachedDiskNotesGone(t *testing.T, c *idFakeClient, legacy *birthHeldPlacement, deferred bool, wantNotes, wantOverlays map[string]json.RawMessage) {
	t.Helper()
	cfg := c.configs[birthHeldVMID]
	requireLegacyDiskUntouched(t, c, legacy, wantNotes, wantOverlays)
	notes, overlays := birthHeldEntries(t, pve.DescriptionFromConfig(cfg))
	if _, kept := notes[idTestToken]; kept {
		t.Error("the detached disk's note under its stable ID is still there")
	}
	if _, kept := overlays[idTestToken]; kept {
		t.Error("the detached disk's overlay under its stable ID is still there")
	}
	if deferred {
		requireDeferredPark(t, cfg)
	}
	if _, kept := notes[birthHeldAttached]; kept {
		t.Error("the detached disk's note under its old name is still there")
	}
	if _, kept := overlays[birthHeldAttached]; kept {
		t.Error("the detached disk's overlay under its old name is still there")
	}
}

// requireNoLeftKeysWarning checks that the cleanup logged no warning about
// keys it left in place, which it does only when the read of the VM fails.
func requireNoLeftKeysWarning(t *testing.T, observed *log.Observer) {
	t.Helper()
	for _, entry := range observed.All() {
		if strings.Contains(entry.Message, "keys_left_in_place") {
			t.Errorf("unexpected warning about keys left in place: %q", entry.Message)
		}
	}
}

// TestDetachStableIDRemovesAFreeBirthName is the control for the row above.
// No slot on the VM names the birth volume, so the detached disk's overlay
// filed under its birth name comes off with the rest of its notes, and the
// cleanup logs no warning.
func TestDetachStableIDRemovesAFreeBirthName(t *testing.T) {
	t.Parallel()
	c := birthHeldClient(t, nil)
	logger, observed := log.NewObservedLogger(log.LevelWarn)

	if err := detachBirthHeld(context.Background(), t, c, logger); err != nil {
		t.Fatalf("detach: %v", err)
	}

	requireNoLeftKeysWarning(t, observed)
	notes, overlays := birthHeldEntries(t, pve.DescriptionFromConfig(c.configs[birthHeldVMID]))
	for _, key := range []string{idTestToken, birthHeldAttached, birthHeldBirth} {
		if _, kept := overlays[key]; kept {
			t.Errorf("overlay under %s is still there", key)
		}
	}
	for _, key := range []string{idTestToken, birthHeldAttached} {
		if _, kept := notes[key]; kept {
			t.Errorf("note under %s is still there", key)
		}
	}
}

// pendingFailClient fails every pending read of the source VM once the
// detached disk has left it and landed on the parker, or, when deferred is
// set, once its move has been refused, which is the read the notes cleanup
// makes. It fails with a 5xx that the transient retry absorbs and then gives
// up on.
type pendingFailClient struct {
	*idFakeClient
	deferred bool
	mu       sync.Mutex
	failures int
}

func (c *pendingFailClient) Nodes() sdknodes.Service {
	return &pendingFailNodes{Service: c.idFakeClient.Nodes(), c: c}
}

type pendingFailNodes struct {
	sdknodes.Service
	c *pendingFailClient
}

func (n *pendingFailNodes) ListQemuPending(ctx context.Context, node, vmid string) (*sdknodes.ListQemuPendingResponse, error) {
	if vmid == "700" && n.c.diskLeft() {
		n.c.mu.Lock()
		n.c.failures++
		n.c.mu.Unlock()
		return nil, sdkerrors.ParseAPIError(596, []byte(`{"message":"connection close"}`))
	}
	return n.Service.ListQemuPending(ctx, node, vmid)
}

// failureCount returns how many pending reads the client has failed.
func (c *pendingFailClient) failureCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failures
}

// diskLeft reports whether the source VM no longer has the disk's slot and
// the transfer is over. A deferred transfer is over once its move has been
// tried, and any other once the parker carries the disk's serial.
func (c *pendingFailClient) diskLeft() bool {
	c.idFakeClient.mu.Lock()
	defer c.idFakeClient.mu.Unlock()
	if _, present := c.configs[birthHeldVMID]["scsi1"]; present {
		return false
	}
	if c.deferred {
		return c.moveCalls > 0
	}
	for _, value := range c.configs[birthHeldParker] {
		if text, ok := value.(string); ok && strings.Contains(text, "serial="+idTestToken) {
			return true
		}
	}
	return false
}

// requireLeftKeysWarning checks that the cleanup logged a warning naming the
// VM, the disk, the keys it left in place, the description maps they sit in,
// and the troubleshooting entry for removing them.
func requireLeftKeysWarning(t *testing.T, observed *log.Observer, wantCID, wantLeft string) {
	t.Helper()
	var warned bool
	for _, entry := range observed.All() {
		if !strings.Contains(entry.Message, "keys_left_in_place") {
			continue
		}
		warned = true
		if got := entry.Attrs["vmid"]; got != int64(birthHeldVMID) {
			t.Errorf("warning vmid = %v (%T), want %d", got, got, birthHeldVMID)
		}
		if got, _ := entry.Attrs["disk_cid"].(string); got != wantCID {
			t.Errorf("warning disk_cid = %q, want %q", got, wantCID)
		}
		if got, _ := entry.Attrs["keys_left_in_place"].(string); got != wantLeft {
			t.Errorf("warning keys_left_in_place = %q, want %q", got, wantLeft)
		}
		for _, want := range []string{"bosh_attached_disks", "bosh_disk_opt_overlays", `"A detach left a disk's notes on the VM it left" in docs/troubleshooting.md`} {
			if !strings.Contains(entry.Message, want) {
				t.Errorf("warning %q doesn't name %q", entry.Message, want)
			}
		}
	}
	if !warned {
		t.Error("no warning names the names the cleanup left in place")
	}
}

// TestDetachStableIDKeepsOtherNamesWhenTheSourceReadFails covers a cleanup
// whose read of the source VM fails after its transient retries. The detach
// still succeeds, and the notes under the stable ID come off. A name another
// disk could hold is left in place, so the legacy disk's note and overlay come
// through byte for byte, and a warning names the VM, the disk, and the names it
// left. After a landed transfer, that's the disk's old name and its birth
// name. After a deferred park, the disk's own unused entry still holds its old
// name, so its notes under that name come off and only the birth name is left.
func TestDetachStableIDKeepsOtherNamesWhenTheSourceReadFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		moveErr  error
		wantLeft string
	}{
		{name: "transfer lands", wantLeft: birthHeldAttached + "," + birthHeldBirth},
		{
			name:     "snapshot refuses the reassignment",
			moveErr:  errors.New("API request failed: Can't move disk used by a snapshot to another VM"),
			wantLeft: birthHeldBirth,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &pendingFailClient{idFakeClient: birthHeldClient(t, birthHeldOnASlot), deferred: tc.moveErr != nil}
			c.moveErr = tc.moveErr
			wantNotes, wantOverlays := birthHeldEntries(t, pve.DescriptionFromConfig(c.configs[birthHeldVMID]))
			logger, observed := log.NewObservedLogger(log.LevelWarn)
			ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })

			if err := detachBirthHeld(ctx, t, c, logger); err != nil {
				t.Fatalf("detach: %v", err)
			}

			if failures := c.failureCount(); failures < 2 {
				t.Errorf("source read failed %d times, want the transient retry to read it again", failures)
			}
			cfg := c.configs[birthHeldVMID]
			if tc.moveErr != nil {
				requireDeferredPark(t, cfg)
			}
			requireLegacyDiskUntouched(t, c.idFakeClient, birthHeldOnASlot, wantNotes, wantOverlays)
			notes, overlays := birthHeldEntries(t, pve.DescriptionFromConfig(cfg))
			if _, kept := notes[idTestToken]; kept {
				t.Error("the detached disk's note under its stable ID is still there")
			}
			if _, kept := overlays[idTestToken]; kept {
				t.Error("the detached disk's overlay under its stable ID is still there")
			}
			_, noteKept := notes[birthHeldAttached]
			_, overlayKept := overlays[birthHeldAttached]
			if tc.moveErr == nil && (!noteKept || !overlayKept) {
				t.Errorf("under the disk's old name, note kept = %v and overlay kept = %v, want both kept without a read that shows the name free",
					noteKept, overlayKept)
			}
			if tc.moveErr != nil && (noteKept || overlayKept) {
				t.Errorf("under the disk's old name, note kept = %v and overlay kept = %v, want both removed, because the disk's own unused entry holds that name",
					noteKept, overlayKept)
			}

			requireLeftKeysWarning(t, observed, birthHeldCID(t, birthHeldBirth), tc.wantLeft)
		})
	}
}

// TestDetachStableIDLogsNoWarningWhenNoKeyIsLeft covers a failed read after a
// deferred park of a disk whose birth name is the name it has on the VM, which
// is how a free-floating disk looks after a config edit attached it. The
// disk's own unused entry holds that one name, so the cleanup removes it and
// has no key left to name. The detach succeeds, the disk's notes come off, and
// the cleanup logs no warning, because a warning that names no key would only
// send an operator looking for entries that aren't there.
func TestDetachStableIDLogsNoWarningWhenNoKeyIsLeft(t *testing.T) {
	t.Parallel()
	c := &pendingFailClient{idFakeClient: birthHeldClientFor(t, birthHeldAttached, nil), deferred: true}
	c.moveErr = errors.New("API request failed: Can't move disk used by a snapshot to another VM")
	logger, observed := log.NewObservedLogger(log.LevelWarn)
	ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })

	if err := detachBirthHeldFor(ctx, t, c, logger, birthHeldAttached); err != nil {
		t.Fatalf("detach: %v", err)
	}

	if failures := c.failureCount(); failures < 2 {
		t.Errorf("source read failed %d times, want the transient retry to read it again", failures)
	}
	requireNoLeftKeysWarning(t, observed)
	cfg := c.configs[birthHeldVMID]
	requireDeferredPark(t, cfg)
	notes, overlays := birthHeldEntries(t, pve.DescriptionFromConfig(cfg))
	for _, key := range []string{idTestToken, birthHeldAttached} {
		if _, kept := notes[key]; kept {
			t.Errorf("note under %s is still there", key)
		}
		if _, kept := overlays[key]; kept {
			t.Errorf("overlay under %s is still there", key)
		}
	}
}
