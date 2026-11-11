package pve

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

// The tests in this file build clients through newClient, with a cache
// directory, a dead seed, and loopback servers that stand for the nodes an
// az_map names. A dial function routes each derived name to its server, so the
// client dials names such as pve2.dc1.example.com without a resolver. They
// drain the process registry and the refusal record, so none runs in parallel,
// and none sleeps: the dead seed refuses at once, and every wait is on the
// client's own answer.

const (
	coldWireDomain = "dc1.example.com"
	coldWirePort   = 8006
	coldWireToken  = "root@pam!cpi=0123456789abcdef"
	coldWireVMID   = 4242
)

// coldWireNames are the nodes the lab's az_map names.
var coldWireNames = []string{"pve1", "pve2", "pve3"}

// coldLab holds the certificate authority and the routes of one test.
type coldLab struct {
	t  *testing.T
	ca *pvetest.CA

	mu      sync.Mutex
	targets map[string]string
	dialed  map[string]int
}

func newColdLab(t *testing.T) *coldLab {
	t.Helper()
	l := &coldLab{t: t, ca: pvetest.NewCA(t), targets: map[string]string{}, dialed: map[string]int{}}
	t.Cleanup(SetClientDialForTest(l.dial))
	resetColdStartRefusals(t)
	return l
}

