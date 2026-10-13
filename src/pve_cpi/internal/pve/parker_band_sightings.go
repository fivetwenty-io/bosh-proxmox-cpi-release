package pve

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// parkerSightingSampleLimit caps how many guests one warning names. The count
// field always carries the full number, so a cluster with hundreds of strays
// still produces one readable log line.
const parkerSightingSampleLimit = 20

// parkerBandSightings collects, from rows a parker listing has already
// decoded, the parkers that say two cpi_config entries on one cluster disagree
// about the parker band or the parker prefix. It never issues a request of its
// own: every decision reads the tags and name the listing carried, so hooking
// it into a listing costs nothing beyond the loop that was already running.
//
// It looks for two shapes.
//
// A parker outside our band that carries our director's attribution tag and
// our prefix is one we, or an entry configured like us, created under a
// different band. Either another entry for this Director uses another
// parked_disk_vmid_range with the same prefix, or the band moved while disks
// were parked. Attribution has to be explicit here, because an unattributed
// parker outside the band could belong to anyone.
//
// A parker inside our band that carries another prefix belongs to an entry
// that shares our band, or it is a leftover from a parker prefix change, and
// the two look the same from here. The entries never adopt each other's
// parkers, but they allocate parker VMIDs from the same range and each one's
// parker accounting treats the other's guests as foreign occupants. Either
// entry can still unpark the other's disks, because unparking is keyed on the
// tag and the band and ignores the prefix.
//
// A row counts only when its prefix is evidenced, by a "vm-prefix--" tag or a
// "<prefix>-parker-<vmid>" name. A parker with neither falls back to the
// default prefix in parkerPrefixIdentity, and warning on a guessed identity
// would accuse every pre-tag parker of belonging to "bosh". Movers are
// included, since they are allocated from the parker band and carry the same
// tags.
type parkerBandSightings struct {
	cfg         ParkerConfig
	directorTag string
	outside     []string
	otherPrefix []string
}

func newParkerBandSightings(cfg ParkerConfig) *parkerBandSightings {
	return &parkerBandSightings{cfg: cfg, directorTag: parkerDirectorTag(cfg.DirectorID)}
}

// observe classifies one listed guest. tags and name are what the listing (or
// the evidence read the listing already made) returned; an empty tag string
// is no evidence and is skipped.
func (s *parkerBandSightings) observe(node string, vmid int, tags, name string) {
	if s == nil || vmid <= 0 || !tagContainsParker(tags) {
		return
	}
	if !tagsCarryParkerPrefix(tags) && !parkerNameRe.MatchString(strings.TrimSpace(name)) {
		return
	}
	ours := parkerBelongsToPrefix(tags, name, s.cfg.Prefix)
	inBand := vmid >= s.cfg.VMIDRangeStart && vmid <= s.cfg.VMIDRangeEnd
	where := strconv.Itoa(vmid)
	if node != "" {
		where = node + "/" + where
	}
	switch {
	case inBand && !ours:
		s.otherPrefix = append(s.otherPrefix, where+"("+parkerPrefixIdentity(tags, name)+")")
	case !inBand && ours && s.attributedToUs(tags):
		s.outside = append(s.outside, where)
	}
}

// attributedToUs reports whether tags carry our director's attribution tag
// explicitly. parkerBelongsToDirector also accepts an unattributed parker,
// which is right for adoption and wrong for this warning.
func (s *parkerBandSightings) attributedToUs(tags string) bool {
	if s.directorTag == "" {
		return false
	}
	for _, t := range splitTagString(tags) {
		if strings.EqualFold(t, s.directorTag) {
			return true
		}
	}
	return false
}

// report logs one warning per shape that was seen. listing names the caller,
// so an operator can tell a detach-path listing from the provenance scan.
//
// The out-of-band rows got no config read, so the warning can't tell whether
// a parker still holds disks. Its advice is therefore conditional: covering
// the parkers with the band again comes first, and removal is only for a
// parker the operator has proved empty.
//
// The warnings come from the per-node listing behind parking, disk transfer,
// and the pool sweep, and from the cluster listing behind the provenance scan.
// FindDiskTransferRecords and FindDiskKeyHolders also read parker tags but
// have no ParkerConfig in scope, so they raise neither warning; threading the
// config through would add nothing those listings don't already report.
func (s *parkerBandSightings) report(ctx context.Context, listing string) {
	if s == nil || (len(s.outside) == 0 && len(s.otherPrefix) == 0) {
		return
	}
	logger := log.FromContext(ctx)
	band := fmt.Sprintf("%d-%d", s.cfg.VMIDRangeStart, s.cfg.VMIDRangeEnd)
	prefix := resolveParkerPrefix(s.cfg.Prefix)
	if len(s.outside) > 0 {
		logger.Warn("parkers carrying this director's tag and parker prefix sit outside the configured parker band; "+
			"another cpi_config entry for this director may use a different parked_disk_vmid_range with the same prefix, "+
			"or the band was moved while disks were parked. This entry refuses to attach, update, or delete a disk parked "+
			"on one of them, so while they hold disks set parked_disk_vmid_range_start/end to cover them again instead of "+
			"removing them. Remove a parker only after confirming it holds no disks, as Recovering empty parker VMs in "+
			"operations.md describes",
			log.String("listing", listing),
			log.String("parker_band", band),
			log.String("parker_prefix", prefix),
			log.Int("count", len(s.outside)),
			log.String("parkers", sampleParkerSightings(s.outside)))
	}
	if len(s.otherPrefix) > 0 {
		logger.Warn("parkers with another parker prefix sit inside this entry's parker band; "+
			"either two cpi_config entries on this cluster share parked_disk_vmid_range and its VMIDs, or these are "+
			"leftovers from a parker prefix change, and the two look the same from inside one entry. Leftover parkers "+
			"drain as the Director attaches or deletes their disks. Never move a band away from parkers that still hold "+
			"disks, because the CPI refuses to attach, update, or delete a disk parked outside the band",
			log.String("listing", listing),
			log.String("parker_band", band),
			log.String("parker_prefix", prefix),
			log.Int("count", len(s.otherPrefix)),
			log.String("parkers", sampleParkerSightings(s.otherPrefix)))
	}
}

func sampleParkerSightings(items []string) string {
	if len(items) <= parkerSightingSampleLimit {
		return strings.Join(items, ",")
	}
	return strings.Join(items[:parkerSightingSampleLimit], ",") +
		fmt.Sprintf(",... (%d more)", len(items)-parkerSightingSampleLimit)
}
