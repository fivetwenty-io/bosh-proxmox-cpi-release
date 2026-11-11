package pve

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// coldStartCandidateLimit is the most cold-start candidates a client hands its
// pool. It is the pool's own cap, which refuses a longer list.
const coldStartCandidateLimit = 64

// coldStartProbeBodyLimit bounds the /cluster/status answer the prober reads.
// A cluster of the largest size PVE supports answers in a few tens of
// kilobytes, so a longer body is refused rather than read.
const coldStartProbeBodyLimit = 1 << 20

// hintWorkerName names a cold-start hint write in the drain's result.
const hintWorkerName = "cold-start hint"

// coldStartInputs is what a client learned about its cold start when it read
// the endpoint cache. The pool's candidates are built from it once the pool's
// endpoints are settled, because only then is it known how many discovered
// endpoints the pool loads.
type coldStartInputs struct {
	// names are the entry's az_map names that config.ColdStartNodeNames
	// returned and that pass the node-name helper, in its order. They are
	// empty when any config condition for the cold start fails.
	names []string
	// hint is the node a cold_start hint in the cache names, or empty. The
	// read already dropped a hint whose node names do not hold.
	hint string
	// outcome is the discovery outcome the cache file records, or empty when
	// the file is missing, fails validation, or holds no discovery result.
	outcome string
	// seed is the first configured endpoint, whose domain and port every
	// candidate's address takes.
	seed string
	// configured are the configured endpoints, which no candidate repeats.
	configured []string
	// userAgent is the User-Agent the client puts on every request, which
	// the probe carries too.
	userAgent string
}

// coldStartNames returns the node names of cfg's az_map that the cold start
// may try, which are the names config.ColdStartNodeNames returns for cfg under
// job that pass endpointcache.ValidateNodeName. A name that fails is left out
// with a Debug line, so a value such as pve2.example.com never becomes an
// address. job is the job-level config cfg was built from, or cfg itself for
// the job-level client.
func coldStartNames(cfg, job *config.CPIConfig, logger *log.Logger) []string {
	names := config.ColdStartNodeNames(cfg, job)
	valid := make([]string, 0, len(names))
	for _, name := range names {
		if err := endpointcache.ValidateNodeName(name); err != nil {
			logger.LogAttrs(context.Background(), log.LevelDebug,
				"pve endpoints: the cold start from placement.az_map leaves out a node whose name is not a usable "+
					"node name: "+err.Error(), log.String("node", name))
			continue
		}
		valid = append(valid, name)
	}
	return valid
}

// coldStartCandidates derives the address of each name in in.names under the
// seed, leaves out a name whose address cannot be derived and an address that
// normalizes to a configured endpoint or to an earlier candidate, moves the
// hinted node to the front, and keeps at most coldStartCandidateLimit. Each
// name that is left out logs one Debug line.
func coldStartCandidates(in coldStartInputs, logger *log.Logger) []endpointpool.Candidate {
	ctx := context.Background()
	deriver, err := endpointcache.NewDeriver(in.seed)
	if err != nil {
		logger.LogAttrs(ctx, log.LevelDebug, "pve endpoints: the cold start from placement.az_map has no "+
			"candidates, because no node address derives from the first configured endpoint: "+err.Error(),
			log.String("endpoint", in.seed))
		return nil
	}
	seen := normalizedEndpointSet(in.configured)
	var out []endpointpool.Candidate
	for _, name := range in.names {
		address, err := deriver.Address(name)
		if err != nil {
			logger.LogAttrs(ctx, log.LevelDebug, "pve endpoints: the cold start from placement.az_map leaves out "+
				"a node whose address cannot be derived: "+err.Error(), log.String("node", name))
			continue
		}
		key := normalizedEndpoint(address)
		if key == "" || seen[key] {
			logger.LogAttrs(ctx, log.LevelDebug, "pve endpoints: the cold start from placement.az_map leaves out "+
				"a node whose address is a configured endpoint or another node's", log.String("node", name),
				log.String("endpoint", address))
			continue
		}
		seen[key] = true
		out = append(out, endpointpool.Candidate{Node: name, Address: address})
	}
	if i := slices.IndexFunc(out, func(c endpointpool.Candidate) bool { return c.Node == in.hint }); i > 0 {
		hinted := out[i]
		out = append(out[:i], out[i+1:]...)
		out = append([]endpointpool.Candidate{hinted}, out...)
	}
	if len(out) > coldStartCandidateLimit {
		logger.LogAttrs(ctx, log.LevelDebug, fmt.Sprintf("pve endpoints: the cold start from placement.az_map "+
			"keeps the first %d of %d candidates", coldStartCandidateLimit, len(out)))
		out = out[:coldStartCandidateLimit]
	}
	return out
}

