package handlers

import (
	"math/bits"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// siblingAcquiredClockSkewTolerance widens the window in which a sibling's
// acquired bytes still count against a placement we are ranking. Record.UpdatedAt
// is stamped from the wall clock of whichever process wrote the record, while
// the inventory snapshot's StartedAt comes from the collector's injectable
// clock, and under several directors those two clocks sit on different hosts.
// Two minutes is longer than any plausible offset between NTP-disciplined hosts
// and far shorter than the interval over which a share's utilization changes
// materially. Subtracting the tolerance biases us toward counting, which is
// deliberate. Counting a sibling whose bytes PVE already reports over-counts by
// at most one allocation, while dropping them reproduces in miniature the
// same-share pile-up this accounting exists to prevent.
const siblingAcquiredClockSkewTolerance = 2 * time.Minute

// StorageAllocationCharging reports whether an allocation in this state charges
// its bytes against a placement that is being ranked right now. The four states
// it names are the genuinely in-flight ones, where the volume is neither absent
// nor yet reflected in the capacity numbers PVE reports.
//
// The resting states are excluded on purpose. In ready_to_return, adopted, and
// vm_deleted_retained the volume exists and PVE already reports it, and in
// deleted and cleaned it does not exist at all. A rule that charged every
// non-terminal record would charge every live VM forever at its virtual size on
// top of the bytes the snapshot already carries, and because the capacity
// projection is a hard gate, the capacity ceiling would quietly turn into a
// VM-count ceiling.
//
// This predicate is exported because the storage journal audit command reports
// the same flag for each record it prints. A second copy of the state list in
// that command would drift away from the planner's arithmetic, and an operator
// would then read a view that no longer matches what the ranking charges.
func StorageAllocationCharging(state aj.State) bool {
	switch state {
	case aj.Planned, aj.Submitted, aj.Observed, aj.ReconciliationRequired:
		return true
	default:
		return false
	}
}

// siblingCharges derives everything the storage remedies need from one journal
// scan, so each record is decoded at most once.
//
// member and domain hold the bytes that in-flight allocations other than ours
// have already claimed, keyed by capacity key and by capacity domain key, ready
// to seed a ranking ledger. counts holds the number of records whose group,
// cut to scope by StorageAntiAffinityGroupKey, matches our own on each
// capacity key, which the ranking partitions by. It is nil when the cut key
// is empty, which covers an empty group and the none scope in one test.
//
// self is our own allocation identifier and is always skipped. Excluding it
// keeps a retry on the share it already holds rather than steering it away from
// its own retained artifacts. snapshotStart is the inventory snapshot's
// StartedAt, which decides whether a sibling's acquired bytes still count.
//
// A record in a terminal state feeds neither result, so it is never decoded.
// A charging record whose plan is invalid JSON or carries an unknown plan
// version returns an error, because a namespace we cannot read in full cannot
// be accounted for in full, and the caller surfaces that the way it surfaces an
// admission audit failure.
func siblingCharges(records []aj.Record, self, group, scope string, snapshotStart time.Time) (map[string]uint64, map[string]uint64, map[string]int, error) {
	member, domain := map[string]uint64{}, map[string]uint64{}
	var counts map[string]int
	groupKey := StorageAntiAffinityGroupKey(group, scope)
	if groupKey != "" {
		counts = map[string]int{}
	}
	for i := range records {
		record := records[i]
		if record.ID == self {
			continue
		}
		charging := StorageAllocationCharging(record.State)
		// The count's state list is wider than the byte maps' list on purpose:
		// a sibling that finished successfully still occupies its share.
		countable := counts != nil && record.State != aj.Deleted && record.State != aj.Cleaned
		if !charging && !countable {
			continue
		}
		plan, err := siblingStorageAllocationPlan(record)
		if err != nil {
			if charging {
				return nil, nil, nil, err
			}
			// A resting record only feeds the count, and a count we cannot
			// take must not fail every create in the namespace. That keeps a
			// rollback across a plan version change from wedging a director
			// whose in-flight records all read cleanly.
			continue
		}
		if charging {
			if err := addSiblingRecordBytes(member, domain, record, plan, snapshotStart); err != nil {
				return nil, nil, nil, err
			}
		}
		// A record written before this feature carries no Group, so it counts
		// for nothing and an existing namespace keeps today's behaviour until
		// its first create after the upgrade.
		if countable && StorageAntiAffinityGroupKey(plan.Group, scope) == groupKey {
			if root, ok := managedVMRoleTarget(plan, storageRoleRoot); ok && root.CapacityKey != "" {
				counts[root.CapacityKey]++
			}
		}
	}
	return member, domain, counts, nil
}

// addSiblingRecordBytes folds one in-flight record's claim into the two byte
// maps. The bytes come from two sources and we take the larger per key rather
// than the sum, because once a step exists its charges restate the claim the
// plan made. The plan's own charges cover a record in planned that has chosen
// its targets but written no step yet.
func addSiblingRecordBytes(member, domain map[string]uint64, record aj.Record, plan *StorageAllocationPlan, snapshotStart time.Time) error {
	planMember, planDomain := map[string]uint64{}, map[string]uint64{}
	for i := range plan.Charges {
		charge := plan.Charges[i]
		if err := addSiblingKeyBytes(planMember, charge.CapacityKey, charge.Charge.Bytes); err != nil {
			return siblingBytesError(record, err)
		}
		if err := addSiblingKeyBytes(planDomain, charge.DomainKey, charge.Charge.Bytes); err != nil {
			return siblingBytesError(record, err)
		}
	}
	stepMember, stepDomain := map[string]uint64{}, map[string]uint64{}
	// Acquired bytes count only when the record was touched no earlier than the
	// snapshot began, less the skew tolerance. Older acquired bytes are already
	// in the observation the snapshot carries, so counting them again would
	// double-charge the share.
	countAcquired := record.UpdatedAt.After(snapshotStart.Add(-siblingAcquiredClockSkewTolerance))
	for i := range record.Steps {
		// Summation is per charge and never per step, because charges are
		// acquired one at a time, so a step with two charges can legitimately
		// sit with the first acquired and the second still outstanding.
		for _, charge := range record.Steps[i].Charges {
			if charge.AcquiredBytes < 0 || charge.OutstandingBytes < 0 {
				return cpierrors.Cloud("allocation %s records a negative charge; audit required", record.ID)
			}
			// Outstanding bytes always count: they are in no PVE status yet.
			claimed := uint64(charge.OutstandingBytes)
			if countAcquired {
				sum, err := addSiblingBytes(claimed, uint64(charge.AcquiredBytes))
				if err != nil {
					return siblingBytesError(record, err)
				}
				claimed = sum
			}
			if claimed == 0 {
				continue
			}
			if err := addSiblingKeyBytes(stepMember, charge.Backing, claimed); err != nil {
				return siblingBytesError(record, err)
			}
			if err := addSiblingKeyBytes(stepDomain, charge.Domain, claimed); err != nil {
				return siblingBytesError(record, err)
			}
		}
	}
	if err := mergeSiblingLarger(member, planMember, stepMember); err != nil {
		return siblingBytesError(record, err)
	}
	if err := mergeSiblingLarger(domain, planDomain, stepDomain); err != nil {
		return siblingBytesError(record, err)
	}
	return nil
}

// mergeSiblingLarger adds the larger of the two claims for each key into the
// running total. A key present in only one of them takes that claim.
func mergeSiblingLarger(into, planned, stepped map[string]uint64) error {
	for key, claim := range planned {
		if stepped[key] > claim {
			claim = stepped[key]
		}
		if err := addSiblingKeyBytes(into, key, claim); err != nil {
			return err
		}
	}
	for key, claim := range stepped {
		if _, both := planned[key]; both {
			continue
		}
		if err := addSiblingKeyBytes(into, key, claim); err != nil {
			return err
		}
	}
	return nil
}

// addSiblingKeyBytes accumulates bytes under one key, ignoring a blank key,
// which is how an unset capacity domain arrives.
func addSiblingKeyBytes(into map[string]uint64, key string, value uint64) error {
	if key == "" || value == 0 {
		return nil
	}
	sum, err := addSiblingBytes(into[key], value)
	if err != nil {
		return err
	}
	into[key] = sum
	return nil
}

// addSiblingBytes refuses to wrap, because a wrapped total would understate a
// share's commitment and the capacity projection would admit a placement that
// cannot fit.
func addSiblingBytes(a, b uint64) (uint64, error) {
	sum, carry := bits.Add64(a, b, 0)
	if carry != 0 {
		return 0, cpierrors.Cloud("sibling charge bytes overflow")
	}
	return sum, nil
}

func siblingBytesError(record aj.Record, err error) error {
	return cpierrors.Cloud("allocation %s has unusable charge evidence: %s; audit required", record.ID, err.Error())
}
