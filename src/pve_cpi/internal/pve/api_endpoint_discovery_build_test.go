package pve

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// discFingerprint returns a /nodes style fingerprint, upper-case hex pairs
// joined by colons, and the pin it parses to.
func discFingerprint(label string) (string, endpointpool.Pin) {
	sum := sha256.Sum256([]byte(label))
	pairs := make([]string, len(sum))
	for i, b := range sum {
		pairs[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(pairs, ":"), endpointpool.Pin(sum)
}

// discNode returns a /nodes row whose fingerprint derives from the node name.
func discNode(name, status string) DiscoveryNodeRead {
	fingerprint, _ := discFingerprint("node:" + name)
	return DiscoveryNodeRead{Name: name, Status: status, Fingerprint: fingerprint}
}

func discNodePin(name string) endpointpool.Pin {
	_, pin := discFingerprint("node:" + name)
	return pin
}

var discRoot = sha256.Sum256([]byte("cluster root"))

// quorateStatus is a status read of a quorate cluster whose nodes are all
// online, answered by local.
func quorateStatus(local string, names ...string) DiscoveryStatus {
	online := map[string]bool{}
	for _, name := range names {
		online[name] = true
	}
	return DiscoveryStatus{Available: true, InCluster: true, Quorate: true, Local: local, Online: online}
}

func acceptAll(context.Context, string) error { return nil }

func discInput(seed string, resolver NameResolver) DiscoveryInput {
	return DiscoveryInput{Seed: seed, Configured: []string{seed}, Resolver: resolver}
}

func nodeNames(nodes []DiscoveredNode) []string {
	names := make([]string, len(nodes))
	for i, node := range nodes {
		names[i] = node.Name
	}
	return names
}

func droppedFor(result DiscoveryResult, name string) DropReason {
	for _, d := range result.Dropped {
		if d.Name == name {
			return d.Reason
		}
	}
	return ""
}

func TestBuildDiscoveryResult_FullReadWithFQDNSeed(t *testing.T) {
	t.Parallel()
	pveCert, _ := discFingerprint("cert:pve2:ssl")
	proxyCert, _ := discFingerprint("cert:pve2:proxy")
	pveCertPin, _ := endpointpool.ParsePin(pveCert)
	proxyCertPin, _ := endpointpool.ParsePin(proxyCert)
	pve2 := discNode("pve2", "online")
	// The first cert pin repeats the /nodes fingerprint, as it does when
	// pveproxy-ssl.pem is absent, and must not repeat in the set.
	pve2.CertPins = []endpointpool.Pin{discNodePin("pve2"), pveCertPin, proxyCertPin}
	reads := DiscoveryReads{
		Nodes:  []DiscoveryNodeRead{discNode("pve1", "online"), pve2, discNode("pve3", "online")},
		Status: quorateStatus("pve1", "pve1", "pve2", "pve3"),
		RootCA: discRoot,
	}
	result, err := BuildDiscoveryResult(context.Background(), reads, discInput("pve1.dc1.example.com:8006", acceptAll))
	if err != nil {
		t.Fatalf("BuildDiscoveryResult: %v", err)
	}
	if result.Outcome != DiscoveryOK || !result.Quorate || result.RootCA != discRoot {
		t.Fatalf("result = %+v, want ok, quorate, with the root", result)
	}
	if got, want := nodeNames(result.Nodes), []string{"pve2", "pve3"}; !slices.Equal(got, want) {
		t.Fatalf("nodes = %v, want %v", got, want)
	}
	first := result.Nodes[0]
	if first.Address != "pve2.dc1.example.com:8006" || !first.Online {
		t.Fatalf("first node = %+v", first)
	}
	wantPins := []endpointpool.Pin{discNodePin("pve2"), pveCertPin, proxyCertPin}
	if !slices.Equal(first.Pins, wantPins) {
		t.Fatalf("pins = %v, want the /nodes fingerprint first, then the certificate pins, without repeats", first.Pins)
	}
	if got := droppedFor(result, "pve1"); got != DropSeedNode {
		t.Fatalf("pve1 dropped for %q, want the seed's own node", got)
	}
}

func TestBuildDiscoveryResult_BareAndIPSeedsDeriveBareNames(t *testing.T) {
	t.Parallel()
	reads := DiscoveryReads{
		Nodes:  []DiscoveryNodeRead{discNode("pve1", "online"), discNode("pve2", "online")},
		Status: quorateStatus("pve1", "pve1", "pve2"),
		RootCA: discRoot,
	}
	for _, seed := range []string{"pve1:8443", "192.0.2.10:8443", "[2001:db8::10]:8443"} {
		result, err := BuildDiscoveryResult(context.Background(), reads, discInput(seed, acceptAll))
		if err != nil {
			t.Fatalf("seed %s: %v", seed, err)
		}
		if len(result.Nodes) != 1 || result.Nodes[0].Address != "pve2:8443" {
			t.Fatalf("seed %s: nodes = %+v, want pve2:8443 with the seed's port", seed, result.Nodes)
		}
	}
}

func TestBuildDiscoveryResult_PublicSuffixSeedGetsNoNodes(t *testing.T) {
	t.Parallel()
	reads := DiscoveryReads{
		Nodes:  []DiscoveryNodeRead{discNode("pve1", "online"), discNode("pve2", "online")},
		Status: quorateStatus("pve1", "pve1", "pve2"),
		RootCA: discRoot,
	}
	seeds := []string{"pve1.example.co.uk", "pve1.dyndns.org", "pve1.lan", "pve1.sch.uk"}
	for _, seed := range seeds {
		result, err := BuildDiscoveryResult(context.Background(), reads, discInput(seed+":8006", acceptAll))
		if err != nil {
			t.Fatalf("seed %s: %v", seed, err)
		}
		// pve1.example.co.uk has the registrable domain example.co.uk, so
		// only the seeds whose domain or derived hosts are suffixes fail.
		wantSuffix := seed != "pve1.example.co.uk"
		if wantSuffix && (result.Outcome != DiscoveryPublicSuffix || len(result.Nodes) != 0) {
			t.Fatalf("seed %s: outcome %q with %d nodes, want public_suffix and none", seed, result.Outcome, len(result.Nodes))
		}
		if !wantSuffix && result.Outcome != DiscoveryOK {
			t.Fatalf("seed %s: outcome %q, want ok", seed, result.Outcome)
		}
		if result.RootCA != discRoot {
			t.Fatalf("seed %s: the identity must still be recorded", seed)
		}
	}
	result, err := BuildDiscoveryResult(context.Background(), reads, discInput("example.co.uk:8006", acceptAll))
	if err != nil || result.Outcome != DiscoveryPublicSuffix {
		t.Fatalf("seed example.co.uk: outcome %q, err %v, want public_suffix", result.Outcome, err)
	}
}

func TestBuildDiscoveryResult_SeedThatIsNotANameGetsNoCandidates(t *testing.T) {
	t.Parallel()
	reads := DiscoveryReads{
		Nodes:  []DiscoveryNodeRead{discNode("pve1", "online"), discNode("pve2", "online")},
		Status: quorateStatus("pve1", "pve1", "pve2"),
		RootCA: discRoot,
	}
	for _, seed := range []string{"10.1:8006", "pve1.123:8006", "pve1"} {
		result, err := BuildDiscoveryResult(context.Background(), reads, discInput(seed, acceptAll))
		if err != nil {
			t.Fatalf("seed %s: %v", seed, err)
		}
		if seed == "pve1" {
			// A portless seed cannot be an endpoint the reads went through.
			if result.Outcome != DiscoveryNoCandidates {
				t.Fatalf("seed %s: outcome %q", seed, result.Outcome)
			}
			continue
		}
		if result.Outcome != DiscoveryNoCandidates || len(result.Nodes) != 0 {
			t.Fatalf("seed %s: outcome %q with %d nodes, want no_candidates", seed, result.Outcome, len(result.Nodes))
		}
	}
}

func TestBuildDiscoveryResult_NoStatusOrdersByNodesStatus(t *testing.T) {
	t.Parallel()
	reads := DiscoveryReads{
		Nodes: []DiscoveryNodeRead{
			discNode("pve1", "online"), discNode("pve4", "offline"), discNode("pve3", "online"),
			discNode("pve2", "offline"), discNode("pve5", "unknown"),
		},
		RootCA: discRoot,
	}
	result, err := BuildDiscoveryResult(context.Background(), reads, discInput("pve1.dc1.example.com:8006", acceptAll))
	if err != nil {
		t.Fatalf("BuildDiscoveryResult: %v", err)
	}
	if result.Outcome != DiscoveryNoStatus || result.Quorate {
		t.Fatalf("outcome %q quorate %v, want no_status and not quorate", result.Outcome, result.Quorate)
	}
	// Without a status nothing marks the seed's node, so pve1 stays in the
	// list only if its address is not already a configured endpoint.
	if got, want := nodeNames(result.Nodes), []string{"pve3", "pve2", "pve4", "pve5"}; !slices.Equal(got, want) {
		t.Fatalf("nodes = %v, want %v", got, want)
	}
	if got := droppedFor(result, "pve1"); got != DropConfigured {
		t.Fatalf("pve1 dropped for %q, want configured", got)
	}
}

func TestBuildDiscoveryResult_QuorateOrdersOnlineFirstInNameOrder(t *testing.T) {
	t.Parallel()
	status := quorateStatus("pve1", "pve1", "pve3", "pve5")
	status.Online["pve2"] = false
	status.Online["pve4"] = false
	reads := DiscoveryReads{
		// The /nodes status fields say the opposite, and must be ignored.
		Nodes: []DiscoveryNodeRead{
			discNode("pve5", "offline"), discNode("pve2", "online"), discNode("pve1", "online"),
			discNode("pve4", "online"), discNode("pve3", "offline"),
		},
		Status: status,
		RootCA: discRoot,
	}
	result, err := BuildDiscoveryResult(context.Background(), reads, discInput("pve1.dc1.example.com:8006", acceptAll))
	if err != nil {
		t.Fatalf("BuildDiscoveryResult: %v", err)
	}
	if got, want := nodeNames(result.Nodes), []string{"pve3", "pve5", "pve2", "pve4"}; !slices.Equal(got, want) {
		t.Fatalf("nodes = %v, want the online nodes first, in name order", got)
	}
}

func TestBuildDiscoveryResult_NonQuorateStatusKeepsNameOrder(t *testing.T) {
	t.Parallel()
	status := quorateStatus("pve1", "pve1")
	status.Quorate = false
	status.Online["pve2"] = false
	status.Online["pve3"] = false
	reads := DiscoveryReads{
		Nodes: []DiscoveryNodeRead{
			discNode("pve1", "online"), discNode("pve3", "offline"), discNode("pve2", "online"),
		},
		Status: status,
		RootCA: discRoot,
	}
	result, err := BuildDiscoveryResult(context.Background(), reads, discInput("pve1.dc1.example.com:8006", acceptAll))
	if err != nil {
		t.Fatalf("BuildDiscoveryResult: %v", err)
	}
	if result.Outcome != DiscoveryOK || result.Quorate {
		t.Fatalf("outcome %q quorate %v, want ok and not quorate", result.Outcome, result.Quorate)
	}
	if got, want := nodeNames(result.Nodes), []string{"pve2", "pve3"}; !slices.Equal(got, want) {
		t.Fatalf("nodes = %v, want every node in name order", got)
	}
}

func TestBuildDiscoveryResult_StandaloneHost(t *testing.T) {
	t.Parallel()
	reads := DiscoveryReads{
		Nodes:  []DiscoveryNodeRead{discNode("pve1", "online")},
		Status: DiscoveryStatus{Available: true, Local: "pve1", Online: map[string]bool{"pve1": true}},
		RootCA: discRoot,
	}
	result, err := BuildDiscoveryResult(context.Background(), reads, discInput("pve1.dc1.example.com:8006", acceptAll))
	if err != nil {
		t.Fatalf("BuildDiscoveryResult: %v", err)
	}
	if result.Outcome != DiscoveryStandalone || len(result.Nodes) != 0 || result.RootCA != discRoot {
		t.Fatalf("result = %+v, want standalone with no nodes and the identity", result)
	}
	// Several nodes in /nodes contradict a status with no cluster row, so the
	// claim of a standalone host does not stand.
	reads.Nodes = append(reads.Nodes, discNode("pve2", "online"))
	result, err = BuildDiscoveryResult(context.Background(), reads, discInput("pve1.dc1.example.com:8006", acceptAll))
	if err != nil || result.Outcome != DiscoveryOK || len(result.Nodes) != 1 {
		t.Fatalf("contradicting reads: outcome %q, %d nodes, err %v", result.Outcome, len(result.Nodes), err)
	}
}

func TestBuildDiscoveryResult_SingleNodeClusterHasNoCandidates(t *testing.T) {
	t.Parallel()
	reads := DiscoveryReads{
		Nodes:  []DiscoveryNodeRead{discNode("pve1", "online")},
		Status: quorateStatus("pve1", "pve1"),
		RootCA: discRoot,
	}
	result, err := BuildDiscoveryResult(context.Background(), reads, discInput("pve1.dc1.example.com:8006", acceptAll))
	if err != nil || result.Outcome != DiscoveryNoCandidates {
		t.Fatalf("outcome %q err %v, want no_candidates", result.Outcome, err)
	}
}

func TestBuildDiscoveryResult_NoResultFailures(t *testing.T) {
	t.Parallel()
	good := DiscoveryReads{Nodes: []DiscoveryNodeRead{discNode("pve1", "online")}, RootCA: discRoot}
	cases := map[string]struct {
		reads    func() DiscoveryReads
		resolver NameResolver
		want     error
	}{
		"empty nodes": {
			reads: func() DiscoveryReads { r := good; r.Nodes = nil; return r },
			want:  ErrDiscoveryNodes, resolver: acceptAll,
		},
		"name with a dot": {
			reads: func() DiscoveryReads {
				r := good
				r.Nodes = []DiscoveryNodeRead{discNode("pve1", "online"), discNode("api.attacker.example", "online")}
				return r
			},
			want: ErrDiscoveryNodeName, resolver: acceptAll,
		},
		"unnamed row": {
			reads: func() DiscoveryReads {
				r := good
				r.Nodes = []DiscoveryNodeRead{discNode("pve1", "online"), {Status: "online"}}
				return r
			},
			want: ErrDiscoveryNodeName, resolver: acceptAll,
		},
		"name too long": {
			reads: func() DiscoveryReads {
				r := good
				r.Nodes = []DiscoveryNodeRead{discNode(strings.Repeat("a", 64), "online")}
				return r
			},
			want: ErrDiscoveryNodeName, resolver: acceptAll,
		},
		"no root": {
			reads: func() DiscoveryReads { r := good; r.RootCA = [sha256.Size]byte{}; return r },
			want:  ErrDiscoveryRoot, resolver: acceptAll,
		},
		"no resolver": {reads: func() DiscoveryReads { return good }, want: ErrDiscoveryInput},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			result, err := BuildDiscoveryResult(context.Background(), tc.reads(), discInput("pve1.dc1.example.com:8006", tc.resolver))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(result.Nodes) != 0 || result.Outcome != "" {
				t.Fatalf("a failure must give no result, got %+v", result)
			}
			if strings.Contains(err.Error(), "attacker") {
				t.Fatalf("the error echoes a node name: %v", err)
			}
		})
	}
	var noContext context.Context
	if _, err := BuildDiscoveryResult(noContext, good, discInput("pve1.dc1.example.com:8006", acceptAll)); err == nil {
		t.Fatal("a nil context must fail")
	}
}

