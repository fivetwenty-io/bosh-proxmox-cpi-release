package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// HAResourceRow is the part of a GET /cluster/ha/resources row the doctor
// reads. State is the requested state, such as started, stopped, disabled, or
// ignored.
type HAResourceRow struct {
	SID     string
	State   string
	Comment string
}

// JournalView is what the doctor reads of the allocation journal. Configured is
// false when the CPI has no journal to read, which leaves the orphan copies a
// forced retirement recorded unchecked rather than absent.
type JournalView struct {
	Configured bool
	// Records holds the VM records that carry a replicated allocation.
	Records []aj.Record
}

// HAReader is the narrow slice of read-only PVE operations the ha subcommand
// depends on. Production is satisfied by pveHAReader, which adapts a real
// pve.Client; tests substitute a fake. Like Reader it exposes only read-shaped
// methods, so the doctor has no way to write to PVE.
type HAReader interface {
	// Profile probes the cluster, in mode off too, and returns the resolved
	// HA profile.
	Profile(ctx context.Context) (*ha.Profile, error)
	// Resources lists the HA resources.
	Resources(ctx context.Context) ([]HAResourceRow, error)
	// Rules lists the HA rules.
	Rules(ctx context.Context) ([]ha.Rule, error)
	// Guests lists every QEMU guest of the cluster from the authoritative
	// per-node listings. excluded names members the listing could not ask
	// because they are offline.
	Guests(ctx context.Context) (guests []pve.GuestRef, excluded []string, err error)
	// GuestMemory returns each guest's configured memory in bytes by VMID.
	GuestMemory(ctx context.Context) (map[int]int64, error)
	// GuestConfig returns the raw config of a QEMU guest.
	GuestConfig(ctx context.Context, node string, vmid int) (map[string]any, error)
	// NodeFacts returns the placement facts of the cluster's nodes.
	NodeFacts(ctx context.Context) ([]placement.NodeFacts, error)
	// ReplicationJobs lists the replication job rows.
	ReplicationJobs(ctx context.Context) ([]ha.ReplicaJobState, error)
	// ReplicationSchedules returns each replication job's schedule by job ID.
	ReplicationSchedules(ctx context.Context) (map[string]string, error)
	// ReplicaStatus reads a replication job's status on its source node.
	ReplicaStatus(ctx context.Context, node, id string) (ha.ReplicaSyncStatus, error)
	// StorageVolumes lists the volume IDs a storage holds on node.
	StorageVolumes(ctx context.Context, node, storage string) (map[string]bool, error)
	// Storages returns the storage index of the cluster.
	Storages(ctx context.Context) ([]pve.StorageInfo, error)
	// NextID returns the datacenter next-id range as an inclusive band.
	NextID(ctx context.Context) (pve.VMIDBand, error)
	// Journal reads the replicated allocations from the allocation journal.
	// nodes are the cluster's node names, which identify the cluster.
	Journal(ctx context.Context, nodes []string) (JournalView, error)
}

// replicaReader is the read half of ha.ReplicationAPI. The doctor holds only
// this half, so no write method of the replication client is reachable from it.
type replicaReader interface {
	ListJobs(ctx context.Context) ([]ha.ReplicaJobState, error)
	Status(ctx context.Context, node, id string) (ha.ReplicaSyncStatus, error)
	Volumes(ctx context.Context, node, storage string) (map[string]bool, error)
}

// pveHAReader adapts a real pve.Client to HAReader.
type pveHAReader struct {
	client   pve.Client
	cfg      *config.CPIConfig
	logger   *log.Logger
	replicas replicaReader
}

// newPVEHAReader constructs a production HAReader backed by client.
func newPVEHAReader(client pve.Client, cfg *config.CPIConfig, logger *log.Logger) HAReader {
	return &pveHAReader{client: client, cfg: cfg, logger: logger, replicas: ha.NewReplicationAPI(client)}
}

func (r *pveHAReader) ready() error {
	if r.client == nil || r.client.Cluster() == nil || r.client.Nodes() == nil {
		return errors.New("pve-cid: no cluster or nodes service available")
	}
	return nil
}

