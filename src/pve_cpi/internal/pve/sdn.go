// SDN backend adapter: typed read/delete primitives over PVE SDN zones,
// vnets, and vnet subnets. Each primitive returns errors classified by
// WrapError; 404 responses surface as ErrSDNNotFound so callers can
// distinguish missing entities from generic failures. Create and apply
// operations for SDN objects are handled inline by the create_network and
// create_vm_routes handlers, which call the SDK directly.
//
// PVE quirks captured here:
//   - SDN zone create has no description/notes/comment field; callers cannot
//     tag CPI-owned zones in-band. Identification of CPI-owned objects must
//     be done by name convention at the caller.
//   - "Apply" is PUT /cluster/sdn (cluster.Service.UpdateSdn). The similarly
//     named CreateSdnRollback endpoint reverts pending changes — easy to
//     confuse but the opposite operation.
//   - Zone, vnet, and subnet create/delete return synchronously (no UPID
//     task); only UpdateSdn potentially returns a task identifier.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// ErrSDNNotFound is the sentinel returned when an SDN zone, vnet, or subnet
// lookup, delete, or get resolves to HTTP 404. Callers can match with
// errors.Is(err, ErrSDNNotFound) and treat the condition as success for
// idempotent delete flows.
var ErrSDNNotFound = errors.New("pve sdn entity not found")

// isSDNNotFoundShape reports whether err represents an SDN missing-entity
// response in any shape PVE is known to return:
//
//   - HTTP 404 (handled by IsNotFound — covers GET /cluster/sdn/zones/X and
//     /subnets/X when those endpoints return a proper 404).
//   - HTTP 500 / code 0 with body text "sdn vnet 'X' does not exist"
//     (or zone/subnet variants). Observed on real PVE 8 for GET
//     /cluster/sdn/vnets/<missing> — the perl handler die()s with this
//     message rather than returning a structured 404.
//
// Real deployments return both shapes depending on the PVE version and the
// endpoint, so callers must detect both to keep idempotent delete flows
// correct.
func isSDNNotFoundShape(err error) bool {
	if err == nil {
		return false
	}
	if IsNotFound(err) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "does not exist")
}

// ---------------------------------------------------------------------------
// Zone CRUD
// ---------------------------------------------------------------------------

// SDNZone is the decoded shape of a single row from GET /cluster/sdn/zones
// and the singleton from GET /cluster/sdn/zones/{zone}. Fields beyond Zone
// and Type are best-effort — PVE returns a sparse object whose keys depend
// on the zone plugin type. Raw preserves the unparsed JSON for callers that
// need fields not promoted here.
type SDNZone struct {
	Zone   string          `json:"zone"`
	Type   string          `json:"type"`
	Bridge string          `json:"bridge,omitempty"`
	MTU    int64           `json:"mtu,omitempty"`
	Nodes  string          `json:"nodes,omitempty"`
	Raw    json.RawMessage `json:"-"`
}

// DeleteSDNZone issues DELETE /cluster/sdn/zones/{zone}.
//
// Returns nil on success, ErrSDNNotFound (wrapped) when the zone is absent,
// or a WrapError-classified failure otherwise. Callers wanting idempotent
// delete should check errors.Is(err, ErrSDNNotFound) and treat as success.
func DeleteSDNZone(ctx context.Context, c Client, zone string) error {
	if ctx == nil {
		return cpierrors.Cloud("DeleteSDNZone: ctx must not be nil")
	}
	if c == nil {
		return cpierrors.Cloud("DeleteSDNZone: client must not be nil")
	}
	if strings.TrimSpace(zone) == "" {
		return cpierrors.Cloud("DeleteSDNZone: zone is required")
	}
	svc := c.Cluster()
	if svc == nil {
		return cpierrors.Cloud("DeleteSDNZone: cluster service unavailable")
	}

	if err := svc.DeleteSdnZones(ctx, zone, nil); err != nil {
		if isSDNNotFoundShape(err) {
			return cpierrors.WrapAs(ErrSDNNotFound, cpierrors.TypeCloud,
				fmt.Sprintf("DeleteSDNZone: zone %q not found", zone))
		}
		return WrapError(err)
	}
	return nil
}