// dial sends a registered address to its server and refuses every other, which
// stands for a host that is down. Each dial is counted.
func (l *coldLab) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	l.mu.Lock()
	l.dialed[addr]++
	target, ok := l.targets[addr]
	l.mu.Unlock()
	if !ok {
		return nil, &net.OpError{Op: "dial", Net: network, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	return (&net.Dialer{}).DialContext(ctx, network, target)
}

// dials is the number of connections the client tried to make to addr.
func (l *coldLab) dials(addr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dialed[addr]
}

// coldNodeOptions changes how a node answers.
type coldNodeOptions struct {
	// signer signs the node's certificate, the lab's CA when nil.
	signer *pvetest.CA
	// names are the certificate's DNS names, the node's host when empty.
	names []string
	// status is the answer to GET /cluster/status, 200 when zero.
	status int
	// local is the node /cluster/status marks local, the node itself when empty.
	local string
	// members are the node rows of /cluster/status, pve1 to pve3 when nil.
	members []string
}

// coldNode is one node server.
type coldNode struct {
	host string
	addr string
	srv  *pvetest.Server
}

// node starts a server for host, a derived name such as pve2.dc1.example.com,
// and routes host:8006 to it.
func (l *coldLab) node(host string, opts coldNodeOptions) *coldNode {
	l.t.Helper()
	name, _, _ := strings.Cut(host, ".")
	signer := opts.signer
	if signer == nil {
		signer = l.ca
	}
	names := opts.names
	if names == nil {
		names = []string{host}
	}
	local := opts.local
	if local == "" {
		local = name
	}
	members := opts.members
	if members == nil {
		members = []string{"pve1", "pve2", "pve3"}
	}
	rows := make([]any, 0, 1+len(members))
	rows = append(rows, map[string]any{"type": "cluster", "name": "lab", "nodes": len(members), "quorate": 1})
	for _, m := range members {
		isLocal := 0
		if m == local {
			isLocal = 1
		}
		rows = append(rows, map[string]any{"type": "node", "name": m, "local": isLocal, "online": 1})
	}
	routes := pvetest.NewRoutes()
	if opts.status != 0 && opts.status != http.StatusOK {
		routes.Fail(http.MethodGet, "/cluster/status", opts.status, "permission check failed")
	} else {
		routes.JSON(http.MethodGet, "/cluster/status", http.StatusOK, rows)
	}
	routes.JSON(http.MethodGet, "/pools/p1", http.StatusOK, tetherPoolBody)
	routes.JSON(http.MethodPost, "/nodes/pve9/qemu", http.StatusOK, "UPID:"+name+":0001:create")
	srv := pvetest.NewLive(l.t, signer.NewLeaf(l.t, pvetest.LeafOptions{DNSNames: names}), routes)
	address := net.JoinHostPort(host, strconv.Itoa(coldWirePort))
	l.mu.Lock()
	l.targets[address] = srv.Addr
	l.mu.Unlock()
	return &coldNode{host: host, addr: address, srv: srv}
}

// requests lists the node's requests as "METHOD path", in the order it read
// them.
func (n *coldNode) requests() []string {
	seen := n.srv.Seen()
	out := make([]string, 0, len(seen))
	for _, r := range seen {
		out = append(out, r.Method+" "+strings.TrimPrefix(r.Path, pvetest.APIPrefix))
	}
	return out
}

// tokenOnEveryRequest fails the test when a request the node read carried
// anything but the lab's token as its credential.
func (n *coldNode) tokenOnEveryRequest(t *testing.T) {
	t.Helper()
	for _, r := range n.srv.Seen() {
		got := r.Header.Values("Authorization")
		if want := "PVEAPIToken=" + coldWireToken; len(got) != 1 || got[0] != want {
			t.Errorf("%s read %s %s with Authorization %v, want the token alone", n.host, r.Method, r.Path, got)
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("CSRFPreventionToken") != "" {
			t.Errorf("%s read a request with a ticket: %v", n.host, r.Header)
		}
	}
}

// config builds the config of an entry whose seed is dead, whose cache is
// empty, and whose az_map names the lab's three nodes.
func (l *coldLab) config(t *testing.T) *config.CPIConfig {
	t.Helper()
	verify := true
	return &config.CPIConfig{
		Host:                 "pve1." + coldWireDomain,
		Port:                 coldWirePort,
		APIToken:             coldWireToken,
		APIEndpointDiscovery: config.DiscoveryModeOn,
		VerifySSL:            &verify,
		PVECACertPEM:         string(l.ca.CertPEM()),
		EndpointCacheDir:     newCacheDir(t),
		Placement:            &config.PlacementConfig{AZMap: map[string][]string{"az1": {"pve1", "pve2", "pve3"}}},
	}
}

// process builds a client the way a new CPI process would.
func (l *coldLab) process(t *testing.T, cfg *config.CPIConfig) (Client, *log.Observer) {
	t.Helper()
	resetColdStartRefusals(t)
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	c, err := newClient(cfg, logger, nil)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return c, obs
}

// createVM sends the mutating request create_vm makes to reach a node, which
// is the POST that creates the guest.
func createVM(c Client) (string, error) {
	return c.QEMU().Create(context.Background(), "pve9", map[string]interface{}{"vmid": coldWireVMID})
}

func mustCreate(t *testing.T, c Client, node *coldNode) {
	t.Helper()
	upid, err := createVM(c)
	if err != nil {
		t.Fatalf("create_vm request: %v", err)
	}
	if !strings.HasPrefix(upid, "UPID:"+strings.SplitN(node.host, ".", 2)[0]) {
		t.Errorf("UPID = %q, want the answer of %s", upid, node.host)
	}
}

// mustFailToConnect fails the test unless err says no endpoint answered. A
// pool that walked returns its EntryUnreachableError, and a client with one
// endpoint and no pool returns the dial error, which is what the same client
// returned before the cold start existed.
func mustFailToConnect(t *testing.T, err error) {
	t.Helper()
	var walk *endpointpool.EntryUnreachableError
	if errors.As(err, &walk) {
		return
	}
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error = %v, want a failure to connect", err)
	}
}

func TestColdStartCreateVMReachesAnAcceptedCandidateThroughTheProductionClient(t *testing.T) {
	lab := newColdLab(t)
	cfg := lab.config(t)
	pve2 := lab.node("pve2."+coldWireDomain, coldNodeOptions{})
	pve3 := lab.node("pve3."+coldWireDomain, coldNodeOptions{})
	c, _ := lab.process(t, cfg)

	mustCreate(t, c, pve2)

	probeThenCreate := []string{"GET /cluster/status", "POST /nodes/pve9/qemu"}
	if got := pve2.requests(); !slices.Equal(got, probeThenCreate) {
		t.Fatalf("pve2 read %v, want the probe and then the create", got)
	}
	pve2.tokenOnEveryRequest(t)
	if n := pve3.srv.Counts.Requests.Load(); n != 0 {
		t.Errorf("pve3 read %d requests, want none, because the first candidate was accepted", n)
	}
	if n := lab.dials(pve3.addr); n != 0 {
		t.Errorf("the client dialed pve3 %d times, want none", n)
	}
	mustCreate(t, c, pve2)
	if n := pve2.srv.Counts.Method(http.MethodGet); n != 1 {
		t.Errorf("pve2 read %d probes, want the one for the process", n)
	}

	drainProcessRegistry(t)
	payload := readSeededCacheNamed(t, cfg, coldWireNames)
	if payload == nil || payload.ColdStart == nil || payload.ColdStart.Name != "pve2" {
		t.Fatalf("cache after the drain = %+v, want a cold_start hint for pve2", payload)
	}
	if time.Since(payload.ColdStart.ProbedAt) > time.Minute {
		t.Errorf("hint probed_at = %v, want now", payload.ColdStart.ProbedAt)
	}
	if _, ok := payload.Marks[net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))]; !ok {
		t.Errorf("marks = %v, want the seed's down mark", payload.Marks)
	}
	if len(payload.Marks) != 1 {
		t.Errorf("marks = %v, want only the seed's, because a candidate gets no mark", payload.Marks)
	}
}

