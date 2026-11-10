package ha

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// The tests in this file drive the rule writer and the replication surface
// through the real failover wrapper, with two pvetest servers and a client from
// pve.NewClientWithTracer. They never run in parallel, because a call's scope
// release clears the scopeless tethers of every pool in the process.

// wireLab holds the certificate authority and leaf the lab's servers share.
type wireLab struct {
	ca   *pvetest.CA
	leaf *pvetest.Leaf
}

func newWireLab(t *testing.T) *wireLab {
	t.Helper()
	ca := pvetest.NewCA(t)
	return &wireLab{ca: ca, leaf: ca.NewLeaf(t, pvetest.LeafOptions{})}
}

// client builds a CPI client over the servers whose first entry is the first
// server, verifying against the lab's CA.
func (l *wireLab) client(t *testing.T, servers ...*pvetest.Server) pve.Client {
	t.Helper()
	verify := true
	host, port := servers[0].HostPort()
	cfg := &config.CPIConfig{
		Host:         host,
		Port:         port,
		APIToken:     "root@pam!test=tok-ha-wire",
		VerifySSL:    &verify,
		PVECACertPEM: string(l.ca.CertPEM()),
	}
	for _, s := range servers {
		cfg.Endpoints = append(cfg.Endpoints, s.Addr)
	}
	c, err := pve.NewClientWithTracer(cfg, log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("NewClientWithTracer: %v", err)
	}
	return c
}

// hijackAndClose takes the connection and closes it with no answer, so the
// client sees a connection that dropped after it sent the request.
func hijackAndClose(w http.ResponseWriter, _ *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "the connection cannot be hijacked", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
}