// ListSDNZones issues GET /cluster/sdn/zones and decodes each row into an
// SDNZone. Empty cluster → empty slice (non-nil). SDK errors flow through
// WrapError. The pending state is requested (pending=true) so callers can
// observe zones created but not yet applied via ApplySDN.
func ListSDNZones(ctx context.Context, c Client) ([]SDNZone, error) {
	if ctx == nil {
		return nil, cpierrors.Cloud("ListSDNZones: ctx must not be nil")
	}
	if c == nil {
		return nil, cpierrors.Cloud("ListSDNZones: client must not be nil")
	}
	svc := c.Cluster()
	if svc == nil {
		return nil, cpierrors.Cloud("ListSDNZones: cluster service unavailable")
	}

	pending := true
	resp, err := svc.ListSdnZones(ctx, &sdkcluster.ListSdnZonesParams{Pending: &pending})
	if err != nil {
		return nil, WrapError(err)
	}
	if resp == nil {
		return []SDNZone{}, nil
	}
	out := make([]SDNZone, 0, len(*resp))
	for i, raw := range *resp {
		var z SDNZone
		if err := json.Unmarshal(raw, &z); err != nil {
			return nil, cpierrors.Wrap(err,
				fmt.Sprintf("ListSDNZones: decode row %d", i))
		}
		z.Raw = append(json.RawMessage(nil), raw...)
		out = append(out, z)
	}
	return out, nil
}

// GetSDNZone issues GET /cluster/sdn/zones/{zone}.
//
// Returns the decoded zone on success, ErrSDNNotFound (wrapped) when the
// zone is absent, or a WrapError-classified failure otherwise.
func GetSDNZone(ctx context.Context, c Client, zone string) (*SDNZone, error) {
	if ctx == nil {
		return nil, cpierrors.Cloud("GetSDNZone: ctx must not be nil")
	}
	if c == nil {
		return nil, cpierrors.Cloud("GetSDNZone: client must not be nil")
	}
	if strings.TrimSpace(zone) == "" {
		return nil, cpierrors.Cloud("GetSDNZone: zone is required")
	}
	svc := c.Cluster()
	if svc == nil {
		return nil, cpierrors.Cloud("GetSDNZone: cluster service unavailable")
	}

	pending := true
	resp, err := svc.GetSdnZones(ctx, zone, &sdkcluster.GetSdnZonesParams{Pending: &pending})
	if err != nil {
		if isSDNNotFoundShape(err) {
			return nil, cpierrors.WrapAs(ErrSDNNotFound, cpierrors.TypeCloud,
				fmt.Sprintf("GetSDNZone: zone %q not found", zone))
		}
		return nil, WrapError(err)
	}
	if resp == nil {
		return nil, cpierrors.WrapAs(ErrSDNNotFound, cpierrors.TypeCloud,
			fmt.Sprintf("GetSDNZone: zone %q not found (empty response)", zone))
	}
	var z SDNZone
	if err := json.Unmarshal(*resp, &z); err != nil {
		return nil, cpierrors.Wrap(err, "GetSDNZone: decode response")
	}
	// GET single-zone responses may omit the zone id; populate from arg
	// when the server response is sparse, so callers see the expected name.
	if z.Zone == "" {
		z.Zone = zone
	}
	z.Raw = append(json.RawMessage(nil), *resp...)
	return &z, nil
}

// ---------------------------------------------------------------------------
// Vnet CRUD
// ---------------------------------------------------------------------------

