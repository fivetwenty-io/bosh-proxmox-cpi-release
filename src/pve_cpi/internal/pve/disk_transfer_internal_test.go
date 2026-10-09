// disk_transfer_internal_test.go — white-box tests for the D13 ownership
// transfer: attach- and detach-direction reassignment, crash-window resume,
// and the parker-owned deletion path. scanFakeClient simulates the PVE
// behaviors the live spike established: move_disk renames the volume for its
// new owner, an attached-slot move carries the option string, an
// unused-entry move drops it, and removing an unusedN entry physically
// deletes a volume its holder owns. Deleting a bus slot leaves an unusedN
// entry only for a volume the VM owns by name
// (vmconfig_register_unused_drive), and attaching a volume the VM already
// names on an unusedN entry drops that entry (write_vm_config).
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// scanFakeClient is a stateful single-node PVE for identity and transfer
// tests. Guest configs are live maps mutated by the qemu/nodes fakes; the
// cluster listing is derived from them (or overridden via rows).
type scanFakeClient struct {
	parkerLockClient

	mu      sync.Mutex
	configs map[int]map[string]any
	rows    []map[string]any
	// events records every state-changing call, in order, so ordering
	// assertions (intent before detach, bake before move) are direct.
	events []string
	// destroyed records volids PVE physically removed (the owned-unused
	// sweep semantics).
	destroyed []string
	// moveErr, when set, fails the next CreateQemuMoveDisk with it.
	moveErr error
	// configErr, when set for a vmid, fails every config read for it with
	// that error (e.g. PVE's "Configuration file ... does not exist" for a
	// destroyed VM).
	configErr map[int]error
	// renameCounter numbers vm-<target>-disk-<n> names per target VM.
	renameCounter map[int]int
	// running marks VMs that are running, whose bus-slot deletes PVE can
	// only record as pending. It holds one when the VM's hotplug setting
	// lacks disk, and when busy marks the guest as holding the device, in
	// which case the delete also fails busy. A revert drops the pending
	// delete.
	running map[int]bool
	busy    map[int]bool
	// held is each VM's pending section, every key whose delete is pending
	// with its current value. The config read serves the applied view
	// without them, and the pending endpoint reports them.
	held map[int]map[string]any
	// replaced holds, per VM, each key whose config value is a pending value
	// that replaces a different current drive, with that current value. The
	// config read serves the pending value, and the pending endpoint reports
	// both.
	replaced map[int]map[string]any
	// pendingErrOnce fails the next pending read of each listed VM with its
	// error, and only that read.
	pendingErrOnce map[int]error
	// snapshots holds, per VM, each snapshot's configuration by name, and
	// snapshotErr fails every snapshot listing and configuration read.
	snapshots   map[int]map[string]map[string]any
	snapshotErr error
	// revertErr, when set, fails every revert, and revertKeepsPending makes
	// every revert succeed without dropping the pending delete.
	revertErr          error
	revertKeepsPending bool
	// released holds volumes a slot delete took off a VM that doesn't own
	// them, which no config names afterwards while they still sit on
	// storage. unlistedVolumes hides volumes from the storage content
	// listing, the way a volume something else renamed or removed is gone
	// from it, and the listing serves every other volume a config names or
	// a delete released.
	released        map[string]bool
	unlistedVolumes map[string]bool
	// attachErr, when set, fails every AttachDisk with it, and attachCalls
	// counts every AttachDisk, refused ones included.
	attachErr   error
	attachCalls int
}

func newScanFakeClient(configs map[int]map[string]any) *scanFakeClient {
	return &scanFakeClient{configs: configs, renameCounter: map[int]int{}}
}

func (c *scanFakeClient) logEvent(format string, args ...any) {
	c.events = append(c.events, fmt.Sprintf(format, args...))
}

// fakeAnsweredDigest stands in for the digest PVE puts in every config
// answer, for a fake config whose test sets none. A parker description write
// refuses a read without a digest, because a write built from it could erase
// records it never saw.
const fakeAnsweredDigest = "fake-answered-digest"

// withAnsweredDigest gives cfg the digest PVE would answer with when the test
// set none, and returns cfg.
func withAnsweredDigest(cfg map[string]any) map[string]any {
	if _, ok := cfg["digest"]; !ok {
		cfg["digest"] = fakeAnsweredDigest
	}
	return cfg
}

