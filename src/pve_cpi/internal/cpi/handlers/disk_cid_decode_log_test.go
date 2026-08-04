package handlers_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// ---------------------------------------------------------------------------
// Disk-CID decode diagnosability: every disk handler must log the codec's
// precise rejection reason before mapping a malformed CID to DiskNotFound.
//
// Before this, each handler discarded decErr from pve.ParseEncodedDiskCID and
// returned a bare "disk not found: <cid>". An operator who hand-edited a CID,
// or replayed one from a pre-envelope release, saw a Director task fail with
// "disk not found", went looking for a missing volume on PVE, and found the
// volume sitting right there. The reason (bad prefix, bad base64url, bad gzip,
// oversized inflation, empty volid) was recoverable from neither the CPI
// response nor the logs.
//
// The error TYPE is deliberately unchanged: DiskNotFound is what the Director
// knows how to act on for orphan cleanup, and a CID this CPI could not have
// emitted names a disk this CPI does not have.
// ---------------------------------------------------------------------------

// cidLogDeps builds the minimal Deps a handler needs to reach its disk-CID
// decode. No PVE client is wired: every handler decodes the CID before it
// touches PVE, so a decode-failure test never gets that far.
func cidLogDeps(logger *log.Logger) handlers.Deps {
	cfg := &config.CPIConfig{Node: "pve1"}
	cfg.ApplyDefaults()
	return handlers.Deps{Config: cfg, Logger: logger}
}

func rawArgs(t *testing.T, vals ...any) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, 0, len(vals))
	for _, v := range vals {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %v: %v", v, err)
		}
		out = append(out, b)
	}
	return out
}

// TestDiskHandlers_LogDecodeReasonOnMalformedCID covers every disk handler that
// decodes an encoded disk CID. Each must (a) still return DiskNotFound, and
// (b) emit a Warn carrying the codec's reason and the offending CID.
func TestDiskHandlers_LogDecodeReasonOnMalformedCID(t *testing.T) {
	t.Parallel()

	// A bare volid: the pre-envelope form the codec now hard-rejects. This is
	// exactly the shape the live V8 run replayed, and the shape an operator is
	// most likely to paste by hand.
	const badCID = "local-lvm-data:vm-11018-disk-0"

	cases := []struct {
		method string
		handle func(handlers.Deps) handlers.Handler
		args   []json.RawMessage
	}{
		{"has_disk", handlers.HandleHasDisk, rawArgs(t, badCID)},
		{"delete_disk", handlers.HandleDeleteDisk, rawArgs(t, badCID)},
		{"attach_disk", handlers.HandleAttachDisk, rawArgs(t, "100", badCID)},
		{"detach_disk", handlers.HandleDetachDisk, rawArgs(t, "100", badCID)},
		{"resize_disk", handlers.HandleResizeDisk, rawArgs(t, badCID, 2048)},
		{"snapshot_disk", handlers.HandleSnapshotDisk, rawArgs(t, badCID, map[string]any{})},
		{"update_disk", handlers.HandleUpdateDisk, rawArgs(t, badCID, map[string]any{})},
	}

	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			t.Parallel()
			logger, obs := log.NewObservedLogger(log.LevelDebug)
			h := tc.handle(cidLogDeps(logger))

			_, err := h.Handle(context.Background(), tc.args, jsonrpc.Context{})
			if err == nil {
				t.Fatal("expected an error for a malformed disk CID")
			}
			if !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
				t.Errorf("error type: want DiskNotFound (the Director acts on it), got %T %v", err, err)
			}

			entries := obs.All()
			var found bool
			for _, e := range entries {
				if e.Level != log.LevelWarn {
					continue
				}
				reason, _ := e.Attrs["error"].(string)
				cid, _ := e.Attrs["disk_cid"].(string)
				// The codec's bad-prefix message; see pve.ParseEncodedDiskCID.
				if strings.Contains(reason, "envelope prefix") && cid == badCID {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected a Warn naming the decode reason and the CID, got %d entries: %+v", len(entries), entries)
			}
		})
	}
}

// TestHandleSetDiskMetadata_LogsDecodeReason covers set_disk_metadata, whose
// constructor returns the cpi.Handler interface rather than handlers.Handler.
func TestHandleSetDiskMetadata_LogsDecodeReason(t *testing.T) {
	t.Parallel()
	const badCID = "local-lvm-data:vm-11018-disk-0"

	logger, obs := log.NewObservedLogger(log.LevelDebug)
	h := handlers.HandleSetDiskMetadata(cidLogDeps(logger))

	_, err := h.Handle(context.Background(), rawArgs(t, badCID, map[string]any{"director": "x"}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("expected an error for a malformed disk CID")
	}
	if !cpierrors.IsType(err, cpierrors.TypeDiskNotFound) {
		t.Errorf("error type: want DiskNotFound, got %T %v", err, err)
	}

	var found bool
	for _, e := range obs.All() {
		if e.Level == log.LevelWarn && strings.Contains(e.Message, "set_disk_metadata") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a Warn naming the method, got: %+v", obs.All())
	}
}
