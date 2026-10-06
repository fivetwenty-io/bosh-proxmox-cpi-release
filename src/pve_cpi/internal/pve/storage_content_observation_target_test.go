package pve_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestStorageContentObservationErrorCarriesTarget pins that a failed content
// listing names the storage it listed and the node it asked, beside its
// reason, so a caller can say which listing failed. The target and the reason
// survive the joins and wraps callers put around the error, and the error's
// text still carries no backend message.
func TestStorageContentObservationErrorCarriesTarget(t *testing.T) {
	t.Parallel()
	const secret = "https://token:secret@pve.invalid/api2/json"
	for _, tc := range []struct {
		name   string
		listFn func(context.Context, string, string, *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error)
		reason string
	}{
		{
			name: "a server error",
			listFn: func(context.Context, string, string, *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
				return nil, &sdkerrors.APIError{HTTPCode: 500, Message: secret}
			},
			reason: "listing_http_500",
		},
		{
			name: "no listing at all",
			listFn: func(context.Context, string, string, *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
				return nil, nil
			},
			reason: "listing_data_missing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe := absenceProbe{listFn: tc.listFn, visible: true}
			_, err := pve.ObserveStorageVolumePresence(t.Context(), probe.client(), "pve-a", absenceVolid)
			if err == nil {
				t.Fatal("a failed listing reported presence")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "https://") {
				t.Fatalf("error %q carries backend text", err)
			}
			for _, wrapped := range []error{err, errors.Join(err, errors.New("other")), fmt.Errorf("verify cleanup volume: %w", err)} {
				if reason := pve.StorageVolumeObservationReason(wrapped); reason != tc.reason {
					t.Fatalf("reason through %T = %q, want %q", wrapped, reason, tc.reason)
				}
				storage, node := pve.StorageVolumeObservationTarget(wrapped)
				if storage != "nfs-images" || node != "pve-a" {
					t.Fatalf("target through %T = %q on %q, want %q on pve-a", wrapped, storage, node, "nfs-images")
				}
			}
		})
	}
	if storage, node := pve.StorageVolumeObservationTarget(errors.New("unrelated")); storage != "" || node != "" {
		t.Fatalf("an unrelated error named %q on %q", storage, node)
	}
}