func TestColdStartFailoverOfAReadLogsOneWarnAndKeepsTheCandidateActive(t *testing.T) {
	lab := newColdLab(t)
	cfg := lab.config(t)
	pve2 := lab.node("pve2."+coldWireDomain, coldNodeOptions{})
	c, obs := lab.process(t, cfg)

	readPoolComment(t, c)
	readPoolComment(t, c)

	lines := linesAt(obs.All(), log.LevelWarn, "request sent to pve2.dc1.example.com:8006 from placement.az_map "+
		"after it proved membership")
	if len(lines) != 1 {
		t.Fatalf("Warn failover lines = %v, want one", lines)
	}
	if got := pve2.requests(); !slices.Equal(got, []string{"GET /cluster/status", "GET /pools/p1", "GET /pools/p1"}) {
		t.Errorf("pve2 read %v, want one probe and then both reads", got)
	}
	drainProcessRegistry(t)
}

func TestColdStartRefusesACandidateBeforeTheRequestReachesIt(t *testing.T) {
	otherCA := pvetest.NewCA(t)
	cases := []struct {
		name string
		bad  coldNodeOptions
		// probed is what the refused node reads, which is the probe or, when
		// the handshake is refused, nothing.
		probed []string
		reason string
	}{
		{"a certificate that does not chain", coldNodeOptions{signer: otherCA}, nil,
			"certificate does not chain to pve.ca_cert"},
		{"a name mismatch", coldNodeOptions{names: []string{"pve9." + coldWireDomain}}, nil,
			"certificate is not valid for that name"},
		{"a 403", coldNodeOptions{status: http.StatusForbidden}, []string{"GET /cluster/status"}, "HTTP 403"},
		{"a different local node", coldNodeOptions{local: "pve3"}, []string{"GET /cluster/status"},
			"reports itself as another node"},
		{"a list missing an az_map name", coldNodeOptions{members: []string{"pve2", "pve3"}},
			[]string{"GET /cluster/status"}, "lacks pve1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lab := newColdLab(t)
			cfg := lab.config(t)
			bad := lab.node("pve2."+coldWireDomain, tc.bad)
			good := lab.node("pve3."+coldWireDomain, coldNodeOptions{})
			c, obs := lab.process(t, cfg)

			mustCreate(t, c, good)

			if got := bad.requests(); !slices.Equal(got, tc.probed) {
				t.Errorf("the refused node read %v, want %v, and never the create", got, tc.probed)
			}
			probeThenCreate := []string{"GET /cluster/status", "POST /nodes/pve9/qemu"}
			if got := good.requests(); !slices.Equal(got, probeThenCreate) {
				t.Errorf("the accepted node read %v, want the probe and the create", got)
			}
			good.tokenOnEveryRequest(t)
			if tc.probed == nil && bad.srv.Counts.Requests.Load() != 0 {
				t.Error("the token reached a node whose certificate failed the chain or the name check")
			}
			lines := linesAt(obs.All(), log.LevelWarn, "pve2."+coldWireDomain+":8006 from placement.az_map refused")
			if len(lines) != 1 || !strings.Contains(lines[0], tc.reason) {
				t.Errorf("refusal lines = %v, want one with %q at Warn for an explicit on", lines, tc.reason)
			}
			drainProcessRegistry(t)
			payload := readSeededCacheNamed(t, cfg, coldWireNames)
			if payload == nil || payload.ColdStart == nil || payload.ColdStart.Name != "pve3" {
				t.Errorf("hint = %+v, want pve3, the node that was accepted", payload)
			}
		})
	}
}

