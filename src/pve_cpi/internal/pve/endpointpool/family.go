package endpointpool

import (
	"net/http"
	"path"
	"strings"
)

// family names a group of mutations that share one tether, whatever their
// method or path. A few writers re-plan after an unknown outcome, and the
// re-plan can change the method or the path of what is logically the same
// write. Without a family, the re-planned request would carry a new key and
// could reach another endpoint while the first write might still be running.
type family string

// The tether families.
const (
	// familyHA covers every mutation under /cluster/ha: HA resources, rules,
	// and groups. The rule writer re-plans a create into an update after a
	// held write, and staging, the narrow barrier, and SetHAState settle an
	// error with a read and then write again.
	familyHA family = "ha"
	// familyReplication covers every mutation under /cluster/replication. The
	// replication writers create a job, set its comment, and delete it first
	// without and then with force.
	familyReplication family = "replication"
	// familyPools covers every mutation under /pools. Lock sentinels are
	// pools, and a sentinel create that may have landed can be followed by a
	// cleanup delete of the same sentinel, which differs in method.
	familyPools family = "pools"
)

// familyRoutes maps each family to the API route it covers. A route belongs to
// a family when it is the family's route itself or lies below it, so
// "/cluster/ha" and "/cluster/ha/rules/x" are in the ha family, and
// "/cluster/hardware" is not.
var familyRoutes = []struct {
	name  family
	route string
}{
	{familyHA, "/cluster/ha"},
	{familyReplication, "/cluster/replication"},
	{familyPools, "/pools"},
}

// tetherKey identifies the mutations that share one tether. A mutation in a
// family carries only the family's name, and every other mutation carries its
// method and its API route, without the query string. So a POST to the same
// path with a different query shares the tether, and a re-planned HA write
// shares the tether of the HA write before it.
type tetherKey struct {
	family family
	method string
	route  string
}

// keyRoute returns the route a tether key and its log lines use for a request
// path. It is the API route after APIPrefix, or the whole path for one outside
// the prefix, cleaned so that a trailing slash or a doubled slash names the
// same route. Cleaning only ever merges keys, which keeps more requests on
// one endpoint and never fewer.
func keyRoute(p string) string {
	route, ok := apiRoute(p)
	if !ok {
		route = p
	}
	return path.Clean("/" + route)
}

// familyOf returns the family a cleaned API route belongs to, or "" when it
// belongs to none.
func familyOf(route string) family {
	for _, f := range familyRoutes {
		if route == f.route || strings.HasPrefix(route, f.route+"/") {
			return f.name
		}
	}
	return ""
}

// tetherKeyOf returns the tether key of a mutation. The method is compared in
// upper case, as the pool's send-by markers compare it. Only a mutation, which
// classOf names, looks up, reserves, or fixes a tether, and reads and the
// ticket login never do.
func tetherKeyOf(req *http.Request) tetherKey {
	route := keyRoute(req.URL.Path)
	if f := familyOf(route); f != "" {
		return tetherKey{family: f}
	}
	return tetherKey{method: strings.ToUpper(methodOf(req)), route: route}
}
