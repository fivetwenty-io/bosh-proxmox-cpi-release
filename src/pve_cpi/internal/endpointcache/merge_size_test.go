package endpointcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// largestLegal is the largest file the config's bounds allow. The config holds
// 16 endpoints, each with a host of 253 characters, and the seed's domain is
// 189 characters, so a node of 63 characters derives a host of 253 as well.
// Every endpoint, node, and extra pool candidate is marked, and the times
// carry nanoseconds, their longest spelling.
type largestLegal struct {
	cfg    KeyConfig
	change Change
	// newestFirst lists every mark the change carries, newest first, with no
	// two sharing a time.
	newestFirst []string
}

func newLargestLegal(t *testing.T) largestLegal {
	t.Helper()
	const port = 65535
	domain := strings.Repeat("d", 63) + "." + strings.Repeat("e", 63) + "." + strings.Repeat("f", 57) + ".com"
	label := func(prefix string, i int) string {
		name := fmt.Sprintf("%s%02d", prefix, i)
		return name + strings.Repeat("x", MaxNodeNameLength-len(name))
	}
	endpoints := make([]string, 0, 16)
	for i := range 16 {
		host := label("c", i) + "." + domain
		if len(host) != 253 {
			t.Fatalf("configured host is %d characters, want 253", len(host))
		}
		endpoints = append(endpoints, net.JoinHostPort(host, strconv.Itoa(port)))
	}
	cfg := storeTestConfig()
	cfg.Endpoints = endpoints

	at := payloadNow.Add(-123456789)
	result := &DiscoveryResult{Cluster: payloadCluster(at), Discovery: payloadDiscovery()}
	result.Cluster.ObservedVia = endpoints[0]
	result.Discovery.DiscoveredAt = at
	result.Discovery.NextRefreshAfter = at.Add(time.Hour)
	result.Discovery.Outcome = OutcomeNoStatus
	marks := make(map[string]DownMark)
	var newestFirst []string
	mark := func(endpoint string) {
		// One second apart keeps every mark inside MarkTTL and gives each its own
		// place in the order.
		marks[endpoint] = DownMark{
			DownAt: at.Add(-time.Duration(len(newestFirst)) * time.Second),
			Class:  MarkTLSHandshake,
		}
		newestFirst = append(newestFirst, endpoint)
	}
	pool := make([]string, 0, 8)
	for i := range 8 {
		endpoint := net.JoinHostPort(label("z", i)+"."+domain, strconv.Itoa(port))
		pool = append(pool, endpoint)
		mark(endpoint)
	}
	for i := range MaxNodes {
		name := label("n", i)
		address, err := DeriveAddress(endpoints[0], name)
		if err != nil {
			t.Fatal(err)
		}
		host, _, _ := net.SplitHostPort(address)
		result.Discovery.Nodes = append(result.Discovery.Nodes, DiscoveredNode{
			Name: name, Address: host, Port: port, ConfirmedAt: at,
			Pins: []string{payloadPin(fmt.Sprintf("%02x", i)), payloadPin("a1"), payloadPin("9e")},
		})
		mark(address)
	}
	// The configured endpoints are the oldest marks.
	for _, endpoint := range endpoints {
		mark(endpoint)
	}
	return largestLegal{cfg: cfg, change: Change{Result: result, Marks: marks, Pool: pool}, newestFirst: newestFirst}
}

func TestStoreWritesTheLargestConfigWithinTheSizeCap(t *testing.T) {
	big := newLargestLegal(t)
	dir := newStoreDir(t)
	s := openStoreFor(t, dir, big.cfg)

	res, err := s.UpdatePayload(
		context.Background(),
		payloadNow,
		func(Stored) (Change, error) { return big.change, nil },
	)
	if err != nil || !res.Written || len(res.Dropped) != 0 {
		t.Fatalf("UpdatePayload: %+v, %v", res, err)
	}
	info, err := os.Stat(filepath.Join(dir, s.FileName()))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the largest legal file is %d of %d bytes", info.Size(), maxFileBytes)
	if info.Size() > maxFileBytes {
		t.Errorf("the file is %d bytes, more than 64 KiB", info.Size())
	}
	// The trim stops as soon as the file fits, so it leaves less than one
	// mark's room, which is about 400 bytes with this config's hosts.
	if info.Size() < maxFileBytes-1024 {
		t.Errorf("the file is %d bytes, so the trim dropped more than it had to", info.Size())
	}
	got, drops, err := s.ReadPayload(payloadNow, nil)
	if err != nil || len(drops) != 0 {
		t.Fatalf("ReadPayload: %v, drops %v", err, drops)
	}
	if len(got.Discovery.Nodes) != MaxNodes || got.Cluster == nil {
		t.Errorf("%d nodes, cluster %v, want %d nodes and a cluster", len(got.Discovery.Nodes), got.Cluster, MaxNodes)
	}
	if len(got.Marks) == 0 || len(got.Marks) >= len(big.change.Marks) {
		t.Errorf("%d marks kept of %d, want the trim to keep some and drop some", len(got.Marks), len(big.change.Marks))
	}
}

