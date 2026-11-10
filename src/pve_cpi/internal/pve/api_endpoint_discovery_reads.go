package pve

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

const (
	// discoveryReadTimeout bounds the whole read, the same bound the cluster
	// identity reads use.
	discoveryReadTimeout = storageClusterIdentityTimeout
	// discoveryCertWorkers is the number of certificates/info reads that run
	// at once.
	discoveryCertWorkers = 4
	// discoveryCertNodeLimit is the most nodes whose certificates/info is
	// read. The result holds at most maxDiscoveredNodes nodes, so a longer
	// /nodes list is no reason to make an unbounded number of calls. A node
	// past the limit, in name order, is pinned by its /nodes fingerprint alone.
	discoveryCertNodeLimit = 2 * maxDiscoveredNodes

	nodeCertFilename      = "pve-ssl.pem"
	proxyCertFilename     = "pveproxy-ssl.pem"
	clusterStatusRowType  = "cluster"
	nodeStatusOnlineValue = "online"
)

// Read failures. Each one means discovery writes no result. None of them
// echoes a node name, an API answer, or certificate data.
var (
	// ErrDiscoveryNodes means GET /nodes failed or listed no nodes, so there
	// is nothing to pin and nothing to trust.
	ErrDiscoveryNodes = errors.New("discovery: GET /nodes gave no usable node list")
	// ErrDiscoveryNodeName means a node name broke the pve-node format. A
	// forged name such as api.attacker.example would otherwise become an
	// address, so the whole result is rejected.
	ErrDiscoveryNodeName = errors.New("discovery: a node name is not a valid pve-node label")
	// ErrDiscoveryRoot means no node answered the cluster root read, so the
	// result has no identity to record.
	ErrDiscoveryRoot = errors.New("discovery: the cluster root CA could not be read")
)

// DiscoveryNodeRead is what the reads learned about one node.
type DiscoveryNodeRead struct {
	// Name is the node name exactly as /nodes reported it, or empty when the
	// row carried no readable name.
	Name string
	// Status is the /nodes status field, such as online or offline.
	Status string
	// Fingerprint is the /nodes ssl_fingerprint exactly as reported, or empty
	// when the row carried none.
	Fingerprint string
	// CertPins holds the fingerprints of the node's pve-ssl.pem and
	// pveproxy-ssl.pem from its certificates/info. It is empty when that read
	// failed or was not made.
	CertPins []endpointpool.Pin
}

// DiscoveryStatus is the part of GET /cluster/status that discovery uses.
type DiscoveryStatus struct {
	// Available is false when the read failed, as it does for a token without
	// Sys.Audit on /. Every other field is then zero.
	Available bool
	// InCluster is true when the answer holds a cluster row. A standalone
	// host answers with its own node row only.
	InCluster bool
	// Quorate is the cluster row's quorate flag.
	Quorate bool
	// Local is the name of the node that answered, or empty when no row is
	// marked local.
	Local string
	// Online holds each node row's online flag, by node name.
	Online map[string]bool
}

// DiscoveryReads is everything one discovery run read from the cluster.
type DiscoveryReads struct {
	// Nodes holds the /nodes rows in the order the API gave them.
	Nodes []DiscoveryNodeRead
	// Status is the /cluster/status answer.
	Status DiscoveryStatus
	// RootCA is the SHA-256 of the cluster root CA's DER bytes.
	RootCA [sha256.Size]byte
}