// SDNVnet is the decoded shape of a vnet row. Zone is promoted because
// callers commonly derive the parent zone from a vnet name lookup.
type SDNVnet struct {
	Vnet      string          `json:"vnet"`
	Zone      string          `json:"zone"`
	Alias     string          `json:"alias,omitempty"`
	Tag       int64           `json:"tag,omitempty"`
	Vlanaware int64           `json:"vlanaware,omitempty"`
	Type      string          `json:"type,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

// DeleteSDNVnet issues DELETE /cluster/sdn/vnets/{vnet}. NotFound surfaces
// as ErrSDNNotFound (wrapped).
func DeleteSDNVnet(ctx context.Context, c Client, vnet string) error {
	if ctx == nil {
		return cpierrors.Cloud("DeleteSDNVnet: ctx must not be nil")
	}
	if c == nil {
		return cpierrors.Cloud("DeleteSDNVnet: client must not be nil")
	}
	if strings.TrimSpace(vnet) == "" {
		return cpierrors.Cloud("DeleteSDNVnet: vnet is required")
	}
	svc := c.Cluster()
	if svc == nil {
		return cpierrors.Cloud("DeleteSDNVnet: cluster service unavailable")
	}

	if err := svc.DeleteSdnVnets(ctx, vnet, nil); err != nil {
		if isSDNNotFoundShape(err) {
			return cpierrors.WrapAs(ErrSDNNotFound, cpierrors.TypeCloud,
				fmt.Sprintf("DeleteSDNVnet: vnet %q not found", vnet))
		}
		return WrapError(err)
	}
	return nil
}

// ListSDNVnets issues GET /cluster/sdn/vnets and decodes each row.
// Pending=true so callers observe uncommitted creates.
func ListSDNVnets(ctx context.Context, c Client) ([]SDNVnet, error) {
	if ctx == nil {
		return nil, cpierrors.Cloud("ListSDNVnets: ctx must not be nil")
	}
	if c == nil {
		return nil, cpierrors.Cloud("ListSDNVnets: client must not be nil")
	}
	svc := c.Cluster()
	if svc == nil {
		return nil, cpierrors.Cloud("ListSDNVnets: cluster service unavailable")
	}

	pending := true
	resp, err := svc.ListSdnVnets(ctx, &sdkcluster.ListSdnVnetsParams{Pending: &pending})
	if err != nil {
		return nil, WrapError(err)
	}
	if resp == nil {
		return []SDNVnet{}, nil
	}
	out := make([]SDNVnet, 0, len(*resp))
	for i, raw := range *resp {
		var v SDNVnet
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, cpierrors.Wrap(err,
				fmt.Sprintf("ListSDNVnets: decode row %d", i))
		}
		v.Raw = append(json.RawMessage(nil), raw...)
		out = append(out, v)
	}
	return out, nil
}

// GetSDNVnet issues GET /cluster/sdn/vnets/{vnet}. NotFound surfaces as
// ErrSDNNotFound (wrapped). The returned SDNVnet exposes Zone so callers
// can chain into Zone-scoped operations without a separate list call.
func GetSDNVnet(ctx context.Context, c Client, vnet string) (*SDNVnet, error) {
	if ctx == nil {
		return nil, cpierrors.Cloud("GetSDNVnet: ctx must not be nil")
	}
	if c == nil {
		return nil, cpierrors.Cloud("GetSDNVnet: client must not be nil")
	}
	if strings.TrimSpace(vnet) == "" {
		return nil, cpierrors.Cloud("GetSDNVnet: vnet is required")
	}
	svc := c.Cluster()
	if svc == nil {
		return nil, cpierrors.Cloud("GetSDNVnet: cluster service unavailable")
	}

	pending := true
	resp, err := svc.GetSdnVnets(ctx, vnet, &sdkcluster.GetSdnVnetsParams{Pending: &pending})
	if err != nil {
		if isSDNNotFoundShape(err) {
			return nil, cpierrors.WrapAs(ErrSDNNotFound, cpierrors.TypeCloud,
				fmt.Sprintf("GetSDNVnet: vnet %q not found", vnet))
		}
		return nil, WrapError(err)
	}
	if resp == nil {
		return nil, cpierrors.WrapAs(ErrSDNNotFound, cpierrors.TypeCloud,
			fmt.Sprintf("GetSDNVnet: vnet %q not found (empty response)", vnet))
	}
	var v SDNVnet
	if err := json.Unmarshal(*resp, &v); err != nil {
		return nil, cpierrors.Wrap(err, "GetSDNVnet: decode response")
	}
	if v.Vnet == "" {
		v.Vnet = vnet
	}
	v.Raw = append(json.RawMessage(nil), *resp...)
	return &v, nil
}

// ---------------------------------------------------------------------------
// Vnet Subnet CRUD
// ---------------------------------------------------------------------------

// SDNSubnet is the decoded shape of a subnet row. Vnet is promoted so a
// flattened list across vnets stays self-describing.
type SDNSubnet struct {
	Subnet  string          `json:"subnet"`
	Vnet    string          `json:"vnet,omitempty"`
	Zone    string          `json:"zone,omitempty"`
	Gateway string          `json:"gateway,omitempty"`
	Type    string          `json:"type,omitempty"`
	Cidr    string          `json:"cidr,omitempty"`
	Raw     json.RawMessage `json:"-"`
}

// DeleteSDNVnetSubnet issues DELETE /cluster/sdn/vnets/{vnet}/subnets/{subnetCIDR}.
//
// PVE encodes the subnet id with slashes intact (e.g. "10.0.0.0/24") in the
// URL path; the SDK applies url.PathEscape so callers pass the raw CIDR.
// NotFound surfaces as ErrSDNNotFound (wrapped).
func DeleteSDNVnetSubnet(ctx context.Context, c Client, vnet, subnetCIDR string) error {
	if ctx == nil {
		return cpierrors.Cloud("DeleteSDNVnetSubnet: ctx must not be nil")
	}
	if c == nil {
		return cpierrors.Cloud("DeleteSDNVnetSubnet: client must not be nil")
	}
	if strings.TrimSpace(vnet) == "" {
		return cpierrors.Cloud("DeleteSDNVnetSubnet: vnet is required")
	}
	if strings.TrimSpace(subnetCIDR) == "" {
		return cpierrors.Cloud("DeleteSDNVnetSubnet: subnet (CIDR) is required")
	}
	svc := c.Cluster()
	if svc == nil {
		return cpierrors.Cloud("DeleteSDNVnetSubnet: cluster service unavailable")
	}

	if err := svc.DeleteSdnVnetsSubnets(ctx, vnet, subnetCIDR, nil); err != nil {
		if isSDNNotFoundShape(err) {
			return cpierrors.WrapAs(ErrSDNNotFound, cpierrors.TypeCloud,
				fmt.Sprintf("DeleteSDNVnetSubnet: subnet %q on vnet %q not found",
					subnetCIDR, vnet))
		}
		return WrapError(err)
	}
	return nil
}

// ListSDNVnetSubnets issues GET /cluster/sdn/vnets/{vnet}/subnets. Each row
// is decoded into an SDNSubnet; the Vnet field is back-filled from the
// argument since PVE rows do not always echo it.
func ListSDNVnetSubnets(ctx context.Context, c Client, vnet string) ([]SDNSubnet, error) {
	if ctx == nil {
		return nil, cpierrors.Cloud("ListSDNVnetSubnets: ctx must not be nil")
	}
	if c == nil {
		return nil, cpierrors.Cloud("ListSDNVnetSubnets: client must not be nil")
	}
	if strings.TrimSpace(vnet) == "" {
		return nil, cpierrors.Cloud("ListSDNVnetSubnets: vnet is required")
	}
	svc := c.Cluster()
	if svc == nil {
		return nil, cpierrors.Cloud("ListSDNVnetSubnets: cluster service unavailable")
	}

	pending := true
	resp, err := svc.ListSdnVnetsSubnets(ctx, vnet,
		&sdkcluster.ListSdnVnetsSubnetsParams{Pending: &pending})
	if err != nil {
		// A 404 on list typically means the vnet itself is gone; surface
		// the sentinel so callers can decide whether to treat as empty.
		if isSDNNotFoundShape(err) {
			return nil, cpierrors.WrapAs(ErrSDNNotFound, cpierrors.TypeCloud,
				fmt.Sprintf("ListSDNVnetSubnets: vnet %q not found", vnet))
		}
		return nil, WrapError(err)
	}
	if resp == nil {
		return []SDNSubnet{}, nil
	}
	out := make([]SDNSubnet, 0, len(*resp))
	for i, raw := range *resp {
		var s SDNSubnet
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, cpierrors.Wrap(err,
				fmt.Sprintf("ListSDNVnetSubnets: decode row %d", i))
		}
		if s.Vnet == "" {
			s.Vnet = vnet
		}
		s.Raw = append(json.RawMessage(nil), raw...)
		out = append(out, s)
	}
	return out, nil
}
