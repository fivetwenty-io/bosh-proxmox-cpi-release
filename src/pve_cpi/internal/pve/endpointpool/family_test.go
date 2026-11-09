package endpointpool

import (
	"context"
	"net/http"
	"testing"
)

// A mutation's tether key is its method and API route without the query
// string, unless the route falls in a family, whose name is the key whatever
// the method.
func TestTetherKeyOf(t *testing.T) {
	methodPath := func(method, route string) tetherKey { return tetherKey{method: method, route: route} }
	cases := []struct {
		method, url string
		want        tetherKey
	}{
		{"POST", "https://pve1:8006/api2/json/nodes/n3/qemu", methodPath("POST", "/nodes/n3/qemu")},
		{"POST", "https://pve1:8006/api2/json/nodes/n3/qemu?vmid=101", methodPath("POST", "/nodes/n3/qemu")},
		{"POST", "https://pve1:8006/api2/json/nodes/n3/qemu/", methodPath("POST", "/nodes/n3/qemu")},
		{"POST", "https://pve1:8006/api2/json//nodes/n3//qemu", methodPath("POST", "/nodes/n3/qemu")},
		{"post", "https://pve1:8006/api2/json/nodes/n3/qemu", methodPath("POST", "/nodes/n3/qemu")},
		{"DELETE", "https://pve1:8006/api2/json/nodes/n3/qemu/101", methodPath("DELETE", "/nodes/n3/qemu/101")},
		{"PUT", "https://pve1:8006/api2/json/nodes/n3/qemu/101/config",
			methodPath("PUT", "/nodes/n3/qemu/101/config")},
		{"POST", "https://pve1:8006/other/path", methodPath("POST", "/other/path")},

		{"PUT", "https://pve1:8006/api2/json/cluster/ha/rules/cpi-aa-web", tetherKey{family: familyHA}},
		{"POST", "https://pve1:8006/api2/json/cluster/ha/rules", tetherKey{family: familyHA}},
		{"DELETE", "https://pve1:8006/api2/json/cluster/ha/resources/vm:101", tetherKey{family: familyHA}},
		{"POST", "https://pve1:8006/api2/json/cluster/ha/groups", tetherKey{family: familyHA}},
		{"POST", "https://pve1:8006/api2/json/cluster/ha", tetherKey{family: familyHA}},
		{"PUT", "https://pve1:8006/api2/json/cluster/hardware", methodPath("PUT", "/cluster/hardware")},

		{"POST", "https://pve1:8006/api2/json/cluster/replication", tetherKey{family: familyReplication}},
		{"PUT", "https://pve1:8006/api2/json/cluster/replication/101-0", tetherKey{family: familyReplication}},
		{"DELETE", "https://pve1:8006/api2/json/cluster/replication/101-0?force=1",
			tetherKey{family: familyReplication}},
		{"POST", "https://pve1:8006/api2/json/nodes/n3/replication/101-0/schedule_now",
			methodPath("POST", "/nodes/n3/replication/101-0/schedule_now")},

		{"POST", "https://pve1:8006/api2/json/pools", tetherKey{family: familyPools}},
		{"DELETE", "https://pve1:8006/api2/json/pools?poolid=cpi-lock", tetherKey{family: familyPools}},
		{"PUT", "https://pve1:8006/api2/json/pools/cpi-lock", tetherKey{family: familyPools}},
		{"POST", "https://pve1:8006/api2/json/poolsx", methodPath("POST", "/poolsx")},
	}
	for _, c := range cases {
		req, err := http.NewRequestWithContext(context.Background(), c.method, c.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := tetherKeyOf(req); got != c.want {
			t.Errorf("%s %s: key = %+v, want %+v", c.method, c.url, got, c.want)
		}
	}
}

func TestFamilyOf(t *testing.T) {
	cases := map[string]family{
		"/cluster/ha":                familyHA,
		"/cluster/ha/status/current": familyHA,
		"/cluster/hax":               "",
		"/cluster/replication":       familyReplication,
		"/cluster/replicationx":      "",
		"/pools":                     familyPools,
		"/pools/x":                   familyPools,
		"/nodes/pve1/qemu":           "",
		"/":                          "",
	}
	for route, want := range cases {
		if got := familyOf(route); got != want {
			t.Errorf("familyOf(%q) = %q, want %q", route, got, want)
		}
	}
}