func (c *scanFakeClient) configCopy(vmid int) (map[string]any, bool) {
	cfg, ok := c.configs[vmid]
	if !ok {
		return nil, false
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out, true
}

func (c *scanFakeClient) storageOf(volid string) string {
	if i := strings.IndexByte(volid, ':'); i >= 0 {
		return volid[:i]
	}
	return volid
}

// renameFor produces the volid move_disk gives a volume landing on target.
func (c *scanFakeClient) renameFor(target int, oldVolid string) string {
	n := c.renameCounter[target]
	c.renameCounter[target]++
	return fmt.Sprintf("%s:vm-%d-disk-%d", c.storageOf(oldVolid), target, n)
}

func (c *scanFakeClient) QEMU() qemu.Service {
	return &fakeQEMUService{
		listSnapshotsFn: func(_ context.Context, _ string, vmid int) ([]map[string]any, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.snapshotErr != nil {
				return nil, c.snapshotErr
			}
			out := []map[string]any{{"name": "current"}}
			for name := range c.snapshots[vmid] {
				out = append(out, map[string]any{"name": name})
			}
			return out, nil
		},
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if err, forced := c.configErr[vmid]; forced {
				return nil, err
			}
			cfg, ok := c.configCopy(vmid)
			if !ok {
				return nil, fmt.Errorf("fake: no config for vmid %d", vmid)
			}
			return withAnsweredDigest(cfg), nil
		},
		attachDiskFn: func(_ context.Context, _ string, vmid int, volid, _ string, opts *qemu.AttachOpts) (string, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.attachCalls++
			if c.attachErr != nil {
				return "", c.attachErr
			}
			cfg, ok := c.configs[vmid]
			if !ok {
				return "", fmt.Errorf("fake: no config for vmid %d", vmid)
			}
			slot := ""
			if opts != nil {
				slot = opts.DiskID
			}
			cfg[slot] = volid
			bare := volid
			if comma := strings.IndexByte(bare, ','); comma >= 0 {
				bare = bare[:comma]
			}
			for key, unused := range FindUnusedDiskEntries(cfg) {
				if unused == bare {
					delete(cfg, key)
				}
			}
			c.logEvent("attach:%d:%s:%s", vmid, slot, volid)
			return slot, nil
		},
		detachDiskFn: func(_ context.Context, _ string, vmid int, diskID string) error {
			// SDK semantics: config delete of the slot, then a second request
			// removing the unusedN entry PVE demoted the volume to. PVE
			// physically deletes an unused volume its holder owns.
			c.mu.Lock()
			defer c.mu.Unlock()
			cfg, ok := c.configs[vmid]
			if !ok {
				return fmt.Errorf("fake: no config for vmid %d", vmid)
			}
			raw, present := cfg[diskID]
			if !present {
				return nil
			}
			val, _ := raw.(string)
			bare := val
			if comma := strings.IndexByte(bare, ','); comma >= 0 {
				bare = bare[:comma]
			}
			delete(cfg, diskID)
			c.logEvent("detach:%d:%s:%s", vmid, diskID, bare)
			if strings.HasPrefix(diskID, "unused") {
				if scanFakeOwns(bare, vmid) {
					c.destroyed = append(c.destroyed, bare)
					c.logEvent("destroy:%s", bare)
				}
				return nil
			}
			// The demote-and-sweep pair, collapsed: options drop on the
			// unused entry, and the sweep removes it again immediately.
			if scanFakeOwns(bare, vmid) {
				c.destroyed = append(c.destroyed, bare)
				c.logEvent("destroy:%s", bare)
			}
			return nil
		},
	}
}

func (c *scanFakeClient) Nodes() sdknodes.Service {
	return &fakeNodesService{
		listQemuSnapshotConfigFn: func(_ context.Context, _, vmidText, name string) (*sdknodes.ListQemuSnapshotConfigResponse, error) {
			vmid, _ := strconv.Atoi(vmidText)
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.snapshotErr != nil {
				return nil, c.snapshotErr
			}
			cfg, ok := c.snapshots[vmid][name]
			if !ok {
				return nil, fmt.Errorf("fake: vm %d has no snapshot %q", vmid, name)
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				return nil, err
			}
			resp := sdknodes.ListQemuSnapshotConfigResponse(raw)
			return &resp, nil
		},
		updateQemuConfigFn:   c.updateQemuConfig,
		createQemuMoveDiskFn: c.createQemuMoveDisk,
		listStorageContentFn: c.listStorageContent,
		qemuConfigFn:         c.QEMU().Config,
		listQemuPendingFn:    c.listQemuPending,
		// The authoritative per-node listing the parker and holder scans now
		// read, derived from the same configs the ListResources fake serves.
		listQemuFn: func(context.Context, string, *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			var resp sdknodes.ListQemuResponse
			if c.rows != nil {
				for _, row := range c.rows {
					b, err := json.Marshal(row)
					if err != nil {
						return nil, err
					}
					resp = append(resp, b)
				}
				return &resp, nil
			}
			for vmid, cfg := range c.configs {
				tags, _ := cfg["tags"].(string)
				b, err := json.Marshal(map[string]any{"vmid": vmid, cfgKeyTags: tags})
				if err != nil {
					return nil, err
				}
				resp = append(resp, b)
			}
			return &resp, nil
		},
	}
}

