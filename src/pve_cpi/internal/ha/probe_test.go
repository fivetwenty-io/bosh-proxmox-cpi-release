package ha_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestConfigNodeRowsThatCannotBeUsed(t *testing.T) {
	for name, body := range map[string]string{
		"negative votes":         `[{"node":"pve1","quorum_votes":-1},{"node":"pve2"},{"node":"pve3"}]`,
		"row without a name":     `[{"quorum_votes":1},{"node":"pve2"},{"node":"pve3"}]`,
		"row of the wrong shape": `["pve1"]`,
	} {
		t.Run(name, func(t *testing.T) {
			w := three()
			w.configNodesBody = json.RawMessage(body)
			p := resolve(t, w, cfgMode(config.HAModeAuto))
			if p.Quorum.Known || p.Effective != ha.EffectiveObserveOnly {
				t.Fatalf("quorum=%+v effective=%s", p.Quorum, p.Effective)
			}
		})
	}
}

func TestConfigNodeRowsNamedByNameField(t *testing.T) {
	w := three()
	w.configNodesBody = json.RawMessage(`[{"name":"pve1"},{"name":"pve2","quorum_votes":"1"},{"name":"pve3","quorum_votes":1}]`)
	p := resolve(t, w, cfgMode(config.HAModeAuto))
	if !p.Quorum.Known || p.Quorum.Expected != 3 {
		t.Fatalf("quorum = %+v", p.Quorum)
	}
}

// depsClient is a pve.Client whose services are the fakes. Only the three
// services the profile reads are set.
type depsClient struct {
	pve.Client
	cluster sdkcluster.Service
	nodes   sdknodes.Service
	storage sdkclusterstorage.Service
}

func (c *depsClient) Cluster() sdkcluster.Service               { return c.cluster }
func (c *depsClient) Nodes() sdknodes.Service                   { return c.nodes }
func (c *depsClient) ClusterStorage() sdkclusterstorage.Service { return c.storage }

type permClient struct {
	*depsClient
	fakePerms
}

func TestDepsFromClient(t *testing.T) {
	if d := ha.DepsFromClient(nil, cfgMode("auto"), nil); d.Cluster != nil || d.Config == nil {
		t.Fatalf("a nil client must leave the sources unset: %+v", d)
	}

	plain := ha.DepsFromClient(&depsClient{}, cfgMode("auto"), nil)
	if plain.Permissions != nil {
		t.Fatal("a client that cannot read permissions leaves Permissions nil")
	}
	if got := strings.Join(plain.MaintenanceTags, ","); got != "maintenance" {
		t.Fatalf("an unset maintenance_node_tags must pass the default tag, got %q", got)
	}
	off := cfgMode("auto")
	off.Placement = &config.PlacementConfig{MaintenanceNodeTags: []string{}}
	if d := ha.DepsFromClient(&depsClient{}, off, nil); len(d.MaintenanceTags) != 0 {
		t.Fatalf("an explicit empty maintenance_node_tags must pass no tags, got %q", d.MaintenanceTags)
	}
	drain := cfgMode("auto")
	drain.Placement = &config.PlacementConfig{MaintenanceNodeTags: []string{"drain", "maint"}}
	if d := ha.DepsFromClient(nil, drain, nil); strings.Join(d.MaintenanceTags, ",") != "drain,maint" {
		t.Fatalf("the configured tags must pass even without a client, got %q", d.MaintenanceTags)
	}

	pc := &permClient{depsClient: &depsClient{}, fakePerms: fakePerms{w: &world{}}}
	withPerms := ha.DepsFromClient(pc, cfgMode("auto"), nil)
	if withPerms.Permissions == nil {
		t.Fatal("a client that reads permissions must be wired")
	}
}

func TestPermissionlessDepsDowngradeWithAnExplicitReason(t *testing.T) {
	w := three()
	w.noPerms = true
	p := resolve(t, w, cfgMode(config.HAModeManaged))
	err := p.PrerequisiteError()
	if err == nil || !strings.Contains(err.Error(), "Sys.Console") {
		t.Fatalf("err = %v", err)
	}
}

func TestCanceledContextFailsClosed(t *testing.T) {
	w := three()
	d := w.deps(cfgMode(config.HAModeAuto))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.Cluster = &canceledCluster{fakeCluster: w.cluster}
	p, err := ha.Resolve(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if p.Effective != ha.EffectiveObserveOnly {
		t.Fatalf("effective = %s", p.Effective)
	}
}

type canceledCluster struct{ *fakeCluster }

func (c *canceledCluster) ListStatus(ctx context.Context) (*sdkcluster.ListStatusResponse, error) {
	return nil, errors.Join(ctx.Err(), errors.New("request aborted"))
}
