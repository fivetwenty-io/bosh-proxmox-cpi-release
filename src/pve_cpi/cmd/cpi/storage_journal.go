package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// This command runs explicitly, outside the JSON-RPC startup path. Directory
// provisioning never enrolls an authority and ordinary CPI calls never invoke it.
func runStorageJournal(args []string, stdout, stderr io.Writer, opts runOptions) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: cpi storage-journal <audit-enrollment|initialize|audit|recover-authority|recover-index|resolve-missing-vm|adopt|finalize-cleanup|cleanup> --config PATH")
		return 2
	}
	action := args[0]
	if !supportedStorageJournalAction(action) {
		fmt.Fprintln(stderr, "unknown storage-journal action")
		return 2
	}
	fs := flag.NewFlagSet("cpi storage-journal "+action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "CPI JSON configuration")
	authority := fs.String("authority-id", "", "stable operator identifier for this journal writer")
	remoteTasksSettled := fs.Bool("remote-tasks-settled", false, "attest all previous writer PVE tasks, including unknown-response submissions, settled before this audit")
	recoveredTaskEvidence := fs.String("recovered-task-evidence", "", "private retained original request/response receipt for this exact step")
	recoveredTaskStep := fs.String("recovered-task-step", "", "exact unknown step whose task response was independently recovered")
	recoveredTaskUPID := fs.String("recovered-task-upid", "", "independently recovered UPID, verified against the exact step and live PVE task")
	indexFirst := fs.Bool("index-first-crash-confirmed", false, "attest independent evidence no allocation mutation was submitted; missing file alone is insufficient")
	expectedCID := fs.String("expected-cid", "", "exact CID independently verified for adoption")
	decisionID := fs.String("decision-id", "", "nonsecret operator audit reference")
	agentID := fs.String("agent-id", "", "exact BOSH agent identifier for a missing indexed generation")
	allocationID := fs.String("allocation-id", "", "exact full allocation UUID")
	fenced := fs.Bool("previous-writer-fenced", false, "attest that no previous writer can use this namespace")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(stderr)
			fs.PrintDefaults()
			return 0
		}
		fmt.Fprintln(stderr, "invalid storage-journal flags")
		return 2
	}
	request := storageJournalMissingGeneration{AgentID: *agentID, AllocationID: *allocationID, IndexFirstConfirmed: *indexFirst, ExpectedCID: *expectedCID, DecisionID: *decisionID, RemoteTasksSettled: *remoteTasksSettled, RecoveredTaskStep: *recoveredTaskStep, RecoveredTaskUPID: *recoveredTaskUPID, RecoveredTaskEvidencePath: *recoveredTaskEvidence}
	if !validStorageJournalFlags(action, fs, *configPath, *authority, *fenced, request) {
		fmt.Fprintln(stderr, "config is required; mutations require authority-id and previous-writer-fenced; missing-generation recovery also requires agent-id, allocation-id and index-first-crash-confirmed; adoption/cleanup decisions require allocation-id and decision-id, and adoption requires expected-cid; initialize and recover-index require remote-tasks-settled")
		return 2
	}
	if request.RecoveredTaskEvidencePath != "" {
		evidence, err := readStorageRecoveredTaskEvidence(request.RecoveredTaskEvidencePath)
		if err != nil {
			fmt.Fprintln(stderr, "recovered task evidence could not be read or validated")
			return 1
		}
		request.RecoveredTaskEvidence = evidence
	}
	cfg, err := config.LoadFile(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "storage journal configuration could not be loaded")
		return 1
	}
	if err = cfg.ValidateStoragePlacementAllocation(); err != nil {
		fmt.Fprintln(stderr, "storage journal operation failed; inspect retained evidence")
		return 1
	}
	logger := log.NewNopLogger()
	factory := opts.ClientFactory
	if factory == nil {
		factory = pve.NewClientWithTracer
	}
	client, err := factory(cfg, logger, nil)
	if err != nil || client == nil {
		fmt.Fprintln(stderr, "storage journal PVE client unavailable")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	nodes, err := storageJournalNodes(ctx, client)
	if err != nil {
		fmt.Fprintln(stderr, "storage journal operation failed; inspect retained evidence")
		return 1
	}
	identity, err := pve.ObserveStorageClusterIdentity(ctx, client.Nodes(), nodes)
	if err != nil {
		fmt.Fprintln(stderr, "storage journal cluster identity unavailable")
		return 1
	}
	if action == "audit" || action == "recover-authority" || action == "recover-index" || action == "resolve-missing-vm" || action == "adopt" || action == "finalize-cleanup" || action == "cleanup" {
		return runStorageJournalRecovery(ctx, action, cfg, client, nodes, identity.ID(), *authority, *fenced, stdout, stderr, request)
	}
	report, err := handlers.AuditStorageAllocationEnrollment(ctx, handlers.Deps{Config: cfg, PVE: client, Logger: logger}, nodes)
	if err != nil {
		fmt.Fprintln(stderr, "storage journal enrollment audit failed")
		return 1
	}
	if action == "audit-enrollment" {
		if err = json.NewEncoder(stdout).Encode(report); err != nil {
			return 1
		}
		if !report.Complete || !report.VMScanComplete {
			return 1
		}
		return 0
	}
	if !report.Complete || !report.VMScanComplete || len(report.Conflicts) > 0 || len(report.Evidence) > 0 {
		fmt.Fprintln(stderr, "initialization refused: complete historical absence is unproven; inspect audit-enrollment output")
		return 1
	}
	auditID, err := aj.RetainAudit(cfg.StorageAllocationJournalDir, struct {
		Namespace, ClusterID string
		RemoteTasksSettled   bool
		Audit                handlers.StorageAllocationAudit
	}{cfg.StoragePlacementNamespace, identity.ID(), *remoteTasksSettled, report})
	if err != nil {
		fmt.Fprintln(stderr, "initialization refused: audit evidence could not be retained")
		return 1
	}
	journal, err := aj.Initialize(ctx, cfg.StorageAllocationJournalDir, cfg.StoragePlacementNamespace, aj.Enrollment{ClusterID: identity.ID(), AuthorityID: *authority, AuditID: auditID, CompleteHistoricalAudit: true, PreviousWriterFenced: *fenced})
	if err != nil {
		fmt.Fprintln(stderr, "storage journal operation failed; inspect retained evidence")
		return 1
	}
	if err = journal.Close(); err != nil {
		fmt.Fprintln(stderr, "journal initialized but close failed; inspect enrollment before retrying")
		return 1
	}
	// The retained audit already records the storages the absence proof did
	// not inspect. Surface them here as well, because an operator who runs
	// initialize without reading the audit-enrollment report would otherwise
	// enroll without learning that anything was skipped.
	if len(report.SkippedDisabledStorages) > 0 {
		fmt.Fprintf(stderr, "enrolled without inspecting %d disabled storage(s): %s; audit again after re-enabling any of them\n", len(report.SkippedDisabledStorages), strings.Join(report.SkippedDisabledStorages, ", "))
	}
	summary := map[string]any{"namespace": cfg.StoragePlacementNamespace, "cluster_id": identity.ID(), "audit_id": auditID, "state": "initialized"}
	if len(report.SkippedDisabledStorages) > 0 {
		summary["skipped_disabled_storages"] = report.SkippedDisabledStorages
	}
	if err = json.NewEncoder(stdout).Encode(summary); err != nil {
		return 1
	}
	return 0
}
func storageJournalNodes(ctx context.Context, client pve.Client) ([]string, error) {
	if client.Nodes() == nil {
		return nil, fmt.Errorf("storage journal node service unavailable")
	}
	response, err := client.Nodes().ListNodes(ctx)
	if err != nil || response == nil || len(*response) == 0 {
		return nil, fmt.Errorf("storage journal node enumeration unavailable")
	}
	var names []string
	for _, raw := range *response {
		var node struct {
			Node string `json:"node"`
		}
		if json.Unmarshal(raw, &node) != nil || node.Node == "" {
			return nil, fmt.Errorf("storage journal node enumeration malformed")
		}
		names = append(names, node.Node)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

func runStorageJournalRecovery(ctx context.Context, action string, cfg *config.CPIConfig, client pve.Client, nodes []string, clusterID, authority string, fenced bool, stdout, stderr io.Writer, missing ...storageJournalMissingGeneration) (code int) {
	enrolled, err := aj.InspectEnrollment(cfg.StorageAllocationJournalDir, cfg.StoragePlacementNamespace)
	if err != nil {
		fmt.Fprintln(stderr, "existing journal enrollment unavailable; restore retained authority before recovery")
		return 1
	}
	journal, err := aj.Open(cfg.StorageAllocationJournalDir, cfg.StoragePlacementNamespace, enrolled.Enrollment.ClusterID)
	if err != nil {
		fmt.Fprintln(stderr, "existing journal cannot be opened for audit")
		return 1
	}
	defer func() {
		if err := journal.Close(); err != nil {
			fmt.Fprintln(stderr, "journal close failed; inspect recovery state")
			code = 1
		}
	}()
	if action == "adopt" || action == "finalize-cleanup" || action == "cleanup" {
		if clusterID != enrolled.Enrollment.ClusterID || len(missing) != 1 {
			fmt.Fprintln(stderr, "allocation decision requires verified cluster continuity")
			return 1
		}
		applyDecision := handlers.ApplyStorageAllocationDecision
		if action == "cleanup" {
			applyDecision = handlers.CleanupStorageAllocation
		}
		record, err := applyDecision(ctx, handlers.Deps{Config: cfg, PVE: client, Logger: log.NewNopLogger()}, journal, nodes, handlers.StorageAllocationDecision{Action: action, AllocationID: missing[0].AllocationID, ExpectedCID: missing[0].ExpectedCID, DecisionID: missing[0].DecisionID, PreviousWriterFenced: fenced, RemoteTasksSettled: missing[0].RemoteTasksSettled, RecoveredTaskStep: missing[0].RecoveredTaskStep, RecoveredTaskUPID: missing[0].RecoveredTaskUPID, RecoveredTaskEvidence: missing[0].RecoveredTaskEvidence, AuthorityID: authority})
		if err != nil {
			fmt.Fprintf(stderr, "allocation decision refused (%s); inspect settled outcome, identity and audit evidence\n", handlers.StorageAllocationDecisionFailure(err))
			return 1
		}
		if err = json.NewEncoder(stdout).Encode(map[string]string{"allocation_id": record.ID, "state": string(record.State), "cid": record.CID}); err != nil {
			return 1
		}
		return 0
	}
	report, err := handlers.AuditStorageAllocations(ctx, handlers.Deps{Config: cfg, PVE: client}, journal, nodes)
	if err != nil {
		fmt.Fprintln(stderr, "retained journal audit failed")
		return 1
	}
	probe := "storage-journal-generation-health-probe"
	for recordIndex := range report.Records {
		record := report.Records[recordIndex]
		if record.Kind == "vm" {
			probe = record.AgentID
			break
		}
	}
	_, _, indexErr := journal.InspectVMContext(ctx, probe)
	if action == "resolve-missing-vm" {
		if len(missing) != 1 {
			return 2
		}
		return resolveStorageJournalMissingVM(ctx, cfg, journal, clusterID, enrolled.Enrollment.ClusterID, report, missing[0], authority, fenced, stdout, stderr)
	}
	if action == "audit" {
		return writeStorageJournalAudit(stdout, stderr, report, indexErr, clusterID == enrolled.Enrollment.ClusterID)
	}
	if action == "recover-index" && (len(missing) != 1 || !missing[0].RemoteTasksSettled) {
		fmt.Fprintln(stderr, "index recovery requires independent attestation that all previous remote tasks settled before audit")
		return 1
	}
	if !report.Complete || !report.VMScanComplete || len(report.Conflicts) > 0 || action == "recover-authority" && indexErr != nil {
		fmt.Fprintln(stderr, "recovery requires a complete consistent historical audit")
		return 1
	}
	concise, err := conciseStorageJournalAudit(report)
	if err != nil {
		fmt.Fprintln(stderr, "recovery record evidence invalid")
		return 1
	}
	auditID, err := aj.RetainAudit(cfg.StorageAllocationJournalDir, struct {
		Operation, Namespace, ClusterID string
		RemoteTasksSettled              bool
		Audit                           storageJournalRetainedAudit
	}{action, cfg.StoragePlacementNamespace, clusterID, action == "recover-index" && missing[0].RemoteTasksSettled, concise})
	if err != nil {
		fmt.Fprintln(stderr, "recovery audit could not be retained")
		return 1
	}
	var provenance []string
	for _, evidence := range report.Evidence {
		provenance = append(provenance, evidence.AllocationID)
	}
	slices.Sort(provenance)
	provenance = slices.Compact(provenance)
	enrollment := aj.Enrollment{ClusterID: clusterID, AuthorityID: authority, AuditID: auditID, CompleteHistoricalAudit: true, PreviousWriterFenced: fenced, ProvenanceIDs: provenance}
	if action == "recover-authority" {
		err = journal.RecoverAuthority(ctx, enrollment)
	} else {
		err = journal.RecoverIndex(ctx, enrollment)
	}
	if err != nil {
		fmt.Fprintln(stderr, "storage journal operation failed; inspect retained evidence")
		return 1
	}
	if err = json.NewEncoder(stdout).Encode(map[string]string{"namespace": cfg.StoragePlacementNamespace, "audit_id": auditID, "state": action + " completed"}); err != nil {
		return 1
	}
	return 0
}

type storageJournalMissingGeneration struct {
	AgentID, AllocationID, ExpectedCID, DecisionID string
	RecoveredTaskEvidencePath                      string
	RecoveredTaskEvidence                          *handlers.StorageRecoveredTaskEvidence
	RecoveredTaskStep, RecoveredTaskUPID           string
	IndexFirstConfirmed                            bool
	RemoteTasksSettled                             bool
}
type storageJournalRetainedAudit struct {
	Audit        handlers.StorageAllocationAudit `json:"audit"`
	RecordSHA256 map[string]string               `json:"record_sha256"`
}

func conciseStorageJournalAudit(report handlers.StorageAllocationAudit) (storageJournalRetainedAudit, error) {
	result := storageJournalRetainedAudit{Audit: report, RecordSHA256: map[string]string{}}
	result.Audit.Records = nil
	for recordIndex := range report.Records {
		record := report.Records[recordIndex]
		fingerprint, _, err := aj.VerificationEvidence(record)
		if err != nil {
			return storageJournalRetainedAudit{}, err
		}
		result.RecordSHA256[record.ID] = fingerprint
	}
	return result, nil
}
func resolveStorageJournalMissingVM(ctx context.Context, cfg *config.CPIConfig, journal *aj.Journal, clusterID, enrolledClusterID string, report handlers.StorageAllocationAudit, missing storageJournalMissingGeneration, authority string, fenced bool, stdout, stderr io.Writer) int {
	if !missing.IndexFirstConfirmed || !fenced || strings.TrimSpace(authority) == "" || clusterID != enrolledClusterID || !report.Complete || !report.VMScanComplete || len(report.Conflicts) != 0 || report.StartedAt.IsZero() || report.CompletedAt.Before(report.StartedAt) {
		fmt.Fprintln(stderr, "missing-generation recovery requires fencing, cluster continuity and a complete fresh audit")
		return 1
	}
	agentHash := sha256.Sum256([]byte(missing.AgentID))
	for _, evidence := range report.Evidence {
		if evidence.AllocationID == missing.AllocationID || evidence.AgentSHA256 == hex.EncodeToString(agentHash[:]) {
			fmt.Fprintln(stderr, "missing generation still has allocation or agent provenance; recovery refused")
			return 1
		}
	}
	concise, err := conciseStorageJournalAudit(report)
	if err != nil {
		fmt.Fprintln(stderr, "missing-generation audit evidence invalid")
		return 1
	}
	evidence := struct {
		Operation, Namespace, ClusterID, AllocationID, AgentSHA256, AuthorityID string
		PreviousWriterFenced, IndexFirstCrashConfirmed                          bool
		Audit                                                                   storageJournalRetainedAudit
	}{"resolve-missing-vm", cfg.StoragePlacementNamespace, clusterID, missing.AllocationID, hex.EncodeToString(agentHash[:]), authority, fenced, missing.IndexFirstConfirmed, concise}
	evidenceID, evidenceJSON, err := aj.VerificationEvidence(evidence)
	if err != nil {
		fmt.Fprintln(stderr, "missing-generation evidence could not be encoded")
		return 1
	}
	auditID, err := aj.RetainAudit(cfg.StorageAllocationJournalDir, evidence)
	if err != nil || auditID != evidenceID {
		fmt.Fprintln(stderr, "missing-generation evidence could not be retained")
		return 1
	}
	// An index-first crash cannot have submitted a mutation: the complete first
	// record must be durable before any mutation is permitted. This empty-target
	// plan is explicitly an audit reconstruction, never a resumable create plan.
	plan, err := json.Marshal(handlers.StorageAllocationPlan{Version: 1, Namespace: cfg.StoragePlacementNamespace, AllocationKey: missing.AgentID, PolicyFingerprint: evidenceID})
	if err != nil {
		fmt.Fprintln(stderr, "missing-generation reconstruction could not be encoded")
		return 1
	}
	intent := aj.Intent{IntentFingerprint: evidenceID, PolicyFingerprint: evidenceID, FrozenInputsFingerprint: evidenceID, PlanVersion: 1, Plan: plan}
	verification := aj.Verification{EvidenceID: evidenceID, EvidenceJSON: evidenceJSON, Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true}
	if err = journal.ResolveMissingVMGeneration(ctx, missing.AgentID, missing.AllocationID, intent, verification); err != nil {
		fmt.Fprintln(stderr, "missing-generation recovery refused; exact indexed record absence must be proven")
		return 1
	}
	if err = json.NewEncoder(stdout).Encode(map[string]string{"namespace": cfg.StoragePlacementNamespace, "allocation_id": missing.AllocationID, "audit_id": auditID, "state": "missing generation resolved as cleaned"}); err != nil {
		return 1
	}
	return 0
}

type storageJournalRecordSummary struct{ ID, Kind, State, CID, SHA256 string }

func validStorageJournalFlags(action string, fs *flag.FlagSet, configPath, authority string, fenced bool, request storageJournalMissingGeneration) bool {
	if request.RecoveredTaskStep != "" || request.RecoveredTaskUPID != "" || request.RecoveredTaskEvidencePath != "" {
		if action != "cleanup" || !request.RemoteTasksSettled || request.RecoveredTaskEvidencePath == "" || strings.TrimSpace(request.RecoveredTaskStep) != request.RecoveredTaskStep || request.RecoveredTaskStep == "" || len(request.RecoveredTaskStep) > 256 || request.RecoveredTaskUPID == "" || len(request.RecoveredTaskUPID) > 4096 || strings.ContainsAny(request.RecoveredTaskUPID, "\r\n\t ") {
			return false
		}
	}
	if fs.NArg() != 0 || configPath == "" {
		return false
	}
	switch action {
	case "initialize", "recover-authority", "recover-index", "resolve-missing-vm":
		if strings.TrimSpace(authority) == "" || !fenced {
			return false
		}
	}
	if action == "resolve-missing-vm" && (strings.TrimSpace(request.AgentID) == "" || strings.TrimSpace(request.AllocationID) == "" || !request.IndexFirstConfirmed) {
		return false
	}
	switch action {
	case "adopt", "finalize-cleanup", "cleanup":
		if strings.TrimSpace(request.AllocationID) == "" || strings.TrimSpace(request.DecisionID) == "" {
			return false
		}
	}
	if action == "cleanup" && request.RemoteTasksSettled && (!fenced || strings.TrimSpace(authority) == "") {
		return false
	}
	if action == "adopt" && request.ExpectedCID == "" {
		return false
	}
	return (action != "initialize" && action != "recover-index") || request.RemoteTasksSettled
}

func writeStorageJournalAudit(stdout, stderr io.Writer, report handlers.StorageAllocationAudit, indexErr error, continuity bool) int {
	concise, err := conciseStorageJournalAudit(report)
	if err != nil {
		fmt.Fprintln(stderr, "audit record summary unavailable")
		return 1
	}
	outputReport := report
	outputReport.Records = nil
	var summaries []storageJournalRecordSummary
	for rIndex := range report.Records {
		r := report.Records[rIndex]
		summaries = append(summaries, storageJournalRecordSummary{r.ID, r.Kind, string(r.State), r.CID, concise.RecordSHA256[r.ID]})
	}
	output := struct {
		Records           []storageJournalRecordSummary   `json:"records"`
		IndexHealthy      bool                            `json:"generation_index_healthy"`
		IndexFinding      string                          `json:"generation_index_finding,omitempty"`
		ClusterContinuity bool                            `json:"cluster_continuity"`
		Audit             handlers.StorageAllocationAudit `json:"audit"`
	}{summaries, indexErr == nil, "", continuity, outputReport}
	if indexErr != nil {
		output.IndexFinding = "generation index invalid or unavailable; record listing does not establish healthy authority"
	}
	if err = json.NewEncoder(stdout).Encode(output); err != nil {
		return 1
	}
	if !output.IndexHealthy || !output.ClusterContinuity || !report.Complete || !report.VMScanComplete {
		return 1
	}
	return 0
}

func supportedStorageJournalAction(action string) bool {
	return action == "audit-enrollment" || action == "initialize" || action == "audit" || action == "recover-authority" || action == "recover-index" || action == "resolve-missing-vm" || action == "adopt" || action == "finalize-cleanup" || action == "cleanup"
}