func (c *scanFakeClient) updateQemuConfig(_ context.Context, _ string, vmidStr string, params *sdknodes.UpdateQemuConfigParams) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	vmid, _ := strconv.Atoi(vmidStr)
	cfg, ok := c.configs[vmid]
	if !ok {
		return fmt.Errorf("fake: no config for vmid %s", vmidStr)
	}
	if params.Revert != nil {
		return c.revertLocked(cfg, vmid, *params.Revert)
	}
	if params.Delete != nil {
		if held, err := c.holdDeleteLocked(cfg, vmid, *params.Delete); held {
			return err
		}
		c.deleteConfigKeyLocked(cfg, vmid, *params.Delete)
	}
	if params.Description != nil {
		cfg["description"] = *params.Description
		c.logEvent("description:%d", vmid)
	}
	if params.Protection != nil {
		cfg[paramProtection] = *params.Protection
		c.logEvent("protection:%d:%v", vmid, *params.Protection)
	}
	return nil
}

// holdDeleteLocked records a bus-slot delete on a running VM as pending, the
// way qemu-server does when the VM's hotplug setting lacks disk or the guest
// still holds the device, and reports whether it did. A busy guest also fails
// the delete.
func (c *scanFakeClient) holdDeleteLocked(cfg map[string]any, vmid int, slot string) (bool, error) {
	if !c.running[vmid] || strings.HasPrefix(slot, "unused") {
		return false, nil
	}
	hotplug, set := cfg["hotplug"].(string)
	lacksDisk := set && !strings.Contains(","+hotplug+",", ",disk,")
	if !lacksDisk && !c.busy[vmid] {
		return false, nil
	}
	if value, present := cfg[slot]; present {
		if c.held == nil {
			c.held = map[int]map[string]any{}
		}
		if c.held[vmid] == nil {
			c.held[vmid] = map[string]any{}
		}
		c.held[vmid][slot] = value
		delete(cfg, slot)
	}
	c.logEvent("pending-delete:%d:%s", vmid, slot)
	if lacksDisk {
		return true, nil
	}
	return true, fmt.Errorf("API request failed: parameter error: Parameter verification failed. (code: 0, errors: %s: "+
		"hotplug problem - error on hot-unplugging device 'virtio%s' - still busy in guest?)", slot, slot)
}

// revertLocked drops the pending delete of each named key, which puts the key
// back in the applied view.
func (c *scanFakeClient) revertLocked(cfg map[string]any, vmid int, keys string) error {
	c.logEvent("revert:%d:%s", vmid, keys)
	if c.revertErr != nil {
		return c.revertErr
	}
	if c.revertKeepsPending {
		return nil
	}
	for _, key := range strings.Split(keys, ",") {
		if value, held := c.held[vmid][key]; held {
			cfg[key] = value
			delete(c.held[vmid], key)
		}
	}
	return nil
}

// listQemuPending serves the pending endpoint from the config read, so an
// injected config failure fails it the same way. A replaced key comes with its
// current value and its pending one, and each held key comes with its current
// value and a delete flag.
func (c *scanFakeClient) listQemuPending(ctx context.Context, node, vmidText string) (*sdknodes.ListQemuPendingResponse, error) {
	if vmid, err := strconv.Atoi(vmidText); err == nil {
		c.mu.Lock()
		failure, once := c.pendingErrOnce[vmid]
		delete(c.pendingErrOnce, vmid)
		c.mu.Unlock()
		if once {
			return nil, failure
		}
	}
	resp, err := PendingFromConfigRead(ctx, c.QEMU().Config, node, vmidText)
	if err != nil {
		return nil, err
	}
	vmid, _ := strconv.Atoi(vmidText)
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, raw := range *resp {
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		key, _ := item["key"].(string)
		current, isReplaced := c.replaced[vmid][key]
		if !isReplaced {
			continue
		}
		rewritten, err := json.Marshal(map[string]any{"key": key, "value": current, "pending": item["value"]})
		if err != nil {
			return nil, err
		}
		(*resp)[i] = rewritten
	}
	keys := make([]string, 0, len(c.held[vmid]))
	for key := range c.held[vmid] {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		raw, err := json.Marshal(map[string]any{"key": key, "value": c.held[vmid][key], "delete": 1})
		if err != nil {
			return nil, err
		}
		*resp = append(*resp, raw)
	}
	return resp, nil
}

