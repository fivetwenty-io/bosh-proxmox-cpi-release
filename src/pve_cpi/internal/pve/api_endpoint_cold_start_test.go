package pve

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// coldTestSeed is the first configured endpoint of the unit tests, whose
// domain every derived candidate takes.
const coldTestSeed = "pve1.dc1.example.com:8006"

// coldTestJob is a job-level config that meets every condition of the cold
// start and names az_map. The tests change one thing at a time.
func coldTestJob(azMap map[string][]string) *config.CPIConfig {
	verify := true
	return &config.CPIConfig{
		Host:                 "pve1.dc1.example.com",
		Port:                 8006,
		APIToken:             cacheTestToken,
		APIEndpointDiscovery: config.DiscoveryModeOn,
		VerifySSL:            &verify,
		PVECACertPEM:         "-----BEGIN CERTIFICATE-----\nplaceholder\n-----END CERTIFICATE-----\n",
		Placement:            &config.PlacementConfig{AZMap: azMap},
	}
}

func candidateNodes(cs []endpointpool.Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Node)
	}
	return out
}

func TestColdStartNamesKeepsAZOrderAndLeavesOutNamesTheHelperRefuses(t *testing.T) {
	t.Parallel()
	job := coldTestJob(map[string][]string{
		"az2": {"pve3", "pve2.example.com", "localhost", "12345", "0xab", "-pve", "pve4"},
		"az1": {"pve1", "pve2", "pve3"},
	})
	logger, obs := log.NewObservedLogger(log.LevelDebug)

	got := coldStartNames(job, job, logger)

	if want := []string{"pve1", "pve2", "pve3", "pve4"}; !slices.Equal(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	for _, refused := range []string{"pve2.example.com", "localhost", "12345", "0xab", "-pve"} {
		found := false
		for _, e := range obs.All() {
			if e.Level == log.LevelDebug && e.Attrs["node"] == refused {
				found = true
			}
		}
		if !found {
			t.Errorf("no Debug line names the refused %q", refused)
		}
	}
	if e, bad := anyLineAbove(obs.All(), log.LevelDebug); bad {
		t.Errorf("a refused name logged %q at %v, want Debug only", e.Message, e.Level)
	}
}

func TestColdStartNamesOfAMapWithOnlyInvalidNamesAreEmpty(t *testing.T) {
	t.Parallel()
	job := coldTestJob(map[string][]string{"az1": {"pve1.example.com", "localhost", "999"}})

	if got := coldStartNames(job, job, log.NewNopLogger()); len(got) != 0 {
		t.Fatalf("names = %v, want none, so the gate never sees a list of refused names", got)
	}
}

func TestColdStartCandidatesTakeTheSeedsDomainAndLeaveOutDuplicates(t *testing.T) {
	t.Parallel()
	in := coldStartInputs{
		names:      []string{"pve1", "pve2", "pve3", "pve2"},
		seed:       coldTestSeed,
		configured: []string{coldTestSeed, "PVE3.dc1.example.com:8006"},
	}
	logger, obs := log.NewObservedLogger(log.LevelDebug)

	got := coldStartCandidates(in, logger)

	want := []endpointpool.Candidate{{Node: "pve2", Address: "pve2.dc1.example.com:8006"}}
	if !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	if lines := linesAt(obs.All(), log.LevelDebug, "configured endpoint or another node's"); len(lines) != 3 {
		t.Errorf("Debug lines for left-out nodes = %v, want one for each of the three repeats", lines)
	}
}

func TestColdStartCandidatesPutTheHintedNodeFirstAndKeepTheRestInOrder(t *testing.T) {
	t.Parallel()
	in := coldStartInputs{names: []string{"pve2", "pve3", "pve4", "pve5"}, seed: coldTestSeed, hint: "pve4"}

	got := candidateNodes(coldStartCandidates(in, log.NewNopLogger()))

	if want := []string{"pve4", "pve2", "pve3", "pve5"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestColdStartCandidatesIgnoreAHintThatNamesNoCandidate(t *testing.T) {
	t.Parallel()
	in := coldStartInputs{names: []string{"pve2", "pve3"}, seed: coldTestSeed, hint: "pve9"}

	got := candidateNodes(coldStartCandidates(in, log.NewNopLogger()))

	if want := []string{"pve2", "pve3"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestColdStartCandidatesAreCappedAtTheLimitWithTheHintStillFirst(t *testing.T) {
	t.Parallel()
	names := make([]string, 0, 100)
	for i := range 100 {
		names = append(names, fmt.Sprintf("n%03d", i))
	}
	in := coldStartInputs{names: names, seed: coldTestSeed, hint: "n090"}
	logger, obs := log.NewObservedLogger(log.LevelDebug)

	got := candidateNodes(coldStartCandidates(in, logger))

	if len(got) != coldStartCandidateLimit {
		t.Fatalf("%d candidates, want %d", len(got), coldStartCandidateLimit)
	}
	if got[0] != "n090" {
		t.Errorf("first = %q, want the hinted node", got[0])
	}
	if slices.Contains(got, "n099") {
		t.Error("the cap kept the last node, want it cut")
	}
	if len(linesAt(obs.All(), log.LevelDebug, "keeps the first 64 of 100")) != 1 {
		t.Errorf("the cap logged %v, want one Debug line", obs.All())
	}
}

func TestColdStartCandidatesRefuseASeedTheDerivationRefuses(t *testing.T) {
	t.Parallel()
	for _, seed := range []string{"pve1.com:8006", "pve1.co.uk:8006", "pve1:", "", "pve1.example.123:8006"} {
		in := coldStartInputs{names: []string{"pve2"}, seed: seed}
		if got := coldStartCandidates(in, log.NewNopLogger()); len(got) != 0 {
			t.Errorf("seed %q gave candidates %v, want none", seed, got)
		}
	}
}

func TestColdStartCandidatesOfABareSeedKeepBareNames(t *testing.T) {
	t.Parallel()
	in := coldStartInputs{names: []string{"pve2"}, seed: "pve1:8006", configured: []string{"pve1:8006"}}

	got := coldStartCandidates(in, log.NewNopLogger())

	if want := []endpointpool.Candidate{{Node: "pve2", Address: "pve2:8006"}}; !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

func TestAttachColdStartGate(t *testing.T) {
	t.Parallel()
	discovered := []endpointpool.Endpoint{{Address: "pve9.dc1.example.com:8006", Kind: endpointpool.KindDiscovered}}
	cases := []struct {
		name       string
		names      []string
		discovered []endpointpool.Endpoint
		outcome    string
		want       bool
	}{
		{"no cached file", []string{"pve2"}, nil, "", true},
		{"outcome ok with no node left", []string{"pve2"}, nil, "ok", true},
		{"no_status", []string{"pve2"}, nil, "no_status", true},
		{"no_candidates", []string{"pve2"}, nil, "no_candidates", true},
		{"public_suffix", []string{"pve2"}, nil, "public_suffix", true},
		{"a usable discovered entry", []string{"pve2"}, discovered, "ok", false},
		{"a standalone outcome", []string{"pve2"}, nil, "standalone", false},
		{"an outcome outside the enumeration", []string{"pve2"}, nil, "sideways", false},
		{"no names", nil, nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cache := &endpointCache{
				logger:     log.NewNopLogger(),
				mode:       config.DiscoveryModeOn,
				discovered: tc.discovered,
				cold:       coldStartInputs{names: tc.names, seed: coldTestSeed, outcome: tc.outcome},
			}
			var cfg endpointpool.Config

			cache.attach(&cfg)

			if got := len(cfg.Candidates) > 0 && cfg.Prober != nil; got != tc.want {
				t.Fatalf("candidates %v, prober %v; want carried = %v", cfg.Candidates, cfg.Prober, tc.want)
			}
			if !tc.want && (cfg.Candidates != nil || cfg.Prober != nil) {
				t.Errorf("a pool that does not carry the cold start got candidates %v and prober %v",
					cfg.Candidates, cfg.Prober)
			}
		})
	}
}

func TestAttachColdStartOfANilCacheCarriesNothing(t *testing.T) {
	t.Parallel()
	var cache *endpointCache
	var cfg endpointpool.Config

	cache.attach(&cfg)

	if cfg.Candidates != nil || cfg.Prober != nil || cfg.MarkSink != nil {
		t.Fatalf("a call with no usable cache directory got %+v, want no cold start", cfg)
	}
}

// statusTransport answers every probe with fn.
type statusTransport struct {
	fn   func(*http.Request) (*http.Response, error)
	reqs []*http.Request
}

func (s *statusTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.reqs = append(s.reqs, req)
	return s.fn(req)
}

func answer(status int, body string) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
}

// clusterStatus renders a /cluster/status answer whose node rows are names and
// whose local row is local.
func clusterStatus(local string, names ...string) string {
	rows := make([]string, 0, 1+len(names))
	rows = append(rows, `{"type":"cluster","name":"lab","nodes":3,"quorate":1}`)
	for _, name := range names {
		isLocal := 0
		if name == local {
			isLocal = 1
		}
		rows = append(rows, fmt.Sprintf(`{"type":"node","name":%q,"local":%d,"online":1}`, name, isLocal))
	}
	return `{"data":[` + strings.Join(rows, ",") + `]}`
}

func newTestProber(level slog.Level, names ...string) (*coldStartProber, *log.Observer, *[]string) {
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	var accepted []string
	return &coldStartProber{
		names:     names,
		userAgent: "bosh-proxmox-cpi/test",
		level:     level,
		logger:    logger,
		accepted:  func(node string) { accepted = append(accepted, node) },
	}, obs, &accepted
}

func probeRequest(node string, tr http.RoundTripper) endpointpool.ProbeRequest {
	return endpointpool.ProbeRequest{
		Candidate:     endpointpool.Candidate{Node: node, Address: node + ".dc1.example.com:8006"},
		Authorization: "PVEAPIToken=root@pam!cpi=secret",
		Transport:     tr,
	}
}

func TestProbeAcceptsAMemberAndSendsOneTokenGET(t *testing.T) {
	p, obs, accepted := newTestProber(log.LevelDebug, "pve1", "pve2", "pve3")
	tr := &statusTransport{fn: answer(http.StatusOK, clusterStatus("pve2", "pve1", "pve2", "pve3", "pve4"))}

	if err := p.Probe(context.Background(), probeRequest("pve2", tr)); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	if len(tr.reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(tr.reqs))
	}
	req := tr.reqs[0]
	if req.Method != http.MethodGet || req.URL.String() != "https://pve2.dc1.example.com:8006"+endpointpool.ProbePath {
		t.Errorf("request = %s %s, want the GET of %s", req.Method, req.URL, endpointpool.ProbePath)
	}
	if got := req.Header.Values("Authorization"); !slices.Equal(got, []string{"PVEAPIToken=root@pam!cpi=secret"}) {
		t.Errorf("Authorization = %v, want the request's token alone", got)
	}
	if req.Header.Get("User-Agent") != "bosh-proxmox-cpi/test" {
		t.Errorf("User-Agent = %q, want the client's", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("Cookie") != "" || req.Header.Get("CSRFPreventionToken") != "" {
		t.Errorf("the probe carries a ticket header: %v", req.Header)
	}
	if !slices.Equal(*accepted, []string{"pve2"}) {
		t.Errorf("accepted = %v, want the hint for pve2", *accepted)
	}
	if len(obs.All()) != 0 {
		t.Errorf("an accepted probe logged %v, want nothing (the pool logs the failover)", obs.All())
	}
}

func TestProbeRefusals(t *testing.T) {
	longBody := `{"data":[` + strings.Repeat(" ", coldStartProbeBodyLimit+1) + `]}`
	cases := []struct {
		name   string
		status int
		body   string
		want   error
		reason string
	}{
		{"a 403", http.StatusForbidden, `{"data":null}`, errColdStartStatus, "HTTP 403"},
		{"a redirect", http.StatusFound, ``, errColdStartStatus, "HTTP 302"},
		{"an informational status", http.StatusSwitchingProtocols, ``, errColdStartStatus, ""},
		{"a 500", http.StatusInternalServerError, `{}`, errColdStartStatus, "HTTP 500"},
		{"a different local node", http.StatusOK, clusterStatus("pve3", "pve1", "pve2", "pve3"), errColdStartLocal, ""},
		{"no local node", http.StatusOK, clusterStatus("", "pve1", "pve2", "pve3"), errColdStartLocal, ""},
		{"a node list missing a name", http.StatusOK, clusterStatus("pve2", "pve2", "pve3"), errColdStartMissing,
			"pve1"},
		{"a name that differs in case", http.StatusOK, clusterStatus("pve2", "PVE1", "pve2", "pve3"),
			errColdStartMissing, "pve1"},
		{"not JSON", http.StatusOK, `<html>`, errColdStartBody, ""},
		{"a null data member", http.StatusOK, `{"data":null}`, errColdStartBody, ""},
		{"no data member", http.StatusOK, `{}`, errColdStartBody, ""},
		{"a row that does not decode", http.StatusOK, `{"data":[{"type":"node","name":7,"local":1}]}`,
			errColdStartBody, ""},
		{"an empty list", http.StatusOK, `{"data":[]}`, errColdStartLocal, ""},
		{"a body over the limit", http.StatusOK, longBody, errColdStartBody, "longer than"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, obs, accepted := newTestProber(log.LevelDebug, "pve1", "pve2", "pve3")
			tr := &statusTransport{fn: answer(tc.status, tc.body)}

			err := p.Probe(context.Background(), probeRequest("pve2", tr))

			if !errors.Is(err, tc.want) {
				t.Fatalf("Probe = %v, want %v", err, tc.want)
			}
			if tc.reason != "" && !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("error %q lacks %q", err, tc.reason)
			}
			if len(*accepted) != 0 {
				t.Errorf("a refused candidate was accepted for a hint: %v", *accepted)
			}
			if len(linesAt(obs.All(), log.LevelDebug, "from placement.az_map refused")) != 1 {
				t.Errorf("log = %v, want one refusal line", obs.All())
			}
			for _, e := range obs.All() {
				if strings.Contains(e.Message, "secret") || strings.Contains(e.Message, "PVEAPIToken") {
					t.Errorf("the refusal line carries a credential: %q", e.Message)
				}
			}
		})
	}
}

func TestProbeRefusalsDoNotEchoTheAnswer(t *testing.T) {
	p, obs, _ := newTestProber(log.LevelDebug, "pve1", "pve2")
	tr := &statusTransport{fn: answer(http.StatusForbidden, `{"message":"send the token to evil.example"}`)}

	_ = p.Probe(context.Background(), probeRequest("pve2", tr))

	for _, e := range obs.All() {
		if strings.Contains(e.Message, "evil.example") {
			t.Fatalf("the refusal line repeats the candidate's answer: %q", e.Message)
		}
	}
}

func TestProbeRefusesATruncatedBody(t *testing.T) {
	p, _, accepted := newTestProber(log.LevelDebug, "pve2")
	body := io.MultiReader(strings.NewReader(clusterStatus("pve2", "pve2")), errReader{})
	tr := &statusTransport{fn: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body)}, nil
	}}

	err := p.Probe(context.Background(), probeRequest("pve2", tr))

	if !errors.Is(err, errColdStartBody) || len(*accepted) != 0 {
		t.Fatalf("Probe = %v, accepted %v, want a body refusal", err, *accepted)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestProbeRefusesABodyThatFailsToClose(t *testing.T) {
	p, _, accepted := newTestProber(log.LevelDebug, "pve2")
	tr := &statusTransport{fn: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: failingCloser{
			Reader: strings.NewReader(clusterStatus("pve2", "pve2")),
		}}, nil
	}}

	err := p.Probe(context.Background(), probeRequest("pve2", tr))

	if !errors.Is(err, errColdStartBody) || len(*accepted) != 0 {
		t.Fatalf("Probe = %v, accepted %v, want a refusal for the close", err, *accepted)
	}
}

