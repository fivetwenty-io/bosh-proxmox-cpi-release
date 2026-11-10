package config_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// emptyHostEndpoints are the two spellings that leave no host once the
// trailing dot is removed.
var emptyHostEndpoints = []string{".", ".:8006"}

const emptyHostWant = "host must not be empty once the trailing dot is removed"

func TestValidate_NodeEndpointsRefuseAnEmptyHost(t *testing.T) {
	t.Parallel()
	for _, addr := range emptyHostEndpoints {
		t.Run(addr, func(t *testing.T) {
			t.Parallel()
			c := baseValidCfg()
			c.NodeEndpoints = map[string]string{"pve2": addr}
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), `node_endpoints["pve2"] `+emptyHostWant) {
				t.Errorf("Validate() = %v, want the node_endpoints empty host refusal", err)
			}
		})
	}
}

func TestValidate_EndpointsListRefusesAnEmptyHost(t *testing.T) {
	t.Parallel()
	for _, addr := range emptyHostEndpoints {
		t.Run(addr, func(t *testing.T) {
			t.Parallel()
			second := baseValidCfg()
			second.Host = ""
			second.Endpoints = []string{"pve1.example.com", addr}
			err := second.Validate()
			if err == nil || !strings.Contains(err.Error(), "endpoints[1] "+emptyHostWant) {
				t.Errorf("second entry: Validate() = %v, want the endpoints[1] empty host refusal", err)
			}

			first := baseValidCfg()
			first.Host = ""
			first.Endpoints = []string{addr, "pve1.example.com"}
			err = first.Validate()
			if err == nil || !strings.Contains(err.Error(), "endpoints[0] "+emptyHostWant) {
				t.Errorf("first entry: Validate() = %v, want the endpoints[0] empty host refusal", err)
			}
		})
	}
}

func TestValidate_PVEHostRefusesAnEmptyHost(t *testing.T) {
	t.Parallel()
	for _, addr := range emptyHostEndpoints {
		t.Run(addr, func(t *testing.T) {
			t.Parallel()
			c := baseValidCfg()
			c.Host = addr
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), "pve.host "+emptyHostWant) {
				t.Errorf("Validate() = %v, want the pve.host empty host refusal", err)
			}

			// A list is validated entry by entry, so the first entry
			// carries the refusal and pve.host adds none of its own.
			listed := baseValidCfg()
			listed.Host = addr
			listed.Endpoints = []string{addr}
			err = listed.Validate()
			if err == nil || !strings.Contains(err.Error(), "endpoints[0] "+emptyHostWant) {
				t.Errorf("with a list: Validate() = %v, want the endpoints[0] empty host refusal", err)
			}
		})
	}
}

func TestLoad_RefusesAnEmptyHostInEachEndpointProperty(t *testing.T) {
	t.Parallel()
	for _, addr := range emptyHostEndpoints {
		t.Run(addr, func(t *testing.T) {
			t.Parallel()
			for name, extra := range map[string]string{
				"node_endpoints": `"node_endpoints": {"pve2": "` + addr + `"}`,
				"endpoints":      `"endpoints": ["` + addr + `"]`,
			} {
				_, err := mustLoad(t, endpointsLoadJSON(extra))
				if err == nil || !strings.Contains(err.Error(), emptyHostWant) {
					t.Errorf("%s: Load error = %v, want the empty host refusal", name, err)
				}
			}
		})
	}
}

func TestApplyContextOverrides_RefuseAnEmptyHost(t *testing.T) {
	t.Parallel()
	for _, addr := range emptyHostEndpoints {
		t.Run(addr, func(t *testing.T) {
			t.Parallel()
			for name, tc := range map[string]struct {
				extra map[string]any
				want  string
			}{
				"pve_host": {
					extra: map[string]any{"pve_host": addr},
					want:  "pve.host " + emptyHostWant,
				},
				"pve_endpoints": {
					extra: map[string]any{"pve_endpoints": []any{addr}},
					want:  "endpoints[0] " + emptyHostWant,
				},
			} {
				_, _, _, err := config.ApplyContextOverrides(endpointsTokenJob(), endpointsEntryWithToken(tc.extra))
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s: err = %v, want it to contain %q", name, err, tc.want)
				}
			}

			// node_endpoints is not an entry key. An entry inherits the job's
			// map, and the merged config is validated again.
			job := endpointsTokenJob()
			job.NodeEndpoints = map[string]string{"pve2": addr}
			_, _, _, err := config.ApplyContextOverrides(job, endpointsEntryWithToken(map[string]any{"pve_user": "ops@pam"}))
			if err == nil || !strings.Contains(err.Error(), `node_endpoints["pve2"] `+emptyHostWant) {
				t.Errorf("inherited node_endpoints: err = %v, want the empty host refusal", err)
			}
		})
	}
}
