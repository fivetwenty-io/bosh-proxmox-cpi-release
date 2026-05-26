// Package pve — SDN backend adapter.
//
// Typed CRUD primitives over PVE SDN zones, vnets, and vnet subnets, plus an
// ApplySDN entry point that commits pending SDN configuration changes
// (PUT /cluster/sdn). Each primitive accepts a typed parameter struct and
// returns errors classified by WrapError; 404 responses surface as
// ErrSDNNotFound so callers can distinguish missing entities from generic
// failures.
//
// PVE quirks captured here:
//   - SDN zone create has no description/notes/comment field; callers cannot
//     tag CPI-owned zones in-band. Identification of CPI-owned objects must
//     be done by name convention at the caller.
//   - "Apply" is PUT /cluster/sdn (cluster.Service.UpdateSdn). The similarly
//     named CreateSdnRollback endpoint reverts pending changes — easy to
//     confuse but the opposite operation.
//   - Zone, vnet, and subnet create/delete return synchronously (no UPID
//     task); only UpdateSdn potentially returns a task identifier. The
//     primitives below do not poll task completion — that is the caller's
//     responsibility based on the ApplySDN response.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
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

// SDNZoneParams is the typed input for CreateSDNZone.
//
// Type is required and must be one of "simple", "vlan", "qinq", "vxlan",
// "evpn" — the PVE-supported zone plugin types. Bridge is required for
// "vlan" zones (the underlying Linux bridge for VLAN tagging) and optional
// elsewhere. Other type-specific fields (peers, controller, exitnodes, etc.)
// are intentionally omitted from this minimal struct; extend the type when a
// caller needs them rather than passing free-form maps.
//
// No description/notes/comment field — PVE rejects those on zone create.
type SDNZoneParams struct {
	Zone   string
	Type   string
	Bridge string
}

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

