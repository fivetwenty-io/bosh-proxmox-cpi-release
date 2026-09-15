package allocationjournal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// authorityFile is the enrollment record inside a namespace directory. Its
// presence is what separates an enrolled namespace from a directory somebody
// created and never enrolled.
const authorityFile = "authority.json"

// Journal is a namespace-scoped authority. Close only after all Handles close.
// Opening/inspection does not initialize state or claim remote writer ownership.
type Journal struct {
	root *os.Root
	auth authority
	ops  fileOps
}

// EnrollmentStatus is the persisted authority, available without a write lock.
type EnrollmentStatus struct {
	Namespace  string
	Epoch      string
	Enrollment Enrollment
}

func openNamespace(directory, namespace string) (*os.Root, error) {
	if err := validateNamespace(namespace); err != nil {
		return nil, err
	}
	base, err := openPrivateRoot(directory)
	if err != nil {
		return nil, err
	}

	path := filepath.Join(directory, pathKey(namespace))
	info, statErr := base.Lstat(pathKey(namespace))
	closeErr := base.Close()
	if err := errors.Join(statErr, closeErr); err != nil {
		return nil, err
	}
	if err := privateInfo(info, true); err != nil {
		return nil, err
	}
	return openPrivateRoot(path)
}
func readAuthority(r *os.Root, namespace string) (authority, error) {
	var a authority
	if err := readJSON(r, authorityFile, &a); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return a, ErrNotInitialized
		}
		return a, err
	}
	if a.Version != Version || a.Namespace != namespace || !allocationIDPattern.MatchString(a.Epoch) || validateEnrollment(a.Enrollment) != nil {
		return a, fmt.Errorf("%w: invalid authority enrollment", ErrCorrupt)
	}
	return a, nil
}
func validateEnrollment(e Enrollment) error {
	if !nonblank(e.ClusterID) || !nonblank(e.AuthorityID) || !nonblank(e.AuditID) || !e.CompleteHistoricalAudit || !e.PreviousWriterFenced {
		return fmt.Errorf("%w: complete historical audit and explicit writer fencing required", ErrAuthority)
	}
	seen := map[string]bool{}
	for _, id := range e.ProvenanceIDs {
		if !allocationIDPattern.MatchString(id) || seen[id] {
			return fmt.Errorf("%w: invalid/duplicate provenance ID", ErrAuthority)
		}
		seen[id] = true
	}
	return nil
}

// Initialize requires a pre-provisioned private durable directory. It refuses
// existing namespace directories, including an interrupted initialization. Such
// evidence requires operator recovery, never an implicit empty installation.
func Initialize(ctx context.Context, directory, namespace string, e Enrollment) (j *Journal, retErr error) {
	defer func() {
		if retErr != nil && j != nil {
			retErr = errors.Join(retErr, j.Close())
			j = nil
		}
	}()
	if err := validateNamespace(namespace); err != nil {
		return nil, err
	}
	if err := validateEnrollment(e); err != nil {
		return nil, err
	}
	if len(e.ProvenanceIDs) != 0 {
		return nil, fmt.Errorf("%w: existing provenance requires restored records and recovery", ErrReconciliationRequired)
	}
	base, err := openPrivateRoot(directory)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, base.Close()) }()
	lock, err := waitLock(ctx, base, "initialize.lock")
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, lock.close()) }()
	key := pathKey(namespace)
	if err = base.Mkdir(key, 0o700); err != nil {
		return nil, fmt.Errorf("%w: namespace already exists or cannot be created: %w", ErrNotInitialized, err)
	}
	if err = syncDirectory(base); err != nil {
		return nil, &DurabilityError{Err: err}
	}
	root, err := openPrivateRoot(filepath.Join(directory, key))
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			retErr = errors.Join(retErr, root.Close())
		}
	}()
	epoch, err := NewAllocationID()
	if err != nil {
		return nil, err
	}
	a := authority{Version: Version, Namespace: namespace, Epoch: epoch, Enrollment: e}
	ops := defaultFileOps()
	if err := atomicJSON(root, "index.json", index{Version: Version, ActiveVMs: map[string]string{}}, ops); err != nil {
		return nil, err
	}
	// Authority is last: an incomplete initialization can never admit creates.
	// Validate lock contention on the actual filesystem before activation.
	l, err := waitLock(ctx, root, "index.lock")
	if err != nil {
		return nil, err
	}
	second, acquired, checkErr := tryLock(root, "index.lock")
	if acquired {
		checkErr = errors.Join(fmt.Errorf("journal filesystem does not enforce exclusive locks"), second.close())
	}
	if err := errors.Join(checkErr, l.close()); err != nil {
		return nil, err
	}
	if err := atomicJSON(root, authorityFile, a, ops); err != nil {
		return nil, err
	}
	success = true
	return &Journal{root: root, auth: a, ops: ops}, nil
}