// mutationsSeen lists, as "METHOD path", every request the server read other
// than a read and the login.
func mutationsSeen(s *pvetest.Server) []string {
	var out []string
	for _, r := range s.Seen() {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			continue
		}
		if r.Path == pvetest.APIPrefix+"/access/ticket" {
			continue
		}
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

// readsSeen counts the GETs the server read on the API-relative path.
func readsSeen(s *pvetest.Server, path string) int {
	n := 0
	for _, r := range s.Seen() {
		if r.Method == http.MethodGet && r.Path == pvetest.APIPrefix+path {
			n++
		}
	}
	return n
}

// requireOnlyOneMutation fails the test unless the server read exactly one
// mutation, which is the one that opened the tether.
func requireOnlyOneMutation(t *testing.T, name string, s *pvetest.Server) {
	t.Helper()
	if got := mutationsSeen(s); len(got) != 1 {
		t.Errorf("%s read %d mutations, want only the first: %v", name, len(got), got)
	}
}

// requireNoMutations fails the test when the server read any mutation.
func requireNoMutations(t *testing.T, name string, s *pvetest.Server) {
	t.Helper()
	if got := mutationsSeen(s); len(got) != 0 {
		t.Errorf("%s read %d mutations, want none: %v", name, len(got), got)
	}
}

// requireTetheredRefusal fails the test unless err is a walk that tried A alone.
func requireTetheredRefusal(t *testing.T, what string, err error, addr string) {
	t.Helper()
	var walk *endpointpool.EntryUnreachableError
	if !errors.As(err, &walk) || !slices.Equal(walk.Endpoints(), []string{addr}) {
		t.Fatalf("%s: error = %v, want a walk that tried only %s", what, err, addr)
	}
}

// closeAfterFirstWrite closes the first server once the first HA rule write
// returns, so every later request sees that server refuse its dial while the
// second server stays healthy.
type closeAfterFirstWrite struct {
	RuleAPI
	closeFirst func()
	once       sync.Once
	sends      atomic.Int32
}

func (c *closeAfterFirstWrite) afterWrite() {
	c.sends.Add(1)
	c.once.Do(c.closeFirst)
}

func (c *closeAfterFirstWrite) CreateHaRules(ctx context.Context, p *sdkcluster.CreateHaRulesParams) error {
	err := c.RuleAPI.CreateHaRules(ctx, p)
	c.afterWrite()
	return err
}

func (c *closeAfterFirstWrite) UpdateHaRules(ctx context.Context, rule string, p *sdkcluster.UpdateHaRulesParams) error {
	err := c.RuleAPI.UpdateHaRules(ctx, rule, p)
	c.afterWrite()
	return err
}

func (c *closeAfterFirstWrite) DeleteHaRules(ctx context.Context, rule string) error {
	err := c.RuleAPI.DeleteHaRules(ctx, rule)
	c.afterWrite()
	return err
}

// ruleListRoute serves a rule list holding r2 only, so a re-read never shows
// the rule a write meant to create or update and always shows r2, which a
// delete of r2 therefore did not remove.
func ruleListRoute(routes *pvetest.Routes) {
	routes.JSON(http.MethodGet, "/cluster/ha/rules", http.StatusOK, []any{map[string]any{
		"rule":      "r2",
		"type":      "resource-affinity",
		"affinity":  "negative",
		"resources": "vm:100,vm:101",
		"digest":    "digest-one",
	}})
}

// planSequence returns a planner that plans, in turn, a create of r1, an update
// of r1, a delete of r2, a create of r3, and an update of r1, and the same
// again after those. Each is a write the lab's re-read never shows, so the
// writer re-plans after every one of them.
func planSequence() (Planner, *atomic.Int32) {
	var round atomic.Int32
	r1 := raRuleFor("r1", "vm:100", "vm:101")
	r2 := raRuleFor("r2", "vm:100", "vm:101")
	r3 := raRuleFor("r3", "vm:100", "vm:102")
	steps := []RuleOp{
		{Kind: OpCreate, Rule: r1},
		{Kind: OpUpdate, Rule: r1},
		{Kind: OpDelete, Rule: r2},
		{Kind: OpCreate, Rule: r3},
		{Kind: OpUpdate, Rule: r1},
	}
	return func(*RuleSet) ([]RuleOp, error) {
		n := int(round.Add(1)) - 1
		return []RuleOp{steps[n%len(steps)]}, nil
	}, &round
}

func raRuleFor(id string, sids ...string) Rule {
	return Rule{ID: id, Type: RuleTypeResourceAffinity, Affinity: AffinityNegative, Resources: sids}
}

// A rule write held by the first server never reaches the second. The writer
// makes five sends, which are the first write and four re-sends, and then ends
// with a retriable error. With the first server closed after the first write,
// the writer re-reads, finds the rule missing, and sends again. Every re-send
// is another HA mutation of a different method and path, and every one of them
// goes to the first server alone, so the first server reads only the first
// write. The re-reads may reach the second server, and do.
func TestRuleWriterFirstWriteAndFourResendsNeverReachTheOtherServer(t *testing.T) {
	cases := []struct {
		name string
		// failures makes the first server answer each rule write the way the case says.
		failures func(routes *pvetest.Routes)
	}{
		{"connection dropped after the request", func(routes *pvetest.Routes) {
			for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
				routes.Handle(m, "/cluster/", http.HandlerFunc(hijackAndClose))
			}
		}},
		{"500", func(routes *pvetest.Routes) {
			for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
				routes.Fail(m, "/cluster/", http.StatusInternalServerError, "internal error")
			}
		}},
		{"503", func(routes *pvetest.Routes) {
			for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
				routes.Fail(m, "/cluster/", http.StatusServiceUnavailable, "service unavailable")
			}
		}},
		{"596", func(routes *pvetest.Routes) {
			for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
				routes.Fail(m, "/cluster/", 596, "connection timed out")
			}
		}},
		{"a create answered as already defined", func(routes *pvetest.Routes) {
			for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
				routes.Fail(m, "/cluster/", http.StatusBadRequest, "rule already defined")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newWireLab(t)
			aRoutes, bRoutes := pvetest.NewRoutes(), pvetest.NewRoutes()
			ruleListRoute(aRoutes)
			ruleListRoute(bRoutes)
			tc.failures(aRoutes)
			a, b := pvetest.NewLive(t, l.leaf, aRoutes), pvetest.NewLive(t, l.leaf, bRoutes)
			api := &closeAfterFirstWrite{RuleAPI: l.client(t, a, b).Cluster(), closeFirst: a.Close}
			w := &RuleWriter{API: api, Owned: func(*Rule) bool { return false }}
			plan, rounds := planSequence()
			ctx, release := endpointpool.WithAffinityScope(t.Context())
			defer release()

			_, err := w.Apply(ctx, "tether", plan)

			if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("Apply error = %v, want a retriable cloud error after %d held writes", err, RuleWriterMaxAttempts)
			}
			if got := int(api.sends.Load()); got != RuleWriterMaxAttempts {
				t.Errorf("the writer sent %d rule writes, want %d", got, RuleWriterMaxAttempts)
			}
			if got := int(rounds.Load()); got != RuleWriterMaxAttempts {
				t.Errorf("the planner ran %d times, want %d", got, RuleWriterMaxAttempts)
			}
			requireOnlyOneMutation(t, "the first server", a)
			requireNoMutations(t, "the second server", b)
			if got := readsSeen(b, "/cluster/ha/rules"); got == 0 {
				t.Errorf("the second server read no re-reads of the rules, so the lab did not fail them over")
			}
		})
	}
}

