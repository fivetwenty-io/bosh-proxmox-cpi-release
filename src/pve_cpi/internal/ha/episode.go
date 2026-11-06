package ha

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The recovery episode record lives in the endpoint cache directory, next to
// the HA profile cache. Only the time the CPI first saw each episode is kept,
// so a lost record costs nothing but a restarted grace window.
const (
	// EpisodeFileName is the name of the record in the cache directory.
	EpisodeFileName = "ha-recovery-episodes.json"

	// episodeRecordVersion is the version of the record's JSON shape.
	episodeRecordVersion = 1

	// episodeMaxBytes bounds the record a read accepts. A record that large
	// is not one the CPI wrote, so it is read as lost.
	episodeMaxBytes = 1 << 20

	// episodeRetention is how long an episode stays in the record after the
	// CPI first saw it. A recovery that has lasted longer than this is long
	// past any grace window, so pruning it changes no decision.
	episodeRetention = 7 * 24 * time.Hour

	// cacheDirPrefix names the endpoint cache directory under the temporary
	// directory, followed by the effective user ID.
	cacheDirPrefix = "pve_cpi-endpoint-cache."
)

// DefaultCacheDir returns the endpoint cache directory,
// $TMPDIR/pve_cpi-endpoint-cache.<euid>.
func DefaultCacheDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s%d", cacheDirPrefix, os.Geteuid()))
}

// EpisodeStore records when the CPI first observed each HA recovery episode.
// The CRM gives a service a new uid on every state change, so the pair of the
// service ID and its uid while it is in recovery names one episode. The store
// keeps one entry per service ID, and a uid that differs from the recorded one
// replaces the entry, which starts the episode's clock again.
//
// The record is a JSON file in a directory that only the effective user may
// use. A directory or file that is missing, unsafe, or unreadable, and a
// record that does not parse, all read as no record, so the clock starts at
// the current observation. That only delays a handover, which is the safe
// direction. Concurrent writers may lose each other's updates for the same
// reason: the loser's clock restarts, and nothing else changes.
type EpisodeStore struct {
	// Dir is the cache directory. Empty means DefaultCacheDir.
	Dir string

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
	Version  int                     `json:"version"`
	Episodes map[string]episodeEntry `json:"episodes"`
}

type episodeEntry struct {
	UID       string `json:"uid"`
	FirstSeen int64  `json:"first_seen"`
}

func (s *EpisodeStore) dir() string {
	if s == nil || s.Dir == "" {
		return DefaultCacheDir()
	}
	return s.Dir
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
	dir := s.dir()
	path := filepath.Join(dir, EpisodeFileName)

	rec, readErr := readEpisodeRecord(dir, path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
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

	rec.Version = episodeRecordVersion
	rec.Episodes[sid] = episodeEntry{UID: uid, FirstSeen: now.Unix()}
	for key, entry := range rec.Episodes {
		if now.Sub(time.Unix(entry.FirstSeen, 0)) > episodeRetention {
			delete(rec.Episodes, key)
		}
	}
	writeErr := writeEpisodeRecord(dir, path, rec)
	switch {
	case readErr != nil && !errors.Is(readErr, os.ErrNotExist):
		return ep, errors.Join(readErr, writeErr)
	case writeErr != nil:
		return ep, writeErr
	}
	return ep, nil
}

// readEpisodeRecord reads the record. A missing file returns an error that
// matches os.ErrNotExist.
func readEpisodeRecord(dir, path string) (episodeRecord, error) {
	if err := checkPrivatePath(dir, true); err != nil {
		return episodeRecord{}, err
	}
	if err := checkPrivatePath(path, false); err != nil {
		return episodeRecord{}, err
	}
	f, err := os.Open(path) // #nosec G304 -- path is checked private to the effective user above
	if err != nil {
		return episodeRecord{}, fmt.Errorf("ha episode: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, episodeMaxBytes+1))
	if err != nil {
		return episodeRecord{}, fmt.Errorf("ha episode: read %s: %w", path, err)
	}
	if len(data) > episodeMaxBytes {
		return episodeRecord{}, fmt.Errorf("ha episode: %s is larger than %d bytes", path, episodeMaxBytes)
	}
	var rec episodeRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return episodeRecord{}, fmt.Errorf("ha episode: %s does not parse: %w", path, err)
	}
	if rec.Version != episodeRecordVersion {
		return episodeRecord{}, fmt.Errorf("ha episode: %s has version %d, want %d", path, rec.Version, episodeRecordVersion)
	}
	return rec, nil
}

// writeEpisodeRecord replaces the record through a private temporary file and
// a rename, creating the directory when it is missing.
func writeEpisodeRecord(dir, path string, rec episodeRecord) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("ha episode: create %s: %w", dir, err)
	}
	if err := checkPrivatePath(dir, true); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("ha episode: encode record: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+EpisodeFileName+".*")
	if err != nil {
		return fmt.Errorf("ha episode: create temporary record in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	_, writeErr := tmp.Write(data)
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("ha episode: write %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("ha episode: replace %s: %w", path, err)
	}
	return nil
}

// checkPrivatePath refuses a path that is a symbolic link, is not of the
// expected type, is owned by another user, or grants any group or other
// access. A missing path returns an error that matches os.ErrNotExist.
func checkPrivatePath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("ha episode: stat %s: %w", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case !ok || int64(st.Uid) != int64(os.Geteuid()):
		return fmt.Errorf("ha episode: %s is not owned by the effective user", path)
	case directory && !info.IsDir():
		return fmt.Errorf("ha episode: %s is not a directory", path)
	case !directory && !info.Mode().IsRegular():
		return fmt.Errorf("ha episode: %s is not a regular file", path)
	case info.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("ha episode: %s grants access beyond its owner (mode %04o)", path, info.Mode().Perm())
	}
	return nil
}
