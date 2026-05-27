// Package pve wraps the pve-apiclient-go SDK for use by the BOSH PVE CPI.
// It provides disk CID parsing and slot resolution, persistent-disk backend
// abstraction, storage classification, task-await helpers, VMID allocation,
// SDN wiring, SDK-to-BOSH error mapping, and stemcell-volume management.
package pve
