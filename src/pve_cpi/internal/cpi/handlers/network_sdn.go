// Shared helpers for create_network and delete_network.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	pveerr "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

// vnetNameRE matches valid PVE vnet names: 1–8 lowercase alphanumeric characters.
var vnetNameRE = regexp.MustCompile(`^[a-z0-9]{1,8}$`)

// networkSpec is the BOSH create_network network_spec argument shape.
// Type and NetmaskBits are parsed for completeness but not consumed by the
// network handlers; they are available for callers that inspect the spec
// directly (e.g. future address-management extensions).
type networkSpec struct {
	Type            string         `json:"type"`
	Range           string         `json:"range,omitempty"`
	Gateway         string         `json:"gateway,omitempty"`
	NetmaskBits     int            `json:"netmask_bits,omitempty"`
	CloudProperties map[string]any `json:"cloud_properties,omitempty"`
}

// parseNetworkSpec decodes a raw JSON arg into networkSpec.
// Returns cpierrors.Cloud on malformed JSON.
func parseNetworkSpec(raw json.RawMessage) (*networkSpec, error) {
	var spec networkSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, cpierrors.Cloud("create_network: network_spec is not valid JSON: %s", err.Error())
	}
	if spec.CloudProperties == nil {
		spec.CloudProperties = map[string]any{}
	}
	return &spec, nil
}

// cpStr extracts a string cloud_property by key; returns "" when absent or non-string.
func cpStr(cp map[string]any, key string) string {
	v, ok := cp[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// validateVnetName returns cpierrors.Cloud when name is invalid per PVE rules.
func validateVnetName(name string) error {
	if name == "" {
		return cpierrors.Cloud("create_network: cloud_properties.vnet is required for SDN path")
	}
	if !vnetNameRE.MatchString(name) {
		return cpierrors.Cloud(
			"create_network: vnet name %q is invalid — must be 1–8 lowercase alphanumeric characters [a-z0-9]",
			name,
		)
	}
	return nil
}

// isSDNNotFound returns true when err indicates an absent SDN resource.
// Handles the pveerr.APIError chain from cluster_gen error wrapping, plus PVE's
// SDN-specific absence message: a GET/DELETE on a missing vnet/zone/subnet does
// not return HTTP 404 — it returns a generic error (code 0) whose message reads
// "sdn vnet 'X' does not exist" (likewise for zones/subnets). Without matching
// that text, delete_network would surface the probe error instead of falling
// back to the bridge path (or treating the SDN resource as already gone).
func isSDNNotFound(err error) bool {
	if err == nil {
		return false
	}
	// Direct sentinel.
	if errors.Is(err, pveerr.ErrNotFound) {
		return true
	}
	// Unwrap APIError and check IsNotFound().
	var apiErr *pveerr.APIError
	if errors.As(err, &apiErr) {
		if apiErr.IsNotFound() {
			return true
		}
	}
	// PVE SDN absence message (not surfaced as HTTP 404).
	return strings.Contains(strings.ToLower(err.Error()), "does not exist")
}

// isSDNConflict returns true when err indicates a 409 Conflict (already exists).
func isSDNConflict(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, pveerr.ErrConflict) {
		return true
	}
	var apiErr *pveerr.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == 409 || apiErr.HTTPCode == 409
	}
	return false
}

// applySDN calls UpdateSdn (PUT /cluster/sdn) to commit staged SDN changes.
//
// PVE simple-zone applies are synchronous and return no body. Async zone types
// (vlan, vxlan, evpn) may return a UPID string. When a UPID is present the
// function waits for the task using pve.AwaitTaskWithLogger so that subsequent
// operations (ListSdnVnets, vnet realization checks) observe the committed state.
// Node for task polling is extracted from the UPID itself (field 2,
// "UPID:<node>:..."); if the UPID is malformed or the node field is empty the
// function falls back to config.Node. If node is also empty a warning is logged
// and the apply is treated as successful — the HTTP 200 already confirmed the
// request was accepted; operator should verify manually for non-simple zones.
func applySDN(ctx context.Context, deps Deps, clusterSvc sdkcluster.Service, opCtx string) error {
	resp, err := clusterSvc.UpdateSdn(ctx, nil)
	if err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("%s: apply SDN", opCtx))
	}
	if resp == nil || len(*resp) == 0 {
		// Nil / empty body — simple zone synchronous apply; no task to await.
		return nil
	}

	// Attempt to decode a UPID from the response.
	upid, upidErr := pve.UPIDFromRaw(*resp)
	if upidErr != nil {
		// Response body is not a recognizable UPID shape — log and continue.
		// The HTTP 200 confirms the apply was accepted; this is informational.
		if deps.Logger != nil {
			deps.Logger.Warn(
				fmt.Sprintf("%s: apply SDN returned unrecognized body; task completion unconfirmed", opCtx),
				log.String("body", string(*resp)),
			)
		}
		return nil
	}
	if upid == "" {
		// Empty string decoded: synchronous / no-task response.
		return nil
	}

	// UPID present — extract node from "UPID:<node>:<rest>" format.
	node := deps.Config.Node
	if parts := strings.SplitN(upid, ":", 3); len(parts) >= 2 && parts[1] != "" {
		node = parts[1]
	}
	if node == "" {
		if deps.Logger != nil {
			deps.Logger.Warn(
				fmt.Sprintf("%s: apply SDN returned UPID but cannot determine node; task completion unconfirmed", opCtx),
				log.String("upid", upid),
			)
		}
		return nil
	}

	return pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, deps.Logger)
}