// EnrollmentRecorded reports whether an enrollment authority file exists for
// namespace under directory. It answers the question a reader has when
// InspectEnrollment fails for a reason that is not "nothing is there": a
// directory whose ownership or permissions this package refuses to open may
// hold an enrollment, or may be an empty directory an operator created with the
// umask default and never enrolled, and those two deserve different treatment.
//
// It is deliberately weaker than opening the journal. It follows no symlink
// checks and validates no content, because presence is all it claims. A reader
// that gets false may treat the journal as having nothing to say; anything it
// would act on still has to come from InspectEnrollment or Open.
//
// The error return is "could not tell", which is neither presence nor absence,
// and a caller about to conclude something from absence has to fail closed on
// it.
func EnrollmentRecorded(directory, namespace string) (bool, error) {
	if err := validateNamespace(namespace); err != nil {
		return false, err
	}
	if !filepath.IsAbs(directory) {
		return false, fmt.Errorf("journal: directory must be absolute")
	}
	if _, err := os.Lstat(filepath.Join(directory, pathKey(namespace), authorityFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// InspectEnrollment reads existing enrollment without initialization or locks.
func InspectEnrollment(directory, namespace string) (status EnrollmentStatus, retErr error) {
	root, err := openNamespace(directory, namespace)
	if err != nil {
		return status, err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	a, err := readAuthority(root, namespace)
	if err != nil {
		return status, err
	}
	return EnrollmentStatus{Namespace: a.Namespace, Epoch: a.Epoch, Enrollment: a.Enrollment}, nil
}

// Open loads the enrolled authority and checks verified cluster identity, which
// must not be an API endpoint URL. Endpoint aliases therefore share one tree.
func Open(directory, namespace, clusterID string) (*Journal, error) {
	root, err := openNamespace(directory, namespace)
	if err != nil {
		return nil, err
	}
	a, err := readAuthority(root, namespace)
	if err == nil && a.Enrollment.ClusterID != clusterID {
		err = ErrAuthority
	}
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return &Journal{root: root, auth: a, ops: defaultFileOps()}, nil
}

// Close releases the journal directory handle.
func (j *Journal) Close() error { return j.root.Close() }
func (j *Journal) checkAuthority() error {
	a, err := readAuthority(j.root, j.auth.Namespace)
	if err != nil {
		return err
	}
	if a.Epoch != j.auth.Epoch || a.Enrollment.ClusterID != j.auth.Enrollment.ClusterID || a.Enrollment.AuthorityID != j.auth.Enrollment.AuthorityID {
		return ErrAuthority
	}
	return nil
}
func (j *Journal) readIndex() (index, error) {
	var idx index
	if err := readJSON(j.root, "index.json", &idx); err != nil {
		return idx, err
	}
	if idx.Version != Version || idx.ActiveVMs == nil {
		return idx, fmt.Errorf("%w: invalid generation index", ErrCorrupt)
	}
	for key, id := range idx.ActiveVMs {
		if !validFingerprint(key) || !allocationIDPattern.MatchString(id) {
			return idx, fmt.Errorf("%w: invalid generation index entry", ErrCorrupt)
		}
	}
	return idx, nil
}
func recordName(id string) string { return "allocation-" + id + ".json" }

// Inspect reads and validates one record without acquiring mutation authority.
func (j *Journal) Inspect(id string) (Record, error) {
	var r Record
	if !allocationIDPattern.MatchString(id) {
		return r, fmt.Errorf("journal: invalid allocation UUID")
	}
	if err := readJSON(j.root, recordName(id), &r); err != nil {
		return r, err
	}
	if err := validateRecord(r); err != nil {
		return r, err
	}
	if r.ID != id || r.Namespace != j.auth.Namespace {
		return r, fmt.Errorf("%w: misplaced record", ErrCorrupt)
	}
	return r, nil
}

// List includes historical generations and ignores temporary replacement files.
// Any corrupt record fails the whole scan; callers cannot infer absence from it.
func (j *Journal) List() ([]Record, error) {
	dir, err := j.root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := dir.ReadDir(-1)
	if err := errors.Join(readErr, dir.Close()); err != nil {
		return nil, err
	}
	records := []Record{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "allocation-") && strings.HasSuffix(entry.Name(), ".json") {
			id := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "allocation-"), ".json")
			r, err := j.Inspect(id)
			if err != nil {
				return nil, err
			}
			records = append(records, r)
		}
	}
	sort.Slice(records, func(a, b int) bool { return records[a].ID < records[b].ID })
	return records, nil
}

// ValidateProvenance requires a complete scan of recorded and historical targets.
// A missing ID detects lost/stale history. Existing journal records absent from
// the scan are not deleted: their state still requires resource reconciliation.
func (j *Journal) ValidateProvenance(ids []string, complete bool) error {
	if !complete {
		return fmt.Errorf("%w: incomplete historical provenance scan", ErrReconciliationRequired)
	}
	for _, id := range ids {
		if _, err := j.Inspect(id); err != nil {
			return fmt.Errorf("%w: provenance missing from journal: %s: %w", ErrReconciliationRequired, id, err)
		}
	}
	_, err := j.List()
	return err
}

func (j *Journal) newRecord(id, kind, agentID string, intent Intent) (Record, error) {
	if err := validateIntent(intent); err != nil {
		return Record{}, err
	}
	now := time.Now().UTC()
	r := Record{Version: Version, ID: id, Namespace: j.auth.Namespace, ClusterID: j.auth.Enrollment.ClusterID, AuthorityEpoch: j.auth.Epoch, Kind: kind, AgentID: agentID, Intent: intent, State: Planned, CreatedAt: now, UpdatedAt: now}
	if kind == "disk" {
		token, err := DiskCorrelationToken(id)
		if err != nil {
			return r, err
		}
		r.DiskToken = token
	}
	r.Attempts = []Attempt{{Number: 0, Plan: initialPlan(r.Intent)}}
	r = cloneRecord(r)
	return r, validateRecord(r)
}

// VMPlanFunc plans an allocation while the journal holds the index lock. It
// receives the identifier the new record will carry and every record the
// journal holds, read under that lock, so an allocation a sibling create
// started moments earlier is already visible. It returns the Intent the new
// record freezes, and that Intent is validated exactly like a caller-supplied
// one, so a fingerprint-only Intent fails rather than writing a bad record.
//
// The contract is strict, because index.lock is cluster visible and every
// create in the namespace queues behind the callback.
//
//   - It runs under index.lock.
//   - It may call List and Inspect, neither of which takes a lock.
//   - It must not call anything that takes the index lock in either mode,
//     which includes AcquireVM, AcquireVMPlanned, Acquire, and CreateDisk.
//     The journal's locks are not reentrant, so such a call deadlocks the
//     caller against itself.
//   - It must not perform network work. Discovery, node facts, and template
//     lookups belong before the acquisition.
//
// An error returned by the callback aborts the acquisition, and neither an
// index entry nor a record file is written.
type VMPlanFunc func(id string, siblings []Record) (Intent, error)

// AcquireVM converges matching concurrent callers. Existing returns Resumed=true:
// caller must reconcile actual ownership/existence before any mutation/CID return.
// Policy changes alone never replace an existing allocation or its frozen plan.
func (j *Journal) AcquireVM(ctx context.Context, agentID string, intent Intent) (*Handle, error) {
	if !validFingerprint(intent.IntentFingerprint) {
		return nil, fmt.Errorf("journal: invalid caller intent fingerprint")
	}
	return j.acquireVM(ctx, agentID, intent, nil)
}

// AcquireVMPlanned acquires a generation whose Intent the callback produces
// under the index lock, so ranking sees the claims of in-flight siblings. It
// converges concurrent callers exactly as AcquireVM does, and an existing
// non-closed generation returns Resumed=true without the callback running at
// all, so a retry pass never re-plans.
//
// Unlike AcquireVM it cannot validate a caller intent fingerprint up front,
// because there is no Intent until the callback returns one. The Intent is
// validated when the record is built. See VMPlanFunc for what the callback may
// and may not do while the lock is held.
func (j *Journal) AcquireVMPlanned(ctx context.Context, agentID string, plan VMPlanFunc) (*Handle, error) {
	if plan == nil {
		return nil, fmt.Errorf("journal: planning callback is required")
	}
	return j.acquireVM(ctx, agentID, Intent{}, plan)
}

// acquireVM owns the retry loop both entry points share: it takes the index
// lock, runs one attempt, and pauses before retrying a generation another
// process holds. A nil plan keeps the caller-supplied intent.
func (j *Journal) acquireVM(ctx context.Context, agentID string, intent Intent, plan VMPlanFunc) (*Handle, error) {
	if !nonblank(agentID) {
		return nil, fmt.Errorf("journal: agent ID is required")
	}
	for {
		idxLock, err := waitLock(ctx, j.root, "index.lock")
		if err != nil {
			return nil, err
		}
		h, busy, err := j.acquireVMUnderIndex(agentID, intent, plan)
		releaseErr := idxLock.close()
		if err = errors.Join(err, releaseErr); err != nil {
			if h != nil {
				err = errors.Join(err, h.Close())
			}
			return nil, err
		}
		if !busy {
			return h, nil
		}
		if err := pause(ctx); err != nil {
			return nil, err
		}
	}
}
func (j *Journal) acquireVMUnderIndex(agentID string, intent Intent, plan VMPlanFunc) (h *Handle, busy bool, retErr error) {
	if err := j.checkAuthority(); err != nil {
		return nil, false, err
	}
	idx, err := j.readIndex()
	if err != nil {
		return nil, false, err
	}
	if err := j.validateGenerationIndex(idx); err != nil {
		return nil, false, err
	}
	key := pathKey(agentID)
	if id := idx.ActiveVMs[key]; id != "" {
		lock, ok, err := tryLock(j.root, id+".lock")
		if err != nil || !ok {
			return nil, !ok, err
		}
		keep := false
		defer func() {
			if !keep {
				retErr = errors.Join(retErr, lock.close())
			}
		}()
		r, err := j.Inspect(id)
		if err != nil {
			return nil, false, fmt.Errorf("%w: indexed generation unavailable: %w", ErrReconciliationRequired, err)
		}
		if r.Kind != "vm" || r.AgentID != agentID {
			return nil, false, fmt.Errorf("%w: incorrect agent generation", ErrCorrupt)
		}
		if !generationClosed(r.State) {
			if r.Intent.IntentFingerprint != intent.IntentFingerprint {
				return nil, false, ErrConflict
			}
			keep = true
			return &Handle{journal: j, lock: lock, record: r, Resumed: true, requiresReconciliation: true}, false, nil
		}
	}
	id, err := NewAllocationID()
	if err != nil {
		return nil, false, err
	}
	if _, err = j.root.Lstat(recordName(id)); err == nil {
		return nil, false, ErrConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	// Plan here and nowhere earlier. newRecord consumes the Intent, the
	// existing-generation branch above has already returned every resume, and
	// a callback placed before the retry loop would re-plan on every pass.
	if plan != nil {
		siblings, listErr := j.List()
		if listErr != nil {
			return nil, false, listErr
		}
		planned, planErr := plan(id, siblings)
		if planErr != nil {
			return nil, false, planErr
		}
		intent = planned
	}
	r, err := j.newRecord(id, "vm", agentID, intent)
	if err != nil {
		return nil, false, err
	}
	lock, ok, err := tryLock(j.root, id+".lock")
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, fmt.Errorf("%w: UUID collision", ErrConflict)
	}
	keep := false
	defer func() {
		if !keep {
			retErr = errors.Join(retErr, lock.close())
		}
	}()
	// Index first: a crash before record creation leaves a blocked generation.
	// Never allocate twice by treating an indexed missing record as absent.
	idx.ActiveVMs[key] = id
	if err := atomicJSON(j.root, "index.json", idx, j.ops); err != nil {
		return nil, false, err
	}
	if err := atomicJSON(j.root, recordName(id), r, j.ops); err != nil {
		return nil, false, err
	}
	keep = true
	return &Handle{journal: j, lock: lock, record: r}, false, nil
}

// CreateDisk accepts the UUID generated before ranking and rejects its reuse.
// Independent CPI invocations must each generate a fresh ID.
func (j *Journal) CreateDisk(ctx context.Context, id string, intent Intent) (h *Handle, retErr error) {
	defer func() {
		if retErr != nil && h != nil {
			retErr = errors.Join(retErr, h.Close())
			h = nil
		}
	}()
	if !allocationIDPattern.MatchString(id) {
		return nil, fmt.Errorf("journal: invalid allocation UUID")
	}
	idxLock, err := waitLock(ctx, j.root, "index.lock")
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, idxLock.close()) }()
	if err := j.checkAuthority(); err != nil {
		return nil, err
	}
	idx, err := j.readIndex()
	if err != nil {
		return nil, err
	}
	if err := j.validateGenerationIndex(idx); err != nil {
		return nil, err
	}
	if _, err = j.root.Lstat(recordName(id)); err == nil {
		return nil, ErrConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	lock, ok, err := tryLock(j.root, id+".lock")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrConflict
	}
	keep := false
	defer func() {
		if !keep {
			retErr = errors.Join(retErr, lock.close())
		}
	}()
	r, err := j.newRecord(id, "disk", "", intent)
	if err != nil {
		return nil, err
	}
	records, err := j.List()
	if err != nil {
		return nil, err
	}
	if err := validateTokenUnique(r.DiskToken, records); err != nil {
		return nil, err
	}
	if err := atomicJSON(j.root, recordName(id), r, j.ops); err != nil {
		return nil, err
	}
	keep = true
	return &Handle{journal: j, lock: lock, record: r}, nil
}

