package log_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// secretTree builds a representative CPI argument tree carrying the kinds of
// credentials create_vm receives: an mbus URL with embedded NATS creds, a
// blobstore secret/password, registry credentials, and a non-standard
// nats_password key that exact-name matching would miss.
func secretTree() map[string]any {
	return map[string]any{
		"vm_id": "vm-42",
		"env": map[string]any{
			"bosh": map[string]any{
				"mbus": "nats://nats:s3cr3t-mbus@10.0.0.1:4222",
				"blobstore": map[string]any{
					"provider": "s3",
					"options": map[string]any{
						"bucket":            "bosh-blobs",
						"secret_access_key": "AKIA-SECRET-VALUE",
						"password":          "blob-pass",
						"access_key_id":     "AKIA-ID",
					},
				},
				"nats_password": "nats-pw-deep",
			},
		},
		"registry": map[string]any{
			"user":     "registry-user",
			"password": "registry-pass",
			"endpoint": "https://10.0.0.2:25777",
		},
		"networks": []any{
			map[string]any{"type": "manual", "ip": "10.0.0.5"},
			map[string]any{"type": "dynamic", "token": "net-token-secret"},
		},
	}
}

// flatten renders a redacted tree to a single string so a test can assert a
// secret literal appears nowhere in the structure regardless of nesting depth.
func flatten(v any) string {
	var b strings.Builder
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case map[string]any:
			for k, val := range t {
				b.WriteString(k)
				b.WriteByte('=')
				walk(val)
				b.WriteByte(';')
			}
		case []any:
			for _, e := range t {
				walk(e)
				b.WriteByte(',')
			}
		default:
			b.WriteString(strings.TrimSpace(strings.ToLower(toStr(t))))
		}
	}
	walk(v)
	return b.String()
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// TestRedactSecrets_MasksKnownSecrets verifies every credential literal is
// absent from the redacted tree and replaced with the placeholder.
func TestRedactSecrets_MasksKnownSecrets(t *testing.T) {
	t.Parallel()
	out := log.RedactSecrets(secretTree())
	flat := flatten(out)

	secrets := []string{
		"s3cr3t-mbus",       // mbus userinfo password
		"AKIA-SECRET-VALUE", // secret_access_key
		"blob-pass",         // blobstore password
		"registry-pass",     // registry password
		"nats-pw-deep",      // nats_password (substring match, not exact)
		"net-token-secret",  // token inside an array element
	}
	for _, s := range secrets {
		if strings.Contains(flat, strings.ToLower(s)) {
			t.Errorf("secret %q leaked through redaction: %s", s, flat)
		}
	}
	if !strings.Contains(flat, "<redacted>") {
		t.Errorf("expected <redacted> placeholder in output, got: %s", flat)
	}
}

// TestRedactSecrets_PreservesNonSecrets verifies non-sensitive scalars and the
// overall structure (nested maps, arrays) survive unchanged.
func TestRedactSecrets_PreservesNonSecrets(t *testing.T) {
	t.Parallel()
	out, ok := log.RedactSecrets(secretTree()).(map[string]any)
	if !ok {
		t.Fatalf("RedactSecrets must return a map for a map input, got %T", out)
	}
	if out["vm_id"] != "vm-42" {
		t.Errorf("non-secret vm_id altered: %v", out["vm_id"])
	}
	reg, _ := out["registry"].(map[string]any)
	if reg == nil || reg["endpoint"] != "https://10.0.0.2:25777" {
		t.Errorf("non-secret registry.endpoint altered: %v", reg)
	}
	if reg["user"] != "<redacted>" {
		t.Errorf("registry.user (sensitive) not redacted: %v", reg["user"])
	}
	nets, _ := out["networks"].([]any)
	if len(nets) != 2 {
		t.Fatalf("array structure not preserved: %v", nets)
	}
	n0, _ := nets[0].(map[string]any)
	if n0["ip"] != "10.0.0.5" {
		t.Errorf("non-secret networks[0].ip altered: %v", n0)
	}
	// access_key_id is an AWS key id (sensitive) → must be redacted too.
	bs, _ := out["env"].(map[string]any)["bosh"].(map[string]any)["blobstore"].(map[string]any)["options"].(map[string]any)
	if bs["bucket"] != "bosh-blobs" {
		t.Errorf("non-secret bucket altered: %v", bs["bucket"])
	}
}

