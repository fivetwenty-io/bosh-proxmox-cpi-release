package pve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

type storageContentRawReader interface {
	GetRawCtx(context.Context, string, map[string]interface{}) (*sdkclient.Response, error)
}

// strictStorageContentNodes preserves unknown listing responses as errors.
// The generated SDK maps data:null to an empty array, which cannot prove that
// a volume or guest is absent. Unrelated node methods retain their behavior.
type strictStorageContentNodes struct {
	nodes.Service
	reader storageContentRawReader
}

func (s *strictStorageContentNodes) ListStorageContent(ctx context.Context, node, storage string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	if ctx == nil {
		return nil, fmt.Errorf("storage content observation requires context")
	}
	var query map[string]interface{}
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("storage content parameters malformed")
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		if err := decoder.Decode(&query); err != nil {
			return nil, fmt.Errorf("storage content parameters malformed")
		}
	}
	path := fmt.Sprintf("/nodes/%s/storage/%s/content", url.PathEscape(node), url.PathEscape(storage))
	response, err := s.reader.GetRawCtx(ctx, path, query)
	if err != nil {
		return nil, storageContentFailure(err)
	}
	if response == nil || response.Data == nil {
		return nil, &storageContentObservationError{reason: "listing_data_missing"}
	}
	encoded, err := json.Marshal(response.Data)
	if err != nil {
		return nil, &storageContentObservationError{reason: "listing_data_invalid"}
	}
	var listing nodes.ListStorageContentResponse
	if err := json.Unmarshal(encoded, &listing); err != nil || listing == nil {
		return nil, &storageContentObservationError{reason: "listing_shape_invalid"}
	}
	return &listing, nil
}

// strictHAResourcesCluster preserves unknown HA listings as errors. PVE requires
// Sys.Audit at / for this endpoint and does not filter rows by guest permissions.
type strictHAResourcesCluster struct {
	cluster.Service
	reader storageContentRawReader
}

func (s *strictHAResourcesCluster) ListHaResources(ctx context.Context, params *cluster.ListHaResourcesParams) (*cluster.ListHaResourcesResponse, error) {
	if ctx == nil {
		return nil, fmt.Errorf("HA resources observation requires context")
	}
	query := map[string]interface{}{}
	if params != nil && params.Type != nil {
		query["type"] = *params.Type
	}
	response, err := s.reader.GetRawCtx(ctx, "/cluster/ha/resources", query)
	if err != nil || response == nil || response.Data == nil {
		return nil, fmt.Errorf("HA resources observation unavailable")
	}
	encoded, err := json.Marshal(response.Data)
	if err != nil {
		return nil, fmt.Errorf("HA resources observation malformed")
	}
	var listing cluster.ListHaResourcesResponse
	if err := json.Unmarshal(encoded, &listing); err != nil || listing == nil {
		return nil, fmt.Errorf("HA resources observation is not an array")
	}
	return &listing, nil
}

func (s *strictHAResourcesCluster) ListHaRules(ctx context.Context, params *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	if ctx == nil {
		return nil, fmt.Errorf("HA rules observation requires context")
	}
	query := map[string]interface{}{}
	if params != nil && params.Type != nil {
		query["type"] = *params.Type
	}
	if params != nil && params.Resource != nil {
		query["resource"] = *params.Resource
	}
	response, err := s.reader.GetRawCtx(ctx, "/cluster/ha/rules", query)
	if err != nil || response == nil || response.Data == nil {
		return nil, fmt.Errorf("HA rules observation unavailable")
	}
	encoded, err := json.Marshal(response.Data)
	if err != nil {
		return nil, fmt.Errorf("HA rules observation malformed")
	}
	var listing cluster.ListHaRulesResponse
	if err := json.Unmarshal(encoded, &listing); err != nil || listing == nil {
		return nil, fmt.Errorf("HA rules observation is not an array")
	}
	return &listing, nil
}

// ListQemu keeps unavailable wire data distinct from a successful empty list.
func (s *strictStorageContentNodes) ListQemu(ctx context.Context, node string, params *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	query := map[string]interface{}{}
	if params != nil && params.Full != nil {
		query["full"] = *params.Full
	}
	rows, err := s.guestList(ctx, node, "qemu", query)
	if err != nil {
		return nil, err
	}
	result := nodes.ListQemuResponse(rows)
	return &result, nil
}

// ListLxc preserves the same strict array boundary for container inventories.
func (s *strictStorageContentNodes) ListLxc(ctx context.Context, node string) (*nodes.ListLxcResponse, error) {
	rows, err := s.guestList(ctx, node, "lxc", nil)
	if err != nil {
		return nil, err
	}
	result := nodes.ListLxcResponse(rows)
	return &result, nil
}

func (s *strictStorageContentNodes) guestList(ctx context.Context, node, kind string, query map[string]interface{}) ([]json.RawMessage, error) {
	if ctx == nil {
		return nil, fmt.Errorf("guest listing requires context")
	}
	path := fmt.Sprintf("/nodes/%s/%s", url.PathEscape(node), kind)
	response, err := s.reader.GetRawCtx(ctx, path, query)
	if err != nil {
		return nil, fmt.Errorf("guest listing unavailable: %w", err)
	}
	if response == nil || response.Data == nil {
		return nil, fmt.Errorf("guest listing data unavailable")
	}
	encoded, err := json.Marshal(response.Data)
	if err != nil {
		return nil, fmt.Errorf("guest listing data malformed")
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(encoded, &rows); err != nil || rows == nil {
		return nil, fmt.Errorf("guest listing data is not an array")
	}
	return rows, nil
}