// replicationLab starts a first server that drops the first replication write
// after reading it, and a healthy second server, and returns the replication
// surface over a client for both.
func replicationLab(t *testing.T) (api ReplicationAPI, a, b *pvetest.Server) {
	t.Helper()
	l := newWireLab(t)
	aRoutes := pvetest.NewRoutes()
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		aRoutes.Handle(m, "/cluster/", http.HandlerFunc(hijackAndClose))
	}
	a, b = pvetest.NewLive(t, l.leaf, aRoutes), pvetest.NewLive(t, l.leaf, nil)
	return NewReplicationAPI(l.client(t, a, b)), a, b
}

// A replication job update or delete held by the first server tethers the
// whole replication family to it. The other writes of the family, whatever
// their method or path, go to the first server alone and fail at its closed
// listener, and the second server reads none of them. A schedule-now request is
// outside the family and is free to reach the second server.
func TestReplicationWritesStayOnTheServerThatHeldTheFirst(t *testing.T) {
	cases := []struct {
		name    string
		first   func(ctx context.Context, api ReplicationAPI) error
		follows []func(ctx context.Context, api ReplicationAPI) error
	}{
		{
			name: "an update first",
			first: func(ctx context.Context, api ReplicationAPI) error {
				return api.SetComment(ctx, "100-0", "held")
			},
			follows: []func(ctx context.Context, api ReplicationAPI) error{
				func(ctx context.Context, api ReplicationAPI) error { return api.DeleteJob(ctx, "100-0", false) },
				func(ctx context.Context, api ReplicationAPI) error { return api.DeleteJob(ctx, "100-0", true) },
				func(ctx context.Context, api ReplicationAPI) error {
					return api.CreateJob(ctx, ReplicaJobSpec{ID: "101-0", Target: "pve2", Schedule: "*/15"})
				},
				func(ctx context.Context, api ReplicationAPI) error { return api.SetComment(ctx, "100-0", "again") },
			},
		},
		{
			name: "a delete first",
			first: func(ctx context.Context, api ReplicationAPI) error {
				return api.DeleteJob(ctx, "100-0", false)
			},
			follows: []func(ctx context.Context, api ReplicationAPI) error{
				func(ctx context.Context, api ReplicationAPI) error { return api.SetComment(ctx, "100-0", "after") },
				func(ctx context.Context, api ReplicationAPI) error { return api.DeleteJob(ctx, "100-0", true) },
				func(ctx context.Context, api ReplicationAPI) error {
					return api.CreateJob(ctx, ReplicaJobSpec{ID: "100-0", Target: "pve2"})
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, a, b := replicationLab(t)
			ctx, release := endpointpool.WithAffinityScope(t.Context())
			defer release()

			if err := tc.first(ctx, api); err == nil || endpointpool.IsConnectPhase(err) {
				t.Fatalf("the first write: error = %v, want a failure that may have reached the first server", err)
			}
			a.Close()
			for _, follow := range tc.follows {
				requireTetheredRefusal(t, "a later write", follow(ctx, api), a.Addr)
			}
			requireOnlyOneMutation(t, "the first server", a)
			requireNoMutations(t, "the second server", b)

			if err := api.ScheduleNow(ctx, "pve1", "100-0"); err != nil {
				t.Fatalf("ScheduleNow, which is outside the replication family: %v", err)
			}
			if got := mutationsSeen(b); len(got) != 1 {
				t.Errorf("the second server read %d mutations, want only the schedule-now: %v", len(got), got)
			}
		})
	}
}