// CreateSDNZone issues POST /cluster/sdn/zones with the typed parameters.
//
// Returns nil on success. Validation errors (empty Zone or Type) surface as
// non-retriable CloudError. SDK errors flow through WrapError; 404 is not
// expected on create but is mapped through if PVE returns it.
func CreateSDNZone(ctx context.Context, c Client, p SDNZoneParams) error {
	if ctx == nil {
		return cpierrors.Cloud("CreateSDNZone: ctx must not be nil")
	}
	if c == nil {
		return cpierrors.Cloud("CreateSDNZone: client must not be nil")
	}
	if strings.TrimSpace(p.Zone) == "" {
		return cpierrors.Cloud("CreateSDNZone: zone is required")
	}
	if strings.TrimSpace(p.Type) == "" {
		return cpierrors.Cloud("CreateSDNZone: type is required")
	}
	svc := c.Cluster()
	if svc == nil {
		return cpierrors.Cloud("CreateSDNZone: cluster service unavailable")
	}

	params := &sdkcluster.CreateSdnZonesParams{
		Zone: p.Zone,
		Type: p.Type,
	}
	if p.Bridge != "" {
		b := p.Bridge
		params.Bridge = &b
	}

	if err := svc.CreateSdnZones(ctx, params); err != nil {
		return WrapError(err)
	}
	return nil
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

// SDNVnetParams is the typed input for CreateSDNVnet. Vnet and Zone are
// required. Alias, Tag, and Vlanaware are optional and forwarded only when
// non-zero — zero values are treated as "not set" so callers don't have to
// reach for pointer types in handler code.
type SDNVnetParams struct {
	Vnet      string
	Zone      string
	Alias     string
	Tag       int64
	Vlanaware bool
}

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

// CreateSDNVnet issues POST /cluster/sdn/vnets.
func CreateSDNVnet(ctx context.Context, c Client, p SDNVnetParams) error {
	if ctx == nil {
		return cpierrors.Cloud("CreateSDNVnet: ctx must not be nil")
	}
	if c == nil {
		return cpierrors.Cloud("CreateSDNVnet: client must not be nil")
	}
	if strings.TrimSpace(p.Vnet) == "" {
		return cpierrors.Cloud("CreateSDNVnet: vnet is required")
	}
	if strings.TrimSpace(p.Zone) == "" {
		return cpierrors.Cloud("CreateSDNVnet: zone is required")
	}
	svc := c.Cluster()
	if svc == nil {
		return cpierrors.Cloud("CreateSDNVnet: cluster service unavailable")
	}

	params := &sdkcluster.CreateSdnVnetsParams{
		Vnet: p.Vnet,
		Zone: p.Zone,
	}
	if p.Alias != "" {
		a := p.Alias
		params.Alias = &a
	}
	if p.Tag != 0 {
		t := p.Tag
		params.Tag = &t
	}
	if p.Vlanaware {
		v := true
		params.Vlanaware = &v
	}

	if err := svc.CreateSdnVnets(ctx, params); err != nil {
		return WrapError(err)
	}
	return nil
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

// SDNVnetSubnetParams is the typed input for CreateSDNVnetSubnet. Vnet and
// Subnet (CIDR) are required. Gateway is optional but recommended for
// layer-3 zones. Snat, DHCP, and DNS-register flags are forwarded only when
// non-zero — same zero-value-means-unset rule as SDNVnetParams.
type SDNVnetSubnetParams struct {
	Vnet          string
	Subnet        string
	Gateway       string
	Snat          bool
	DhcpDNS       string
	DhcpRange     []string
	DnsZonePrefix string
}

// SDNSubnet is the decoded shape of a subnet row. Vnet is promoted so a
// flattened list across vnets stays self-describing.
type SDNSubnet struct {
	Subnet  string          `json:"subnet"`
	Vnet    string          `json:"vnet,omitempty"`
	Zone    string          `json:"zone,omitempty"`
	Gateway string          `json:"gateway,omitempty"`
	Type    string          `json:"type,omitempty"`
	Raw     json.RawMessage `json:"-"`
}

// CreateSDNVnetSubnet issues POST /cluster/sdn/vnets/{vnet}/subnets.
//
// The SDK requires a "type" field whose only PVE-accepted value is
// "subnet" — hardcoded here so callers don't have to remember.
func CreateSDNVnetSubnet(ctx context.Context, c Client, p SDNVnetSubnetParams) error {
	if ctx == nil {
		return cpierrors.Cloud("CreateSDNVnetSubnet: ctx must not be nil")
	}
	if c == nil {
		return cpierrors.Cloud("CreateSDNVnetSubnet: client must not be nil")
	}
	if strings.TrimSpace(p.Vnet) == "" {
		return cpierrors.Cloud("CreateSDNVnetSubnet: vnet is required")
	}
	if strings.TrimSpace(p.Subnet) == "" {
		return cpierrors.Cloud("CreateSDNVnetSubnet: subnet (CIDR) is required")
	}
	svc := c.Cluster()
	if svc == nil {
		return cpierrors.Cloud("CreateSDNVnetSubnet: cluster service unavailable")
	}

	params := &sdkcluster.CreateSdnVnetsSubnetsParams{
		Subnet: p.Subnet,
		Type:   "subnet",
	}
	if p.Gateway != "" {
		g := p.Gateway
		params.Gateway = &g
	}
	if p.Snat {
		s := true
		params.Snat = &s
	}
	if p.DhcpDNS != "" {
		d := p.DhcpDNS
		params.DhcpDnsServer = &d
	}
	if len(p.DhcpRange) > 0 {
		params.DhcpRange = append([]string(nil), p.DhcpRange...)
	}
	if p.DnsZonePrefix != "" {
		d := p.DnsZonePrefix
		params.Dnszoneprefix = &d
	}

	if err := svc.CreateSdnVnetsSubnets(ctx, p.Vnet, params); err != nil {
		return WrapError(err)
	}
	return nil
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

// ---------------------------------------------------------------------------
// Apply
// ---------------------------------------------------------------------------

// ApplySDN commits pending SDN configuration via PUT /cluster/sdn
// (cluster.Service.UpdateSdn). Passes nil params (no lock-token, no
// release-lock) so the call is suitable for unlocked apply flows.
//
// CAUTION: this is "apply", not "rollback". The similarly named SDK method
// CreateSdnRollback reverts pending changes and must NOT be used here.
//
// PVE may return a UPID identifying an asynchronous task. ApplySDN does not
// poll the task — callers needing strong "applied" semantics should poll
// the returned identifier via tasks.Service.
func ApplySDN(ctx context.Context, c Client) error {
	if ctx == nil {
		return cpierrors.Cloud("ApplySDN: ctx must not be nil")
	}
	if c == nil {
		return cpierrors.Cloud("ApplySDN: client must not be nil")
	}
	svc := c.Cluster()
	if svc == nil {
		return cpierrors.Cloud("ApplySDN: cluster service unavailable")
	}

	if _, err := svc.UpdateSdn(ctx, nil); err != nil {
		return WrapError(err)
	}
	return nil
}