func TestBuildDiscoveryResult_AmbiguousNameLeavesOnlyThatNodeOut(t *testing.T) {
	t.Parallel()
	reads := DiscoveryReads{
		Nodes: []DiscoveryNodeRead{
			discNode("pve1", "online"), discNode("localhost", "online"), discNode("12345", "online"),
			discNode("0x7f000001", "online"), discNode("IP6-Loopback", "online"), discNode("pve2", "online"),
		},
		Status: quorateStatus("pve1", "pve1", "pve2"),
		RootCA: discRoot,
	}
	result, err := BuildDiscoveryResult(context.Background(), reads, discInput("pve1.dc1.example.com:8006", acceptAll))
	if err != nil {
		t.Fatalf("BuildDiscoveryResult: %v", err)
	}
	if got := nodeNames(result.Nodes); !slices.Equal(got, []string{"pve2"}) {
		t.Fatalf("nodes = %v, want pve2 alone", got)
	}
	for _, name := range []string{"localhost", "12345", "0x7f000001", "IP6-Loopback"} {
		if got := droppedFor(result, name); got != DropAmbiguousName {
			t.Fatalf("%s dropped for %q, want ambiguous_name", name, got)
		}
	}
}

func TestBuildDiscoveryResult_FingerprintProblemsLeaveTheNodeOut(t *testing.T) {
	t.Parallel()
	missing := discNode("pve2", "online")
	missing.Fingerprint = ""
	malformed := discNode("pve3", "online")
	malformed.Fingerprint = "AA:BB:CC"
	notHex := discNode("pve4", "online")
	notHex.Fingerprint = strings.Repeat("zz", 32)
	reads := DiscoveryReads{
		Nodes:  []DiscoveryNodeRead{discNode("pve1", "online"), missing, malformed, notHex, discNode("pve5", "online")},
		Status: quorateStatus("pve1", "pve1", "pve5"),
		RootCA: discRoot,
	}
	result, err := BuildDiscoveryResult(context.Background(), reads, discInput("pve1.dc1.example.com:8006", acceptAll))
	if err != nil {
		t.Fatalf("BuildDiscoveryResult: %v", err)
	}
	if got := nodeNames(result.Nodes); !slices.Equal(got, []string{"pve5"}) {
		t.Fatalf("nodes = %v, want pve5 alone", got)
	}
	for _, name := range []string{"pve2", "pve3", "pve4"} {
		if got := droppedFor(result, name); got != DropNoFingerprint {
			t.Fatalf("%s dropped for %q, want no_fingerprint", name, got)
		}
	}
}