func (r *pveHAReader) Profile(ctx context.Context) (*ha.Profile, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	deps := ha.DepsFromClient(r.client, r.cfg, r.logger)
	// The doctor reports what the cluster says now, so it reads every fact
	// live instead of from the profile cache.
	deps.Slow = nil
	deps.ProbeWhenOff = true
	return ha.Resolve(ctx, deps)
}

func (r *pveHAReader) Resources(ctx context.Context) ([]HAResourceRow, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	resp, err := r.client.Cluster().ListHaResources(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list HA resources: %w", err)
	}
	if resp == nil {
		return nil, errors.New("list HA resources: nil response")
	}
	out := make([]HAResourceRow, 0, len(*resp))
	for _, raw := range *resp {
		var row struct {
			SID     string `json:"sid"`
			State   string `json:"state"`
			Comment string `json:"comment"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("HA resource row unreadable: %w", err)
		}
		if row.SID == "" {
			return nil, errors.New("HA resource row has no sid")
		}
		out = append(out, HAResourceRow{SID: row.SID, State: row.State, Comment: row.Comment})
	}
	return out, nil
}

func (r *pveHAReader) Rules(ctx context.Context) ([]ha.Rule, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	resp, err := r.client.Cluster().ListHaRules(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list HA rules: %w", err)
	}
	set, err := ha.ParseRuleList(resp)
	if err != nil {
		return nil, fmt.Errorf("parse HA rules: %w", err)
	}
	return set.Rules, nil
}

func (r *pveHAReader) Guests(ctx context.Context) ([]pve.GuestRef, []string, error) {
	return pve.ListGuestsAuthoritativeTolerant(ctx, r.client, r.logger)
}

func (r *pveHAReader) GuestMemory(ctx context.Context) (map[int]int64, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	resp, err := r.client.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{})
	if err != nil {
		return nil, fmt.Errorf("list cluster resources: %w", err)
	}
	if resp == nil {
		return nil, errors.New("list cluster resources: nil response")
	}
	out := map[int]int64{}
	for _, raw := range *resp {
		var row struct {
			Type   string           `json:"type"`
			VMID   sdkclient.PVEInt `json:"vmid"`
			Maxmem sdkclient.PVEInt `json:"maxmem"`
		}
		if err := json.Unmarshal(raw, &row); err != nil || row.Type != "qemu" || row.VMID.Int() <= 0 {
			continue
		}
		out[int(row.VMID.Int())] = row.Maxmem.Int()
	}
	return out, nil
}

func (r *pveHAReader) GuestConfig(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if r.client == nil || r.client.QEMU() == nil {
		return nil, errors.New("pve-cid: no QEMU service available")
	}
	return r.client.QEMU().Config(ctx, node, vmid)
}

func (r *pveHAReader) NodeFacts(ctx context.Context) ([]placement.NodeFacts, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	logger := r.logger
	if logger == nil {
		logger = log.NewNopLogger()
	}
	return placement.GatherNodeFacts(ctx, r.client.Cluster(), r.client.Nodes(), logger, placement.GatherOptions{})
}

func (r *pveHAReader) ReplicationJobs(ctx context.Context) ([]ha.ReplicaJobState, error) {
	return r.replicas.ListJobs(ctx)
}

func (r *pveHAReader) ReplicationSchedules(ctx context.Context) (map[string]string, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	resp, err := r.client.Cluster().ListReplication(ctx)
	if err != nil {
		return nil, fmt.Errorf("list replication jobs: %w", err)
	}
	if resp == nil {
		return nil, errors.New("list replication jobs: nil response")
	}
	out := map[string]string{}
	for _, raw := range *resp {
		var row struct {
			ID       string `json:"id"`
			Schedule string `json:"schedule"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("replication job row unreadable: %w", err)
		}
		if row.ID != "" {
			out[row.ID] = strings.TrimSpace(row.Schedule)
		}
	}
	return out, nil
}

func (r *pveHAReader) ReplicaStatus(ctx context.Context, node, id string) (ha.ReplicaSyncStatus, error) {
	return r.replicas.Status(ctx, node, id)
}

func (r *pveHAReader) StorageVolumes(ctx context.Context, node, storage string) (map[string]bool, error) {
	return r.replicas.Volumes(ctx, node, storage)
}

