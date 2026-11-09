package pvetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// APIPrefix is the path every PVE API route lives under. Route paths given to
// Routes are relative to it, so "/version" serves "/api2/json/version".
const APIPrefix = "/api2/json"

// maxStatus is the largest status code net/http writes.
const maxStatus = 999

type routeKey struct {
	method string
	path   string
}

// Routes is a table of fake PVE API routes. A path that ends in "/" matches
// every path below it, and the longest match wins. A route registered for the
// empty method answers any method, and a route for the exact method beats it.
// Registering a route again replaces the earlier one. Routes is safe for
// concurrent use.
type Routes struct {
	mu      sync.RWMutex
	entries map[routeKey]http.Handler
}

// NewRoutes returns a table with the routes most tests need. GET /version
// reports a PVE 9 release, GET /nodes and GET /cluster/resources return empty
// lists, POST /access/ticket answers a login with a fresh ticket, a CSRF token,
// and the user name, and every other path under the API prefix answers a PVE
// envelope whose data is an empty list for GET and null for every other method.
func NewRoutes() *Routes {
	r := &Routes{entries: make(map[routeKey]http.Handler)}
	r.JSON(http.MethodGet, "/version", http.StatusOK, map[string]string{
		"version": "9.0.2",
		"release": "9.0",
		"repoid":  "pvetest",
	})
	r.JSON(http.MethodGet, "/nodes", http.StatusOK, []any{})
	r.JSON(http.MethodGet, "/cluster/resources", http.StatusOK, []any{})
	r.HandleFunc(http.MethodPost, "/access/ticket", ticketHandler)
	r.JSON(http.MethodGet, "/", http.StatusOK, []any{})
	r.JSON("", "/", http.StatusOK, nil)
	return r
}

// ticketHandler answers a password login the way pveproxy does. The ticket's
// time stamp is the current time, so a client that reads it does not think the
// ticket has already expired.
func ticketHandler(w http.ResponseWriter, req *http.Request) {
	user := "root@pam"
	if err := req.ParseForm(); err == nil && req.PostForm.Get("username") != "" {
		user = req.PostForm.Get("username")
		if !strings.Contains(user, "@") {
			realm := req.PostForm.Get("realm")
			if realm == "" {
				realm = "pam"
			}
			user += "@" + realm
		}
	}
	stamp := fmt.Sprintf("%08X", time.Now().Unix())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
		"ticket":              "PVE:" + user + ":" + stamp + "::pvetest",
		"CSRFPreventionToken": stamp + ":pvetest",
		"username":            user,
	}})
}

// Handle registers h for the method and the API-relative path.
func (r *Routes) Handle(method, path string, h http.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[routeKey{method: method, path: path}] = h
}

// HandleFunc registers a handler function for the method and path.
func (r *Routes) HandleFunc(method, path string, f http.HandlerFunc) {
	r.Handle(method, path, f)
}

// JSON registers a route that answers status with the PVE envelope {"data": data}.
func (r *Routes) JSON(method, path string, status int, data any) {
	body, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		panic("pvetest: marshal route data: " + err.Error())
	}
	r.Handle(method, path, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
}

// Fail registers a route that answers status with an empty data envelope and
// reason as its message. Any status from 200 to 999 works, so Fail serves the
// 502, 503, 595, and 596 codes that pveproxy uses for backend trouble. It
// panics on a status below 200, because Go's server sends a 1xx other than 101
// as an interim response and then the real 200, so the route would not fail.
func (r *Routes) Fail(method, path string, status int, reason string) {
	if status < http.StatusOK || status > maxStatus {
		panic(fmt.Sprintf("pvetest: Fail status %d is outside 200 to %d", status, maxStatus))
	}
	body, err := json.Marshal(map[string]any{"data": nil, "message": reason})
	if err != nil {
		panic("pvetest: marshal failure message: " + err.Error())
	}
	r.Handle(method, path, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
}

func (r *Routes) lookup(method, path string) http.Handler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if h, ok := r.entries[routeKey{method: method, path: path}]; ok {
		return h
	}
	if h, ok := r.entries[routeKey{path: path}]; ok {
		return h
	}
	for p := path; p != ""; {
		cut := strings.LastIndex(strings.TrimSuffix(p, "/"), "/")
		if cut < 0 {
			break
		}
		p = p[:cut+1]
		if h, ok := r.entries[routeKey{method: method, path: p}]; ok {
			return h
		}
		if h, ok := r.entries[routeKey{path: p}]; ok {
			return h
		}
	}
	return nil
}

// ServeHTTP serves the route for the request, or answers 404 when the path is
// outside the API prefix or no route matches.
func (r *Routes) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	rel, ok := strings.CutPrefix(req.URL.Path, APIPrefix)
	if !ok || (rel != "" && rel[0] != '/') {
		http.NotFound(w, req)
		return
	}
	if rel == "" {
		rel = "/"
	}
	h := r.lookup(req.Method, rel)
	if h == nil {
		http.NotFound(w, req)
		return
	}
	h.ServeHTTP(w, req)
}
