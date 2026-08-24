package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/configdrive"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// localISOStorageWarnOnce guards the single-shot warning emitted when the
// effective iso_storage resolves to "local". One warning per CPI process is
// sufficient; re-firing on every create_vm would flood operator logs.
//
// Process-scoped, not cluster-scoped: with per-request context overrides one
// process can serve several clusters, and only the first cluster to hit this
// path gets the warning. Accepted — the warning is a low-stakes storage-
// placement hint; contrast handlers' firewallMasterSwitchProbedClusters,
// which is keyed per cluster because its warning is enforcement-relevant.
var localISOStorageWarnOnce sync.Once

// configDriveSlot is the SCSI bus slot used to attach the ConfigDrive ISO as
// a CD-ROM. The stemcell finds it by volume label (CONFIG-2 / config-2), so
// the slot index does not need to match a path the agent expects. PVE
// exposes scsi0–scsi30 (31 slots total). Reservation map:
//
//	virtio0 (default) or scsi0 (pve.root_disk_bus=scsi)
//	                system disk (stemcell-imported root; see create_vm's
//	                rootDiskKey). scsi0 is reserved for the root disk either
//	                way — AttachDisk always starts its free-slot search at
//	                scsi1, so persistent-disk allocation is identical
//	                regardless of which bus the root disk is on.
//	scsi1..scsi28   ephemeral + persistent disks (create_vm + attach_disk).
//	scsi29          reserved headroom (unused; leaves space for future use).
//	scsi30          ConfigDrive CD-ROM (this constant).
//
// Picking scsi30 — rather than scsi6, the historic choice — keeps the
// CD-ROM out of NextIndexForBus's path so attach_disk never overwrites it
// regardless of how many persistent disks the VM carries. create_vm
// enforces the matching cap of 28 persistent disks at creation time.
const configDriveSlot = "scsi30"

// configDriveSlotIndex is the integer index of configDriveSlot (30), used
// with nodes.UpdateQemuConfigParams.Scsi.
const configDriveSlotIndex = 30

// Compile-time assertion: ConfigDrive satisfies Agent.
var _ Agent = (*ConfigDrive)(nil)

// ConfigDrive writes BOSH agent settings via an OpenStack ConfigDrive ISO
// attached as a CD-ROM. The ISO contains the raw settings.json bytes at
// /ec2/latest/user-data (and /ec2/latest/meta-data.json) so the bosh-agent on
// an OpenStack stemcell can read them via the ConfigDrive datasource —
// without depending on a cloud-init runcmd to restart a specific systemd unit.
//
// This is the only cloud-init bootstrap path for the CPI.
type ConfigDrive struct {
	storage string
	pveSvc  pve.Client
	logger  *log.Logger
	// nodeEndpoints resolves the direct pveproxy address for the upload's
	// target node; nil (every test that does not care) means the upload takes
	// the proxied path through the configured endpoint.
	nodeEndpoints *pve.NodeEndpointResolver
}

// NewConfigDrive constructs a ConfigDrive bound to the given PVE client.
// pveClient, logger must not be nil; storage must not be empty; nodeEndpoints
// may be nil (no direct-to-node upload routing).
func NewConfigDrive(pveClient pve.Client, storage string, nodeEndpoints *pve.NodeEndpointResolver, logger *log.Logger) *ConfigDrive {
	// invariant violation: nil pve.Client at construction; cannot occur at runtime
	if pveClient == nil {
		panic("agent: NewConfigDrive: pveClient must not be nil")
	}
	// invariant violation: nil logger at construction; cannot occur at runtime
	if logger == nil {
		panic("agent: NewConfigDrive: logger must not be nil")
	}
	// invariant violation: empty iso_storage at construction; cannot occur at runtime
	if storage == "" {
		panic("agent: NewConfigDrive: storage must not be empty")
	}
	return &ConfigDrive{
		storage:       storage,
		pveSvc:        pveClient,
		logger:        logger,
		nodeEndpoints: nodeEndpoints,
	}
}

// newConfigDriveForTest builds a ConfigDrive directly from a fake pve.Client.
// Tests only; production code always calls NewConfigDrive.
func newConfigDriveForTest(pveSvc pve.Client, storage string, logger *log.Logger) *ConfigDrive {
	return &ConfigDrive{pveSvc: pveSvc, storage: storage, logger: logger}
}

