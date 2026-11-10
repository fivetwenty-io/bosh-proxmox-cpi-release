package pve

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// discClient serves the SDK-service fakes a discovery read talks to.
type discClient struct {
	Client
	nodes   *fakeNodesService
	cluster *fakeClusterService
}

func (c *discClient) Nodes() sdknodes.Service {
	if c.nodes == nil {
		return nil
	}
	return c.nodes
}

func (c *discClient) Cluster() sdkcluster.Service {
	if c.cluster == nil {
		return nil
	}
	return c.cluster
}

func jsonRow(t *testing.T, fields map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// discCluster is a three-node cluster fixture whose reads the tests change.
type discCluster struct {
	t       *testing.T
	root    string
	rootFP  string
	rootDER []byte

	mu          sync.Mutex
	certCalls   []string
	nodeRows    []json.RawMessage
	nodesErr    error
	statusRows  []json.RawMessage
	statusErr   error
	certFailure map[string]error
	rootless    map[string]bool
	// names are the cluster members the config nodes answer lists.
	names []string
	// membersErr fails the config nodes read.
	membersErr error
	// hang makes a node's certificates/info block until its context ends.
	hang map[string]bool
	// certDelay holds every certificates/info read that long, so reads overlap.
	certDelay   time.Duration
	inflight    int
	maxInflight int
}

func newDiscCluster(t *testing.T) *discCluster {
	t.Helper()
	root, fingerprint, der := clusterIdentityRootFixture(t, 11, true)
	d := &discCluster{t: t, root: root, rootFP: fingerprint, rootDER: der,
		certFailure: map[string]error{}, rootless: map[string]bool{}, hang: map[string]bool{}}
	for _, name := range []string{"pve1", "pve2", "pve3"} {
		d.nodeRows = append(d.nodeRows, d.nodeRow(name, true))
	}
	d.names = []string{"pve1", "pve2", "pve3"}
	d.statusRows = []json.RawMessage{
		jsonRow(t, map[string]any{"type": "cluster", "name": "lab", "quorate": 1, "nodes": 3}),
		jsonRow(t, map[string]any{"type": "node", "name": "pve1", "online": 1, "local": 1}),
		jsonRow(t, map[string]any{"type": "node", "name": "pve2", "online": 1, "local": 0}),
		jsonRow(t, map[string]any{"type": "node", "name": "pve3", "online": 0, "local": 0}),
	}
	return d
}

// setNodes replaces the cluster with the named members, each online in /nodes,
// and a quorate status that answers from local. A name in offline is reported
// offline by the status.
func (d *discCluster) setNodes(local string, names []string, offline ...string) {
	d.names = names
	d.nodeRows = nil
	d.statusRows = []json.RawMessage{
		jsonRow(d.t, map[string]any{"type": "cluster", "name": "lab", "quorate": 1, "nodes": len(names)}),
	}
	for _, name := range names {
		d.nodeRows = append(d.nodeRows, d.nodeRow(name, true))
		d.statusRows = append(d.statusRows, jsonRow(d.t, map[string]any{
			"type": "node", "name": name, "local": boolInt(name == local), "online": boolInt(!slices.Contains(offline, name)),
		}))
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// certReads returns the nodes certificates/info was read for, sorted.
func (d *discCluster) certReads() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	reads := slices.Clone(d.certCalls)
	slices.Sort(reads)
	return reads
}

func (d *discCluster) nodeRow(name string, withFingerprint bool) json.RawMessage {
	row := map[string]any{"node": name, "status": "online"}
	if withFingerprint {
		fingerprint, _ := discFingerprint("node:" + name)
		row["ssl_fingerprint"] = fingerprint
	}
	return jsonRow(d.t, row)
}

func (d *discCluster) client() *discClient {
	return &discClient{
		nodes: &fakeNodesService{
			listNodesFn: func(context.Context) (*sdknodes.ListNodesResponse, error) {
				d.mu.Lock()
				defer d.mu.Unlock()
				if d.nodesErr != nil {
					return nil, d.nodesErr
				}
				rows := sdknodes.ListNodesResponse(slices.Clone(d.nodeRows))
				return &rows, nil
			},
			listCertificatesInfoFn: func(ctx context.Context, node string) (*sdknodes.ListCertificatesInfoResponse, error) {
				d.mu.Lock()
				d.certCalls = append(d.certCalls, node)
				d.inflight++
				d.maxInflight = max(d.maxInflight, d.inflight)
				hang, delay := d.hang[node], d.certDelay
				d.mu.Unlock()
				defer func() {
					d.mu.Lock()
					d.inflight--
					d.mu.Unlock()
				}()
				if hang {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				if delay > 0 {
					select {
					case <-time.After(delay):
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				d.mu.Lock()
				defer d.mu.Unlock()
				if err := d.certFailure[node]; err != nil {
					return nil, err
				}
				return d.certAnswer(node), nil
			},
		},
		cluster: &fakeClusterService{
			listStatusFn: func(context.Context) (*sdkcluster.ListStatusResponse, error) {
				d.mu.Lock()
				defer d.mu.Unlock()
				if d.statusErr != nil {
					return nil, d.statusErr
				}
				rows := sdkcluster.ListStatusResponse(slices.Clone(d.statusRows))
				return &rows, nil
			},
			listConfigNodesFn: func(context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
				d.mu.Lock()
				defer d.mu.Unlock()
				if d.membersErr != nil {
					return nil, d.membersErr
				}
				rows := sdkcluster.ListConfigNodesResponse{}
				for _, name := range d.names {
					rows = append(rows, jsonRow(d.t, map[string]any{"name": name}))
				}
				return &rows, nil
			},
		},
	}
}

// certAnswer is node's certificates/info answer, with the shared root and the
// node's own two certificates.
func (d *discCluster) certAnswer(node string) *sdknodes.ListCertificatesInfoResponse {
	sslFP, _ := discFingerprint("cert:" + node + ":ssl")
	proxyFP, _ := discFingerprint("cert:" + node + ":proxy")
	rows := []json.RawMessage{
		jsonRow(d.t, map[string]any{"filename": "pve-ssl.pem", "fingerprint": sslFP}),
		jsonRow(d.t, map[string]any{"filename": "pveproxy-ssl.pem", "fingerprint": proxyFP}),
	}
	if !d.rootless[node] {
		rows = append(rows, jsonRow(d.t, map[string]any{
			"filename": "pve-root-ca.pem", "pem": d.root, "fingerprint": d.rootFP,
		}))
	}
	answer := sdknodes.ListCertificatesInfoResponse(rows)
	return &answer
}

func certPin(node, which string) endpointpool.Pin {
	_, pin := discFingerprint("cert:" + node + ":" + which)
	return pin
}

func TestReadDiscovery_FullRead(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	reads, err := ReadDiscovery(lgCtx(), d.client())
	if err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if want := sha256.Sum256(d.rootDER); reads.RootCA != want {
		t.Fatalf("RootCA = %x, want the root's full digest %x", reads.RootCA, want)
	}
	status := reads.Status
	if !status.Available || !status.InCluster || !status.Quorate || status.Local != "pve1" ||
		!status.Online["pve2"] || status.Online["pve3"] {
		t.Fatalf("status = %+v", status)
	}
	if len(reads.Nodes) != 3 {
		t.Fatalf("%d nodes, want 3", len(reads.Nodes))
	}
	for _, node := range reads.Nodes {
		wantFP, _ := discFingerprint("node:" + node.Name)
		if node.Fingerprint != wantFP || node.Status != "online" {
			t.Fatalf("node %+v, want the /nodes fingerprint and status", node)
		}
		want := []endpointpool.Pin{certPin(node.Name, "ssl"), certPin(node.Name, "proxy")}
		if !slices.Equal(node.CertPins, want) {
			t.Fatalf("node %s cert pins = %v, want %v", node.Name, node.CertPins, want)
		}
	}
}

func TestReadDiscovery_RootComesFromTheNodeThatAnswered(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	// pve2 is local, and pve1 would come first in name order.
	d.setNodes("pve2", []string{"pve1", "pve2", "pve3"})
	reads, err := ReadDiscovery(lgCtx(), d.client())
	if err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if calls := d.certReads(); len(calls) == 0 {
		t.Fatal("no certificate read")
	}
	if d.certCalls[0] != "pve2" {
		t.Fatalf("first certificates/info call = %v, want the local node first", d.certCalls)
	}
	if len(reads.Nodes[1].CertPins) != 2 {
		t.Fatalf("the local node's pins come from the root read: %v", reads.Nodes[1].CertPins)
	}
}

func TestReadDiscovery_WithoutAStatusTheFirstOnlineNodeAnswersTheRoot(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	d.statusErr = &sdkerrors.APIError{HTTPCode: 403, Message: "Permission check failed (/, Sys.Audit)"}
	fingerprint, _ := discFingerprint("node:pve1")
	d.nodeRows[0] = jsonRow(t, map[string]any{"node": "pve1", "status": "offline", "ssl_fingerprint": fingerprint})
	reads, err := ReadDiscovery(lgCtx(), d.client())
	if err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if reads.Status.Available {
		t.Fatal("the status must be unavailable")
	}
	if d.certCalls[0] != "pve2" {
		t.Fatalf("first certificates/info call = %v, want pve2, the first node /nodes lists as online", d.certCalls)
	}
	if len(d.certCalls) != 3 || slices.Contains(d.certCalls[1:], "pve2") || d.certCalls[0] == "pve1" {
		t.Fatalf("calls = %v, want pve2 once, from the root read", d.certCalls)
	}
}

func TestReadDiscovery_EachNodeIsReadOnceAndOnlyWhenItCanMakeTheResult(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	if _, err := ReadDiscovery(lgCtx(), d.client()); err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if got := d.certReads(); !slices.Equal(got, []string{"pve1", "pve2", "pve3"}) {
		t.Fatalf("calls = %v, want each node once, the local one from the root read alone", got)
	}

	// Rows with a missing or unparseable fingerprint, a repeat of a name in
	// another case, and a name the helper rejects are never read.
	d = newDiscCluster(t)
	d.nodeRows = append(d.nodeRows,
		d.nodeRow("pve4", false),
		jsonRow(t, map[string]any{"node": "pve5", "status": "online", "ssl_fingerprint": "AA:BB"}),
		d.nodeRow("PVE2", true),
		d.nodeRow("localhost", true))
	if _, err := ReadDiscovery(lgCtx(), d.client()); err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if got := d.certReads(); !slices.Equal(got, []string{"pve1", "pve2", "pve3"}) {
		t.Fatalf("calls = %v, want only the nodes that can make the result", got)
	}

	// Without a status the only row is the seed's own node, and the root read
	// is the one call.
	d = newDiscCluster(t)
	d.nodeRows = d.nodeRows[:1]
	d.statusErr = errors.New("no status")
	if _, err := ReadDiscovery(lgCtx(), d.client()); err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if got := d.certReads(); !slices.Equal(got, []string{"pve1"}) {
		t.Fatalf("calls = %v, want the root read alone", got)
	}
}

func TestReadDiscovery_HungCertificateReadKeepsTheResult(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	d.hang["pve3"] = true
	start := time.Now()
	reads, err := readDiscovery(lgCtx(), d.client(), 30*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("one hung node took %v, want about its own 100ms timeout", elapsed)
	}
	if len(reads.Nodes[0].CertPins) != 2 || len(reads.Nodes[1].CertPins) != 2 || len(reads.Nodes[2].CertPins) != 0 {
		t.Fatalf("cert pins = %v, %v, %v, want only the hung pve3 empty",
			reads.Nodes[0].CertPins, reads.Nodes[1].CertPins, reads.Nodes[2].CertPins)
	}
	if reads.Nodes[2].Fingerprint == "" || reads.RootCA == ([sha256.Size]byte{}) {
		t.Fatalf("the node keeps its /nodes fingerprint and the root stands: %+v", reads)
	}
}

func TestReadDiscovery_OverallDeadlineCutsTheRemainingReads(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	d.setNodes("pve1", []string{"pve1", "pve2", "pve3", "pve4", "pve5", "pve6", "pve7"})
	for _, name := range d.names[1:] {
		d.hang[name] = true
	}
	start := time.Now()
	reads, err := readDiscovery(lgCtx(), d.client(), 300*time.Millisecond, 30*time.Second)
	if err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the cut took %v, want about the 300ms deadline", elapsed)
	}
	if len(reads.Nodes) != 7 || len(reads.Nodes[0].CertPins) != 2 {
		t.Fatalf("the root node keeps its pins and every row stands: %+v", reads.Nodes)
	}
	for _, node := range reads.Nodes[1:] {
		if len(node.CertPins) != 0 || node.Fingerprint == "" {
			t.Fatalf("node %+v, want its /nodes fingerprint alone", node)
		}
	}
	if got := len(d.certReads()); got > 1+discoveryCertWorkers {
		t.Fatalf("%d reads, want the root read and at most one per worker before the cut", got)
	}
}

func TestReadDiscovery_CertificateReadsRunAtMostFourAtOnce(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	var names []string
	for i := 0; i < 20; i++ {
		names = append(names, fmt.Sprintf("pve%02d", i))
	}
	d.setNodes("pve00", names)
	d.certDelay = 20 * time.Millisecond
	if _, err := ReadDiscovery(lgCtx(), d.client()); err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if d.maxInflight > 4 {
		t.Fatalf("%d certificate reads ran at once, want at most 4", d.maxInflight)
	}
	if d.maxInflight < 2 {
		t.Fatalf("%d certificate read at a time, want the reads to overlap", d.maxInflight)
	}
	if got := len(d.certReads()); got != 20 {
		t.Fatalf("%d reads, want 20", got)
	}
}

func TestReadDiscovery_StatusForbiddenIsNotAnError(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	d.statusErr = &sdkerrors.APIError{HTTPCode: 403, Message: "Permission check failed (/, Sys.Audit)"}
	reads, err := ReadDiscovery(lgCtx(), d.client())
	if err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if reads.Status.Available || reads.Status.Local != "" || len(reads.Status.Online) != 0 {
		t.Fatalf("status = %+v, want an unavailable status", reads.Status)
	}
	if len(reads.Nodes) != 3 || reads.RootCA == ([sha256.Size]byte{}) {
		t.Fatalf("the nodes and the root must still be read: %+v", reads)
	}
}

func TestReadDiscovery_StandaloneStatusHasNoClusterRow(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	d.nodeRows = d.nodeRows[:1]
	d.statusRows = []json.RawMessage{jsonRow(t, map[string]any{"type": "node", "name": "pve1", "online": 1, "local": 1})}
	reads, err := ReadDiscovery(lgCtx(), d.client())
	if err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if !reads.Status.Available || reads.Status.InCluster || reads.Status.Quorate || reads.Status.Local != "pve1" {
		t.Fatalf("status = %+v, want an available status with no cluster", reads.Status)
	}
}

func TestReadDiscovery_StatusRowsThatDoNotDecodeAreSkipped(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	d.statusRows = append(d.statusRows, json.RawMessage(`"not an object"`),
		jsonRow(t, map[string]any{"type": "node", "name": "", "online": 1}),
		jsonRow(t, map[string]any{"type": "quorum", "name": "x"}))
	reads, err := ReadDiscovery(lgCtx(), d.client())
	if err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if len(reads.Status.Online) != 3 || !reads.Status.Quorate {
		t.Fatalf("status = %+v", reads.Status)
	}
}

func TestReadDiscovery_NodesFailureAndEmptyListGiveNoResult(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	d.nodesErr = &sdkerrors.APIError{HTTPCode: 500, Message: "boom"}
	if _, err := ReadDiscovery(lgCtx(), d.client()); !errors.Is(err, ErrDiscoveryNodes) {
		t.Fatalf("a failed /nodes: err = %v, want ErrDiscoveryNodes", err)
	}
	d = newDiscCluster(t)
	d.nodeRows = nil
	if _, err := ReadDiscovery(lgCtx(), d.client()); !errors.Is(err, ErrDiscoveryNodes) {
		t.Fatalf("an empty /nodes: err = %v, want ErrDiscoveryNodes", err)
	}
	if len(d.certCalls) != 0 {
		t.Fatalf("an empty /nodes must stop before certificates/info, got calls %v", d.certCalls)
	}
}

func TestReadDiscovery_ErrorsCarryNoServerText(t *testing.T) {
	t.Parallel()
	const secret = "SECRET-BODY-TEXT token=abc"
	d := newDiscCluster(t)
	d.nodesErr = &sdkerrors.APIError{HTTPCode: 500, Message: secret}
	_, err := ReadDiscovery(lgCtx(), d.client())
	if !errors.Is(err, ErrDiscoveryNodes) {
		t.Fatalf("err = %v, want ErrDiscoveryNodes", err)
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "token") {
		t.Fatalf("the error echoes the server's message: %v", err)
	}
	if want := ErrDiscoveryNodes.Error() + " (HTTP 500)"; err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}

	// The root read fails on every node, and the member list read fails with
	// a message of its own.
	d = newDiscCluster(t)
	for _, name := range d.names {
		d.certFailure[name] = errors.New(secret)
	}
	d.membersErr = &sdkerrors.APIError{HTTPCode: 500, Message: secret}
	_, err = ReadDiscovery(lgCtx(), d.client())
	if !errors.Is(err, ErrDiscoveryRoot) {
		t.Fatalf("err = %v, want ErrDiscoveryRoot", err)
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "pve1") {
		t.Fatalf("the error echoes server text or a node name: %v", err)
	}
}

func TestReadDiscovery_InvalidNodeNameRejectsTheWholeResult(t *testing.T) {
	t.Parallel()
	bad := map[string]json.RawMessage{
		"dotted name": nil, "number name": nil, "row that is not an object": nil,
	}
	bad["dotted name"] = jsonRow(t, map[string]any{"node": "api.attacker.example", "status": "online"})
	bad["number name"] = jsonRow(t, map[string]any{"node": 7, "status": "online"})
	bad["row that is not an object"] = json.RawMessage(`[1]`)
	for name, row := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newDiscCluster(t)
			d.nodeRows = append(d.nodeRows, row)
			_, err := ReadDiscovery(lgCtx(), d.client())
			if !errors.Is(err, ErrDiscoveryNodeName) {
				t.Fatalf("err = %v, want ErrDiscoveryNodeName", err)
			}
			if strings.Contains(err.Error(), "attacker") {
				t.Fatalf("the error echoes a node name: %v", err)
			}
			if len(d.certCalls) != 0 {
				t.Fatalf("no certificate may be read for a rejected result, got %v", d.certCalls)
			}
		})
	}
}