type failingCloser struct{ io.Reader }

func (failingCloser) Close() error { return errors.New("close failed") }

func TestProbeClosesTheBodyOnEveryAnswer(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden} {
		p, _, _ := newTestProber(log.LevelDebug, "pve2")
		body := &closeCounter{Reader: strings.NewReader(clusterStatus("pve2", "pve2"))}
		tr := &statusTransport{fn: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: body}, nil
		}}

		_ = p.Probe(context.Background(), probeRequest("pve2", tr))

		if body.closed != 1 {
			t.Errorf("status %d: the body was closed %d times, want once before Probe returned", status, body.closed)
		}
	}
}

type closeCounter struct {
	io.Reader
	closed int
}

func (c *closeCounter) Close() error { c.closed++; return nil }

func TestProbeRefusesAMalformedRequestBeforeItSendsAnything(t *testing.T) {
	good := endpointpool.ProbeRequest{
		Candidate:     endpointpool.Candidate{Node: "pve2", Address: "pve2.dc1.example.com:8006"},
		Authorization: "PVEAPIToken=root@pam!cpi=secret",
	}
	cases := map[string]func(*endpointpool.ProbeRequest, *statusTransport){
		"no transport":     func(r *endpointpool.ProbeRequest, _ *statusTransport) { r.Transport = nil },
		"no authorization": func(r *endpointpool.ProbeRequest, _ *statusTransport) { r.Authorization = "" },
		"no address":       func(r *endpointpool.ProbeRequest, _ *statusTransport) { r.Candidate.Address = "" },
		"an address that is not a host": func(r *endpointpool.ProbeRequest, _ *statusTransport) {
			r.Candidate.Address = "pve2 evil.example:8006"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p, _, accepted := newTestProber(log.LevelDebug, "pve2")
			tr := &statusTransport{fn: answer(http.StatusOK, clusterStatus("pve2", "pve2"))}
			req := good
			req.Transport = tr
			mutate(&req, tr)

			err := p.Probe(context.Background(), req)

			if err == nil || len(tr.reqs) != 0 || len(*accepted) != 0 {
				t.Fatalf("Probe = %v with %d requests sent and accepted %v, want a refusal before any send",
					err, len(tr.reqs), *accepted)
			}
		})
	}
}

