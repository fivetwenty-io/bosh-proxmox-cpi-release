package endpointpool

import (
	"net/http"
	"net/url"
	"testing"
)

func TestAPIRoute(t *testing.T) {
	cases := []struct {
		path  string
		route string
		ok    bool
	}{
		{"/api2/json/cluster/resources", "/cluster/resources", true},
		{"/api2/json/nodes/pve1/qemu", "/nodes/pve1/qemu", true},
		{"/api2/json/", "/", true},
		{"/api2/json", "", false},
		{"/api2/jsonx/nodes", "", false},
		{"/cluster/resources", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		route, ok := apiRoute(c.path)
		if route != c.route || ok != c.ok {
			t.Errorf("apiRoute(%q) = %q, %v, want %q, %v", c.path, route, ok, c.route, c.ok)
		}
	}
}

func TestRouteNode(t *testing.T) {
	cases := []struct {
		route  string
		node   string
		scoped bool
	}{
		{"/nodes/pve1/qemu/101/status/current", "pve1", true},
		{"/nodes/pve1", "pve1", true},
		{"/nodes/", "", true},
		{"/nodes//qemu", "", true},
		{"/nodes", "", false},
		{"/cluster/resources", "", false},
		{"/access/ticket", "", false},
		{"/storage/nodes/pve1", "", false},
	}
	for _, c := range cases {
		node, scoped := routeNode(c.route)
		if node != c.node || scoped != c.scoped {
			t.Errorf("routeNode(%q) = %q, %v, want %q, %v", c.route, node, scoped, c.node, c.scoped)
		}
	}
}

// A path is the entry's fault only when it names no node, or names the
// entry's own recorded node exactly.
func TestAttributable(t *testing.T) {
	named := &endpoint{node: "pve1"}
	unnamed := &endpoint{}
	cases := []struct {
		path string
		ep   *endpoint
		want bool
	}{
		{"/api2/json/cluster/resources", unnamed, true},
		{"/api2/json/access/ticket", unnamed, true},
		{"/api2/json/version", named, true},
		{"/api2/json/nodes", unnamed, true},
		{"/api2/json/nodes/pve1/status", named, true},
		{"/api2/json/nodes/pve1", named, true},
		{"/api2/json/nodes/pve2/status", named, false},
		{"/api2/json/nodes/PVE1/status", named, false},
		{"/api2/json/nodes/pve1/status", unnamed, false},
		{"/api2/json/nodes//status", named, false},
		{"/api2/json/nodes/", unnamed, false},
		{"/other/cluster/resources", unnamed, false},
	}
	for _, c := range cases {
		if got := attributable(c.path, c.ep); got != c.want {
			t.Errorf("attributable(%q, node %q) = %v, want %v", c.path, c.ep.node, got, c.want)
		}
	}
}

func TestIsLogin(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodPost, "/api2/json/access/ticket", true},
		{http.MethodGet, "/api2/json/access/ticket", false},
		{http.MethodPut, "/api2/json/access/ticket", false},
		{http.MethodPost, "/api2/json/access/ticket/extra", false},
		{http.MethodPost, "/access/ticket", false},
		{http.MethodPost, "/api2/json/access/password", false},
	}
	for _, c := range cases {
		req := &http.Request{Method: c.method, URL: &url.URL{Path: c.path}}
		if got := isLogin(req); got != c.want {
			t.Errorf("isLogin(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}
