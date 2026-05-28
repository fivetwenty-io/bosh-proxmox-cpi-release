// Package handlers_test — shared test fixtures used across multiple handler test files.
package handlers_test

import (
	"encoding/json"

	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
)

// rawVnet returns a GetSdnVnetsResponse containing the given zone field.
// Used by delete_network_test.go and network_sdn_test.go.
func rawVnet(zone string) *sdkcluster.GetSdnVnetsResponse {
	b, _ := json.Marshal(map[string]any{"vnet": "net01", "zone": zone})
	raw := sdkcluster.GetSdnVnetsResponse(b)
	return &raw
}