// ReadDiscovery reads what discovery needs through c: the node list with each
// node's ssl_fingerprint, the cluster status, the cluster root CA, and each
// node's own certificate fingerprints. The caller sends c's requests through a
// configured endpoint, and this function adds no routing of its own.
//
// A failure of GET /nodes, an empty list, a node name that breaks the
// pve-node format, or a root CA no node can answer returns an error from this
// file's Err values, because without them nothing can be trusted and
// discovery writes nothing. A failure of GET /cluster/status only clears
// Status.Available, and a failure of one node's certificates/info only leaves
// that node pinned by its /nodes fingerprint. Cancellation of ctx returns its
// error.
func ReadDiscovery(ctx context.Context, c Client) (DiscoveryReads, error) {
	if ctx == nil {
		return DiscoveryReads{}, cpierrors.Cloud("ReadDiscovery: ctx must not be nil")
	}
	if c == nil || c.Nodes() == nil || c.Cluster() == nil {
		return DiscoveryReads{}, cpierrors.Cloud("ReadDiscovery: the PVE nodes and cluster services are required")
	}
	if err := ctx.Err(); err != nil {
		return DiscoveryReads{}, fmt.Errorf("ReadDiscovery: read canceled: %w", err)
	}
	bounded, cancel := context.WithTimeout(ctx, discoveryReadTimeout)
	defer cancel()

	rows, err := readDiscoveryNodes(bounded, c)
	if err != nil {
		return DiscoveryReads{}, err
	}
	for _, row := range rows {
		if errors.Is(endpointcache.ValidateNodeName(row.Name), endpointcache.ErrNodeNameFormat) {
			return DiscoveryReads{}, ErrDiscoveryNodeName
		}
	}
	reads := DiscoveryReads{Nodes: rows}
	reads.Status = readDiscoveryStatus(bounded, c)
	if err = bounded.Err(); err != nil {
		return DiscoveryReads{}, fmt.Errorf("ReadDiscovery: read ended: %w", err)
	}
	reads.RootCA, err = ReadClusterRootDigest(bounded, c, reads.Status.Local)
	if err != nil {
		return DiscoveryReads{}, fmt.Errorf("%w: %w", ErrDiscoveryRoot, err)
	}
	readDiscoveryCertPins(bounded, c.Nodes(), reads.Nodes)
	if err = bounded.Err(); err != nil {
		return DiscoveryReads{}, fmt.Errorf("ReadDiscovery: read ended: %w", err)
	}
	return reads, nil
}

