package handlers_test

import (
	"context"
	"encoding/json"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"

	sdkclusterapi "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// The disk handlers check that a VM's node can answer before they act on the
// VM, which reads the cluster's status and its HA view. The cluster fakes below
// predate that read and embed a nil cluster service, so they answer it here as
// a cluster with no HA services: an idle CRM, no HA status rows, and no HA
// resources.

func (c *ldmCluster) ListHaStatusManagerStatus(context.Context) (*sdkclusterapi.ListHaStatusManagerStatusResponse, error) {
	return handlers.AbsenceIdleCRM(), nil
}

func (c *ldmCluster) ListHaStatusCurrent(context.Context) (*sdkclusterapi.ListHaStatusCurrentResponse, error) {
	return handlers.AbsenceNoHAStatus(), nil
}

func (c *ldmCluster) ListHaResources(context.Context, *sdkclusterapi.ListHaResourcesParams) (*sdkclusterapi.ListHaResourcesResponse, error) {
	return handlers.AbsenceHAResources(), nil
}

func (m *diskMetaClusterSvc) ListHaStatusManagerStatus(context.Context) (*sdkclusterapi.ListHaStatusManagerStatusResponse, error) {
	return handlers.AbsenceIdleCRM(), nil
}

func (m *diskMetaClusterSvc) ListHaStatusCurrent(context.Context) (*sdkclusterapi.ListHaStatusCurrentResponse, error) {
	return handlers.AbsenceNoHAStatus(), nil
}

func (m *diskMetaClusterSvc) ListHaResources(context.Context, *sdkclusterapi.ListHaResourcesParams) (*sdkclusterapi.ListHaResourcesResponse, error) {
	return handlers.AbsenceHAResources(), nil
}

// onlineStatusFromResources is a /cluster/status answer that lists testNode and
// every node the index rows name as an online member.
func onlineStatusFromResources(rows *sdkclusterapi.ListResourcesResponse) *sdkclusterapi.ListStatusResponse {
	members := []string{testNode}
	nodes := authConfigNodesFromResources(rows, testNode)
	for _, raw := range *nodes {
		var row struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &row) == nil && row.Name != "" && row.Name != testNode {
			members = append(members, row.Name)
		}
	}
	return handlers.AbsenceOnlineStatus(members...)
}

// ListStatus lists testNode and every node the index rows name as online.
func (c *attachDiskClusterSvc) ListStatus(ctx context.Context) (*sdkclusterapi.ListStatusResponse, error) {
	rows, err := c.ListResources(ctx, nil)
	if err != nil {
		return nil, err
	}
	return onlineStatusFromResources(rows), nil
}

func (c *attachDiskClusterSvc) ListHaStatusManagerStatus(context.Context) (*sdkclusterapi.ListHaStatusManagerStatusResponse, error) {
	return handlers.AbsenceIdleCRM(), nil
}

func (c *attachDiskClusterSvc) ListHaStatusCurrent(context.Context) (*sdkclusterapi.ListHaStatusCurrentResponse, error) {
	return handlers.AbsenceNoHAStatus(), nil
}

func (c *attachDiskClusterSvc) ListHaResources(context.Context, *sdkclusterapi.ListHaResourcesParams) (*sdkclusterapi.ListHaResourcesResponse, error) {
	return handlers.AbsenceHAResources(), nil
}
