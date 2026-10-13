package pve

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// csNodes answers certificates/info per node and records every node asked.
type csNodes struct {
	sdknodes.Service
	answer func(node string) (*sdknodes.ListCertificatesInfoResponse, error)

	mu    sync.Mutex
	asked []string
}

func (n *csNodes) ListCertificatesInfo(_ context.Context, node string) (*sdknodes.ListCertificatesInfoResponse, error) {
	n.mu.Lock()
	n.asked = append(n.asked, node)
	n.mu.Unlock()
	return n.answer(node)
}

// csClient serves a cluster membership and a certificate service.
type csClient struct {
	Client
	cluster *lgCluster
	nodes   *csNodes
}

func (c *csClient) Cluster() sdkcluster.Service { return c.cluster }
func (c *csClient) Nodes() sdknodes.Service     { return c.nodes }

// rootAnswer is a certificates/info answer whose pve-root-ca.pem entry
// carries fields, next to the node's own certificate the way PVE lists it.
func rootAnswer(t *testing.T, fields map[string]any) *sdknodes.ListCertificatesInfoResponse {
	t.Helper()
	root := map[string]any{"filename": storageClusterRootFilename}
	for k, v := range fields {
		root[k] = v
	}
	return certificateResponse(
		certificateRow(t, map[string]any{"filename": "pve-ssl.pem"}),
		certificateRow(t, root),
	)
}

// stampOf is the stamp a DER certificate must produce.
func stampOf(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])[:clusterStampHexLen]
}

// intermediateRootFixture returns a PEM certificate that a separate parent CA
// issued, the shape an operator gets by installing a corporate intermediate
// as the cluster root.
func intermediateRootFixture(t *testing.T) (string, []byte) {
	t.Helper()
	parentPub, parentKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	notBefore := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter := time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
	parent := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Corporate root"},
		NotBefore: notBefore, NotAfter: notAfter, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign,
	}
	if _, err = x509.CreateCertificate(rand.Reader, parent, parent, parentPub, parentKey); err != nil {
		t.Fatal(err)
	}
	childPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	child := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "PVE cluster intermediate"},
		NotBefore: notBefore, NotAfter: notAfter, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, child, parent, childPub, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), der
}

func TestReadClusterStamp_PreferredNodeAnswersWithOneCall(t *testing.T) {
	t.Parallel()
	encoded, fingerprint, der := clusterIdentityRootFixture(t, 7, true)
	nodesSvc := &csNodes{answer: func(string) (*sdknodes.ListCertificatesInfoResponse, error) {
		return rootAnswer(t, map[string]any{"pem": encoded, "fingerprint": fingerprint}), nil
	}}
	c := &csClient{cluster: &lgCluster{nodes: []string{"pve1", "pve2"}}, nodes: nodesSvc}
	stamp, err := ReadClusterStamp(lgCtx(), c, "pve2")
	if err != nil {
		t.Fatalf("ReadClusterStamp: %v", err)
	}
	if want := stampOf(der); stamp != want {
		t.Fatalf("stamp = %q, want %q", stamp, want)
	}
	if len(nodesSvc.asked) != 1 || nodesSvc.asked[0] != "pve2" {
		t.Fatalf("asked %v, want only the preferred node", nodesSvc.asked)
	}
}

// A root that is not self-issued is the shape the strict continuity reader
// rejects. The stamp must still be read, because a stamp only needs a stable
// value that differs between clusters.
func TestReadClusterStamp_RootThatIsNotSelfIssued(t *testing.T) {
	t.Parallel()
	encoded, der := intermediateRootFixture(t)
	answer := rootAnswer(t, map[string]any{"pem": encoded})
	if _, err := storageClusterRootDigest(answer); err == nil {
		t.Fatal("precondition: the strict reader must reject a root that is not self-issued")
	}
	c := &csClient{
		cluster: &lgCluster{nodes: []string{"pve1"}},
		nodes: &csNodes{answer: func(string) (*sdknodes.ListCertificatesInfoResponse, error) {
			return answer, nil
		}},
	}
	stamp, err := ReadClusterStamp(lgCtx(), c, "pve1")
	if err != nil {
		t.Fatalf("ReadClusterStamp: %v", err)
	}
	if want := stampOf(der); stamp != want {
		t.Fatalf("stamp = %q, want %q", stamp, want)
	}
}

