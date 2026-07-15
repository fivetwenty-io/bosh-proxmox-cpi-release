// PVE authenticated-identity classification, used by callers that need to
// know whether a privileged-only API parameter (currently: skiplock) is
// available given the CPI's configured credentials.
package pve

import (
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// rootPamIdentity is the exact user@realm string PVE requires for the
// skiplock parameter on stop/destroy calls. PVE::API2::Qemu's check is a
// literal comparison of the full authenticated-user identity against this
// string — it rejects skiplock=true for any other identity regardless of
// granted privileges, and there is no ACL grant that extends it to a role.
//
// Confirmed against Proxmox forum thread 111633 ("Proxmox VE uses token
// based authentication not support option skiplock"): a root-owned token
// calling with skiplock=1 is rejected with "Only root may use this option",
// confirmed by Proxmox staff in that thread. A token request's authenticated
// identity always carries a "!<token-id>" suffix (e.g. "root@pam!bosh-cpi"),
// which never equals the bare "root@pam" this comparison requires — no API
// token qualifies, regardless of which user owns the token.
const rootPamIdentity = "root@pam"

// IsRootPamIdentity reports whether the CPI's configured PVE identity is
// exactly the root@pam superuser authenticated via password — the only
// identity PVE honors the skiplock parameter for (see rootPamIdentity).
// Mirrors the same user@realm composition newClient uses for password auth
// (client.go): cfg.User already containing "@" is used verbatim, otherwise
// cfg.Realm — defaulting to "pam" when empty — is appended.
//
// API-token authentication ALWAYS returns false here, even for a token owned
// by root@pam (e.g. "root@pam!bosh-cpi"): see rootPamIdentity's doc comment
// for why PVE never honors skiplock for a token identity regardless of the
// owning user.
//
// nil cfg → false. A cfg with neither APIToken nor Password set → false (no
// identity is configured yet; callers must treat this the same as "not
// root@pam" rather than risk a skiplock attempt with unresolved auth).
func IsRootPamIdentity(cfg *config.CPIConfig) bool {
	if cfg == nil {
		return false
	}
	if cfg.APIToken != "" {
		return false
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
