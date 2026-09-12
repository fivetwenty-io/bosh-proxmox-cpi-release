package pve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

const (
	storageClusterRootFilename    = "pve-root-ca.pem"
	storageClusterIdentityPrefix  = "pve-root-ca-sha256:"
	storageClusterIdentityWorkers = 4
	storageClusterIdentityTimeout = 30 * time.Second
)

// ErrStorageClusterIdentity marks observations that cannot establish consistent
// cluster continuity. Allocation callers must stop and preserve journal evidence.
var ErrStorageClusterIdentity = errors.New("storage cluster identity cannot be verified")

// StorageClusterCertificateService is the read-only subset of nodes.Service used
// to verify the shared public cluster root. It is independent of endpoint URLs.
type StorageClusterCertificateService interface {
	ListCertificatesInfo(context.Context, string) (*nodes.ListCertificatesInfoResponse, error)
}

// StorageClusterIdentity contains only immutable public identity values. It is
// continuity evidence, not authorization or proof that a copied CA represents an
// independently administered cluster. TLS trust remains the API client's job.
type StorageClusterIdentity struct {
	digest            [sha256.Size]byte
	observedNodeCount int
}

// ID is the stable journal ClusterID. Certificate rotation changes this value;
// journal enrollment must be explicitly audited, never silently rebound.
func (i StorageClusterIdentity) ID() string {
	if i.observedNodeCount == 0 {
		return ""
	}
	return storageClusterIdentityPrefix + i.FingerprintSHA256()
}

// FingerprintSHA256 returns the lowercase SHA256 of the X.509 DER certificate.
func (i StorageClusterIdentity) FingerprintSHA256() string {
	if i.observedNodeCount == 0 {
		return ""
	}
	return hex.EncodeToString(i.digest[:])
}

// ObservedNodeCount reports the number of nodes corroborating this identity.
// ObservedNodeCount reports the nodes corroborating this identity.
func (i StorageClusterIdentity) ObservedNodeCount() int { return i.observedNodeCount }

// storageClusterIdentityError deliberately excludes API response/error text and
// PEM data from Error(). Unwrap retains errors.Is/As for transport/cancellation.
type storageClusterIdentityError struct {
	node, reason string
	cause        error
}

func (e *storageClusterIdentityError) Error() string {
	if e.node == "" {
		return "storage cluster identity: " + e.reason
	}
	return fmt.Sprintf("storage cluster identity: node %q: %s", e.node, e.reason)
}
func (e *storageClusterIdentityError) Unwrap() error { return e.cause }
func (e *storageClusterIdentityError) Is(target error) bool {
	return target == ErrStorageClusterIdentity
}
func clusterIdentityError(node, reason string, cause error) error {
	return &storageClusterIdentityError{node: node, reason: reason, cause: cause}
}