func TestProbeTransportFailureRefusesWithTheCertificateKindNamed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"a name mismatch", x509.HostnameError{Host: "pve2.dc1.example.com"}, "certificate is not valid for that name"},
		{"an unknown authority", x509.UnknownAuthorityError{}, "certificate does not chain to pve.ca_cert"},
		{"an expired certificate", x509.CertificateInvalidError{Reason: x509.Expired},
			"certificate is not valid"},
		{"a wrapped verification failure", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
			"certificate does not chain to pve.ca_cert"},
		{"a refused connection", errors.New("dial tcp 10.0.0.2:8006: connect: connection refused"),
			"the membership probe failed: dial tcp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, obs, accepted := newTestProber(log.LevelDebug, "pve2")
			tr := &statusTransport{fn: func(*http.Request) (*http.Response, error) { return nil, tc.err }}

			err := p.Probe(context.Background(), probeRequest("pve2", tr))

			if !errors.Is(err, tc.err) {
				t.Fatalf("Probe = %v, want the transport's error", err)
			}
			if lines := linesAt(obs.All(), log.LevelDebug, tc.want); len(lines) != 1 {
				t.Errorf("log = %v, want one refusal line containing %q", obs.All(), tc.want)
			}
			if len(*accepted) != 0 {
				t.Errorf("accepted = %v, want none", *accepted)
			}
		})
	}
}