// deleteConfigKeyLocked applies PVE's config-delete semantics: a deleted bus
// slot whose volume the VM owns is demoted to the first free unusedN key,
// dropping the options, and any other volume loses its last reference.
func (c *scanFakeClient) deleteConfigKeyLocked(cfg map[string]any, vmid int, slot string) {
	raw, present := cfg[slot]
	if !present {
		return
	}
	val, _ := raw.(string)
	bare := val
	if comma := strings.IndexByte(bare, ','); comma >= 0 {
		bare = bare[:comma]
	}
	delete(cfg, slot)
	if !strings.HasPrefix(slot, "unused") && !scanFakeOwns(bare, vmid) {
		if c.released == nil {
			c.released = map[string]bool{}
		}
		c.released[bare] = true
	}
	if !strings.HasPrefix(slot, "unused") && scanFakeOwns(bare, vmid) {
		for i := 0; ; i++ {
			key := fmt.Sprintf("unused%d", i)
			if _, taken := cfg[key]; !taken {
				cfg[key] = bare
				break
			}
		}
	}
	c.logEvent("config-delete:%d:%s:%s", vmid, slot, bare)
}

// scanFakeDriveKeyPattern matches the config keys that name a volume.
var scanFakeDriveKeyPattern = regexp.MustCompile(`^(?:scsi|virtio|sata|ide|unused|efidisk|tpmstate)\d+$`)

// listStorageContent serves an unfiltered content listing of storage. It
// lists every volume a config names or a slot delete released, less the
// volumes PVE destroyed and the ones the test hid.
func (c *scanFakeClient) listStorageContent(
	_ context.Context, _, storage string, _ *sdknodes.ListStorageContentParams,
) (*sdknodes.ListStorageContentResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	present := map[string]bool{}
	for volid := range c.released {
		present[volid] = true
	}
	for _, cfg := range c.configs {
		for key, raw := range cfg {
			if val, ok := raw.(string); ok && scanFakeDriveKeyPattern.MatchString(key) {
				bare := val
				if comma := strings.IndexByte(bare, ','); comma >= 0 {
					bare = bare[:comma]
				}
				present[bare] = true
			}
		}
	}
	for _, gone := range c.destroyed {
		delete(present, gone)
	}
	volids := make([]string, 0, len(present))
	for volid := range present {
		if c.storageOf(volid) == storage && !c.unlistedVolumes[volid] {
			volids = append(volids, volid)
		}
	}
	sort.Strings(volids)
	resp := sdknodes.ListStorageContentResponse{}
	for _, volid := range volids {
		b, err := json.Marshal(map[string]any{"volid": volid})
		if err != nil {
			return nil, err
		}
		resp = append(resp, b)
	}
	return &resp, nil
}

// scanFakeOwnerPattern is the fake's own copy of PVE's owner rule, kept apart
// from volumeOwnerVMID so the fake can disagree with production. VM N owns any
// vm-<N>-* or base-<N>-* name. The block storage plugins read it from the
// volume name, and the directory plugin reads it from the images/<vmid>/
// directory, which agree on every name PVE allocates.
var scanFakeOwnerPattern = regexp.MustCompile(`^(?:vm|base)-([1-9]\d*)-`)

// scanFakeOwns reports whether PVE counts vmid as the owner of volid.
func scanFakeOwns(volid string, vmid int) bool {
	m := scanFakeOwnerPattern.FindStringSubmatch(volid[strings.LastIndexAny(volid, "/:")+1:])
	return len(m) == 2 && m[1] == strconv.Itoa(vmid)
}

func (c *scanFakeClient) createQemuMoveDisk(_ context.Context, _ string, vmidStr string, params *sdknodes.CreateQemuMoveDiskParams) (*sdknodes.CreateQemuMoveDiskResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.moveErr != nil {
		err := c.moveErr
		c.moveErr = nil
		return nil, err
	}
	srcVMID, _ := strconv.Atoi(vmidStr)
	srcCfg, ok := c.configs[srcVMID]
	if !ok {
		return nil, fmt.Errorf("fake: no config for vmid %s", vmidStr)
	}
	raw, present := srcCfg[params.Disk]
	if !present {
		return nil, fmt.Errorf("fake: source %s has no key %s", vmidStr, params.Disk)
	}
	val, _ := raw.(string)
	bare := val
	opts := ""
	if comma := strings.IndexByte(val, ','); comma >= 0 {
		bare = val[:comma]
		opts = val[comma:]
	}
	if strings.HasPrefix(params.Disk, "unused") {
		// Spike result: unused entries carry no options.
		opts = ""
	}
	target := int(*params.TargetVmid)
	targetCfg, ok := c.configs[target]
	if !ok {
		return nil, fmt.Errorf("fake: no config for target vmid %d", target)
	}
	landed := c.renameFor(target, bare)
	delete(srcCfg, params.Disk)
	targetCfg[*params.TargetDisk] = landed + opts
	c.logEvent("move:%d:%s->%d:%s:%s", srcVMID, params.Disk, target, *params.TargetDisk, landed)
	// No UPID: the caller treats the 200 as completion.
	resp := json.RawMessage(`""`)
	return &resp, nil
}

