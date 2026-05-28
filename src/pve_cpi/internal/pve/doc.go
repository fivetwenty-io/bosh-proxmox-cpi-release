// Package pve wraps the pve-apiclient-go SDK for use by the BOSH PVE CPI.
// It provides disk CID parsing and slot resolution, persistent-disk backend
// abstraction, storage classification, task-await helpers, VMID allocation,
// SDN wiring, SDK-to-BOSH error mapping, and stemcell-volume management.
//
// Test conventions:
//
// Stub panics: test doubles (e.g., diskMockQEMUService) panic on methods that
// the production code under test must never call. A panic in CI means the
// unit under test widened its call surface unexpectedly.
//
// No env-mutation: tests must not call t.Setenv or os.Setenv; package-level
// state is not safe to mutate across parallel subtests.
package pve
