// Package log provides a slog-backed structured logger for the BOSH Proxmox CPI.
//
// CPI protocol uses stdout for JSON-RPC; all log output targets stderr (or a
// caller-supplied io.Writer) to avoid corrupting the protocol stream.
package log