func TestReadDiscovery_MissingFingerprintIsReadAsEmpty(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	d.nodeRows[1] = d.nodeRow("pve2", false)
	d.nodeRows[2] = jsonRow(t, map[string]any{"node": "pve3", "status": "online", "ssl_fingerprint": 5})
	reads, err := ReadDiscovery(lgCtx(), d.client())
	if err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if reads.Nodes[1].Fingerprint != "" || reads.Nodes[2].Fingerprint != "" || reads.Nodes[0].Fingerprint == "" {
		t.Fatalf("nodes = %+v", reads.Nodes)
	}
}

func TestReadDiscovery_FailedCertificateReadPinsByNodesFingerprintAlone(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	d.certFailure["pve2"] = errors.New("connection reset")
	reads, err := ReadDiscovery(lgCtx(), d.client())
	if err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if len(reads.Nodes[1].CertPins) != 0 || len(reads.Nodes[0].CertPins) != 2 || len(reads.Nodes[2].CertPins) != 2 {
		t.Fatalf("cert pins = %v, %v, %v, want only pve2 empty",
			reads.Nodes[0].CertPins, reads.Nodes[1].CertPins, reads.Nodes[2].CertPins)
	}
}

func TestReadDiscovery_UnreadableRootGivesNoResult(t *testing.T) {
	t.Parallel()
	cases := map[string]func(d *discCluster){
		"every node fails": func(d *discCluster) {
			for _, name := range []string{"pve1", "pve2", "pve3"} {
				d.certFailure[name] = errors.New("connection reset")
			}
		},
		"no node reports a root": func(d *discCluster) {
			for _, name := range []string{"pve1", "pve2", "pve3"} {
				d.rootless[name] = true
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newDiscCluster(t)
			setup(d)
			_, err := ReadDiscovery(lgCtx(), d.client())
			if !errors.Is(err, ErrDiscoveryRoot) {
				t.Fatalf("err = %v, want ErrDiscoveryRoot", err)
			}
		})
	}
}