// Acquire locks an existing record for lifecycle/reconciliation. It never creates
// or changes an active generation, and therefore needs no index lock.
func (j *Journal) Acquire(ctx context.Context, id string) (*Handle, error) {
	if !allocationIDPattern.MatchString(id) {
		return nil, fmt.Errorf("journal: invalid allocation UUID")
	}
	l, err := waitLock(ctx, j.root, id+".lock")
	if err != nil {
		return nil, err
	}
	if err := j.checkAuthority(); err != nil {
		return nil, errors.Join(err, l.close())
	}
	r, err := j.Inspect(id)
	if err != nil {
		return nil, errors.Join(err, l.close())
	}
	return &Handle{journal: j, lock: l, record: r, Resumed: true, requiresReconciliation: true}, nil
}

// Handle owns one allocation OS lock. Save/Record/Close serialize locally.
// Resumed is informational and immutable; it never certifies remote resources.
type Handle struct {
	journal                *Journal
	lock                   *fileLock
	record                 Record
	Resumed                bool
	mu                     sync.Mutex
	closed                 bool
	poisoned               bool
	requiresReconciliation bool
}

// Record returns an isolated copy of the currently held allocation.
func (h *Handle) Record() Record { h.mu.Lock(); defer h.mu.Unlock(); return cloneRecord(h.record) }

