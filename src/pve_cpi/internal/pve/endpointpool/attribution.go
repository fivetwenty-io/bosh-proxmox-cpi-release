package endpointpool

import (
	"net/http"
	"strings"
)

// loginRoute is the API route of the ticket login. The login is the one
// non-idempotent request the walk treats like a read, because it creates no
// state on PVE.
const loginRoute = "/access/ticket"

// apiRoute returns the API route of a request's URL path, which is the path
// after APIPrefix, such as "/nodes/pve1/qemu" for "/api2/json/nodes/pve1/qemu".
// It reports false for a path outside the API prefix. The SDK sends no such
// request, so nothing about one is attributed to an endpoint.
func apiRoute(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, APIPrefix+"/")
	if !ok {
		return "", false
	}
	return "/" + rest, true
}

// routeNode returns the node an API route names and whether the route is
// node-scoped. A route under "/nodes/" is node-scoped, and its next segment
// is the node, which is empty for a malformed route such as "/nodes/".
// "/nodes" itself lists the nodes and names none.
func routeNode(route string) (node string, scoped bool) {
	rest, ok := strings.CutPrefix(route, "/nodes/")
	if !ok {
		return "", false
	}
	node, _, _ = strings.Cut(rest, "/")
	return node, true
}

// attributable reports whether a stall on the request path, or a 502, 503, or
// 595 answer for it, can be blamed on the entry endpoint ep itself. That is so
// when the route is not node-scoped, such as /cluster/resources or
// /access/ticket, or when its node segment names ep's own node. The pool knows
// an endpoint's node only when discovery recorded it, so without that only the
// routes with no node segment count. The comparison is exact, which never
// blames an entry for a node it may not be.
func attributable(path string, ep *endpoint) bool {
	route, ok := apiRoute(path)
	if !ok {
		return false
	}
	node, scoped := routeNode(route)
	if !scoped {
		return true
	}
	return node != "" && ep.node != "" && node == ep.node
}

// isLogin reports whether req is the ticket login, POST /access/ticket under
// the API prefix.
func isLogin(req *http.Request) bool {
	if req.Method != http.MethodPost {
		return false
	}
	route, ok := apiRoute(req.URL.Path)
	return ok && route == loginRoute
}
