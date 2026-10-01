package pve

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// provenanceClockKey carries the clock WithProvenanceClock installs.
type provenanceClockKey struct{}

// WithProvenanceClock returns a derived context whose provenance writes stamp
// parked_at from now, and whose collector judges record ages against it. It
// lets a test age a transfer intent through the real handler path instead of
// editing a parker's description by hand.
//
// Production code MUST NOT call this. A ParkerConfig.NowFunc, when set, still
// wins, so the tests that pin the clock that way are unaffected.
func WithProvenanceClock(ctx context.Context, now func() time.Time) context.Context {
	return context.WithValue(ctx, provenanceClockKey{}, now)
}

// provenanceNow returns the time a provenance write stamps and collects
// against: cfg.NowFunc when set, then the context clock, then the wall clock.
func provenanceNow(ctx context.Context, cfg ParkerConfig) time.Time {
	if cfg.NowFunc != nil {
		return cfg.NowFunc()
	}
	if ctx != nil {
		if now, ok := ctx.Value(provenanceClockKey{}).(func() time.Time); ok && now != nil {
			return now().UTC()
		}
	}
	return time.Now().UTC()
}

// provenanceSourceVMID returns the source VM a record names when the record is
// source-bearing: a stable-ID record (only those carry a volid) whose
// source_vm_cid parses as a positive VMID. Every transfer intent a guest's
// detach, delete_vm, or ephemeral retention writes is one, in every release
// that collects records. Other records keep the age-and-reference rule alone.
func provenanceSourceVMID(entry parkerProvEntry) (int, bool) {
	if entry.Volid == "" || entry.SourceVMCID == "" {
		return 0, false
	}
	vmid, err := strconv.Atoi(entry.SourceVMCID)
	if err != nil || vmid <= 0 {
		return 0, false
	}
	return vmid, true
}

// parkerProvenanceSourceKeeps is the one keep rule both the capacity probe and
// the provenance write apply, so the two can never disagree about which
// records survive. It returns the keys of records that would otherwise be
// collected but whose source VM still names the record's volume on any slot,
// active or unused.
//
// That state is a detach-side transfer that deleted the source slot and then
// failed to move the volume. PVE demoted the volume to an unused entry, which
// carries no serial, so the intent on this parker is the only link from the
// disk's stable ID to its volume, and collecting it strands the disk where no
// CID can find it.
//
// Reads run only for source-bearing collection candidates, so a parker with no
// stale intent costs nothing extra. Any read that fails keeps the record: a
// kept record only delays a collection, and a wrongly collected one loses a
// disk.
func parkerProvenanceSourceKeeps(
	ctx context.Context, c Client, logger *log.Logger,
	vmCfg map[string]any, keepKey string, now time.Time,
) map[string]bool {
	_, disks, _ := parseParkerSentinel(DescriptionFromConfig(vmCfg))
	held := map[string]bool{}
	for _, key := range staleParkerProvenanceKeys(disks, vmCfg, keepKey, now) {
		entry := disks[key]
		vmid, ok := provenanceSourceVMID(entry)
		if !ok {
			continue
		}
		names, err := provenanceSourceNamesVolume(ctx, c, entry, vmid)
		if err != nil && logger != nil {
			logger.Warn("parker provenance: could not read the source VM of a stale transfer record; keeping the record",
				log.String("key", key),
				log.Int("source_vmid", vmid),
				log.String("volid", entry.Volid),
				log.Err(err),
			)
		}
		if names || err != nil {
			held[key] = true
		}
	}
	return held
}

// provenanceSourceNamesVolume reports whether the source VM's current config
// names entry.Volid on an active slot or an unused entry. An error means the
// answer is unknown, and the caller keeps the record.
//
// The record's node is where the transfer ran, which is where the source was
// then, not necessarily where it is now. So a config that is gone at that node
// is not yet proof of absence: FindVMAuthoritative either proves the guest is
// gone cluster-wide or finds it on another node, and only proven absence lets
// the record go.
func provenanceSourceNamesVolume(ctx context.Context, c Client, entry parkerProvEntry, vmid int) (bool, error) {
	if entry.Node != "" {
		cfg, err := c.QEMU().Config(ctx, entry.Node, vmid)
		if err == nil {
			return configNamesVolume(cfg, entry.Volid), nil
		}
		if !parkerConfigGone(err) {
			return false, err
		}
	}
	loc, err := FindVMAuthoritative(ctx, c, vmid)
	if err != nil {
		return false, err
	}
	if !loc.Found {
		return false, nil
	}
	cfg, err := c.QEMU().Config(ctx, loc.Node, vmid)
	if err != nil {
		return false, err
	}
	return configNamesVolume(cfg, entry.Volid), nil
}

// configNamesVolume reports whether any active bus slot or unused entry of cfg
// names bareVolid.
func configNamesVolume(cfg map[string]any, bareVolid string) bool {
	for _, optstr := range qemu.ParseDisks(cfg) {
		if bareDriveVolid(optstr) == bareVolid {
			return true
		}
	}
	return unusedEntriesReference(cfg, bareVolid)
}

// staleParkerProvenanceKeys returns, sorted, the keys the age-and-reference
// rule would collect: every record other than keepKey whose volume nothing on
// the parker names and whose parked_at is older than the grace window or does
// not parse. The collector and the keep rule share it, so the source reads run
// for exactly the records the collector would otherwise remove.
func staleParkerProvenanceKeys(disks map[string]parkerProvEntry, vmCfg map[string]any, keepKey string, now time.Time) []string {
	referenced := parkerReferencedVolids(vmCfg)
	var stale []string
	for key := range disks {
		entry := disks[key]
		if key == keepKey {
			continue
		}
		if referenced[provEntryVolid(key, entry)] {
			continue
		}
		if parsed, parseErr := time.Parse(time.RFC3339, entry.ParkedAt); parseErr == nil {
			if now.Sub(parsed) < parkerProvenanceGraceWindow {
				continue
			}
		}
		stale = append(stale, key)
	}
	sort.Strings(stale)
	return stale
}

// heldKeys returns the keys of held, sorted, for a log line.
func heldKeys(held map[string]bool) []string {
	keys := make([]string, 0, len(held))
	for key := range held {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
