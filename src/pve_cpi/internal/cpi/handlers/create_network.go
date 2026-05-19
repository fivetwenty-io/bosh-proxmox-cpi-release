// Package handlers — create_network handler.
//
// PVE bridge management is out of scope for this CPI (R1, R3). The BOSH CPI v2
// spec marks create_network as optional. Return NotImplemented so the Director
// knows the method is intentionally absent, not broken.
package handlers

import (
	"context"
	"encoding/json"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
)

// HandleCreateNetwork returns a Handler that always returns NotImplemented.
//
// Args[0]: network_spec (map) — accepted but ignored; PVE bridge management is
// out of scope.
func HandleCreateNetwork(_ Deps) cpi.Handler {
	return cpi.HandlerFunc(func(_ context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// Validate that at least one argument (network_spec) was supplied so that
		// malformed Director calls are distinguishable from the intentional
		// NotImplemented response. An absent spec is still rejected.
		if len(args) < 1 {
			return nil, cpierrors.Cloud("create_network: missing required argument network_spec")
		}
		// Confirm the argument is at least valid JSON (not a bare null token is
		// acceptable per spec; null network_spec is still a valid call).
		var spec json.RawMessage
		if err := json.Unmarshal(args[0], &spec); err != nil {
			return nil, cpierrors.Cloud("create_network: network_spec is not valid JSON: %s", err.Error())
		}
		return nil, cpierrors.NotImplemented("create_network")
	})
}