func TestReadDiscovery_RootFallsBackToOtherNodes(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	d.certFailure["pve1"] = errors.New("connection reset")
	d.rootless["pve1"] = true
	reads, err := ReadDiscovery(lgCtx(), d.client())
	if err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if reads.RootCA != sha256.Sum256(d.rootDER) {
		t.Fatal("the root must come from another member when the local node fails")
	}
	if len(reads.Nodes[0].CertPins) != 0 {
		t.Fatalf("pve1's read failed, so it keeps no cert pins: %v", reads.Nodes[0].CertPins)
	}
}

func TestReadDiscovery_AmbiguousNamesGetNoCertificateRead(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	d.nodeRows = append(d.nodeRows, d.nodeRow("localhost", true), d.nodeRow("12345", true))
	reads, err := ReadDiscovery(lgCtx(), d.client())
	if err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	if len(reads.Nodes) != 5 {
		t.Fatalf("%d nodes, want the ambiguous ones kept for the builder to drop", len(reads.Nodes))
	}
	if slices.Contains(d.certCalls, "localhost") || slices.Contains(d.certCalls, "12345") {
		t.Fatalf("certificates/info was read for an ambiguous name: %v", d.certCalls)
	}
}

func TestReadDiscovery_CertificateReadsAreCappedInPreferenceOrder(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	var names, offline []string
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("n%03d", i)
		names = append(names, name)
		if i < 40 {
			offline = append(offline, name)
		}
	}
	// n000 answers. The status reports n000 through n039 offline, so the 64
	// reads go to n040 through n099 and then to n001 through n004.
	d.setNodes("n000", names, offline...)
	if _, err := ReadDiscovery(lgCtx(), d.client()); err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	want := slices.Clone(names[40:])
	want = append(want, "n000", "n001", "n002", "n003", "n004")
	slices.Sort(want)
	got := d.certReads()
	if len(got) != 65 {
		t.Fatalf("%d certificate reads, want the root read and exactly 64 more", len(got))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("reads = %v, want the online nodes first and then the lowest names", got)
	}
}

