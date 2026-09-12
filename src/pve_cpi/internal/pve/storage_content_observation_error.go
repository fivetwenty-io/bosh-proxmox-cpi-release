package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// storageContentObservationError retains only a bounded diagnostic category.
// Backend messages, endpoint URLs, and response bodies must not reach logs.
type storageContentObservationError struct {
	reason string
}

func (e *storageContentObservationError) Error() string {
	return "managed volume content listing unavailable"
}

// StorageVolumeObservationReason returns a safe category for a failed listing.
// An empty result means the error did not originate from this observation path.
func StorageVolumeObservationReason(err error) string {
	var observation *storageContentObservationError
	if errors.As(err, &observation) {
		return observation.reason
	}
	return ""
}

func storageContentFailure(err error) error {
	if StorageVolumeObservationReason(err) != "" {
		return err
	}
	var reason string
	switch {
	case errors.Is(err, context.Canceled):
		reason = "listing_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		reason = "listing_deadline"
	default:
		reason = storageContentFailureCategory(err)
	}
	return &storageContentObservationError{reason: reason}
}

func storageContentFailureCategory(err error) string {
	var permission *sdkerrors.PermissionError
	var authentication *sdkerrors.AuthenticationError
	var parameter *sdkerrors.ParameterError
	var api *sdkerrors.APIError
	var ssl *sdkerrors.SSLError
	var timeout *sdkerrors.TimeoutError
	var connection *sdkerrors.ConnectionError
	var network net.Error
	var syntax *json.SyntaxError
	switch {
	case errors.As(err, &permission):
		return "listing_permission_denied"
	case errors.As(err, &authentication):
		return "listing_authentication_failed"
	case errors.As(err, &parameter):
		return "listing_parameter_rejected"
	case errors.As(err, &api):
		if api.HTTPCode >= 100 && api.HTTPCode <= 599 {
			return fmt.Sprintf("listing_http_%d", api.HTTPCode)
		}
		return "listing_api_error"
	case errors.As(err, &ssl):
		return "listing_tls_error"
	case errors.As(err, &timeout):
		return "listing_timeout"
	case errors.As(err, &connection):
		return "listing_connection_error"
	case errors.As(err, &network):
		if network.Timeout() {
			return "listing_timeout"
		}
		return "listing_network_error"
	case errors.As(err, &syntax):
		return "listing_json_invalid"
	default:
		return "listing_unavailable"
	}
}
