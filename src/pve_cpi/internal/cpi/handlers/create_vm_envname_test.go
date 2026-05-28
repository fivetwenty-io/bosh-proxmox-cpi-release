package handlers

import (
	"testing"
)

// TestExtractJobNameFromEnv_BasicGroups exercises typical env shapes from a
// director-mode deploy where env.bosh.group has the canonical
// "<director>-<deployment>-<job>" form and env.bosh.groups enumerates every
// combination. The shortest groups entry whose suffix matches group is the
// bare job name.
func TestExtractJobNameFromEnv_BasicGroups(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		env  map[string]any
		want string
	}{
		{
			name: "simple job",
			env: map[string]any{
				"bosh": map[string]any{
					"group": "bosh-cf-database",
					"groups": []any{
						"bosh", "cf", "database",
						"bosh-cf", "cf-database", "bosh-database",
						"bosh-cf-database",
					},
				},
			},
			want: "database",
		},
		{
			name: "hyphenated job (diego-api)",
			env: map[string]any{
				"bosh": map[string]any{
					"group": "bosh-cf-diego-api",
					"groups": []any{
						"bosh", "cf", "diego-api",
						"bosh-cf", "cf-diego-api", "bosh-diego-api",
						"bosh-cf-diego-api",
					},
				},
			},
			want: "diego-api",
		},
		{
			name: "shortest matching suffix wins over deployment-job",
			env: map[string]any{
				"bosh": map[string]any{
					"group":  "d1-d2-j",
					"groups": []any{"j", "d2-j", "d1-d2-j"},
				},
			},
			want: "j",
		},
		{
			name: "missing bosh key",
			env:  map[string]any{},
			want: "",
		},
		{
			name: "bosh present but group empty",
			env: map[string]any{
				"bosh": map[string]any{
					"groups": []any{"a", "b"},
				},
			},
			want: "",
		},
		{
			name: "bosh present but groups empty",
			env: map[string]any{
				"bosh": map[string]any{
					"group":  "a-b-c",
					"groups": []any{},
				},
			},
			want: "",
		},
		{
			name: "no matching suffix",
			env: map[string]any{
				"bosh": map[string]any{
					"group":  "a-b-c",
					"groups": []any{"x", "y", "a-b-c"},
				},
			},
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := extractJobNameFromEnv(c.env)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestExtractDeploymentFromEnv verifies that the deployment segment of
// env.bosh.group is resolved against env.bosh.groups using the same
// "shortest matching suffix" rule that extractJobNameFromEnv uses for the
// job. The deployment is the shortest groups entry that is a suffix of
// (group - "-" - job) and is not the bare director.
func TestExtractDeploymentFromEnv(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		env  map[string]any
		job  string
		want string
	}{
		{
			name: "simple deployment + job",
			env: map[string]any{
				"bosh": map[string]any{
					"group": "bosh-cf-database",
					"groups": []any{
						"bosh", "cf", "database",
						"bosh-cf", "cf-database", "bosh-database",
						"bosh-cf-database",
					},
				},
			},
			job:  "database",
			want: "cf",
		},
		{
			name: "hyphenated job preserves deployment resolution",
			env: map[string]any{
				"bosh": map[string]any{
					"group": "bosh-cf-diego-cell",
					"groups": []any{
						"bosh", "cf", "diego-cell",
						"bosh-cf", "cf-diego-cell", "bosh-diego-cell",
						"bosh-cf-diego-cell",
					},
				},
			},
			job:  "diego-cell",
			want: "cf",
		},
		{
			name: "hyphenated deployment (cf-prod)",
			env: map[string]any{
				"bosh": map[string]any{
					"group": "bosh-cf-prod-api",
					"groups": []any{
						"bosh", "cf-prod", "api",
						"bosh-cf-prod", "cf-prod-api", "bosh-api",
						"bosh-cf-prod-api",
					},
				},
			},
			job:  "api",
			want: "cf-prod",
		},
		{
			name: "no bosh key",
			env:  map[string]any{},
			job:  "anything",
			want: "",
		},
		{
			name: "empty job aborts resolution",
			env: map[string]any{
				"bosh": map[string]any{
					"group":  "bosh-cf-database",
					"groups": []any{"bosh", "cf", "database"},
				},
			},
			job:  "",
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := extractDeploymentFromEnv(c.env, c.job)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestComposeVMName verifies the prefix/deployment/job/index assembly rules:
// empty segments are dropped (no double-dash), prefix sorts before
// deployment, and an empty input yields the empty string so the caller can
// fall back to "vm-<vmid>".
func TestComposeVMName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		prefix     string
		deployment string
		job        string
		index      string
		want       string
	}{
		{name: "all four parts", prefix: "cpi", deployment: "cf", job: "api", index: "0", want: "cpi-cf-api-0"},
		{name: "no prefix", prefix: "", deployment: "cf", job: "api", index: "0", want: "cf-api-0"},
		{name: "no deployment", prefix: "cpi", deployment: "", job: "api", index: "1", want: "cpi-api-1"},
		{name: "no index (initial create)", prefix: "cpi", deployment: "create-env", job: "bosh", index: "", want: "cpi-create-env-bosh"},
		{name: "underscores sanitized", prefix: "cpi", deployment: "cf", job: "diego_cell", index: "0", want: "cpi-cf-diego-cell-0"},
		{name: "all empty → empty", prefix: "", deployment: "", job: "", index: "", want: ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := composeVMName(c.prefix, c.deployment, c.job, c.index)
			if got != c.want {
				t.Errorf("composeVMName(%q,%q,%q,%q) = %q, want %q",
					c.prefix, c.deployment, c.job, c.index, got, c.want)
			}
		})
	}
}
