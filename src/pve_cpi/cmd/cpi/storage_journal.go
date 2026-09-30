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

const storageJournalUsage = "usage: cpi storage-journal <audit-enrollment|initialize|audit|recover-authority|recover-index|resolve-missing-vm|adopt|finalize-cleanup|cleanup> --config PATH"

// This command runs explicitly, outside the JSON-RPC startup path. Directory
// provisioning never enrolls an authority and ordinary CPI calls never invoke it.
func runStorageJournal(args []string, stdout, stderr io.Writer, opts runOptions) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, storageJournalUsage)
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
	textSummary := fs.Bool("summary", false, "audit and audit-enrollment only: print one line per finding instead of JSON; the exit code is unchanged")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(stderr)
			fs.PrintDefaults()
			return 0
		}
		// The flag package's text names only what the operator typed.
		fmt.Fprintln(stderr, "invalid storage-journal flags: "+boundStorageJournalText(err.Error()))
		fmt.Fprintln(stderr, storageJournalUsage)
		return 2
	}
	request := storageJournalRequest{AgentID: *agentID, AllocationID: *allocationID, IndexFirstConfirmed: *indexFirst, ExpectedCID: *expectedCID, DecisionID: *decisionID, RemoteTasksSettled: *remoteTasksSettled, RecoveredTaskStep: *recoveredTaskStep, RecoveredTaskUPID: *recoveredTaskUPID, RecoveredTaskEvidencePath: *recoveredTaskEvidence, Summary: *textSummary}
	if !validStorageJournalFlags(action, fs, *configPath, *authority, *fenced, request) {
		fmt.Fprintln(stderr, storageJournalUsage)
		fmt.Fprintln(stderr, "config is required; mutations require authority-id and previous-writer-fenced; missing-generation recovery also requires agent-id, allocation-id and index-first-crash-confirmed; adoption/cleanup decisions require allocation-id and decision-id, and adoption requires expected-cid; initialize and recover-index require remote-tasks-settled; --summary applies only to audit and audit-enrollment")
		return 2
	}
	host := newStorageJournalHost(args)
	if request.RecoveredTaskEvidencePath != "" {
		evidence, err := readStorageRecoveredTaskEvidence(request.RecoveredTaskEvidencePath)
		if err != nil {
			storageJournalFail(stderr, "recovered task evidence could not be read or validated", err)
			return 1
		}
		request.RecoveredTaskEvidence = evidence
	}
	// The pre-check reads only file metadata. A load failure after it prints
	// nothing from the error, because validation errors echo config values.
	if problem := storageJournalConfigProblem(*configPath, host); problem != "" {
		fmt.Fprintln(stderr, problem)
		return 1
	}
	cfg, err := config.LoadFile(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "configuration invalid; check it with the CPI's startup validation")
		return 1
	}
	// This validation's errors are fixed text that names the one missing
	// requirement, unlike LoadFile's, so the CLI prints it.
	if err = cfg.ValidateStoragePlacementAllocation(); err != nil {
		fmt.Fprintln(stderr, "storage journal configuration incomplete: "+err.Error())
		return 1
	}
	logger := log.NewNopLogger()
	factory := opts.ClientFactory
	if factory == nil {
		factory = pve.NewClientWithTracer
	}
	client, err := factory(cfg, logger, nil)
	if err != nil || client == nil {
		storageJournalFail(stderr, "storage journal PVE client unavailable", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	nodes, err := storageJournalNodes(ctx, client)
	if err != nil {
		storageJournalFail(stderr, "storage journal node enumeration failed", err)
		return 1
	}
	identity, err := pve.ObserveStorageClusterIdentity(ctx, client.Nodes(), nodes)
	if err != nil {
		storageJournalFail(stderr, "storage journal cluster identity unavailable", err)
		return 1
	}
	if action == "audit" || action == "recover-authority" || action == "recover-index" || action == "resolve-missing-vm" || action == "adopt" || action == "finalize-cleanup" || action == "cleanup" {
		return runStorageJournalRecovery(ctx, action, cfg, client, nodes, identity.ID(), *authority, *fenced, stdout, stderr, request, host)
	}
	report, err := handlers.AuditStorageAllocationEnrollment(ctx, handlers.Deps{Config: cfg, PVE: client, Logger: logger}, nodes)
	if err != nil {
		storageJournalFail(stderr, "storage journal enrollment audit failed", err)
		return 1
	}
	if action == "audit-enrollment" {
		return writeStorageJournalEnrollment(stdout, stderr, report, request.Summary)
	}
	if preconditions := storageJournalInitializePreconditions(report); len(preconditions) > 0 {
		storageJournalRefuse(stderr, "initialization refused: complete historical absence is unproven; inspect audit-enrollment output", preconditions, report)
		return 1
	}
	auditID, err := aj.RetainAudit(cfg.StorageAllocationJournalDir, struct {
		Namespace, ClusterID string
		RemoteTasksSettled   bool
		Audit                handlers.StorageAllocationAudit
	}{cfg.StoragePlacementNamespace, identity.ID(), *remoteTasksSettled, report})
	if err != nil {
		storageJournalOpenFailure(stderr, "initialization refused: audit evidence could not be retained", cfg.StorageAllocationJournalDir, cfg.StoragePlacementNamespace, err, host)
		return 1
	}
	journal, err := aj.Initialize(ctx, cfg.StorageAllocationJournalDir, cfg.StoragePlacementNamespace, aj.Enrollment{ClusterID: identity.ID(), AuthorityID: *authority, AuditID: auditID, CompleteHistoricalAudit: true, PreviousWriterFenced: *fenced})
	if err != nil {
		storageJournalFail(stderr, "storage journal initialization failed (inspect retained evidence)", err)
		return 1
	}
	if err = journal.Close(); err != nil {
		storageJournalFail(stderr, "journal initialized but close failed (inspect enrollment before retrying)", err)
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
		storageJournalFail(stderr, "initialize output could not be written", err)
		return 1
	}
	return 0
}
func storageJournalNodes(ctx context.Context, client pve.Client) ([]string, error) {
	if client.Nodes() == nil {
		return nil, storageJournalFixedError("PVE node service unavailable")
	}
	response, err := client.Nodes().ListNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", storageJournalFixedError("PVE node listing failed"), err)
	}
	if response == nil || len(*response) == 0 {
		return nil, storageJournalFixedError("PVE listed no nodes")
	}
	var names []string
	for _, raw := range *response {
		var node struct {
			Node string `json:"node"`
		}
		if json.Unmarshal(raw, &node) != nil || node.Node == "" {
			return nil, storageJournalFixedError("PVE node listing malformed")
		}
		names = append(names, node.Node)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

func runStorageJournalRecovery(ctx context.Context, action string, cfg *config.CPIConfig, client pve.Client, nodes []string, clusterID, authority string, fenced bool, stdout, stderr io.Writer, request storageJournalRequest, host storageJournalHost) (code int) {
	dir, namespace := cfg.StorageAllocationJournalDir, cfg.StoragePlacementNamespace
	enrolled, err := aj.InspectEnrollment(dir, namespace)
	if err != nil {
		storageJournalOpenFailure(stderr, "existing journal enrollment unavailable (restore retained authority before recovery)", dir, namespace, err, host)
		return 1
	}
	journal, err := aj.Open(dir, namespace, enrolled.Enrollment.ClusterID)
	if err != nil {
		storageJournalOpenFailure(stderr, "existing journal cannot be opened for audit", dir, namespace, err, host)
		return 1
	}
	defer func() {
		if err := journal.Close(); err != nil {
			storageJournalFail(stderr, "journal close failed (inspect recovery state)", err)
			code = 1
		}
	}()
	if action == "adopt" || action == "finalize-cleanup" || action == "cleanup" {
		if clusterID != enrolled.Enrollment.ClusterID {
			fmt.Fprintln(stderr, "allocation decision requires verified cluster continuity")
			return 1
		}
		applyDecision := handlers.ApplyStorageAllocationDecision
		if action == "cleanup" {
			applyDecision = handlers.CleanupStorageAllocation
		}
		record, err := applyDecision(ctx, handlers.Deps{Config: cfg, PVE: client, Logger: log.NewNopLogger()}, journal, nodes, handlers.StorageAllocationDecision{Action: action, AllocationID: request.AllocationID, ExpectedCID: request.ExpectedCID, DecisionID: request.DecisionID, PreviousWriterFenced: fenced, RemoteTasksSettled: request.RemoteTasksSettled, RecoveredTaskStep: request.RecoveredTaskStep, RecoveredTaskUPID: request.RecoveredTaskUPID, RecoveredTaskEvidence: request.RecoveredTaskEvidence, AuthorityID: authority})
		if err != nil {
			fmt.Fprintf(stderr, "allocation decision refused (%s); inspect settled outcome, identity and audit evidence\n", handlers.StorageAllocationDecisionFailure(err))
			return 1
		}
		if err = json.NewEncoder(stdout).Encode(map[string]string{"allocation_id": record.ID, "state": string(record.State), "cid": record.CID}); err != nil {
			storageJournalFail(stderr, "allocation decision output could not be written", err)
			return 1
		}
		return 0
	}
	report, err := handlers.AuditStorageAllocations(ctx, handlers.Deps{Config: cfg, PVE: client}, journal, nodes)
	if err != nil {
		storageJournalFail(stderr, "retained journal audit failed", err)
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
		return resolveStorageJournalMissingVM(ctx, cfg, journal, clusterID, enrolled.Enrollment.ClusterID, report, request, authority, fenced, stdout, stderr)
	}
	if action == "audit" {
		return writeStorageJournalAudit(stdout, stderr, report, indexErr, clusterID == enrolled.Enrollment.ClusterID, request.Summary)
	}
	if action == "recover-index" && !request.RemoteTasksSettled {
		fmt.Fprintln(stderr, "index recovery requires independent attestation that all previous remote tasks settled before audit")
		return 1
	}
	if preconditions := storageJournalRecoveryPreconditions(action, report, indexErr); len(preconditions) > 0 {
		storageJournalRefuse(stderr, "recovery requires a complete consistent historical audit", preconditions, report)
		return 1
	}
	concise, err := conciseStorageJournalAudit(report)
	if err != nil {
		storageJournalFail(stderr, "recovery record evidence invalid", err)
		return 1
	}
	auditID, err := aj.RetainAudit(cfg.StorageAllocationJournalDir, struct {
		Operation, Namespace, ClusterID string
		RemoteTasksSettled              bool
		Audit                           storageJournalRetainedAudit
	}{action, cfg.StoragePlacementNamespace, clusterID, action == "recover-index" && request.RemoteTasksSettled, concise})
	if err != nil {
		storageJournalFail(stderr, "recovery audit could not be retained", err)
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
		storageJournalFail(stderr, "storage journal "+action+" failed (inspect retained evidence)", err)
		return 1
	}
	if err = json.NewEncoder(stdout).Encode(map[string]string{"namespace": cfg.StoragePlacementNamespace, "audit_id": auditID, "state": action + " completed"}); err != nil {
		storageJournalFail(stderr, action+" output could not be written", err)
		return 1
	}
	return 0
}

// storageJournalRequest carries one invocation's flags past validation to the
// action that uses them. Summary selects the text report for audit and
// audit-enrollment, and validation refuses it for every other action.
type storageJournalRequest struct {
	AgentID, AllocationID, ExpectedCID, DecisionID string
	RecoveredTaskEvidencePath                      string
	RecoveredTaskEvidence                          *handlers.StorageRecoveredTaskEvidence
	RecoveredTaskStep, RecoveredTaskUPID           string
	IndexFirstConfirmed                            bool
	RemoteTasksSettled                             bool
	Summary                                        bool
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
func resolveStorageJournalMissingVM(ctx context.Context, cfg *config.CPIConfig, journal *aj.Journal, clusterID, enrolledClusterID string, report handlers.StorageAllocationAudit, missing storageJournalRequest, authority string, fenced bool, stdout, stderr io.Writer) int {
	if preconditions := storageJournalMissingGenerationPreconditions(missing, authority, fenced, clusterID, enrolledClusterID, report); len(preconditions) > 0 {
		storageJournalRefuse(stderr, "missing-generation recovery requires fencing, cluster continuity and a complete fresh audit", preconditions, report)
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
		storageJournalFail(stderr, "missing-generation audit evidence invalid", err)
		return 1
	}
	evidence := struct {
		Operation, Namespace, ClusterID, AllocationID, AgentSHA256, AuthorityID string
		PreviousWriterFenced, IndexFirstCrashConfirmed                          bool
		Audit                                                                   storageJournalRetainedAudit
	}{"resolve-missing-vm", cfg.StoragePlacementNamespace, clusterID, missing.AllocationID, hex.EncodeToString(agentHash[:]), authority, fenced, missing.IndexFirstConfirmed, concise}
	evidenceID, evidenceJSON, err := aj.VerificationEvidence(evidence)
	if err != nil {
		storageJournalFail(stderr, "missing-generation evidence could not be encoded", err)
		return 1
	}
	auditID, err := aj.RetainAudit(cfg.StorageAllocationJournalDir, evidence)
	if err == nil && auditID != evidenceID {
		err = storageJournalFixedError("the retained audit identity differs from the evidence identity")
	}
	if err != nil {
		storageJournalFail(stderr, "missing-generation evidence could not be retained", err)
		return 1
	}
	// An index-first crash cannot have submitted a mutation: the complete first
	// record must be durable before any mutation is permitted. This empty-target
	// plan is explicitly an audit reconstruction, never a resumable create plan.
	plan, err := json.Marshal(handlers.StorageAllocationPlan{Version: 1, Namespace: cfg.StoragePlacementNamespace, AllocationKey: missing.AgentID, PolicyFingerprint: evidenceID})
	if err != nil {
		storageJournalFail(stderr, "missing-generation reconstruction could not be encoded", err)
		return 1
	}
	intent := aj.Intent{IntentFingerprint: evidenceID, PolicyFingerprint: evidenceID, FrozenInputsFingerprint: evidenceID, PlanVersion: 1, Plan: plan}
	verification := aj.Verification{EvidenceID: evidenceID, EvidenceJSON: evidenceJSON, Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true}
	if err = journal.ResolveMissingVMGeneration(ctx, missing.AgentID, missing.AllocationID, intent, verification); err != nil {
		storageJournalFail(stderr, "missing-generation recovery refused (exact indexed record absence must be proven)", err)
		return 1
	}
	if err = json.NewEncoder(stdout).Encode(map[string]string{"namespace": cfg.StoragePlacementNamespace, "allocation_id": missing.AllocationID, "audit_id": auditID, "state": "missing generation resolved as cleaned"}); err != nil {
		storageJournalFail(stderr, "resolve-missing-vm output could not be written", err)
		return 1
	}
	return 0
}

// storageJournalRefuse prints a refusal as the fixed lead, then one line for
// each precondition that failed, then the audit's finding summary when the
// audit itself falls short. A precondition says why the command refused; the
// summary says what the audit saw.
func storageJournalRefuse(stderr io.Writer, lead string, preconditions []string, report handlers.StorageAllocationAudit) {
	fmt.Fprintln(stderr, lead)
	for _, precondition := range preconditions {
		fmt.Fprintln(stderr, "precondition failed: "+storageJournalLine(precondition))
	}
	if summary := handlers.StorageAuditFindingSummary(report); summary != "" {
		fmt.Fprintln(stderr, "audit findings: "+storageJournalLine(summary))
	}
}

// storageJournalAuditPreconditions names the ways report falls short of the
// complete, conflict-free audit that every journal mutation requires.
func storageJournalAuditPreconditions(report handlers.StorageAllocationAudit) []string {
	var failed []string
	switch {
	case !report.VMScanComplete:
		failed = append(failed, "the VM scan is incomplete")
	case !report.Complete:
		failed = append(failed, "the storage audit is incomplete")
	}
	if n := len(report.Conflicts); n == 1 {
		failed = append(failed, "the audit raised 1 conflict")
	} else if n > 1 {
		failed = append(failed, fmt.Sprintf("the audit raised %d conflicts", n))
	}
	return failed
}

// storageJournalInitializePreconditions adds the usual reason initialize
// refuses, which is provenance that already exists on PVE. It names the
// first few allocations so the operator can find them.
func storageJournalInitializePreconditions(report handlers.StorageAllocationAudit) []string {
	failed := storageJournalAuditPreconditions(report)
	if len(report.Evidence) == 0 {
		return failed
	}
	var ids []string
	for _, evidence := range report.Evidence {
		ids = append(ids, evidence.AllocationID)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	listed := ids[:min(len(ids), 3)]
	noun := "allocations"
	if len(ids) == 1 {
		noun = "allocation"
	}
	text := fmt.Sprintf("existing allocation provenance: %d evidence entries name %d %s: %s", len(report.Evidence), len(ids), noun, strings.Join(listed, ", "))
	if more := len(ids) - len(listed); more > 0 {
		text += fmt.Sprintf(" and %d more", more)
	}
	return append(failed, text)
}

// storageJournalRecoveryPreconditions adds the generation index, which
// recover-authority needs healthy and recover-index exists to repair.
func storageJournalRecoveryPreconditions(action string, report handlers.StorageAllocationAudit, indexErr error) []string {
	failed := storageJournalAuditPreconditions(report)
	if action == "recover-authority" && indexErr != nil {
		failed = append(failed, "the generation index is invalid or unavailable: "+describeStorageJournalError(indexErr))
	}
	return failed
}

// storageJournalMissingGenerationPreconditions names each attestation,
// continuity check, and audit property that resolve-missing-vm lacks.
func storageJournalMissingGenerationPreconditions(missing storageJournalRequest, authority string, fenced bool, clusterID, enrolledClusterID string, report handlers.StorageAllocationAudit) []string {
	var failed []string
	if !missing.IndexFirstConfirmed {
		failed = append(failed, "the index-first crash is not confirmed")
	}
	if !fenced {
		failed = append(failed, "the previous writer is not attested as fenced")
	}
	if strings.TrimSpace(authority) == "" {
		failed = append(failed, "authority-id is blank")
	}
	if clusterID != enrolledClusterID {
		failed = append(failed, "cluster continuity is lost; PVE reports a cluster identity other than the enrolled one")
	}
	failed = append(failed, storageJournalAuditPreconditions(report)...)
	if report.StartedAt.IsZero() {
		failed = append(failed, "the audit has no start time")
	} else if report.CompletedAt.Before(report.StartedAt) {
		failed = append(failed, "the audit completed before it started")
	}
	return failed
}

// storageJournalRecordSummary is the operator-facing view of one journal
// record. CreatedAt and UpdatedAt let an operator see how old a record is,
// and Charging says whether the record's state currently charges its bytes
// against every later create. Charging always comes from
// handlers.StorageAllocationCharging, never from a second copy of the state
// list here, so this view cannot drift from the planner's arithmetic.
type storageJournalRecordSummary struct {
	ID, Kind, State, CID, SHA256 string
	CreatedAt, UpdatedAt         time.Time
	Charging                     bool `json:"charging"`
}

// storageJournalChargingSummary lets an operator reading a long audit see, at
// a glance, whether anything is charging bytes and how long the oldest such
// record has been open, without scanning every row. It names nothing to age
// a record out of its state; that decision stays with the operator.
type storageJournalChargingSummary struct {
	Count     int    `json:"count"`
	OldestID  string `json:"oldest_id,omitempty"`
	OldestAge string `json:"oldest_age,omitempty"`
}

// storageJournalChargingRecords finds the charging records among summaries and
// reports how many there are and how old the oldest one is relative to now.
// now is a parameter, not a call to time.Now(), so the computation stays
// testable without a clock seam on the record itself.
func storageJournalChargingRecords(
	summaries []storageJournalRecordSummary, now time.Time,
) storageJournalChargingSummary {
	var result storageJournalChargingSummary
	var oldest *storageJournalRecordSummary
	for i := range summaries {
		summary := summaries[i]
		if !summary.Charging {
			continue
		}
		result.Count++
		if oldest == nil || summary.CreatedAt.Before(oldest.CreatedAt) {
			oldest = &summaries[i]
		}
	}
	if oldest != nil {
		result.OldestID = oldest.ID
		result.OldestAge = now.Sub(oldest.CreatedAt).Round(time.Second).String()
	}
	return result
}

func validStorageJournalFlags(action string, fs *flag.FlagSet, configPath, authority string, fenced bool, request storageJournalRequest) bool {
	if request.RecoveredTaskStep != "" || request.RecoveredTaskUPID != "" || request.RecoveredTaskEvidencePath != "" {
		if action != "cleanup" || !request.RemoteTasksSettled || request.RecoveredTaskEvidencePath == "" || strings.TrimSpace(request.RecoveredTaskStep) != request.RecoveredTaskStep || request.RecoveredTaskStep == "" || len(request.RecoveredTaskStep) > 256 || request.RecoveredTaskUPID == "" || len(request.RecoveredTaskUPID) > 4096 || strings.ContainsAny(request.RecoveredTaskUPID, "\r\n\t ") {
			return false
		}
	}
	if fs.NArg() != 0 || configPath == "" {
		return false
	}
	// Every action parses the same flag set, so an action that has no text
	// report must refuse --summary rather than silently ignore it.
	if request.Summary && action != "audit" && action != "audit-enrollment" {
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

// storageJournalAuditReport is the audit command's output. The JSON form
// prints it whole, and the text summary prints one line per finding.
type storageJournalAuditReport struct {
	Records           []storageJournalRecordSummary   `json:"records"`
	ChargingSummary   storageJournalChargingSummary   `json:"charging_summary"`
	IndexHealthy      bool                            `json:"generation_index_healthy"`
	IndexFinding      string                          `json:"generation_index_finding,omitempty"`
	ClusterContinuity bool                            `json:"cluster_continuity"`
	Audit             handlers.StorageAllocationAudit `json:"audit"`
	// Attention lists the records the text summary prints in full. The JSON
	// output already carries every record, so it leaves this out.
	Attention []storageJournalAttention `json:"-"`
}

// writeStorageJournalAudit prints the audit as JSON, or as a text summary
// when textSummary is set. The exit code is the same in both modes: 1 when
// the index is unhealthy, cluster continuity is lost, or the audit or its VM
// scan is incomplete, and 0 otherwise.
func writeStorageJournalAudit(stdout, stderr io.Writer, report handlers.StorageAllocationAudit, indexErr error, continuity, textSummary bool) int {
	concise, err := conciseStorageJournalAudit(report)
	if err != nil {
		storageJournalFail(stderr, "audit record summary unavailable", err)
		return 1
	}
	outputReport := report
	outputReport.Records = nil
	var summaries []storageJournalRecordSummary
	for rIndex := range report.Records {
		r := report.Records[rIndex]
		summaries = append(summaries, storageJournalRecordSummary{
			ID:        r.ID,
			Kind:      r.Kind,
			State:     string(r.State),
			CID:       r.CID,
			SHA256:    concise.RecordSHA256[r.ID],
			CreatedAt: r.CreatedAt,
			UpdatedAt: r.UpdatedAt,
			Charging:  handlers.StorageAllocationCharging(r.State),
		})
	}
	output := storageJournalAuditReport{summaries, storageJournalChargingRecords(summaries, time.Now().UTC()), indexErr == nil, "", continuity, outputReport, storageJournalAttentionRecords(report)}
	if indexErr != nil {
		output.IndexFinding = "generation index invalid or unavailable; record listing does not establish healthy authority"
	}
	if textSummary {
		err = writeStorageJournalAuditText(stdout, output)
	} else {
		err = json.NewEncoder(stdout).Encode(output)
	}
	if err != nil {
		storageJournalFail(stderr, "audit output could not be written", err)
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
