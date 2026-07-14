// PVE authenticated-identity classification, used by callers that need to
// know whether a privileged-only API parameter (currently: skiplock) is
// available given the CPI's configured credentials.
package pve

import (
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// rootPamIdentity is the exact user@realm string PVE requires for the
// skiplock parameter on stop/destroy calls (PVE::API2::Qemu rejects
// skiplock=true for any other authenticated identity regardless of granted
// privileges — there is no ACL grant that extends it to a role or token).
const rootPamIdentity = "root@pam"

// IsRootPamIdentity reports whether the CPI's configured PVE identity is
// exactly the root@pam superuser. Mirrors the same user@realm composition
// newClient uses for password auth (client.go): cfg.User already containing
// "@" is used verbatim, otherwise cfg.Realm — defaulting to "pam" when empty
// — is appended. For API-token auth, the user portion of the token string
// ("<user>@<realm>!<token-id>=<uuid>") is extracted and compared the same way.
//
// nil cfg → false. A cfg with neither APIToken nor Password set → false (no
// identity is configured yet; callers must treat this the same as "not
// root@pam" rather than risk a skiplock attempt with unresolved auth).
func IsRootPamIdentity(cfg *config.CPIConfig) bool {
	if cfg == nil {
		return false
	}
	if cfg.APIToken != "" {
		user, _, ok := strings.Cut(cfg.APIToken, "!")
		return ok && user == rootPamIdentity
	}
	if cfg.Password == "" {
		return false
	}
	if strings.Contains(cfg.User, "@") {
		return cfg.User == rootPamIdentity
	}
	realm := cfg.Realm
	if realm == "" {
		realm = "pam"
	}
	return cfg.User == "root" && realm == "pam"
}
