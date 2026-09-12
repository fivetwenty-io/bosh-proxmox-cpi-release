package pve

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

type clusterIdentityCertificateFunc func(context.Context, string) (*nodes.ListCertificatesInfoResponse, error)

func (f clusterIdentityCertificateFunc) ListCertificatesInfo(ctx context.Context, node string) (*nodes.ListCertificatesInfoResponse, error) {
	return f(ctx, node)
}

func clusterIdentityRootFixture(t *testing.T, serial int64, isCA bool) (string, string, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "Public test cluster root"}, NotBefore: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC), IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	encoded := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, sha256.Size)
	for i := range parts {
		parts[i] = encoded[2*i : 2*i+2]
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), strings.Join(parts, ":"), der
}
func certificateRow(t *testing.T, fields map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func certificateResponse(rows ...json.RawMessage) *nodes.ListCertificatesInfoResponse {
	r := nodes.ListCertificatesInfoResponse(rows)
	return &r
}
func constantCertificateService(r *nodes.ListCertificatesInfoResponse) StorageClusterCertificateService {
	return clusterIdentityCertificateFunc(func(context.Context, string) (*nodes.ListCertificatesInfoResponse, error) { return r, nil })
}

func TestStorageClusterIdentityEndpointAliasesAndNodeRenewal(t *testing.T) {
	root, fingerprint, der := clusterIdentityRootFixture(t, 1, true)
	rootRow := certificateRow(t, map[string]any{"filename": "pve-root-ca.pem", "pem": root, "fingerprint": fingerprint})
	// Two independently constructed API services represent different endpoint aliases
	// and different node proxy certificates. Only the shared root is an identity.
	aliasA := constantCertificateService(certificateResponse(rootRow, certificateRow(t, map[string]any{"filename": "pveproxy-ssl.pem", "fingerprint": "old-node-fingerprint"})))
	aliasB := constantCertificateService(certificateResponse(certificateRow(t, map[string]any{"filename": "pve-ssl.pem", "fingerprint": "renewed-node-fingerprint"}), rootRow))
	names := []string{"node-b", "node-a", "node-b"}
	original := append([]string(nil), names...)
	a, err := ObserveStorageClusterIdentity(context.Background(), aliasA, names)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ObserveStorageClusterIdentity(context.Background(), aliasB, []string{"new-node"})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	want := hex.EncodeToString(sum[:])
	if a.FingerprintSHA256() != want || a.ID() != "pve-root-ca-sha256:"+want || b.ID() != a.ID() {
		t.Fatal("endpoint/node certificate changed root identity")
	}
	if a.ObservedNodeCount() != 2 || b.ObservedNodeCount() != 1 {
		t.Fatal("incorrect distinct-node observations")
	}
	if !reflect.DeepEqual(names, original) {
		t.Fatal("mutated caller node list")
	}
	if (StorageClusterIdentity{}).ID() != "" || (StorageClusterIdentity{}).FingerprintSHA256() != "" {
		t.Fatal("zero result claimed identity")
	}
}

func TestStorageClusterIdentityPEMOptionalAndFingerprintAgreement(t *testing.T) {
	root, fingerprint, _ := clusterIdentityRootFixture(t, 2, true)
	service := clusterIdentityCertificateFunc(func(_ context.Context, node string) (*nodes.ListCertificatesInfoResponse, error) {
		fields := map[string]any{"filename": "pve-root-ca.pem"}
		switch node {
		case "pem-only":
			fields["pem"] = root
		case "fingerprint-only":
			fields["fingerprint"] = strings.ToLower(fingerprint)
		default:
			fields["pem"] = root
			fields["fingerprint"] = fingerprint
		}
		return certificateResponse(certificateRow(t, fields)), nil
	})
	result, err := ObserveStorageClusterIdentity(context.Background(), service, []string{"pem-only", "fingerprint-only", "both"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ObservedNodeCount() != 3 {
		t.Fatal("did not require all nodes")
	}
}

func TestStorageClusterIdentityRootRotationAndDisagreement(t *testing.T) {
	a, _, _ := clusterIdentityRootFixture(t, 3, true)
	b, _, _ := clusterIdentityRootFixture(t, 4, true)
	responses := map[string]*nodes.ListCertificatesInfoResponse{
		"old":     certificateResponse(certificateRow(t, map[string]any{"filename": "pve-root-ca.pem", "pem": a})),
		"rotated": certificateResponse(certificateRow(t, map[string]any{"filename": "pve-root-ca.pem", "pem": b})),
	}
	service := clusterIdentityCertificateFunc(func(_ context.Context, node string) (*nodes.ListCertificatesInfoResponse, error) {
		return responses[node], nil
	})
	old, err := ObserveStorageClusterIdentity(context.Background(), service, []string{"old"})
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := ObserveStorageClusterIdentity(context.Background(), service, []string{"rotated"})
	if err != nil {
		t.Fatal(err)
	}
	if old.ID() == rotated.ID() {
		t.Fatal("rotated CA reused authority identity")
	}
	mixed, err := ObserveStorageClusterIdentity(context.Background(), service, []string{"old", "rotated"})
	if !errors.Is(err, ErrStorageClusterIdentity) || mixed.ID() != "" || !strings.Contains(err.Error(), "roots disagree") {
		t.Fatalf("root mismatch admitted: %v", err)
	}
}

func TestStorageClusterIdentityRejectsMalformedRootFacts(t *testing.T) {
	root, fingerprint, der := clusterIdentityRootFixture(t, 5, true)
	leaf, _, _ := clusterIdentityRootFixture(t, 6, false)
	invalidSignature := append([]byte(nil), der...)
	invalidSignature[len(invalidSignature)-1] ^= 1
	row := func(fields map[string]any) json.RawMessage { return certificateRow(t, fields) }
	valid := row(map[string]any{"filename": "pve-root-ca.pem", "pem": root, "fingerprint": fingerprint})
	cases := []struct {
		name     string
		response *nodes.ListCertificatesInfoResponse
	}{
		{"nil response", nil},
		{"empty response", certificateResponse()},
		{"node cert alone", certificateResponse(row(map[string]any{"filename": "pve-ssl.pem", "pem": root}))},
		{"path is not basename", certificateResponse(row(map[string]any{"filename": "/etc/pve/pve-root-ca.pem", "pem": root}))},
		{"wrong case filename", certificateResponse(row(map[string]any{"filename": "PVE-ROOT-CA.PEM", "pem": root}))},
		{"ambiguous duplicate root", certificateResponse(valid, valid)},
		{"missing filename", certificateResponse(row(map[string]any{"pem": root}))},
		{"null filename", certificateResponse(row(map[string]any{"filename": nil, "pem": root}))},
		{"missing identity", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem"}))},
		{"null PEM does not fallback", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "pem": nil, "fingerprint": fingerprint}))},
		{"empty PEM does not fallback", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "pem": "", "fingerprint": fingerprint}))},
		{"wrong PEM type", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "pem": true, "fingerprint": fingerprint}))},
		{"null fingerprint", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "pem": root, "fingerprint": nil}))},
		{"fingerprint wrong type", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "fingerprint": 7}))},
		{"fingerprint empty", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "fingerprint": ""}))},
		{"fingerprint wrong algorithm", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "fingerprint": strings.Repeat("AA:", 19) + "AA"}))},
		{"fingerprint unseparated", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "fingerprint": strings.ReplaceAll(fingerprint, ":", "")}))},
		{"fingerprint bad hex", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "fingerprint": "ZZ" + fingerprint[2:]}))},
		{"fingerprint bad separators", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "fingerprint": strings.ReplaceAll(fingerprint, ":", "-")}))},
		{"fingerprint mismatch", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "pem": root, "fingerprint": strings.Repeat("00:", 31) + "00"}))},
		{"malformed DER", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "pem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not DER")}))}))},
		{"leaf posing as root", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "pem": leaf}))},
		{"invalid root signature", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "pem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: invalidSignature}))}))},
		{"multiple PEM certificates", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "pem": root + root}))},
		{"trailing PEM data", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "pem": root + "unexpected"}))},
		{"noncertificate PEM", certificateResponse(row(map[string]any{"filename": "pve-root-ca.pem", "pem": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("test-only-invalid-key-marker")}))}))},
		{"invalid JSON", certificateResponse(json.RawMessage(`{"filename":`))},
		{"null JSON", certificateResponse(json.RawMessage(`null`))},
		{"array JSON", certificateResponse(json.RawMessage(`[]`))},
		{"duplicate JSON filename", certificateResponse(json.RawMessage(`{"filename":"pve-root-ca.pem","filename":"pve-root-ca.pem"}`))},
		{"trailing JSON object", certificateResponse(json.RawMessage(`{"filename":"pve-root-ca.pem"}{}`))},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ObserveStorageClusterIdentity(context.Background(), constantCertificateService(tt.response), []string{"node-a"})
			if !errors.Is(err, ErrStorageClusterIdentity) || result.ID() != "" {
				t.Fatalf("invalid evidence admitted: %v", err)
			}
			if strings.Contains(err.Error(), "BEGIN CERTIFICATE") || strings.Contains(err.Error(), "test-only-invalid-key-marker") {
				t.Fatal("certificate payload leaked into diagnostic")
			}
		})
	}
}

