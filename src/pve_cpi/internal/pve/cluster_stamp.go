package pve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// clusterStampHexLen is the number of leading hex characters of the root
// fingerprint that a stamped VM CID carries. It matches the stamp length the
// vmcid parser accepts.
const clusterStampHexLen = 8

// ReadClusterStamp returns the cluster stamp of the cluster c talks to, which
// is the first eight lowercase hex characters of the SHA-256 of the DER bytes of the
// pve-root-ca.pem entry that GET /nodes/{node}/certificates/info reports. The
// endpoint needs no privilege beyond an authenticated user.
//
// Every node of a cluster shares one root CA, so one successful read is the
// answer. preferred, when it is a valid node name, is tried first, which is
// the only call made in the common case. When it fails or is not given, the
// remaining cluster members are tried in name order, skipping members a
// quorate /cluster/status reports offline, and the first success wins.
//
// Unlike ObserveStorageClusterIdentity, the root does not have to be a
// self-issued CA, and the DER bytes are not required to parse as X.509. The
// stamp only has to be a stable value that differs between clusters, and an
// operator who installed an intermediate from a corporate CA as the cluster
// root must still get a stamp. A reported fingerprint must still agree with
// the PEM, and a malformed or ambiguous answer is still rejected.
//
// Every failure is retriable. The error names the nodes tried and the reason
// each one failed, and never carries the certificate answer or PEM data.
func ReadClusterStamp(ctx context.Context, c Client, preferred string) (string, error) {
	digest, err := readClusterRoot(ctx, c, preferred, "ReadClusterStamp", nil)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest[:])[:clusterStampHexLen], nil
}

// ReadClusterRootDigest returns the full SHA-256 of the DER bytes of the
// cluster's pve-root-ca.pem, the 32-byte value the endpoint cache stores as
// the cluster's identity and the journal spells in its pve-root-ca-sha256
// form. It is ReadClusterStamp without the truncation, and it tries nodes,
// accepts roots, and fails exactly as ReadClusterStamp does, except that the
// error names ReadClusterRootDigest.
func ReadClusterRootDigest(ctx context.Context, c Client, preferred string) ([sha256.Size]byte, error) {
	return readClusterRoot(ctx, c, preferred, "ReadClusterRootDigest", nil)
}

// readClusterRoot is the read both ReadClusterStamp and ReadClusterRootDigest
// make. op names the caller in its errors. answered, when not nil, receives
// the node whose certificates/info answer produced the root, along with that
// answer, so a caller that also wants the node's own certificates need not
// ask again.
func readClusterRoot(
	ctx context.Context, c Client, preferred, op string,
	answered func(node string, response *nodes.ListCertificatesInfoResponse),
) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	if ctx == nil {
		return zero, cpierrors.Retriable("%s: context is required", op)
	}
	if c == nil || c.Nodes() == nil {
		return zero, cpierrors.Retriable("%s: no PVE nodes service is available", op)
	}
	if err := ctx.Err(); err != nil {
		return zero, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, op+": read canceled")
	}
	bounded, cancel := context.WithTimeout(ctx, storageClusterIdentityTimeout)
	defer cancel()

	var failures []string
	tried := map[string]bool{}
	attempt := func(node string) ([sha256.Size]byte, bool) {
		tried[node] = true
		digest, response, err := readNodeClusterRoot(bounded, c.Nodes(), node)
		if err != nil {
			failures = append(failures, fmt.Sprintf("node %q: %s", node, err.Error()))
			return zero, false
		}
		if answered != nil {
			answered(node, response)
		}
		return digest, true
	}
	if validStorageIdentityNode(preferred) {
		if digest, ok := attempt(preferred); ok {
			return digest, nil
		}
	}
	members, err := ListClusterMemberNames(bounded, c)
	if err != nil {
		return zero, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			op+": "+stampFailureSummary(failures, "and cluster member enumeration failed"))
	}
	offline := offlineClusterNodes(bounded, c, log.FromContext(ctx))
	sorted := slices.Clone(members)
	slices.Sort(sorted)
	for _, node := range sorted {
		if tried[node] || offline[node] || !validStorageIdentityNode(node) {
			continue
		}
		if digest, ok := attempt(node); ok {
			return digest, nil
		}
		if bounded.Err() != nil {
			break
		}
	}
	if err := bounded.Err(); err != nil {
		return zero, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			op+": "+stampFailureSummary(failures, "and the read deadline passed"))
	}
	return zero, cpierrors.Retriable("%s: %s", op,
		stampFailureSummary(failures, "and no other online cluster member was left to try"))
}

// stampFailureSummary renders the per-node failures for a ReadClusterStamp
// error, followed by tail.
func stampFailureSummary(failures []string, tail string) string {
	if len(failures) == 0 {
		return "no node answered the cluster root read, " + tail
	}
	return "the cluster root read failed on " + strings.Join(failures, "; ") + ", " + tail
}

// readNodeClusterRoot reads one node's certificates/info answer and returns
// the root digest and the answer. The returned error text is one of the fixed
// reasons the root parser produces, or a fixed transport reason, so it never
// echoes the API answer.
func readNodeClusterRoot(
	ctx context.Context, service StorageClusterCertificateService, node string,
) ([sha256.Size]byte, *nodes.ListCertificatesInfoResponse, error) {
	var zero [sha256.Size]byte
	response, err := service.ListCertificatesInfo(ctx, node)
	if err != nil {
		return zero, nil, errors.New("certificate observation failed")
	}
	digest, err := clusterStampDigest(response)
	if err != nil {
		return zero, nil, err
	}
	return digest, response, nil
}

// clusterStampDigest returns the SHA-256 of the cluster root's DER bytes, or
// the reported fingerprint when the answer carries no PEM. It applies none of
// the CA rules storageClusterRootDigest enforces; see ReadClusterStamp.
func clusterStampDigest(response *nodes.ListCertificatesInfoResponse) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	block, fingerprint, hasFingerprint, err := storageClusterRootObservation(response)
	if err != nil {
		return zero, err
	}
	if block == nil {
		return fingerprint, nil
	}
	if len(block.Bytes) == 0 {
		return zero, errors.New("empty cluster root certificate")
	}
	digest := sha256.Sum256(block.Bytes)
	if hasFingerprint && digest != fingerprint {
		return zero, errors.New("cluster root PEM and SHA256 fingerprint disagree")
	}
	return digest, nil
}

// CountGuestsTagged counts the guests in one GET /cluster/resources?type=vm
// answer that carry tag, compared without regard to case. It is a hint for an
// error message, so the caller treats a failure as "no hint" rather than as
// an answer, and a row it cannot decode is skipped.
func CountGuestsTagged(ctx context.Context, c Client, tag string) (int, error) {
	if c == nil || c.Cluster() == nil {
		return 0, errors.New("no PVE cluster service is available")
	}
	typ := "vm"
	var resp *sdkcluster.ListResourcesResponse
	if err := RetryOnTransient(ctx, nil, "count_guests_tagged", 0, func() error {
		var inner error
		resp, inner = c.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &typ})
		return inner
	}); err != nil {
		return 0, WrapError(err)
	}
	if resp == nil {
		return 0, nil
	}
	count := 0
	for _, raw := range *resp {
		var row struct {
			Tags string `json:"tags"`
		}
		if json.Unmarshal(raw, &row) != nil {
			continue
		}
		for _, t := range splitTagString(row.Tags) {
			if strings.EqualFold(t, tag) {
				count++
				break
			}
		}
	}
	return count, nil
}