func TestReadClusterStamp_FingerprintOnlyAnswer(t *testing.T) {
	t.Parallel()
	_, fingerprint, der := clusterIdentityRootFixture(t, 9, true)
	c := &csClient{
		cluster: &lgCluster{nodes: []string{"pve1"}},
		nodes: &csNodes{answer: func(string) (*sdknodes.ListCertificatesInfoResponse, error) {
			return rootAnswer(t, map[string]any{"fingerprint": fingerprint}), nil
		}},
	}
	stamp, err := ReadClusterStamp(lgCtx(), c, "pve1")
	if err != nil {
		t.Fatalf("ReadClusterStamp: %v", err)
	}
	if want := stampOf(der); stamp != want {
		t.Fatalf("stamp = %q, want %q", stamp, want)
	}
}

// A PEM and a fingerprint that disagree, two root entries, and an entry with
// neither field are each an ambiguous answer, so no node yields a stamp and
// the read fails retriably.
func TestReadClusterStamp_RejectsAmbiguousAnswers(t *testing.T) {
	t.Parallel()
	encoded, _, _ := clusterIdentityRootFixture(t, 11, true)
	_, otherFingerprint, _ := clusterIdentityRootFixture(t, 12, true)
	cases := map[string]func(t *testing.T) *sdknodes.ListCertificatesInfoResponse{
		"pem and fingerprint disagree": func(t *testing.T) *sdknodes.ListCertificatesInfoResponse {
			return rootAnswer(t, map[string]any{"pem": encoded, "fingerprint": otherFingerprint})
		},
		"two root entries": func(t *testing.T) *sdknodes.ListCertificatesInfoResponse {
			row := certificateRow(t, map[string]any{"filename": storageClusterRootFilename, "pem": encoded})
			return certificateResponse(row, row)
		},
		"no pem and no fingerprint": func(t *testing.T) *sdknodes.ListCertificatesInfoResponse {
			return rootAnswer(t, map[string]any{})
		},
		"no root entry": func(t *testing.T) *sdknodes.ListCertificatesInfoResponse {
			return certificateResponse(certificateRow(t, map[string]any{"filename": "pve-ssl.pem"}))
		},
		"a chain instead of one certificate": func(t *testing.T) *sdknodes.ListCertificatesInfoResponse {
			return rootAnswer(t, map[string]any{"pem": encoded + encoded})
		},
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &csClient{
				cluster: &lgCluster{nodes: []string{"pve1"}},
				nodes: &csNodes{answer: func(string) (*sdknodes.ListCertificatesInfoResponse, error) {
					return answer(t), nil
				}},
			}
			stamp, err := ReadClusterStamp(lgCtx(), c, "pve1")
			if err == nil {
				t.Fatalf("stamp = %q, want an error", stamp)
			}
			if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("error is not retriable: %v", err)
			}
		})
	}
}

// When the preferred node fails, the read moves on to the other members in
// name order, skips a member a quorate cluster reports offline, and stops at
// the first success.
func TestReadClusterStamp_FallsBackToOnlineMembers(t *testing.T) {
	t.Parallel()
	encoded, _, der := clusterIdentityRootFixture(t, 13, true)
	nodesSvc := &csNodes{answer: func(node string) (*sdknodes.ListCertificatesInfoResponse, error) {
		switch node {
		case "pve2":
			return nil, errors.New("connection refused")
		case "pve3":
			return nil, errors.New("offline member must not be asked")
		default:
			return rootAnswer(t, map[string]any{"pem": encoded}), nil
		}
	}}
	c := &csClient{
		cluster: &lgCluster{
			nodes:    []string{"pve4", "pve3", "pve2", "pve1"},
			statusFn: lgStatus(map[string]bool{"pve1": true, "pve2": true, "pve3": false, "pve4": true}),
		},
		nodes: nodesSvc,
	}
	stamp, err := ReadClusterStamp(lgCtx(), c, "pve2")
	if err != nil {
		t.Fatalf("ReadClusterStamp: %v", err)
	}
	if want := stampOf(der); stamp != want {
		t.Fatalf("stamp = %q, want %q", stamp, want)
	}
	if got := strings.Join(nodesSvc.asked, ","); got != "pve2,pve1" {
		t.Fatalf("asked %s, want pve2 then pve1", got)
	}
}

