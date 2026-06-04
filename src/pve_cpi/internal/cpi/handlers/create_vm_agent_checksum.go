package handlers

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// agentChecksumPath is the in-guest path of the BOSH agent binary whose SHA-256
// is asserted against health_check.expected_agent_sha256 (§7.29).
const agentChecksumPath = "/var/vcap/bosh/bin/bosh-agent"

// agentChecksumExecMaxWaitNs bounds the total wait for the guest-agent exec to
// finish, stored as nanoseconds in an atomic int64 so tests can shrink it.
// sha256sum of the ~20 MiB agent binary completes well under a second; 30s is a
// generous ceiling that still bounds a wedged guest agent.
var agentChecksumExecMaxWaitNs atomic.Int64

// agentChecksumPollIntervalNs is the wait between exec-status polls. Default 1s.
var agentChecksumPollIntervalNs atomic.Int64

func init() {
	agentChecksumExecMaxWaitNs.Store(int64(30 * time.Second))
	agentChecksumPollIntervalNs.Store(int64(1 * time.Second))
}

// SetAgentChecksumTimings overrides the exec max-wait and poll interval for the
// duration of a test and returns a restore function. Keeps §7.29 unit tests
// instant.
//
//	defer handlers.SetAgentChecksumTimings(50*time.Millisecond, time.Millisecond)()
func SetAgentChecksumTimings(maxWait, pollInterval time.Duration) func() {
	prevMax := agentChecksumExecMaxWaitNs.Swap(int64(maxWait))
	prevPoll := agentChecksumPollIntervalNs.Swap(int64(pollInterval))
	return func() {
		agentChecksumExecMaxWaitNs.Store(prevMax)
		agentChecksumPollIntervalNs.Store(prevPoll)
	}
}

// runHealthGate runs the opt-in post-create health gate: first wait for the
// guest agent to answer (§7.12), then, when health_check.expected_agent_sha256
// is set, assert the agent binary's checksum (§7.29). A returned error
// propagates out of create_vm and triggers the cleanupVM rollback. The caller
// gates this on HealthCheckEnabled().
func runHealthGate(ctx context.Context, deps Deps, node string, vmid int, logger *log.Logger) error {
	if err := waitUntilAgentReady(ctx, deps, node, vmid, logger); err != nil {
		return err
	}
	if expected := deps.Config.HealthCheckExpectedAgentSHA256(); expected != "" {
		return assertAgentChecksum(ctx, deps, node, vmid, expected, logger)
	}
	return nil
}

// assertAgentChecksum runs `sha256sum <agentChecksumPath>` inside the guest via
// the QEMU guest agent and compares the reported digest against expected (a
// lower-case 64-hex SHA-256). It returns a non-retriable CloudError ONLY on a
// confirmed digest mismatch — that error propagates out of create_vm and the
// existing cleanupVM rollback destroys the VM.
//
// Every other outcome is fail-open (logs a warning, returns nil): a guest-agent
// transport error, a non-zero sha256sum exit (e.g. the binary is not at the
// expected path on this stemcell), an exec that does not finish within the
// bound, or output that does not parse. The assertion layers an integrity
// signal on top of the §7.12 ping; it must not block provisioning when it
// cannot positively confirm tampering.
//
// expected is assumed already normalized to lower case (see
// HealthCheckExpectedAgentSHA256). The caller gates on expected != "".
func assertAgentChecksum(
	ctx context.Context,
	deps Deps,
	node string,
	vmid int,
	expected string,
	logger *log.Logger,
) error {
	vmidStr := strconv.Itoa(vmid)

	execResp, err := deps.PVE.Nodes().CreateQemuAgentExec(ctx, node, vmidStr, &sdknodes.CreateQemuAgentExecParams{
		Command: []string{"sha256sum", agentChecksumPath},
	})
	if err != nil || execResp == nil {
		logger.Warn("create_vm: agent checksum: exec start failed — skipping verification (fail-open)",
			log.Int(metadataKeyVMID, vmid),
			log.String("node", node),
			log.Err(err),
		)
		return nil
	}

	status, ok := awaitAgentExec(ctx, deps, node, vmidStr, execResp.Pid, logger)
	if !ok {
		// awaitAgentExec already logged the fail-open reason.
		return nil
	}

	if status.Exitcode != nil && *status.Exitcode != 0 {
		logger.Warn("create_vm: agent checksum: sha256sum exited non-zero — skipping verification (fail-open)",
			log.Int(metadataKeyVMID, vmid),
			log.String("path", agentChecksumPath),
			log.Int("exit_code", int(*status.Exitcode)),
		)
		return nil
	}

	got, parsed := parseSha256SumOutput(status.OutData)
	if !parsed {
		logger.Warn("create_vm: agent checksum: could not parse sha256sum output — skipping verification (fail-open)",
			log.Int(metadataKeyVMID, vmid),
		)
		return nil
	}

	if got != expected {
		return cpierrors.Cloud(
			"create_vm: agent integrity check failed for vm %d: %s reported SHA-256 %s but expected %s."+
				" The booted BOSH agent binary does not match health_check.expected_agent_sha256.",
			vmid, agentChecksumPath, got, expected,
		)
	}

	logger.Info("create_vm: agent checksum verified",
		log.Int(metadataKeyVMID, vmid),
		log.String("sha256", got),
	)
	return nil
}

// awaitAgentExec polls ListQemuAgentExecStatus until the command has exited or
// the bound elapses. Returns (status, true) when the command exited, or
// (nil, false) on any fail-open condition (transport error, timeout, ctx
// cancel) after logging the reason.
func awaitAgentExec(
	ctx context.Context,
	deps Deps,
	node, vmidStr string,
	pid int64,
	logger *log.Logger,
) (*sdknodes.ListQemuAgentExecStatusResponse, bool) {
	maxWait := time.Duration(agentChecksumExecMaxWaitNs.Load())
	interval := time.Duration(agentChecksumPollIntervalNs.Load())

	ectx, cancel := context.WithTimeout(ctx, maxWait)
	defer cancel()

	for {
		status, err := deps.PVE.Nodes().ListQemuAgentExecStatus(ectx, node, vmidStr, &sdknodes.ListQemuAgentExecStatusParams{
			Pid: pid,
		})
		if err == nil && status != nil && status.Exited {
			return status, true
		}
		if err != nil {
			logger.Debug("create_vm: agent checksum: exec-status poll failed — retrying",
				log.String("vmid", vmidStr),
				log.Err(err),
			)
		}

		select {
		case <-ectx.Done():
			logger.Warn("create_vm: agent checksum: exec did not finish within bound — skipping verification (fail-open)",
				log.String("vmid", vmidStr),
				log.String("timeout", maxWait.String()),
			)
			return nil, false
		case <-time.After(interval):
		}
	}
}

// parseSha256SumOutput extracts the lower-cased hex digest from sha256sum
// stdout, whose format is "<hex>  <path>\n". Returns (digest, true) only when
// the first whitespace-delimited token is a 64-hex string.
func parseSha256SumOutput(out *string) (string, bool) {
	if out == nil {
		return "", false
	}
	fields := strings.Fields(*out)
	if len(fields) == 0 {
		return "", false
	}
	tok := strings.ToLower(fields[0])
	if len(tok) != 64 {
		return "", false
	}
	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return "", false
		}
	}
	return tok, true
}
