package pve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

func TestStorageContentFailureReasonsDoNotExposeBackendData(t *testing.T) {
	secret := "https://token:secret@private.invalid/volume"
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"cancel", fmt.Errorf("%s: %w", secret, context.Canceled), "listing_canceled"},
		{"deadline", context.DeadlineExceeded, "listing_deadline"},
		{"permission", &sdkerrors.PermissionError{What: secret}, "listing_permission_denied"},
		{"authentication", &sdkerrors.AuthenticationError{Realm: secret}, "listing_authentication_failed"},
		{"parameter", &sdkerrors.ParameterError{Usage: secret}, "listing_parameter_rejected"},
		{"server", &sdkerrors.APIError{Message: secret, HTTPCode: 500}, "listing_http_500"},
		{"invalidstatus", &sdkerrors.APIError{Message: secret, HTTPCode: 123456}, "listing_api_error"},
		{"tls", &sdkerrors.SSLError{Host: secret}, "listing_tls_error"},
		{"connection", &sdkerrors.ConnectionError{Host: secret}, "listing_connection_error"},
		{"timeout", &sdkerrors.TimeoutError{Operation: secret}, "listing_timeout"},
		{"unknown", errors.New(secret), "listing_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := storageContentFailure(tc.err)
			if StorageVolumeObservationReason(got) != tc.want {
				t.Fatalf("reason=%q want=%q", StorageVolumeObservationReason(got), tc.want)
			}
			if strings.Contains(got.Error(), secret) {
				t.Fatal("backend text leaked")
			}
			if !errors.Is(storageContentFailure(got), got) {
				t.Fatal("safe reason lost through membership layer")
			}
		})
	}
	if StorageVolumeObservationReason(errors.New(secret)) != "" {
		t.Fatal("untrusted error classified as observation")
	}
}
