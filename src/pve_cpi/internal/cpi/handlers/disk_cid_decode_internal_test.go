// Package handlers — internal tests for decodeDiskCID, the shared
// decode-and-log helper every disk handler routes its disk CID through.
package handlers

import (
	"context"
	"strings"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestDecodeDiskCID_ValidEnvelopeIsSilent keeps the happy path quiet: the Warn
// is a diagnostic for a rejected CID, not per-call noise on every disk
// operation.
func TestDecodeDiskCID_ValidEnvelopeIsSilent(t *testing.T) {
	t.Parallel()
	encoded, err := pve.EncodeDiskCID("local-lvm:vm-100-disk-0", &pve.DiskCIDMeta{Pool: "local-lvm", Node: "pve1"})
	if err != nil {
		t.Fatalf("EncodeDiskCID: %v", err)
	}

	logger, obs := log.NewObservedLogger(log.LevelDebug)
	bare, meta, decErr := decodeDiskCID(context.Background(), Deps{Logger: logger}, "has_disk", encoded)
	if decErr != nil {
		t.Fatalf("a CPI-issued envelope must decode: %v", decErr)
	}
	if bare != "local-lvm:vm-100-disk-0" {
		t.Errorf("bare CID = %q, want %q", bare, "local-lvm:vm-100-disk-0")
	}
	if meta == nil || meta.Node != "pve1" {
		t.Errorf("meta not returned intact: %+v", meta)
	}
	if obs.Len() != 0 {
		t.Errorf("a valid CID must log nothing, got: %+v", obs.All())
	}
}

// TestDecodeDiskCID_RejectionShapes pins that each distinct codec rejection
// reason reaches the log rather than being flattened into "disk not found".
// These are the five shapes ParseEncodedDiskCID distinguishes.
func TestDecodeDiskCID_RejectionShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cid  string
	}{
		{"bare volid (pre-envelope form)", "local-lvm-data:vm-11018-disk-0"},
		{"unknown prefix", "pvx-abc"},
		{"bad base64url", "pvd-not!valid!base64"},
		{"bad gzip under the pvz- prefix", "pvz-YWJjZGVm"},
		{"empty payload", "pvd-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logger, obs := log.NewObservedLogger(log.LevelDebug)
			_, _, err := decodeDiskCID(context.Background(), Deps{Logger: logger}, "delete_disk", tc.cid)
			if err == nil {
				t.Fatal("expected a decode rejection")
			}
			if !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
				t.Errorf("error type: want DiskNotFound, got %T %v", err, err)
			}

			entries := obs.All()
			if len(entries) != 1 {
				t.Fatalf("expected exactly one log entry, got %d: %+v", len(entries), entries)
			}
			e := entries[0]
			if e.Level != log.LevelWarn {
				t.Errorf("entry logged at %v, want Warn", e.Level)
			}
			if !strings.HasPrefix(e.Message, "delete_disk: ") {
				t.Errorf("message must name the method, got %q", e.Message)
			}
			if got, _ := e.Attrs["disk_cid"].(string); got != tc.cid {
				t.Errorf("disk_cid attr = %q, want %q", got, tc.cid)
			}
			reason, _ := e.Attrs["error"].(string)
			if reason == "" {
				t.Error("the codec's rejection reason must reach the log")
			}
			// The reason must be the codec's, not a restatement of DiskNotFound.
			if strings.Contains(reason, "disk not found") {
				t.Errorf("logged reason is the DiskNotFound text, not the codec's: %q", reason)
			}
		})
	}
}

// TestDecodeDiskCID_NilLoggerDoesNotPanic guards the Deps.Log fallback: a
// handler constructed without a logger must still return the error rather than
// panicking inside the diagnostic.
func TestDecodeDiskCID_NilLoggerDoesNotPanic(t *testing.T) {
	t.Parallel()
	_, _, err := decodeDiskCID(context.Background(), Deps{}, "has_disk", "not-an-envelope")
	if !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Errorf("want DiskNotFound, got %T %v", err, err)
	}
}