// Configure builds the ConfigDrive ISO, uploads it to PVE storage, and
// attaches it to the VM as a CD-ROM on scsi30 (see configDriveSlot for
// the SCSI-slot reservation map).
//
//  1. Build settings.json. cfg.MBus must be set explicitly; an empty MBus
//     combined with a derivable blobstore host returns an error (credential-less
//     NATS URLs silently fail authentication).
//  2. Author the ISO via go-diskfs (ConfigDrive v2 layout, Rock Ridge,
//     volume label "config-2").
//  3. Upload via Storage().Upload (content=iso).
//  4. Attach via Nodes().UpdateQemuConfig with scsi30 entry.
//  5. Remove the local temp file unconditionally on the way out.
func (a *ConfigDrive) Configure(ctx context.Context, node string, vmid int, cfg AgentConfig) error {
	if ctx == nil {
		return cpierrors.Cloud("agent configure: ctx must not be nil")
	}
	if node == "" {
		return cpierrors.Cloud("agent configure: node must not be empty")
	}
	if vmid <= 0 {
		return cpierrors.Cloud("agent configure: vmid must be positive, got %d", vmid)
	}

	// Warn once per process when iso_storage resolves to "local". Local storage
	// ISOs are readable by anyone with access to the PVE node-local storage pool,
	// which is not appropriate for multi-tenant environments.
	if a.storage == isoStorageSpecDefault {
		localISOStorageWarnOnce.Do(func() {
			a.logger.Warn("iso_storage=local; ConfigDrive ISOs are readable by anyone with access to the PVE node-local storage. Recommend dedicated pool. See docs/operations.md ISO storage section.")
		})
	}

	a.logger.Debug("configdrive: configure",
		log.String("node", node),
		log.Int("vmid", vmid),
		log.String("agent_id", cfg.AgentID),
	)

	settings, err := buildSettings(cfg, vmid)
	if err != nil {
		return err
	}

	payload, err := json.Marshal(settings)
	if err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("agent configure vm %d: marshal settings.json", vmid))
	}

	isoPath, cleanup, err := configdrive.Build(payload)
	if err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("agent configure vm %d: build configdrive iso", vmid))
	}
	defer cleanup()

	filename := configDriveISOFilename(vmid)

	// Pre-delete any orphan ISO sitting under the same filename. PVE rejects
	// `content=iso` uploads with HTTP 409 when the target name is taken, so a
	// botched earlier create_vm (or a VMID recycled out-of-band) would wedge
	// every subsequent attempt at the same vmid. 404 is tolerated; any other
	// failure short-circuits before the upload.
	//
	// This sweep deliberately stays on the plain ctx while uploadISO's
	// in-loop pre-retry sweep is pinned: a failure here short-circuits
	// Configure with no fallback of its own, and no pinned dial has been
	// proven reachable yet. The in-loop sweep pins because by the time it
	// runs the pinned route has already carried the POST that made the
	// retry necessary, and reading that node's own task list gives
	// read-your-write consistency for the imgdel it queues.
	if existed, rmErr := a.removeISOIfExists(ctx, node, filename); rmErr != nil {
		return cpierrors.Wrap(rmErr, fmt.Sprintf("agent configure vm %d: pre-delete stale configdrive iso", vmid))
	} else if existed {
		a.logger.Warn("configdrive: removed stale orphan iso before upload",
			log.String("node", node),
			log.Int("vmid", vmid),
			log.String("filename", filename),
		)
	}

	if err := a.uploadISO(ctx, node, isoPath, filename); err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("agent configure vm %d: upload configdrive iso", vmid))
	}

	if attachErr := a.attachISO(ctx, node, vmid, filename); attachErr != nil {
		// Best-effort cleanup: remove the uploaded ISO so it does not linger as
		// an orphan in the storage pool. When cleanup also fails the combined
		// error supersedes the attach-only error so operators see both failure
		// surfaces in the single returned message. The attach error is wrapped
		// (via %w) to preserve its BOSH error type classification for retry logic;
		// the cleanup error is appended as context via %v.
		if rmErr := a.removeISOFromStorage(ctx, node, filename); rmErr != nil {
			return fmt.Errorf("agent configure vm %d: attach configdrive iso failed (%w); cleanup also failed: %w",
				vmid, attachErr, rmErr)
		}
		return cpierrors.Wrap(attachErr, fmt.Sprintf("agent configure vm %d: attach configdrive iso", vmid))
	}

	a.logger.Info("configdrive: configured",
		log.String("node", node),
		log.Int("vmid", vmid),
		log.String("slot", configDriveSlot),
	)
	return nil
}

