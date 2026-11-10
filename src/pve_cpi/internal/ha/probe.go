package ha

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// ClusterAPI is the subset of the PVE cluster API the profile reads. It is
// satisfied by the sdk cluster service, and it embeds what NodeHealth reads.
type ClusterAPI interface {
	pve.NodeHealthCluster
	ListConfigNodes(ctx context.Context) (*sdkcluster.ListConfigNodesResponse, error)
	ListHaResources(ctx context.Context, params *sdkcluster.ListHaResourcesParams) (*sdkcluster.ListHaResourcesResponse, error)
	ListConfigQdevice(ctx context.Context) (*sdkcluster.ListConfigQdeviceResponse, error)
	ListHaRules(ctx context.Context, params *sdkcluster.ListHaRulesParams) (*sdkcluster.ListHaRulesResponse, error)
	ListOptions(ctx context.Context) (*sdkcluster.ListOptionsResponse, error)
	ListReplication(ctx context.Context) (*sdkcluster.ListReplicationResponse, error)
}

// NodesAPI is the subset of the PVE nodes API the profile reads. It is
// satisfied by the sdk nodes service, and it embeds what NodeHealth reads.
type NodesAPI interface {
	pve.NodeHealthNodes
	ListVersion(ctx context.Context, node string) (*sdknodes.ListVersionResponse, error)
	ListStatus(ctx context.Context, node string) (*sdknodes.ListStatusResponse, error)
}

// StorageAPI is the subset of the PVE storage API the profile reads. It is
// satisfied by the sdk cluster storage service.
type StorageAPI interface {
	ListStorage(ctx context.Context, params *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error)
}

// Deps holds everything Resolve needs.
type Deps struct {
	// Config supplies pve.ha.mode and pve.ha.storage_topology.
	Config *config.CPIConfig

	// Cluster, Nodes, Storage, and Permissions are the probe sources.
	Cluster     ClusterAPI
	Nodes       NodesAPI
	Storage     StorageAPI
	Permissions pve.TokenPermissionReader

	// Log receives the profile line. When it is nil, Resolve uses the logger in
	// the context.
	Log *log.Logger

	// MaintenanceTags are the operator's node tags that mark maintenance, passed
	// to the node health helper.
	MaintenanceTags []string

	// Slow supplies the slow-changing facts. When it is nil every call probes
	// them live. A cache wraps the live source and is set here, so callers of
	// Resolve do not change.
	Slow SlowSource

	// ProbeWhenOff makes Resolve probe the cluster even in mode off, for the
	// doctor command. Without it, mode off returns at once and probes nothing.
	ProbeWhenOff bool
}

// DepsFromClient builds Deps from a PVE client. A client that cannot read the
// token's permissions leaves Permissions nil, which the profile treats as
// unreadable privileges. The slow-changing facts come from the profile cache,
// and a caller that needs every fact read live sets Slow to nil.
func DepsFromClient(c pve.Client, cfg *config.CPIConfig, logger *log.Logger) Deps {
	d := Deps{Config: cfg, Log: logger}
	if c == nil {
		return d
	}
	d.Cluster = c.Cluster()
	d.Nodes = c.Nodes()
	d.Storage = c.ClusterStorage()
	if r, ok := c.(pve.TokenPermissionReader); ok {
		d.Permissions = r
	}
	// The slow facts come from the profile cache when the entry can key one.
	// An entry that cannot, such as one with no credential, probes every call.
	if opts, ok := ProfileCacheOptionsFromConfig(cfg, logger); ok {
		d = d.WithProfileCache(opts)
	}
	return d
}

// NodeRef names a cluster node and whether cluster status reports it online.
type NodeRef struct {
	Name   string
	Online bool
}

// NodeSlowFacts holds the slow-changing facts of one node.
type NodeSlowFacts struct {
	// Probed is false when the node was offline and so was not asked.
	Probed bool

	// Version is the pve-manager version, such as 9.2.4, or empty when it could
	// not be read. VersionErr says why.
	Version    string
	VersionErr string

	// CPU is the node's CPU model and flags, and CPUErr says why they are
	// unknown when the status could not be read.
	CPU    CPUInfo
	CPUErr string
}