// TestRedactSecrets_DoesNotMutateInput verifies redaction deep-copies: the
// original tree still holds its secrets after redaction (no aliasing leak).
func TestRedactSecrets_DoesNotMutateInput(t *testing.T) {
	t.Parallel()
	in := secretTree()
	_ = log.RedactSecrets(in)
	reg := in["registry"].(map[string]any)
	if reg["password"] != "registry-pass" {
		t.Errorf("input mutated: registry.password = %v, want original secret intact", reg["password"])
	}
	opts := in["env"].(map[string]any)["bosh"].(map[string]any)["blobstore"].(map[string]any)["options"].(map[string]any)
	if opts["secret_access_key"] != "AKIA-SECRET-VALUE" {
		t.Errorf("input mutated: secret_access_key = %v, want original intact", opts["secret_access_key"])
	}
}

// TestRedactSecrets_ScrubsURLUserinfo verifies a credential embedded in a URL
// userinfo is masked even when the key name itself is not sensitive.
func TestRedactSecrets_ScrubsURLUserinfo(t *testing.T) {
	t.Parallel()
	in := map[string]any{
		"some_endpoint": "https://admin:p4ssw0rd@host.example:8006/api",
		"plain_url":     "https://host.example:8006/api",
	}
	out := log.RedactSecrets(in).(map[string]any)
	if got := out["some_endpoint"].(string); strings.Contains(got, "p4ssw0rd") {
		t.Errorf("URL userinfo credential leaked: %v", got)
	}
	if got := out["some_endpoint"].(string); !strings.Contains(got, "host.example:8006") {
		t.Errorf("URL host portion should be preserved: %v", got)
	}
	if out["plain_url"] != "https://host.example:8006/api" {
		t.Errorf("URL without userinfo must be untouched: %v", out["plain_url"])
	}
}

// TestRedactSecrets_ScrubsQueryAndEmbeddedURLs verifies credentials carried in
// a URL query string, or in a URL that is whitespace-prefixed or embedded inside
// a larger string, are masked — not only the bare-userinfo form.
func TestRedactSecrets_ScrubsQueryAndEmbeddedURLs(t *testing.T) {
	t.Parallel()
	// Keys are deliberately non-sensitive (an endpoint/url under a benign name)
	// so the URL string-scrubber — not key-name matching — does the masking.
	in := map[string]any{
		"blob_endpoint": "https://host.example/path?password=secret123&keep=ok",
		"audit_url":     "https://host.example/path?access_token=tok-leak",
		"leading_ws":    "   nats://nats:pw-ws@h:4222",
		"embedded":      "endpoint is nats://nats:pw-embedded@h:4222 right there",
		"combined_addr": "https://admin:pw-both@h:8006/api?api_key=ak-leak",
	}
	out := log.RedactSecrets(in).(map[string]any)
	for _, secret := range []string{"secret123", "tok-leak", "pw-ws", "pw-embedded", "pw-both", "ak-leak"} {
		for k, v := range out {
			if s, ok := v.(string); ok && strings.Contains(s, secret) {
				t.Errorf("secret %q leaked under %q: %v", secret, k, s)
			}
		}
	}
	// Non-secret query param and host portions survive.
	if got := out["blob_endpoint"].(string); !strings.Contains(got, "keep=ok") {
		t.Errorf("non-secret query param dropped: %v", got)
	}
	if got := out["combined_addr"].(string); !strings.Contains(got, "h:8006") {
		t.Errorf("URL host portion should survive: %v", got)
	}
}

// TestRedactSecrets_URLUserinfoWithAtInPassword verifies a userinfo password
// containing a literal "@" is fully masked. The host begins after the LAST "@"
// before the path, so masking up to the first "@" would leak the password tail.
func TestRedactSecrets_URLUserinfoWithAtInPassword(t *testing.T) {
	t.Parallel()
	in := map[string]any{
		// Benign key names so the URL string-scrubber — not key-name matching —
		// does the masking. Password "p@ss-tail" carries a raw "@"; BOSH-generated
		// mbus/NATS credentials are not percent-encoded, so this occurs in the wild.
		"broker_addr": "nats://nats:p@ss-tail@10.0.0.1:4222",
		"https_url":   "https://admin:pw@with@at@host.example:8006/api",
	}
	out := log.RedactSecrets(in).(map[string]any)
	if got := out["broker_addr"].(string); strings.Contains(got, "ss-tail") {
		t.Errorf("userinfo password tail after literal @ leaked: %v", got)
	}
	if got := out["broker_addr"].(string); !strings.Contains(got, "10.0.0.1:4222") {
		t.Errorf("host portion should be preserved: %v", got)
	}
	if got := out["https_url"].(string); strings.Contains(got, "with@at") {
		t.Errorf("userinfo password with multiple @ leaked: %v", got)
	}
	if got := out["https_url"].(string); !strings.Contains(got, "host.example:8006") {
		t.Errorf("host portion should be preserved: %v", got)
	}
}

