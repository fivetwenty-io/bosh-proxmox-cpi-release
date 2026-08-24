package handlers

import (
	"context"
	"fmt"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	stemcellfetch "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/stemcell_fetch"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// handleStemcellDownloadURL implements the server-side download path for
// create_stemcell when cloud_properties.source_url is set.
//
// PVE streams the image directly from source_url into storage via the
// download-url API (POST /nodes/{node}/storage/{storage}/download-url).
// The CPI does not transfer image bytes; only the PVE node needs network
// access to source_url. Requires PVE 7.2+.
//
// cloud_properties.sha256 is mandatory: Checksum and ChecksumAlgorithm
// params are forwarded to PVE for server-side integrity verification, and a
// task failure due to checksum mismatch is returned as a non-retriable cloud
// error. Content identity (the sha-tag cache-template lookup and the CID's
// filename) is derived entirely from this digest — PVE, not the CPI, streams
// the bytes, so there is no independent point at which the CPI could compute
// or verify one after the fact.
//
// The returned CID is always ":heavy:<storage>:import/<file>" — PVE, not the
// CPI, transferred the bytes, but the CPI owns the resulting import volume
// (it is not operator-preuploaded). All downstream steps (template VM
// creation, freeze, provenance, pool assignment, director-ref registration)
// run identically via ensureTemplateAndRegisterRef.
//
// Flow:
//  1. Resolve storage and target node from config; reject the call outright
//     when cloud_properties.sha256 is missing or malformed.
//  2. Derive canonical qcow2 filename from name, version, and sha256.
//  3. Pre-dedup: scan storage for existing volume with that filename. If
//     found, skip download and proceed directly to ensureTemplateAndRegisterRef.
//  4. Call CreateStorageDownloadUrl; await UPID via AwaitTask.
//  5. On task failure (including checksum mismatch): return non-retriable error.
//  6. Locate the downloaded volume by prefix scan to get the canonical volid.
//  7. ensureTemplateAndRegisterRef builds or reuses the cache template and
//     registers directorUUID as a live reference.
//  8. Return the ":heavy:" CID.
func handleStemcellDownloadURL(
	ctx context.Context,
	deps Deps,
	cp stemcellCloudProps,
	directorUUID string,
) (any, error) {
	out, _, err := handleStemcellDownloadURLTracked(ctx, deps, cp, directorUUID)
	return out, err
}

// handleStemcellDownloadURLTracked is handleStemcellDownloadURL plus a
// taskStarted result: true once CreateStorageDownloadUrl has been accepted by
// PVE, meaning a server-side download task may exist (running, finished, or
// failed). The image_url dispatch uses it to bound its CPI-side fallback —
// falling back after the task started would race a still-running download for
// the same import/<file> path (the two writers can interleave, and the
// fallback's prefix dedup could then adopt a half-written volume).
func handleStemcellDownloadURLTracked(
	ctx context.Context,
	deps Deps,
	cp stemcellCloudProps,
	directorUUID string,
) (any, bool, error) {
	// 1. Resolve storage and node. Reuse the shared helper that also enforces
	//    the shared-storage constraint for multi-node clusters.
	node, storage, resolveErr := resolveStemcellStorageAndNode(ctx, deps)
	if resolveErr != nil {
		return nil, false, resolveErr
	}

	templateNode := deps.Config.StemcellTemplateNode
	if templateNode == "" {
		templateNode = node
	}

	// 1b. sha256 is mandatory for server-side download. Without it the
	// resulting template would need a sha8 tag derived from
	// BuildStemcellFilename's "00000000" unknown-digest placeholder — but that
	// placeholder is a literal constant shared by every source_url stemcell
	// lacking a digest, regardless of actual content, so tagging with it would
	// make the sha-tag cluster scan (FindTemplatesBySHATagCluster) treat
	// unrelated stemcells as the same candidate (sha256MatchesTemplateProvenance
	// trusts a tag match when neither side records a full digest). Unlike the
	// light-fetch prefix-dedup case, there is no genuinely content-derived sha8
	// available here to fall back on: PVE, not the CPI, streams the bytes, so
	// this path never computes one. Requiring the digest up front — the same
	// requirement handleLightStemcellPreUploaded already enforces — closes the
	// gap at the root instead of emitting an identity the rest of the system
	// cannot safely dedup or look up by.
	if !isValidSHA256Hex(cp.ExpectedSHA256) {
		return nil, false, cpierrors.Cloud(
			"create_stemcell: server-download (cloud_properties.source_url) requires cloud_properties.sha256 "+
				"so content identity and dedup work (must be a 64-character hex string, got %q)", cp.ExpectedSHA256)
	}

	// 2. Derive canonical qcow2 filename from the verified digest.
	qcow2Filename := pve.BuildStemcellFilename(cp.Name, cp.Version, cp.ExpectedSHA256)
	stemcellCID := pve.BuildHeavyStemcellCID(storage, qcow2Filename)

	deps.Log(ctx).Info("create_stemcell: server-side download requested",
		log.URL("source_url", cp.SourceURL),
		log.String("node", node),
		log.String("storage", storage),
		log.String("filename", qcow2Filename),
		log.String(jsonKeySHA256, cp.ExpectedSHA256),
	)

	// 3. Pre-dedup: if the volume already exists, skip the download entirely.
	existingVol, findErr := pve.FindStemcellByFilename(ctx, deps.PVE, node, storage, qcow2Filename)
	if findErr != nil {
		return nil, false, cpierrors.Wrap(findErr, "create_stemcell: server-download dedup lookup")
	}
	if existingVol != "" {
		deps.Log(ctx).Info("create_stemcell: server-download — volume already present, skipping download",
			log.String("volid", existingVol),
		)
		vmid, winnerNode, tmplErr := ensureTemplateAndRegisterRef(ctx, deps, deps.Log(ctx),
			templateNode, storage, qcow2Filename, cp.ExpectedSHA256, "",
			pve.StemcellKindHeavy, stemcellCID, directorUUID, cp, cp.SourceURL)
		if tmplErr != nil {
			return nil, false, fmt.Errorf("create_stemcell: server-download dedup: ensure template: %w", tmplErr)
		}
		deps.Log(ctx).Info("create_stemcell: server-download (dedup) template ready",
			log.Int64(metadataKeyVMID, vmid),
			log.String("template_node", winnerNode),
			log.String("cid", stemcellCID),
		)
		maybeReplicateServerDownload(ctx, deps, templateNode, storage, qcow2Filename,
			cp.ExpectedSHA256, cp.SourceURL, stemcellCID, directorUUID, cp)
		return stemcellCID, false, nil
	}

	// 4. Build CreateStorageDownloadUrl params.
	params := &sdknodes.CreateStorageDownloadUrlParams{
		Content:  pveStorageContentImport,
		Filename: qcow2Filename,
		Url:      cp.SourceURL,
	}
	if cp.ExpectedSHA256 != "" {
		checksum := cp.ExpectedSHA256
		algo := jsonKeySHA256
		params.Checksum = &checksum
		params.ChecksumAlgorithm = &algo
	}

	if deps.PVE == nil || deps.PVE.Nodes() == nil {
		return nil, false, cpierrors.Cloud("create_stemcell: server-download: nodes service unavailable")
	}

	// RetryOnTransientOrLock + WrapErrorKeepingClass: the download-url POST
	// allocates in the target storage (it can collide on the storage lock),
	// and its exhausted error was previously flattened to a permanent Cloud.
	var resp *sdknodes.CreateStorageDownloadUrlResponse
	dlErr := pve.RetryOnTransientOrLock(ctx, deps.Log(ctx), "create_stemcell.download_url", 0, func() error {
		var inner error
		resp, inner = deps.PVE.Nodes().CreateStorageDownloadUrl(ctx, node, storage, params)
		return inner
	})
	if dlErr != nil {
		return nil, false, cpierrors.Wrap(pve.WrapErrorKeepingClass(dlErr),
			"create_stemcell: server-download CreateStorageDownloadUrl failed")
	}

	// 5. Extract UPID from response and await task.
	// A nil or empty response means PVE accepted but returned no task —
	// treat as immediate success (unexpected but not fatal).
	if resp != nil && len(*resp) > 0 {
		upid, upidErr := pve.UPIDFromRaw(*resp)
		if upidErr != nil {
			return nil, true, cpierrors.Cloud(
				"create_stemcell: server-download: cannot parse task UPID from response: %s", upidErr.Error())
		}
		if upid != "" {
			awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, deps.Log(ctx),
				pve.WithMaxWait(pve.StemcellMaxWait))
			if awaitErr != nil {
				// A retriable await error means the wait budget ran out (or a
				// transient poll fault) while the task may still be running
				// server-side. The partial file must NOT be deleted out from
				// under a live download; propagate the retriable class so the
				// Director's retried create_stemcell re-enters and dedups on
				// the completed file.
				if okToRetryCPIError(awaitErr) {
					return nil, true, cpierrors.WrapAs(awaitErr, cpierrors.TypeRetriableCloud,
						fmt.Sprintf("create_stemcell: server-download task on node %q may still be running — retry once it completes", node))
				}
				// Task failure includes checksum mismatch reported by PVE.
				// Non-retriable: the URL or checksum is wrong; retrying the same
				// call will not fix the root cause.
				//
				// Best-effort: attempt to remove the partial import volume PVE may
				// have written before the task failed (e.g. partially-streamed file
				// left by a checksum failure). Mirrors the fetch path's deferred
				// os.Remove of the temp file. PVE may have already removed it; ignore
				// the delete result so the cleanup never masks the original error.
				volumePath := "import/" + qcow2Filename
				if delErr := pve.RetryOnTransientOrLock(ctx, deps.Log(ctx), "create_stemcell.download_partial_sweep", cleanupSweepMaxAttempts, func() error {
					_, innerErr := deps.PVE.Storage().DeleteVolumeIfExists(ctx, node, storage, volumePath)
					return innerErr
				}); delErr != nil {
					deps.Log(ctx).Warn("create_stemcell: server-download: best-effort cleanup of partial volume failed (non-fatal)",
						log.String("volume", volumePath),
						log.Err(delErr),
					)
				}
				return nil, true, cpierrors.Cloud(
					"create_stemcell: server-download task failed (check source_url reachability from PVE node %q "+
						"and sha256 correctness): %s", node, awaitErr.Error())
			}
		}
	}

	deps.Log(ctx).Info("create_stemcell: server-side download complete",
		log.String("node", node),
		log.String("storage", storage),
		log.String("filename", qcow2Filename),
	)

	// 6. Locate the downloaded volume. PVE normalizes filenames so the volid
	//    may differ slightly from what we requested. Prefer exact lookup first;
	//    fall back to prefix scan when sha256 was absent (filename ends in
	//    "00000000.qcow2" which is a valid but non-unique suffix).
	downloadedVol, volFindErr := pve.FindStemcellByFilename(ctx, deps.PVE, node, storage, qcow2Filename)
	if volFindErr != nil {
		return nil, true, cpierrors.Wrap(volFindErr, "create_stemcell: server-download: post-download volume lookup")
	}
	if downloadedVol == "" {
		// Fallback: prefix scan to handle PVE filename normalization.
		prefix := stemcellDownloadURLPrefix(cp.Name, cp.Version)
		downloadedVol, volFindErr = fetchFindByPrefix(ctx, deps, node, storage, prefix)
		if volFindErr != nil {
			return nil, true, cpierrors.Wrap(volFindErr, "create_stemcell: server-download: prefix scan after download")
		}
		if downloadedVol == "" {
			return nil, true, cpierrors.Cloud(
				"create_stemcell: server-download: volume not found after successful task "+
					"(storage=%q node=%q filename=%q)", storage, node, qcow2Filename)
		}
	}

	// Extract the actual filename PVE assigned to the volume (may differ from
	// qcow2Filename after normalization).
	actualFilename := fetchExtractFilename(downloadedVol)
	if actualFilename == "" {
		return nil, true, cpierrors.Cloud(
			"create_stemcell: server-download: cannot extract filename from volid %q", downloadedVol)
	}

	deps.Log(ctx).Info("create_stemcell: server-download volume confirmed",
		log.String("volid", downloadedVol),
		log.String("actual_filename", actualFilename),
	)

	// 7. Ensure the cache template from the downloaded volume and register
	// this director's reference. server-download: PVE (not the CPI) streamed
	// the bytes, but the CPI owns the resulting import volume — it is not
	// operator-preuploaded, so the CID is :heavy: and, per D10, the volume is
	// never reclaimed here (delete_stemcell owns last-ref deletion). The
	// actual downloaded filename (actualFilename, which may differ from
	// qcow2Filename after PVE normalization) is used for identity from here
	// on, so the CID reflects the same filename PVE registered.
	actualCID := pve.BuildHeavyStemcellCID(storage, actualFilename)
	vmid, winnerNode, tmplErr := ensureTemplateAndRegisterRef(ctx, deps, deps.Log(ctx),
		templateNode, storage, actualFilename, cp.ExpectedSHA256, "",
		pve.StemcellKindHeavy, actualCID, directorUUID, cp, cp.SourceURL)
	if tmplErr != nil {
		return nil, true, fmt.Errorf("create_stemcell: server-download: ensure template: %w", tmplErr)
	}

	deps.Log(ctx).Info("create_stemcell: server-download stemcell ready",
		log.URL("source_url", cp.SourceURL),
		log.Int64(metadataKeyVMID, vmid),
		log.String("template_node", winnerNode),
		log.String("cid", actualCID),
	)
	maybeReplicateServerDownload(ctx, deps, templateNode, storage, actualFilename,
		cp.ExpectedSHA256, cp.SourceURL, actualCID, directorUUID, cp)
	return actualCID, true, nil
}