// SlowFacts are the cluster facts that change slowly: versions, CPU models,
// the configured votes, the QDevice's configuration, the token's privileges,
// and the storage definitions. A cache stores exactly this struct. It never
// holds node health, watchdog answers, or the QDevice's connection status.
type SlowFacts struct {
	// Nodes maps a node name to its version and CPU.
	Nodes map[string]NodeSlowFacts

	// RulesSupported is true when GET /cluster/ha/rules answered.
	RulesSupported bool
	RulesErr       string

	// ConfigVotes maps a node name to its configured quorum_votes. It is nil
	// when ConfigVotesErr is set.
	ConfigVotes    map[string]int
	ConfigVotesErr string

	// QDevice is the QDevice's configuration. Its Status is the status at the
	// time of the probe, and Resolve refreshes it when the facts are cached.
	QDevice QDevice

	// SysConsole is true when the token holds Sys.Console on /. VMReplicate is
	// true when it holds VM.Replicate on /vms with propagation. PermissionsErr
	// says why either could not be read.
	SysConsole     bool
	VMReplicate    bool
	PermissionsErr string

	// Storages holds the storage definitions, and StoragesErr says why they
	// could not be read.
	Storages    []pve.StorageInfo
	StoragesErr string

	// Cached is set by a cache when it returns stored facts, so that Resolve
	// knows the QDevice status is stale. It is never stored.
	Cached bool `json:"-"`
}

// SlowSource supplies the slow-changing facts for the given cluster members.
type SlowSource interface {
	Slow(ctx context.Context, nodes []NodeRef) (*SlowFacts, error)
}

// liveSlow probes the slow-changing facts on every call.
type liveSlow struct {
	deps Deps
}

// Slow implements SlowSource. A failed probe is recorded in the returned facts
// and never returned as an error, so one unreadable source does not hide the
// rest.
func (l liveSlow) Slow(ctx context.Context, nodes []NodeRef) (*SlowFacts, error) {
	d := l.deps
	f := &SlowFacts{Nodes: make(map[string]NodeSlowFacts, len(nodes))}
	for _, n := range nodes {
		f.Nodes[n.Name] = probeNode(ctx, d.Nodes, n)
	}
	if _, err := listRules(ctx, d.Cluster); err != nil {
		f.RulesErr = err.Error()
	} else {
		f.RulesSupported = true
	}
	f.ConfigVotes, f.ConfigVotesErr = readConfigVotes(ctx, d.Cluster)
	f.QDevice = readQDevice(ctx, d.Cluster)
	f.SysConsole, f.VMReplicate, f.PermissionsErr = readPrivileges(ctx, d.Permissions)
	f.Storages, f.StoragesErr = readStorages(ctx, d.Storage)
	return f, nil
}

// probeNode reads the version and the CPU of one node. An offline node is not
// asked, because its answer would only be a timeout.
func probeNode(ctx context.Context, api NodesAPI, n NodeRef) NodeSlowFacts {
	if !n.Online {
		return NodeSlowFacts{}
	}
	out := NodeSlowFacts{Probed: true}

	var ver *sdknodes.ListVersionResponse
	err := pve.RetryOnTransient(ctx, nil, "ha_profile_node_version", 0, func() error {
		var inner error
		ver, inner = api.ListVersion(ctx, n.Name)
		return inner
	})
	switch {
	case err != nil:
		out.VersionErr = err.Error()
	case ver == nil:
		out.VersionErr = "nil response"
	default:
		v := strings.TrimSpace(ver.Version)
		if v == "" {
			v = strings.TrimSpace(ver.Release)
		}
		if _, ok := parseVersion(v); !ok {
			out.VersionErr = fmt.Sprintf("unparseable version %q", v)
		} else {
			out.Version = v
		}
	}

	var st *sdknodes.ListStatusResponse
	err = pve.RetryOnTransient(ctx, nil, "ha_profile_node_status", 0, func() error {
		var inner error
		st, inner = api.ListStatus(ctx, n.Name)
		return inner
	})
	switch {
	case err != nil:
		out.CPUErr = err.Error()
	case st == nil:
		out.CPUErr = "nil response"
	default:
		cpu, perr := parseCPUInfo(st.Cpuinfo)
		if perr != nil {
			out.CPUErr = perr.Error()
		} else {
			out.CPU = cpu
			if !cpu.Known {
				out.CPUErr = "node status carries no CPU model"
			}
		}
	}
	return out
}

func listRules(ctx context.Context, api ClusterAPI) (*sdkcluster.ListHaRulesResponse, error) {
	var resp *sdkcluster.ListHaRulesResponse
	err := pve.RetryOnTransient(ctx, nil, "ha_profile_rules", 0, func() error {
		var inner error
		resp, inner = api.ListHaRules(ctx, nil)
		return inner
	})
	return resp, err
}