// clusterRow builds one /cluster/resources row for a QEMU guest. Shared by
// every fake listing here so the JSON key literals live in one place.
func clusterRow(vmid int, tags string) map[string]any {
	return map[string]any{"vmid": vmid, "node": "pve1", "type": clusterResourceTypeQemu, cfgKeyTags: tags}
}

func (c *scanFakeClient) Cluster() sdkcluster.Service {
	return &fakeClusterService{
		listConfigNodesFn: func(context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
			resp := sdkcluster.ListConfigNodesResponse{json.RawMessage(`{"name": "pve1"}`)}
			return &resp, nil
		},
		listResourcesFn: func(context.Context, *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			rows := c.rows
			if rows == nil {
				for vmid, cfg := range c.configs {
					tags, _ := cfg["tags"].(string)
					rows = append(rows, clusterRow(vmid, tags))
				}
			}
			var resp sdkcluster.ListResourcesResponse
			for _, row := range rows {
				b, err := json.Marshal(row)
				if err != nil {
					return nil, err
				}
				resp = append(resp, b)
			}
			return &resp, nil
		},
	}
}

// parkedEntries decodes the test parker's (vmid 90000) bosh_parked_disks
// sentinel.
func (c *scanFakeClient) parkedEntries(t *testing.T) map[string]parkerProvEntry {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	desc, _ := c.configs[90000]["description"].(string)
	_, disks, _ := parseParkerSentinel(desc)
	return disks
}

func (c *scanFakeClient) eventIndex(substr string) int {
	for i, e := range c.events {
		if strings.Contains(e, substr) {
			return i
		}
	}
	return -1
}

var transferTestCfg = ParkerConfig{
	VMIDRangeStart: 90000,
	VMIDRangeEnd:   90999,
	FallbackNode:   "pve1",
	ParkedEnabled:  true,
}

const transferStableID = "bpd-00112233aabbccdd"

func TestTransferDiskToParker_ProtocolAndOrdering(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		700: {"scsi1": "data:vm-700-disk-1,serial=" + transferStableID + ",size=10G"},
		90000: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
		},
	})
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
	landed, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, pctx)
	if err != nil {
		t.Fatalf("TransferDiskToParker: %v", err)
	}
	if !strings.HasPrefix(landed, "data:vm-90000-disk-") {
		t.Errorf("landed volid = %q, want a parker-named volume", landed)
	}

	// The write ordering the crash-window analysis depends on: the intent
	// record lands on the parker BEFORE the source slot is deleted.
	intentIdx := c.eventIndex("description:90000")
	detachIdx := c.eventIndex("config-delete:700:scsi1")
	if intentIdx < 0 || detachIdx < 0 || intentIdx > detachIdx {
		t.Errorf("intent must precede the source-slot delete; events=%v", c.events)
	}

	// The parker slot carries the serial (re-applied after the unused-entry
	// move dropped the options).
	parkerDisks := qemu.ParseDisks(c.configs[90000])
	slot, current, ok := matchDiskIdentityAs(parkerDisks, "none", transferStableID, matchSerial)
	if !ok || current != landed {
		t.Fatalf("parker slot after transfer: slot=%q current=%q ok=%v", slot, current, ok)
	}

	// The provenance record is keyed by the stable ID and finalized with the
	// landed volid.
	entries := c.parkedEntries(t)
	entry, ok := entries[transferStableID]
	if !ok {
		t.Fatalf("provenance entries = %+v, want one keyed by the stable ID", entries)
	}
	if entry.Volid != landed || entry.Slot != slot || entry.SourceVMCID != "700" {
		t.Errorf("provenance entry = %+v", entry)
	}

	// The source VM no longer references the volume anywhere, and nothing
	// was destroyed.
	srcCfg := c.configs[700]
	if len(FindUnusedDiskEntries(srcCfg)) != 0 {
		t.Errorf("source unused entries remain: %v", srcCfg)
	}
	if len(c.destroyed) != 0 {
		t.Errorf("volumes destroyed during transfer: %v", c.destroyed)
	}
}