func TestBuildDiscoveryResult_NodeWithoutCertPinsKeepsItsNodesPin(t *testing.T) {
	t.Parallel()
	reads := DiscoveryReads{
		Nodes:  []DiscoveryNodeRead{discNode("pve1", "online"), discNode("pve2", "online")},
		Status: quorateStatus("pve1", "pve1", "pve2"),
		RootCA: discRoot,
	}
	result, err := BuildDiscoveryResult(context.Background(), reads, discInput("pve1.dc1.example.com:8006", acceptAll))
	if err != nil {
		t.Fatalf("BuildDiscoveryResult: %v", err)
	}
	if len(result.Nodes) != 1 || !slices.Equal(result.Nodes[0].Pins, []endpointpool.Pin{discNodePin("pve2")}) {
		t.Fatalf("nodes = %+v, want pve2 pinned by its /nodes fingerprint alone", result.Nodes)
	}
}

func TestBuildDiscoveryResult_ResolverFailuresLeaveNamesOut(t *testing.T) {
	t.Parallel()
	reads := DiscoveryReads{
		Nodes: []DiscoveryNodeRead{
			discNode("pve1", "online"), discNode("pve2", "online"), discNode("pve3", "online"), discNode("pve4", "online"),
		},
		Status: quorateStatus("pve1", "pve1", "pve2", "pve3", "pve4"),
		RootCA: discRoot,
	}
	var asked sync.Map
	resolver := func(_ context.Context, host string) error {
		asked.Store(host, true)
		if strings.HasPrefix(host, "pve2.") || strings.HasPrefix(host, "pve4.") {
			return errors.New("no such host")
		}
		return nil
	}
	result, err := BuildDiscoveryResult(context.Background(), reads, discInput("pve1.dc1.example.com:8006", resolver))
	if err != nil {
		t.Fatalf("BuildDiscoveryResult: %v", err)
	}
	if got := nodeNames(result.Nodes); !slices.Equal(got, []string{"pve3"}) {
		t.Fatalf("nodes = %v, want pve3 alone", got)
	}
	for _, name := range []string{"pve2", "pve4"} {
		if got := droppedFor(result, name); got != DropUnresolved {
			t.Fatalf("%s dropped for %q, want unresolved", name, got)
		}
	}
	// The resolver gets the host alone and never the port, and never the
	// seed's own node or a name left out earlier.
	for _, host := range []string{"pve2.dc1.example.com", "pve3.dc1.example.com", "pve4.dc1.example.com"} {
		if _, ok := asked.Load(host); !ok {
			t.Fatalf("resolver was not asked for %s", host)
		}
	}
	if _, ok := asked.Load("pve1.dc1.example.com"); ok {
		t.Fatal("resolver was asked for the seed's own node")
	}
}