// attachColdStart gives the pool config its cold-start candidates and the
// prober, when endpointpool.Gate says the pool carries them. It runs once the
// pool's endpoints are settled, so the count of usable discovered entries is
// the number of cached nodes the pool loads. A cache that is nil, which is a
// call without a usable cache directory, carries none.
func (c *endpointCache) attachColdStart(cfg *endpointpool.Config) {
	if c == nil || len(c.cold.names) == 0 {
		return
	}
	if !endpointpool.Gate(c.cold.names, len(c.discovered), c.cold.outcome) {
		c.logger.LogAttrs(context.Background(), log.LevelDebug, "pve endpoints: no cold start from "+
			"placement.az_map, because the cache holds usable discovered nodes or its last discovery found a "+
			"standalone host", log.Int("discovered", len(c.discovered)), log.String("outcome", c.cold.outcome))
		return
	}
	candidates := coldStartCandidates(c.cold, c.logger)
	if len(candidates) == 0 {
		return
	}
	cfg.Candidates = candidates
	cfg.Prober = &coldStartProber{
		names:     c.cold.names,
		userAgent: c.cold.userAgent,
		level:     endpointcache.Level(c.mode),
		logger:    c.logger,
		accepted:  c.recordColdStart,
	}
}

// recordColdStart writes a cold_start hint for node through the process
// registry, so the exit drain covers the write, and returns at once. A
// registry that has started its drain refuses the write, and the cache then
// misses the hint, which only orders the next process's candidates.
func (c *endpointCache) recordColdStart(node string) {
	if c == nil || c.sink == nil {
		return
	}
	at := c.sink.now()
	err := c.sink.start(hintWorkerName, c.mode, func(ctx context.Context) { c.writeHint(ctx, node, at) })
	if err != nil {
		c.logger.LogAttrs(context.Background(), log.LevelDebug, "endpoint cache: the cold-start hint is not "+
			"recorded", log.String("node", node), log.Err(err))
	}
}

// writeHint records the hint under the key's lock. A failure is logged at the
// level the failure rules give a cache line.
func (c *endpointCache) writeHint(ctx context.Context, node string, at time.Time) {
	if ctx.Err() != nil {
		return
	}
	change := endpointcache.Change{
		Hint: &endpointcache.ColdStartHint{Name: node, ProbedAt: at},
		Pool: c.sink.pool,
	}
	_, err := c.sink.store.UpdatePayload(ctx, c.sink.now(), func(endpointcache.Stored) (endpointcache.Change, error) {
		return change, nil
	})
	if err != nil && ctx.Err() == nil {
		c.logger.LogAttrs(ctx, endpointcache.Level(c.mode), "endpoint cache: the cold-start hint could not be "+
			"recorded", log.String("node", node), log.Err(err))
	}
}

// coldStartProber proves a cold-start candidate's membership with GET
// /cluster/status. It accepts a candidate only on a 2xx whose one local node
// is the candidate's az_map name and whose node rows hold every name in names.
type coldStartProber struct {
	// names are the entry's az_map names that passed the node-name helper.
	names     []string
	userAgent string
	// level is the level of a refusal line, Debug under auto and Warn under
	// an explicit on.
	level  slog.Level
	logger *log.Logger
	// accepted is told the node of a candidate whose probe proved
	// membership, before Probe returns.
	accepted func(node string)

	// mu guards said, the candidates this prober has logged a refusal for.
	// A pool runs one cold start per process and probes each candidate to an
	// answer at most once, so the prober of a client logs each refusal once
	// per call. Another client in the process has its own prober and its own
	// line, at its own mode's level.
	mu   sync.Mutex
	said map[string]bool
}

// firstRefusal reports whether this prober has not yet logged a refusal of
// the candidate, and records that it has.
func (p *coldStartProber) firstRefusal(c endpointpool.Candidate) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := c.Address + " " + c.Node
	if p.said[key] {
		return false
	}
	if p.said == nil {
		p.said = map[string]bool{}
	}
	p.said[key] = true
	return true
}

var _ endpointpool.Prober = (*coldStartProber)(nil)

// Probe sends the membership probe through req.Transport and returns nil only
// when the answer proves membership. A refusal logs one line per candidate per
// call. A probe that ended because the request that started it ended logs at
// Debug and is not counted as that line, because the pool hands such a
// candidate to the next request that reaches the cold start.
func (p *coldStartProber) Probe(ctx context.Context, req endpointpool.ProbeRequest) error {
	err := p.probe(ctx, req)
	if err == nil {
		if p.accepted != nil {
			p.accepted(req.Candidate.Node)
		}
		return nil
	}
	reason := coldStartRefusalReason(err)
	if requestEnded(ctx) {
		p.logger.LogAttrs(context.Background(), log.LevelDebug, "pve endpoint "+req.Candidate.Address+
			" from placement.az_map: the membership probe ended with its request: "+reason,
			log.String("endpoint", req.Candidate.Address), log.String("node", req.Candidate.Node))
		return err
	}
	if p.firstRefusal(req.Candidate) {
		p.logger.LogAttrs(context.Background(), p.level, "pve endpoint "+req.Candidate.Address+
			" from placement.az_map refused: "+reason,
			log.String("endpoint", req.Candidate.Address), log.String("node", req.Candidate.Node))
	}
	return err
}

