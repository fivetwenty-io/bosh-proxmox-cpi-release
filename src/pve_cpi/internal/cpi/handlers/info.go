package handlers

import (
	"context"
	"encoding/json"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
)

// infoResult is the fixed payload returned by the info CPI method.
// api_version=2 is required for BOSH Director CPI v2 routing.
// stemcell_formats lists every image format accepted by create_stemcell.
type infoResult struct {
	APIVersion      int      `json:"api_version"`
	StemcellFormats []string `json:"stemcell_formats"`
}

// HandleInfo returns a Handler that responds to the BOSH CPI "info" method.
// The response is fully static: no PVE SDK calls are made. The Director uses
// this to confirm api_version=2 support and to discover acceptable stemcell
// image formats before uploading a stemcell.
//
// Arguments: none (the BOSH spec defines an empty argument list for info).
// Returns: infoResult — api_version + stemcell_formats.
// Errors: none (cannot fail).
func HandleInfo(_ Deps) cpi.Handler {
	result := infoResult{
		APIVersion: 2,
		StemcellFormats: []string{
			"openstack-qcow2",
			"openstack-raw",
			"pve-qcow2",
			"general-qcow2",
			"general-raw",
		},
	}

	return cpi.HandlerFunc(func(_ context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
		return result, nil
	})
}
