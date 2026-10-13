package handlers_test

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// testClusterStamp is the cluster stamp testCertificatesInfo's answer
// produces, which is the first eight hex characters of its root fingerprint.
const testClusterStamp = "0a1b2c3d"

// testRootFingerprint is a pve-root-ca.pem SHA-256 fingerprint in PVE's
// colon-separated form, starting with the bytes of testClusterStamp.
var testRootFingerprint = "0A:1B:2C:3D:" + strings.Repeat("EE:", 27) + "FF"

// certificatesInfoAnswer returns a /nodes/{node}/certificates/info answer
// whose cluster root reports fingerprint and no PEM, which the stamp reader
// accepts.
func certificatesInfoAnswer(fingerprint string) *sdknodes.ListCertificatesInfoResponse {
	rows := []map[string]any{
		{"filename": "pve-ssl.pem", "fingerprint": strings.Repeat("11:", 31) + "11"},
		{"filename": "pve-root-ca.pem", "fingerprint": fingerprint},
	}
	out := make(sdknodes.ListCertificatesInfoResponse, 0, len(rows))
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			panic(err)
		}
		out = append(out, raw)
	}
	return &out
}

// testCertificatesInfo is the default answer the handler mocks give, which
// stamps every created VM with testClusterStamp.
func testCertificatesInfo() *sdknodes.ListCertificatesInfoResponse {
	return certificatesInfoAnswer(testRootFingerprint)
}

// certificatesInfoOr answers with fn when a test set one, and with the
// default fixture otherwise.
func certificatesInfoOr(
	ctx context.Context, node string,
	fn func(ctx context.Context, node string) (*sdknodes.ListCertificatesInfoResponse, error),
) (*sdknodes.ListCertificatesInfoResponse, error) {
	if fn != nil {
		return fn(ctx, node)
	}
	return testCertificatesInfo(), nil
}

// ListCertificatesInfo answers the cluster stamp read create_vm makes.
func (m *vmMockNodes) ListCertificatesInfo(ctx context.Context, node string) (*sdknodes.ListCertificatesInfoResponse, error) {
	return certificatesInfoOr(ctx, node, m.certificatesInfoFn)
}

// ListCertificatesInfo answers the cluster stamp read create_vm makes.
func (h *healthNodes) ListCertificatesInfo(ctx context.Context, node string) (*sdknodes.ListCertificatesInfoResponse, error) {
	return certificatesInfoOr(ctx, node, nil)
}

// ListCertificatesInfo answers the cluster stamp read that the absent-VMID
// answers and the guard make.
func (m *mockNodesService) ListCertificatesInfo(ctx context.Context, node string) (*sdknodes.ListCertificatesInfoResponse, error) {
	return certificatesInfoOr(ctx, node, m.certificatesInfoFn)
}

// returnedNames reports whether a create_vm result's CID, bare or stamped,
// names vmid.
func returnedNames(cid any, vmid int) bool {
	s, ok := cid.(string)
	return ok && vmcid.SameVM(s, vmid)
}

// withoutTokenTag returns a tag list without its bosh-cid-- entries, with the
// token they carried. token is "" when there was none, and when two entries
// disagree, so a caller that checks it with vmcid.IsStamp fails on both.
func withoutTokenTag(tags string) (rest, token string) {
	kept := make([]string, 0, 8)
	seen := 0
	for _, entry := range strings.Split(tags, ";") {
		if value, ok := strings.CutPrefix(entry, "bosh-cid--"); ok {
			seen++
			token = value
			continue
		}
		kept = append(kept, entry)
	}
	if seen != 1 {
		token = ""
	}
	return strings.Join(kept, ";"), token
}
