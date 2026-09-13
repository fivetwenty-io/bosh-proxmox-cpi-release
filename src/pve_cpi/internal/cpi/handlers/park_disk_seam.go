package handlers

import (
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// parkDisk is pve.ParkDisk behind a package variable, and it is a test seam
// rather than an abstraction over parking. Production never reassigns it, and
// a caller that wants to park a disk should read it as pve.ParkDisk.
//
// It exists because the three funnels that build a park config out of
// parkerWriteConfigFor -- parkFreshDisk, handleAlreadyDetachedParked, and
// parkAfterDetach -- have to be tested on the config they hand down, not on
// the PVE calls that config eventually makes. Most of ParkerConfig does show
// up in those calls: the band bounds pick the VMID, DiskStorage turns the
// storage scan on, and Prefix lands in the created parker's name. Pool does
// not. The sweep that reads Pool runs outside the park, so a funnel that
// dropped the field would still satisfy every call-shaped assertion we could
// write, and that is exactly the silent failure this slice exists to prevent.
// Reading the struct is the only assertion that catches it.
//
// The seam is deliberately narrow. The other parkers in this package
// (parkFreeFloatingStableID, the cross-node migrate park, and the managed park
// that runs against the allocation guard's own client) still call pve.ParkDisk
// directly, because nothing tests them through here. A test that swaps this
// variable must restore it and must not call t.Parallel, since the variable is
// process-wide; see the seam discipline in ha_warn_seam.go.
var parkDisk = pve.ParkDisk