func (r *pveHAReader) Storages(ctx context.Context) ([]pve.StorageInfo, error) {
	if r.client == nil || r.client.ClusterStorage() == nil {
		return nil, errors.New("pve-cid: no cluster storage service available")
	}
	resp, err := r.client.ClusterStorage().ListStorage(ctx, &sdkclusterstorage.ListStorageParams{})
	if err != nil {
		return nil, fmt.Errorf("list storages: %w", err)
	}
	if resp == nil {
		return nil, errors.New("list storages: nil response")
	}
	out := make([]pve.StorageInfo, 0, len(*resp))
	for _, raw := range *resp {
		info, perr := pve.ParseStorageEntry(raw)
		if perr != nil {
			// A storage the parser cannot read could be the one that hides an
			// overlap, so the whole index is unusable rather than shorter.
			return nil, fmt.Errorf("storage entry unreadable: %w", perr)
		}
		out = append(out, info)
	}
	return out, nil
}

func (r *pveHAReader) NextID(ctx context.Context) (pve.VMIDBand, error) {
	if err := r.ready(); err != nil {
		return pve.VMIDBand{}, err
	}
	resp, err := r.client.Cluster().ListOptions(ctx)
	if err != nil {
		return pve.VMIDBand{}, fmt.Errorf("read datacenter options: %w", err)
	}
	if resp == nil {
		return pve.VMIDBand{}, errors.New("read datacenter options: nil response")
	}
	return parseNextID(resp.NextId)
}

// parseNextID reads the next-id option of GET /cluster/options, which PVE
// renders as an object with lower and upper, or as the property string
// "lower=N,upper=M". An absent option takes PVE's default range. A bound that
// is not a whole number is an error, because guessing it would hide an overlap.
func parseNextID(raw json.RawMessage) (pve.VMIDBand, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return pve.NextIDRange(0, 0), nil
	}
	fields := map[string]string{}
	switch {
	case strings.HasPrefix(trimmed, "{"):
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return pve.VMIDBand{}, fmt.Errorf("next-id option unreadable: %w", err)
		}
		for k, v := range obj {
			var s string
			if json.Unmarshal(v, &s) != nil {
				s = strings.TrimSpace(string(v))
			}
			fields[k] = s
		}
	case strings.HasPrefix(trimmed, `"`):
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return pve.VMIDBand{}, fmt.Errorf("next-id option unreadable: %w", err)
		}
		for _, part := range strings.Split(text, ",") {
			if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok {
				fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	default:
		return pve.VMIDBand{}, fmt.Errorf("next-id option has an unexpected shape: %s", trimmed)
	}
	bound := func(key string) (int, error) {
		v, ok := fields[key]
		if !ok || v == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("next-id %s bound %q is not a whole number", key, v)
		}
		return n, nil
	}
	lower, err := bound("lower")
	if err != nil {
		return pve.VMIDBand{}, err
	}
	upper, err := bound("upper")
	if err != nil {
		return pve.VMIDBand{}, err
	}
	return pve.NextIDRange(lower, upper), nil
}

func (r *pveHAReader) Journal(ctx context.Context, nodes []string) (JournalView, error) {
	if r.cfg == nil || r.cfg.ValidateStoragePlacementAllocation() != nil {
		return JournalView{}, nil
	}
	if err := r.ready(); err != nil {
		return JournalView{Configured: true}, err
	}
	identity, err := pve.ObserveStorageClusterIdentity(ctx, r.client.Nodes(), nodes)
	if err != nil {
		return JournalView{Configured: true}, fmt.Errorf("identify the cluster for the journal: %w", err)
	}
	journal, err := aj.Open(r.cfg.StorageAllocationJournalDir, r.cfg.StoragePlacementNamespace, identity.ID())
	if err != nil {
		return JournalView{Configured: true}, fmt.Errorf("open the allocation journal: %w", err)
	}
	records, listErr := journal.List()
	closeErr := journal.Close()
	if err := errors.Join(listErr, closeErr); err != nil {
		return JournalView{Configured: true}, fmt.Errorf("read the allocation journal: %w", err)
	}
	view := JournalView{Configured: true}
	for i := range records {
		if records[i].Kind == "vm" && records[i].Replication != nil {
			view.Records = append(view.Records, records[i])
		}
	}
	return view, nil
}