func TestColdStartRefusalIsDebugUnderAuto(t *testing.T) {
	lab := newColdLab(t)
	cfg := lab.config(t)
	cfg.APIEndpointDiscovery = config.DiscoveryModeAuto
	lab.node("pve2."+coldWireDomain, coldNodeOptions{status: http.StatusForbidden})
	good := lab.node("pve3."+coldWireDomain, coldNodeOptions{})
	c, obs := lab.process(t, cfg)

	mustCreate(t, c, good)

	if lines := linesAt(obs.All(), log.LevelDebug, "from placement.az_map refused"); len(lines) != 1 {
		t.Errorf("Debug refusal lines = %v, want one", lines)
	}
	if e, bad := anyLineAbove(obs.All(), log.LevelWarn); bad {
		t.Errorf("logged %q at %v", e.Message, e.Level)
	}
	for _, e := range obs.All() {
		if e.Level == log.LevelWarn && strings.Contains(e.Message, "refused") {
			t.Errorf("a refusal under auto logged at Warn: %q", e.Message)
		}
	}
	drainProcessRegistry(t)
}

func TestColdStartHintMakesASecondProcessTryThatNodeFirstAndProbeItAgain(t *testing.T) {
	lab := newColdLab(t)
	cfg := lab.config(t)
	// The first process finds pve2 refusing, so the hint names pve3.
	pve2 := lab.node("pve2."+coldWireDomain, coldNodeOptions{status: http.StatusForbidden})
	pve3 := lab.node("pve3."+coldWireDomain, coldNodeOptions{})
	first, _ := lab.process(t, cfg)
	mustCreate(t, first, pve3)
	drainProcessRegistry(t)
	if got := pve2.requests(); !slices.Equal(got, []string{"GET /cluster/status"}) {
		t.Fatalf("the first process sent pve2 %v, want the one refused probe", got)
	}

	second, _ := lab.process(t, cfg)
	mustCreate(t, second, pve3)

	if got := pve2.requests(); len(got) != 1 {
		t.Errorf("the second process sent pve2 %v, want nothing more, because the hint put pve3 first", got)
	}
	if got := pve3.srv.Counts.Method(http.MethodGet); got != 2 {
		t.Errorf("pve3 read %d probes over two processes, want a fresh probe in each", got)
	}
	drainProcessRegistry(t)
}

