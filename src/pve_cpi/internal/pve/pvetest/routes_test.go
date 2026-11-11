package pvetest_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

func serve(r *pvetest.Routes, method, path string) (int, string) {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader("")))
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, strings.TrimSpace(string(body))
}

func TestRoutes_Defaults(t *testing.T) {
	t.Parallel()
	r := pvetest.NewRoutes()
	cases := []struct {
		method, path string
		code         int
		body         string
	}{
		{
			http.MethodGet,
			pvetest.APIPrefix + "/version",
			200,
			`{"data":{"release":"9.0","repoid":"pvetest","version":"9.0.2"}}`,
		},
		{http.MethodGet, pvetest.APIPrefix + "/nodes", 200, `{"data":[]}`},
		{http.MethodGet, pvetest.APIPrefix + "/cluster/resources", 200, `{"data":[]}`},
		{http.MethodGet, pvetest.APIPrefix + "/nodes/pve1/qemu", 200, `{"data":[]}`},
		{http.MethodPost, pvetest.APIPrefix + "/pools", 200, `{"data":null}`},
		{http.MethodGet, "/other", 404, "404 page not found"},
		{http.MethodGet, pvetest.APIPrefix + "x/version", 404, "404 page not found"},
	}
	for _, c := range cases {
		code, body := serve(r, c.method, c.path)
		if code != c.code || body != c.body {
			t.Errorf("%s %s = %d %q, want %d %q", c.method, c.path, code, body, c.code, c.body)
		}
	}
}

func TestRoutes_PrecedenceAndReplacement(t *testing.T) {
	t.Parallel()
	r := pvetest.NewRoutes()
	r.JSON("", "/nodes/", 200, "any-prefix")
	r.JSON(http.MethodGet, "/nodes/pve1/", 200, "get-prefix")
	r.JSON(http.MethodGet, "/nodes/pve1/status", 200, "exact")
	r.JSON("", "/nodes/pve1/status", 200, "exact-any")

	checks := []struct{ method, path, want string }{
		{http.MethodGet, "/nodes/pve1/status", `{"data":"exact"}`},
		{http.MethodPost, "/nodes/pve1/status", `{"data":"exact-any"}`},
		{http.MethodGet, "/nodes/pve1/qemu", `{"data":"get-prefix"}`},
		{http.MethodPost, "/nodes/pve1/qemu", `{"data":"any-prefix"}`},
		{http.MethodGet, "/nodes/pve2/qemu", `{"data":"any-prefix"}`},
	}
	for _, c := range checks {
		if _, body := serve(r, c.method, pvetest.APIPrefix+c.path); body != c.want {
			t.Errorf("%s %s = %q, want %q", c.method, c.path, body, c.want)
		}
	}

	r.JSON(http.MethodGet, "/nodes/pve1/status", 200, "replaced")
	if _, body := serve(r, http.MethodGet, pvetest.APIPrefix+"/nodes/pve1/status"); body != `{"data":"replaced"}` {
		t.Errorf("registering a route again must replace it, got %q", body)
	}
}

func TestRoutes_FailAndHandleFunc(t *testing.T) {
	t.Parallel()
	r := pvetest.NewRoutes()
	r.Fail(http.MethodPost, "/pools", 596, "Connection timed out")
	code, body := serve(r, http.MethodPost, pvetest.APIPrefix+"/pools")
	if code != 596 || body != `{"data":null,"message":"Connection timed out"}` {
		t.Errorf("Fail route = %d %q", code, body)
	}

	r.HandleFunc(http.MethodPut, "/custom", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	if code, _ := serve(r, http.MethodPut, pvetest.APIPrefix+"/custom"); code != http.StatusTeapot {
		t.Errorf("HandleFunc route = %d, want 418", code)
	}
}

func TestRoutes_FailRejectsStatusesThatCannotFail(t *testing.T) {
	t.Parallel()
	r := pvetest.NewRoutes()
	for _, status := range []int{-1, 0, 100, 101, 199, 1000} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Fail with status %d must panic", status)
				}
			}()
			r.Fail(http.MethodGet, "/x", status, "no")
		}()
	}
	r.Fail(http.MethodGet, "/x", 200, "edge")
	r.Fail(http.MethodGet, "/y", 999, "edge")
}

func TestRoutes_FailStatusesReachARealClient(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	routes := pvetest.NewRoutes()
	statuses := []int{502, 503, 595, 596, 597, 599}
	for _, status := range statuses {
		routes.Fail(http.MethodGet, "/status/"+strconv.Itoa(status), status, "backend trouble")
	}
	s := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), routes)
	c := client(ca, false, 0)

	for _, status := range statuses {
		resp, err := get(c, s.URL+pvetest.APIPrefix+"/status/"+strconv.Itoa(status))
		if err != nil {
			t.Fatalf("GET %d: %v", status, err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != status || body != `{"data":null,"message":"backend trouble"}` {
			t.Errorf("status %d answered %d %q", status, resp.StatusCode, body)
		}
	}
}
