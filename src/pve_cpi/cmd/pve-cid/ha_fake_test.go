package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const haGiB = int64(1) << 30

var haNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

const haDirector = "11111111-2222-3333-4444-555555555555"

// fakeHA is an HAReader that serves fixed cluster state and fixed errors. It
// holds no write method, as the interface has none.
type fakeHA struct {
	profile    *ha.Profile
	profileErr error

	resources    []HAResourceRow
	resourcesErr error
	rules        []ha.Rule
	rulesErr     error

	guests    []pve.GuestRef
	excluded  []string
	guestsErr error
	memory    map[int]int64
	memoryErr error
	configs   map[int]map[string]any
	configErr map[int]error

	facts    []placement.NodeFacts
	factsErr error

	jobs         []ha.ReplicaJobState
	jobsErr      error
	schedules    map[string]string
	schedulesErr error
	status       map[string]ha.ReplicaSyncStatus
	statusErr    map[string]error
	volumes      map[string]map[string]bool
	volumesErr   map[string]error

	storages    []pve.StorageInfo
	storagesErr error
	nextID      pve.VMIDBand
	nextIDErr   error

	journal    JournalView
	journalErr error
}

var _ HAReader = (*fakeHA)(nil)

func (f *fakeHA) Profile(context.Context) (*ha.Profile, error) { return f.profile, f.profileErr }

func (f *fakeHA) Resources(context.Context) ([]HAResourceRow, error) {
	return f.resources, f.resourcesErr
}

func (f *fakeHA) Rules(context.Context) ([]ha.Rule, error) { return f.rules, f.rulesErr }

func (f *fakeHA) Guests(context.Context) ([]pve.GuestRef, []string, error) {
	return f.guests, f.excluded, f.guestsErr
}

func (f *fakeHA) GuestMemory(context.Context) (map[int]int64, error) { return f.memory, f.memoryErr }

func (f *fakeHA) GuestConfig(_ context.Context, _ string, vmid int) (map[string]any, error) {
	if err := f.configErr[vmid]; err != nil {
		return nil, err
	}
	return f.configs[vmid], nil
}

func (f *fakeHA) NodeFacts(context.Context) ([]placement.NodeFacts, error) {
	return f.facts, f.factsErr
}

func (f *fakeHA) ReplicationJobs(context.Context) ([]ha.ReplicaJobState, error) {
	return f.jobs, f.jobsErr
}

func (f *fakeHA) ReplicationSchedules(context.Context) (map[string]string, error) {
	return f.schedules, f.schedulesErr
}

func (f *fakeHA) ReplicaStatus(_ context.Context, _, id string) (ha.ReplicaSyncStatus, error) {
	if err := f.statusErr[id]; err != nil {
		return ha.ReplicaSyncStatus{}, err
	}
	return f.status[id], nil
}

func (f *fakeHA) StorageVolumes(_ context.Context, node, storage string) (map[string]bool, error) {
	key := node + "/" + storage
	if err := f.volumesErr[key]; err != nil {
		return nil, err
	}
	return f.volumes[key], nil
}

func (f *fakeHA) Storages(context.Context) ([]pve.StorageInfo, error) {
	return f.storages, f.storagesErr
}

func (f *fakeHA) NextID(context.Context) (pve.VMIDBand, error) { return f.nextID, f.nextIDErr }

func (f *fakeHA) Journal(context.Context, []string) (JournalView, error) {
	return f.journal, f.journalErr
}

// haCfg builds a valid CPI config with the given HA mode and local policy.
func haCfg(mode, policy string) *config.CPIConfig {
	text := `{"host":"pve.test.local","user":"root@pam","api_token":"test-token","vm_storage":"local-lvm",` +
		`"disk_storage":"local-lvm","network_bridge":"vmbr0","ha":{"mode":"` + mode + `","local_policy":"` + policy + `"}}`
	cfg, err := config.Load(strings.NewReader(text))
	if err != nil {
		panic(err)
	}
	return cfg
}

// haHealthyProfile is a three-node cluster with every prerequisite met.
func haHealthyProfile(mode string) *ha.Profile {
	p := &ha.Profile{
		Mode: mode, Effective: ha.EffectiveFull, Clustered: true, Quorate: true, RulesSupported: true,
		HAVersionOK: true, MinVersion: "9.1.1", Topology: "mixed", EntryNode: "n1", CRMMaster: "n1",
		Quorum:     ha.Quorum{Known: true, Expected: 3, Participating: 3, VotingNodes: 3, SurvivesOne: true},
		CRS:        ha.CRS{Known: true, Mode: "static", AutoRebalance: true},
		CPUDomains: []ha.CPUDomain{{Model: "EPYC", Nodes: []string{"n1", "n2", "n3"}}},
		Health:     &pve.NodeHealthReport{},
	}
	for _, name := range []string{"n1", "n2", "n3"} {
		p.Nodes = append(p.Nodes, ha.NodeFacts{
			Name: name, Online: true, Votes: 1, VotesKnown: true, Version: "9.1.1",
			CPU: ha.CPUInfo{Known: true, Model: "EPYC"},
		})
		p.Health.Nodes = append(p.Health.Nodes, pve.NodeHealthEntry{
			Name: name, Health: pve.NodeHealthOnline, Placeable: true, ProxyTarget: true,
		})
	}
	return p
}

// haMarker is the ownership marker the CPI writes for the test Director.
const haMarker = "bosh-cpi:v1 owner=" + haDirector

// haBaseFake is a healthy three-node cluster with one replicate-free guest on
// shared storage, so a run against it reports nothing.
func haBaseFake() *fakeHA {
	return &fakeHA{
		profile: haHealthyProfile(config.HAModeAuto),
		guests: []pve.GuestRef{
			{VMID: 100, Node: "n1", Name: "vm-100", Tags: "bosh-cpi;bosh-ha-managed", Status: "running"},
		},
		memory:    map[int]int64{100: 4 * haGiB},
		resources: []HAResourceRow{{SID: "vm:100", State: "started", Comment: haMarker}},
		configs: map[int]map[string]any{
			100: {"cpu": "x86-64-v2-AES", "scsi0": "nfs1:100/vm-100-disk-0.raw,size=10G"},
		},
		facts: []placement.NodeFacts{
			{Node: "n1", Online: true, TotalMemBytes: 64 * haGiB},
			{Node: "n2", Online: true, TotalMemBytes: 64 * haGiB},
			{Node: "n3", Online: true, TotalMemBytes: 64 * haGiB},
		},
		storages: []pve.StorageInfo{
			{Name: "nfs1", Type: "nfs", Shared: true, Content: "images"},
			{Name: "local-zfs", Type: "zfspool", Content: "images"},
		},
		nextID:    pve.NextIDRange(100, 1000000),
		schedules: map[string]string{},
		status:    map[string]ha.ReplicaSyncStatus{},
		journal:   JournalView{Configured: true},
	}
}

var errFake = errors.New("fake read failure")