func TestProbeRefusalLevelFollowsTheMode(t *testing.T) {
	cases := []struct {
		mode config.DiscoveryMode
		want slog.Level
	}{
		{config.DiscoveryModeAuto, log.LevelDebug},
		{config.DiscoveryModeOn, log.LevelWarn},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			p, obs, _ := newTestProber(endpointcache.Level(tc.mode), "pve2")
			tr := &statusTransport{fn: answer(http.StatusForbidden, ``)}

			_ = p.Probe(context.Background(), probeRequest("pve2", tr))

			if lines := linesAt(obs.All(), tc.want, "from placement.az_map refused"); len(lines) != 1 {
				t.Fatalf("log = %v, want one refusal at %v", obs.All(), tc.want)
			}
		})
	}
}

func TestProbeLogsEachRefusedCandidateOncePerProber(t *testing.T) {
	p, obs, _ := newTestProber(log.LevelWarn, "pve2", "pve3")
	tr := &statusTransport{fn: answer(http.StatusForbidden, ``)}

	for range 3 {
		_ = p.Probe(context.Background(), probeRequest("pve2", tr))
	}
	_ = p.Probe(context.Background(), probeRequest("pve3", tr))

	if lines := linesAt(obs.All(), log.LevelWarn, "refused"); len(lines) != 2 {
		t.Fatalf("refusal lines = %v, want one for each of the two candidates", lines)
	}
}