func TestTransferDiskToParker_SnapshotRefusalLeavesResumableState(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		700: {"scsi1": "data:vm-700-disk-1,serial=" + transferStableID + ",size=10G"},
		90000: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
		},
	})
	c.moveErr = errors.New("Can't move disk used by a snapshot to another VM")
	pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}
	_, err := TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", transferTestCfg, pctx)
	if !IsMoveDiskSnapshotRefusal(err) {
		t.Fatalf("err = %v, want a detectable snapshot refusal", err)
	}
	// The state the detach handler's deferred-park fallback relies on: the
	// source slot is off the bus (demoted to unusedN, where the snapshot
	// keeps the volume anchored) and the intent record already names the
	// disk on the parker, so a later resume can finish the park.
	srcDisks := qemu.ParseDisks(c.configs[700])
	if _, onBus := FindDiskIDByVolID(srcDisks, "data:vm-700-disk-1"); onBus {
		t.Error("volume still on an active source slot after the refused transfer")
	}
	unused := FindUnusedDiskEntries(c.configs[700])
	found := false
	for _, volid := range unused {
		if volid == "data:vm-700-disk-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("source unused entries = %v, want the demoted volume kept", unused)
	}
	if _, ok := c.parkedEntries(t)[transferStableID]; !ok {
		t.Errorf("parker provenance = %+v, want the intent record kept", c.parkedEntries(t))
	}
	if len(c.destroyed) != 0 {
		t.Errorf("volumes destroyed: %v", c.destroyed)
	}
}

func TestTransferDiskFromParker_CarriesOptionsAndRenames(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		90000: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
			"scsi0":         "data:vm-90000-disk-0,serial=" + transferStableID,
		},
		700: {},
	})
	parker := DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi0"}
	optStr := "data:vm-90000-disk-0,iothread=1,serial=" + transferStableID
	landed, err := TransferDiskFromParker(context.Background(), c, nil, parker, 700, "scsi1", "data:vm-90000-disk-0", optStr, transferTestCfg)
	if err != nil {
		t.Fatalf("TransferDiskFromParker: %v", err)
	}
	if !strings.HasPrefix(landed, "data:vm-700-disk-") {
		t.Errorf("landed volid = %q, want a target-named volume", landed)
	}
	// The full option string rode the move: serial and iothread are on the
	// target slot.
	targetVal, _ := c.configs[700]["scsi1"].(string)
	if !strings.Contains(targetVal, "serial="+transferStableID) || !strings.Contains(targetVal, "iothread=1") {
		t.Errorf("target slot = %q, want serial and iothread carried", targetVal)
	}
	// The option bake happened before the move, and protection was cleared
	// for the move and restored after.
	bakeIdx := c.eventIndex("attach:90000:scsi0")
	moveIdx := c.eventIndex("move:90000")
	protOffIdx := c.eventIndex("protection:90000:false")
	protOnIdx := c.eventIndex("protection:90000:true")
	if bakeIdx < 0 || moveIdx < 0 || bakeIdx > moveIdx {
		t.Errorf("option bake must precede the move; events=%v", c.events)
	}
	if protOffIdx < 0 || protOnIdx < 0 || protOffIdx >= moveIdx || moveIdx >= protOnIdx {
		t.Errorf("protection must be cleared around the move and restored; events=%v", c.events)
	}
	if len(c.destroyed) != 0 {
		t.Errorf("volumes destroyed during transfer: %v", c.destroyed)
	}
}

func TestTransferDiskFromParker_SnapshotRefusalIsDetectable(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		90000: {
			cfgKeyTags: "bosh-cpi;bosh-parker",
			"scsi0":    "data:vm-90000-disk-0,serial=" + transferStableID,
		},
		700: {},
	})
	c.moveErr = errors.New("Can't move disk used by a snapshot to another VM")
	parker := DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi0"}
	_, err := TransferDiskFromParker(context.Background(), c, nil, parker, 700, "scsi1",
		"data:vm-90000-disk-0", "data:vm-90000-disk-0,serial="+transferStableID, transferTestCfg)
	if !errors.Is(err, ErrMoveDiskSnapshotRefused) {
		t.Fatalf("err = %v, want ErrMoveDiskSnapshotRefused", err)
	}
	// Protection must be back on after the refused move.
	if prot, _ := c.configs[90000][paramProtection].(bool); !prot {
		t.Error("protection not restored after the refused move")
	}
}

