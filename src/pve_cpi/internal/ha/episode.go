package ha

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

// The recovery episode record lives in the endpoint cache directory, next to
// the HA profile cache, and follows pve.endpoint_cache_dir when an operator
// sets one. Only the time the CPI first saw each episode is kept, so a lost
// record costs nothing but a restarted grace window.
const (
	// EpisodeFileName is the name of the record in the cache directory.
	EpisodeFileName = "ha-recovery-episodes.json"

	// EpisodeName is what the record's directory refusals call it, so an
	// operator reading one does not look at the endpoint cache's settings.
	EpisodeName = "HA episode record"

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
// unsafe or unreadable, and a record that does not verify, all read as no
// record, so the clock starts at the current observation. That only delays a
// handover, which is the safe direction. Concurrent writers may lose each
// other's updates for the same reason: the loser's clock restarts, and nothing
// else changes.
//
// A pve.endpoint_cache_dir that does not exist is the one refusal that does
// not end the search. A record that could never be written would keep the
// clock at zero on every call and so remove the handover rather than delay it,
// so the store falls back to the layers after the override, logs a Warn line
// that names the missing directory, and records the episode there. ForCall
// makes that one line per call however many times the call observes. An
// override that exists but fails the private checks still gives no record.
type EpisodeStore struct {
	// Dir says where to look for the cache directory, and comes from
	// endpointcache.OptionsFor. The record sits in the same directory as the
	// endpoint cache and the HA profile cache.
	Dir endpointcache.DirOptions

	// Log receives the Warn line for a missing override directory. Nil logs
	// nothing.
	Log *log.Logger

	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	// warnOnce, when it is set, lets the missing-directory Warn through once.
	// ForCall sets it.
	warnOnce *sync.Once
}

// ForCall returns a copy of the store that logs the missing-directory Warn at
// most once however many observations it serves. One CPI call can observe an
// episode twice, such as the delete gate and the dead-node completion that
// follows it, and the operator reads one line about the directory. The copy
// shares nothing else with the store, and a call builds its own.
func (s *EpisodeStore) ForCall() *EpisodeStore {
	if s == nil {
		return &EpisodeStore{warnOnce: new(sync.Once)}
	}
	call := *s
	call.warnOnce = new(sync.Once)
	return &call
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

func (s *EpisodeStore) dir() endpointcache.DirOptions {
	if s == nil {
		return endpointcache.DirOptions{}
	}
	return s.Dir.Named(EpisodeName)
}

// resolve chooses the directory of the record. A missing override directory,
// and nothing else that refuses it, falls back to the layers after the
// override with a Warn line, which says whether the fallback gave a directory.
func (s *EpisodeStore) resolve() endpointcache.Choice {
	dir := s.dir()
	choice := dir.Resolve()
	if choice.Usable() || choice.Layer != endpointcache.LayerOverride || !errors.Is(choice.Err, fs.ErrNotExist) {
		return choice
	}
	fallback := dir.WithoutOverride().Resolve()
	s.warnMissingOverride(choice, fallback)
	return fallback
}

// warnMissingOverride logs the Warn line for a missing override directory. The
// line promises the fallback only when the fallback gave a directory, and it
// names the layer that answered, because the fallback is the directory beside
// the allocation journal when that exists and the temporary directory
// otherwise.
func (s *EpisodeStore) warnMissingOverride(missing, fallback endpointcache.Choice) {
	if s == nil || s.Log == nil {
		return
	}
	where := fallbackDescription(fallback.Layer)
	warn := func() {
		if fallback.Usable() {
			s.Log.Warn("ha episode: pve.endpoint_cache_dir does not exist, so the recovery episode record uses "+
				where+" instead; create the directory so the record sits beside the other cache files",
				log.String("path", missing.Path), log.Err(missing.Err), log.String("fallback", fallback.Path))
			return
		}
		s.Log.Warn("ha episode: pve.endpoint_cache_dir does not exist, and "+where+" cannot be used either, "+
			"so the recovery episode has no record; create the directory",
			log.String("path", missing.Path), log.Err(missing.Err), log.String("fallback", fallback.Path),
			log.String("fallback_error", fallbackError(fallback)))
	}
	if s.warnOnce != nil {
		s.warnOnce.Do(warn)
		return
	}
	warn()
}

// fallbackDescription says in words which directory a layer after the override
// stands for.
func fallbackDescription(layer endpointcache.Layer) string {
	switch layer {
	case endpointcache.LayerJournal:
		return "the cache directory beside the allocation journal directory"
	case endpointcache.LayerTemporary:
		return "the temporary cache directory"
	default:
		return "the fallback cache directory"
	}
}

// fallbackError is the text of the refusal that ended the fallback.
func fallbackError(fallback endpointcache.Choice) string {
	if fallback.Err == nil {
		return ""
	}
	return fallback.Err.Error()
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
	choice := s.resolve()
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