// readConfigVotes reads each node's configured quorum_votes. PVE counts a node
// that has no quorum_votes as 1. An empty list, a row without a name, or a
// negative value makes the whole reading unusable.
func readConfigVotes(ctx context.Context, api ClusterAPI) (map[string]int, string) {
	var resp *sdkcluster.ListConfigNodesResponse
	err := pve.RetryOnTransient(ctx, nil, "ha_profile_config_nodes", 0, func() error {
		var inner error
		resp, inner = api.ListConfigNodes(ctx)
		return inner
	})
	if err != nil {
		return nil, err.Error()
	}
	if resp == nil || len(*resp) == 0 {
		return nil, "cluster config lists no nodes"
	}
	votes := make(map[string]int, len(*resp))
	for _, raw := range *resp {
		var row struct {
			Node  string            `json:"node"`
			Name  string            `json:"name"`
			Votes *sdkclient.PVEInt `json:"quorum_votes"`
		}
		if jsonErr := json.Unmarshal(raw, &row); jsonErr != nil {
			return nil, "cluster config node row unreadable: " + jsonErr.Error()
		}
		name := row.Node
		if name == "" {
			name = row.Name
		}
		if name == "" {
			return nil, "cluster config node row has no name"
		}
		n := 1
		if row.Votes != nil {
			n = int(row.Votes.Int())
		}
		if n < 0 {
			return nil, fmt.Sprintf("node %s has %d configured votes", name, n)
		}
		votes[name] = n
	}
	return votes, ""
}

// readQDevice reads GET /cluster/config/qdevice. The body is untyped, so the
// parser tolerates missing fields. An empty body or empty object means no
// QDevice. A configured QDevice carries one vote unless the body says
// otherwise, which is the fewest it can carry and so the safe count. Its
// status is connected only when the body says so, through a connected flag or
// a status or state of "connected", and a body that does not say counts as
// unknown, which makes the votes count as zero.
func readQDevice(ctx context.Context, api ClusterAPI) QDevice {
	var resp *sdkcluster.ListConfigQdeviceResponse
	err := pve.RetryOnTransient(ctx, nil, "ha_profile_qdevice", 0, func() error {
		var inner error
		resp, inner = api.ListConfigQdevice(ctx)
		return inner
	})
	if err != nil {
		return QDevice{Status: QDeviceUnknown, ReadErr: err.Error()}
	}
	if resp == nil {
		return QDevice{Status: QDeviceUnknown, ReadErr: "nil response"}
	}
	return parseQDevice(*resp)
}

// parseQDevice interprets the body of GET /cluster/config/qdevice.
func parseQDevice(raw json.RawMessage) QDevice {
	trimmed := strings.TrimSpace(string(raw))
	switch trimmed {
	case "", "null", "{}", "[]", `""`:
		return QDevice{Read: true, Status: QDeviceNone}
	}
	var wire struct {
		Votes     *sdkclient.PVEInt  `json:"votes"`
		Connected *sdkclient.PVEBool `json:"connected"`
		Status    string             `json:"status"`
		State     string             `json:"state"`
	}
	if !strings.HasPrefix(trimmed, "{") || json.Unmarshal(raw, &wire) != nil {
		return QDevice{Status: QDeviceUnknown, ReadErr: fmt.Sprintf("unreadable QDevice body %.40s", trimmed)}
	}
	q := QDevice{Read: true, Configured: true, Votes: 1, Status: QDeviceUnknown}
	if wire.Votes != nil && wire.Votes.Int() > 0 {
		q.Votes = int(wire.Votes.Int())
	}
	switch {
	case wire.Connected != nil:
		if wire.Connected.Bool() {
			q.Status = QDeviceConnected
		} else {
			q.Status = QDeviceDisconnected
		}
	default:
		for _, s := range []string{wire.Status, wire.State} {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "connected":
				q.Status = QDeviceConnected
			case "disconnected", "not connected":
				q.Status = QDeviceDisconnected
			}
			if q.Status != QDeviceUnknown {
				break
			}
		}
	}
	return q
}

