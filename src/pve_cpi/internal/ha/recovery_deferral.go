package ha

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

// The recovery deferral record sits beside the recovery episode record, in the
// same cache directory, under the same scoped keys. It keeps, for each HA
// service, the last time a delete_vm deferred because the service was in fence,
// on a dead node, or in recovery. A delete that later finds the service started
// again reads it to tell a VM that PVE HA has just restarted after an outage
// from a VM that has simply been started. The record is a file of its own, so
// the episode record's format stays the one earlier releases read.
const (
	// RecoveryDeferralFileName is the name of the record in the cache directory.
	RecoveryDeferralFileName = "ha-recovery-deferrals.json"

	// recoveryDeferralLockName is the lock file that serializes the changes to
	// the record across processes. Its name carries no key digest, so the cache
	// directory's sweep never removes it.
	recoveryDeferralLockName = "ha-recovery-deferrals.lock"

	// RecoveryDeferralName is what a lock refusal of the record calls it.
	RecoveryDeferralName = "HA recovery deferral record"

	// recoveryDeferralRecordVersion is the version of the record's envelope.
	recoveryDeferralRecordVersion = 1

	// recoveryDeferralRetention is how long a deferral stays in the record. The
	// delete reads a deferral for minutes, so pruning one a day old changes no
	// decision and keeps the file small.
	recoveryDeferralRetention = 24 * time.Hour
)

// recoveryDeferralPolicy reads and writes the record through the private-file
// helpers, as episodePolicy does for the episode record. A record that a crash
// tears reads as lost, and a lost record only lets a delete proceed as it would
// have before the record existed.
var recoveryDeferralPolicy = privatefile.Policy{
	Label:        "ha recovery deferral",
	MaxBytes:     episodeMaxBytes,
	IntegrityKey: "sha256",
	Integrity:    privatefile.SHA256Hex,
}

type recoveryDeferralRecord struct {
	Deferrals map[string]int64 `json:"deferrals"`
}

// RecoveryDeferral is what the store answers when it holds a deferral for a
// service.
type RecoveryDeferral struct {
	// SID is the HA service ID.
	SID string

	// At is the time of the last recorded deferral, and Now is the time of this
	// read, both on the store's clock.
	At  time.Time
	Now time.Time
}

// Age returns how long ago the last deferral was recorded. A deferral that sits
// a moment ahead of the clock, within the trusted skew, reads as age zero.
func (d RecoveryDeferral) Age() time.Duration {
	if d.Now.Before(d.At) {
		return 0
	}
	return d.Now.Sub(d.At)
}

// RecordRecoveryDeferral records the current time as the last deferral of sid.
// The change is made under the record's lock, and it also prunes deferrals
// older than the retention and any that no clock of this host could have made.
// The error says why the record could not be read or written, and the caller
// logs it.
func (s *EpisodeStore) RecordRecoveryDeferral(sid string) error {
	if strings.TrimSpace(sid) == "" {
		return errors.New("ha recovery deferral: the service ID must not be empty")
	}
	return s.changeRecoveryDeferrals(func(rec *recoveryDeferralRecord, key string, now time.Time) bool {
		rec.Deferrals[key] = now.Unix()
		return true
	}, sid)
}

// LastRecoveryDeferral returns the last recorded deferral of sid, and false when
// the record holds none that can be trusted. It reads without the lock, which
// the atomic replacement of the record makes safe. A missing record is not an
// error. A record that cannot be read or does not verify is, and the caller
// then treats the service as having no deferral.
func (s *EpisodeStore) LastRecoveryDeferral(sid string) (RecoveryDeferral, bool, error) {
	now := s.now()
	out := RecoveryDeferral{SID: sid, Now: now}
	if strings.TrimSpace(sid) == "" {
		return out, false, errors.New("ha recovery deferral: the service ID must not be empty")
	}
	choice := s.resolve()
	if !choice.Usable() {
		return out, false, fmt.Errorf("ha recovery deferral: %w", choice.Err)
	}
	root := choice.Root
	defer func() { _ = root.Close() }()

	rec, err := readRecoveryDeferralRecord(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return out, false, nil
	case err != nil:
		return out, false, err
	}
	at, ok := trustedDeferral(rec, s.key(sid), now)
	if !ok {
		return out, false, nil
	}
	out.At = at
	return out, true, nil
}

