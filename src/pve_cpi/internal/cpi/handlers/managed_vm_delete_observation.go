package handlers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// A successful deletion task can precede visibility convergence. This bounded
// observation never repeats the mutation and requires a complete absence proof.
// Default NFS directory attributes can remain cached for 60 seconds; allow that
// cache to expire after PVE removes and recreates an empty content directory.
// The caller deadline still wins, and persistent uncertainty remains an error.
func awaitManagedVMVolumeAbsence(ctx context.Context, deps Deps, node, volume string) error {
	observationCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	lastReason := ""
	return pollManagedVMVolumeAbsence(observationCtx, 500*time.Millisecond, func(probeCtx context.Context) (bool, error) {
		return pve.ObserveStorageVolumePresence(probeCtx, deps.PVE, node, volume)
	}, func(reason string) {
		if reason != lastReason {
			deps.Log(ctx).Info("managed VM delete volume absence observation", log.String("reason", reason))
			lastReason = reason
		}
	})
}

func pollManagedVMVolumeAbsence(ctx context.Context, interval time.Duration, observe func(context.Context) (bool, error), report func(string)) error {
	reason := "not_observed"
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("volume absence observation %s: %w", reason, err)
		}
		present, err := observe(ctx)
		if ctx.Err() != nil {
			return fmt.Errorf("volume absence observation %s: %w", reason, ctx.Err())
		}
		if err == nil && !present {
			return nil
		}
		reason = managedVMVolumeAbsenceReason(present, err)
		report(reason)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("volume absence observation %s: %w", reason, ctx.Err())
		case <-timer.C:
		}
	}
}

// Never log API response text, credentials, paths, or an arbitrary wrapped error.
func managedVMVolumeAbsenceReason(present bool, err error) string {
	if err == nil && present {
		return "still_present"
	}
	if err == nil {
		return "unproven"
	}
	if reason := pve.StorageVolumeObservationReason(err); reason != "" {
		return reason
	}
	switch err.Error() {
	case "managed volume content listing unavailable":
		return "listing_unavailable"
	case "managed volume content listing malformed":
		return "listing_malformed"
	case "managed volume content listing target mismatch":
		return "listing_target_mismatch"
	case "managed volume content visibility proof unavailable":
		return "visibility_unavailable"
	case "managed volume content visibility unproven":
		return "visibility_unproven"
	default:
		return "observation_unavailable"
	}
}

// managedVMDestroyCRMWait bounds how long the destroy observation waits for the
// CRM to drop the service of a guest that no longer has an HA resource. The CRM
// drops it on its next round, which is on the order of ten seconds.
const (
	managedVMDestroyCRMWait     = 30 * time.Second
	managedVMDestroyCRMInterval = 500 * time.Millisecond
)

// awaitManagedVMDestroyAbsence proves the destroyed VM absent. It repeats the
// proof only while the one outcome that time cures keeps coming back, the CRM
// still listing the service, and returns every other result at once. When the
// wait ends with the CRM still listing it, the last retriable error returns.
func awaitManagedVMDestroyAbsence(ctx context.Context, deps Deps, vmid int) (pve.VMLocation, error) {
	return pollManagedVMDestroyAbsence(ctx, managedVMDestroyCRMWait, managedVMDestroyCRMInterval, func() (pve.VMLocation, error) {
		return findVMAuthoritative(ctx, deps, vmid)
	})
}

func pollManagedVMDestroyAbsence(
	ctx context.Context, wait, interval time.Duration, find func() (pve.VMLocation, error),
) (pve.VMLocation, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		location, err := find()
		if err == nil || !errors.Is(err, pve.ErrHAManagerHoldsService) {
			return location, err
		}
		tick := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			tick.Stop()
			return location, err
		case <-deadline.C:
			tick.Stop()
			return location, err
		case <-tick.C:
		}
	}
}
