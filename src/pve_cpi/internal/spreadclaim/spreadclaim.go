// Package spreadclaim records the nodes that running create_vm calls have
// picked for the members of an instance group.
//
// The soft anti-affinity spread counts a group's members on each node from the
// member tag in /cluster/resources. A create stamps that tag only when its clone
// is configured, a few seconds or more after it scored the nodes, and the
// Director creates a group's VMs in parallel, so creates that score at the same
// time see none of each other and can all pick the same node. Each create
// therefore claims the node it picks in this record, and every create counts
// the live claims of its group on top of the tagged members. A create keeps
// its claim until it returns, because a sibling can read the tags before the
// create's tag lands and the claims after. Once the create has allocated its
// VM, the claim names the VMID, and a sibling that already counts that VMID
// from its tag leaves the claim out, so the member counts once.
//
// A claim whose create crashed lapses at that create's deadline. On a host
// that reports its boot identity, as Linux does, a claim made in the same
// boot and PID namespace also stops counting as soon as the process that made
// it is gone. Every other claim, including one from another boot, which may
// be another host that shares the cache directory, lapses at its expiry.
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
	// VMID is the VM the create allocated, or 0 before it has one.
	VMID int `json:"vmid,omitempty"`
	// PID and Boot name the process that made the claim. Boot is the host's
	// boot identity, which is empty where the host does not report one.
	PID  int    `json:"pid,omitempty"`
	Boot string `json:"boot,omitempty"`
}

// Errors that Pick, Move, Bind, and Release return, which errors.Is matches.
var (
	// ErrUnavailable means the record could not be used at all, because its
	// directory is unusable or another create's turn did not end in time.
	// Nothing was counted or recorded.
	ErrUnavailable = errors.New("the spread claim record is not available")

	// ErrRewritten means the record could not be read, so it was rewritten
	// from an empty one. The claims it held are lost.
	ErrRewritten = errors.New("the spread claim record could not be read and was rewritten")

	// ErrNotRecorded means the claims were read but the write of this claim
	// failed, so siblings do not see it.
	ErrNotRecorded = errors.New("the spread claim was not recorded")
)

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

	// Process names the running process for the claims it makes. Nil means
	// this process and the host's boot identity.
	Process func() (pid int, boot string)

	// Alive reports whether a process of this host and boot still runs. Nil
	// asks the operating system.
	Alive func(pid int) bool

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
	vmid    int
	// recorded is true while the record may hold this claim, so a Release
	// that has nothing to remove never waits for the record.
	recorded bool
	// current is true while the record holds this claim on node with vmid,
	// as the last write left it. A claim whose write failed is not current,
	// so the next Move or Bind writes it again.
	current bool
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

// Holds reports whether the record holds this claim on node, as far as the
// claim's last write knows. A claim whose last write failed holds nothing, so
// a caller that would skip a Move to the node it already claimed moves again.
func (c *Claim) Holds(node string) bool {
	return c != nil && node != "" && c.node == node && c.current
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
	return c.PickExcept(ctx, nil, choose)
}

// PickExcept is Pick, except that a claim whose VMID tagged reports leaves the
// counts. The caller passes the VMIDs it already counts from their member tag,
// so a sibling whose VM carries the tag counts once. A nil tagged counts every
// live claim.
//
// The error matches ErrUnavailable when choose ran without the record's
// claims, ErrRewritten when the record was damaged and rewritten, and
// ErrNotRecorded when choose saw the claims but this claim's write failed.
func (c *Claim) PickExcept(ctx context.Context, tagged func(vmid int) bool,
	choose func(counts map[string]int) string) (string, error) {
	if c == nil {
		return choose(nil), nil
	}
	var node string
	counted := false
	wrote, err := c.store.withRecord(ctx, func(rec *record, now time.Time) bool {
		counted = true
		node = choose(c.counts(rec, now, tagged))
		if node == "" {
			return false
		}
		c.put(rec, node, 0)
		return true
	})
	if !counted {
		node = choose(nil)
	}
	if node != "" {
		c.node, c.vmid = node, 0
		c.recorded = c.recorded || wrote
		c.current = wrote
	}
	return node, err
}

// Move records this claim on node, as a create does when it tries another
// node after an attempt failed on the one it picked. A claim moved to another
// node names no VMID until Bind names the VM of the new attempt, and one moved
// to the node it already claims keeps its VMID. Every Move writes the claim,
// so a Move retries a write that failed before.
func (c *Claim) Move(ctx context.Context, node string) error {
	if c == nil || node == "" {
		return nil
	}
	if node != c.node {
		c.vmid = 0
	}
	c.node = node
	vmid := c.vmid
	wrote, err := c.store.withRecord(ctx, func(rec *record, _ time.Time) bool {
		c.put(rec, node, vmid)
		return true
	})
	c.recorded = c.recorded || wrote
	c.current = wrote
	return err
}

// Bind names vmid as the VM this claim's create allocated on the node it
// holds. It writes the claim whenever the record does not hold it on that node
// with vmid, so a claim whose earlier write failed, or whose entry a rewrite
// of the record lost, is recorded now.
func (c *Claim) Bind(ctx context.Context, vmid int) error {
	if c == nil || c.node == "" || vmid <= 0 {
		return nil
	}
	c.vmid = vmid
	same := false
	wrote, err := c.store.withRecord(ctx, func(rec *record, _ time.Time) bool {
		if e, ok := rec.Claims[c.id]; ok && e.Node == c.node && e.VMID == vmid {
			same = true
			return false
		}
		c.put(rec, c.node, vmid)
		return true
	})
	c.recorded = c.recorded || wrote
	c.current = wrote || same
	return err
}

