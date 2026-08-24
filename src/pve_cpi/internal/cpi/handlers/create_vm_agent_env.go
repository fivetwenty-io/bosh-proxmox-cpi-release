// create_vm_agent_env.go bridges the BOSH env/agent contract: extracting
// job/deployment/instance identity from the CPI env map, configuring the
// guest agent, and waiting for/health-checking agent readiness.
// Split out of create_vm.go (mechanical move, no behavior change).
package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// assertRegistryLessCompleteness verifies that all fields required for a
// configdrive (registry-less) agent boot are non-empty. Called only on the
// configdrive path; noagent skips this assertion entirely.
//
// Returns a Cloud error naming the first missing field. A well-configured
// deploy never hits this — it surfaces already-failing misconfigurations early
// instead of producing a silent agent-dead VM.
func assertRegistryLessCompleteness(agentCfg agent.AgentConfig) error {
	if agentCfg.MBus == "" {
		return cpierrors.Cloud("create_vm: registry-less agent requires non-empty mbus (agent.mbus in CPI config)")
	}
	if agentCfg.AgentID == "" {
		return cpierrors.Cloud("create_vm: registry-less agent requires non-empty agent_id")
	}
	if len(agentCfg.Networks) == 0 {
		return cpierrors.Cloud("create_vm: registry-less agent requires at least one network configured")
	}
	if agentCfg.Disks.System == "" {
		return cpierrors.Cloud("create_vm: registry-less agent requires non-empty system disk path")
	}
	return nil
}

// configureAgent builds the agent.AgentConfig and calls the chosen agent's Configure.
// For configdrive paths a completeness assertion fires before Configure. noagent
// returns immediately with no action.
// ephemeralDevPath is the by-id device path for a dedicated ephemeral disk created in
// step 6.5; empty string = no dedicated disk (agent carves ephemeral from root, default).
func configureAgent(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	parsed *createVMParsedArgs,
	shape *createVMShape,
	vmid int,
	vmName string,
	ephemeralDevPath string,
) error {
	// noagent: nothing to configure.
	if deps.Config.AgentMode == config.AgentModeNoAgent {
		return nil
	}

	// All non-noagent modes (cloudinit, auto) use the configdrive agent.
	// auto always selects configdrive.
	chosenAgent := deps.Agent

	agentNetworks := buildAgentNetworks(parsed.networks)
	mbus, blobstore := extractMBusAndBlobstore(parsed.env)
	if bsRaw, ok := parsed.env["blobstore"].(map[string]any); ok && len(bsRaw) > 0 && blobstore.Provider == "" {
		logger.Warn("create_vm: env.blobstore.provider type assertion failed; configuring agent without blobstore",
			log.String("vm", vmName))
	}
	// In the modern (NATS-mTLS) BOSH director path the director-side
	// agent_settings env carries env.bosh.mbus.cert but NOT env.bosh.mbus.url
	// — the URL has to come from the CPI's job-level `properties.agent.mbus`
	// config (matches the pattern bosh-deployment uses for other CPIs, e.g.
	// virtualbox_cpi in misc/ipv6/bosh.yml). The same fallback handles
	// create-env (bosh-init), where env carries no mbus at all and the URL
	// lives in cloud_provider.properties.agent.mbus.
	if mbus == "" {
		mbus = deps.Config.AgentMBus
	}
	if blobstore.Provider == "" && len(deps.Config.AgentBlobstore) > 0 {
		if p, ok := deps.Config.AgentBlobstore["provider"].(string); ok {
			blobstore.Provider = p
		}
		if opts, ok := deps.Config.AgentBlobstore["options"].(map[string]any); ok {
			blobstore.Options = opts
		}
		if blobstore.Options == nil {
			blobstore.Options = map[string]any{}
		}
	}

	agentCfg := agent.AgentConfig{
		AgentID:  parsed.agentID,
		Networks: agentNetworks,
		Disks: agent.DisksSpec{
			// "/dev/sda" is the *form* the agent's mappedDevicePathResolver
			// expects: it strips the "/dev/sd" prefix and tries "/dev/xvd",
			// "/dev/vd", "/dev/sd" in turn. Under the default virtio0 root
			// disk this lands on /dev/vda (found second) even though our PVE
			// config never exposes /dev/sda. Under pve.root_disk_bus=scsi the
			// root disk IS scsi0 — no virtio disk exists at all, so /dev/vd
			// probes fail and the resolver falls through to /dev/sda, which
			// is the actual root device (scsi0 is always the lowest-numbered
			// scsi slot, so the kernel assigns it letter "a" — the same
			// invariant persistent-disk resolution already depends on via
			// chooseSCSISlotSkippingZero reserving scsi0). This literal is
			// unchanged and correct for both bus choices; no root_disk_bus
			// branching is needed here.
			// A numeric index like "0" would route to idDevicePathResolver,
			// which globs /dev/disk/by-id/*0 — that file does not exist
			// unless we also set a matching `serial=` on the PVE disk.
			System: "/dev/sda",
			// Ephemeral: empty = agent carves ephemeral from root disk
			// (CreatePartitionIfNoEphemeralDisk=true in stemcell agent.json).
			// Non-empty = dedicated ephemeral disk was attached in step 6.5;
			// the agent's idDevicePathResolver finds it via the by-id symlink.
			Ephemeral:  ephemeralDevPath,
			Persistent: map[string]string{},
		},
		Env:       parsed.env,
		MBus:      mbus,
		Blobstore: blobstore,
		VM: agent.VMSpec{
			Name: vmName,
			ID:   strconv.Itoa(vmid),
		},
	}

	// Completeness assertion fires on every non-noagent path (configdrive /
	// auto modes all use configdrive). noagent returned early above. This
	// converts a guaranteed silent mis-bootstrap into an early Cloud error.
	if assertErr := assertRegistryLessCompleteness(agentCfg); assertErr != nil {
		return assertErr
	}

	if err := chosenAgent.Configure(ctx, shape.node, vmid, agentCfg); err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("create_vm: agent configure vmid=%d", vmid))
	}
	return nil
}

