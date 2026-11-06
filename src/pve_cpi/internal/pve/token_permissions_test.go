package pve

import (
	"context"
	"errors"
	"testing"
)

func TestDecodeTokenPermissions(t *testing.T) {
	cases := []struct {
		name    string
		raw     any
		want    map[string]bool
		wantErr bool
	}{
		{
			name: "held and propagating, held without propagation",
			raw:  map[string]any{"/": map[string]any{"Sys.Console": 1, "Sys.Audit": 0}},
			want: map[string]bool{"Sys.Console": true, "Sys.Audit": false},
		},
		{name: "path omitted means nothing held", raw: map[string]any{}, want: map[string]bool{}},
		{name: "path present but empty", raw: map[string]any{"/": map[string]any{}}, want: map[string]bool{}},
		{name: "null body", raw: nil, wantErr: true},
		{name: "array body", raw: []any{1}, wantErr: true},
		{name: "privilege map of the wrong shape", raw: map[string]any{"/": []any{"Sys.Console"}}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeTokenPermissions(tc.raw, "/")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if gv, ok := got[k]; !ok || gv != v {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestSDKClientEffectivePermissions(t *testing.T) {
	g := &visibilityGetter{paths: map[string]map[string]any{"/vms": {"VM.Replicate": 1}}}
	c := &sdkClient{auditReader: g}

	got, err := c.EffectivePermissions(context.Background(), "/vms")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got["VM.Replicate"] {
		t.Fatalf("VM.Replicate should be held and propagating, got %v", got)
	}
	if len(g.calls) != 1 || g.calls[0] != "/access/permissions" {
		t.Fatalf("calls = %v", g.calls)
	}

	g.failure = errors.New("boom")
	if _, err := c.EffectivePermissions(context.Background(), "/vms"); err == nil {
		t.Fatal("a failed read must return an error")
	}
}

func TestSDKClientEffectivePermissionsRejectsBadInput(t *testing.T) {
	c := &sdkClient{auditReader: &visibilityGetter{}}
	var nilCtx context.Context
	if _, err := c.EffectivePermissions(nilCtx, "/"); err == nil {
		t.Fatal("nil ctx must be rejected")
	}
	if _, err := c.EffectivePermissions(context.Background(), "vms"); err == nil {
		t.Fatal("a path without a leading slash must be rejected")
	}
	if _, err := (&sdkClient{}).EffectivePermissions(context.Background(), "/"); err == nil {
		t.Fatal("a client without a raw reader must be rejected")
	}
	var _ TokenPermissionReader = c
}