// ClearRecoveryDeferral removes the deferral of sid from the record. A record
// that holds none needs no change, so the call returns without taking the
// lock.
func (s *EpisodeStore) ClearRecoveryDeferral(sid string) error {
	if strings.TrimSpace(sid) == "" {
		return errors.New("ha recovery deferral: the service ID must not be empty")
	}
	choice := s.resolve()
	if !choice.Usable() {
		return fmt.Errorf("ha recovery deferral: %w", choice.Err)
	}
	rec, err := readRecoveryDeferralRecord(choice.Root)
	_ = choice.Root.Close()
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err == nil {
		if _, held := rec.Deferrals[s.key(sid)]; !held {
			return nil
		}
	}
	return s.changeRecoveryDeferrals(func(rec *recoveryDeferralRecord, key string, _ time.Time) bool {
		if _, held := rec.Deferrals[key]; !held {
			return false
		}
		delete(rec.Deferrals, key)
		return true
	}, sid)
}

// changeRecoveryDeferrals applies change to the record under its lock, prunes
// it, and writes it when change reports a change or the prune removed an
// entry. A record that cannot be read is rewritten from an empty one.
func (s *EpisodeStore) changeRecoveryDeferrals(
	change func(rec *recoveryDeferralRecord, key string, now time.Time) bool, sid string,
) error {
	choice := s.resolve()
	if !choice.Usable() {
		return fmt.Errorf("ha recovery deferral: %w", choice.Err)
	}
	root := choice.Root
	defer func() { _ = root.Close() }()

	lock, err := endpointcache.LockFile(
		context.Background(), root, recoveryDeferralLockName, RecoveryDeferralName, episodeLockWait)
	if err != nil {
		return fmt.Errorf("ha recovery deferral: %w", err)
	}
	defer func() { _ = lock.Close() }()

	now := s.now()
	rec, readErr := readRecoveryDeferralRecord(root)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		rec = recoveryDeferralRecord{}
	}
	if rec.Deferrals == nil {
		rec.Deferrals = map[string]int64{}
	}
	changed := change(&rec, s.key(sid), now)
	for name, unix := range rec.Deferrals {
		at := time.Unix(unix, 0)
		if unix <= 0 || firstSeenAhead(at, now) || now.Sub(at) > recoveryDeferralRetention {
			delete(rec.Deferrals, name)
			changed = true
		}
	}
	if !changed && (readErr == nil || errors.Is(readErr, fs.ErrNotExist)) {
		return nil
	}
	if s.beforeWrite != nil {
		s.beforeWrite()
	}
	writeErr := recoveryDeferralPolicy.WriteEnvelope(
		root, RecoveryDeferralFileName, recoveryDeferralRecordVersion, rec, privatefile.NoSyncOps())
	if writeErr != nil {
		writeErr = fmt.Errorf("ha recovery deferral: write %s: %w", RecoveryDeferralFileName, writeErr)
	}
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return errors.Join(readErr, writeErr)
	}
	return writeErr
}

// trustedDeferral returns the deferral the record holds for key, when it holds
// one that can be trusted at now. One further in the future than the skew
// allows is not one this clock made.
func trustedDeferral(rec recoveryDeferralRecord, key string, now time.Time) (time.Time, bool) {
	unix, ok := rec.Deferrals[key]
	if !ok || unix <= 0 {
		return time.Time{}, false
	}
	at := time.Unix(unix, 0)
	if firstSeenAhead(at, now) {
		return time.Time{}, false
	}
	return at, true
}

// readRecoveryDeferralRecord reads the record through the private-file checks.
// A missing file returns an error that matches fs.ErrNotExist, and a file that
// is oversized, damaged, or of another version returns one that matches
// privatefile.ErrCorrupt.
func readRecoveryDeferralRecord(root *os.Root) (recoveryDeferralRecord, error) {
	env, err := recoveryDeferralPolicy.ReadEnvelope(root, RecoveryDeferralFileName)
	if err != nil {
		return recoveryDeferralRecord{}, fmt.Errorf("ha recovery deferral: read %s: %w", RecoveryDeferralFileName, err)
	}
	var rec recoveryDeferralRecord
	if err := recoveryDeferralPolicy.DecodeEnvelope(env, recoveryDeferralRecordVersion, &rec); err != nil {
		return recoveryDeferralRecord{}, fmt.Errorf("ha recovery deferral: %s: %w", RecoveryDeferralFileName, err)
	}
	return rec, nil
}