// readPrivileges reads the two privileges the profile cares about. Sys.Console
// on / gates every HA write. VM.Replicate on /vms gates the replicate policy,
// and it counts only when it propagates to the VM paths below. An unreadable
// answer leaves the privilege false, so it fails closed.
func readPrivileges(ctx context.Context, r pve.TokenPermissionReader) (sysConsole, vmReplicate bool, errText string) {
	if r == nil {
		return false, false, "this client cannot read token permissions"
	}
	var errs []string
	root, err := r.EffectivePermissions(ctx, "/")
	if err != nil {
		errs = append(errs, "/: "+err.Error())
	} else {
		_, sysConsole = root["Sys.Console"]
	}
	vms, err := r.EffectivePermissions(ctx, "/vms")
	if err != nil {
		errs = append(errs, "/vms: "+err.Error())
	} else {
		vmReplicate = vms["VM.Replicate"]
	}
	return sysConsole, vmReplicate, strings.Join(errs, "; ")
}

// readStorages reads the storage definitions. One malformed definition makes
// the whole list unreadable, because a missing storage would read as absent.
func readStorages(ctx context.Context, api StorageAPI) ([]pve.StorageInfo, string) {
	var resp *sdkclusterstorage.ListStorageResponse
	err := pve.RetryOnTransient(ctx, nil, "ha_profile_storage", 0, func() error {
		var inner error
		resp, inner = api.ListStorage(ctx, nil)
		return inner
	})
	if err != nil {
		return nil, err.Error()
	}
	if resp == nil {
		return nil, "nil response"
	}
	out := make([]pve.StorageInfo, 0, len(*resp))
	for _, raw := range *resp {
		info, perr := pve.ParseStorageEntry(raw)
		if perr != nil {
			return nil, "storage definition unreadable: " + perr.Error()
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, ""
}

// clusterStatus is what the profile keeps from GET /cluster/status.
type clusterStatus struct {
	Clustered bool // a cluster or quorum row exists
	Quorate   bool // the quorate flag of that row, as the entry node sees it
	Entry     string
	Nodes     []NodeRef
}

// readClusterStatus reads GET /cluster/status once. A clustered host reports a
// cluster row with the quorate flag. A host with no cluster row is standalone
// when it lists one node, and a response we cannot read quorum from when it
// lists more, so that case counts as not quorate.
func readClusterStatus(ctx context.Context, api ClusterAPI) (*clusterStatus, error) {
	var resp *sdkcluster.ListStatusResponse
	err := pve.RetryOnTransient(ctx, nil, "ha_profile_cluster_status", 0, func() error {
		var inner error
		resp, inner = api.ListStatus(ctx)
		return inner
	})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("nil response from cluster status")
	}
	cs := &clusterStatus{}
	for _, raw := range *resp {
		var row struct {
			Type    string            `json:"type"`
			Name    string            `json:"name"`
			Online  sdkclient.PVEBool `json:"online"`
			Local   sdkclient.PVEBool `json:"local"`
			Quorate sdkclient.PVEBool `json:"quorate"`
		}
		if json.Unmarshal(raw, &row) != nil {
			continue
		}
		switch row.Type {
		case "node":
			if row.Name == "" {
				continue
			}
			cs.Nodes = append(cs.Nodes, NodeRef{Name: row.Name, Online: row.Online.Bool()})
			if row.Local.Bool() {
				cs.Entry = row.Name
			}
		case "cluster", "quorum":
			cs.Clustered = true
			cs.Quorate = row.Quorate.Bool()
		}
	}
	if !cs.Clustered {
		cs.Quorate = len(cs.Nodes) <= 1
	}
	sort.Slice(cs.Nodes, func(i, j int) bool { return cs.Nodes[i].Name < cs.Nodes[j].Name })
	return cs, nil
}

// readCRMMaster returns the name of the CRM master from manager status, or ""
// when the status names none. A read or decode failure is an error.
func readCRMMaster(ctx context.Context, api ClusterAPI) (string, error) {
	var resp *sdkcluster.ListHaStatusManagerStatusResponse
	err := pve.RetryOnTransient(ctx, nil, "ha_profile_manager_status", 0, func() error {
		var inner error
		resp, inner = api.ListHaStatusManagerStatus(ctx)
		return inner
	})
	if err != nil {
		return "", err
	}
	if resp == nil || len(*resp) == 0 || strings.TrimSpace(string(*resp)) == "null" {
		return "", nil
	}
	var top struct {
		MasterNode    string          `json:"master_node"`
		ManagerStatus json.RawMessage `json:"manager_status"`
	}
	if jsonErr := json.Unmarshal(*resp, &top); jsonErr != nil {
		return "", jsonErr
	}
	if top.MasterNode != "" {
		return top.MasterNode, nil
	}
	if strings.HasPrefix(strings.TrimSpace(string(top.ManagerStatus)), "{") {
		var nested struct {
			MasterNode string `json:"master_node"`
		}
		if jsonErr := json.Unmarshal(top.ManagerStatus, &nested); jsonErr != nil {
			return "", jsonErr
		}
		return nested.MasterNode, nil
	}
	return "", nil
}

// CRS is the cluster's resource scheduling setting.
type CRS struct {
	// Known is true when cluster options were read.
	Known bool

	// Mode is the scheduler for HA, basic when the cluster sets none.
	Mode string

	// RebalanceOnStart is true when ha-rebalance-on-start is on.
	RebalanceOnStart bool

	// AutoRebalance is true when the dynamic load balancer is on.
	AutoRebalance bool

	// Err says why the options could not be read.
	Err string
}

// readCRS reads the crs property of GET /cluster/options. PVE returns it as an
// object, and the parser also accepts the property-string form.
func readCRS(ctx context.Context, api ClusterAPI) CRS {
	var resp *sdkcluster.ListOptionsResponse
	err := pve.RetryOnTransient(ctx, nil, "ha_profile_options", 0, func() error {
		var inner error
		resp, inner = api.ListOptions(ctx)
		return inner
	})
	if err != nil {
		return CRS{Err: err.Error()}
	}
	if resp == nil {
		return CRS{Err: "nil response"}
	}
	crs, perr := parseCRS(resp.Crs)
	if perr != nil {
		return CRS{Err: perr.Error()}
	}
	return crs
}

func parseCRS(raw json.RawMessage) (CRS, error) {
	out := CRS{Known: true, Mode: "basic"}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return out, nil
	}
	kv := map[string]string{}
	switch {
	case strings.HasPrefix(trimmed, "{"):
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return CRS{}, fmt.Errorf("crs: %w", err)
		}
		for k, v := range obj {
			var s string
			if json.Unmarshal(v, &s) != nil {
				s = strings.TrimSpace(string(v))
			}
			kv[k] = s
		}
	case strings.HasPrefix(trimmed, `"`):
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return CRS{}, fmt.Errorf("crs: %w", err)
		}
		for _, part := range strings.Split(s, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if ok {
				kv[k] = v
			}
		}
	default:
		return CRS{}, fmt.Errorf("crs: unexpected shape %.20s", trimmed)
	}
	if m := strings.TrimSpace(kv["ha"]); m != "" {
		out.Mode = m
	}
	out.RebalanceOnStart = truthy(kv["ha-rebalance-on-start"])
	out.AutoRebalance = truthy(kv["ha-auto-rebalance"])
	return out, nil
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ReplicationJob is one storage replication job.
type ReplicationJob struct {
	ID        string
	Guest     int64
	Source    string
	Target    string
	FailCount int64
}

