package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const storagePlanRequestLimit = 1 << 20

func readStoragePlanRequest(r io.Reader) (handlers.StoragePlanDiagnosticRequest, error) {
	var request handlers.StoragePlanDiagnosticRequest
	data, err := io.ReadAll(io.LimitReader(r, storagePlanRequestLimit+1))
	if err != nil || len(data) > storagePlanRequestLimit {
		return request, errors.New("storage-plan: request unreadable or exceeds 1 MiB")
	}
	// Strict envelope validation avoids accidentally accepting a full credential
	// carrying CPI context as diagnostic input.
	var fields map[string]json.RawMessage
	if !uniqueStoragePlanEnvelope(data) || json.Unmarshal(data, &fields) != nil || len(fields) != 2 || fields["method"] == nil || fields["arguments"] == nil {
		return request, errors.New("storage-plan: expected JSON method and arguments only")
	}
	if json.Unmarshal(data, &request) != nil {
		return request, errors.New("storage-plan: invalid request")
	}
	if request.Method != "create_disk" && request.Method != "create_vm" {
		return request, errors.New("storage-plan: method must be create_disk or create_vm")
	}
	return request, nil
}
func loadStoragePlanDeps(path string) (handlers.Deps, error) {
	if path == "" {
		path = os.Getenv(envConfigPath)
	}
	if path == "" {
		path = defaultConfigPath
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		return handlers.Deps{}, errors.New("storage-plan: configuration unavailable or invalid")
	}
	logger := log.NewNopLogger()
	client, err := pve.NewClientWithTracer(cfg, logger, nil)
	if err != nil {
		return handlers.Deps{}, errors.New("storage-plan: PVE client initialization failed")
	}
	return handlers.Deps{Config: cfg, PVE: client, Logger: logger}, nil
}
func runStoragePlan(args []string, stdout, stderr io.Writer) int {
	return runStoragePlanWithLoader(args, stdout, stderr, loadStoragePlanDeps)
}
func runStoragePlanWithLoader(args []string, stdout, stderr io.Writer, load func(string) (handlers.Deps, error)) int {
	return runStoragePlanWithObserver(args, stdout, stderr, load, handlers.ObserveStoragePlanDiagnostics)
}
func runStoragePlanWithObserver(args []string, stdout, stderr io.Writer, load func(string) (handlers.Deps, error), observe func(context.Context, handlers.Deps, handlers.StoragePlanDiagnosticRequest) (*handlers.StoragePlanDiagnostic, error)) int {
	fs := flag.NewFlagSet("storage-plan", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("config", "", "CPI configuration file")
	requestPath := fs.String("request", "", "sanitized CPI request JSON file")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		fs.SetOutput(stderr)
		fs.PrintDefaults()
		return exitOK
	} else if err != nil || fs.NArg() != 0 || *requestPath == "" {
		_, _ = fmt.Fprintln(stderr, "usage: pve-cid storage-plan --request PATH [--config PATH] [--json]")
		return exitUsage
	}
	file, err := os.Open(*requestPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "storage-plan: request file unavailable")
		return exitError
	}
	request, readErr := readStoragePlanRequest(file)
	closeErr := file.Close()
	if readErr != nil {
		_, _ = fmt.Fprintln(stderr, readErr)
		return exitUsage
	}
	if closeErr != nil {
		_, _ = fmt.Fprintln(stderr, "storage-plan: request file close failed")
		return exitError
	}
	deps, err := load(*path)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "storage-plan: configuration or client unavailable")
		return exitError
	}
	report, err := observe(context.Background(), deps, request)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "storage-plan: diagnostic observation failed")
		return exitError
	}
	if *asJSON {
		return writeJSON(stdout, stderr, report)
	}
	tracked := &storagePlanWriter{writer: stdout}
	stdout = tracked
	_, _ = fmt.Fprintln(stdout, "Storage planning observation only; no capacity is reserved.")
	_, _ = fmt.Fprintf(stdout, "Method: %s\nObserved: %s\nChosen node: %s\nSampled allocation key: %s\nSeed: %s\n", report.Method, report.ObservedAt.Format("2006-01-02T15:04:05Z07:00"), report.Node, report.AllocationKey, report.Seed)
	for _, s := range report.Selectors {
		_, _ = fmt.Fprintf(stdout, "Selector %s: %s=%s from %s.%s; boundary=%s encrypted=%t reserve_mb=%d ceiling=%v\n", s.Role, s.Kind, s.Value, s.Layer, s.Property, s.Boundary, s.Encrypted, s.ReserveMB, ceilingValue(s.CeilingPct))
	}
	names := make([]string, 0, len(report.FrozenMembership))
	for n := range report.FrozenMembership {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		_, _ = fmt.Fprintf(stdout, "Frozen set %s: %s\n", n, strings.Join(report.FrozenMembership[n], ", "))
	}
	for cIndex := range report.Capacities {
		c := report.Capacities[cIndex]
		_, _ = fmt.Fprintf(stdout, "Capacity %s/%s: available=%d total=%d age_ms=%d backing=%s domain=%s reason=%s\n", c.Pair.Node, c.Pair.StorageID, c.Pair.AvailableBytes, c.Pair.TotalBytes, c.AgeMilliseconds, c.Pair.BackingKey, c.Domain, c.Pair.Reason)
	}
	for _, d := range report.Domains {
		_, _ = fmt.Fprintf(stdout, "Domain %s: members=%s available=%d total=%d reason=%s\n", d.Name, strings.Join(d.Members, ","), d.AvailableBytes, d.TotalBytes, d.Reason)
	}
	names = names[:0]
	for n := range report.Strategies {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := report.Strategies[n]
		_, _ = fmt.Fprintf(stdout, "Strategy %s: %s v%d\n", n, p.Name, p.Version)
	}
	for i := range report.CandidateRanks {
		candidate := report.CandidateRanks[i]
		r := candidate.Ranking
		_, _ = fmt.Fprintf(stdout, "Candidate context: role=%s node=%s\n", candidate.Role, candidate.Node)
		_, _ = fmt.Fprintf(stdout, "Rank %d: %s backing=%s domain=%s score=%.17g domain_score=%.17g rendezvous=%x member_weight=%d domain_weight=%d reason=%s\n", i+1, r.Candidate.StorageID, r.Candidate.BackingKey, r.Candidate.DomainKey, r.Score, r.DomainScore, r.Rendezvous, r.MemberWeight, r.DomainWeight, r.Reason)
	}
	for tIndex := range report.Targets {
		t := report.Targets[tIndex]
		_, _ = fmt.Fprintf(stdout, "Target %s: %s/%s mechanism=%s bytes=%d backing=%s domain=%s\n", t.Role, t.Node, t.StorageID, t.Mechanism, t.ChargeBytes, t.BackingKey, t.DomainKey)
	}
	for _, r := range report.Rejections {
		_, _ = fmt.Fprintf(stdout, "Rejected: %s\n", r)
	}
	for _, r := range report.Journal {
		_, _ = fmt.Fprintf(stdout, "Journal %s: %s %s CID=%s\n", r.AllocationID, r.Kind, r.State, r.CID)
	}
	for _, f := range report.Findings {
		_, _ = fmt.Fprintf(stdout, "Finding: %s\n", f)
	}
	if tracked.err != nil {
		_, _ = fmt.Fprintln(stderr, "storage-plan: output write failed")
		return exitError
	}
	return exitOK
}

func ceilingValue(p *int) any {
	if p == nil {
		return "unset"
	}
	return *p
}

func uniqueStoragePlanEnvelope(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return false
		}
		seen[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
	}
	end, err := decoder.Token()
	return err == nil && end == json.Delim('}')
}

type storagePlanWriter struct {
	writer io.Writer
	err    error
}

func (w *storagePlanWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}