func TestResumeDiskTransferToParker_Windows(t *testing.T) {
	t.Parallel()

	t.Run("serial already landed: only finalize was lost", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			90000: {
				cfgKeyTags: "bosh-cpi;bosh-parker",
				"scsi2":    "data:vm-90000-disk-5,serial=" + transferStableID,
			},
		})
		intent := DiskTransferIntent{ParkerVMID: 90000, ParkerNode: "pve1", Slot: "scsi2", Volid: "data:vm-700-disk-1", SourceVMCID: "700"}
		landed, err := ResumeDiskTransferToParker(context.Background(), c, nil, intent, transferStableID, transferTestCfg, ParkContext{})
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if landed != "data:vm-90000-disk-5" {
			t.Errorf("landed = %q", landed)
		}
		if entry := c.parkedEntries(t)[transferStableID]; entry.Volid != landed {
			t.Errorf("finalized entry = %+v", entry)
		}
	})

	t.Run("move never ran: source unusedN still holds the recorded volid", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			700:   {"unused0": "data:vm-700-disk-1"},
			90000: {cfgKeyTags: "bosh-cpi;bosh-parker"},
		})
		intent := DiskTransferIntent{ParkerVMID: 90000, ParkerNode: "pve1", Slot: "scsi4", Volid: "data:vm-700-disk-1", SourceVMCID: "700"}
		landed, err := ResumeDiskTransferToParker(context.Background(), c, nil, intent, transferStableID, transferTestCfg, ParkContext{})
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if !strings.HasPrefix(landed, "data:vm-90000-disk-") {
			t.Errorf("landed = %q", landed)
		}
		if got, _ := c.configs[90000]["scsi4"].(string); !strings.Contains(got, "serial="+transferStableID) {
			t.Errorf("parker slot after resume = %q, want the serial re-applied", got)
		}
		if len(FindUnusedDiskEntries(c.configs[700])) != 0 {
			t.Errorf("source unused entries remain: %v", c.configs[700])
		}
	})

	t.Run("move landed but the serial write was lost: claim the recorded slot", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			// The source VM still exists but no longer references the volume
			// anywhere — the move already took it.
			700: {},
			90000: {
				cfgKeyTags: "bosh-cpi;bosh-parker",
				"scsi4":    "data:vm-90000-disk-3",
			},
		})
		intent := DiskTransferIntent{ParkerVMID: 90000, ParkerNode: "pve1", Slot: "scsi4", Volid: "data:vm-700-disk-1", SourceVMCID: "700"}
		landed, err := ResumeDiskTransferToParker(context.Background(), c, nil, intent, transferStableID, transferTestCfg, ParkContext{})
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if landed != "data:vm-90000-disk-3" {
			t.Errorf("landed = %q", landed)
		}
		if got, _ := c.configs[90000]["scsi4"].(string); !strings.Contains(got, "serial="+transferStableID) {
			t.Errorf("claimed slot = %q, want the serial applied", got)
		}
	})

	t.Run("source VM destroyed after a bypassed detach: config-edit park converges", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			90000: {cfgKeyTags: "bosh-cpi;bosh-parker"},
		})
		c.configErr = map[int]error{
			700: errors.New("Configuration file 'nodes/pve1/qemu-server/700.conf' does not exist"),
		}
		// The volume is foreign-named for the destroyed source VM, so PVE's
		// purge left it on storage; only the intent record still names it.
		intent := DiskTransferIntent{ParkerVMID: 90000, ParkerNode: "pve1", Slot: "scsi4", Volid: "data:vm-11949-disk-0", SourceVMCID: "700"}
		landed, err := ResumeDiskTransferToParker(context.Background(), c, nil, intent, transferStableID, transferTestCfg, ParkContext{})
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if landed != "data:vm-11949-disk-0" {
			t.Errorf("landed = %q, want the birth-named volume parked under its own name", landed)
		}
		got, _ := c.configs[90000]["scsi0"].(string)
		if !strings.Contains(got, "data:vm-11949-disk-0") || !strings.Contains(got, "serial="+transferStableID) {
			t.Errorf("parker slot = %q, want the volume config-edit attached with the serial", got)
		}
		if entry := c.parkedEntries(t)[transferStableID]; entry.Volid != landed {
			t.Errorf("finalized entry = %+v", entry)
		}
	})

	t.Run("source VM exists but released the volume: config-edit park converges", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			700:   {},
			90000: {cfgKeyTags: "bosh-cpi;bosh-parker"},
		})
		intent := DiskTransferIntent{ParkerVMID: 90000, ParkerNode: "pve1", Slot: "", Volid: "data:vm-11949-disk-0", SourceVMCID: "700"}
		landed, err := ResumeDiskTransferToParker(context.Background(), c, nil, intent, transferStableID, transferTestCfg, ParkContext{})
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if landed != "data:vm-11949-disk-0" {
			t.Errorf("landed = %q", landed)
		}
		if got, _ := c.configs[90000]["scsi0"].(string); !strings.Contains(got, "serial="+transferStableID) {
			t.Errorf("parker slot = %q, want the serial applied", got)
		}
	})

	t.Run("unconvergeable state is a permanent, descriptive error", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			90000: {cfgKeyTags: "bosh-cpi;bosh-parker"},
		})
		intent := DiskTransferIntent{ParkerVMID: 90000, ParkerNode: "pve1", Slot: "scsi4", Volid: "data:vm-700-disk-1", SourceVMCID: ""}
		_, err := ResumeDiskTransferToParker(context.Background(), c, nil, intent, transferStableID, transferTestCfg, ParkContext{})
		if err == nil || !strings.Contains(err.Error(), "intent record") {
			t.Fatalf("err = %v, want the unconvergeable-state refusal", err)
		}
	})
}

