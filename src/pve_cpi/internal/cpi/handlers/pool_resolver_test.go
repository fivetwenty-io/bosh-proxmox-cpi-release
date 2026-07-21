package handlers

import (
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// ---------------------------------------------------------------------------
// resolvePoolName precedence
// ---------------------------------------------------------------------------

func TestResolvePoolName_CallLevelWins(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMPool:         "bosh",
		VMPoolTemplate: "{prefix}-template",
		VMTypes: map[string]config.TypeProfile{
			"medium": {CloudProperties: map[string]any{"pool": "vt"}},
		},
	}
	callCP := map[string]any{"vm_type": "medium", "pool": "team-a"}
	r, err := newPoolResolver(callCP, cfg)
	if err != nil {
		t.Fatalf("newPoolResolver: unexpected error: %v", err)
	}
	name, err := resolvePoolName(cfg, r, map[string]any{})
	if err != nil {
		t.Fatalf("resolvePoolName: unexpected error: %v", err)
	}
	if name != "team-a" {
		t.Errorf("name = %q; want team-a", name)
	}
}

func TestResolvePoolName_VMTypeOverGlobal(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMPool: "bosh",
		VMTypes: map[string]config.TypeProfile{
			"medium": {CloudProperties: map[string]any{"pool": "vt"}},
		},
	}
	callCP := map[string]any{"vm_type": "medium"}
	r, err := newPoolResolver(callCP, cfg)
	if err != nil {
		t.Fatalf("newPoolResolver: unexpected error: %v", err)
	}
	name, err := resolvePoolName(cfg, r, map[string]any{})
	if err != nil {
		t.Fatalf("resolvePoolName: unexpected error: %v", err)
	}
	if name != "vt" {
		t.Errorf("name = %q; want vt", name)
	}
}

func TestResolvePoolName_DiskTypePoolIgnored(t *testing.T) {
	t.Parallel()

	t.Run("vm_type beats disk_type", func(t *testing.T) {
		t.Parallel()

		cfg := &config.CPIConfig{
			VMPool: "bosh",
			VMTypes: map[string]config.TypeProfile{
				"medium": {CloudProperties: map[string]any{"pool": "vt"}},
			},
			DiskTypes: map[string]config.TypeProfile{
				"fast": {CloudProperties: map[string]any{"pool": "dt"}},
			},
		}
		callCP := map[string]any{"vm_type": "medium", "disk_type": "fast"}
		r, err := newPoolResolver(callCP, cfg)
		if err != nil {
			t.Fatalf("newPoolResolver: unexpected error: %v", err)
		}
		name, err := resolvePoolName(cfg, r, map[string]any{})
		if err != nil {
			t.Fatalf("resolvePoolName: unexpected error: %v", err)
		}
		if name != "vt" {
			t.Errorf("name = %q; want vt (disk_type pool must not outrank vm_type)", name)
		}
	})

	t.Run("disk_type alone falls through to global", func(t *testing.T) {
		t.Parallel()

		cfg := &config.CPIConfig{
			VMPool: "bosh",
			DiskTypes: map[string]config.TypeProfile{
				"fast": {CloudProperties: map[string]any{"pool": "dt"}},
			},
		}
		callCP := map[string]any{"disk_type": "fast"}
		r, err := newPoolResolver(callCP, cfg)
		if err != nil {
			t.Fatalf("newPoolResolver: unexpected error: %v", err)
		}
		name, err := resolvePoolName(cfg, r, map[string]any{})
		if err != nil {
			t.Fatalf("resolvePoolName: unexpected error: %v", err)
		}
		if name != "bosh" {
			t.Errorf("name = %q; want bosh (disk_type layer has no vote in pool resolution)", name)
		}
	})
}

func TestResolvePoolName_TemplateOverGlobal(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMPool:         "bosh",
		VMPoolTemplate: "{prefix}-{deployment}",
		VMPrefix:       "cpi",
	}
	env := map[string]any{
		"bosh": map[string]any{
			"group":  "dir1-dep1-web",
			"groups": []any{"dir1", "dep1", "web"},
		},
	}
	r, err := newPoolResolver(map[string]any{}, cfg)
	if err != nil {
		t.Fatalf("newPoolResolver: unexpected error: %v", err)
	}
	name, err := resolvePoolName(cfg, r, env)
	if err != nil {
		t.Fatalf("resolvePoolName: unexpected error: %v", err)
	}
	if name != "cpi-dep1" {
		t.Errorf("name = %q; want cpi-dep1", name)
	}
}

func TestResolvePoolName_GlobalDefaultBosh(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMPool: "bosh"}
	r, err := newPoolResolver(map[string]any{}, cfg)
	if err != nil {
		t.Fatalf("newPoolResolver: unexpected error: %v", err)
	}
	name, err := resolvePoolName(cfg, r, map[string]any{})
	if err != nil {
		t.Fatalf("resolvePoolName: unexpected error: %v", err)
	}
	if name != "bosh" {
		t.Errorf("name = %q; want bosh", name)
	}
}

