package handlers

import (
	"errors"
	"fmt"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// wholeStorageListingFailure returns the storage, node, and reason of a failed
// storage content listing when that listing is the whole failure. It follows
// single-error unwrapping, and it looks through a join that has exactly one
// member, because errors.Join(err, closeErr...) with every close error nil is
// that. It gives up at an error that joins several, because a listing joined
// with another failure isn't a listing that stopped the delete on its own, and
// at a CPI error, because a typed error in the chain carries a classification
// of its own, such as a lifecycle's uncertainty. It also gives up when the
// listing did not record its target, because the operator's message must name
// the storage and the node.
func wholeStorageListingFailure(err error) (storage, node, reason string, ok bool) {
	reason = pve.StorageVolumeObservationReason(err)
	storage, node = pve.StorageVolumeObservationTarget(err)
	if reason == "" || storage == "" || node == "" {
		return "", "", "", false
	}
	for cause := err; cause != nil; {
		if _, typed := cause.(*cpierrors.Error); typed { //nolint:errorlint // The walk unwraps one error at a time itself, so it can stop at a joined error, which errors.As would search through.
			return "", "", "", false
		}
		joined, isJoin := cause.(interface{ Unwrap() []error }) //nolint:errorlint // Same walk.
		if !isJoin {
			cause = errors.Unwrap(cause)
			continue
		}
		var members []error
		for _, member := range joined.Unwrap() {
			if member != nil {
				members = append(members, member)
			}
		}
		if len(members) != 1 {
			return "", "", "", false
		}
		cause = members[0]
	}
	return storage, node, reason, true
}

// storageListingStop marks the error delete_vm returns when a failed listing
// alone stopped a disposal, so the operator's cleanup command can name the
// storage and say to run it again. The marker wraps the CloudError, so its
// CPI type and text stay what the Director reads.
type storageListingStop struct {
	err                   *cpierrors.Error
	storage, node, reason string
}

func (e *storageListingStop) Error() string { return e.err.Error() }
func (e *storageListingStop) Unwrap() error { return e.err }

// storageListingStoppedDelete is the error delete_vm returns when a failed
// listing alone stopped a disposal. It is not marked definite, so a request
// that the CPI's budget cut off reads as one the Director may retry.
func storageListingStoppedDelete(storage, node, reason string) error {
	return &storageListingStop{
		err:     cpierrors.Cloud("delete_vm could not list storage %s on node %s (%s) and stopped before its next change, with every step it took recorded; rerun the delete once that storage lists", storage, node, reason),
		storage: storage, node: node, reason: reason,
	}
}

// storageListingFailureDetail names the storage, node, and reason of a failed
// listing in front of the listing's own error, which stays in the chain.
type storageListingFailureDetail struct {
	storage, node, reason string
	err                   error
}

func (e *storageListingFailureDetail) Error() string {
	return fmt.Sprintf("could not list storage %s on node %s (%s)", e.storage, e.node, e.reason)
}

func (e *storageListingFailureDetail) Unwrap() error { return e.err }

// describeStorageListingFailure puts the storage, node, and reason of a failed
// listing that is the whole failure into err's text, so a reconciliation
// error built on it names the listing. Any other error comes back unchanged.
func describeStorageListingFailure(err error) error {
	storage, node, reason, ok := wholeStorageListingFailure(err)
	if !ok {
		return err
	}
	return &storageListingFailureDetail{storage: storage, node: node, reason: reason, err: err}
}