// Save validates and atomically persists an authorized evidence transition.
func (h *Handle) Save(next Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.saveLocked(next, false)
}
func (h *Handle) saveLocked(next Record, attemptTransition bool) error {
	if h.closed {
		return ErrClosed
	}
	if h.poisoned {
		return ErrReconciliationRequired
	}
	if err := h.journal.checkAuthority(); err != nil {
		return err
	}
	reconciled := false
	if len(next.Verifications) > len(h.record.Verifications) {
		v := next.Verifications[len(next.Verifications)-1]
		reconciled = validateVerification(v) && (v.OwnershipVerified || v.AbsenceVerified || next.State == VMDeletedRetained && v.VMAbsenceVerified && v.ArtifactDispositionVerified)
	}
	if h.requiresReconciliation && next.State != ReconciliationRequired && !reconciled {
		return ErrReconciliationRequired
	}
	old := h.record
	if attemptTransition {
		if err := validateAttemptTransition(old, next); err != nil {
			return err
		}
		old = cloneRecord(old)
		old.Attempts = cloneAttempts(next.Attempts)
		old.State = next.State
	}
	if err := validateUpdate(old, next); err != nil {
		return err
	}
	if terminal(h.record.State) {
		return nil
	}
	next.UpdatedAt = time.Now().UTC()
	if err := atomicJSON(h.journal.root, recordName(next.ID), next, h.journal.ops); err != nil {
		h.poisoned = true
		return err
	}
	h.record = cloneRecord(next)
	if reconciled {
		h.requiresReconciliation = false
	}
	return nil
}