func TestResolvePoolName_ExplicitEmptyOptOut(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	r, err := newPoolResolver(map[string]any{}, cfg)
	if err != nil {
		t.Fatalf("newPoolResolver: unexpected error: %v", err)
	}
	name, err := resolvePoolName(cfg, r, map[string]any{})
	if err != nil {
		t.Fatalf("resolvePoolName: unexpected error: %v", err)
	}
	if name != "" {
		t.Errorf("name = %q; want empty (no pool assignment)", name)
	}
}

func TestResolvePoolName_InvalidSlashName(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMPool: "bosh"}
	callCP := map[string]any{"pool": "a/b"}
	r, err := newPoolResolver(callCP, cfg)
	if err != nil {
		t.Fatalf("newPoolResolver: unexpected error: %v", err)
	}
	name, err := resolvePoolName(cfg, r, map[string]any{})
	if err == nil {
		t.Fatalf("expected error for slash-containing pool name, got name=%q", name)
	}
	if name != "" {
		t.Errorf("name = %q on error; want empty", name)
	}
}

// ---------------------------------------------------------------------------
// renderPoolTemplate
// ---------------------------------------------------------------------------

func TestRenderPoolTemplate_AllTokens(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMPoolTemplate: "{prefix}-{director}-{deployment}-{instance_group}",
		VMPrefix:       "ocfp",
	}
	env := map[string]any{
		"bosh": map[string]any{
			"group":  "dir1-dep1-web",
			"groups": []any{"dir1", "dep1", "web"},
		},
	}
	got := renderPoolTemplate(cfg, env)
	if got != "ocfp-dir1-dep1-web" {
		t.Errorf("renderPoolTemplate = %q; want ocfp-dir1-dep1-web", got)
	}
}

func TestRenderPoolTemplate_CreateEnvFallback(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMPoolTemplate:      "{prefix}-{deployment}",
		VMPrefix:            "",
		CreateEnvDeployment: "create-env",
	}
	got := renderPoolTemplate(cfg, map[string]any{})
	if got != "create-env" {
		t.Errorf("renderPoolTemplate = %q; want create-env (leading '-' trimmed)", got)
	}
}

func TestRenderPoolTemplate_CollapsesToEmpty(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{VMPoolTemplate: "{director}"}
	got := renderPoolTemplate(cfg, map[string]any{})
	if got != "" {
		t.Errorf("renderPoolTemplate = %q; want empty (caller falls through to global)", got)
	}
}

// ---------------------------------------------------------------------------
// extractDirectorFromEnv
// ---------------------------------------------------------------------------

func TestExtractDirectorFromEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		env        map[string]any
		deployment string
		job        string
		want       string
	}{
		{
			name: "derives director from group suffix trim",
			env: map[string]any{
				"bosh": map[string]any{"group": "dir1-dep1-web"},
			},
			deployment: "dep1",
			job:        "web",
			want:       "dir1",
		},
		{
			name:       "empty deployment returns empty",
			env:        map[string]any{"bosh": map[string]any{"group": "dir1-dep1-web"}},
			deployment: "",
			job:        "web",
			want:       "",
		},
		{
			name:       "empty job returns empty",
			env:        map[string]any{"bosh": map[string]any{"group": "dir1-dep1-web"}},
			deployment: "dep1",
			job:        "",
			want:       "",
		},
		{
			name:       "missing bosh env returns empty",
			env:        map[string]any{},
			deployment: "dep1",
			job:        "web",
			want:       "",
		},
		{
			name:       "no-op trim (group does not end with suffix) returns empty",
			env:        map[string]any{"bosh": map[string]any{"group": "unrelated"}},
			deployment: "dep1",
			job:        "web",
			want:       "",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := extractDirectorFromEnv(tc.env, tc.deployment, tc.job)
			if got != tc.want {
				t.Errorf("extractDirectorFromEnv() = %q; want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// validateResolvedPoolName
// ---------------------------------------------------------------------------

func TestValidateResolvedPoolName_SlashRejected(t *testing.T) {
	t.Parallel()

	_, err := validateResolvedPoolName("bosh/cf")
	if err == nil {
		t.Fatal("expected error for slash-containing name")
	}
}

func TestValidateResolvedPoolName_BadCharsetRejected(t *testing.T) {
	t.Parallel()

	_, err := validateResolvedPoolName("bosh cf!")
	if err == nil {
		t.Fatal("expected error for name outside PVE poolid charset")
	}
}

func TestValidateResolvedPoolName_BoshLockPrefixRejected(t *testing.T) {
	t.Parallel()

	_, err := validateResolvedPoolName("bosh-lock-x")
	if err == nil {
		t.Fatal("expected error for reserved bosh-lock- namespace")
	}
}

func TestValidateResolvedPoolName_Valid(t *testing.T) {
	t.Parallel()

	name, err := validateResolvedPoolName("bosh")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "bosh" {
		t.Errorf("name = %q; want bosh", name)
	}
}