func TestMergeDropsTheOldestMarksFirstToFit(t *testing.T) {
	big := newLargestLegal(t)
	key := openStoreFor(t, newStoreDir(t), big.cfg).Key()

	next, _, err := Merge(nil, key, big.change, payloadNow)
	if err != nil {
		t.Fatal(err)
	}
	size, err := payloadSize(next)
	if err != nil || size+envelopeMargin > maxFileBytes {
		t.Fatalf("the merged payload is %d bytes plus the envelope, error %v", size, err)
	}
	// The mark cap keeps the 64 newest other marks, and the size trim then
	// keeps a prefix of what is left in time order, configured endpoints
	// included, so the survivors are exactly the newest ones.
	capped := slices.Concat(big.newestFirst[:MaxOtherMarks], big.newestFirst[MaxNodes+8:])
	kept := len(next.Marks)
	if kept == 0 || kept >= len(capped) {
		t.Fatalf("%d marks kept of %d, want the trim to drop some and keep some", kept, len(capped))
	}
	for i, endpoint := range capped {
		if _, ok := next.Marks[endpoint]; ok != (i < kept) {
			t.Errorf("mark %d of %d, for %s, kept %v with %d kept", i, len(capped), endpoint[:12], ok, kept)
		}
	}
	// The oldest are configured endpoints, and the trim took some of them.
	if _, ok := next.Marks[big.cfg.Endpoints[len(big.cfg.Endpoints)-1]]; ok {
		t.Error("the oldest configured endpoint's mark survived the trim")
	}
	// One more mark would not fit, so the trim dropped no more than it had to.
	more := next.clone()
	more.Marks[capped[kept]] = big.change.Marks[capped[kept]]
	if size, err := payloadSize(more); err != nil || size+envelopeMargin <= maxFileBytes {
		t.Errorf("the next mark fits, %d bytes, error %v", size, err)
	}
}

func TestMergeKeepsEveryMarkOfANormalFile(t *testing.T) {
	cfg := storeTestConfig()
	dir := newStoreDir(t)
	s := openStoreFor(t, dir, cfg)
	result := mergeResult()
	marks := map[string]DownMark{}
	for i := range MaxNodes {
		name := fmt.Sprintf("pve%02d", i+2)
		node := payloadNode(name, payloadNow)
		result.Discovery.Nodes = append(result.Discovery.Nodes, node)
		marks[net.JoinHostPort(node.Address, strconv.Itoa(node.Port))] = DownMark{
			DownAt: payloadNow.Add(-time.Duration(i) * time.Second), Class: MarkDial,
		}
	}
	for _, endpoint := range cfg.Endpoints {
		marks[endpoint] = DownMark{DownAt: payloadNow.Add(-100 * time.Second), Class: MarkDNS}
	}
	// The key's first endpoint derives the nodes, so the result names it.
	result.Cluster.ObservedVia = cfg.Endpoints[0]
	if _, err := s.UpdatePayload(context.Background(), payloadNow, func(Stored) (Change, error) {
		return Change{Result: result, Marks: marks}, nil
	}); err != nil {
		t.Fatal(err)
	}
	got, drops, err := s.ReadPayload(payloadNow, nil)
	if err != nil || len(drops) != 0 {
		t.Fatalf("ReadPayload: %v, drops %v", err, drops)
	}
	if len(got.Marks) != len(marks) {
		t.Errorf("%d marks kept of %d", len(got.Marks), len(marks))
	}
	for endpoint, want := range marks {
		if got.Marks[endpoint] != want {
			t.Errorf("mark for %s is %+v, want %+v", endpoint, got.Marks[endpoint], want)
		}
	}
}

func TestEnvelopeMarginCoversTheEnvelope(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	putResult(t, s, payloadNow, mergeResult("pve2"))
	var stored Payload
	if err := s.Read(&stored); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(&stored)
	if err != nil {
		t.Fatal(err)
	}
	size, err := payloadSize(&stored)
	if err != nil || size != len(payload) {
		t.Fatalf("payloadSize is %d, %v, want %d", size, err, len(payload))
	}
	info, err := os.Stat(filepath.Join(dir, s.FileName()))
	if err != nil {
		t.Fatal(err)
	}
	overhead := int(info.Size()) - len(payload)
	if overhead > envelopeMargin || overhead < envelopeMargin-32 {
		t.Errorf(
			"the envelope adds %d bytes, want at most the margin %d and not far under it",
			overhead,
			envelopeMargin,
		)
	}
}

func TestMergeRefusesAPayloadThatCannotMarshal(t *testing.T) {
	key := openStoreFor(t, newStoreDir(t), storeTestConfig()).Key()
	result := mergeResult("pve2")
	result.Cluster.ObservedVia = key.Endpoints[0]
	// A time past year 9999 has no JSON form.
	result.Cluster.ObservedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	next, _, err := Merge(nil, key, Change{Result: result}, payloadNow)
	if err == nil || next != nil || errors.Is(err, ErrInvalidChange) {
		t.Errorf("Merge: %+v, %v, want a marshal error", next, err)
	}
}
