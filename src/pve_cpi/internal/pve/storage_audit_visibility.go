package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// StorageAuditVisibilityReader proves that permission-filtered listings can
// cover the namespace. The normal Client interface remains unchanged for legacy
// consumers; managed audit requires this additional read capability.
type StorageAuditVisibilityReader interface{ StorageAuditVisibility(context.Context) error }
type auditPermissionGetter interface {
	GetCtx(context.Context, string, map[string]interface{}) (interface{}, error)
}

// auditVisibilityMemo caches a proven audit visibility for the life of the
// client. Only a success is cached: a failure can come from a transport fault
// as readily as from a missing grant, and re-proving on the next call is what
// lets a retry succeed once PVE answers again. A cached success cannot go
// stale within a CPI call, because revoking a grant mid-call would only ever
// make the proof stricter than the listings it guards.
type auditVisibilityMemo struct {
	mu     sync.Mutex
	proven bool
}

func (c *sdkClient) StorageAuditVisibility(ctx context.Context) error {
	c.auditVisibility.mu.Lock()
	proven := c.auditVisibility.proven
	c.auditVisibility.mu.Unlock()
	if proven {
		return nil
	}
	if err := observeStorageAuditVisibility(ctx, c.auditReader); err != nil {
		return err
	}
	c.auditVisibility.mu.Lock()
	c.auditVisibility.proven = true
	c.auditVisibility.mu.Unlock()
	return nil
}

// PVE omits denied paths from the all-permissions response. Read the unfiltered
// ACL path inventory and query each relevant path explicitly, including pool
// NoAccess rules that can hide members despite inherited VM/storage privileges.
func observeStorageAuditVisibility(ctx context.Context, reader auditPermissionGetter) error {
	if ctx == nil || reader == nil {
		return fmt.Errorf("allocation audit visibility reader unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	access, err := auditPermissionsAt(ctx, reader, "/access")
	if err != nil {
		return err
	}
	if _, ok := access["Sys.Audit"]; !ok {
		return fmt.Errorf("allocation audit requires Sys.Audit at /access to inspect unfiltered ACL paths")
	}
	raw, err := reader.GetCtx(ctx, "/access/acl", nil)
	if err != nil {
		return fmt.Errorf("allocation audit ACL visibility unavailable")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("allocation audit ACL inventory malformed")
	}
	var rows []struct {
		Path string `json:"path"`
		Role string `json:"roleid"`
	}
	if json.Unmarshal(encoded, &rows) != nil || rows == nil || len(rows) > 4096 {
		return fmt.Errorf("allocation audit ACL inventory malformed or exceeds limit")
	}
	required := map[string]string{"/vms": "VM.Audit", "/storage": "Datastore.Audit"}
	for _, row := range rows {
		if !strings.HasPrefix(row.Path, "/") || row.Role == "" {
			return fmt.Errorf("allocation audit ACL entry malformed")
		}
		switch {
		case row.Path == "/vms" || strings.HasPrefix(row.Path, "/vms/"):
			required[row.Path] = "VM.Audit"
		case row.Path == "/storage" || strings.HasPrefix(row.Path, "/storage/"):
			required[row.Path] = "Datastore.Audit"
		case row.Role == "NoAccess" && (row.Path == "/pool" || strings.HasPrefix(row.Path, "/pool/")):
			required[row.Path] = ""
		}
	}
	paths := make([]string, 0, len(required))
	for path := range required {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	observed := make(map[string]map[string]json.RawMessage, len(paths))
	for _, path := range paths {
		values, err := auditPermissionsAt(ctx, reader, path)
		if err != nil {
			return err
		}
		if len(values) == 0 {
			return fmt.Errorf("allocation audit visibility is restricted at %s", path)
		}
		observed[path] = values
		if privilege := required[path]; privilege != "" {
			if err := requireAuditPrivilege(values, privilege, path); err != nil {
				return err
			}
		}
	}
	return requireImageListingVisibility(paths, observed)
}

func requireImageListingVisibility(paths []string, observed map[string]map[string]json.RawMessage) error {
	// PVE filters image listings by VM.Config.Disk, even when VM.Audit allows
	// the VM itself to be listed. Datastore.Allocate bypasses that image check.
	// Only a grant covering every storage path can replace the VM privilege.
	storageAllocate := true
	for _, path := range paths {
		if path == "/storage" || strings.HasPrefix(path, "/storage/") {
			storageAllocate = storageAllocate && requireAuditPrivilege(observed[path], "Datastore.Allocate", path) == nil
		}
	}
	if !storageAllocate {
		for _, path := range paths {
			if path == "/vms" || strings.HasPrefix(path, "/vms/") {
				if err := requireAuditPrivilege(observed[path], "VM.Config.Disk", path); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func requireAuditPrivilege(values map[string]json.RawMessage, privilege, path string) error {
	value, present := values[privilege]
	if !present {
		return fmt.Errorf("allocation audit requires %s at %s", privilege, path)
	}
	// Permission values describe propagation, not whether the grant exists.
	propagates := false
	switch string(value) {
	case "1", "true":
		propagates = true
	case "0", "false":
	default:
		return fmt.Errorf("allocation audit propagation flag malformed")
	}
	if (path == "/vms" || path == "/storage") && !propagates {
		return fmt.Errorf("allocation audit requires propagated %s at %s", privilege, path)
	}
	return nil
}

func auditPermissionsAt(ctx context.Context, reader auditPermissionGetter, path string) (map[string]json.RawMessage, error) {
	raw, err := reader.GetCtx(ctx, "/access/permissions", map[string]interface{}{"path": path})
	if err != nil {
		return nil, fmt.Errorf("allocation audit effective permission observation unavailable")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("allocation audit effective permissions malformed")
	}
	var permissions map[string]map[string]json.RawMessage
	if json.Unmarshal(encoded, &permissions) != nil || permissions == nil {
		return nil, fmt.Errorf("allocation audit effective permissions malformed")
	}
	values := permissions[path]
	for _, value := range values {
		switch string(value) {
		case "0", "1", "true", "false":
		default:
			return nil, fmt.Errorf("allocation audit effective permission flag malformed")
		}
	}
	return values, nil
}
