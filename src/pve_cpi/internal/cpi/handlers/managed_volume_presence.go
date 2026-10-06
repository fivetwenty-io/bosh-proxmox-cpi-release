package handlers

import (
	"context"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// managedVolumePresenceReads bounds how many times managedVolumePresent lists
// a storage before it reports the listing's failure.
const managedVolumePresenceReads = 3

// managedVolumePresent reports whether volume is in its storage's content
// listing on node. A listing that fails for a reason that usually clears on
// its own, such as a server error, a timeout, or a dropped connection, is read
// again up to managedVolumePresenceReads times in all, with
// managedVolumePresenceRetryDelay between reads, inside ctx. A refusal, such as
// a permission, authentication, parameter, or certificate failure, or a
// listing whose content can't be read, returns at once, because another read
// would only meet it again.
//
// When it gives up on a listing, it logs the reason, the storage, and the node
// at Warn. The reason is a bounded category, so no endpoint URL, response
// body, or credential reaches the log.
func managedVolumePresent(ctx context.Context, deps Deps, node, volume string) (bool, error) {
	var err error
	for read := 1; ; read++ {
		var present bool
		present, err = pve.ObserveStorageVolumePresence(ctx, deps.PVE, node, volume)
		if err == nil {
			return present, nil
		}
		reason := pve.StorageVolumeObservationReason(err)
		if read >= managedVolumePresenceReads || !transientStorageListingReason(reason) || !waitManagedVolumePresenceRetry(ctx) {
			logStorageListingFailure(ctx, deps, err, read)
			return false, err
		}
	}
}

// transientStorageListingReason reports whether a failed listing's reason is
// one another read can clear. Every 5xx answer counts except 501, which means
// PVE does not implement the call. A 595, 596, or 599 is how the PVE proxy
// reports a node it could not reach or a request it gave up on.
func transientStorageListingReason(reason string) bool {
	switch reason {
	case "listing_timeout", "listing_connection_error", "listing_network_error", "listing_data_missing", "listing_unavailable":
		return true
	case "listing_http_501":
		return false
	}
	code, found := strings.CutPrefix(reason, "listing_http_")
	return found && len(code) == 3 && code[0] == '5' && strings.Trim(code, "0123456789") == ""
}

// waitManagedVolumePresenceRetry waits out the delay before another read. It
// returns false when ctx ends first.
func waitManagedVolumePresenceRetry(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	delay := managedVolumePresenceRetryDelay()
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// logStorageListingFailure records a listing that managedVolumePresent gave up
// on. An error from outside the listing, such as a malformed volume ID, has
// no reason and is left to the caller.
func logStorageListingFailure(ctx context.Context, deps Deps, err error, reads int) {
	reason := pve.StorageVolumeObservationReason(err)
	if reason == "" {
		return
	}
	storage, node := pve.StorageVolumeObservationTarget(err)
	deps.Log(ctx).Warn("storage content listing failed",
		log.String("reason", reason),
		log.String("storage", storage),
		log.String("node", node),
		log.Int("reads", reads))
}