// --------------------------------------------------------------------------
// extractJobNameFromEnv returns the BOSH instance-group (job) name from the
// create_vm env, or "" if it cannot be derived.
//
// env["bosh"]["group"] is "<director>-<deployment>-<job>" and
// env["bosh"]["groups"] is an array containing all combinations of those
// three (including each one standalone). The bare job name is the shortest
// element G for which group has suffix "-G" (or group == G). Using the
// shortest such suffix avoids confusing "<deployment>-<job>" with "<job>"
// when the job name itself contains hyphens (e.g. "diego-api").
//
// Returns the raw job name; the caller must run sanitizeVMName before
// handing it to PVE.
// --------------------------------------------------------------------------
func extractJobNameFromEnv(env map[string]any) string {
	boshRaw, ok := env["bosh"].(map[string]any)
	if !ok {
		return ""
	}
	group, _ := boshRaw["group"].(string)
	groupsRaw, _ := boshRaw["groups"].([]any)
	if group == "" || len(groupsRaw) == 0 {
		return ""
	}
	var best string
	for _, g := range groupsRaw {
		s, ok := g.(string)
		if !ok || s == "" || s == group {
			continue
		}
		if !strings.HasSuffix(group, "-"+s) {
			continue
		}
		if best == "" || len(s) < len(best) {
			best = s
		}
	}
	return best
}