// readDiscoveryNodes reads GET /nodes through ListNodes and decodes the three
// fields discovery uses. A row it cannot decode comes back with an empty name,
// which the caller treats as a name that breaks the format.
func readDiscoveryNodes(ctx context.Context, c Client) ([]DiscoveryNodeRead, error) {
	var raws []json.RawMessage
	if err := RetryOnTransient(ctx, nil, "discovery_list_nodes", 0, func() error {
		resp, inner := c.Nodes().ListNodes(ctx)
		if inner != nil {
			return inner
		}
		raws = nil
		if resp != nil {
			raws = *resp
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDiscoveryNodes, WrapError(err))
	}
	if len(raws) == 0 {
		return nil, ErrDiscoveryNodes
	}
	rows := make([]DiscoveryNodeRead, 0, len(raws))
	for _, raw := range raws {
		rows = append(rows, decodeDiscoveryNodeRow(raw))
	}
	return rows, nil
}

// decodeDiscoveryNodeRow decodes one /nodes row. A field of the wrong type
// reads as absent, and a row that is not an object reads as an unnamed node.
func decodeDiscoveryNodeRow(raw json.RawMessage) DiscoveryNodeRead {
	var item struct {
		Node        json.RawMessage `json:"node"`
		Status      json.RawMessage `json:"status"`
		Fingerprint json.RawMessage `json:"ssl_fingerprint"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return DiscoveryNodeRead{}
	}
	return DiscoveryNodeRead{
		Name:        rawString(item.Node),
		Status:      rawString(item.Status),
		Fingerprint: rawString(item.Fingerprint),
	}
}

// rawString returns the JSON string in raw, or empty when raw is absent, null,
// or not a string.
func rawString(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

// discoveryStatusRow is the shape of one GET /cluster/status row that
// discovery uses. The cluster row carries quorate, and the node rows carry
// name, online, and local.
type discoveryStatusRow struct {
	Type    string            `json:"type"`
	Name    string            `json:"name"`
	Quorate sdkclient.PVEBool `json:"quorate"`
	Online  sdkclient.PVEBool `json:"online"`
	Local   sdkclient.PVEBool `json:"local"`
}

// readDiscoveryStatus reads GET /cluster/status. Any failure, a 403 for a
// token without Sys.Audit included, returns a status that is not Available,
// because discovery goes on without quorum ordering. A row it cannot decode is
// skipped.
func readDiscoveryStatus(ctx context.Context, c Client) DiscoveryStatus {
	var resp *sdkcluster.ListStatusResponse
	err := RetryOnTransient(ctx, nil, "discovery_cluster_status", 0, func() error {
		var inner error
		resp, inner = c.Cluster().ListStatus(ctx)
		return inner
	})
	if err != nil || resp == nil {
		return DiscoveryStatus{}
	}
	status := DiscoveryStatus{Available: true, Online: map[string]bool{}}
	for _, raw := range *resp {
		var row discoveryStatusRow
		if json.Unmarshal(raw, &row) != nil {
			continue
		}
		switch row.Type {
		case clusterStatusRowType:
			status.InCluster = true
			status.Quorate = row.Quorate.Bool()
		case resourceTypeNode:
			if row.Name == "" {
				continue
			}
			status.Online[row.Name] = row.Online.Bool()
			if row.Local.Bool() {
				status.Local = row.Name
			}
		}
	}
	return status
}

// readDiscoveryCertPins reads certificates/info for each node whose name the
// helper accepts, in name order and up to discoveryCertNodeLimit nodes, and
// stores the node's own certificate fingerprints in its row. A node whose read
// fails keeps no CertPins. The caller checks ctx for cancellation afterward.
func readDiscoveryCertPins(ctx context.Context, service StorageClusterCertificateService, rows []DiscoveryNodeRead) {
	var indexes []int
	for i, row := range rows {
		if endpointcache.ValidNodeName(row.Name) {
			indexes = append(indexes, i)
		}
	}
	slices.SortFunc(indexes, func(a, b int) int { return strings.Compare(rows[a].Name, rows[b].Name) })
	if len(indexes) > discoveryCertNodeLimit {
		indexes = indexes[:discoveryCertNodeLimit]
	}
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(discoveryCertWorkers, len(indexes)) {
		workers.Go(func() {
			for i := range jobs {
				if ctx.Err() != nil {
					continue
				}
				response, err := service.ListCertificatesInfo(ctx, rows[i].Name)
				if err != nil {
					continue
				}
				rows[i].CertPins = nodeCertificatePins(response)
			}
		})
	}
	for _, i := range indexes {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
}

// nodeCertificatePins returns the pins of a node's pve-ssl.pem and
// pveproxy-ssl.pem entries in the order the answer lists them, without
// repeats. An entry's pin is its reported fingerprint, or the SHA-256 of the
// first certificate in its PEM when it reports none. An entry whose
// fingerprint is malformed, or whose PEM and fingerprint disagree, adds no
// pin, because a pin is trusted only when its source is unambiguous.
func nodeCertificatePins(response *nodes.ListCertificatesInfoResponse) []endpointpool.Pin {
	if response == nil {
		return nil
	}
	var pins []endpointpool.Pin
	for _, raw := range *response {
		var row struct {
			Filename    json.RawMessage `json:"filename"`
			Fingerprint json.RawMessage `json:"fingerprint"`
			PEM         json.RawMessage `json:"pem"`
		}
		if json.Unmarshal(raw, &row) != nil {
			continue
		}
		if name := rawString(row.Filename); name != nodeCertFilename && name != proxyCertFilename {
			continue
		}
		pin, ok := certificateEntryPin(rawString(row.Fingerprint), rawString(row.PEM))
		if ok && !slices.Contains(pins, pin) {
			pins = append(pins, pin)
		}
	}
	return pins
}

// certificateEntryPin derives the pin of one certificate entry from its
// reported fingerprint and PEM, either of which may be empty.
func certificateEntryPin(fingerprint, pemText string) (endpointpool.Pin, bool) {
	var reported, derived endpointpool.Pin
	var hasReported, hasDerived bool
	if fingerprint != "" {
		pin, err := endpointpool.ParsePin(fingerprint)
		if err != nil {
			return endpointpool.Pin{}, false
		}
		reported, hasReported = pin, true
	}
	if pemText != "" {
		if block, _ := pem.Decode([]byte(pemText)); block != nil && block.Type == "CERTIFICATE" && len(block.Bytes) > 0 {
			derived, hasDerived = endpointpool.PinOf(block.Bytes), true
		}
	}
	switch {
	case hasReported && hasDerived && reported != derived:
		return endpointpool.Pin{}, false
	case hasReported:
		return reported, true
	default:
		return derived, hasDerived
	}
}