func TestStorageClusterIdentityObservationFailureRedaction(t *testing.T) {
	_, fp, _ := clusterIdentityRootFixture(t, 7, true)
	response := certificateResponse(certificateRow(t, map[string]any{"filename": "pve-root-ca.pem", "fingerprint": fp}))
	cause := errors.New("API https://user:PRIVATE-CREDENTIAL@example.invalid failed")
	service := clusterIdentityCertificateFunc(func(_ context.Context, node string) (*nodes.ListCertificatesInfoResponse, error) {
		if node == "bad-node" {
			return nil, cause
		}
		return response, nil
	})
	result, err := ObserveStorageClusterIdentity(context.Background(), service, []string{"good-node", "bad-node"})
	if !errors.Is(err, cause) || !errors.Is(err, ErrStorageClusterIdentity) || result.ID() != "" {
		t.Fatalf("observation failure ignored: %v", err)
	}
	if strings.Contains(err.Error(), "PRIVATE-CREDENTIAL") || strings.Contains(err.Error(), "https://") {
		t.Fatal("API error credentials exposed")
	}
}

func TestStorageClusterIdentityBoundsAndContextCancellation(t *testing.T) {
	t.Run("bounded concurrency and default deadline", func(t *testing.T) {
		_, fp, _ := clusterIdentityRootFixture(t, 8, true)
		response := certificateResponse(certificateRow(t, map[string]any{"filename": "pve-root-ca.pem", "fingerprint": fp}))
		entered := make(chan struct{}, 8)
		gate := make(chan struct{})
		var active, peak, calls atomic.Int32
		service := clusterIdentityCertificateFunc(func(ctx context.Context, _ string) (*nodes.ListCertificatesInfoResponse, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 30*time.Second {
				t.Error("missing bounded observation deadline")
			}
			now := active.Add(1)
			defer active.Add(-1)
			for previous := peak.Load(); now > previous; previous = peak.Load() {
				if peak.CompareAndSwap(previous, now) {
					break
				}
			}
			calls.Add(1)
			entered <- struct{}{}
			select {
			case <-gate:
				return response, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
		finished := make(chan error, 1)
		go func() {
			_, err := ObserveStorageClusterIdentity(context.Background(), service, []string{"n1", "n2", "n3", "n4", "n5", "n6", "n7"})
			finished <- err
		}()
		for range 4 {
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("workers did not enter")
			}
		}
		if active.Load() != 4 {
			t.Fatalf("want 4 active calls, got %d", active.Load())
		}
		close(gate)
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
		if peak.Load() != 4 || calls.Load() != 7 {
			t.Fatalf("observed peak/calls %d/%d", peak.Load(), calls.Load())
		}
	})
	t.Run("cancel active observations", func(t *testing.T) {
		entered := make(chan struct{}, 4)
		var active atomic.Int32
		service := clusterIdentityCertificateFunc(func(ctx context.Context, _ string) (*nodes.ListCertificatesInfoResponse, error) {
			active.Add(1)
			defer active.Add(-1)
			entered <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		finished := make(chan error, 1)
		go func() {
			_, err := ObserveStorageClusterIdentity(ctx, service, []string{"n1", "n2", "n3", "n4", "n5"})
			finished <- err
		}()
		<-entered
		cancel()
		select {
		case err := <-finished:
			if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrStorageClusterIdentity) {
				t.Fatalf("cancellation lost: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cancellation did not stop observation")
		}
		if active.Load() != 0 {
			t.Fatal("API observation leaked after return")
		}
	})
	t.Run("caller deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		service := clusterIdentityCertificateFunc(func(ctx context.Context, _ string) (*nodes.ListCertificatesInfoResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		result, err := ObserveStorageClusterIdentity(ctx, service, []string{"node-a"})
		if !errors.Is(err, context.DeadlineExceeded) || result.ID() != "" {
			t.Fatalf("deadline ignored: %v", err)
		}
	})
}

func TestStorageClusterIdentityInvalidInputsDoNotObserve(t *testing.T) {
	var called atomic.Bool
	service := clusterIdentityCertificateFunc(func(context.Context, string) (*nodes.ListCertificatesInfoResponse, error) {
		called.Store(true)
		return nil, nil
	})
	for _, names := range [][]string{nil, {""}, {"../node"}, {"node.example"}, {" node"}, {"node/other"}, {"-node"}, {"node-"}, {"node_1"}} {
		if _, err := ObserveStorageClusterIdentity(context.Background(), service, names); !errors.Is(err, ErrStorageClusterIdentity) {
			t.Fatalf("invalid nodes accepted: %v", err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ObserveStorageClusterIdentity(canceled, service, []string{"node-a"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context accepted: %v", err)
	}

	if _, err := ObserveStorageClusterIdentity(context.Background(), nil, []string{"node-a"}); !errors.Is(err, ErrStorageClusterIdentity) {
		t.Fatal("nil service accepted")
	}
	if called.Load() {
		t.Fatal("invalid inputs triggered API observation")
	}
}