// Remove deletes the ConfigDrive ISO for vmid from the PVE storage pool. A
// 404 is treated as success (idempotent) so callers may invoke this during
// delete_vm without checking whether the agent was ever configured.
func (a *ConfigDrive) Remove(ctx context.Context, node string, vmid int) error {
	if ctx == nil {
		return cpierrors.Cloud("agent remove: ctx must not be nil")
	}
	if node == "" {
		return cpierrors.Cloud("agent remove: node must not be empty")
	}
	if vmid <= 0 {
		return cpierrors.Cloud("agent remove: vmid must be positive, got %d", vmid)
	}

	filename := configDriveISOFilename(vmid)
	if err := a.removeISOFromStorage(ctx, node, filename); err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("agent remove vm %d: delete configdrive iso", vmid))
	}
	a.logger.Info("configdrive: removed configdrive iso",
		log.String("node", node),
		log.Int("vmid", vmid),
	)
	return nil
}

func (a *ConfigDrive) uploadISO(ctx context.Context, node, localPath, filename string) error {
	// Upload returns a UPID (async storage task). The file is not yet
	// visible to subsequent calls (e.g. attaching as a CD-ROM) until the
	// task completes. Both the multipart POST and the task run under the
	// per-storage lockfile, so on bursty deploys this can surface
	// "can't lock file ... got timeout" on either side, and an overloaded
	// pveproxy can drop the connection mid-POST (surfacing as EOF). Retry
	// the whole open+upload+await tuple on those signals; the body stream
	// is reopened each attempt so PVE always sees a fresh reader.
	//
	// Disable the SDK's inner HTTP retry for this request: it tries to
	// replay a multipart upload by re-reading req.Body, but the body has
	// already been drained by attempt 0. The replay sends an empty body
	// while Content-Length still advertises the original size, and Go's
	// transport rejects with "http: ContentLength=N with Body length 0".
	// Our outer RetryOnTransientOrLock reopens the file each iteration,
	// so transient failures are still retried with a fresh stream.
	//
	// Direct-to-node routing: when the resolver maps the target node, the
	// upload POST, its UPID await, and the pre-retry sweep all dial that
	// node's own pveproxy instead of letting the configured endpoint proxy
	// the multipart POST cross-node (the hop sheds connections under burst
	// load, and reading the target node's own task list gives read-your-write
	// consistency for the task the upload just created there). The attach and
	// everything else stay on the plain ctx: VM config writes go through
	// pmxcfs and gain nothing from pinning. The override presumes TLS
	// fingerprint pinning stays unset in buildTransportOpts; enabling any
	// pinning option makes the SDK reject overridden requests.
	pinnedCtx := ctx
	directHost := ""
	if h, ok := a.nodeEndpoints.HostFor(ctx, node); ok {
		pinnedCtx = pve.UploadContext(ctx, h)
		directHost = h
		a.logger.Debug("configdrive: uploading directly to the target node",
			log.String("node", node),
			log.String("direct_host", h),
		)
	}
	uploadCtx := sdkclient.WithRetries(pinnedCtx, 0)
	firstAttempt := true
	return pve.RetryOnTransientOrLock(ctx, a.logger, "configdrive_upload", pve.StorageUploadMaxAttempts(), func() error {
		// A retry means the previous POST died mid-flight (connection drop,
		// lock timeout). PVE may still have committed the file after the drop,
		// and a duplicate `content=iso` upload is rejected with HTTP 409, so
		// clear the target name before re-uploading. 404 is "not committed"
		// (the common case). A sweep that fails on the same retryable fault
		// class the loop handles (lock contention, pushback, another drop) is
		// returned to the loop so the sweep itself rides the backoff curve:
		// pressing on would upload into a possibly-taken name and turn the
		// retryable fault into a permanent 409. Only a permanently-failing
		// sweep proceeds best-effort, leaving the upload to surface the truth.
		if !firstAttempt {
			if _, rmErr := a.removeISOIfExists(pinnedCtx, node, filename); rmErr != nil {
				if pve.IsRetryableOrLockFault(rmErr) {
					return rmErr
				}
				a.logger.Warn("configdrive: pre-retry iso cleanup failed; retrying the upload anyway",
					log.String("node", node),
					log.String("filename", filename),
					log.Err(rmErr),
				)
			}
		}
		firstAttempt = false

		awaitPhase := false
		attempt := func(upCtx context.Context) error {
			awaitPhase = false
			f, openErr := os.Open(localPath) // #nosec G304 -- localPath is CPI-owned MkdirTemp output
			if openErr != nil {
				return fmt.Errorf("open local iso: %w", openErr)
			}
			defer func() { _ = f.Close() }()

			upid, err := a.pveSvc.Storage().Upload(upCtx, node, a.storage, "iso", filename, f)
			if err != nil {
				return pve.WrapError(err)
			}
			if upid == "" {
				return nil
			}
			awaitPhase = true
			return pve.AwaitTaskWithLogger(pinnedCtx, a.pveSvc, node, upid, a.logger)
		}

		err := attempt(uploadCtx)
		if err != nil && directHost != "" && (pve.IsTLSCertVerificationFailure(err) || pve.IsDirectDialFailure(err)) {
			// The direct address is unusable: the node certificate does not
			// cover it, or the dial itself failed (wrong entry, unreachable
			// address). Nothing was sent in either case, so re-attempting
			// un-pinned through the configured endpoint (the pre-existing
			// path) needs no pre-sweep. The dead route is memoized on the
			// resolver so later uploads this process skip it, and the
			// remaining attempts here stay un-pinned. When the failure
			// surfaced in the await phase the POST already went through, so
			// the inline re-attempt is skipped: the retry loop re-enters with
			// the (now un-pinned) pre-retry sweep clearing the possibly
			// committed name before the next upload.
			a.logger.Warn("configdrive: direct-to-node dial failed; falling back to the configured endpoint",
				log.String("node", node),
				log.String("direct_host", directHost),
				log.String("hint", "verify the pve.node_endpoints entry for this node is a reachable address its certificate SANs cover"),
				log.Err(err),
			)
			a.nodeEndpoints.MarkDirectRouteFailed(node)
			directHost = ""
			pinnedCtx = ctx
			uploadCtx = sdkclient.WithRetries(ctx, 0)
			if !awaitPhase {
				err = attempt(uploadCtx)
			}
		}
		return err
	})
}