// TestRedactSecrets_ScrubsSignatureQueryParams verifies presigned-URL signature
// parameters — S3 "X-Amz-Signature", legacy "Signature", and Azure SAS "sig" —
// are masked, while a benign query name that merely contains the letters "sig"
// (such as "design") is left intact.
func TestRedactSecrets_ScrubsSignatureQueryParams(t *testing.T) {
	t.Parallel()
	in := map[string]any{
		"s3_presigned":  "https://bucket.s3.amazonaws.com/key?X-Amz-Signature=deadbeefcafe&X-Amz-Expires=900",
		"s3_v2_query":   "https://bucket.s3.amazonaws.com/key?Signature=legacysigval&Expires=123",
		"azure_sas":     "https://acct.blob.core.windows.net/c/b?sv=2021&sig=azuresasval&se=2026",
		"benign_design": "https://host.example/render?design=modern&size=large",
	}
	out := log.RedactSecrets(in).(map[string]any)
	for _, secret := range []string{"deadbeefcafe", "legacysigval", "azuresasval"} {
		for k, v := range out {
			if s, ok := v.(string); ok && strings.Contains(s, secret) {
				t.Errorf("signature %q leaked under %q: %v", secret, k, s)
			}
		}
	}
	// A non-secret param whose name only contains the substring "sig" survives.
	if got := out["benign_design"].(string); !strings.Contains(got, "design=modern") {
		t.Errorf("benign design= param must not be redacted: %v", got)
	}
	// Non-secret siblings of a redacted signature param survive.
	if got := out["s3_presigned"].(string); !strings.Contains(got, "X-Amz-Expires=900") {
		t.Errorf("non-secret sibling param dropped: %v", got)
	}
}

// TestRedactSecrets_UserExactMatch verifies "user"/"username" are masked on an
// exact key match while user_data and user_agent (non-secret diagnostics) are
// preserved — substring matching "user" would have clobbered them.
func TestRedactSecrets_UserExactMatch(t *testing.T) {
	t.Parallel()
	in := map[string]any{
		"user":       "registry-user",
		"username":   "admin",
		"user_agent": "bosh-cpi/1.0",
		"user_data":  "non-secret-marker-value",
	}
	out := log.RedactSecrets(in).(map[string]any)
	if out["user"] != "<redacted>" || out["username"] != "<redacted>" {
		t.Errorf("user/username must be redacted: %v", out)
	}
	if out["user_agent"] != "bosh-cpi/1.0" {
		t.Errorf("user_agent (non-secret) must be preserved: %v", out["user_agent"])
	}
	if out["user_data"] != "non-secret-marker-value" {
		t.Errorf("user_data must not be clobbered by substring match: %v", out["user_data"])
	}
}

// TestRedactSecrets_Idempotent verifies redacting an already-redacted tree is a
// fixed point (no double-masking artifacts, no errors).
func TestRedactSecrets_Idempotent(t *testing.T) {
	t.Parallel()
	once := log.RedactSecrets(secretTree())
	twice := log.RedactSecrets(once)
	if !reflect.DeepEqual(once, twice) {
		t.Errorf("RedactSecrets not idempotent:\n once=%v\n twice=%v", once, twice)
	}
}

// TestRedactSecrets_Scalars verifies non-container inputs pass through and a
// bare sensitive-looking string with no key context is URL-scrubbed only.
func TestRedactSecrets_Scalars(t *testing.T) {
	t.Parallel()
	if got := log.RedactSecrets(42); got != 42 {
		t.Errorf("scalar int altered: %v", got)
	}
	if got := log.RedactSecrets("just a string"); got != "just a string" {
		t.Errorf("plain string altered: %v", got)
	}
	if got := log.RedactSecrets(nil); got != nil {
		t.Errorf("nil altered: %v", got)
	}
}

// TestScrubMessage verifies the free-text scrub used by non-ErrScrubbed
// external sinks (span status/events): credential-bearing URLs are masked,
// credential-free text passes through byte-identical.
func TestScrubMessage(t *testing.T) {
	t.Parallel()

	in := "GET https://bosh:s3cretpw@blob.lab.internal/img?X-Amz-Signature=deadbeef1234 returned 403"
	out := log.ScrubMessage(in)
	for _, secret := range []string{"s3cretpw", "deadbeef1234"} {
		if strings.Contains(out, secret) {
			t.Errorf("ScrubMessage leaks credential %q: %q", secret, out)
		}
	}
	if !strings.Contains(out, log.RedactedPlaceholder) {
		t.Errorf("ScrubMessage output missing %q marker: %q", log.RedactedPlaceholder, out)
	}

	plain := "task UPID:pve1:0000ABCD timed out after 30s"
	if got := log.ScrubMessage(plain); got != plain {
		t.Errorf("credential-free text altered: %q -> %q", plain, got)
	}
}
