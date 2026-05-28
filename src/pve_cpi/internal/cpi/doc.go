// Package cpi implements the BOSH CPI dispatcher: a handler registry that routes
// JSON-RPC requests to per-method handlers. All 22 canonical CPI methods are
// pre-registered as NotImplemented placeholders; callers override slots via Register.
package cpi