// requestEnded reports whether ctx ended because the request that began the
// probe ended. The pool gives the probe's context its own time limit with a
// cause of its own, so a context that ended with the plain cancellation or
// deadline cause ended with its request, and one that ended on the probe's
// limit did not.
func requestEnded(ctx context.Context) bool {
	if ctx.Err() == nil {
		return false
	}
	cause := context.Cause(ctx)
	return errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)
}

// The refusals of an answer that does not prove membership. None of them
// echoes the answer, because it comes from a host the operator never listed.
var (
	errColdStartStatus  = errors.New("the membership probe was not answered with a success")
	errColdStartBody    = errors.New("the membership probe's answer could not be read as a cluster status")
	errColdStartLocal   = errors.New("the node reports itself as another node")
	errColdStartMissing = errors.New("the cluster's node list does not hold every node placement.az_map names")
	errColdStartRequest = errors.New("the membership probe could not be built")
)

// probe sends the probe and checks the answer. The response body is read in
// full, up to coldStartProbeBodyLimit, and closed before probe returns.
func (p *coldStartProber) probe(ctx context.Context, req endpointpool.ProbeRequest) error {
	if req.Transport == nil || req.Authorization == "" || req.Candidate.Address == "" {
		return errColdStartRequest
	}
	target := url.URL{Scheme: "https", Host: req.Candidate.Address, Path: endpointpool.ProbePath}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		return fmt.Errorf("%w: %w", errColdStartRequest, err)
	}
	httpReq.Header.Set("Authorization", req.Authorization)
	httpReq.Header.Set("Accept", "application/json")
	if p.userAgent != "" {
		httpReq.Header.Set("User-Agent", p.userAgent)
	}
	resp, err := req.Transport.RoundTrip(httpReq)
	if err != nil {
		return err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, coldStartProbeBodyLimit+1))
	closeErr := resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%w (HTTP %d)", errColdStartStatus, resp.StatusCode)
	}
	switch {
	case readErr != nil:
		return fmt.Errorf("%w: %w", errColdStartBody, readErr)
	case closeErr != nil:
		return fmt.Errorf("%w: %w", errColdStartBody, closeErr)
	case len(body) > coldStartProbeBodyLimit:
		return fmt.Errorf("%w: it is longer than %d bytes", errColdStartBody, coldStartProbeBodyLimit)
	}
	return p.checkStatus(body, req.Candidate.Node)
}

// coldStartStatusRow is one row of the /cluster/status answer the probe reads.
type coldStartStatusRow struct {
	Type  string            `json:"type"`
	Name  string            `json:"name"`
	Local sdkclient.PVEBool `json:"local"`
}

// checkStatus checks a /cluster/status answer. Exactly one node row must be
// marked local, and its name must be node. The node rows must hold every name
// in p.names, compared exactly. A row that does not decode refuses the whole
// answer, because a row that cannot be read could be one that decides it.
func (p *coldStartProber) checkStatus(body []byte, node string) error {
	var answer struct {
		Data *[]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || answer.Data == nil {
		return errColdStartBody
	}
	members := map[string]bool{}
	locals := 0
	localIsNode := false
	for _, raw := range *answer.Data {
		var row coldStartStatusRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return errColdStartBody
		}
		if row.Type != resourceTypeNode {
			continue
		}
		members[row.Name] = true
		if row.Local.Bool() {
			locals++
			localIsNode = row.Name == node
		}
	}
	if locals != 1 || !localIsNode {
		return errColdStartLocal
	}
	var missing []string
	for _, name := range p.names {
		if !members[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w; it lacks %s", errColdStartMissing, strings.Join(missing, ", "))
	}
	return nil
}

// coldStartRefusalReason describes why a probe refused its candidate, in the
// words of the refusal line. A certificate failure names its kind, and every
// other cause is scrubbed of credentials.
func coldStartRefusalReason(err error) string {
	var (
		hostErr      x509.HostnameError
		authorityErr x509.UnknownAuthorityError
		invalidErr   x509.CertificateInvalidError
		verifyErr    *tls.CertificateVerificationError
	)
	switch {
	case errors.As(err, &hostErr):
		return "certificate is not valid for that name"
	case errors.As(err, &authorityErr):
		return "certificate does not chain to pve.ca_cert"
	case errors.As(err, &invalidErr):
		return "certificate is not valid: " + log.ScrubMessage(invalidErr.Error())
	case errors.As(err, &verifyErr):
		return "certificate verification failed: " + log.ScrubMessage(verifyErr.Error())
	case errors.Is(err, errColdStartStatus), errors.Is(err, errColdStartBody), errors.Is(err, errColdStartLocal),
		errors.Is(err, errColdStartMissing), errors.Is(err, errColdStartRequest):
		return err.Error()
	default:
		return "the membership probe failed: " + log.ScrubMessage(err.Error())
	}
}