// Two clients in one process each have a prober, and each logs its own
// refusal at its own mode's level. A refusal one client logged at Debug under
// auto must not hide the Warn an explicit on client owes.
func TestProbeRefusalIsLoggedOncePerProberNotOncePerProcess(t *testing.T) {
	auto, autoObs, _ := newTestProber(log.LevelDebug, "pve2")
	explicit, explicitObs, _ := newTestProber(log.LevelWarn, "pve2")
	tr := &statusTransport{fn: answer(http.StatusForbidden, ``)}

	_ = auto.Probe(context.Background(), probeRequest("pve2", tr))
	_ = explicit.Probe(context.Background(), probeRequest("pve2", tr))
	_ = explicit.Probe(context.Background(), probeRequest("pve2", tr))

	if lines := linesAt(autoObs.All(), log.LevelDebug, "refused"); len(lines) != 1 {
		t.Errorf("the auto prober logged %v, want one Debug refusal", lines)
	}
	if lines := linesAt(explicitObs.All(), log.LevelWarn, "refused"); len(lines) != 1 {
		t.Errorf("the explicit prober logged %v, want one Warn refusal that the other prober did not hide", lines)
	}
}

func TestProbeThatEndedWithItsRequestLogsAtDebugAndDoesNotSpendTheRefusalLine(t *testing.T) {
	p, obs, accepted := newTestProber(log.LevelWarn, "pve2")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr := &statusTransport{fn: func(req *http.Request) (*http.Response, error) { return nil, req.Context().Err() }}

	err := p.Probe(ctx, probeRequest("pve2", tr))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Probe = %v, want the cancellation", err)
	}
	if lines := linesAt(obs.All(), log.LevelDebug, "ended with its request"); len(lines) != 1 {
		t.Errorf("log = %v, want one Debug line for the cut probe", obs.All())
	}
	if _, bad := anyLineAbove(obs.All(), log.LevelDebug); bad {
		t.Errorf("a probe cut short by its request logged above Debug: %v", obs.All())
	}
	// The same candidate, refused on a later request, still gets its line.
	tr.fn = answer(http.StatusForbidden, ``)
	_ = p.Probe(context.Background(), probeRequest("pve2", tr))
	if lines := linesAt(obs.All(), log.LevelWarn, "refused"); len(lines) != 1 {
		t.Errorf("log = %v, want the refusal line the cut probe left unspent", obs.All())
	}
	if len(*accepted) != 0 {
		t.Errorf("accepted = %v, want none", *accepted)
	}
}