// maybeReplicateServerDownload applies the same replication gate
// HandleCreateStemcell's mainline uses (opt-in, template-disk pool node-local
// per templateReplicasNeeded, more than one cluster node) before fanning the
// download out to every other node via
// replicateServerDownloadToNodes. Both handleStemcellDownloadURL return paths
// (pre-dedup hit and fresh download) call this at the equivalent point after
// template creation, so a source_url stemcell is no longer stranded on the
// single node PVE happened to download it to.
//
// Best-effort: a failure to list cluster nodes only skips replication and
// logs a warning; it never fails the call whose CID was already committed.
func maybeReplicateServerDownload(
	ctx context.Context,
	deps Deps,
	templateNode, storage, qcow2Filename, sha256hex, sourceURL, stemcellCID, directorUUID string,
	cp stemcellCloudProps,
) {
	if sha256hex == "" {
		return
	}
	if !templateReplicasNeeded(ctx, deps) {
		return
	}
	clusterNodes, listErr := listClusterNodes(ctx, deps)
	if listErr != nil {
		deps.Log(ctx).Warn("create_stemcell: server-download: replication: cannot list cluster nodes (skipping replication)",
			log.Err(listErr),
		)
		return
	}
	if len(clusterNodes) <= 1 {
		return
	}
	replicateServerDownloadToNodes(ctx, deps, templateNode, storage, qcow2Filename,
		sha256hex, clusterNodes, sourceURL, stemcellCID, directorUUID, cp)
}

// stemcellDownloadURLPrefix returns the filename prefix used for the fallback
// prefix scan after a server-side download. It delegates to
// stemcellfetch.FilenamePrefixForDedup so the prefix form matches exactly what
// handleLightStemcellFetch uses, keeping dedup semantics consistent across paths.
func stemcellDownloadURLPrefix(name, version string) string {
	return stemcellfetch.FilenamePrefixForDedup(name, version)
}
