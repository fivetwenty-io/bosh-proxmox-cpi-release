package handlers

import (
	"context"
	"sync/atomic"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// parkDiskFunc is the shape of pve.ParkDisk, named so the seam below can hold
// it in an atomic pointer and so a test can declare a replacement without
// restating the parameter list.
type parkDiskFunc func(
	ctx context.Context,
	c pve.Client,
	logger *log.Logger,
	node, bareVolid string,
	cfg pve.ParkerConfig,
	pctx pve.ParkContext,
) error

// parkDiskImpl holds the park the three folded funnels call, which is
// pve.ParkDisk in every process that is not running a test. It is a test seam
// rather than an abstraction over parking, and production never swaps it.
//
// It exists because the three funnels that build a park config out of
// parkerWriteConfigFor, which are parkFreshDisk, handleAlreadyDetachedParked,
// and parkAfterDetach, have to be tested on the config they hand down rather
// than on the PVE calls that config eventually makes. Most of ParkerConfig does
// show up in those calls. The band bounds pick the VMID, DiskStorage turns the
// storage scan on, and Prefix lands in the created parker's name. Pool does
// not. The sweep that reads Pool runs outside the park, so a funnel that
// dropped the field would still satisfy every call-shaped assertion we could
// write, and that is exactly the silent failure this seam exists to catch.
// Reading the struct is the only assertion that catches it.
//
// It is held in an atomic.Pointer for the reason haResurrectorWarnOnce is,
// which is that a plain package var swapped by a test is an unsynchronized
// write the race detector is entitled to flag. The seam is deliberately
// narrow. The other parkers in this package, which are
// parkFreeFloatingStableID, the cross-node migrate park, and the managed park
// that runs against the allocation guard's own client, still call pve.ParkDisk
// directly, because nothing tests them through here.
var parkDiskImpl atomic.Pointer[parkDiskFunc]

func init() {
	production := parkDiskFunc(pve.ParkDisk)
	parkDiskImpl.Store(&production)
}

// parkDisk calls whatever the seam currently holds. Read it as pve.ParkDisk.
func parkDisk(
	ctx context.Context,
	c pve.Client,
	logger *log.Logger,
	node, bareVolid string,
	cfg pve.ParkerConfig,
	pctx pve.ParkContext,
) error {
	return (*parkDiskImpl.Load())(ctx, c, logger, node, bareVolid, cfg, pctx)
}

// setParkDiskForTest replaces the park for the duration of a test and returns a
// restore function.
//
// The swap itself is race-safe, but the seam is still process-wide, so two
// tests that swap it at the same time would take each other's replacement.
// Tests using it must not call t.Parallel, exactly as the tests using
// resetHAResurrectorWarnOnceForTest must not.
//
//	defer setParkDiskForTest(fn)()
func setParkDiskForTest(fn parkDiskFunc) func() {
	prev := parkDiskImpl.Swap(&fn)
	return func() { parkDiskImpl.Store(prev) }
}
