package pve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

func TestDescribeAuditErrorRendersOnlyClassifiedFields(t *testing.T) {
	var apiErr *sdkerrors.APIError
	if !errors.As(sdkerrors.ParseAPIError(500, []byte(`{"message":"storage 'nas' is not online\n"}`)), &apiErr) {
		t.Fatal("fixture did not produce an APIError")
	}
	permission := sdkerrors.ParseAPIError(403, []byte(`{"message":"Permission check failed (/storage/nas, Datastore.Audit)"}`))
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"api error", apiErr, "HTTP 500: storage 'nas' is not online"},
		{"wrapped api error", cpierrors.Wrap(WrapError(apiErr), "list content"), "HTTP 500: storage 'nas' is not online"},
		{"permission error", permission, "HTTP 403: Permission check failed (/storage/nas, Datastore.Audit)"},
		{"parameter error", &sdkerrors.ParameterError{APIError: sdkerrors.APIError{Message: "bad node", HTTPCode: 400}, Usage: "secret-usage"}, "HTTP 400: bad node"},
		{"authentication error", &sdkerrors.AuthenticationError{APIError: sdkerrors.APIError{Message: "invalid ticket", HTTPCode: 401}, Realm: "pam"}, "HTTP 401: invalid ticket"},
		{"connection error", &sdkerrors.ConnectionError{Host: "pve1.lab", Port: 8006, Message: "dial secret-detail", Cause: errors.New("password=hunter2")}, "connection to pve1.lab:8006 failed"},
		{"timeout error", &sdkerrors.TimeoutError{Operation: "GET /secret", Duration: "30s"}, "request timed out"},
		{"deadline", fmt.Errorf("list: %w", context.DeadlineExceeded), "context deadline exceeded"},
		{"canceled", context.Canceled, "context canceled"},
		{"visibility", &AuditVisibilityError{Privilege: "VM.Audit", Path: "/vms"}, "allocation audit requires VM.Audit at /vms"},
		{"propagated visibility", &AuditVisibilityError{Privilege: "Datastore.Audit", Path: "/storage", Propagated: true}, "allocation audit requires propagated Datastore.Audit at /storage"},
		{"restricted visibility", &AuditVisibilityError{Path: "/pool/secret-pool"}, "allocation audit visibility is restricted at /pool/secret-pool"},
		{"enumeration", &GuestEnumerationError{Nodes: []string{"pve2", "pve3"}}, "could not list guests on node(s) pve2,pve3"},
		{"enumeration with verdict", &GuestEnumerationError{Nodes: []string{"pve2"}, Cause: permission}, "could not list guests on node pve2 (HTTP 403: Permission check failed (/storage/nas, Datastore.Audit))"},
		{"enumeration with transport", &GuestEnumerationError{Nodes: []string{"pve2"}, Cause: errors.New("secret-transport")}, "could not list guests on node pve2 (unclassified error)"},
		{"known provenance parse error", fmt.Errorf("ambiguous storage allocation provenance"), "ambiguous storage allocation provenance"},
		{"known disk provenance parse error", fmt.Errorf("conflicting managed disk provenance carriers"), "conflicting managed disk provenance carriers"},
		{"unclassified", errors.New("transport response secret-password"), "unclassified error"},
		{"wrapped unclassified", cpierrors.Cloud("config: open /x: secret"), "unclassified error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DescribeAuditError(tc.err); got != tc.want {
				t.Fatalf("DescribeAuditError = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDescribeAuditErrorScrubsBeforeTruncating(t *testing.T) {
	// The credential sits across the 200-byte boundary. Truncating first would
	// cut the userinfo before its "@", and the scrubber would then miss it.
	message := strings.Repeat("x", 180) + " https://root:hunter2@pve1:8006/api2/json"
	got := DescribeAuditError(&sdkerrors.APIError{Message: message, HTTPCode: 502})
	if strings.Contains(got, "hunter2") || strings.Contains(got, "hunt") {
		t.Fatalf("credential survived scrub-then-truncate: %q", got)
	}
	if len(got) > auditDescriptionLimit {
		t.Fatalf("description is %d bytes, want at most %d", len(got), auditDescriptionLimit)
	}
}

func TestDescribeAuditErrorStripsControlCharacters(t *testing.T) {
	got := DescribeAuditError(&sdkerrors.APIError{Message: "<html>\r\n<body>proxy\x1b[31m error</body>\t</html>", HTTPCode: 502})
	if strings.ContainsAny(got, "\r\n\t\x1b") {
		t.Fatalf("control characters survived: %q", got)
	}
	if !strings.HasPrefix(got, "HTTP 502: <html>") || !strings.Contains(got, "proxy [31m error") {
		t.Fatalf("description lost its text: %q", got)
	}
	if got := DescribeAuditError(&sdkerrors.APIError{Message: "PVEAPIToken=root@pam!cpi=token-secret denied", HTTPCode: 401}); strings.Contains(got, "token-secret") {
		t.Fatalf("token header survived: %q", got)
	}
}

func TestDescribeAuditErrorTruncatesOnRuneBoundary(t *testing.T) {
	// "HTTP 500: " is 10 bytes, so 189 ASCII bytes put the next three-byte rune
	// across the 200-byte limit.
	message := strings.Repeat("a", 189) + strings.Repeat("€", 10)
	got := DescribeAuditError(&sdkerrors.APIError{Message: message, HTTPCode: 500})
	if !utf8.ValidString(got) {
		t.Fatalf("truncation split a rune: %q", got)
	}
	if len(got) != 199 {
		t.Fatalf("description is %d bytes, want 199 (the whole rune that crossed the limit dropped)", len(got))
	}
}

func TestAuditVisibilityErrorsAreTyped(t *testing.T) {
	for _, tc := range []struct {
		name string
		g    *visibilityGetter
		want AuditVisibilityError
	}{
		{"access", &visibilityGetter{paths: map[string]map[string]any{"/access": {}}}, AuditVisibilityError{Privilege: "Sys.Audit", Path: "/access"}},
		{"restricted", &visibilityGetter{paths: map[string]map[string]any{"/access": {"Sys.Audit": 1}}, acl: []any{}}, AuditVisibilityError{Path: "/storage"}},
		{"missing privilege", &visibilityGetter{paths: map[string]map[string]any{"/access": {"Sys.Audit": 1}, "/storage": {"Datastore.Audit": 1}, "/vms": {"VM.Allocate": 1}}, acl: []any{}}, AuditVisibilityError{Privilege: "VM.Audit", Path: "/vms"}},
		{"unpropagated", &visibilityGetter{paths: map[string]map[string]any{"/access": {"Sys.Audit": 1}, "/storage": {"Datastore.Audit": 0}, "/vms": {"VM.Audit": 1}}, acl: []any{}}, AuditVisibilityError{Privilege: "Datastore.Audit", Path: "/storage", Propagated: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := observeStorageAuditVisibility(context.Background(), tc.g)
			var visibility *AuditVisibilityError
			if !errors.As(err, &visibility) {
				t.Fatalf("visibility failure is untyped: %v", err)
			}
			if visibility.Privilege != tc.want.Privilege || visibility.Path != tc.want.Path || visibility.Propagated != tc.want.Propagated {
				t.Fatalf("visibility error = %+v, want %+v", *visibility, tc.want)
			}
		})
	}
}

func TestPartialFleetErrorNamesFailedNodesWithUnchangedText(t *testing.T) {
	retriable := partialFleetError([]string{"pve2", "pve3"}, []error{errors.New("dial"), errors.New("dial")})
	if !cpierrors.IsType(retriable, cpierrors.TypeRetriableCloud) {
		t.Fatalf("transport partial fleet lost its retriable class: %v", retriable)
	}
	if retriable.Error() != "ListGuestsAuthoritative: could not list guests on node(s) pve2,pve3; refusing to decide from a partial fleet" {
		t.Fatalf("partial fleet text changed: %q", retriable.Error())
	}
	verdict := partialFleetError([]string{"pve2"}, []error{sdkerrors.ParseAPIError(403, []byte(`{"message":"denied"}`))})
	if cpierrors.IsType(verdict, cpierrors.TypeRetriableCloud) {
		t.Fatalf("permanent API verdict became retriable: %v", verdict)
	}
	if !strings.HasPrefix(verdict.Error(), "ListGuestsAuthoritative: could not list guests on node pve2; refusing to decide from a partial fleet: ") {
		t.Fatalf("verdict text changed: %q", verdict.Error())
	}
	for _, err := range []error{retriable, verdict} {
		var enumeration *GuestEnumerationError
		if !errors.As(err, &enumeration) || len(enumeration.Nodes) == 0 {
			t.Fatalf("partial fleet error does not carry its nodes: %v", err)
		}
	}
}

// A walk that reached no endpoint names every endpoint it tried, in the order
// it tried them, so an operator sees which nodes were down and not only the
// primary the SDK reports.
func TestDescribeAuditErrorNamesEveryEndpointAWalkTried(t *testing.T) {
	refused := errors.New("connection refused")
	dial := func(endpoint string) endpointpool.EndpointFailure {
		return walkFailure(endpoint, endpointpool.PhaseDial, refused)
	}
	verify := func(endpoint string) endpointpool.EndpointFailure {
		return walkFailure(endpoint, endpointpool.PhaseTLSVerify, errors.New("x509: unknown authority"))
	}
	for _, tc := range []struct {
		name     string
		attempts []endpointpool.EndpointFailure
		want     string
	}{
		{"one endpoint", []endpointpool.EndpointFailure{dial("pve1:8006")}, "connection to pve1:8006 failed"},
		{
			"three endpoints in the order tried",
			[]endpointpool.EndpointFailure{dial("pve2:8006"), dial("pve1:8006"), dial("pve3:8006")},
			"connection to pve2:8006, pve1:8006, pve3:8006 failed",
		},
		{
			"an endpoint tried twice is named once",
			[]endpointpool.EndpointFailure{dial("pve1:8006"), dial("pve2:8006"), dial("pve1:8006")},
			"connection to pve1:8006, pve2:8006 failed",
		},
		{
			"an IPv6 endpoint keeps its brackets",
			[]endpointpool.EndpointFailure{dial("[fd00::1]:8006"), dial("pve2:8006")},
			"connection to [fd00::1]:8006, pve2:8006 failed",
		},
		{
			"every hop failed certificate verification",
			[]endpointpool.EndpointFailure{verify("pve1:8006"), verify("pve2:8006")},
			"connection to pve1:8006, pve2:8006 failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			walk := endpointpool.NewEntryUnreachableError(tc.attempts)
			for shape, err := range map[string]error{
				"bare":                  walk,
				"in the SDK's wrapping": sdkWrapTransportError(walk),
				"under a CPI wrap":      cpierrors.Wrap(sdkWrapTransportError(walk), "list guests"),
			} {
				if got := DescribeAuditError(err); got != tc.want {
					t.Errorf("%s: DescribeAuditError = %q, want %q", shape, got, tc.want)
				}
			}
		})
	}
}

