package tasks

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Sentinel errors for ParseUPID validation failures.
var (
	// ErrUPIDFieldCount is returned when the UPID string does not have the expected number of colon-separated fields.
	ErrUPIDFieldCount = errors.New("tasks: ParseUPID: unexpected field count")
	// ErrUPIDPrefix is returned when the UPID string does not start with the "UPID" prefix.
	ErrUPIDPrefix = errors.New("tasks: ParseUPID: missing UPID prefix")
)

// UPID holds the parsed fields of a Proxmox UPID string.
// Format: "UPID:<node>:<pid-hex>:<pstart-hex>:<starttime-hex>:<type>:<id>:<user>:".
type UPID struct {
	// Node is the PVE node name the task runs on.
	Node string
	// PID is the process ID (hex-encoded in the raw string).
	PID uint64
	// PStart is the process start counter (hex-encoded in the raw string).
	PStart uint64
	// StartTime is the Unix start time (hex-encoded in the raw string).
	StartTime uint64
	// Type is the task type (e.g. "qmstart", "vzdump").
	Type string
	// ID is the task subject (e.g. VMID "100" or empty string).
	ID string
	// User is the PVE user that initiated the task (e.g. "root@pam").
	User string
	// Raw is the original unparsed UPID string.
	Raw string
}

// ParseUPID parses a Proxmox UPID string into a UPID struct.
// Expected format: "UPID:<node>:<pid-hex>:<pstart-hex>:<starttime-hex>:<type>:<id>:<user>:"
// The trailing colon is required (standard PVE format).
func ParseUPID(raw string) (UPID, error) {
	// PVE always produces a trailing colon; strip it for uniform field splitting.
	trimmed := strings.TrimSuffix(raw, ":")
	parts := strings.Split(trimmed, ":")

	// After trimming the trailing ":" we expect exactly 8 fields:
	//   parts[0]="UPID"  [1]=node [2]=pid [3]=pstart [4]=starttime [5]=type [6]=id [7]=user
	const expectedParts = 8
	if len(parts) != expectedParts {
		return UPID{}, fmt.Errorf("%w: expected %d fields, got %d in %q",
			ErrUPIDFieldCount, expectedParts, len(parts), raw)
	}

	if parts[0] != "UPID" {
		return UPID{}, fmt.Errorf("%w: got %q in %q", ErrUPIDPrefix, parts[0], raw)
	}

	pid, err := strconv.ParseUint(parts[2], 16, 64)
	if err != nil {
		return UPID{}, fmt.Errorf("tasks: ParseUPID: invalid PID hex %q in %q: %w", parts[2], raw, err)
	}

	pstart, err := strconv.ParseUint(parts[3], 16, 64)
	if err != nil {
		return UPID{}, fmt.Errorf("tasks: ParseUPID: invalid PStart hex %q in %q: %w", parts[3], raw, err)
	}

	startTime, err := strconv.ParseUint(parts[4], 16, 64)
	if err != nil {
		return UPID{}, fmt.Errorf("tasks: ParseUPID: invalid StartTime hex %q in %q: %w", parts[4], raw, err)
	}

	return UPID{
		Node:      parts[1],
		PID:       pid,
		PStart:    pstart,
		StartTime: startTime,
		Type:      parts[5],
		ID:        parts[6],
		User:      parts[7],
		Raw:       raw,
	}, nil
}

// WaitForUPID parses the node from upid and delegates to s.Wait.
// This is a convenience wrapper so callers need not extract the node themselves.
func (s *service) WaitForUPID(ctx context.Context, upid string, opts *WaitOptions) (*Status, error) {
	parsed, err := ParseUPID(upid)
	if err != nil {
		return nil, err
	}

	return s.Wait(ctx, parsed.Node, upid, opts)
}