// extractDeploymentFromEnv returns the BOSH deployment name from the
// create_vm env, or "" if it cannot be derived. Given env.bosh.group =
// "<director>-<deployment>-<job>" and the already-resolved job, the
// remainder ("<director>-<deployment>") has the deployment as the shortest
// suffix in env.bosh.groups that is neither the remainder itself nor the
// bare director. This mirrors extractJobNameFromEnv's "shortest matching
// suffix" rule so a deployment name containing hyphens still resolves
// correctly. Returns "" when env.bosh is absent, when group is empty, or
// when job is empty (the deployment cannot be located without first
// stripping the job suffix from group).
func extractDeploymentFromEnv(env map[string]any, job string) string {
	if job == "" {
		return ""
	}
	boshRaw, ok := env["bosh"].(map[string]any)
	if !ok {
		return ""
	}
	group, _ := boshRaw["group"].(string)
	groupsRaw, _ := boshRaw["groups"].([]any)
	if group == "" || len(groupsRaw) == 0 {
		return ""
	}
	remainder := strings.TrimSuffix(group, "-"+job)
	if remainder == group || remainder == "" {
		return ""
	}
	var best string
	for _, g := range groupsRaw {
		s, ok := g.(string)
		if !ok || s == "" || s == remainder || s == group || s == job {
			continue
		}
		if !strings.HasSuffix(remainder, "-"+s) {
			continue
		}
		if best == "" || len(s) < len(best) {
			best = s
		}
	}
	return best
}

// extractInstanceNameFromEnv returns the BOSH instance-group name embedded
// in env.bosh.instance.name (the bosh-init / create-env env shape includes
// this even when env.bosh.group is absent). Returns "" when neither key is
// present.
func extractInstanceNameFromEnv(env map[string]any) string {
	boshRaw, ok := env["bosh"].(map[string]any)
	if !ok {
		return ""
	}
	if inst, ok := boshRaw["instance"].(map[string]any); ok {
		if s, _ := inst[metadataKeyName].(string); s != "" {
			return s
		}
	}
	return ""
}

// instanceGroupName returns the BOSH instance-group name for the VM being
// created — the unit of anti-affinity spreading. It prefers the job suffix
// derived from env.bosh.group/groups (director deploys) and falls back to
// env.bosh.instance.name (create-env / bosh-init). Returns "" when neither is
// derivable.
func instanceGroupName(env map[string]any) string {
	if job := extractJobNameFromEnv(env); job != "" {
		return job
	}
	return extractInstanceNameFromEnv(env)
}

// antiAffinityGroupTag returns the PVE tag that marks membership in the VM's
// BOSH instance group, formed to match the tag scheme set_vm_metadata stamps
// ("job--<sanitized>"). It returns "" — disabling scheduler-soft spreading for
// this create — when anti-affinity is not enabled or the instance group cannot
// be derived from env.
func antiAffinityGroupTag(cfg *config.CPIConfig, env map[string]any) string {
	if !cfg.AntiAffinityEnabled() {
		return ""
	}
	group := sanitizeTagValue(instanceGroupName(env))
	if group == "" {
		return ""
	}
	return "job--" + group
}

// --------------------------------------------------------------------------
// extractMBusAndBlobstore pulls mbus and blobstore from the env map. BOSH
// uses three distinct env shapes depending on the caller:
//
//   - Director deploys (the common path): keys live under env["bosh"],
//     with env["bosh"]["mbus"] as a STRING (e.g. "nats://10.0.0.1:4222")
//     and env["bosh"]["blobstores"] as an array (first entry is the
//     director-side blobstore).
//   - create-env / bosh-init: keys also live under env["bosh"], but
//     env["bosh"]["mbus"] is an OBJECT with at least env["bosh"]["mbus"]["url"]
//     plus TLS cert fields. We extract .url.
//   - Legacy / out-of-band callers: top-level env["mbus"] (string) and
//     env["blobstore"] (object). Accepted as a fallback for compatibility.
//
// Tolerant: missing keys return zero values.
// --------------------------------------------------------------------------
func extractMBusAndBlobstore(env map[string]any) (string, agent.BlobstoreSpec) {
	mbus, _ := env["mbus"].(string)

	var bs agent.BlobstoreSpec
	if bsRaw, ok := env["blobstore"].(map[string]any); ok {
		bs.Provider, _ = bsRaw["provider"].(string)
		bs.Options, _ = bsRaw["options"].(map[string]any)
	}

	if boshRaw, ok := env["bosh"].(map[string]any); ok {
		if mbus == "" {
			// Director-deploy shape: env.bosh.mbus is a flat string.
			if s, ok := boshRaw["mbus"].(string); ok {
				mbus = s
			}
		}
		if mbus == "" {
			// create-env / bosh-init shape: env.bosh.mbus is an object
			// with .url (plus cert fields we forward via env elsewhere).
			if mbusRaw, ok := boshRaw["mbus"].(map[string]any); ok {
				if u, ok := mbusRaw["url"].(string); ok {
					mbus = u
				}
			}
		}
		if bs.Provider == "" {
			if blobs, ok := boshRaw["blobstores"].([]any); ok && len(blobs) > 0 {
				if b0, ok := blobs[0].(map[string]any); ok {
					bs.Provider, _ = b0["provider"].(string)
					bs.Options, _ = b0["options"].(map[string]any)
				}
			}
		}
	}

	if bs.Options == nil {
		bs.Options = map[string]any{}
	}
	return mbus, bs
}