// Close releases allocation ownership.
func (h *Handle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	return h.lock.close()
}

// RecoverAuthority performs explicit fenced recovery of an existing authority.
// All record locks must be free. It never rewrites historical identities.
// The caller must reopen after success; old handles are fenced by the new epoch.
func (j *Journal) RecoverAuthority(ctx context.Context, e Enrollment) (retErr error) {
	if err := validateEnrollment(e); err != nil {
		return err
	}
	idxLock, err := waitLock(ctx, j.root, "index.lock")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, idxLock.close()) }()
	if err := j.checkAuthority(); err != nil {
		return err
	}
	records, err := j.List()
	if err != nil {
		return err
	}
	locks := []*fileLock{}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			retErr = errors.Join(retErr, locks[i].close())
		}
	}()
	for rIndex := range records {
		r := records[rIndex]
		l, ok, err := tryLock(j.root, r.ID+".lock")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: allocation writer still active", ErrAuthority)
		}
		locks = append(locks, l)
	}
	// Reread after locking: a lifecycle writer may have completed since List.
	records, err = j.List()
	if err != nil {
		return err
	}
	if err := j.ValidateProvenance(e.ProvenanceIDs, true); err != nil {
		return err
	}
	idx, err := j.readIndex()
	if err != nil {
		return err
	}
	if err := j.validateGenerationIndex(idx); err != nil {
		return err
	}
	if e.ClusterID != j.auth.Enrollment.ClusterID {
		for rIndex := range records {
			r := records[rIndex]
			if !terminal(r.State) {
				return fmt.Errorf("%w: live or unresolved allocations prevent cluster rebinding", ErrAuthority)
			}
		}
	}
	epoch, err := NewAllocationID()
	if err != nil {
		return err
	}
	return atomicJSON(j.root, authorityFile, authority{Version: Version, Namespace: j.auth.Namespace, Epoch: epoch, Enrollment: e}, j.ops)
}

