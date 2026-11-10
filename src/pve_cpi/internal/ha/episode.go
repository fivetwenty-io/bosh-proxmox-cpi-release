package ha

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

// The recovery episode record lives in the endpoint cache directory, next to
// the HA profile cache, and follows pve.endpoint_cache_dir when an operator
// sets one. Only the time the CPI first saw each episode is kept, so a lost
// record costs nothing but a restarted grace window.
const (
	// EpisodeFileName is the name of the record in the cache directory.
	EpisodeFileName = "ha-recovery-episodes.json"

	// episodeRecordVersion is the version of the record's envelope.
	episodeRecordVersion = 1

	// episodeMaxBytes bounds the record a read accepts and a write produces. A
	// record that large is not one the CPI wrote, so it is read as lost.
	episodeMaxBytes = 1 << 20

	// episodeRetention is how long an episode stays in the record after the
	// CPI first saw it. A recovery that has lasted longer than this is long
	// past any grace window, so pruning it changes no decision.
	episodeRetention = 7 * 24 * time.Hour
)

// episodePolicy reads and writes the record through the private-file helpers.
// The envelope carries a plain SHA-256 of the payload, which catches a torn or
// damaged file. Nothing is synced, because a record a crash tears reads as lost
// and costs only a restarted grace window.
var episodePolicy = privatefile.Policy{
	Label:        "ha episode",
	MaxBytes:     episodeMaxBytes,
	IntegrityKey: "sha256",
	Integrity:    privatefile.SHA256Hex,
}

// EpisodeStore records when the CPI first observed each HA recovery episode.
// The CRM gives a service a new uid on every state change, so the pair of the
// service ID and its uid while it is in recovery names one episode. The store
// keeps one entry per service ID, and a uid that differs from the recorded one
// replaces the entry, which starts the episode's clock again.
//
// The record is a private file in the endpoint cache directory, which
// endpointcache.Resolve chooses and checks. A directory or file that is
// missing, unsafe, or unreadable, and a record that does not verify, all read
// as no record, so the clock starts at the current observation. That only
// delays a handover, which is the safe direction. Concurrent writers may lose
// each other's updates for the same reason: the loser's clock restarts, and
// nothing else changes.
type EpisodeStore struct {
	// Override is pve.endpoint_cache_dir, empty when it is unset. The record
	// sits in the same directory as the endpoint cache and the HA profile cache.
	Override string

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Episode is what the store answers for one observation.
type Episode struct {
	// SID and UID name the episode.
	SID string
	UID string

	// FirstSeen is when the CPI first observed the episode, and Now is the
	// time of this observation, both on the store's clock.
	FirstSeen time.Time
	Now       time.Time

	// Recorded is true when the episode was already in the record, so the
	// clock did not start at this observation.
	Recorded bool
}

// Elapsed returns how long the CPI has observed the episode.
func (e Episode) Elapsed() time.Duration {
	if e.Now.Before(e.FirstSeen) {
		return 0
	}
	return e.Now.Sub(e.FirstSeen)
}

type episodeRecord struct {
	Episodes map[string]episodeEntry `json:"episodes"`
}

type episodeEntry struct {
	UID       string `json:"uid"`
	FirstSeen int64  `json:"first_seen"`
}

func (s *EpisodeStore) override() string {
	if s == nil {
		return ""
	}
	return s.Override
}

func (s *EpisodeStore) now() time.Time {
	if s == nil || s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// Observe returns the episode for sid and uid, recording the current time as
// its first observation when the record does not already hold this pair. The
// episode is always usable. The error says why the record could not be read or
// written, in which case the clock of the returned episode starts now, and the
// caller logs it.
func (s *EpisodeStore) Observe(sid, uid string) (Episode, error) {
	now := s.now()
	ep := Episode{SID: sid, UID: uid, FirstSeen: now, Now: now}
	if strings.TrimSpace(sid) == "" || strings.TrimSpace(uid) == "" {
		return ep, errors.New("ha episode: the service ID and uid must not be empty")
	}
	choice := endpointcache.Resolve(endpointcache.Options{Override: s.override()})
	if !choice.Usable() {
		return ep, fmt.Errorf("ha episode: %w", choice.Err)
	}
	root := choice.Root
	defer func() { _ = root.Close() }()

	rec, readErr := readEpisodeRecord(root)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		// The record is unusable, so it is rewritten from this observation.
		rec = episodeRecord{}
	}
	if rec.Episodes == nil {
		rec.Episodes = map[string]episodeEntry{}
	}
	if entry, ok := rec.Episodes[sid]; ok && entry.UID == uid && entry.FirstSeen > 0 {
		// A first observation in the future is not one this clock made, so
		// the episode starts again rather than trusting it.
		if first := time.Unix(entry.FirstSeen, 0); !first.After(now) {
			ep.FirstSeen = first
			ep.Recorded = true
			return ep, nil
		}
	}

	rec.Episodes[sid] = episodeEntry{UID: uid, FirstSeen: now.Unix()}
	for key, entry := range rec.Episodes {
		if now.Sub(time.Unix(entry.FirstSeen, 0)) > episodeRetention {
			delete(rec.Episodes, key)
		}
	}
	writeErr := episodePolicy.WriteEnvelope(root, EpisodeFileName, episodeRecordVersion, rec, privatefile.NoSyncOps())
	if writeErr != nil {
		writeErr = fmt.Errorf("ha episode: write %s: %w", EpisodeFileName, writeErr)
	}
	switch {
	case readErr != nil && !errors.Is(readErr, fs.ErrNotExist):
		return ep, errors.Join(readErr, writeErr)
	case writeErr != nil:
		return ep, writeErr
	}
	return ep, nil
}

// readEpisodeRecord reads the record through the private-file checks. A missing
// file returns an error that matches fs.ErrNotExist, and a file that is
// oversized, damaged, or of another version returns one that matches
// privatefile.ErrCorrupt.
func readEpisodeRecord(root *os.Root) (episodeRecord, error) {
	env, err := episodePolicy.ReadEnvelope(root, EpisodeFileName)
	if err != nil {
		return episodeRecord{}, fmt.Errorf("ha episode: read %s: %w", EpisodeFileName, err)
	}
	var rec episodeRecord
	if err := episodePolicy.DecodeEnvelope(env, episodeRecordVersion, &rec); err != nil {
		return episodeRecord{}, fmt.Errorf("ha episode: %s: %w", EpisodeFileName, err)
	}
	return rec, nil
}
