package handlers

import (
	"encoding/json"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// The answers below are what a healthy cluster gives to the reads an absence
// proof makes: GET /cluster/status with every member online, the corosync
// member list, no HA resources, and an idle CRM. Cluster fakes in both test
// packages serve them, so a test that walks a not-found or config-missing
// branch exercises the proof without scripting each read.

// AbsenceOnlineStatus is a quorate /cluster/status answer that lists every
// member online.
func AbsenceOnlineStatus(members ...string) *cluster.ListStatusResponse {
	resp := make(cluster.ListStatusResponse, 0, 1+len(members))
	resp = append(resp, json.RawMessage(`{"type":"cluster","name":"fixture","quorate":1}`))
	for _, m := range members {
		row, _ := json.Marshal(map[string]any{"type": "node", "name": m, "online": 1})
		resp = append(resp, row)
	}
	return &resp
}

// AbsenceConfigNodes is a /cluster/config/nodes answer naming members.
func AbsenceConfigNodes(members ...string) *cluster.ListConfigNodesResponse {
	resp := make(cluster.ListConfigNodesResponse, 0, len(members))
	for _, m := range members {
		row, _ := json.Marshal(map[string]any{"name": m, "node": m})
		resp = append(resp, row)
	}
	return &resp
}

// AbsenceHAResources is a /cluster/ha/resources answer with one row per SID.
func AbsenceHAResources(sids ...string) *cluster.ListHaResourcesResponse {
	resp := make(cluster.ListHaResourcesResponse, 0, len(sids))
	for _, sid := range sids {
		row, _ := json.Marshal(map[string]any{"sid": sid, "state": "started"})
		resp = append(resp, row)
	}
	return &resp
}

// AbsenceIdleCRM is the manager status of a CRM that holds no services.
func AbsenceIdleCRM() *cluster.ListHaStatusManagerStatusResponse {
	resp := cluster.ListHaStatusManagerStatusResponse(`{}`)
	return &resp
}

// AbsenceNoHAStatus is a /cluster/ha/status/current answer with no rows.
func AbsenceNoHAStatus() *cluster.ListHaStatusCurrentResponse {
	resp := cluster.ListHaStatusCurrentResponse{}
	return &resp
}