func TestColdStartHintForANodeTheMapNoLongerNamesIsIgnored(t *testing.T) {
	lab := newColdLab(t)
	cfg := lab.config(t)
	pve2 := lab.node("pve2."+coldWireDomain, coldNodeOptions{})
	first, _ := lab.process(t, cfg)
	mustCreate(t, first, pve2)
	drainProcessRegistry(t)
	if payload := readSeededCacheNamed(t, cfg, coldWireNames); payload == nil || payload.ColdStart == nil {
		t.Fatalf("the first process left no hint: %+v", payload)
	}

	// The second process's map no longer names pve2, so the hint is not its
	// hint, and pve2 gets no request though it would answer.
	second := *cfg
	second.Placement = &config.PlacementConfig{AZMap: map[string][]string{"az1": {"pve1", "pve3"}}}
	pve3 := lab.node("pve3."+coldWireDomain, coldNodeOptions{})
	c, obs := lab.process(t, &second)
	mustCreate(t, c, pve3)

	if got := pve2.requests(); len(got) != 2 {
		t.Errorf("pve2 read %v, want only the first process's probe and create", got)
	}
	if got := pve3.requests(); len(got) != 2 {
		t.Errorf("pve3 read %v, want the probe and the create", got)
	}
	if lines := linesAt(obs.All(), log.LevelDebug, "cold-start hint"); len(lines) == 0 {
		t.Logf("no Debug line names the dropped hint: %v", obs.All())
	}
	drainProcessRegistry(t)
}

func TestColdStartStaysOffWhenAConditionFails(t *testing.T) {
	cases := []struct {
		name  string
		shape func(t *testing.T, lab *coldLab, cfg *config.CPIConfig)
	}{
		{"discovery is off", func(_ *testing.T, _ *coldLab, cfg *config.CPIConfig) {
			cfg.APIEndpointDiscovery = config.DiscoveryModeOff
		}},
		{"verify_ssl is false", func(_ *testing.T, _ *coldLab, cfg *config.CPIConfig) {
			off := false
			cfg.VerifySSL = &off
		}},
		{"there is no ca_cert", func(_ *testing.T, _ *coldLab, cfg *config.CPIConfig) {
			cfg.PVECACertPEM = ""
		}},
		{"the client uses ticket auth", func(_ *testing.T, _ *coldLab, cfg *config.CPIConfig) {
			cfg.APIToken = ""
			cfg.User, cfg.Realm, cfg.Password = "root", "pam", "secret"
		}},
		{"az_map is empty", func(_ *testing.T, _ *coldLab, cfg *config.CPIConfig) {
			cfg.Placement = &config.PlacementConfig{AZMap: map[string][]string{}}
		}},
		{"there is no placement", func(_ *testing.T, _ *coldLab, cfg *config.CPIConfig) {
			cfg.Placement = nil
		}},
		{"az_map names only invalid nodes", func(_ *testing.T, _ *coldLab, cfg *config.CPIConfig) {
			cfg.Placement = &config.PlacementConfig{AZMap: map[string][]string{"az1": {"pve2.dc1.example.com", "123"}}}
		}},
		{"the cache directory is unusable", func(t *testing.T, _ *coldLab, cfg *config.CPIConfig) {
			if err := os.Chmod(cfg.EndpointCacheDir, 0o777); err != nil { // #nosec G302 -- the test makes it unusable
				t.Fatal(err)
			}
		}},
		{"the cache holds a usable discovered node", func(t *testing.T, _ *coldLab, cfg *config.CPIConfig) {
			seedEndpointCache(t, cfg, cacheSeed{nodes: []cachedNode{{name: "pve4", online: true}}})
		}},
		{"the last discovery found a standalone host", func(t *testing.T, _ *coldLab, cfg *config.CPIConfig) {
			seedEndpointCache(t, cfg, cacheSeed{outcome: endpointcache.OutcomeStandalone})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lab := newColdLab(t)
			cfg := lab.config(t)
			pve2 := lab.node("pve2."+coldWireDomain, coldNodeOptions{})
			pve3 := lab.node("pve3."+coldWireDomain, coldNodeOptions{})
			tc.shape(t, lab, cfg)
			c, _ := lab.process(t, cfg)

			var err error
			if cfg.APIToken == "" {
				err = c.Pools().CreatePool(context.Background(), "p1", "x")
			} else {
				_, err = createVM(c)
			}

			mustFailToConnect(t, err)
			for _, n := range []*coldNode{pve2, pve3} {
				if got := n.srv.Counts.Conns.Load(); got != 0 {
					t.Errorf("%s accepted %d connections, want none", n.host, got)
				}
				if got := lab.dials(n.addr); got != 0 {
					t.Errorf("the client dialed %s %d times, want none", n.host, got)
				}
			}
			drainProcessRegistry(t)
		})
	}
}