// validateTokenUnique is also used by recovery adapters against remote collision
// inventories. Historical journal tokens remain reserved after deletion.
func validateTokenUnique(token string, records []Record) error {
	for rIndex := range records {
		r := records[rIndex]
		if r.DiskToken == token {
			return fmt.Errorf("%w: shortened disk token collision", ErrConflict)
		}
	}
	return nil
}

// ResolveMissingVMGeneration repairs an index-first crash only after an external
// complete historical audit verifies absence and artifact disposition. It writes
// a cleaned tombstone for the exact indexed UUID; it cannot submit a mutation or
// fabricate the absent record's original plan. The supplied nonsecret intent is
// retained as audit reconstruction, identified by verification.EvidenceID.
func (j *Journal) ResolveMissingVMGeneration(ctx context.Context, agentID, id string, intent Intent, verification Verification) (retErr error) {
	if !nonblank(agentID) || !allocationIDPattern.MatchString(id) || !validateVerification(verification) || !verification.AbsenceVerified || !verification.ArtifactDispositionVerified {
		return fmt.Errorf("%w: complete verified absence required", ErrReconciliationRequired)
	}
	idxLock, err := waitLock(ctx, j.root, "index.lock")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, idxLock.close()) }()
	if err := j.checkAuthority(); err != nil {
		return err
	}
	idx, err := j.readIndex()
	if err != nil {
		return err
	}
	if idx.ActiveVMs[pathKey(agentID)] != id {
		return ErrConflict
	}
	lock, ok, err := tryLock(j.root, id+".lock")
	if err != nil {
		return err
	}
	if !ok {
		return ErrConflict
	}
	defer func() { retErr = errors.Join(retErr, lock.close()) }()
	if _, err = j.root.Lstat(recordName(id)); err == nil {
		return ErrConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	r, err := j.newRecord(id, "vm", agentID, intent)
	if err != nil {
		return err
	}
	r.State = Cleaned
	r.Reason = "indexed record absent; complete historical audit verified cleanup"
	r.Verifications = []Verification{verification}
	if err := validateRecord(r); err != nil {
		return err
	}
	return atomicJSON(j.root, recordName(id), r, j.ops)
}