// A walk with no attempt to name falls back to the SDK's connection error, and
// a walk's cause text never reaches the description.
func TestDescribeAuditErrorWalkWithoutAttemptsFallsBack(t *testing.T) {
	walk := endpointpool.NewEntryUnreachableError(nil)
	if got := DescribeAuditError(walk); got != "unclassified error" {
		t.Errorf("bare empty walk = %q, want %q", got, "unclassified error")
	}
	wrapped := &sdkerrors.ConnectionError{Host: "pve1", Port: 8006, Message: "failed", Cause: walk}
	if got := DescribeAuditError(wrapped); got != "connection to pve1:8006 failed" {
		t.Errorf("empty walk in a ConnectionError = %q, want the SDK's endpoint", got)
	}
	secret := endpointpool.NewEntryUnreachableError([]endpointpool.EndpointFailure{
		walkFailure("pve1:8006", endpointpool.PhaseDial, errors.New("password=hunter2")),
	})
	if got := DescribeAuditError(sdkWrapTransportError(secret)); strings.Contains(got, "hunter2") {
		t.Errorf("a hop's cause text reached the description: %q", got)
	}
}

// The walk check sits after the context checks, as the connection checks do,
// so a call that ended on its own context reads exactly as it did before the
// pool existed, whether the walk is the only error or joined with the context
// error the way disk_transfer.go joins its last error. A walk with a header
// deadline hop satisfies errors.Is(context.DeadlineExceeded) and reads the same
// way.
func TestDescribeAuditErrorContextOutranksAWalk(t *testing.T) {
	walk := endpointpool.NewEntryUnreachableError([]endpointpool.EndpointFailure{
		walkFailure("pve1:8006", endpointpool.PhaseDial, errors.New("connection refused")),
		walkFailure("pve2:8006", endpointpool.PhaseDial, errors.New("connection refused")),
	})
	headerDeadline := endpointpool.NewEntryUnreachableError([]endpointpool.EndpointFailure{
		{Endpoint: "pve1:8006", Err: context.DeadlineExceeded},
		walkFailure("pve2:8006", endpointpool.PhaseDial, errors.New("connection refused")),
	})
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{
			"joined with a deadline",
			errors.Join(sdkWrapTransportError(walk), context.DeadlineExceeded),
			"context deadline exceeded",
		},
		{"joined with a cancellation", errors.Join(sdkWrapTransportError(walk), context.Canceled), "context canceled"},
		{
			"joined with a deadline, walk first",
			errors.Join(walk, context.DeadlineExceeded),
			"context deadline exceeded",
		},
		{"a header deadline hop", sdkWrapTransportError(headerDeadline), "context deadline exceeded"},
		{"the walk alone", sdkWrapTransportError(walk), "connection to pve1:8006, pve2:8006 failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DescribeAuditError(tc.err); got != tc.want {
				t.Errorf("DescribeAuditError = %q, want %q", got, tc.want)
			}
		})
	}
}

