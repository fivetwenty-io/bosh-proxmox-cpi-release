// Package spreadclaim records the nodes that running create_vm calls have
// picked for the members of an instance group.
//
// The soft anti-affinity spread counts a group's members on each node from the
// member tag in /cluster/resources. A create stamps that tag only when its clone
// is configured, a few seconds or more after it scored the nodes, and the
// Director creates a group's VMs in parallel, so creates that score at the same
// time see none of each other and can all pick the same node. Each create
// therefore claims the node it picks in this record, and every create counts
// the live claims of its group on top of the tagged members. A create gives up
// its claim once its clone is configured and the tag carries the count.
//
// All of a Director's CPI processes run on one host, and a group's tag names
// its Director, so a record in the CPI's cache directory reaches every create
// that can be a sibling.
package spreadclaim

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

const (
	// FileName is the record's name in the cache directory.
	FileName = "spread-claims.json"

	// LockFileName is the lock that serializes every read and change of the
	// record across processes. The cache directory's sweep only removes names
	// that carry a key digest, so it never removes this one or the record.
	LockFileName = "spread-claims.lock"

	// Name is what the record's directory refusals and log lines call it.
	Name = "spread claim record"

	// DefaultLockWait bounds how long a create waits for another create's
	// turn with the record. A turn is one small read and write, so a wait that
	// runs out means the holder is stuck, and the create picks without the
	// record.
	DefaultLockWait = 5 * time.Second

	// MaxClaimLifetime is the longest a claim may live. A claim that expires
	// further ahead than this is not one a create made, so it is dropped.
	MaxClaimLifetime = 24 * time.Hour

	// recordVersion is the version of the record's envelope.
	recordVersion = 1

	// maxBytes bounds the record a read accepts and a write produces.
	maxBytes = 1 << 20

	// maxClaims bounds the claims the record keeps. Past it the claims that
	// expire first are dropped, which only weakens the spread they describe.
	maxClaims = 2048
)

// policy reads and writes the record through the private-file helpers. The
// envelope carries a plain SHA-256 of the payload, which catches a torn or
// damaged file. Nothing is synced, because a record that a crash loses costs
// only the spread of the creates that were running.
var policy = privatefile.Policy{
	Label:        "spread claim",
	MaxBytes:     maxBytes,
	IntegrityKey: "sha256",
	Integrity:    privatefile.SHA256Hex,
}

type record struct {
	Claims map[string]entry `json:"claims"`
}

type entry struct {
	Scope   string `json:"scope"`
	Group   string `json:"group"`
	Node    string `json:"node"`
	Expires int64  `json:"expires_unix"`
}

// Store is the record of one cluster entry's claims. The zero value uses the
// default cache directory, an empty scope, and the system clock.
type Store struct {
	// Dir says where to look for the cache directory, and comes from
	// endpointcache.OptionsFor. The record sits beside the endpoint cache and
	// the HA records.
	Dir endpointcache.DirOptions

	// Scope names the cluster entry, such as ha.EpisodeScope gives. Two
	// clusters can both have a node called pve1, so a claim only counts for
	// creates in the same scope.
	Scope string

	// Log receives the Warn line for a missing override directory. Nil logs
	// nothing.
	Log *log.Logger

	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	// LockWait overrides DefaultLockWait. Zero means DefaultLockWait.
	LockWait time.Duration

	// warnOnce lets the missing-directory Warn through once per store.
	warnOnce sync.Once
}

// Claim is one create's claim on a node for its group. Nothing is recorded
// until Pick or Move, and Release removes what was recorded.
type Claim struct {
	store   *Store
	id      string
	group   string
	expires time.Time
	node    string
}

// NewClaim returns an unrecorded claim for a create in group that ends at
// expires at the latest.
func (s *Store) NewClaim(group string, expires time.Time) (*Claim, error) {
	if strings.TrimSpace(group) == "" {
		return nil, errors.New("spread claim: the group must not be empty")
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("spread claim: make an id: %w", err)
	}
	return &Claim{store: s, id: hex.EncodeToString(raw[:]), group: group, expires: expires}, nil
}

// Node returns the node the claim holds, or "" when it holds none.
func (c *Claim) Node() string {
	if c == nil {
		return ""
	}
	return c.node
}

// Pick runs choose with the number of live claims each node holds for the
// group, leaving this claim out, and records this claim on the node choose
// returns. The read, the choice, and the write happen in one turn with the
// record, so two creates that pick at the same time each see the other's
// claim. choose returning "" records nothing and keeps an earlier claim.
//
// When the record cannot be used, choose still runs, with the counts read so
// far or none, so the create picks as it would without the record. The error
// says why for the caller to log, and the node is still returned.
func (c *Claim) Pick(ctx context.Context, choose func(counts map[string]int) string) (string, error) {
	if c == nil {
		return choose(nil), nil
	}
	var node string
	err := c.store.withRecord(ctx, func(rec *record, now time.Time) bool {
		node = choose(c.counts(rec, now))
		if node == "" {
			return false
		}
		c.put(rec, node)
		return true
	})
	if err != nil && node == "" {
		node = choose(nil)
	}
	if node != "" {
		c.node = node
	}
	return node, err
}