func TestProbeThatEndedOnItsOwnLimitIsRefusedAndLogged(t *testing.T) {
	p, obs, _ := newTestProber(log.LevelWarn, "pve2")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("the probe's own limit"))
	tr := &statusTransport{fn: func(req *http.Request) (*http.Response, error) { return nil, req.Context().Err() }}

	_ = p.Probe(ctx, probeRequest("pve2", tr))

	if lines := linesAt(obs.All(), log.LevelWarn, "refused"); len(lines) != 1 {
		t.Fatalf("log = %v, want the probe's own timeout to refuse its candidate", obs.All())
	}
}

func TestRequestEnded(t *testing.T) {
	t.Parallel()
	live := context.Background()
	if requestEnded(live) {
		t.Error("a live context reported as ended")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if !requestEnded(cancelled) {
		t.Error("a cancelled request was not reported as ended")
	}
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer stop()
	if !requestEnded(expired) {
		t.Error("a request past its deadline was not reported as ended")
	}
	own, cancelOwn := context.WithCancelCause(context.Background())
	cancelOwn(errors.New("probe limit"))
	if requestEnded(own) {
		t.Error("a context that ended on the probe's own cause was reported as the request's")
	}
}

func TestCheckStatusBoundaries(t *testing.T) {
	t.Parallel()
	p := &coldStartProber{names: []string{"pve1", "pve2"}}
	twoLocals := `{"data":[{"type":"node","name":"pve1","local":1},{"type":"node","name":"pve2","local":1}]}`
	cases := []struct {
		name string
		body string
		node string
		want error
	}{
		{"two local rows", twoLocals, "pve1", errColdStartLocal},
		{"two local rows with the candidate's last", twoLocals, "pve2", errColdStartLocal},
		{"two local rows with the candidate's first and another after", `{"data":[` +
			`{"type":"node","name":"pve2","local":1},{"type":"node","name":"pve1","local":1}]}`, "pve2",
			errColdStartLocal},
		{"no local row", `{"data":[{"type":"node","name":"pve1","local":0},` +
			`{"type":"node","name":"pve2","local":0}]}`, "pve2", errColdStartLocal},
		{"a local cluster row is not a node", `{"data":[{"type":"cluster","name":"pve2","local":1},` +
			`{"type":"node","name":"pve1","local":0},{"type":"node","name":"pve2","local":0}]}`, "pve2",
			errColdStartLocal},
		{"a local flag as text", `{"data":[{"type":"node","name":"pve1","local":"1"},` +
			`{"type":"node","name":"pve2","local":0}]}`, "pve1", nil},
		{"extra members are fine", clusterStatus("pve1", "pve1", "pve2", "pve3"), "pve1", nil},
	}
	for _, tc := range cases {
		err := p.checkStatus([]byte(tc.body), tc.node)
		if !errors.Is(err, tc.want) && (tc.want != nil || err != nil) {
			t.Errorf("%s: checkStatus = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestColdStartRefusalReasonScrubsAnUnknownCause(t *testing.T) {
	t.Parallel()
	got := coldStartRefusalReason(errors.New("Authorization: PVEAPIToken=root@pam!cpi=topsecret failed"))

	if strings.Contains(got, "topsecret") {
		t.Fatalf("reason %q carries the token secret", got)
	}
	if !bytes.HasPrefix([]byte(got), []byte("the membership probe failed")) {
		t.Errorf("reason = %q, want the generic probe failure", got)
	}
}
