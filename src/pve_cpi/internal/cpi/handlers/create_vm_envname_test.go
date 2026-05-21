package handlers

import "testing"

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
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := extractJobNameFromEnv(c.env)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