// VMID returns the VMID Bind named, or 0.
func (c *Claim) VMID() int {
	if c == nil {
		return 0
	}
	return c.vmid
}

// Release removes this claim from the record. A claim that holds no node, or
// that no write ever recorded, touches nothing, so calling it again is free
// and a record whose lock is stuck costs no second wait.
func (c *Claim) Release(ctx context.Context) error {
	if c == nil || c.node == "" {
		return nil
	}
	c.node, c.vmid, c.current = "", 0, false
	if !c.recorded {
		return nil
	}
	c.recorded = false
	_, err := c.store.withRecord(ctx, func(rec *record, _ time.Time) bool {
		if _, ok := rec.Claims[c.id]; !ok {
			return false
		}
		delete(rec.Claims, c.id)
		return true
	})
	return err
}

// counts returns the live claims of the group per node in the store's scope,
// leaving out this claim and every claim whose VMID tagged reports.
func (c *Claim) counts(rec *record, now time.Time, tagged func(vmid int) bool) map[string]int {
	out := map[string]int{}
	for id, e := range rec.Claims {
		if id == c.id || e.Scope != c.store.Scope || e.Group != c.group || !c.store.counts(e, now) {
			continue
		}
		if e.VMID > 0 && tagged != nil && tagged(e.VMID) {
			continue
		}
		out[e.Node]++
	}
	return out
}

func (c *Claim) put(rec *record, node string, vmid int) {
	pid, boot := c.store.process()
	rec.Claims[c.id] = entry{Scope: c.store.Scope, Group: c.group, Node: node, Expires: c.expires.Unix(),
		VMID: vmid, PID: pid, Boot: boot}
}

// counts reports whether a claim still counts at now. It must be live, and
// the process that made it must not be known to be gone.
func (s *Store) counts(e entry, now time.Time) bool {
	return live(e, now) && !s.gone(e)
}

// live reports whether a claim is within its lifetime at now.
func live(e entry, now time.Time) bool {
	exp := time.Unix(e.Expires, 0)
	return e.Node != "" && exp.After(now) && !exp.After(now.Add(MaxClaimLifetime+endpointcache.FutureSkew))
}

// gone reports whether the process that made e is known to have ended. That
// is only so when e was made in this boot and PID namespace by a process that
// no longer runs. A claim from another boot may come from another host that
// shares the cache directory, whose processes this host cannot see, so like a
// claim from another PID namespace, or one without a boot identity on either
// side, it lapses at its expiry.
func (s *Store) gone(e entry) bool {
	if e.PID <= 0 || e.Boot == "" {
		return false
	}
	_, boot := s.process()
	if boot == "" || e.Boot != boot {
		return false
	}
	return !s.alive(e.PID)
}

func (s *Store) process() (int, string) {
	if s.Process != nil {
		return s.Process()
	}
	return os.Getpid(), hostBootIdentity()
}

func (s *Store) alive(pid int) bool {
	if s.Alive != nil {
		return s.Alive(pid)
	}
	return processAlive(pid)
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
// when change reports a change, and it reports whether that write landed.
// Claims that no longer count are pruned on every write. A record that cannot
// be read is rewritten from an empty one, and the read's error is returned
// with any write error. change never runs when the error matches
// ErrUnavailable.
func (s *Store) withRecord(ctx context.Context, change func(rec *record, now time.Time) bool) (bool, error) {
	choice := s.resolve()
	if !choice.Usable() {
		return false, fmt.Errorf("spread claim: %w: %w", ErrUnavailable, choice.Err)
	}
	root := choice.Root
	defer func() { _ = root.Close() }()

	lock, err := endpointcache.LockFile(ctx, root, LockFileName, Name, s.lockWait())
	if err != nil {
		return false, fmt.Errorf("spread claim: %w: %w", ErrUnavailable, err)
	}
	defer func() { _ = lock.Close() }()

	rec, readErr := readRecord(root)
	if readErr != nil {
		rec = record{}
		if errors.Is(readErr, fs.ErrNotExist) {
			readErr = nil
		} else {
			readErr = fmt.Errorf("%w: %w", ErrRewritten, readErr)
		}
	}
	if rec.Claims == nil {
		rec.Claims = map[string]entry{}
	}
	now := s.now()
	if !change(&rec, now) {
		return false, readErr
	}
	s.prune(&rec, now)
	writeErr := policy.WriteEnvelope(root, FileName, recordVersion, rec, privatefile.NoSyncOps())
	if writeErr != nil {
		writeErr = fmt.Errorf("spread claim: %w: write %s: %w", ErrNotRecorded, FileName, writeErr)
	}
	return writeErr == nil, errors.Join(readErr, writeErr)
}

// prune drops the claims that no longer count, and then the claims that
// expire first while the record holds more than maxClaims.
func (s *Store) prune(rec *record, now time.Time) {
	for id, e := range rec.Claims {
		if !s.counts(e, now) {
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
