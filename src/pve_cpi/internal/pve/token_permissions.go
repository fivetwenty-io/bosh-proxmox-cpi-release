package pve

import (
	"context"
	"encoding/json"
	"strings"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// TokenPermissionReader reads the privileges the API token holds on an ACL
// path. The normal Client interface stays unchanged for legacy consumers; a
// caller that needs the privileges asserts a Client to this interface and
// treats a client that does not implement it as unable to read them.
type TokenPermissionReader interface {
	// EffectivePermissions returns the privileges the token holds on path, from
	// GET /access/permissions?path=<path>. A privilege is held when its key is
	// present. Its value is true when the privilege also propagates to children
	// of the path, which PVE reports as 1 rather than 0. A path on which the
	// token holds nothing returns an empty map and no error.
	EffectivePermissions(ctx context.Context, path string) (map[string]bool, error)
}

// EffectivePermissions implements TokenPermissionReader.
func (c *sdkClient) EffectivePermissions(ctx context.Context, path string) (map[string]bool, error) {
	if ctx == nil {
		return nil, cpierrors.Cloud("EffectivePermissions: ctx must not be nil")
	}
	if !strings.HasPrefix(path, "/") {
		return nil, cpierrors.Cloud("EffectivePermissions: path %q must start with /", path)
	}
	if c.auditReader == nil {
		return nil, cpierrors.Cloud("EffectivePermissions: no raw PVE reader available")
	}
	var raw interface{}
	err := RetryOnTransient(ctx, nil, "token_permissions", 0, func() error {
		var inner error
		raw, inner = c.auditReader.GetCtx(ctx, "/access/permissions", map[string]interface{}{"path": path})
		return inner
	})
	if err != nil {
		return nil, cpierrors.Wrap(WrapError(err), "token permissions: read "+path)
	}
	return decodeTokenPermissions(raw, path)
}

// decodeTokenPermissions extracts the privilege map of path from the response
// of GET /access/permissions. PVE omits a path on which the token holds
// nothing, so a missing path is an empty map, while a body of any other shape
// is an error because it must not pass for "no privileges".
func decodeTokenPermissions(raw interface{}, path string) (map[string]bool, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, cpierrors.Wrap(err, "token permissions: re-encode response")
	}
	var byPath map[string]map[string]sdkclient.PVEBool
	if err := json.Unmarshal(encoded, &byPath); err != nil || byPath == nil {
		return nil, cpierrors.Cloud("token permissions: response for %s is not an object of privilege maps", path)
	}
	out := make(map[string]bool, len(byPath[path]))
	for privilege, propagates := range byPath[path] {
		out[privilege] = propagates.Bool()
	}
	return out, nil
}