func TestColdStartStaysOnForAnOutcomeThatLeavesNoNode(t *testing.T) {
	for _, outcome := range []endpointcache.DiscoveryOutcome{
		endpointcache.OutcomeNoCandidates, endpointcache.OutcomePublicSuffix,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			lab := newColdLab(t)
			cfg := lab.config(t)
			seedEndpointCache(t, cfg, cacheSeed{outcome: outcome})
			pve2 := lab.node("pve2."+coldWireDomain, coldNodeOptions{})
			c, _ := lab.process(t, cfg)

			mustCreate(t, c, pve2)

			drainProcessRegistry(t)
		})
	}
}

// coldJob is a valid job-level config for the entry tests.
func (l *coldLab) job(t *testing.T) *config.CPIConfig {
	t.Helper()
	job := l.config(t)
	job.User, job.Realm = "root", "pam"
	job.VMStorage, job.DiskStorage, job.NetworkBridge = "local", "local", "vmbr0"
	job.AgentMode = "noagent"
	job.ApplyDefaults()
	if err := job.Validate(); err != nil {
		t.Fatalf("the job config does not validate: %v", err)
	}
	return job
}

func (l *coldLab) entry(t *testing.T, job *config.CPIConfig, extra map[string]any) *config.CPIConfig {
	t.Helper()
	eff, _, _, err := config.ApplyContextOverrides(job, extra)
	if err != nil {
		t.Fatalf("ApplyContextOverrides: %v", err)
	}
	return eff
}