// Move records this claim on node, as a create does when it tries another
// node after an attempt failed on the one it picked.
func (c *Claim) Move(ctx context.Context, node string) error {
	if c == nil || node == "" {
		return nil
	}
	c.node = node
	return c.store.withRecord(ctx, func(rec *record, _ time.Time) bool {
		c.put(rec, node)
		return true
	})
}

// Release removes this claim from the record. A claim that holds no node
// touches nothing, so calling it again is free.
func (c *Claim) Release(ctx context.Context) error {
	if c == nil || c.node == "" {
		return nil
	}
	c.node = ""
	return c.store.withRecord(ctx, func(rec *record, _ time.Time) bool {
		if _, ok := rec.Claims[c.id]; !ok {
			return false
		}
		delete(rec.Claims, c.id)
		return true
	})
}

// counts returns the live claims of the group per node in the store's scope,
// leaving this claim out.
func (c *Claim) counts(rec *record, now time.Time) map[string]int {
	out := map[string]int{}
	for id, e := range rec.Claims {
		if id == c.id || e.Scope != c.store.Scope || e.Group != c.group || !live(e, now) {
			continue
		}
		out[e.Node]++
	}
	return out
}

func (c *Claim) put(rec *record, node string) {
	rec.Claims[c.id] = entry{Scope: c.store.Scope, Group: c.group, Node: node, Expires: c.expires.Unix()}
}

// live reports whether a claim still counts at now.
func live(e entry, now time.Time) bool {
	exp := time.Unix(e.Expires, 0)
	return e.Node != "" && exp.After(now) && !exp.After(now.Add(MaxClaimLifetime+endpointcache.FutureSkew))
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) lockWait() time.Duration {
	if s.LockWait > 0 {
		return s.LockWait
	}
	return DefaultLockWait
}

// withRecord runs change on the record under its lock and writes the record
// when change reports a change. Claims that no longer count are pruned on
// every write. A record that cannot be read is rewritten from an empty one,
// and the read's error is returned with any write error.
func (s *Store) withRecord(ctx context.Context, change func(rec *record, now time.Time) bool) error {
	choice := s.resolve()
	if !choice.Usable() {
		return fmt.Errorf("spread claim: %w", choice.Err)
	}
	root := choice.Root
	defer func() { _ = root.Close() }()

	lock, err := endpointcache.LockFile(ctx, root, LockFileName, Name, s.lockWait())
	if err != nil {
		return fmt.Errorf("spread claim: %w", err)
	}
	defer func() { _ = lock.Close() }()

	rec, readErr := readRecord(root)
	if readErr != nil {
		rec = record{}
		if errors.Is(readErr, fs.ErrNotExist) {
			readErr = nil
		}
	}
	if rec.Claims == nil {
		rec.Claims = map[string]entry{}
	}
	now := s.now()
	if !change(&rec, now) {
		return readErr
	}
	prune(&rec, now)
	writeErr := policy.WriteEnvelope(root, FileName, recordVersion, rec, privatefile.NoSyncOps())
	if writeErr != nil {
		writeErr = fmt.Errorf("spread claim: write %s: %w", FileName, writeErr)
	}
	return errors.Join(readErr, writeErr)
}

// prune drops the claims that no longer count, and then the claims that
// expire first while the record holds more than maxClaims.
func prune(rec *record, now time.Time) {
	for id, e := range rec.Claims {
		if !live(e, now) {
			delete(rec.Claims, id)
		}
	}
	if len(rec.Claims) <= maxClaims {
		return
	}
	ids := make([]string, 0, len(rec.Claims))
	for id := range rec.Claims {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := rec.Claims[ids[i]], rec.Claims[ids[j]]
		if a.Expires != b.Expires {
			return a.Expires < b.Expires
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids[:len(ids)-maxClaims] {
		delete(rec.Claims, id)
	}
}

// resolve chooses the record's directory. A pve.endpoint_cache_dir that does
// not exist falls back to the layers after it with one Warn line, as the HA
// episode record does, so the spread keeps working while the operator creates
// the directory.
func (s *Store) resolve() endpointcache.Choice {
	dir := s.Dir.Named(Name)
	choice := dir.Resolve()
	if choice.Usable() || choice.Layer != endpointcache.LayerOverride || !errors.Is(choice.Err, fs.ErrNotExist) {
		return choice
	}
	fallback := dir.WithoutOverride().Resolve()
	if s.Log != nil {
		s.warnOnce.Do(func() {
			s.Log.Warn("spread claim: pve.endpoint_cache_dir does not exist, so the spread claim record uses "+
				"the fallback cache directory; create the directory so the record sits beside the other cache files",
				log.String("path", choice.Path), log.Err(choice.Err), log.String("fallback", fallback.Path))
		})
	}
	return fallback
}

// readRecord reads the record through the private-file checks. A missing file
// returns an error that matches fs.ErrNotExist, and a file that is oversized,
// damaged, or of another version returns one that matches
// privatefile.ErrCorrupt.
func readRecord(root *os.Root) (record, error) {
	env, err := policy.ReadEnvelope(root, FileName)
	if err != nil {
		return record{}, fmt.Errorf("spread claim: read %s: %w", FileName, err)
	}
	var rec record
	if err := policy.DecodeEnvelope(env, recordVersion, &rec); err != nil {
		return record{}, fmt.Errorf("spread claim: %s: %w", FileName, err)
	}
	return rec, nil
}
