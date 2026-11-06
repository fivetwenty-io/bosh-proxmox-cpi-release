package ha_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
)

func TestRuleWriteGate(t *testing.T) {
	t.Parallel()
	node := func(name, version string, online bool) ha.NodeFacts {
		return ha.NodeFacts{Name: name, Online: online, Version: version}
	}
	cases := []struct {
		name    string
		profile *ha.Profile
		wantErr string
	}{
		{"nil profile", nil, "cluster profile"},
		{"every node on PVE 9", &ha.Profile{HAVersionOK: true,
			Nodes: []ha.NodeFacts{node("a", "9.0.3", true), node("b", "9.2.1", true)}}, ""},
		{"an offline node known to be below PVE 9", &ha.Profile{HAVersionOK: true,
			Nodes: []ha.NodeFacts{node("a", "9.2.1", true), node("b", "8.4.1", false)}}, "b (8.4.1)"},
		{"floor not confirmed", &ha.Profile{Nodes: []ha.NodeFacts{node("a", "9.2.1", true)}}, "does not confirm"},
	}
	for _, tc := range cases {
		err := tc.profile.RuleWriteGate()
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: error = %v; want it to contain %q", tc.name, err, tc.wantErr)
		}
	}
}
