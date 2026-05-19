// Package handlers — delete_network handler.
//
// PVE bridge management is out of scope for this CPI (R1, R3). The BOSH CPI v2
// spec marks delete_network as optional. Return NotImplemented so the Director
// knows the method is intentionally absent, not broken.
package handlers

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
)

// HandleDeleteNetwork returns a Handler that always returns NotImplemented.
//
// Args[0]: network_cid (string) — accepted but ignored; PVE bridge management
// is out of scope.
func HandleDeleteNetwork(_ Deps) cpi.Handler {
	return cpi.HandlerFunc(func(_ context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
		// Validate that at least one argument (network_cid) was supplied.
		if len(args) < 1 {
			return nil, cpierrors.Cloud("delete_network: missing required argument network_cid")
		}
		// Unmarshal and validate the network_cid is a non-empty string.
		var networkCID string
		if err := json.Unmarshal(args[0], &networkCID); err != nil {
			return nil, cpierrors.Cloud("delete_network: network_cid must be a JSON string: %s", err.Error())
		}
		if strings.TrimSpace(networkCID) == "" {
			return nil, cpierrors.Cloud("delete_network: network_cid must not be empty")
		}
		return nil, cpierrors.NotImplemented("delete_network")
	})
}