func TestColdStartOfAnEntryFollowsWhoseAZMapItCarries(t *testing.T) {
	const otherToken = "root@pam!entry=fedcba9876543210"
	az := func() map[string]any {
		return map[string]any{"az_map": map[string]any{"az1": []any{"pve1", "pve2", "pve3"}}}
	}
	t.Run("an entry on the job's endpoint list gets the job's names", func(t *testing.T) {
		lab := newColdLab(t)
		job := lab.job(t)
		pve2 := lab.node("pve2."+coldWireDomain, coldNodeOptions{})
		entry := lab.entry(t, job, map[string]any{"pve_api_token": otherToken})
		logger, _ := log.NewObservedLogger(log.LevelDebug)

		c, err := NewEntryClientWithTracer(entry, job, logger, nil)
		if err != nil {
			t.Fatalf("NewEntryClientWithTracer: %v", err)
		}

		upid, err := createVM(c)
		if err != nil || !strings.HasPrefix(upid, "UPID:pve2") {
			t.Fatalf("create = %q, %v, want the answer of pve2", upid, err)
		}
		want := []string{"GET /cluster/status", "POST /nodes/pve9/qemu"}
		if got := pve2.requests(); !slices.Equal(got, want) {
			t.Errorf("pve2 read %v, want the probe and the create", got)
		}
		drainProcessRegistry(t)
	})
	t.Run("an entry that names its own placement on another host cold-starts from it", func(t *testing.T) {
		lab := newColdLab(t)
		job := lab.job(t)
		pve2 := lab.node("pve2.dc2.example.com", coldNodeOptions{})
		entry := lab.entry(t, job, map[string]any{
			"pve_host": "pve1.dc2.example.com", "pve_api_token": otherToken, "pve_ca_cert": string(lab.ca.CertPEM()),
			"pve_placement": az(),
		})
		logger, _ := log.NewObservedLogger(log.LevelDebug)

		c, err := NewEntryClientWithTracer(entry, job, logger, nil)
		if err != nil {
			t.Fatalf("NewEntryClientWithTracer: %v", err)
		}

		if upid, err := createVM(c); err != nil || !strings.HasPrefix(upid, "UPID:pve2") {
			t.Fatalf("create = %q, %v, want the answer of %s", upid, err, pve2.host)
		}
		drainProcessRegistry(t)
	})
	t.Run("an entry on another host that names no placement gets none", func(t *testing.T) {
		lab := newColdLab(t)
		job := lab.job(t)
		// The job's names are real members of the entry's host in this lab, so
		// a client that took them would be accepted.
		pve2 := lab.node("pve2.dc2.example.com", coldNodeOptions{})
		jobNode := lab.node("pve2."+coldWireDomain, coldNodeOptions{})
		entry := lab.entry(t, job, map[string]any{
			"pve_host": "pve1.dc2.example.com", "pve_api_token": otherToken, "pve_ca_cert": string(lab.ca.CertPEM()),
		})
		if len(entry.Placement.AZMap) == 0 {
			t.Fatal("the entry lost the inherited az_map the test is about")
		}
		logger, _ := log.NewObservedLogger(log.LevelDebug)

		c, err := NewEntryClientWithTracer(entry, job, logger, nil)
		if err != nil {
			t.Fatalf("NewEntryClientWithTracer: %v", err)
		}

		_, err = createVM(c)
		mustFailToConnect(t, err)
		for _, n := range []*coldNode{pve2, jobNode} {
			if lab.dials(n.addr) != 0 || n.srv.Counts.Conns.Load() != 0 {
				t.Errorf("the client reached %s, which only a borrowed az_map could name", n.host)
			}
		}
		drainProcessRegistry(t)
	})
	t.Run("an entry passed as its own job gets none from an inherited map", func(t *testing.T) {
		lab := newColdLab(t)
		job := lab.job(t)
		pve2 := lab.node("pve2."+coldWireDomain, coldNodeOptions{})
		entry := lab.entry(t, job, map[string]any{"pve_api_token": otherToken})
		logger, _ := log.NewObservedLogger(log.LevelDebug)

		c, err := NewEntryClientWithTracer(entry, entry, logger, nil)
		if err != nil {
			t.Fatalf("NewEntryClientWithTracer: %v", err)
		}

		_, err = createVM(c)
		mustFailToConnect(t, err)
		if lab.dials(pve2.addr) != 0 {
			t.Error("an entry that is its own job borrowed the job's names")
		}
		drainProcessRegistry(t)
	})
}

func TestNewEntryClientRefusesANilJob(t *testing.T) {
	lab := newColdLab(t)
	cfg := lab.config(t)

	c, err := NewEntryClientWithTracer(cfg, nil, log.NewNopLogger(), nil)

	if err == nil || c != nil {
		t.Fatalf("NewEntryClientWithTracer(nil job) = %v, %v, want a refusal", c, err)
	}
	if names, readErr := readDirNames(cfg.EndpointCacheDir); readErr != nil || len(names) != 0 {
		t.Errorf("the refused build touched the cache directory: %v, %v", names, readErr)
	}
}

func TestJobLevelClientIsItsOwnJob(t *testing.T) {
	lab := newColdLab(t)
	cfg := lab.config(t)
	pve2 := lab.node("pve2."+coldWireDomain, coldNodeOptions{})
	logger, _ := log.NewObservedLogger(log.LevelDebug)

	c, err := NewClientWithTracer(cfg, logger, nil)
	if err != nil {
		t.Fatalf("NewClientWithTracer: %v", err)
	}

	mustCreate(t, c, pve2)
	drainProcessRegistry(t)
}
