package handlers

import (
	"context"
	"fmt"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	stemcellfetch "github.com/fivetwenty-io/bosh-pve-cpi/internal/pve/stemcell_fetch"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// handleStemcellDownloadURL implements the server-side download path for
// create_stemcell when cloud_properties.source_url is set.
//
// PVE streams the image directly from source_url into storage via the
// download-url API (POST /nodes/{node}/storage/{storage}/download-url).
// The CPI does not transfer image bytes; only the PVE node needs network
// access to source_url. Requires PVE 7.2+.
//
// When cloud_properties.sha256 is also set, Checksum and ChecksumAlgorithm
// params are forwarded to PVE for server-side integrity verification. A task
// failure due to checksum mismatch is returned as a non-retriable cloud error.
// When sha256 is absent, no checksum params are sent.
//
// The returned CID is "template:<vmid>" — identical semantics to all other
// create_stemcell paths. All downstream steps (template VM creation, freeze,
// provenance, pool assignment) run identically via ensureTemplateVM.
//
// Flow:
//  1. Resolve storage and target node from config.
//  2. Derive canonical qcow2 filename from name, version, and sha256
//     (or placeholder sha8 "00000000" when sha256 is absent).
//  3. Pre-dedup: scan storage for existing volume with that filename. If
//     found, skip download and proceed directly to ensureTemplateVM.
//  4. Call CreateStorageDownloadUrl; await UPID via AwaitTask.
//  5. On task failure (including checksum mismatch): return non-retriable error.
//  6. Locate the downloaded volume by prefix scan to get the canonical volid.
//  7. ensureTemplateVM builds or reuses the frozen template VM.
//  8. Return "template:<vmid>".
func handleStemcellDownloadURL(
	ctx context.Context,
	deps Deps,
	cp stemcellCloudProps,
) (any, error) {
	// 1. Resolve storage and node. Reuse the shared helper that also enforces
	//    the shared-storage constraint for multi-node clusters.
	node, storage, resolveErr := resolveStemcellStorageAndNode(ctx, deps)
	if resolveErr != nil {
		return nil, resolveErr
	}

	templateNode := deps.Config.StemcellTemplateNode
	if templateNode == "" {
		templateNode = node
	}

	// 2. Derive canonical qcow2 filename.
	// When sha256 is provided, bake it into the filename for exact dedup and
	// content-based identity. When absent, sha8 defaults to "00000000" via
	// BuildStemcellFilename — two different source_urls with the same name+version
	// and no sha256 produce the same filename and will share the same import volume
	// (first-writer wins). Warn so operators are aware of the weak identity.
	if cp.ExpectedSHA256 == "" {
		deps.Logger.Warn("create_stemcell: server-download: sha256 not set; "+
			"filename identity is weak (name+version only) — strongly recommend "+
			"setting cloud_properties.sha256 alongside source_url",
			log.String("source_url", cp.SourceURL),
			log.String("name", cp.Name),
			log.String("version", cp.Version),
		)
	}
	qcow2Filename := pve.BuildStemcellFilename(cp.Name, cp.Version, cp.ExpectedSHA256)

	deps.Logger.Info("create_stemcell: server-side download requested",
		log.String("source_url", cp.SourceURL),
		log.String("node", node),
		log.String("storage", storage),
		log.String("filename", qcow2Filename),
		log.String("sha256", cp.ExpectedSHA256),
	)

	// 3. Pre-dedup: if the volume already exists, skip the download entirely.
	existingVol, findErr := pve.FindStemcellByFilename(ctx, deps.PVE, node, storage, qcow2Filename)
	if findErr != nil {
		return nil, cpierrors.Wrap(findErr, "create_stemcell: server-download dedup lookup")
	}
	if existingVol != "" {
		deps.Logger.Info("create_stemcell: server-download — volume already present, skipping download",
			log.String("volid", existingVol),
		)
		vmid, tmplErr := ensureTemplateVM(ctx, deps, templateNode, storage, qcow2Filename, cp.ExpectedSHA256, false, cp, cp.SourceURL)
		if tmplErr != nil {
			return nil, fmt.Errorf("create_stemcell: server-download dedup: ensure template: %w", tmplErr)
		}
		return pve.BuildTemplateStemcellCID(vmid), nil
	}

	// 4. Build CreateStorageDownloadUrl params.
	params := &sdknodes.CreateStorageDownloadUrlParams{
		Content:  "import",
		Filename: qcow2Filename,
		Url:      cp.SourceURL,
	}
	if cp.ExpectedSHA256 != "" {
		checksum := cp.ExpectedSHA256
		algo := "sha256"
		params.Checksum = &checksum
		params.ChecksumAlgorithm = &algo
	}

	if deps.PVE == nil || deps.PVE.Nodes() == nil {
		return nil, cpierrors.Cloud("create_stemcell: server-download: nodes service unavailable")
	}

	resp, dlErr := deps.PVE.Nodes().CreateStorageDownloadUrl(ctx, node, storage, params)
	if dlErr != nil {
		return nil, cpierrors.Cloud(
			"create_stemcell: server-download CreateStorageDownloadUrl failed: %s", dlErr.Error())
	}

	// 5. Extract UPID from response and await task.
	// A nil or empty response means PVE accepted but returned no task —
	// treat as immediate success (unexpected but not fatal).
	if resp != nil && len(*resp) > 0 {
		upid, upidErr := pve.UPIDFromRaw(*resp)
		if upidErr != nil {
			return nil, cpierrors.Cloud(
				"create_stemcell: server-download: cannot parse task UPID from response: %s", upidErr.Error())
		}
		if upid != "" {
			awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, deps.Logger,
				pve.WithMaxWait(pve.StemcellMaxWait))
			if awaitErr != nil {
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
				if _, delErr := deps.PVE.Storage().DeleteVolumeIfExists(ctx, node, storage, volumePath); delErr != nil {
					deps.Logger.Warn("create_stemcell: server-download: best-effort cleanup of partial volume failed (non-fatal)",
						log.String("volume", volumePath),
						log.Err(delErr),
					)
				}
				return nil, cpierrors.Cloud(
					"create_stemcell: server-download task failed (check source_url reachability from PVE node %q "+
						"and sha256 correctness): %s", node, awaitErr.Error())
			}
		}
	}

	deps.Logger.Info("create_stemcell: server-side download complete",
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
		return nil, cpierrors.Wrap(volFindErr, "create_stemcell: server-download: post-download volume lookup")
	}
	if downloadedVol == "" {
		// Fallback: prefix scan to handle PVE filename normalization.
		prefix := stemcellDownloadURLPrefix(cp.Name, cp.Version)
		downloadedVol, volFindErr = fetchFindByPrefix(ctx, deps, node, storage, prefix)
		if volFindErr != nil {
			return nil, cpierrors.Wrap(volFindErr, "create_stemcell: server-download: prefix scan after download")
		}
		if downloadedVol == "" {
			return nil, cpierrors.Cloud(
				"create_stemcell: server-download: volume not found after successful task "+
					"(storage=%q node=%q filename=%q)", storage, node, qcow2Filename)
		}
	}

	// Extract the actual filename PVE assigned to the volume (may differ from
	// qcow2Filename after normalization).
	actualFilename := fetchExtractFilename(downloadedVol)
	if actualFilename == "" {
		return nil, cpierrors.Cloud(
			"create_stemcell: server-download: cannot extract filename from volid %q", downloadedVol)
	}

	deps.Logger.Info("create_stemcell: server-download volume confirmed",
		log.String("volid", downloadedVol),
		log.String("actual_filename", actualFilename),
	)

	// 7. Build template VM from the downloaded volume.
	// server-download: CPI did not upload the volume (PVE owns the bytes), but
	// the volume is a new CPI-managed import. cpiOwnsSource=true so ensureTemplateVM
	// deletes the import volume after the template is frozen (import volumes
	// on stemcell storage are staging only; the template disk is the live copy).
	vmid, tmplErr := ensureTemplateVM(ctx, deps, templateNode, storage, actualFilename, cp.ExpectedSHA256, true, cp, cp.SourceURL)
	if tmplErr != nil {
		return nil, fmt.Errorf("create_stemcell: server-download: ensure template: %w", tmplErr)
	}

	templateCID := pve.BuildTemplateStemcellCID(vmid)
	deps.Logger.Info("create_stemcell: server-download stemcell ready",
		log.String("source_url", cp.SourceURL),
		log.String("cid", templateCID),
	)
	return templateCID, nil
}

// stemcellDownloadURLPrefix returns the filename prefix used for the fallback
// prefix scan after a server-side download. It delegates to
// stemcellfetch.FilenamePrefixForDedup so the prefix form matches exactly what
// handleLightStemcellFetch uses, keeping dedup semantics consistent across paths.
func stemcellDownloadURLPrefix(name, version string) string {
	return stemcellfetch.FilenamePrefixForDedup(name, version)
}