// With no preferred node the members are read in name order.
func TestReadClusterStamp_NoPreferredNodeReadsMembers(t *testing.T) {
	t.Parallel()
	encoded, _, der := clusterIdentityRootFixture(t, 14, true)
	nodesSvc := &csNodes{answer: func(string) (*sdknodes.ListCertificatesInfoResponse, error) {
		return rootAnswer(t, map[string]any{"pem": encoded}), nil
	}}
	c := &csClient{cluster: &lgCluster{nodes: []string{"pve2", "pve1"}}, nodes: nodesSvc}
	stamp, err := ReadClusterStamp(lgCtx(), c, "")
	if err != nil {
		t.Fatalf("ReadClusterStamp: %v", err)
	}
	if stamp != stampOf(der) || strings.Join(nodesSvc.asked, ",") != "pve1" {
		t.Fatalf("stamp %q asked %v, want %q from pve1 alone", stamp, nodesSvc.asked, stampOf(der))
	}
}

// When every node fails, the error is retriable, names each node, and never
// carries the text of the API answer.
func TestReadClusterStamp_EveryNodeFailsRetriablyWithoutAnswerText(t *testing.T) {
	t.Parallel()
	const secret = "SECRET-ANSWER-TEXT"
	c := &csClient{
		cluster: &lgCluster{nodes: []string{"pve1", "pve2"}},
		nodes: &csNodes{answer: func(string) (*sdknodes.ListCertificatesInfoResponse, error) {
			return nil, errors.New(secret)
		}},
	}
	_, err := ReadClusterStamp(lgCtx(), c, "pve1")
	if err == nil {
		t.Fatal("want an error when every node fails")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("error is not retriable: %v", err)
	}
	msg := err.Error()
	if strings.Contains(msg, secret) {
		t.Fatalf("error carries the API answer text: %s", msg)
	}
	if !strings.Contains(msg, `"pve1"`) || !strings.Contains(msg, `"pve2"`) {
		t.Fatalf("error does not name both nodes: %s", msg)
	}
}

// A preferred node that fails while the membership cannot be enumerated is a
// retriable failure, never a guess.
func TestReadClusterStamp_MembershipFailureIsRetriable(t *testing.T) {
	t.Parallel()
	c := &csClient{
		cluster: &lgCluster{configNodesFn: func() (*sdkcluster.ListConfigNodesResponse, error) {
			return nil, errors.New("dial tcp: connection reset")
		}},
		nodes: &csNodes{answer: func(string) (*sdknodes.ListCertificatesInfoResponse, error) {
			return nil, errors.New("timeout")
		}},
	}
	_, err := ReadClusterStamp(lgCtx(), c, "pve1")
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want a retriable error", err)
	}
}

func TestReadClusterStamp_InvalidInputs(t *testing.T) {
	t.Parallel()
	//lint:ignore SA1012 a nil context is the input under test
	if _, err := ReadClusterStamp(nil, &csClient{}, "pve1"); err == nil { //nolint:staticcheck // SA1012: a nil context is the input under test
		t.Fatal("nil context: want an error")
	}
	if _, err := ReadClusterStamp(lgCtx(), nil, "pve1"); err == nil {
		t.Fatal("nil client: want an error")
	}
	ctx, cancel := context.WithCancel(lgCtx())
	cancel()
	nodesSvc := &csNodes{answer: func(string) (*sdknodes.ListCertificatesInfoResponse, error) {
		t.Fatal("a canceled read must not call PVE")
		return nil, nil
	}}
	_, err := ReadClusterStamp(ctx, &csClient{cluster: &lgCluster{nodes: []string{"pve1"}}, nodes: nodesSvc}, "pve1")
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("canceled context: err = %v, want a retriable error", err)
	}
}

// The strict continuity reader keeps every rule it had before the root parse
// was shared with the stamp reader.
func TestStorageClusterRootDigest_StillRejectsNonSelfIssuedRoot(t *testing.T) {
	t.Parallel()
	encoded, _ := intermediateRootFixture(t)
	_, err := storageClusterRootDigest(rootAnswer(t, map[string]any{"pem": encoded}))
	if err == nil || !strings.Contains(err.Error(), "self-issued") {
		t.Fatalf("err = %v, want the self-issued CA rejection", err)
	}
}