func TestBuildDiscoveryResult_AllNamesUnresolvedGivesNoCandidates(t *testing.T) {
	t.Parallel()
	reads := DiscoveryReads{
		Nodes:  []DiscoveryNodeRead{discNode("pve1", "online"), discNode("pve2", "online")},
		Status: quorateStatus("pve1", "pve1", "pve2"),
		RootCA: discRoot,
	}
	fail := func(context.Context, string) error { return errors.New("no such host") }
	result, err := BuildDiscoveryResult(context.Background(), reads, discInput("pve1:8006", fail))
	if err != nil || result.Outcome != DiscoveryNoCandidates || len(result.Nodes) != 0 {
		t.Fatalf("outcome %q, %d nodes, err %v, want no_candidates", result.Outcome, len(result.Nodes), err)
	}
}

func TestBuildDiscoveryResult_ResolvesInParallelWithATimeoutPerName(t *testing.T) {
	t.Parallel()
	const peers = 6
	nodes := []DiscoveryNodeRead{discNode("pve0", "online")}
	names := []string{"pve0"}
	for i := 1; i <= peers; i++ {
		name := fmt.Sprintf("pve%d", i)
		nodes = append(nodes, discNode(name, "online"))
		names = append(names, name)
	}
	reads := DiscoveryReads{Nodes: nodes, Status: quorateStatus("pve0", names...), RootCA: discRoot}
	var arrived atomic.Int32
	release := make(chan struct{})
	var once sync.Once
	// Every lookup waits for all the others, which only happens when they run
	// at the same time. A lookup that never gets its peers gives up when its
	// own timeout fires.
	resolver := func(ctx context.Context, _ string) error {
		if arrived.Add(1) == peers {
			once.Do(func() { close(release) })
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	in := discInput("pve0.dc1.example.com:8006", resolver)
	in.ResolveTimeout = 5 * time.Second
	result, err := BuildDiscoveryResult(context.Background(), reads, in)
	if err != nil || len(result.Nodes) != peers {
		t.Fatalf("%d nodes, err %v, want all %d resolved in parallel", len(result.Nodes), err, peers)
	}

	hang := func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	in = discInput("pve0.dc1.example.com:8006", hang)
	in.ResolveTimeout = 30 * time.Millisecond
	start := time.Now()
	result, err = BuildDiscoveryResult(context.Background(), reads, in)
	if err != nil || result.Outcome != DiscoveryNoCandidates {
		t.Fatalf("outcome %q err %v, want no_candidates after the timeouts", result.Outcome, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("hanging lookups took %v, want one timeout round", elapsed)
	}
}

func TestBuildDiscoveryResult_DefaultResolveTimeoutIsTwoSeconds(t *testing.T) {
	t.Parallel()
	if discoveryResolveTimeout != 2*time.Second {
		t.Fatalf("discoveryResolveTimeout = %v, want 2s", discoveryResolveTimeout)
	}
	reads := DiscoveryReads{
		Nodes:  []DiscoveryNodeRead{discNode("pve1", "online"), discNode("pve2", "online")},
		Status: quorateStatus("pve1", "pve1", "pve2"),
		RootCA: discRoot,
	}
	var budget time.Duration
	resolver := func(ctx context.Context, _ string) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			return errors.New("no deadline")
		}
		budget = time.Until(deadline)
		return nil
	}
	if _, err := BuildDiscoveryResult(context.Background(), reads, discInput("pve1:8006", resolver)); err != nil {
		t.Fatal(err)
	}
	if budget <= time.Second || budget > 2*time.Second {
		t.Fatalf("the lookup's budget was %v, want about two seconds", budget)
	}
}

func TestBuildDiscoveryResult_CanceledContextGivesNoResult(t *testing.T) {
	t.Parallel()
	reads := DiscoveryReads{
		Nodes:  []DiscoveryNodeRead{discNode("pve1", "online"), discNode("pve2", "online")},
		Status: quorateStatus("pve1", "pve1", "pve2"),
		RootCA: discRoot,
	}
	ctx, cancel := context.WithCancel(context.Background())
	resolver := func(context.Context, string) error {
		cancel()
		return errors.New("canceled")
	}
	result, err := BuildDiscoveryResult(ctx, reads, discInput("pve1:8006", resolver))
	if !errors.Is(err, context.Canceled) || len(result.Nodes) != 0 || result.Outcome != "" {
		t.Fatalf("result %+v err %v, want the cancellation and no result", result, err)
	}
}

func TestBuildDiscoveryResult_ConfiguredEndpointsAndRepeatsLeftOut(t *testing.T) {
	t.Parallel()
	reads := DiscoveryReads{
		Nodes: []DiscoveryNodeRead{
			discNode("pve1", "online"), discNode("pve2", "online"), discNode("pve3", "online"),
			discNode("PVE3", "online"), discNode("pve4", "online"),
		},
		Status: quorateStatus("pve1", "pve1", "pve2", "pve3", "pve4"),
		RootCA: discRoot,
	}
	in := discInput("pve1.dc1.example.com:8006", acceptAll)
	// A second configured endpoint, written in another case and with a
	// trailing dot, names pve2's address.
	in.Configured = append(in.Configured, "PVE2.dc1.example.com.:8006", "not an endpoint")
	result, err := BuildDiscoveryResult(context.Background(), reads, in)
	if err != nil {
		t.Fatalf("BuildDiscoveryResult: %v", err)
	}
	if got := nodeNames(result.Nodes); !slices.Equal(got, []string{"pve3", "pve4"}) {
		t.Fatalf("nodes = %v, want pve3 and pve4", got)
	}
	if got := droppedFor(result, "pve2"); got != DropConfigured {
		t.Fatalf("pve2 dropped for %q, want configured", got)
	}
	if got := droppedFor(result, "PVE3"); got != DropDuplicateName {
		t.Fatalf("PVE3 dropped for %q, want duplicate_name", got)
	}
}

func TestBuildDiscoveryResult_CapsTheListAtSixtyFourNodes(t *testing.T) {
	t.Parallel()
	const total = 100
	var rows []DiscoveryNodeRead
	var names []string
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("pve%03d", i)
		rows = append(rows, discNode(name, "online"))
		names = append(names, name)
	}
	reads := DiscoveryReads{Nodes: rows, Status: quorateStatus("pve000", names...), RootCA: discRoot}
	result, err := BuildDiscoveryResult(context.Background(), reads, discInput("pve000.dc1.example.com:8006", acceptAll))
	if err != nil {
		t.Fatalf("BuildDiscoveryResult: %v", err)
	}
	if len(result.Nodes) != maxDiscoveredNodes {
		t.Fatalf("%d nodes, want %d", len(result.Nodes), maxDiscoveredNodes)
	}
	if result.Nodes[0].Name != "pve001" || result.Nodes[63].Name != "pve064" {
		t.Fatalf("the cap kept %s through %s, want the first 64 in order", result.Nodes[0].Name, result.Nodes[63].Name)
	}
	if got := droppedFor(result, "pve065"); got != DropOverCap {
		t.Fatalf("pve065 dropped for %q, want over_cap", got)
	}

	// Names that do not resolve make room for later ones, and the resolver
	// is not asked about a node that is past the cap.
	var asked atomic.Int32
	resolver := func(_ context.Context, host string) error {
		asked.Add(1)
		for _, bad := range []string{"pve003.", "pve010.", "pve020."} {
			if strings.HasPrefix(host, bad) {
				return errors.New("no such host")
			}
		}
		return nil
	}
	result, err = BuildDiscoveryResult(context.Background(), reads, discInput("pve000.dc1.example.com:8006", resolver))
	if err != nil || len(result.Nodes) != maxDiscoveredNodes {
		t.Fatalf("%d nodes, err %v, want a full list after three failures", len(result.Nodes), err)
	}
	if result.Nodes[63].Name != "pve067" {
		t.Fatalf("last node = %s, want pve067", result.Nodes[63].Name)
	}
	if got := asked.Load(); got != 67 {
		t.Fatalf("resolver asked %d times, want 67 (64 places plus the 3 refills)", got)
	}
}

func TestLookupHostResolver(t *testing.T) {
	t.Parallel()
	if err := LookupHostResolver(nil)(context.Background(), "127.0.0.1"); err != nil {
		t.Fatalf("an address literal must resolve: %v", err)
	}
	broken := &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("no resolver in tests")
	}}
	if err := LookupHostResolver(broken)(context.Background(), "pve2.dc1.invalid-test-zone"); err == nil {
		t.Fatal("a name the resolver cannot answer must fail")
	}
}