// waitUntilAgentReady polls the QEMU guest agent via CreateQemuAgentPing until
// the agent responds or the deadline derived from health_check.timeout_sec expires.
//
// Behavior:
//   - A successful ping (nil error) returns nil immediately.
//   - A transient ping error (transport fault, connection refused, 5xx) is
//     retried after the effective poll interval.
//   - A permanent ping error (auth failure, 4xx non-transport) fails fast
//     without waiting for the deadline; diagnostics are still gathered.
//   - On deadline expiry or parent context cancellation, ListQemuStatusCurrent
//     is called to gather VM status diagnostics; the diagnostics are folded into
//     the returned error so the existing rollback defer has context.
//   - Parent context cancellation is honored: if ctx.Done() fires, the function
//     returns promptly regardless of the health-check deadline.
//   - The effective poll interval is max(configured interval, healthPollMinInterval)
//     so a configured value of 0 never produces a tight busy-loop in production.
//     The interval sleep is deadline-aware: both hcCtx.Done() and ctx.Done()
//     wake it early.
//
// Diagnostics source: VM status from ListQemuStatusCurrent only. There is no
// clean REST surface to retrieve arbitrary task-log lines without a UPID, so
// status-only enrichment is the intended and complete behavior.
//
// The returned error is a non-retriable CloudError. The existing create_vm
// rollback (cleanupVM defer) fires automatically because retErr != nil.
func waitUntilAgentReady(
	ctx context.Context,
	deps Deps,
	node string,
	vmid int,
	logger *log.Logger,
) error {
	timeoutSec := deps.Config.HealthCheckTimeoutSec()
	intervalSec := deps.Config.HealthCheckIntervalSec()
	vmidStr := strconv.Itoa(vmid)

	// Compute effective poll interval. The configured value of 0 is valid
	// ("no explicit preference") but must not produce a tight busy-loop in
	// production. Apply the package-level floor; tests may lower it to zero.
	effectiveInterval := time.Duration(intervalSec) * time.Second
	if floor := healthPollMinInterval(); effectiveInterval < floor {
		effectiveInterval = floor
	}

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	hcCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	logger.Debug("create_vm: health gate: polling guest agent",
		log.Int(metadataKeyVMID, vmid),
		log.String("node", node),
		log.Int("timeout_sec", timeoutSec),
	)

	for {
		// Respect parent context cancellation.
		select {
		case <-ctx.Done():
			return gatherHealthDiagnostics(ctx, deps, node, vmid, vmidStr, logger,
				fmt.Sprintf("create_vm: health gate: context cancelled waiting for agent on vm %d: %v",
					vmid, ctx.Err()))
		default:
		}

		_, pingErr := deps.PVE.Nodes().CreateQemuAgentPing(hcCtx, node, vmidStr)
		if pingErr == nil {
			logger.Debug("create_vm: health gate: guest agent ready",
				log.Int(metadataKeyVMID, vmid))
			return nil
		}

		// Check whether the health-check deadline or parent context expired.
		if hcCtx.Err() != nil {
			msg := fmt.Sprintf(
				"create_vm: health gate: timeout waiting for guest agent on vm %d after %ds",
				vmid, timeoutSec)
			return gatherHealthDiagnostics(ctx, deps, node, vmid, vmidStr, logger, msg)
		}
		if ctx.Err() != nil {
			return gatherHealthDiagnostics(ctx, deps, node, vmid, vmidStr, logger,
				fmt.Sprintf("create_vm: health gate: context cancelled waiting for agent on vm %d: %v",
					vmid, ctx.Err()))
		}

		// Classify the ping error: transient faults are retried; permanent faults
		// (auth failures, 4xx non-transport responses) fail fast to avoid spinning
		// for the full timeout when the outcome is already determined.
		if !pve.IsTransientTransport(pingErr) {
			logger.Error("create_vm: health gate: permanent agent ping error, failing fast",
				log.Int(metadataKeyVMID, vmid),
				log.String("node", node),
				log.Err(pingErr),
			)
			return gatherHealthDiagnostics(ctx, deps, node, vmid, vmidStr, logger,
				fmt.Sprintf("create_vm: health gate: permanent error pinging agent on vm %d: %v",
					vmid, pingErr))
		}

		logger.Debug("create_vm: health gate: agent ping failed (retrying)",
			log.Int(metadataKeyVMID, vmid),
			log.String("node", node),
			log.Err(pingErr),
		)

		// Deadline-aware sleep. Both hcCtx.Done() and ctx.Done() wake the select
		// early so the deadline still bounds total wait time regardless of interval.
		select {
		case <-time.After(effectiveInterval):
		case <-ctx.Done():
			return gatherHealthDiagnostics(ctx, deps, node, vmid, vmidStr, logger,
				fmt.Sprintf("create_vm: health gate: context cancelled waiting for agent on vm %d: %v",
					vmid, ctx.Err()))
		case <-hcCtx.Done():
			msg := fmt.Sprintf(
				"create_vm: health gate: timeout waiting for guest agent on vm %d after %ds",
				vmid, timeoutSec)
			return gatherHealthDiagnostics(ctx, deps, node, vmid, vmidStr, logger, msg)
		}
	}
}

