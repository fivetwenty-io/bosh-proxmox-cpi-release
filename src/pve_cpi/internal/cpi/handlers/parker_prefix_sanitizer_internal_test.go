package handlers

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// TestSanitizeVMName_MatchesConfigTwin holds the two copies of the VM-name
// sanitizer together. The config package cannot import this one, because this
// one already imports config and the reverse edge would be a cycle, so
// config.SanitizeVMName duplicates sanitizeVMName rather than calling it. The
// duplication is only safe while the two agree byte for byte, and this test is
// what keeps them agreeing.
//
// The agreement matters because a workload VM and the parker that holds its
// detached disks derive their names from the same prefix. Sanitizers that drift
// would give the two a different prefix for the same pve.vm_prefix, and the
// bosh-parker tag and the parker name would then disagree about what the prefix
// is.
func TestSanitizeVMName_MatchesConfigTwin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"already_clean", "cpi"},
		{"single_character", "p"},
		{"underscore", "my_prefix"},
		{"mixed_case", "MyPrefix"},
		{"leading_hyphen", "-parked"},
		{"trailing_hyphen", "parked-"},
		{"hyphen_runs", "--collapse--me--"},
		{"slashes", "a/b/c"},
		{"bosh_instance_name", "diego-cell/2844c990-1234-5678-9abc-def012345678"},
		{"all_illegal", "____"},
		{"digits_lead", "9-lives"},
		{"non_ascii", "ünïcødé"},
		{"spaces", "a b  c"},
		{"dots", "bosh.parker.pool"},
		{"over_the_pve_cap", strings.Repeat("x", 80)},
		{"cut_lands_on_a_hyphen", strings.Repeat("a", 62) + "-" + strings.Repeat("b", 10)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := sanitizeVMName(tc.in)
			got := config.SanitizeVMName(tc.in)
			if got != want {
				t.Errorf("config.SanitizeVMName(%q) = %q, but handlers.sanitizeVMName(%q) = %q; "+
					"the two copies must agree byte for byte", tc.in, got, tc.in, want)
			}
		})
	}
}
