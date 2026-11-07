package ha_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
)

// replicationSDKCluster records the replication calls the client sends.
type replicationSDKCluster struct {
	sdkcluster.Service
	list    *sdkcluster.ListReplicationResponse
	listErr error
	create  *sdkcluster.CreateReplicationParams
	update  *sdkcluster.UpdateReplicationParams
	deleted string
	delete  *sdkcluster.DeleteReplicationParams
}

func (c *replicationSDKCluster) ListReplication(context.Context) (*sdkcluster.ListReplicationResponse, error) {
	return c.list, c.listErr
}

func (c *replicationSDKCluster) CreateReplication(_ context.Context, p *sdkcluster.CreateReplicationParams) error {
	c.create = p
	return nil
}

func (c *replicationSDKCluster) UpdateReplication(_ context.Context, _ string,
	p *sdkcluster.UpdateReplicationParams,
) error {
	c.update = p
	return nil
}

func (c *replicationSDKCluster) DeleteReplication(_ context.Context, id string,
	p *sdkcluster.DeleteReplicationParams,
) error {
	c.deleted, c.delete = id, p
	return nil
}

// replicationSDKNodes answers the node-scoped replication calls.
type replicationSDKNodes struct {
	sdknodes.Service
	scheduled string
	status    json.RawMessage
	content   *sdknodes.ListStorageContentResponse
}

func (n *replicationSDKNodes) CreateReplicationScheduleNow(_ context.Context, node, id string) (
	*sdknodes.CreateReplicationScheduleNowResponse, error,
) {
	n.scheduled = node + "/" + id
	out := sdknodes.CreateReplicationScheduleNowResponse(`null`)
	return &out, nil
}

func (n *replicationSDKNodes) ListReplicationStatus(context.Context, string, string) (
	*sdknodes.ListReplicationStatusResponse, error,
) {
	out := n.status
	return &out, nil
}

func (n *replicationSDKNodes) ListStorageContent(context.Context, string, string,
	*sdknodes.ListStorageContentParams,
) (*sdknodes.ListStorageContentResponse, error) {
	return n.content, nil
}

func TestReplicationClientSendsTheJobCalls(t *testing.T) {
	ctx := context.Background()
	rows := sdkcluster.ListReplicationResponse{
		json.RawMessage(`{"id":"100-0","guest":100,"jobnum":0,"target":"pve02","source":"pve01","comment":"m"}`),
	}
	c := &replicationSDKCluster{list: &rows}
	n := &replicationSDKNodes{status: json.RawMessage(`{"last_sync":1700000000,"fail_count":0}`)}
	api := ha.NewReplicationAPI(&depsClient{cluster: c, nodes: n})

	jobs, err := api.ListJobs(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].ID != "100-0" || jobs[0].Target != "pve02" {
		t.Fatalf("jobs %+v err %v", jobs, err)
	}
	spec := ha.ReplicaJobSpec{ID: "100-0", Target: "pve02", Schedule: " */15 ", RateMbps: 40, Comment: "marker"}
	if err := api.CreateJob(ctx, spec); err != nil {
		t.Fatal(err)
	}
	p := c.create
	if p.Id != "100-0" || p.Target != "pve02" || p.Type != ha.ReplicaJobType || p.Schedule == nil ||
		*p.Schedule != "*/15" || p.Rate == nil || *p.Rate != 40 || p.Comment == nil || *p.Comment != "marker" {
		t.Fatalf("create params %+v", p)
	}
	if err := api.CreateJob(ctx, ha.ReplicaJobSpec{ID: "100-1", Target: "pve03"}); err != nil {
		t.Fatal(err)
	}
	if c.create.Schedule != nil || c.create.Rate != nil || c.create.Comment != nil {
		t.Fatalf("unset fields are omitted: %+v", c.create)
	}
	if err := api.SetComment(ctx, "100-0", "new"); err != nil || c.update.Comment == nil || *c.update.Comment != "new" {
		t.Fatalf("update %+v err %v", c.update, err)
	}
	if err := api.DeleteJob(ctx, "100-0", false); err != nil || c.deleted != "100-0" || c.delete != nil {
		t.Fatalf("plain delete params %+v err %v; it sends no force", c.delete, err)
	}
	if err := api.DeleteJob(ctx, "100-0", true); err != nil || c.delete == nil || c.delete.Force == nil ||
		!*c.delete.Force {
		t.Fatalf("forced delete params %+v err %v", c.delete, err)
	}
	if err := api.ScheduleNow(ctx, "pve01", "100-0"); err != nil || n.scheduled != "pve01/100-0" {
		t.Fatalf("schedule %q err %v", n.scheduled, err)
	}
	st, err := api.Status(ctx, "pve01", "100-0")
	if err != nil || st.LastSync.Unix() != 1700000000 {
		t.Fatalf("status %+v err %v", st, err)
	}
}

func TestReplicationClientReadsTheTargetListing(t *testing.T) {
	ctx := context.Background()
	content := sdknodes.ListStorageContentResponse{json.RawMessage(`{"volid":"local-zfs:vm-100-disk-0"}`)}
	n := &replicationSDKNodes{content: &content}
	api := ha.NewReplicationAPI(&depsClient{cluster: &replicationSDKCluster{}, nodes: n})
	got, err := api.Volumes(ctx, "pve02", "local-zfs")
	if err != nil || !got["local-zfs:vm-100-disk-0"] || len(got) != 1 {
		t.Fatalf("listing %v err %v", got, err)
	}

	bad := sdknodes.ListStorageContentResponse{json.RawMessage(`{"size":1}`)}
	n.content = &bad
	if _, err := api.Volumes(ctx, "pve02", "local-zfs"); err == nil {
		t.Fatal("a row with no volid must be an error")
	}
	n.content = nil
	if _, err := api.Volumes(ctx, "pve02", "local-zfs"); err == nil {
		t.Fatal("an empty answer must be an error")
	}
}

func TestReplicationClientListErrors(t *testing.T) {
	ctx := context.Background()
	api := ha.NewReplicationAPI(&depsClient{cluster: &replicationSDKCluster{listErr: errors.New("denied")}})
	if _, err := api.ListJobs(ctx); err == nil {
		t.Fatal("a list error must be returned")
	}
	api = ha.NewReplicationAPI(&depsClient{cluster: &replicationSDKCluster{}})
	if _, err := api.ListJobs(ctx); err == nil {
		t.Fatal("an empty answer must be an error")
	}
}