// gatherHealthDiagnostics calls ListQemuStatusCurrent to enrich the error with
// VM state at the time of the health-gate failure. Task-log lines are not
// included: no clean REST surface exists for retrieving arbitrary log content
// without a UPID, so status-only enrichment is the complete intended behavior.
// On ListQemuStatusCurrent error the base message is returned without
// enrichment (best-effort). Always returns a non-retriable CloudError.
func gatherHealthDiagnostics(
	_ context.Context,
	deps Deps,
	node string,
	vmid int,
	vmidStr string,
	logger *log.Logger,
	baseMsg string,
) error {
	// Use a fresh context for the diagnostic scrape; the original context may
	// already be cancelled or at its deadline.
	diagCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	status, statusErr := deps.PVE.Nodes().ListQemuStatusCurrent(diagCtx, node, vmidStr)
	if statusErr != nil {
		logger.Warn("create_vm: health gate: could not gather VM status for diagnostics",
			log.Int(metadataKeyVMID, vmid), log.Err(statusErr))
		return cpierrors.Cloud("%s (diagnostics unavailable: %s)", baseMsg, statusErr.Error())
	}

	qmpStatus := ""
	if status.Qmpstatus != nil {
		qmpStatus = *status.Qmpstatus
	}
	logger.Error("create_vm: health gate failed",
		log.Int(metadataKeyVMID, vmid),
		log.String("node", node),
		log.String("vm_status", status.Status),
		log.String("qmp_status", qmpStatus),
	)

	return cpierrors.Cloud("%s (vm_status=%s qmp_status=%s)",
		baseMsg, status.Status, qmpStatus)
}