// Both directions are required: an older checksummed index can otherwise hide a
// newer live record and authorize duplicate generation creation after restore.
func (j *Journal) validateGenerationIndex(idx index) error {
	records, err := j.List()
	if err != nil {
		return err
	}
	return validateGenerationRecords(idx, records)
}

func validateGenerationRecords(idx index, records []Record) error {
	byID := make(map[string]Record, len(records))
	for rIndex := range records {
		r := records[rIndex]
		byID[r.ID] = r
	}
	for key, id := range idx.ActiveVMs {
		r, ok := byID[id]
		if !ok || r.Kind != "vm" || pathKey(r.AgentID) != key {
			return fmt.Errorf("%w: index points to absent or mismatched generation", ErrReconciliationRequired)
		}
	}
	for rIndex := range records {
		r := records[rIndex]
		if r.Kind == "vm" && !generationClosed(r.State) && idx.ActiveVMs[pathKey(r.AgentID)] != r.ID {
			return fmt.Errorf("%w: nonterminal VM generation is missing from active index", ErrReconciliationRequired)
		}
	}
	return nil
}

// RecoverIndex repairs a missing, corrupt, or stale index from retained records
// after explicit fenced historical audit. Duplicate live generations or missing
// provenance remain unresolved. It changes the authority epoch, so callers must
// reopen before further allocation. Missing record files require their separate
// verified-absence recovery procedure; no live allocation is invented here.
func (j *Journal) RecoverIndex(ctx context.Context, e Enrollment) (retErr error) {
	if err := validateEnrollment(e); err != nil {
		return err
	}
	if e.ClusterID != j.auth.Enrollment.ClusterID {
		return ErrAuthority
	}
	idxLock, err := waitLock(ctx, j.root, "index.lock")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, idxLock.close()) }()
	if err := j.checkAuthority(); err != nil {
		return err
	}
	records, err := j.List()
	if err != nil {
		return err
	}
	locks := []*fileLock{}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			retErr = errors.Join(retErr, locks[i].close())
		}
	}()
	for rIndex := range records {
		r := records[rIndex]
		l, ok, err := tryLock(j.root, r.ID+".lock")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: allocation writer still active", ErrAuthority)
		}
		locks = append(locks, l)
	}
	records, err = j.List()
	if err != nil {
		return err
	}
	if err := j.ValidateProvenance(e.ProvenanceIDs, true); err != nil {
		return err
	}
	// A parseable index retains evidence of a missing generation even when the
	// operator's scan reports no resource. Resolve that generation explicitly.
	old, oldErr := j.readIndex()
	if oldErr == nil {
		for _, id := range old.ActiveVMs {
			if _, err = j.Inspect(id); err != nil {
				return fmt.Errorf("%w: indexed record still missing", ErrReconciliationRequired)
			}
		}
	}
	idx := index{Version: Version, ActiveVMs: map[string]string{}}
	chosen := map[string]Record{}
	for rIndex := range records {
		r := records[rIndex]
		if r.Kind != "vm" {
			continue
		}
		key := pathKey(r.AgentID)
		previous, exists := chosen[key]
		if exists && !generationClosed(previous.State) && !generationClosed(r.State) {
			return fmt.Errorf("%w: duplicate live generations require audit", ErrReconciliationRequired)
		}
		if !exists || generationClosed(previous.State) && (!generationClosed(r.State) || r.CreatedAt.After(previous.CreatedAt)) {
			chosen[key] = r
			idx.ActiveVMs[key] = r.ID
		}
	}
	if err := atomicJSON(j.root, "index.json", idx, j.ops); err != nil {
		return err
	}
	epoch, err := NewAllocationID()
	if err != nil {
		return err
	}
	return atomicJSON(j.root, authorityFile, authority{Version: Version, Namespace: j.auth.Namespace, Epoch: epoch, Enrollment: e}, j.ops)
}
