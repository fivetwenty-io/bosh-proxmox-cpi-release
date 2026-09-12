package pve

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type visibilityGetter struct {
	paths   map[string]map[string]any
	acl     any
	failure error
	calls   []string
}

func (g *visibilityGetter) GetCtx(_ context.Context, path string, params map[string]interface{}) (interface{}, error) {
	g.calls = append(g.calls, path)
	if g.failure != nil {
		return nil, g.failure
	}
	if path == "/access/acl" {
		return g.acl, nil
	}
	p, _ := params["path"].(string)
	return map[string]any{p: g.paths[p]}, nil
}
func visibilityFixture() *visibilityGetter {
	return &visibilityGetter{acl: []any{}, paths: map[string]map[string]any{"/access": {"Sys.Audit": 0}, "/vms": {"VM.Audit": 1, "VM.Config.Disk": 1}, "/storage": {"Datastore.Audit": true}}}
}
func TestStorageAuditVisibilityRequiresUnfilteredCoverage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*visibilityGetter)
	}{
		{"missing image visibility", func(g *visibilityGetter) { delete(g.paths["/vms"], "VM.Config.Disk") }},
		{"unpropagated image visibility", func(g *visibilityGetter) { g.paths["/vms"]["VM.Config.Disk"] = 0 }},
		{"denied descendant image visibility", func(g *visibilityGetter) {
			g.acl = []any{map[string]any{"path": "/vms/123", "roleid": "PVEAuditor"}}
			g.paths["/vms/123"] = map[string]any{"VM.Audit": 0}
		}},
		{"missing storage parent", func(g *visibilityGetter) { delete(g.paths, "/storage") }},
		{"unpropagated VM audit", func(g *visibilityGetter) { g.paths["/vms"]["VM.Audit"] = 0 }},
		{"filtered ACL read", func(g *visibilityGetter) { delete(g.paths, "/access") }},
		{"hidden denied storage", func(g *visibilityGetter) {
			g.acl = []any{map[string]any{"path": "/storage/hidden", "roleid": "NoAccess"}}
		}},
		{"hidden pool members", func(g *visibilityGetter) { g.acl = []any{map[string]any{"path": "/pool/hidden", "roleid": "NoAccess"}} }},
		{"malformed flags", func(g *visibilityGetter) { g.paths["/vms"]["VM.Audit"] = "yes" }},
		{"malformed ACL row", func(g *visibilityGetter) { g.acl = []any{map[string]any{"path": "/storage/hidden"}} }},
		{"transport error", func(g *visibilityGetter) { g.failure = errors.New("backend-secret") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := visibilityFixture()
			tc.mutate(g)
			err := observeStorageAuditVisibility(context.Background(), g)
			if err == nil || strings.Contains(err.Error(), "backend-secret") {
				t.Fatalf("unsafe visibility: %v", err)
			}
		})
	}
}
func TestStorageAuditVisibilityAcceptsExplicitGrantedLeaves(t *testing.T) {
	g := visibilityFixture()
	g.acl = []any{map[string]any{"path": "/storage/a", "roleid": "BoshOperator"}, map[string]any{"path": "/vms/123", "roleid": "PVEAuditor"}, map[string]any{"path": "/pool/other-user-denied", "roleid": "NoAccess"}}
	g.paths["/storage/a"] = map[string]any{"Datastore.Audit": 0}
	g.paths["/vms/123"] = map[string]any{"VM.Audit": false, "VM.Config.Disk": false}
	g.paths["/pool/other-user-denied"] = map[string]any{"Pool.Audit": 1}
	if err := observeStorageAuditVisibility(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if len(g.calls) != 7 {
		t.Fatalf("expected explicit parent/leaf permission queries: %v", g.calls)
	}
}

func TestStorageAuditVisibilityAcceptsCompleteStorageAllocateBypass(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		g := visibilityFixture()
		delete(g.paths["/vms"], "VM.Config.Disk")
		g.paths["/storage"]["Datastore.Allocate"] = 1
		g.acl = []any{map[string]any{"path": "/storage/restricted", "roleid": "StorageReader"}}
		g.paths["/storage/restricted"] = map[string]any{"Datastore.Audit": 0, "Datastore.Allocate": 0}
		if incomplete {
			delete(g.paths["/storage/restricted"], "Datastore.Allocate")
		}
		err := observeStorageAuditVisibility(t.Context(), g)
		if (err != nil) != incomplete {
			t.Fatalf("incomplete=%t: %v", incomplete, err)
		}
	}
}