// ObserveStorageClusterIdentity requires one unambiguous pve-root-ca.pem identity
// from every supplied relevant node. Duplicate names are observed once. It uses
// at most four API calls concurrently and the earlier of the caller's deadline
// or 30 seconds. It has no mutations, global cache, or automatic enrollment.
//
// The official PVE API includes /etc/pve/pve-root-ca.pem and returns its basename:
// https://github.com/proxmox/pve-manager/blob/master/PVE/API2/Certificates.pm
// PVE::Certificate's pve-certificate-info schema makes PEM/fingerprint optional:
// https://github.com/proxmox/pve-common/blob/master/src/PVE/Certificate.pm
// A valid PEM is preferred and any reported SHA256 must agree. Fingerprint-only
// observations remain supported because the stable API schema does not require
// PEM. A supplied malformed field is never treated as omission.
func ObserveStorageClusterIdentity(ctx context.Context, service StorageClusterCertificateService, relevantNodes []string) (StorageClusterIdentity, error) {
	if ctx == nil {
		return StorageClusterIdentity{}, clusterIdentityError("", "context is required", nil)
	}
	if err := ctx.Err(); err != nil {
		return StorageClusterIdentity{}, clusterIdentityError("", "observation canceled", err)
	}
	if service == nil {
		return StorageClusterIdentity{}, clusterIdentityError("", "certificate service is required", nil)
	}
	names := make([]string, 0, len(relevantNodes))
	seen := make(map[string]bool, len(relevantNodes))
	for _, name := range relevantNodes {
		if !validStorageIdentityNode(name) {
			return StorageClusterIdentity{}, clusterIdentityError("", "invalid relevant node name", nil)
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return StorageClusterIdentity{}, clusterIdentityError("", "at least one relevant node is required", nil)
	}
	sort.Strings(names)
	bounded, cancel := context.WithTimeout(ctx, storageClusterIdentityTimeout)
	defer cancel()
	digests := make([][sha256.Size]byte, len(names))
	failures := make([]error, len(names))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(storageClusterIdentityWorkers, len(names)) {
		workers.Go(func() {
			for index := range jobs {
				if err := bounded.Err(); err != nil {
					failures[index] = clusterIdentityError(names[index], "observation canceled", err)
					continue
				}
				response, err := service.ListCertificatesInfo(bounded, names[index])
				if err != nil {
					failures[index] = clusterIdentityError(names[index], "certificate observation failed", err)
					continue
				}
				digests[index], err = storageClusterRootDigest(response)
				if err != nil {
					failures[index] = clusterIdentityError(names[index], "certificate observation rejected: "+err.Error(), err)
				}
			}
		})
	}
dispatch:
	for index := range names {
		select {
		case jobs <- index:
		case <-bounded.Done():
			break dispatch
		}
	}
	close(jobs)
	workers.Wait()
	if err := bounded.Err(); err != nil {
		return StorageClusterIdentity{}, clusterIdentityError("", "observation deadline or cancellation", err)
	}
	if err := errors.Join(failures...); err != nil {
		return StorageClusterIdentity{}, err
	}
	for index := 1; index < len(digests); index++ {
		if digests[index] != digests[0] {
			return StorageClusterIdentity{}, clusterIdentityError(names[index], "cluster roots disagree; audited enrollment is required", nil)
		}
	}
	return StorageClusterIdentity{digest: digests[0], observedNodeCount: len(names)}, nil
}

// Node names follow PVE JSONSchema's pve-node grammar, avoiding invalid API paths
// and unsanitized names in diagnostics. Names are not used as cluster identity.
func validStorageIdentityNode(name string) bool {
	if len(name) == 0 {
		return false
	}
	alphaNumeric := func(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' }
	if !alphaNumeric(name[0]) || !alphaNumeric(name[len(name)-1]) {
		return false
	}
	for i := range len(name) {
		if !alphaNumeric(name[i]) && name[i] != '-' {
			return false
		}
	}
	return true
}

func storageClusterRootDigest(response *nodes.ListCertificatesInfoResponse) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	if response == nil {
		return zero, errors.New("nil certificate response")
	}
	var root map[string]json.RawMessage
	for _, raw := range *response {
		fields, err := storageCertificateFields(raw)
		if err != nil {
			return zero, err
		}
		filename, present, err := storageCertificateString(fields, "filename")
		if err != nil || !present || filename == "" {
			return zero, errors.New("missing or malformed certificate filename")
		}
		if filename != storageClusterRootFilename {
			continue
		}
		if root != nil {
			return zero, errors.New("multiple cluster root certificates")
		}
		root = fields
	}
	if root == nil {
		return zero, errors.New("cluster root certificate missing")
	}
	encoded, hasPEM, err := storageCertificateString(root, "pem")
	if err != nil {
		return zero, err
	}
	reported, hasFingerprint, err := storageCertificateString(root, "fingerprint")
	if err != nil {
		return zero, err
	}
	var fingerprint [sha256.Size]byte
	if hasFingerprint {
		fingerprint, err = storageRootFingerprint(reported)
		if err != nil {
			return zero, err
		}
	}
	if !hasPEM {
		if !hasFingerprint {
			return zero, errors.New("cluster root has no PEM or SHA256 fingerprint")
		}
		return fingerprint, nil
	}
	// A single PEM CERTIFICATE, not a chain, private key, or arbitrary text.
	trimmed := bytes.TrimSpace([]byte(encoded))
	if !bytes.HasPrefix(trimmed, []byte("-----BEGIN CERTIFICATE-----")) {
		return zero, errors.New("invalid cluster root PEM")
	}
	block, rest := pem.Decode(trimmed)
	if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return zero, errors.New("invalid or multiple cluster root PEM blocks")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return zero, errors.New("invalid cluster root X.509 certificate")
	}
	if !cert.IsCA || !cert.BasicConstraintsValid || !bytes.Equal(cert.RawIssuer, cert.RawSubject) {
		return zero, errors.New("cluster root is not a self-issued CA")
	}
	if err = cert.CheckSignatureFrom(cert); err != nil {
		return zero, errors.New("cluster root signature is invalid")
	}
	digest := sha256.Sum256(cert.Raw)
	if hasFingerprint && digest != fingerprint {
		return zero, errors.New("cluster root PEM and SHA256 fingerprint disagree")
	}
	return digest, nil
}

// Reject duplicate JSON keys, including duplicate filename/fingerprint, instead
// of choosing whichever ambiguous observation happened to appear last.
func storageCertificateFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("certificate entry is not an object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, errors.New("malformed certificate object")
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid certificate property")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate certificate property")
		}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return nil, errors.New("malformed certificate property")
		}
		fields[key] = value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("malformed certificate object")
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, errors.New("trailing certificate data")
	}
	return fields, nil
}
func storageCertificateString(fields map[string]json.RawMessage, key string) (string, bool, error) {
	raw, present := fields[key]
	if !present {
		return "", false, nil
	}
	var value string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return "", true, errors.New("malformed certificate identity field")
	}
	return value, true, nil
}
func storageRootFingerprint(value string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	// PVE JSONSchema fingerprint-sha256 is exactly 32 colon-separated hex bytes.
	if len(value) != sha256.Size*3-1 {
		return digest, errors.New("invalid SHA256 fingerprint length")
	}
	for index := 2; index < len(value); index += 3 {
		if value[index] != ':' {
			return digest, errors.New("invalid SHA256 fingerprint separators")
		}
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(value, ":", ""))
	if err != nil || len(decoded) != sha256.Size {
		return digest, errors.New("invalid SHA256 fingerprint hexadecimal bytes")
	}
	copy(digest[:], decoded)
	return digest, nil
}