func (a *ConfigDrive) attachISO(ctx context.Context, node string, vmid int, filename string) error {
	value := fmt.Sprintf("%s:iso/%s,media=cdrom", a.storage, filename)
	params := &sdknodes.UpdateQemuConfigParams{
		Scsi: map[int]string{configDriveSlotIndex: value},
	}
	if err := a.pveSvc.Nodes().UpdateQemuConfig(ctx, node, strconv.Itoa(vmid), params); err != nil {
		return pve.WrapError(err)
	}
	return nil
}

// removeISOIfExists deletes the configdrive ISO at `filename` and reports
// whether anything was actually removed. Callers (Configure's pre-delete
// path) use the existed flag to log a warning when a stale orphan was
// found. 404 is treated as "did not exist" (returns false, nil); other
// errors propagate.
//
// The delete is awaited via the returned imgdel UPID before this function
// returns. PVE's DELETE on a storage volume queues an imgdel task under the
// per-storage lockfile; under bursty deploys that lock is contended and the
// imgdel can sit in the queue for seconds. Without an await, Configure would
// proceed to upload the replacement ISO and Start the VM *before* the queued
// imgdel ran, then PVE would fire imgdel during qmstart and remove the just-
// uploaded ISO — causing qmstart to fail with "volume … does not exist".
func (a *ConfigDrive) removeISOIfExists(ctx context.Context, node, filename string) (bool, error) {
	volume := fmt.Sprintf("%s:iso/%s", a.storage, filename)
	existed, upid, err := a.pveSvc.Storage().DeleteVolumeIfExistsAsync(ctx, node, a.storage, volume)
	if err != nil {
		return false, pve.WrapError(err)
	}
	if upid != "" {
		if werr := pve.AwaitTaskWithLogger(ctx, a.pveSvc, node, upid, a.logger); werr != nil {
			return existed, pve.WrapError(werr)
		}
	}
	return existed, nil
}

// removeISOFromStorage issues a DELETE for the configdrive ISO volume and
// treats 404 as success (idempotent). Awaits the queued imgdel UPID so
// downstream operations (e.g. re-uploading to the same name on a recycled
// VMID) cannot race a late-firing imgdel.
func (a *ConfigDrive) removeISOFromStorage(ctx context.Context, node, filename string) error {
	volume := fmt.Sprintf("%s:iso/%s", a.storage, filename)
	delErr := pve.RetryOnTransientOrLock(ctx, a.logger, "configdrive_delete", 0, func() error {
		upid, err := a.pveSvc.Storage().DeleteVolumeAsync(ctx, node, a.storage, volume)
		if err != nil {
			return err
		}
		if upid == "" {
			return nil
		}
		return pve.AwaitTaskWithLogger(ctx, a.pveSvc, node, upid, a.logger)
	})
	if delErr != nil {
		return pve.WrapError(delErr)
	}
	return nil
}

func configDriveISOFilename(vmid int) string {
	return fmt.Sprintf("vm-%d-config.iso", vmid)
}
