package pve

import (
	"context"
	"encoding/json"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// clusterResourceTypeLXC is the "type" discriminator for container rows in a
// GET /cluster/resources response.
const clusterResourceTypeLXC = "lxc"

// IndexedGuest is one guest row of the /cluster/resources index.
type IndexedGuest struct {
	GuestRef
	// Type is the row's resource type, "qemu" or "lxc", and it is empty when
	// the row leaves it out.
	Type string
}

// IsContainer reports whether the row is an LXC container. A container can't
// hold a QEMU disk, so a caller that only reads VM configs can pass over it.
func (g IndexedGuest) IsContainer() bool {
	return g.Type == clusterResourceTypeLXC
}

// ListIndexedGuests reads the /cluster/resources index once and returns every
// guest row it lists, QEMU and LXC alike, in index order. It is the evidence a
// caller of ListGuestsAuthoritativeTolerant can use for a member that the
// quorate cluster reports offline, whose own listing can't answer. pmxcfs
// keeps each guest's config in one member's directory, and the quorate side
// keeps an offline member's files, so the index still lists that member's
// guests, with their node and tags, while the node is down. The index lags a
// guest's creation, though, so a guest created on the member just before it
// went dark may be missing, and a caller has to accept that corner before it
// rests a decision on the rows.
//
// Node comes from the row and names the member whose directory holds the
// config. Status, Name, and Template come from the status data the index last
// had for the guest, which for an offline member is stale. Tags come from the
// config. A row with no VMID or no node is skipped, and so is a row whose type
// names a resource other than a guest.
//
// A client with no cluster service, and a read that fails, return an error and
// no rows, so a caller that reads the rows as proof can fail closed.
func ListIndexedGuests(ctx context.Context, c Client) ([]IndexedGuest, error) {
	if c == nil || c.Cluster() == nil {
		return nil, cpierrors.Retriable("ListIndexedGuests: cluster service unavailable; cannot read the cluster index")
	}
	typ := "vm"
	var resp *sdkcluster.ListResourcesResponse
	err := RetryOnTransient(ctx, nil, "list_indexed_guests", 0, func() error {
		var inner error
		resp, inner = c.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &typ})
		return inner
	})
	if err != nil {
		return nil, cpierrors.Wrap(WrapError(err), "ListIndexedGuests: read the cluster index")
	}
	if resp == nil {
		return nil, cpierrors.Retriable("ListIndexedGuests: the cluster index returned no rows")
	}
	out := make([]IndexedGuest, 0, len(*resp))
	for _, raw := range *resp {
		var row struct {
			VMID     sdkclient.PVEInt  `json:"vmid"`
			Node     string            `json:"node"`
			Type     string            `json:"type"`
			Name     string            `json:"name"`
			Tags     string            `json:"tags"`
			Status   string            `json:"status"`
			Template sdkclient.PVEBool `json:"template"`
		}
		if json.Unmarshal(raw, &row) != nil || row.VMID.Int() <= 0 || row.Node == "" {
			continue
		}
		if row.Type != "" && row.Type != clusterResourceTypeQemu && row.Type != clusterResourceTypeLXC {
			continue
		}
		out = append(out, IndexedGuest{
			GuestRef: GuestRef{
				VMID:     int(row.VMID.Int()),
				Node:     row.Node,
				Name:     row.Name,
				Tags:     row.Tags,
				Status:   row.Status,
				Template: row.Template.Bool(),
			},
			Type: row.Type,
		})
	}
	return out, nil
}
