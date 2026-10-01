package pve

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// failingVisibilityGetter answers like visibilityFixture except at one path,
// where it fails with failure or, when failure is nil, answers with a value
// that is not the object PVE sends.
type failingVisibilityGetter struct {
	*visibilityGetter
	at      string
	failure error
}

func (g *failingVisibilityGetter) GetCtx(ctx context.Context, path string, params map[string]interface{}) (interface{}, error) {
	key := path
	if p, ok := params["path"].(string); ok {
		key = p
	}
	if key != g.at {
		return g.visibilityGetter.GetCtx(ctx, path, params)
	}
	if g.failure != nil {
		return nil, g.failure
	}
	return "not an object", nil
}

// TestAuditVisibilityReadFailuresHaveANamedClass pins that a visibility read
// that failed, or answered with something malformed, describes itself by the
// read, the path, and the class of its cause, where it used to describe
// itself as "unclassified error". Neither the description nor err.Error()
// may carry the cause's own text.
func TestAuditVisibilityReadFailuresHaveANamedClass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		at      string
		failure error
		want    string
	}{
		{"timeout", "/access", context.DeadlineExceeded, "could not read effective permissions at /access (context deadline exceeded)"},
		{"HTTP error", "/access/acl", sdkerrors.ParseAPIError(500, []byte(`{"message":"acl read failed"}`)), "could not read ACL entries at /access/acl (HTTP 500: acl read failed)"},
		{"malformed permissions", "/vms", nil, "PVE returned malformed effective permissions at /vms"},
		{"malformed ACL", "/access/acl", nil, "PVE returned malformed ACL entries at /access/acl"},
		{"untyped transport error", "/storage", errors.New("transport backend-secret"), "could not read effective permissions at /storage (unclassified error)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := observeStorageAuditVisibility(t.Context(), &failingVisibilityGetter{visibilityGetter: visibilityFixture(), at: tc.at, failure: tc.failure})
			if err == nil {
				t.Fatal("visibility proven despite a failed read")
			}
			if got := DescribeAuditError(err); got != tc.want {
				t.Fatalf("DescribeAuditError = %q, want %q", got, tc.want)
			}
			if strings.Contains(err.Error(), "backend-secret") || strings.Contains(err.Error(), "acl read failed") {
				t.Fatalf("err.Error() = %q carries the cause's text", err)
			}
			if tc.failure != nil && !errors.Is(err, tc.failure) {
				t.Fatalf("err %v does not expose its cause", err)
			}
		})
	}
	err := observeStorageAuditVisibility(t.Context(), nil)
	if got := DescribeAuditError(err); got != "the audit visibility reader is unavailable (CPI defect)" {
		t.Fatalf("missing reader described as %q", got)
	}
}
