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
	"time"

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
	// discoveryCertTimeout bounds one certificates/info read. A read also ends
	// with the overall deadline, so it gets the smaller of the two.
	discoveryCertTimeout = 5 * time.Second
	// discoveryCertNodeLimit is the most nodes whose certificates/info is
	// read, which is the most the result can hold. A node past the limit, in
	// the order the builder prefers, is pinned by its /nodes fingerprint alone.
	discoveryCertNodeLimit = maxDiscoveredNodes

	nodeCertFilename      = "pve-ssl.pem"
	proxyCertFilename     = "pveproxy-ssl.pem"
	clusterStatusRowType  = "cluster"
	nodeStatusOnlineValue = "online"
)

// Read failures. Each one means discovery writes no result. Their text is a
// fixed reason, with the HTTP status code when the server gave one, and never
// echoes an API answer, a node name, or certificate data.
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

// discoveryReadError reports a read failure as a fixed reason. Error returns
// base's text and the HTTP status code, never the cause's text, so a server
// message cannot reach a log or a payload. Unwrap keeps base and the cause
// for errors.Is and errors.As.
type discoveryReadError struct {
	base  error
	code  int
	cause error
}

func (e *discoveryReadError) Error() string {
	if e.code > 0 {
		return fmt.Sprintf("%s (HTTP %d)", e.base.Error(), e.code)
	}
	return e.base.Error()
}

func (e *discoveryReadError) Unwrap() []error { return []error{e.base, e.cause} }

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
// Status.Available. A certificates/info read that fails, or that outlasts its
// own timeout or the overall deadline, only leaves that node pinned by its
// /nodes fingerprint, and the result stands. Cancellation of ctx returns its
// error.
func ReadDiscovery(ctx context.Context, c Client) (DiscoveryReads, error) {
	return readDiscovery(ctx, c, discoveryReadTimeout, discoveryCertTimeout)
}

// readDiscovery is ReadDiscovery with its two time bounds as arguments, so a
// test can use short ones.
func readDiscovery(ctx context.Context, c Client, overall, certTimeout time.Duration) (DiscoveryReads, error) {
	if ctx == nil {
		return DiscoveryReads{}, cpierrors.Cloud("ReadDiscovery: ctx must not be nil")
	}
	if c == nil || c.Nodes() == nil || c.Cluster() == nil {
		return DiscoveryReads{}, cpierrors.Cloud("ReadDiscovery: the PVE nodes and cluster services are required")
	}
	if err := ctx.Err(); err != nil {
		return DiscoveryReads{}, fmt.Errorf("ReadDiscovery: read canceled: %w", err)
	}
	bounded, cancel := context.WithTimeout(ctx, overall)
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
	// The node that answered is the best first try for the root. Without a
	// status nothing marks it, so the first node /nodes lists as online stands
	// in, which keeps an offline member from taking the first try.
	preferred := reads.Status.Local
	if !reads.Status.Available {
		preferred = firstOnlineNode(rows)
	}
	var rootNode string
	var rootAnswer *nodes.ListCertificatesInfoResponse
	reads.RootCA, err = readClusterRoot(bounded, c, preferred, "ReadClusterRootDigest",
		func(node string, response *nodes.ListCertificatesInfoResponse) { rootNode, rootAnswer = node, response })
	if err != nil {
		return DiscoveryReads{}, &discoveryReadError{base: ErrDiscoveryRoot, cause: err}
	}
	// The root read already holds that node's certificates, so it is not asked
	// again.
	if rootAnswer != nil {
		for i := range rows {
			if strings.EqualFold(rows[i].Name, rootNode) {
				rows[i].CertPins = nodeCertificatePins(rootAnswer)
				break
			}
		}
	}
	readDiscoveryCertPins(bounded, c.Nodes(), reads, rootNode, certTimeout)
	// Only the caller's own context ends the run here. The overall deadline
	// cutting the reads leaves the nodes it cut with their /nodes pin.
	if err = ctx.Err(); err != nil {
		return DiscoveryReads{}, fmt.Errorf("ReadDiscovery: read ended: %w", err)
	}
	return reads, nil
}

// firstOnlineNode returns the name of the first row whose /nodes status is
// online and whose name is a valid node name, or empty when there is none.
func firstOnlineNode(rows []DiscoveryNodeRead) string {
	for _, row := range rows {
		if row.Status == nodeStatusOnlineValue && endpointcache.ValidNodeName(row.Name) {
			return row.Name
		}
	}
	return ""
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
		code, _ := apiHTTPCode(err)
		return nil, &discoveryReadError{base: ErrDiscoveryNodes, code: code, cause: WrapError(err)}
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

// certReadOrder returns the indexes of the rows whose certificates/info can
// matter, in the order the builder prefers them and at most
// discoveryCertNodeLimit of them. It leaves out what the builder would drop
// before it ever looked at a pin, which is a name the helper rejects, a repeat
// of an earlier name, the seed's own node, and a row without a parseable
// fingerprint.
func certReadOrder(reads DiscoveryReads) []int {
	seen := map[string]bool{}
	var indexes []int
	for i, row := range reads.Nodes {
		key := strings.ToLower(row.Name)
		if !endpointcache.ValidNodeName(row.Name) || seen[key] {
			continue
		}
		seen[key] = true
		if isSeedNode(reads.Status, len(reads.Nodes), row.Name) {
			continue
		}
		if _, err := endpointpool.ParsePin(row.Fingerprint); err != nil {
			continue
		}
		indexes = append(indexes, i)
	}
	slices.SortStableFunc(indexes, func(a, b int) int {
		return compareNodePreference(
			nodeOnline(reads.Status, reads.Nodes[a]), nodeOnline(reads.Status, reads.Nodes[b]),
			reads.Nodes[a].Name, reads.Nodes[b].Name)
	})
	return indexes[:min(len(indexes), discoveryCertNodeLimit)]
}

// readDiscoveryCertPins reads certificates/info for the nodes certReadOrder
// picks, except the node named answered, whose answer the root read already
// holds, and stores each node's own certificate fingerprints in its row. At
// most discoveryCertWorkers reads run at once, and each gets the smaller of
// certTimeout and the time left on ctx. A node whose read fails or times out,
// or that ctx ends before anyone reads, keeps no CertPins. The caller checks
// its own context for cancellation afterward.
func readDiscoveryCertPins(
	ctx context.Context, service StorageClusterCertificateService, reads DiscoveryReads, answered string,
	certTimeout time.Duration,
) {
	var indexes []int
	for _, i := range certReadOrder(reads) {
		if answered == "" || !strings.EqualFold(reads.Nodes[i].Name, answered) {
			indexes = append(indexes, i)
		}
	}
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(discoveryCertWorkers, len(indexes)) {
		workers.Go(func() {
			for i := range jobs {
				if ctx.Err() != nil {
					continue
				}
				readCtx, cancel := context.WithTimeout(ctx, certTimeout)
				response, err := service.ListCertificatesInfo(readCtx, reads.Nodes[i].Name)
				cancel()
				if err != nil {
					continue
				}
				reads.Nodes[i].CertPins = nodeCertificatePins(response)
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
		block, _ := pem.Decode([]byte(pemText))
		if block != nil && block.Type == "CERTIFICATE" && len(block.Bytes) > 0 {
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