// A long walk never has a name cut in half and never loses the word that says
// it failed. Endpoints are named in full while they fit under the cap, and the
// line ends with a count of the rest.
func TestDescribeAuditErrorBoundsALongWalkOnAnEndpointBoundary(t *testing.T) {
	endpointAt := func(i int) string { return fmt.Sprintf("pve-node-%02d.lab.example.com:8006", i) }
	walkOf := func(n int) error {
		attempts := make([]endpointpool.EndpointFailure, 0, n)
		for i := 1; i <= n; i++ {
			attempts = append(
				attempts,
				walkFailure(endpointAt(i), endpointpool.PhaseDial, errors.New("connection refused")),
			)
		}
		return sdkWrapTransportError(endpointpool.NewEntryUnreachableError(attempts))
	}
	t.Run("five names fit and are all named", func(t *testing.T) {
		want := "connection to " + endpointAt(1) + ", " + endpointAt(2) + ", " + endpointAt(3) + ", " + endpointAt(4) +
			", " + endpointAt(5) + " failed"
		if got := DescribeAuditError(walkOf(5)); got != want {
			t.Errorf("DescribeAuditError = %q, want %q", got, want)
		}
	})
	t.Run("sixteen names end with a count", func(t *testing.T) {
		got := DescribeAuditError(walkOf(16))
		if len(got) > auditDescriptionLimit {
			t.Errorf("description is %d bytes, want at most %d: %q", len(got), auditDescriptionLimit, got)
		}
		if !strings.HasPrefix(got, "connection to "+endpointAt(1)+", "+endpointAt(2)+", ") {
			t.Errorf("description %q does not start with the first endpoints in order", got)
		}
		named := strings.Count(got, ".lab.example.com:8006")
		if named == 0 || named >= 16 {
			t.Fatalf("description %q names %d endpoints, want some but not all", got, named)
		}
		wantTail := fmt.Sprintf(" and %d more failed", 16-named)
		if !strings.HasSuffix(got, wantTail) {
			t.Errorf("description %q does not end with %q", got, wantTail)
		}
		for i := 1; i <= named; i++ {
			if !strings.Contains(got, endpointAt(i)+",") && !strings.Contains(got, endpointAt(i)+" and") {
				t.Errorf("description %q does not name %s in full", got, endpointAt(i))
			}
		}
		// One more name would not have fit.
		longer := strings.TrimSuffix(
			got,
			wantTail,
		) + ", " + endpointAt(named+1) + fmt.Sprintf(" and %d more failed", 16-named-1)
		if len(longer) <= auditDescriptionLimit {
			t.Errorf("description %q left room for another name", got)
		}
	})
	t.Run("a name too long for the cap gives only the count", func(t *testing.T) {
		long := strings.Repeat("a", 190) + ".example.com:8006"
		walk := endpointpool.NewEntryUnreachableError([]endpointpool.EndpointFailure{
			walkFailure(long, endpointpool.PhaseDial, errors.New("connection refused")),
			walkFailure("pve2:8006", endpointpool.PhaseDial, errors.New("connection refused")),
		})
		if got, want := DescribeAuditError(walk), "connection to 2 endpoints failed"; got != want {
			t.Errorf("DescribeAuditError = %q, want %q", got, want)
		}
		single := endpointpool.NewEntryUnreachableError([]endpointpool.EndpointFailure{
			walkFailure(long, endpointpool.PhaseDial, errors.New("connection refused")),
		})
		if got, want := DescribeAuditError(single), "connection to 1 endpoint failed"; got != want {
			t.Errorf("DescribeAuditError = %q, want %q", got, want)
		}
	})
}