// readReplication reads GET /cluster/replication.
func readReplication(ctx context.Context, api ClusterAPI) ([]ReplicationJob, string) {
	var resp *sdkcluster.ListReplicationResponse
	err := pve.RetryOnTransient(ctx, nil, "ha_profile_replication", 0, func() error {
		var inner error
		resp, inner = api.ListReplication(ctx)
		return inner
	})
	if err != nil {
		return nil, err.Error()
	}
	if resp == nil {
		return nil, "nil response"
	}
	jobs := make([]ReplicationJob, 0, len(*resp))
	for _, raw := range *resp {
		var row struct {
			ID        string            `json:"id"`
			Guest     sdkclient.PVEInt  `json:"guest"`
			Source    string            `json:"source"`
			Target    string            `json:"target"`
			FailCount *sdkclient.PVEInt `json:"fail_count"`
		}
		if jsonErr := json.Unmarshal(raw, &row); jsonErr != nil {
			return nil, "replication job unreadable: " + jsonErr.Error()
		}
		job := ReplicationJob{ID: row.ID, Guest: row.Guest.Int(), Source: row.Source, Target: row.Target}
		if row.FailCount != nil {
			job.FailCount = row.FailCount.Int()
		}
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	return jobs, ""
}

// Version is a parsed pve-manager version.
type Version struct {
	Major, Minor, Patch int
}

// AtLeast reports whether v is at least major.minor.
func (v Version) AtLeast(major, minor int) bool {
	return v.Major > major || v.Major == major && v.Minor >= minor
}

func (v Version) less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

var versionPattern = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+))?`)

// parseVersion reads a version such as 9.2.4 or 9.0. A trailing suffix such as
// ~2 is ignored.
func parseVersion(s string) (Version, bool) {
	m := versionPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, false
	}
	var v Version
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		v.Patch, _ = strconv.Atoi(m[3])
	}
	return v, true
}