func TestDeleteParkedOwnedDisk_DeallocatesAndCleansProvenance(t *testing.T) {
	t.Parallel()
	desc := `<!--BOSH:{"bosh_parked_disks":{"` + transferStableID + `":{"disk_cid":"pvd-x","parked_at":"t",` +
		`"node":"pve1","volid":"data:vm-90000-disk-2","slot":"scsi1"}}}-->`
	c := newScanFakeClient(map[int]map[string]any{
		90000: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
			"scsi1":         "data:vm-90000-disk-2,serial=" + transferStableID,
			"description":   desc,
			// PVE answers every config read with a digest, and the
			// provenance removal writes only with one.
			"digest": "digest-0",
		},
	})
	err := DeleteParkedOwnedDisk(context.Background(), c, nil, "pve1", 90000, "data:vm-90000-disk-2", transferStableID, transferTestCfg)
	if err != nil {
		t.Fatalf("DeleteParkedOwnedDisk: %v", err)
	}
	if len(c.destroyed) != 1 || c.destroyed[0] != "data:vm-90000-disk-2" {
		t.Errorf("destroyed = %v, want exactly the parked volume", c.destroyed)
	}
	if len(c.parkedEntries(t)) != 0 {
		t.Errorf("provenance entries remain: %+v", c.parkedEntries(t))
	}
	if prot, _ := c.configs[90000][paramProtection].(bool); !prot {
		t.Error("protection not restored after the delete")
	}
}

func TestDeleteParkedOwnedDisk_RefusesForeignNamedVolume(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{90000: {cfgKeyTags: "bosh-parker"}})
	err := DeleteParkedOwnedDisk(context.Background(), c, nil, "pve1", 90000, "data:vm-9001-disk-0", transferStableID, transferTestCfg)
	if err == nil || !strings.Contains(err.Error(), "not named for parker") {
		t.Fatalf("err = %v, want the not-owner-named refusal", err)
	}
}

func TestUnparkAtLocked_RefusesParkerNamedVolume(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		90000: {
			cfgKeyTags: "bosh-cpi;bosh-parker",
			"scsi1":    "data:vm-90000-disk-2,serial=" + transferStableID,
		},
	})
	holder := DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi1"}
	err := UnparkDiskAt(context.Background(), c, nil, "data:vm-90000-disk-2", holder, transferTestCfg)
	if err == nil || !strings.Contains(err.Error(), "reassignment") {
		t.Fatalf("err = %v, want the config-edit refusal for a parker-owned volume", err)
	}
	// The refusal must fire before anything was mutated.
	if _, present := c.configs[90000]["scsi1"]; !present {
		t.Error("the slot was touched despite the refusal")
	}
	if len(c.destroyed) != 0 {
		t.Errorf("volume destroyed: %v", c.destroyed)
	}
}

func TestAttachToParkerLocked_BakesSerialIntoVolidArg(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		90000: {cfgKeyTags: "bosh-cpi;bosh-parker"},
	})
	slot, err := attachToParkerLocked(context.Background(), c, nil, "pve1", 90000, "data:vm-9001-disk-0", transferStableID)
	if err != nil {
		t.Fatalf("attachToParkerLocked: %v", err)
	}
	if slot != "scsi0" {
		t.Errorf("slot = %q", slot)
	}
	got, _ := c.configs[90000]["scsi0"].(string)
	if got != "data:vm-9001-disk-0,serial="+transferStableID {
		t.Errorf("parker slot = %q, want the serial baked into the attach", got)
	}
}