func TestReadDiscovery_CertificateReadsFollowTheNodesStatusWithoutAStatus(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	d.nodeRows = nil
	for i := 0; i < 70; i++ {
		row := map[string]any{"node": fmt.Sprintf("n%03d", i), "status": "offline"}
		if i >= 10 {
			row["status"] = "online"
		}
		fingerprint, _ := discFingerprint(fmt.Sprintf("node:n%03d", i))
		row["ssl_fingerprint"] = fingerprint
		d.nodeRows = append(d.nodeRows, jsonRow(t, row))
	}
	d.statusErr = errors.New("no status")
	if _, err := ReadDiscovery(lgCtx(), d.client()); err != nil {
		t.Fatalf("ReadDiscovery: %v", err)
	}
	// The root read goes to n010, the first online row, and it counts as one
	// of the 64, so 63 more reads reach n011 through n069 and then n000
	// through n003.
	got := d.certReads()
	if len(got) != 64 || slices.Contains(got, "n004") || !slices.Contains(got, "n010") || !slices.Contains(got, "n003") {
		t.Fatalf("reads = %v, want 64 with the online nodes first", got)
	}
}

func TestReadDiscovery_CanceledContextAndMissingServices(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	ctx, cancel := context.WithCancel(lgCtx())
	cancel()
	if _, err := ReadDiscovery(ctx, d.client()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	if _, err := ReadDiscovery(lgCtx(), nil); err == nil {
		t.Fatal("a nil client must fail")
	}
	if _, err := ReadDiscovery(lgCtx(), &discClient{}); err == nil {
		t.Fatal("a client with no services must fail")
	}
	var noContext context.Context
	if _, err := ReadDiscovery(noContext, d.client()); err == nil {
		t.Fatal("a nil context must fail")
	}
}

func TestReadDiscovery_CancellationDuringCertificateReads(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	ctx, cancel := context.WithCancel(lgCtx())
	client := d.client()
	inner := client.nodes.listCertificatesInfoFn
	var calls int
	client.nodes.listCertificatesInfoFn = func(c context.Context, node string) (*sdknodes.ListCertificatesInfoResponse, error) {
		d.mu.Lock()
		calls++
		first := calls == 1
		d.mu.Unlock()
		answer, err := inner(c, node)
		if !first {
			cancel()
		}
		return answer, err
	}
	if _, err := ReadDiscovery(ctx, client); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
}

func TestNodeCertificatePins(t *testing.T) {
	t.Parallel()
	pemText, fingerprint, der := clusterIdentityRootFixture(t, 21, false)
	derived := endpointpool.PinOf(der)
	otherFP, other := discFingerprint("other")
	row := func(fields map[string]any) json.RawMessage { return jsonRow(t, fields) }
	cases := map[string]struct {
		rows []json.RawMessage
		want []endpointpool.Pin
	}{
		"both files report a fingerprint": {
			rows: []json.RawMessage{
				row(map[string]any{"filename": "pve-ssl.pem", "fingerprint": otherFP}),
				row(map[string]any{"filename": "pveproxy-ssl.pem", "fingerprint": fingerprint}),
			},
			want: []endpointpool.Pin{other, derived},
		},
		"pem alone": {
			rows: []json.RawMessage{row(map[string]any{"filename": "pveproxy-ssl.pem", "pem": pemText})},
			want: []endpointpool.Pin{derived},
		},
		"pem and fingerprint agree": {
			rows: []json.RawMessage{row(map[string]any{"filename": "pve-ssl.pem", "pem": pemText, "fingerprint": fingerprint})},
			want: []endpointpool.Pin{derived},
		},
		"pem and fingerprint disagree": {
			rows: []json.RawMessage{row(map[string]any{"filename": "pve-ssl.pem", "pem": pemText, "fingerprint": otherFP})},
		},
		"malformed fingerprint with a good pem": {
			rows: []json.RawMessage{row(map[string]any{"filename": "pve-ssl.pem", "pem": pemText, "fingerprint": "AA:BB"})},
		},
		"a repeat is kept once": {
			rows: []json.RawMessage{
				row(map[string]any{"filename": "pve-ssl.pem", "fingerprint": otherFP}),
				row(map[string]any{"filename": "pveproxy-ssl.pem", "fingerprint": otherFP}),
			},
			want: []endpointpool.Pin{other},
		},
		"other files and bad rows are ignored": {
			rows: []json.RawMessage{
				row(map[string]any{"filename": "pve-root-ca.pem", "fingerprint": otherFP}),
				row(map[string]any{"filename": "pve-ssl.key", "fingerprint": otherFP}),
				json.RawMessage(`7`),
				row(map[string]any{"filename": 5, "fingerprint": otherFP}),
				row(map[string]any{"filename": "pve-ssl.pem", "pem": "not a pem"}),
				row(map[string]any{"filename": "pve-ssl.pem"}),
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			response := sdknodes.ListCertificatesInfoResponse(tc.rows)
			if got := nodeCertificatePins(&response); !slices.Equal(got, tc.want) {
				t.Fatalf("pins = %v, want %v", got, tc.want)
			}
		})
	}
	if got := nodeCertificatePins(nil); got != nil {
		t.Fatalf("a nil answer gave %v", got)
	}
}

func TestReadClusterRootDigest(t *testing.T) {
	t.Parallel()
	d := newDiscCluster(t)
	client := d.client()
	digest, err := ReadClusterRootDigest(lgCtx(), client, "pve2")
	if err != nil {
		t.Fatalf("ReadClusterRootDigest: %v", err)
	}
	if want := sha256.Sum256(d.rootDER); digest != want {
		t.Fatalf("digest = %x, want the full SHA-256 %x", digest, want)
	}
	stamp, err := ReadClusterStamp(lgCtx(), client, "pve2")
	if err != nil {
		t.Fatalf("ReadClusterStamp: %v", err)
	}
	if !strings.HasPrefix(fmt.Sprintf("%x", digest), stamp) || len(stamp) != clusterStampHexLen {
		t.Fatalf("stamp %q is not the first %d characters of the digest", stamp, clusterStampHexLen)
	}
	if !slices.Equal(d.certCalls[:2], []string{"pve2", "pve2"}) {
		t.Fatalf("both reads should ask the preferred node once each, got %v", d.certCalls)
	}

	d.certFailure["pve1"] = errors.New("reset")
	d.certFailure["pve2"] = errors.New("reset")
	d.certFailure["pve3"] = errors.New("reset")
	_, err = ReadClusterRootDigest(lgCtx(), d.client(), "pve2")
	if err == nil || !strings.Contains(err.Error(), "ReadClusterRootDigest") || strings.Contains(err.Error(), "ReadClusterStamp") {
		t.Fatalf("err = %v, want a failure that names ReadClusterRootDigest", err)
	}
	if _, err = ReadClusterRootDigest(lgCtx(), nil, "pve2"); err == nil {
		t.Fatal("a nil client must fail")
	}
}
