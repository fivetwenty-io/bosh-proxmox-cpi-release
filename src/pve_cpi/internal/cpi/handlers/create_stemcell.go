package handlers

import (
	"archive/tar"
	"compress/gzip"
	"context"
	cryptosha1 "crypto/sha1" // #nosec G505 -- SHA-1 used only for operator-supplied expected-digest comparison, not for security
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	stemcellfetch "github.com/fivetwenty-io/bosh-pve-cpi/internal/pve/stemcell_fetch"

	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
)

// MaxStemcellTotalExtract caps cumulative extracted bytes from a stemcell
// tarball. A stemcell tarball contains one disk image (typically 3-10 GiB
// compressed to ~700 MiB) plus small metadata files. 32 GiB is a hard upper
// bound above any real stemcell; exceeding it indicates a tar-bomb or
// corrupted archive.
const MaxStemcellTotalExtract = 32 * 1024 * 1024 * 1024 // 32 GiB

// pveStorageContentImport is the PVE storage "content" type for stemcell
// qcow2 volumes (ListStorageContent filter, Upload/CreateStorageDownloadUrl
// content param). Distinct from config.StemcellStrategyImport, which names
// the unrelated create_vm clone-vs-import strategy.
const pveStorageContentImport = "import"

// normalizeOSType maps common BOSH/stemcell os_type values to PVE's enumerated
// ostype set (other, wxp, w2k, w2k3, w2k8, wvista, win7-11, l24, l26, solaris).
// Anything not recognized is returned verbatim and will be validated by PVE.
func normalizeOSType(v string) string {
	switch v {
	case "linux", "ubuntu", "centos", "rhel", "debian", "fedora", "alpine", osTypeLinux26:
		return osTypeLinux26
	case "linux24", "l24":
		return osTypeLinux24
	case "windows", "win", "win10":
		return osTypeWindows
	case "win11":
		return "win11"
	case "win7":
		return "win7"
	case "win8":
		return "win8"
	case "solaris":
		return "solaris"
	default:
		return v
	}
}

// stemcellCloudProps holds fields parsed from the create_stemcell cloud_properties argument.
type stemcellCloudProps struct {
	// DiskFormat is the image format: qcow2, raw, vmdk. Defaults to "qcow2".
	DiskFormat string
	// OSType is the PVE OS type hint: l26, win10, etc. Defaults to "l26".
	OSType string
	// Name is the stemcell name from stemcell.MF (required for CID).
	Name string
	// Version is the stemcell version from stemcell.MF (required for CID).
	Version string
	// DiskMiB is the disk size hint in MiB; 0 means no hint.
	DiskMiB int
	// ImageID identifies a pre-uploaded volume the operator placed externally.
	// Format: "<storage>:import/<file>" — e.g. "nfs:import/ubuntu-jammy.qcow2".
	// Mutually exclusive with ImageURL.
	ImageID string
	// ImageURL is a remote URL from which the CPI fetches the disk image.
	// Supported schemes: https://, s3://, bosh+blobstore:, oci://.
	// Mutually exclusive with ImageID.
	ImageURL string
	// ImageURLAuth holds per-stemcell credentials that override config defaults
	// when non-empty. Re-marshalled from the raw cloud_properties value so
	// callers receive canonical JSON bytes.
	ImageURLAuth json.RawMessage
	// Node pins light-stemcell placement to a specific cluster node.
	// Used when stemcell storage is local-dir and multi-node placement matters.
	Node string
	// SourceURL is a remote URL for PVE server-side download via the
	// download-url storage API. When set, PVE streams the image directly into
	// storage without the CPI transferring bytes. Requires PVE 7.2+.
	// Mutually exclusive with ImageID and ImageURL.
	SourceURL string

	// ExpectedSHA256 is the expected SHA-256 hex digest from cloud_properties.sha256.
	// When non-empty the CPI verifies the computed digest matches after
	// download or extraction. SHA-256 takes precedence over SHA-1 when both
	// are provided. Empty means integrity is unverified (warn only).
	ExpectedSHA256 string

	// ExpectedSHA1 is the expected SHA-1 hex digest from cloud_properties.sha1.
	// Compared only when ExpectedSHA256 is empty. Empty means no SHA-1 check.
	ExpectedSHA1 string

	// DirectorTags holds per-stemcell tags supplied by the BOSH Director via the
	// optional env argument on create_stemcell (CPI v3 contract). It is populated
	// by HandleCreateStemcell from env[jsonKeyTags], NOT from cloud_properties.
	// nil/empty means no director-supplied tags.
	DirectorTags map[string]string
}

// validateLightMutex returns an error when more than one of ImageID, ImageURL,
// or SourceURL are set simultaneously. Each identifies a different upload
// mechanism; combining them is an operator error.
func (p stemcellCloudProps) validateLightMutex() error {
	set := 0
	if p.ImageID != "" {
		set++
	}
	if p.ImageURL != "" {
		set++
	}
	if p.SourceURL != "" {
		set++
	}
	if set > 1 {
		return cpierrors.Cloud(
			"create_stemcell: cloud_properties.image_id, cloud_properties.image_url, and " +
				"cloud_properties.source_url are mutually exclusive; set at most one")
	}
	return nil
}

// IsLight reports whether the stemcell is a light stemcell (no local tarball
// required). True when ImageID, ImageURL, or SourceURL is set.
func (p stemcellCloudProps) IsLight() bool {
	return p.ImageID != "" || p.ImageURL != "" || p.SourceURL != ""
}

// LightMode returns the light-stemcell variant string:
//   - "preuploaded"      when ImageID is set (operator pre-placed volume)
//   - "fetch"            when ImageURL is set (CPI fetches from remote URL)
//   - "server-download"  when SourceURL is set (PVE downloads server-side)
//   - ""                 when none is set (heavy stemcell, normal tarball upload)
func (p stemcellCloudProps) LightMode() string {
	if p.ImageID != "" {
		return "preuploaded"
	}
	if p.ImageURL != "" {
		return "fetch"
	}
	if p.SourceURL != "" {
		return "server-download"
	}
	return ""
}

// parseStemcellCloudProps extracts known fields from cloud_properties.
// Missing or unrecognized keys are silently ignored; defaults apply.
func parseStemcellCloudProps(cp map[string]any) stemcellCloudProps {
	p := stemcellCloudProps{
		DiskFormat: diskFormatQCOW2,
		OSType:     osTypeLinux26,
	}

	if v, ok := cp["disk_format"].(string); ok && v != "" {
		p.DiskFormat = v
	}
	if v, ok := cp["os_type"].(string); ok && v != "" {
		p.OSType = normalizeOSType(v)
	}
	if v, ok := cp[metadataKeyName].(string); ok {
		p.Name = v
	}
	if v, ok := cp["version"].(string); ok {
		p.Version = v
	}
	// disk field may arrive as float64 (JSON number decoded by encoding/json).
	switch v := cp["disk"].(type) {
	case float64:
		p.DiskMiB = int(v)
	case int:
		p.DiskMiB = v
	case int64:
		p.DiskMiB = int(v)
	}

	if v, ok := cp["image_id"].(string); ok {
		p.ImageID = v
	}
	if v, ok := cp["image_url"].(string); ok {
		p.ImageURL = v
	}
	// image_url_auth: accept json.RawMessage directly or re-marshal from any.
	// Re-marshal needed because encoding/json decodes JSON objects as map[string]any,
	// and the caller needs canonical JSON bytes for downstream credential parsing.
	if raw, ok := cp["image_url_auth"].(json.RawMessage); ok {
		p.ImageURLAuth = raw
	} else if v, ok := cp["image_url_auth"]; ok && v != nil {
		if b, merr := json.Marshal(v); merr == nil {
			p.ImageURLAuth = b
		}
		// marshal failure is impossible for values that came from json.Unmarshal;
		// on the off chance it occurs, leave ImageURLAuth nil so the caller falls
		// back to config-level credentials rather than using garbled bytes.
	}
	if v, ok := cp["node"].(string); ok {
		p.Node = v
	}
	if v, ok := cp[jsonKeySHA256].(string); ok {
		p.ExpectedSHA256 = v
	}
	if v, ok := cp["sha1"].(string); ok {
		p.ExpectedSHA1 = v
	}
	if v, ok := cp["source_url"].(string); ok {
		p.SourceURL = v
	}

	return p
}

// stemcellStagingRoots returns the absolute, cleaned set of directories under
// which an incoming image_path is permitted to resolve. The set covers the two
// standard BOSH stemcell staging locations:
//
//  1. os.TempDir() — the BOSH director stages stemcells in its scratch area on
//     the CPI host; on Linux this is /tmp (or $TMPDIR), on macOS /var/folders/…
//
//  2. <HOME>/.bosh/installations — `bosh create-env` runs the CPI locally on
//     the operator host and stages images under
//     ~/.bosh/installations/<id>/tmp/stemcell-manager<n>/image. This path is
//     NOT inside os.TempDir(), so without this root every create-env stemcell
//     upload would be rejected as "outside permitted staging root".
//
// Both roots are resolved through EvalSymlinks once so a real image path that
// resolves to a symlinked realpath (e.g. /tmp -> /private/tmp on macOS) still
// matches.
func stemcellStagingRoots() []string {
	roots := []string{os.TempDir()}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, filepath.Join(home, ".bosh", "installations"))
	}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if r == "" {
			continue
		}
		abs, err := filepath.Abs(r)
		if err != nil {
			abs = r
		}
		clean := filepath.Clean(abs)
		if resolved, err := filepath.EvalSymlinks(clean); err == nil {
			clean = resolved
		}
		out = append(out, clean)
	}
	return out
}

// validateStemcellImagePath rejects an image_path that does not resolve under
// any element of stemcellStagingRoots. Containment is verified via filepath.Rel
// against the absolute, symlink-resolved form of both sides; a relative result
// that does not start with ".." (and is not the OS-specific volume-traversal
// form) means the path is inside the root. A "../" prefix or an error from Rel
// means the path escapes the root and is rejected.
//
// The check tolerates a non-existent image_path: filepath.EvalSymlinks fails
// for missing files, in which case the cleaned absolute form is compared as-is.
// The subsequent os.Stat call surfaces the missing-file error with a more
// specific message; this function's job is only to reject containment breaches.
func validateStemcellImagePath(imagePath string) error {
	abs, err := filepath.Abs(imagePath)
	if err != nil {
		return cpierrors.Cloud(
			"create_stemcell: imagePath %q could not be resolved: %s", imagePath, err.Error())
	}
	clean := filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		clean = resolved
	}

	roots := stemcellStagingRoots()
	for _, root := range roots {
		rel, err := filepath.Rel(root, clean)
		if err != nil {
			// Rel fails when root and clean are on different volumes (Windows);
			// on a unix CPI host this is effectively unreachable, but treat it
			// as a non-match rather than an error so the next root is tried.
			continue
		}
		// rel == "." means clean IS root; reject (must be a file under root).
		// A "../" prefix means clean escapes root via traversal — reject.
		// Anything else (e.g. "subdir/file.tgz") is contained — accept.
		if rel == "." {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return nil
	}

	return cpierrors.StemcellEscapedRoot(
		"create_stemcell: imagePath %q outside permitted staging root", imagePath)
}

// HandleCreateStemcell returns a Handler for the BOSH CPI create_stemcell method.
//
// Arguments (positional JSON array):
//
//	[0] image_path      string — absolute local path to stemcell disk image (or tarball).
//	[1] cloud_properties object — stemcell.MF cloud_properties section (may be omitted).
//	[2] env             object — optional CPI v3 argument; env[jsonKeyTags] (map[string]string)
//	                    carries director-supplied tags merged into the template's PVE tags
//	                    and provenance notes. Absent/null/non-object env is ignored
//	                    (2-arg calls are byte-identical to today's behavior).
//
// Returns: stemcell_cid string — a path-identity CID, ":light:<storage>:import/<file>"
// or ":heavy:<storage>:import/<file>" (see pve.BuildLightStemcellCID /
// pve.BuildHeavyStemcellCID). The qcow2 file at that storage path — not any PVE
// VMID — IS the stemcell's identity:
//
//   - ":light:" — the operator owns the file (preuploaded via cloud_properties.image_id).
//     The CPI never deletes it.
//   - ":heavy:" — the CPI uploaded or downloaded the file (tarball upload, image_url
//     fetch, or source_url server-download). The CPI deletes it when the last
//     registered BOSH director reference within this cluster is dropped
//     (delete_stemcell), never as a side effect of create_stemcell.
//
// Every mode additionally builds (or reuses) a per-cluster PVE template VM —
// tagged "bosh-stemcell-cache" — that serves two purposes: a clone source for
// create_vm's template strategy, and the anchor that stores the live set of
// BOSH director UUIDs referencing this CID (its provenance notes JSON,
// "director_refs"). The template VMID is never part of the returned CID and
// is never exposed to the Director. Template identity/dedup is cluster-scoped
// (pve.FindTemplatesBySHATagCluster / pve.FindTemplateByNameCluster) so every
// node in the cluster — and every director calling through it — converges on
// the same cache template.
//
// Registering the calling director's reference (registerStemcellDirectorRef)
// is a hard step on every return path, fresh build and dedup hit alike: a
// silently-dropped registration would let a later delete_stemcell from a
// different director destroy a template this caller still depends on.
//
// PVE's content APIs do not accept arbitrary metadata for import volumes
// (the upload endpoint validates file extension; the notes endpoint is
// backup-only). The qcow2 filename encodes name, version, and sha8; full
// content provenance (name, version, full sha256, kind, creating/referencing
// director UUIDs) lives in the cache template's description JSON.
//
// Error cases returned as *cpierrors.Error (TypeCloud, not retriable):
//   - Missing/non-string image_path.
//   - image_path does not exist or is not a regular file.
//   - Missing/non-object cloud_properties when provided.
//   - cloud_properties.name or cloud_properties.version is empty.
//   - config.node or effective storage is empty.
//   - Stemcell storage is local-only and cluster has more than one node.
//   - Image extraction or SHA-256 failure.
//   - qcow2 upload failure.
//   - preuploaded (image_id) mode without a valid cloud_properties.sha256.
//   - server-download (source_url) mode without a valid cloud_properties.sha256.
//
//nolint:gocognit,gocyclo // Orchestration shell: light-vs-heavy dispatch then heavy-path phases (resolveStemcellStorageAndNode, buildAndDeduplicateStemcellCID, uploadAndReturnCID). Phase logic lives in extracted helpers. CPI v3 env-arg parsing adds one branch to the already-high count.
func HandleCreateStemcell(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
		// ----------------------------------------------------------------
		// Step 1a: Parse arg 0 — image_path string (always required)
		// ----------------------------------------------------------------
		if len(args) < 1 {
			return nil, cpierrors.Cloud("create_stemcell: missing required argument image_path")
		}
		var imagePath string
		if err := json.Unmarshal(args[0], &imagePath); err != nil || imagePath == "" {
			return nil, cpierrors.Cloud("create_stemcell: image_path must be a non-empty string")
		}

		// ----------------------------------------------------------------
		// Step 2: Parse arg 1 — cloud_properties (optional)
		// ----------------------------------------------------------------
		var cloudProps map[string]any
		if len(args) >= 2 && args[1] != nil {
			if err := json.Unmarshal(args[1], &cloudProps); err != nil {
				return nil, cpierrors.Cloud(
					"create_stemcell: cloud_properties must be a JSON object: %s", err.Error())
			}
		}
		if cloudProps == nil {
			cloudProps = map[string]any{}
		}

		cp := parseStemcellCloudProps(cloudProps)
		if err := cp.validateLightMutex(); err != nil {
			return nil, err
		}

		// ----------------------------------------------------------------
		// Step 2b: Parse arg 2 — env (optional, CPI v3).
		// env[jsonKeyTags] carries director-supplied key/value pairs to stamp on the
		// stemcell template. Absent/null/non-object env is tolerated (ignored).
		// Non-string tag values are silently skipped; only string→string pairs
		// are retained. When env is absent this block is a no-op and cp is
		// byte-identical to a 2-arg call.
		// ----------------------------------------------------------------
		if len(args) >= 3 && args[2] != nil {
			var envMap map[string]any
			if jsonErr := json.Unmarshal(args[2], &envMap); jsonErr == nil && envMap != nil {
				if rawTags, ok := envMap[jsonKeyTags]; ok && rawTags != nil {
					if tagsMap, ok := rawTags.(map[string]any); ok {
						directorTags := make(map[string]string, len(tagsMap))
						for k, v := range tagsMap {
							if sv, ok := v.(string); ok {
								directorTags[k] = sv
							}
						}
						if len(directorTags) > 0 {
							cp.DirectorTags = directorTags
						}
					}
				}
			}
			// Non-object env (e.g. null, string, number) is silently ignored so
			// old directors passing unexpected types cannot break create_stemcell.
		}

		// ----------------------------------------------------------------
		// Step 1b: image_path file checks — skipped for light stemcells.
		// Light mode provides a pre-uploaded or remotely-fetched image;
		// the BOSH director may pass /dev/null or a synthetic path here
		// that must not be stat'd. Path-containment and regular-file
		// checks only apply to the heavy (local tarball) upload path.
		// ----------------------------------------------------------------
		if !cp.IsLight() {
			// Path containment: image_path must resolve under a permitted staging root.
			// The BOSH director writes stemcell tarballs to its own scratch area before
			// invoking the CPI; that area is os.TempDir() on the CPI host (the director
			// stages via /tmp by default). Any path that resolves outside this root —
			// e.g. /etc/passwd, /var/vcap/jobs/uaa/config/uaa.yml — is rejected so a
			// malicious or compromised director cannot use create_stemcell as a path
			// probe against arbitrary host files. The check uses filepath.Rel against
			// the resolved-absolute form of both sides so symlinks or "../" sequences
			// that escape the root are caught.
			if pathErr := validateStemcellImagePath(imagePath); pathErr != nil {
				return nil, pathErr
			}

			fi, statErr := os.Stat(imagePath)
			if statErr != nil {
				if os.IsNotExist(statErr) {
					return nil, cpierrors.Cloud("create_stemcell: image_path %q does not exist", imagePath)
				}
				return nil, cpierrors.Cloud("create_stemcell: cannot stat image_path %q: %s", imagePath, statErr.Error())
			}
			if !fi.Mode().IsRegular() {
				return nil, cpierrors.Cloud("create_stemcell: image_path %q is not a regular file", imagePath)
			}
		}

		// ----------------------------------------------------------------
		// Step 3: Validate name + version (required for deterministic CID)
		// ----------------------------------------------------------------
		if cp.Name == "" {
			return nil, cpierrors.Cloud(
				"create_stemcell: cloud_properties.name is required for direct-qcow stemcell upload")
		}
		if cp.Version == "" {
			return nil, cpierrors.Cloud(
				"create_stemcell: cloud_properties.version is required for direct-qcow stemcell upload")
		}

		// ----------------------------------------------------------------
		// Light-stemcell short-circuit. cp.LightMode() returns "preuploaded"
		// or "fetch" when the operator supplied image_id or image_url; both
		// bypass the local image_path upload pipeline entirely.
		// ----------------------------------------------------------------
		switch cp.LightMode() {
		case "preuploaded":
			return handleLightStemcellPreUploaded(ctx, deps, cp, reqCtx.DirectorUUID)
		case "fetch":
			// Prefer the PVE server-side download-url path when the image_url
			// qualifies (https, no credentials, sha256 present, no node pin):
			// PVE streams the bytes itself and the CPI never transfers them,
			// which sidesteps the cluster-proxy upload problem class entirely.
			// Both paths derive the same digest-based filename and ":heavy:"
			// CID, so a failed server-side attempt falls back to the CPI-side
			// fetch below without changing the stemcell's identity — but ONLY
			// while no download task was started: once PVE accepted the task,
			// a fallback would race a possibly-still-running server-side
			// download for the same import/<file> path, so the server-side
			// error (retriable when the task may still be running) is returned
			// instead and the Director's retry re-enters through the dedup.
			if serverSideFetchEligible(deps, cp) {
				sd := cp
				sd.SourceURL = cp.ImageURL
				sd.ImageURL = ""
				out, taskStarted, sdErr := handleStemcellDownloadURLTracked(ctx, deps, sd, reqCtx.DirectorUUID)
				if sdErr == nil {
					return out, nil
				}
				if taskStarted {
					return nil, sdErr
				}
				deps.Log(ctx).Warn("create_stemcell: server-side download for image_url could not start; falling back to the CPI-side fetch",
					log.URL("image_url", cp.ImageURL),
					log.Err(sdErr),
				)
			}
			return handleLightStemcellFetch(ctx, deps, cp, reqCtx.DirectorUUID)
		case "server-download":
			return handleStemcellDownloadURL(ctx, deps, cp, reqCtx.DirectorUUID)
		}

		// Steps 4-5: Resolve target node+storage and validate shared constraint.
		node, storage, resolveErr := resolveStemcellStorageAndNode(ctx, deps)
		if resolveErr != nil {
			return nil, resolveErr
		}

		// Steps 6-10: Extract/hash image, build the qcow2 filename, dedup check.
		// Cleanup is the tmpDir teardown for the (possibly extracted) image; the
		// caller owns its lifetime so the upload step can reuse the same path.
		// sha256hex is returned alongside the filename so the template tag can be
		// set without a second file-read pass.
		dedup, buildErr := buildAndDeduplicateStemcellCID(
			ctx, deps, node, storage, imagePath, cp, deps.Log(ctx))
		if buildErr != nil {
			return nil, buildErr
		}
		defer dedup.Cleanup()

		found := dedup.Found
		uploadSourcePath := dedup.UploadSourcePath
		sha256hex := dedup.SHA256Hex
		qcow2Filename := dedup.QCow2Filename
		// heavy always: the tarball-upload path (mainline and the qcow2-already-
		// on-storage dedup arm) is always CPI-owned bytes — the CID's identity
		// is the path this handler wrote (or previously wrote) to storage.
		stemcellCID := pve.BuildHeavyStemcellCID(storage, qcow2Filename)
		templateNode := deps.Config.StemcellTemplateNode
		if templateNode == "" {
			templateNode = node
		}

		// Per-node in-flight gate (opt-in; limit=0 → unlimited, no gating).
		// Acquire before the first ensureTemplateVM call on templateNode so
		// concurrent create_stemcell calls for the same node are serialised when
		// max_inflight_per_node is set. Replication to other nodes is gated
		// per-node inside replicateStemcellToNodes.
		if deps.Config != nil {
			inflightRelease, inflightErr := deps.Inflight.acquire(ctx, templateNode, deps.Config.MaxInflightPerNodeLimit())
			if inflightErr != nil {
				return nil, cpierrors.Retriable(
					"create_stemcell: in-flight limit exceeded or context cancelled on node %s: %s",
					templateNode, inflightErr.Error())
			}
			defer inflightRelease()
		}

		if !found {
			// Step 11: Upload qcow2 to storage. uploadSourcePath is reused so the
			// tarball case extracts only once.
			if uploadErr := uploadAndReturnCID(ctx, deps, node, storage, imagePath, uploadSourcePath, qcow2Filename, deps.Log(ctx)); uploadErr != nil {
				return nil, uploadErr
			}
		}

		// Step 12: Ensure the per-cluster cache template exists (built fresh, or
		// reused on a dedup hit — both arms converge here identically now that
		// the qcow2 is never reclaimed) and register this director's reference.
		// No post-freeze source deletion: the qcow2 IS the stemcell identity
		// (D10); delete_stemcell owns removing it, at last-ref, for :heavy: only.
		_, _, tmplErr := ensureTemplateAndRegisterRef(ctx, deps, deps.Log(ctx),
			templateNode, storage, qcow2Filename, sha256hex, "",
			pve.StemcellKindHeavy, stemcellCID, reqCtx.DirectorUUID, cp, imagePath)
		if tmplErr != nil {
			return nil, fmt.Errorf("create_stemcell: ensure template: %w", tmplErr)
		}

		// Per-node replication (opt-in, default off; see maybeReplicateTemplate
		// for the gate). Best-effort fire-and-forget: individual node failures
		// are logged as warnings and do not fail create_stemcell.
		uploadStagingDir := ""
		if uploadSourcePath == imagePath {
			uploadStagingDir = deps.Config.StemcellStagingDir
		}
		maybeReplicateTemplate(ctx, deps, templateNode, storage, qcow2Filename, sha256hex,
			uploadSourcePath, uploadStagingDir, stemcellCID, reqCtx.DirectorUUID,
			pve.StemcellKindHeavy, cp, imagePath)

		return stemcellCID, nil
	})
}

// ensureTemplateAndRegisterRef builds/reuses the per-cluster cache template
// for a path-identity stemcell CID and registers directorUUID as a live
// reference, retrying once when the template vanishes out from under the
// registration (ErrStemcellTemplateGone — a concurrent last-ref delete raced
// the lookup). Every create_stemcell return path (fresh build and every dedup
// arm, across all four modes) funnels through this one function so
// registration can never be silently skipped on one code path and not
// another.
//
// Returns the winning template's (vmid, node). A registration failure that
// survives the one retry is a hard error: the director-UUID ref set is the
// sole source of truth for whether this cache template may be destroyed, and
// an understated ref count risks a live template being destroyed under a
// caller that still depends on it.
func ensureTemplateAndRegisterRef(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	templateNode, storage, qcow2Filename, sha256hex string,
	knownSHA8 string,
	kind pve.StemcellKind,
	stemcellCID string,
	directorUUID string,
	cp stemcellCloudProps,
	source string,
) (vmid int64, node string, err error) {
	vmid, node, err = ensureTemplateVM(ctx, deps, templateNode, storage, qcow2Filename, sha256hex, knownSHA8, kind, stemcellCID, directorUUID, cp, source)
	if err != nil {
		return 0, "", err
	}

	regErr := registerStemcellDirectorRef(ctx, deps, logger, node, vmid, directorUUID)
	if errors.Is(regErr, ErrStemcellTemplateGone) {
		logger.Warn("ensureTemplateAndRegisterRef: cache template vanished before ref registration "+
			"(concurrent last-ref delete raced this lookup); rebuilding",
			log.Int64(metadataKeyVMID, vmid),
			log.String("node", node),
		)
		vmid, node, err = ensureTemplateVM(ctx, deps, templateNode, storage, qcow2Filename, sha256hex, knownSHA8, kind, stemcellCID, directorUUID, cp, source)
		if err != nil {
			return 0, "", fmt.Errorf("ensureTemplateAndRegisterRef: rebuild after template-gone: %w", err)
		}
		regErr = registerStemcellDirectorRef(ctx, deps, logger, node, vmid, directorUUID)
	}
	if regErr != nil {
		return 0, "", fmt.Errorf("ensureTemplateAndRegisterRef: register director ref vmid=%d node=%q: %w", vmid, node, regErr)
	}
	return vmid, node, nil
}

// stemcellStorageIsShared classifies storage's shared-ness for the
// replication gate: replication to other cluster nodes is only
// meaningful when storage is node-local — on shared storage the single cache
// template built on templateNode is already reachable from every node.
//
// Reuses the same live-listing + pve.StorageInfo.IsShared() classification as
// needsReplicaCheck (create_vm_disk.go) and dlbStorageIsShared
// (placement_dlb.go) — the one canonical shared/local decision this codebase
// makes, rather than a second ad-hoc reading of the "shared" flag.
//
// Returns (false, false) — unknown — on any classification failure (storage
// not found in the index, nil ClusterStorage, API error). Callers treat
// known=false as "cannot determine; proceed with replication" (fail open,
// preserving pre-existing default-off behavior for operators who have not
// configured stemcell_replicate_local).
func stemcellStorageIsShared(ctx context.Context, deps Deps, storage string) (shared bool, known bool) {
	info, ok := liveStorageInfo(ctx, deps, storage)
	if !ok {
		return false, false
	}
	return info.IsShared(), true
}

// lightStemcellPolicyOpts builds the option set for
// pve.ValidateLightStemcellStorage: the unpinned-local relaxation applies
// exactly when the single-shared-template topology holds (strategy is not
// import, and vm_storage positively classifies as shared), mirroring
// validateStemcellStorageShared's relaxation on the heavy path so every
// create_stemcell mode accepts the same configurations. Fail-closed: unknown
// vm_storage classification keeps rule 5's pin requirement.
func lightStemcellPolicyOpts(ctx context.Context, deps Deps) []pve.LightStemcellPolicyOption {
	if deps.Config == nil || deps.Config.StemcellStrategy == config.StemcellStrategyImport {
		return nil
	}
	if vmShared, known := stemcellStorageIsShared(ctx, deps, deps.Config.VMStorage); known && vmShared {
		return []pve.LightStemcellPolicyOption{pve.WithUnpinnedLocalAccepted()}
	}
	return nil
}

// templateReplicasNeeded reports whether per-node cache-template replicas
// serve any purpose in this configuration: replication is opt-in
// (stemcell_replicate_local) and only meaningful when the TEMPLATE-DISK pool
// — config vm_storage, where attemptCreateTemplateVM places every cache
// template's root disk — is node-local. It deliberately reuses create_vm's
// needsReplicaCheck so the build side and the consume side of the replica
// contract classify the same pool the same way: create_vm demands a per-node
// replica exactly when the template's disk cannot be cloned cross-node, and
// that is a property of vm_storage, not of the stemcell (qcow2) pool. A
// shared qcow2 pool with node-local vm_storage still needs replicas; gating
// on the stemcell pool's shared-ness skipped them in exactly that split
// configuration.
func templateReplicasNeeded(ctx context.Context, deps Deps) bool {
	if deps.Config == nil || !deps.Config.StemcellReplicateLocal {
		return false
	}
	return needsReplicaCheck(ctx, deps, deps.Config.VMStorage)
}

// maybeReplicateTemplate replicates the cache template to the remaining
// cluster nodes when replication is on and the template-disk pool is
// node-local (templateReplicasNeeded). Applies to every kind create_stemcell
// caches, including light stemcells: the operator owns a light qcow2, but the
// cache template — and any per-node replica of it — is CPI-owned, exactly
// like the primary. Best-effort fire-and-forget: individual node failures
// are logged as warnings and never fail create_stemcell. Only runs when
// sha256hex is known (required for replica idempotency checks).
//
// uploadSourcePath may be empty when the qcow2 needs no per-node transfer —
// the light-preuploaded case, where the operator's file lives on a shared
// pool every node mounts; replicateOneNode skips its upload step whenever the
// stemcell pool classifies as shared.
func maybeReplicateTemplate(
	ctx context.Context,
	deps Deps,
	templateNode, storage, qcow2Filename, sha256hex string,
	uploadSourcePath, uploadStagingDir string,
	stemcellCID, directorUUID string,
	kind pve.StemcellKind,
	cp stemcellCloudProps,
	source string,
) {
	if sha256hex == "" {
		return
	}
	if !templateReplicasNeeded(ctx, deps) {
		return
	}
	clusterNodes, listErr := listClusterNodes(ctx, deps)
	if listErr != nil {
		deps.Log(ctx).Warn("create_stemcell: replication: cannot list cluster nodes (skipping replication)",
			log.Err(listErr),
		)
		return
	}
	if len(clusterNodes) <= 1 {
		return
	}
	replicateStemcellToNodes(ctx, deps, templateNode, storage, qcow2Filename,
		sha256hex, clusterNodes, uploadSourcePath, uploadStagingDir,
		stemcellCID, directorUUID, kind, cp, source)
}

// sha8Of returns the first 8 lowercase hex characters of sha256hex, or "" when
// sha256hex has fewer than 8 characters (including the empty string). Unlike
// the historical inline `if len(sha8) > 8 { sha8 = sha8[:8] }` pattern (which
// produced a degenerate "bosh-stemcell-sha-" tag for an unknown digest), an
// empty return here is a deliberate signal to skip sha-tag identity entirely
// and fall back to name-keyed lookup. Server-side download now requires
// cloud_properties.sha256 outright (handleStemcellDownloadURL), so the
// remaining sha8-unknown case is the light-fetch prefix-dedup path when the
// matched existing filename does not carry a parseable sha8 suffix — see
// ensureTemplateVM's knownSHA8 parameter for the case where a genuine sha8 IS
// available despite sha256hex being empty.
func sha8Of(sha256hex string) string {
	if len(sha256hex) < 8 {
		return ""
	}
	return strings.ToLower(sha256hex[:8])
}

// sha256MatchesTemplateProvenance reads ref's PVE config description and
// compares its recorded full sha256 (stemcellProvenance.SHA256) against
// wantSHA256hex. This is the sha8 (32-bit) collision guard: two different
// disk images can share an 8-hex-character tag by chance, so a sha-tag match
// alone is not sufficient proof of identity once dedup is cluster-scoped
// across a wider candidate pool.
//
// Returns:
//   - (true, nil): provenance carries no SHA256 (legacy/unknown template —
//     the sha8 tag match is the only signal available, matching pre-guard
//     dedup behavior) OR provenance SHA256 matches wantSHA256hex exactly
//     (case-insensitive).
//   - (false, nil): provenance SHA256 is recorded and differs — caller must
//     NOT reuse this candidate.
//   - (false, err): the candidate's config could not be read (PVE API
//     failure, including not-found from a stale cluster-resource listing);
//     caller treats an unverifiable candidate as unusable and tries the next.
func sha256MatchesTemplateProvenance(ctx context.Context, deps Deps, ref pve.TemplateRef, wantSHA256hex string) (bool, error) {
	if deps.PVE == nil || deps.PVE.QEMU() == nil {
		return false, fmt.Errorf("sha256MatchesTemplateProvenance: PVE QEMU service unavailable")
	}
	vmidInt := int(ref.VMID) //nolint:gosec // VMID is bounded by PVE valid range (1–999999999)
	cfg, cfgErr := deps.PVE.QEMU().Config(ctx, ref.Node, vmidInt)
	if cfgErr != nil {
		return false, cfgErr
	}
	description := stringConfigField(cfg, pveConfigKeyDescription)
	prov, ok := parseStemcellProvenanceFromDescription(description)
	if !ok || prov.SHA256 == "" {
		return true, nil
	}
	return strings.EqualFold(prov.SHA256, wantSHA256hex), nil
}

// stemcellBackingQCow2Exists reports whether storage, queried via node, still
// lists an "import/<qcow2Filename>" content entry — the file identity the
// returned stemcell CID names. Used to guard the sha-tag dedup hit in
// ensureTemplateVM: a partially-failed delete_stemcell run can destroy the
// qcow2 while a replica or race-orphaned template survives with a matching
// sha tag and provenance, and reusing that template would hand the caller a
// CID resolving to nothing on storage.
//
// Returns (false, err) on any ListStorageContent API failure — the caller
// treats an unverifiable candidate the same as a missing one (skip, try the
// next candidate or fall through to a fresh build).
func stemcellBackingQCow2Exists(ctx context.Context, deps Deps, node, storage, qcow2Filename string) (bool, error) {
	volid, err := pve.FindStemcellByFilename(ctx, deps.PVE, node, storage, qcow2Filename)
	if err != nil {
		return false, err
	}
	return volid != "", nil
}

// ensureTemplateVM builds or reuses a per-cluster cache template VM for a
// path-identity stemcell CID.
//
// Sequence:
//  1. BuildTemplateNameWithSHA(cp.Name, cp.Version, sha8) → deterministic name
//     (idempotency key when sha8 is unknown; sha-tag identity is preferred
//     when known).
//  2. Cluster-scoped dedup (create-side and destroy-side see the
//     same world, unlike the historical per-node ListQemu scan):
//     a. sha8 known: pve.FindTemplatesBySHATagCluster(sha8), tried in
//     ascending-VMID order. Each candidate's full sha256 is verified via
//     sha256MatchesTemplateProvenance before reuse (sha8 collision guard);
//     a mismatch is logged and skipped, not reused.
//     b. sha8 unknown (the light-fetch prefix-dedup fallback, when the
//     matched filename carries no parseable sha8 suffix, is the remaining
//     case — server-side download now requires cloud_properties.sha256):
//     pve.FindTemplateByNameCluster(templateName), lowest VMID (excluding
//     replicas) wins. Name-keyed dedup is intentionally NOT attempted
//     when sha8 IS known — a content-addressed identity must never fall
//     back to a weaker name-only match.
//     Any lookup hit returns immediately without building anything; no
//     source deletion happens on this path regardless of the return CID's
//     kind (D10 — the qcow2 is never reclaimed by create_stemcell).
//  3. Cache miss: allocate a new VMID in [config.StemcellTemplateVMIDRangeStart,
//     …End] via AllocateWithRetry, same retry/conflict pattern as create_vm.
//  4. QEMU().Create with import-from=<storage>:import/<qcow2Filename>; no NIC,
//     no agent, onboot=0; tags include "bosh-stemcell-cache" (cache marker,
//     unconditional) and "bosh-stemcell-sha-<sha8>" (only when sha8 is
//     known); provenance tags/notes are always stamped (no config
//     gate); await UPID with StemcellMaxWait.
//  5. MakeTemplate → freeze VM; await UPID if non-empty.
//  6. Race reconciliation (reconcileTemplateRace, cluster-scoped): if a
//     concurrently-built lower-VMID twin is now visible, delete our duplicate
//     and adopt the survivor's (VMID, node).
//  7. Pool assignment: if deps.Config.StemcellTemplatePool != "", the pool is
//     create-if-missing (pve.EnsurePoolExists) BEFORE step 4 and passed as the
//     qemu-create "pool" param, so our own template is born a pool member (and
//     a token whose only VM.Allocate grant lives on the pool path can create
//     it at all). Only a lost race assigns afterward: the survivor may have
//     been built without a pool configured — closing the gap where that would
//     leave the cache outside the operator's intended pool — via
//     AssignVMToPool. Both calls are fatal on error.
//
// Returns the (VMID, node) of the resulting cache template on success. node
// may differ from templateNode when a cluster-scoped dedup hit or a lost
// race resolves to a template actually hosted elsewhere in the cluster.
//
// Error contract:
//   - FindTemplatesBySHATagCluster / FindTemplateByNameCluster API failure →
//     wrapped error returned.
//   - AllocateWithRetry exhausted → error returned.
//   - QEMU.Create failure → error returned (cleanup attempted inside AllocateWithRetry retry).
//   - MakeTemplate failure → error returned (template not safe to use).
//   - EnsurePoolExists or AssignVMToPool failure (when StemcellTemplatePool != "") →
//     error returned (fatal misconfiguration).
//
//nolint:gocognit // Multi-step cluster-lookup+allocation+freeze+reconcile; phases are load-bearing and cannot be further decomposed without losing clarity.
func ensureTemplateVM(
	ctx context.Context,
	deps Deps,
	templateNode, storage, qcow2Filename, sha256hex string,
	knownSHA8 string,
	kind pve.StemcellKind,
	stemcellCID string,
	creatingDirectorUUID string,
	cp stemcellCloudProps,
	source string,
) (vmid int64, node string, err error) {
	logger := deps.Log(ctx)

	// sha8 is normally derived from the full digest. knownSHA8 is a fallback
	// for callers that know a genuinely content-derived sha8 (e.g. recovered
	// from an existing qcow2's filename) but not the full sha256 that
	// produced it — unlike a synthesized/placeholder sha256hex, this keeps
	// spec.SHA256Hex (and therefore the provenance notes' SHA256 field)
	// honestly empty rather than asserting a digest the caller cannot prove.
	sha8 := sha8Of(sha256hex)
	if sha8 == "" && knownSHA8 != "" {
		sha8 = knownSHA8
	}
	templateName := pve.BuildTemplateNameWithSHA(cp.Name, cp.Version, sha8)

	// Step 2a: cluster-scoped sha-tag dedup, only when content-addressed
	// identity is available.
	if sha8 != "" {
		refs, findErr := pve.FindTemplatesBySHATagCluster(ctx, deps.PVE, sha8)
		if findErr != nil {
			// The lookup classifies its own failures (retry + WrapError inside
			// listClusterQemuTemplates); re-running WrapError here would
			// flatten that classification, so pass the chain through.
			return 0, "", fmt.Errorf("ensureTemplateVM: cluster sha-tag lookup %q: %w", sha8, findErr)
		}
		for _, ref := range refs {
			if ref.IsReplica() {
				// Replicas never hold their own director references (their
				// provenance ref set is a fossil of their creator) — a random
				// VMID allocation can place a replica below the primary the
				// live ref set is actually anchored on. Anchoring here on a
				// replica would register (or later deregister) against the
				// wrong ref set. Skip and keep scanning for the real primary.
				continue
			}
			matches, verifyErr := sha256MatchesTemplateProvenance(ctx, deps, ref, sha256hex)
			if verifyErr != nil {
				logger.Warn("ensureTemplateVM: cannot verify candidate cache template provenance (skipping candidate)",
					log.Int64(metadataKeyVMID, ref.VMID),
					log.String("node", ref.Node),
					log.Err(verifyErr),
				)
				continue
			}
			if !matches {
				logger.Warn("ensureTemplateVM: sha8 tag matched but full sha256 differs (collision guard); not reusing",
					log.String("sha8", sha8),
					log.Int64("candidate_vmid", ref.VMID),
					log.String("candidate_node", ref.Node),
				)
				continue
			}
			exists, existsErr := stemcellBackingQCow2Exists(ctx, deps, templateNode, storage, qcow2Filename)
			if existsErr != nil {
				logger.Warn("ensureTemplateVM: cannot verify backing qcow2 still present on storage (skipping candidate)",
					log.Int64(metadataKeyVMID, ref.VMID),
					log.String("node", ref.Node),
					log.Err(existsErr),
				)
				continue
			}
			if !exists {
				// A partially-failed delete_stemcell run can leave a tagged,
				// provenance-matching template whose backing qcow2 has already
				// been removed from storage — reusing it would hand the caller
				// a CID that resolves to nothing on import. Treat as no dedup
				// hit; a fresh build re-uploads the file.
				logger.Warn("ensureTemplateVM: cache template matched by sha tag but backing qcow2 missing from storage (partial delete?); not reusing",
					log.String("sha8", sha8),
					log.Int64("candidate_vmid", ref.VMID),
					log.String("candidate_node", ref.Node),
					log.String("storage", storage),
					log.String("filename", qcow2Filename),
				)
				continue
			}
			logger.Info("ensureTemplateVM: reusing existing cache template (matched by sha tag, cluster-scoped)",
				log.String("sha8", sha8),
				log.Int64(metadataKeyVMID, ref.VMID),
				log.String("node", ref.Node),
			)
			return ref.VMID, ref.Node, nil
		}
	}

	// Step 2b: cluster-scoped name fallback, ONLY when sha8 is unknown. A
	// content-addressed identity (sha8 known) must never silently degrade to
	// a name-only match — that would let two different disk images sharing a
	// name+version collapse onto one cache template.
	if sha8 == "" {
		refs, findErr := pve.FindTemplateByNameCluster(ctx, deps.PVE, templateName)
		if findErr != nil {
			// See the sha-tag lookup branch above: pve.WrapError restores
			// retriability the inner cluster lookup's own wrap drops.
			return 0, "", fmt.Errorf("ensureTemplateVM: cluster name lookup %q: %w", templateName, pve.WrapError(findErr))
		}
		for _, primary := range refs {
			if primary.IsReplica() {
				// Same anchor-stability rule as the sha-tag branch above: a
				// replica's ref set is a fossil, never the live anchor.
				continue
			}
			logger.Info("ensureTemplateVM: reusing existing cache template (matched by name, cluster-scoped; sha8 unknown)",
				log.String("name", templateName),
				log.Int64(metadataKeyVMID, primary.VMID),
				log.String("node", primary.Node),
			)
			return primary.VMID, primary.Node, nil
		}
	}

	// Step 3-4: Allocate VMID + create VM with import-from.
	shaTag := ""
	if sha8 != "" {
		shaTag = stemcellSHATagPrefix + sha8
	}
	// import-from volid: "<storage>:import/<filename>"
	importVolid := storage + ":import/" + qcow2Filename

	isRetryable := func(e error) bool {
		return pve.IsVMIDConflict(e) || pve.IsStorageLockTimeout(e) || pve.IsTransientTransport(e)
	}

	rangeStart := deps.Config.StemcellTemplateVMIDRangeStart
	rangeEnd := deps.Config.StemcellTemplateVMIDRangeEnd

	spec := templateBuildSpec{
		TemplateName:         templateName,
		ImportVolid:          importVolid,
		ShaTag:               shaTag,
		SHA256Hex:            sha256hex,
		TargetStorage:        deps.Config.VMStorage,
		Kind:                 kind,
		CID:                  stemcellCID,
		CreatingDirectorUUID: creatingDirectorUUID,
	}

	// The configured template pool must exist before the create loop below:
	// attemptCreateTemplateVM passes it as the qemu-create "pool" param, and
	// PVE rejects a create referencing a non-existent pool.
	// pve.EnsurePoolExists creates it with a "managed by bosh-pve-cpi"
	// provenance comment (create_stemcell has no env.bosh, so the director is
	// not derivable — the comment carries no director suffix here, unlike
	// create_vm's per-director comment) and tolerates the pool already
	// existing (idempotent).
	if deps.Config.StemcellTemplatePool != "" {
		if ensureErr := pve.EnsurePoolExists(ctx, deps.PVE, deps.Config.StemcellTemplatePool,
			pve.PoolProvenance("")); ensureErr != nil {
			return 0, "", fmt.Errorf("ensureTemplateVM: ensure pool %q exists: %w",
				deps.Config.StemcellTemplatePool, ensureErr)
		}
	}

	// pve.WithStorageScan(templateNode, deps.Config.VMStorage): the template's
	// disk lands on deps.Config.VMStorage (spec.TargetStorage above — importVolid/
	// storage is only the import SOURCE and is intentionally not scanned here),
	// and deps.Config already reflects the effective per-request config (any
	// pve_node/pve_vm_storage context override applied by
	// Deps.WithRequestOverrides before this handler ran). Scanning it closes the
	// same shared-storage co-mingling gap as create_vm's allocateVM: without it,
	// a template VMID could collide with a VM or template another AZ's cluster
	// already owns on the same shared storage.
	allocatedRaw, allocErr := pve.AllocateWithRetry(ctx, deps.PVE,
		func(candidate int) error {
			return attemptCreateTemplateVM(ctx, deps, logger, templateNode, candidate, spec, cp, source)
		},
		isRetryable,
		0, // use AllocateWithRetry default (3 attempts)
		pve.WithRange(rangeStart, rangeEnd),
		pve.WithStorageScan(templateNode, deps.Config.VMStorage),
	)
	if allocErr != nil {
		return 0, "", fmt.Errorf("ensureTemplateVM: allocate+create template VM: %w", allocErr)
	}
	allocatedVMID := int64(allocatedRaw)

	logger.Info("ensureTemplateVM: template VM created, freezing",
		log.String("name", templateName),
		log.Int64(metadataKeyVMID, allocatedVMID),
	)

	// Step 5: Freeze the VM into a PVE template. qm template renames the base
	// volumes under the storage lock, so both the submit and its task ride
	// the lock-aware retry curve. A retry after a committed-but-dropped
	// attempt would see PVE reject the second freeze, so the closure treats
	// an already-frozen config as success (the goal state is reached).
	freezeErr := pve.RetryOnTransientOrLock(ctx, logger, "create_stemcell.freeze", 0, func() error {
		freezeUPID, mkErr := pve.MakeTemplate(ctx, deps.PVE, templateNode, allocatedVMID)
		if mkErr != nil {
			if templateFrozen(ctx, deps, templateNode, allocatedVMID) {
				return nil
			}
			return mkErr
		}
		if freezeUPID == "" {
			return nil
		}
		awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, templateNode, freezeUPID, logger,
			pve.WithMaxWait(pve.StemcellMaxWait))
		if awaitErr != nil && templateFrozen(ctx, deps, templateNode, allocatedVMID) {
			return nil
		}
		return awaitErr
	})
	if freezeErr != nil {
		cleanupLeakedTemplateVM(ctx, deps, templateNode, allocatedVMID, logger, "freeze")
		return 0, "", fmt.Errorf("ensureTemplateVM: freeze template vmid=%d: %w",
			allocatedVMID, pve.WrapErrorKeepingClass(freezeErr))
	}

	logger.Info("ensureTemplateVM: template frozen",
		log.String("name", templateName),
		log.Int64(metadataKeyVMID, allocatedVMID),
	)

	// Step 6: Race reconciliation. The Step-2 dedup lookup and this freeze are
	// not atomic: a concurrent create_stemcell for the same stemcell can pass its
	// own lookup (seeing no frozen template, because ours was not yet frozen) and
	// create a second template in the gap. Now that our template is frozen — and
	// therefore visible to every scanner — re-scan (cluster-scoped) and converge
	// on the lowest VMID. If an older (lower-VMID) twin exists, we lost the
	// race: delete the template we just created and adopt the survivor. This
	// makes concurrent create_stemcell calls idempotent without cross-process
	// locking.
	winnerVMID := allocatedVMID
	winnerNode := templateNode
	if survivorVMID, survivorNode, recErr := reconcileTemplateRace(ctx, deps, templateName, sha8, sha256hex, templateNode, storage, qcow2Filename); recErr != nil {
		// Non-fatal: a failed re-scan leaves our freshly-frozen template in place.
		// A later create_stemcell will reconcile via the Step-2 lookup.
		logger.Warn("ensureTemplateVM: race reconcile scan failed (non-fatal; keeping new template)",
			log.Int64(metadataKeyVMID, allocatedVMID),
			log.Err(recErr),
		)
	} else if survivorVMID != 0 && survivorVMID < allocatedVMID {
		winnerVMID = survivorVMID
		winnerNode = survivorNode
		logger.Info("ensureTemplateVM: lost create race, deleting duplicate and reusing survivor",
			log.Int64("deleted_vmid", allocatedVMID),
			log.Int64("survivor_vmid", survivorVMID),
			log.String("survivor_node", survivorNode),
		)
		if delErr := deleteTemplateVM(ctx, deps, templateNode, allocatedVMID, logger); delErr != nil {
			logger.Warn("ensureTemplateVM: failed to delete duplicate template after lost race (non-fatal)",
				log.Int64(metadataKeyVMID, allocatedVMID),
				log.Err(delErr),
			)
		}
	}

	// Step 7: Pool assignment for the lost-race arm only. Our own create
	// already placed the template in the configured pool via the qemu-create
	// "pool" param (attemptCreateTemplateVM), so when we won the race there is
	// nothing to assign — and re-adding a pool member via PUT /pools/{poolid}
	// would be rejected by PVE ("already a pool member"). When we LOST the
	// race, the surviving twin's create call may have configured no pool (or a
	// different one) — this caller's pool preference must still apply to
	// whichever template survives, so the survivor is assigned via
	// pve.AssignVMToPool (PUT /pools/{poolid} with vms=[vmid]; VMID-scoped, no
	// node parameter needed, so this is safe for a survivor hosted on a
	// different node). The pool itself was already ensured before the create
	// loop above.
	//
	// An AssignVMToPool failure is fatal: the operator explicitly named a
	// pool; an assign failure indicates misconfiguration that must surface
	// immediately rather than leaving a template silently outside the
	// expected pool.
	if deps.Config.StemcellTemplatePool != "" && winnerVMID != allocatedVMID {
		if poolErr := pve.AssignVMToPool(ctx, deps.PVE, deps.Config.StemcellTemplatePool, winnerVMID); poolErr != nil {
			return 0, "", fmt.Errorf("ensureTemplateVM: assign template vmid=%d to pool %q: %w",
				winnerVMID, deps.Config.StemcellTemplatePool, poolErr)
		}
		logger.Info("ensureTemplateVM: template assigned to pool",
			log.String("pool", deps.Config.StemcellTemplatePool),
			log.Int64(metadataKeyVMID, winnerVMID),
		)
	}

	// No source retention step: the qcow2 backing this cache template is
	// never reclaimed by create_stemcell (D10) — it IS the stemcell identity
	// for :heavy: CIDs, and :light: sources are operator-owned and never
	// touched. delete_stemcell owns the last-ref :heavy: deletion.
	return winnerVMID, winnerNode, nil
}

// reconcileTemplateRace returns the (lowest VMID, node) of a frozen template
// matching the stemcell identity, used after freeze to detect a
// concurrently-created duplicate. Cluster-scoped: prefers the stable
// sha tag (when sha8 is known — the caller's already-resolved value, which
// may come from a full digest or a knownSHA8 fallback) and falls back to the
// deterministic name. A return of (0, "", nil) means no matching template was
// visible — treated by the caller as "no duplicate".
//
// sha256hex is used only for the sha8-collision verification below
// (sha256MatchesTemplateProvenance) and may be empty even when sha8 is known
// (the knownSHA8 case) — an empty sha256hex there compares as "unverifiable
// against a candidate with a recorded digest", which correctly declines to
// adopt rather than fabricating a match.
//
// Sha-tag candidates are verified against the full sha256 recorded in their
// provenance (sha256MatchesTemplateProvenance) before adoption — the sha8 tag
// is a 32-bit truncation, and adopting a colliding template here would clone
// every subsequent VM from the wrong stemcell. The Step-2 dedup lookup applies
// the same full-hash guard; a tag match that fails it is skipped, never
// adopted. The caller's own freshly-frozen template always passes this check,
// so a scan that sees only colliding twins converges on the caller's template.
//
// queryNode, storage, and qcow2Filename back the same missing-backing-file
// guard the Step-2 dedup lookup applies (stemcellBackingQCow2Exists): without
// it, a stale tagged/matching template left behind by a partially-failed
// delete_stemcell run could win the reconcile and get adopted in place of the
// caller's own just-uploaded, just-frozen (and therefore verified-present)
// template — handing the caller a CID pointing at a file that no longer
// exists.
func reconcileTemplateRace(ctx context.Context, deps Deps, templateName, sha8, sha256hex, queryNode, storage, qcow2Filename string) (vmid int64, node string, err error) {
	// Every candidate is checked against the same (queryNode, storage,
	// qcow2Filename) — the identity in question is the caller's own
	// stemcell, not anything specific to a given candidate ref — so this
	// closure takes no per-candidate argument.
	backingExists := func() (bool, error) {
		exists, existsErr := stemcellBackingQCow2Exists(ctx, deps, queryNode, storage, qcow2Filename)
		if existsErr != nil {
			return false, existsErr
		}
		return exists, nil
	}

	if sha8 != "" {
		refs, findErr := pve.FindTemplatesBySHATagCluster(ctx, deps.PVE, sha8)
		if findErr != nil {
			return 0, "", findErr
		}
		for _, ref := range refs {
			if ref.IsReplica() {
				// A replica reconciled here would win purely on a random low
				// VMID and carries no live director ref set — never a valid
				// race winner. The caller's own freshly-frozen template is
				// never a replica (reconcileTemplateRace only runs on the
				// primary build path), so skipping replicas here cannot cause
				// this scan to miss the caller's own template.
				continue
			}
			match, provErr := sha256MatchesTemplateProvenance(ctx, deps, ref, sha256hex)
			if provErr != nil {
				return 0, "", provErr
			}
			if !match {
				continue
			}
			exists, existsErr := backingExists()
			if existsErr != nil || !exists {
				// Unverifiable or confirmed-missing: never adopt. The
				// caller's own template (about to survive by default when no
				// candidate is adopted) was just uploaded and frozen in this
				// same call, so it is always backed by a real file.
				continue
			}
			return ref.VMID, ref.Node, nil
		}
		return 0, "", nil
	}

	refs, findErr := pve.FindTemplateByNameCluster(ctx, deps.PVE, templateName)
	if findErr != nil {
		return 0, "", findErr
	}
	for _, ref := range refs {
		if ref.IsReplica() {
			continue
		}
		exists, existsErr := backingExists()
		if existsErr != nil || !exists {
			continue
		}
		return ref.VMID, ref.Node, nil
	}
	return 0, "", nil
}

// deleteTemplateVM destroys a template VM (purge, and destroy unreferenced
// disks when deps.Config.DestroyUnreferencedDisks is set) and awaits the
// destroy task. A not-found result is treated as success: the VM is already
// gone, which is the desired end state. Used by the race-reconcile path to
// remove a duplicate template after losing a concurrent create, and by the
// freeze-failure cleanup path to reclaim a VM that never became a template.
//
// DestroyUnreferencedDisks follows pve.destroy_unreferenced_disks (default
// false), same rationale as destroyTemplateVM in delete_stemcell.go: on
// storage shared by a second cluster with an overlapping VMID band, forcing
// true here would free the OTHER cluster's VMID-matching volumes. The
// template/leaked VM's own disk is always config-referenced, so the default
// loses nothing for the caller's own cleanup.
func deleteTemplateVM(ctx context.Context, deps Deps, node string, vmid int64, logger *log.Logger) error {
	purge := true
	destroyDisks := deps.Config.DestroyUnreferencedDisks
	vmidStr := strconv.FormatInt(vmid, 10)

	resp, err := deps.PVE.Nodes().DeleteQemu(ctx, node, vmidStr, &sdknodes.DeleteQemuParams{
		Purge:                    &purge,
		DestroyUnreferencedDisks: &destroyDisks,
	})
	if err != nil {
		if pve.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("deleteTemplateVM: delete vmid=%d: %w", vmid, err)
	}

	if resp != nil {
		upid, upidErr := pve.UPIDFromRaw(*resp)
		if upidErr == nil && upid != "" {
			if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, logger,
				pve.WithMaxWait(pve.StemcellMaxWait)); awaitErr != nil {
				return fmt.Errorf("deleteTemplateVM: await destroy vmid=%d upid=%s: %w", vmid, upid, awaitErr)
			}
		}
	}
	return nil
}

// cleanupLeakedTemplateVM best-effort deletes vmid on node after a freeze (or
// freeze-await) failure. An unfrozen VM created by ensureTemplateVM /
// ensureReplicaTemplateVM never carries template=true, so it is invisible to
// every discovery scan (dedup, delete_stemcell's sweep, the orphan prune,
// pve-cid stemcells) — left in place it permanently occupies a VMID in the
// template band and a disk on vm_storage with no automated reclaim path.
// stage names the failing step for the warn log; the original freeze error is
// always what the caller returns, regardless of whether this cleanup succeeds.
// templateFrozen reports whether the guest's authoritative config already
// carries the PVE template flag. Used only to convert a replayed freeze into
// success (the goal state is reached, whoever reached it); any read failure
// reports false so the original freeze error surfaces instead. PVE renders
// the flag as the JSON number 1, but tolerate the literal and string forms
// like pveBool does.
func templateFrozen(ctx context.Context, deps Deps, node string, vmid int64) bool {
	if deps.PVE == nil || deps.PVE.QEMU() == nil {
		return false
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, node, int(vmid))
	if err != nil || cfg == nil {
		return false
	}
	switch v := cfg["template"].(type) {
	case bool:
		return v
	case float64:
		return v == 1
	case string:
		return v == "1" || v == "true"
	default:
		return false
	}
}

func cleanupLeakedTemplateVM(ctx context.Context, deps Deps, node string, vmid int64, logger *log.Logger, stage string) {
	if delErr := deleteTemplateVM(ctx, deps, node, vmid, logger); delErr != nil {
		logger.Warn("ensureTemplateVM: failed to clean up template VM left unfrozen after "+stage+" failure (non-fatal; VM leaked)",
			log.Int64(metadataKeyVMID, vmid),
			log.String("node", node),
			log.Err(delErr),
		)
	}
}

// stemcellCacheTag marks every cache template VM this handler builds,
// independent of the bosh-stemcell-sha-<sha8> content tag (omitted from the
// tag set when sha8 is unknown — see sha8Of) and ownershipTag ("bosh-cpi",
// the generic CPI-managed marker). Templates matching this tag are the
// per-cluster clone-cache-and-refs-anchor set create_vm's template strategy
// and delete_stemcell's sweep both operate over.
const stemcellCacheTag = "bosh-stemcell-cache"

// templateBuildSpec bundles the identity and provenance inputs for
// attemptCreateTemplateVM, keeping the function's parameter list within the
// linter's threshold as the path-identity CID rewrite added several
// provenance-only fields (Kind, CID, CreatingDirectorUUID) to what
// attemptCreateTemplateVM needs to know at create time.
type templateBuildSpec struct {
	// TemplateName is the deterministic PVE VM name (idempotency key).
	TemplateName string
	// ImportVolid is the "<storage>:import/<filename>" source for import-from.
	ImportVolid string
	// ShaTag is the full "bosh-stemcell-sha-<sha8>" PVE tag, or "" when sha8
	// is unknown (server-download without cloud_properties.sha256) — an empty
	// ShaTag means no sha tag is written at all, rather than the historical
	// degenerate "bosh-stemcell-sha-" (empty suffix) tag.
	ShaTag string
	// SHA256Hex is the full lowercase hex digest for the provenance notes
	// JSON's SHA256 field (sha8-collision verification uses this at reuse
	// time); may be empty in the same case ShaTag is empty.
	SHA256Hex string
	// TargetStorage is the PVE storage pool the template's root disk lands
	// on (deps.Config.VMStorage) — distinct from the import-from SOURCE
	// storage, which only needs "import" content type.
	TargetStorage string
	// Kind is the path-identity CID kind (pve.StemcellKindLight or
	// pve.StemcellKindHeavy) this cache template serves.
	Kind pve.StemcellKind
	// CID is the path-identity stemcell CID this cache template serves.
	CID string
	// CreatingDirectorUUID is the BOSH director UUID that triggered this
	// build, recorded as the provenance CreatedBy field and seeded as the
	// template's first DirectorRefs entry.
	CreatingDirectorUUID string
	// ExtraBaseTags holds any additional identity tags (e.g. the per-node
	// replica tag) appended to the base tag set alongside ShaTag/stemcellCacheTag.
	ExtraBaseTags []string
}

// attemptCreateTemplateVM builds CreateQemuParams for a minimal cache
// template VM and calls QEMU().Create + await. Called on each
// AllocateWithRetry attempt. Returns an error on any failure so
// AllocateWithRetry can retry on conflict.
//
// Template VM characteristics (differs from create_vm):
//   - No NIC (net0 absent) — templates carry only the root disk.
//   - No QEMU guest agent — agent=0 (template is frozen; agent not needed).
//   - onboot=0 — templates must not auto-start.
//   - Root disk key virtio0 (default) or scsi0 (pve.root_disk_bus=scsi): import-from=
//     with format=qcow2 and size=5G default.
//   - Tags: ownershipTag, stemcellCacheTag always; "bosh-stemcell-sha-<sha8>"
//     when spec.ShaTag is non-empty; provenance name/version tags always
//     (provenance is unconditional, no config gate).
//
// Provenance notes (buildStemcellProvenanceNotesPath) are always written to
// the description field — the path-identity design has no "disabled" mode;
// the cache template's provenance JSON is both operator documentation and the
// DirectorRefs store registerStemcellDirectorRef/deregisterStemcellDirectorRef
// read and write.
//
// cp and source supply the provenance content. source is the human-readable
// origin label (image_path, image_id, or image_url).
func attemptCreateTemplateVM(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	candidate int,
	spec templateBuildSpec,
	cp stemcellCloudProps,
	source string,
) error {
	// Root disk: allocate the template's root disk on spec.TargetStorage (the
	// VM/images storage), under the same PVE config key create_vm's rootDiskKey
	// resolves to for the current pve.root_disk_bus (virtio0 by default, scsi0
	// when set to "scsi") — a clone of this template inherits whichever key it
	// is created under, and create_vm's clone path verifies the two match
	// before cloning. PVE requires the "<storage>:<size>" form — a bare "0" is
	// parsed as a volume ID and rejected ("unable to parse volume ID '0'").
	// spec.TargetStorage MUST support the "images" content type and is
	// intentionally distinct from the import-from source (spec.ImportVolid
	// lives on StemcellStorage, which only needs "import"); no single PVE
	// storage need support both — mirrors the create_vm import-from path.
	// size=5G matches defaultStemcellDiskGiB; PVE will not shrink below the
	// imported image's actual size.
	rootKey := rootDiskKey(deps.Config)
	rootDiskVal := fmt.Sprintf("%s:0,import-from=%s,format=%s,size=%dG",
		spec.TargetStorage, spec.ImportVolid, diskFormatQCOW2, defaultStemcellDiskGiB)

	// baseTags is the ordered set of identity tags that always appear in the
	// template's tags field. ownershipTag ("bosh-cpi") and stemcellCacheTag
	// are unconditional; the content sha tag is present only when spec.ShaTag
	// is non-empty (sha8 known); extraBaseTags carries e.g. the per-node
	// replica tag.
	baseTags := make([]string, 0, 3+len(spec.ExtraBaseTags))
	baseTags = append(baseTags, ownershipTag, stemcellCacheTag)
	if spec.ShaTag != "" {
		baseTags = append(baseTags, spec.ShaTag)
	}
	baseTags = append(baseTags, spec.ExtraBaseTags...)

	createParams := map[string]any{
		metadataKeyVMID: candidate,
		metadataKeyName: spec.TemplateName,
		"ostype":        osTypeLinux26,
		"scsihw":        "virtio-scsi-pci",
		rootKey:         rootDiskVal,
		"boot":          "order=" + rootKey,
		"agent":         "enabled=0",
		// tablet=0 on the template itself: create_vm's clone patch already
		// forces it on every CPI-created clone, but a template carrying the
		// PVE default (tablet on) would leak it into any clone an operator
		// makes by hand from a CPI-managed template.
		"tablet": 0,
		// balloon=0 on the template itself for the same reason as tablet:
		// create_vm patches every CPI-created clone, but a template carrying
		// PVE's default (balloon device enabled) would leak it into any clone
		// an operator makes by hand from a CPI-managed template.
		pveConfigKeyBalloon: 0,
		"onboot":            0,
	}

	// Create directly into the configured template pool. Passing "pool" at
	// create time (rather than assigning after) lets a token whose only
	// VM.Allocate grant lives on /pool/<stemcell_template_pool> create the
	// template at all: PVE's qemu-create permission check accepts VM.Allocate
	// on the target pool as an alternative to /vms/{vmid}, but only when the
	// create request itself names the pool. The caller ensures the pool
	// exists before the allocation loop — PVE rejects a create referencing a
	// non-existent pool.
	if deps.Config.StemcellTemplatePool != "" {
		createParams["pool"] = deps.Config.StemcellTemplatePool
	}

	// Provenance is unconditional: every cache template gets full
	// notes JSON (name/version/os_type/disk_format/sha8/sha256/kind/cid/
	// created_by/created/director_refs seeded with the creating director) and
	// tags (marker + name + version). Per-director "director--<uuid>" tags
	// are stamped separately, per registration, by registerStemcellDirectorRef
	// — not duplicated here — so buildStemcellProvenanceTags is called with an
	// empty directorID.
	notes, notesErr := buildStemcellProvenanceNotesPath(cp, spec.Kind, spec.CID, spec.SHA256Hex, source,
		spec.CreatingDirectorUUID, time.Now().UTC(), cp.DirectorTags)
	if notesErr != nil {
		// buildStemcellProvenanceNotesPath fails only on json.Marshal of a
		// plain struct — a programming error, never an operator- or
		// PVE-supplied condition. A template built without provenance notes
		// carries no DirectorRefs store, so registerStemcellDirectorRef reads
		// back a fabricated zero-value provenance (see
		// parseStemcellProvenanceFromDescription) with no recorded SHA256,
		// which disables the sha8-collision guard for every future dedup
		// against this template. Unmanageable by design; fail the create
		// rather than produce it.
		return fmt.Errorf("attemptCreateTemplateVM: build provenance notes: %w", notesErr)
	}
	createParams[pveConfigKeyDescription] = notes

	// The identity tags are deliberately NOT passed at create time. PVE's
	// create worker checks tag permissions strictly against /vms/{vmid}
	// (assert_tag_permissions passes no pool to check_vm_perm) and runs that
	// check BEFORE registering the VM in the pool named by the "pool" create
	// param — so a token whose VM.Config.Options grant lives only on the pool
	// path can never create a tagged VM in one call. Creating untagged and
	// writing the tags immediately after (below) closes the gap: by then the
	// VM is a pool member and the pool ACL satisfies the /vms/{vmid} check.
	directorTagTokens := buildDirectorTagTokens(cp.DirectorTags)
	provTags := buildStemcellProvenanceTags(cp, "")
	allTags := mergeTagList(baseTags, provTags, 0)
	templateTags := mergeTagList(strings.Split(allTags, ";"), directorTagTokens, maxTagLength)

	upid, cerr := deps.PVE.QEMU().Create(ctx, node, createParams)
	if cerr != nil {
		switch {
		case pve.IsVMIDConflict(cerr):
			logger.Info("ensureTemplateVM: vmid conflict, retrying",
				log.Int("vmid_attempted", candidate),
			)
		case pve.IsStorageLockTimeout(cerr):
			logger.Info("ensureTemplateVM: storage lock timeout on create, retrying",
				log.Int("vmid_attempted", candidate),
			)
		case pve.IsTransientTransport(cerr):
			logger.Info("ensureTemplateVM: transient transport fault on create, retrying",
				log.Int("vmid_attempted", candidate),
				log.Err(cerr),
			)
		}
		return cerr
	}

	if upid != "" {
		if werr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, logger,
			pve.WithMaxWait(pve.StemcellMaxWait)); werr != nil {
			if pve.IsVMIDConflict(werr) || pve.IsStorageLockTimeout(werr) || pve.IsTransientTransport(werr) {
				logger.Info("ensureTemplateVM: retryable error awaiting create task",
					log.Int("vmid_attempted", candidate),
					log.Err(werr),
				)
			}
			return werr
		}
	}

	// Stamp the identity tags now that the VM exists and is a pool member
	// (see the note above the create call). A failure here leaves a VM whose
	// tags — the exact keys the stemcell lookups, delete_stemcell's sha8
	// sweep, and the orphan prune match on — are absent, so the VM is
	// unmanageable as a cache template: destroy it rather than leak it.
	tagParams := &sdknodes.UpdateQemuConfigParams{Tags: &templateTags}
	if tagErr := deps.PVE.Nodes().UpdateQemuConfig(ctx, node, strconv.Itoa(candidate), tagParams); tagErr != nil {
		cleanupLeakedTemplateVM(ctx, deps, node, int64(candidate), logger, "tag after create")
		return cpierrors.Wrap(pve.WrapError(tagErr),
			fmt.Sprintf("attemptCreateTemplateVM: tag template vmid %d", candidate))
	}

	logger.Info("ensureTemplateVM: VM disk imported",
		log.Int("vmid_attempted", candidate),
		log.String("upid", upid),
	)
	return nil
}

// buildDirectorTagTokens sanitizes directorTags (key/value pairs supplied via
// the CPI v3 env argument) into "key-value" PVE tag tokens. Any token where
// either side sanitizes to "" is dropped. Returns nil for an empty/nil input.
func buildDirectorTagTokens(directorTags map[string]string) []string {
	if len(directorTags) == 0 {
		return nil
	}
	tokens := make([]string, 0, len(directorTags))
	for k, v := range directorTags {
		sk := sanitizeTagValue(k)
		sv := sanitizeTagValue(v)
		if sk == "" || sv == "" {
			continue
		}
		tokens = append(tokens, sk+"-"+sv)
	}
	return tokens
}

// resolveStemcellStorageAndNode resolves the target PVE node and storage for a
// heavy-stemcell upload (steps 4-5 of the eleven-step flow).
//
// node comes from deps.Config.Node (required; empty is a cloud error), then is
// retargeted to one of the storage's owning nodes when the storage carries a
// PVE "nodes" restriction that excludes the configured node — see
// stemcellStorageOwningNode. storage is deps.Config.StemcellStorage with a
// fallback to VMStorage (both empty is a cloud error). After the storage name
// is determined, validateStemcellStorageShared enforces that local-only
// storage is rejected when the cluster has more than one node.
func resolveStemcellStorageAndNode(ctx context.Context, deps Deps) (node, storage string, err error) {
	node = deps.Config.Node
	if node == "" {
		return "", "", cpierrors.Cloud("create_stemcell: config.node must not be empty")
	}

	storage = deps.Config.StemcellStorage
	if storage == "" {
		storage = deps.Config.VMStorage
	}
	if storage == "" {
		return "", "", cpierrors.Cloud("create_stemcell: no stemcell storage configured (stemcell_storage and vm_storage both empty)")
	}

	// Block-only pools (rbd, lvm, lvmthin, zfspool, ...) hold VM disk images
	// and can never receive a qcow2 file upload: PVE limits their content to
	// "Disk image"/"Container". Reaching one here (directly, or through the
	// vm_storage fallback above) fails fast with guidance instead of an
	// opaque PVE upload error. Best-effort: an unlisted/unclassifiable pool
	// falls through and lets the upload itself report the failure.
	if info, ok := liveStorageInfo(ctx, deps, storage); ok && pve.IsBlockStorage(info.Type) {
		hint := ""
		// The node-local-staging hint only holds under strategy=template,
		// where the single cache template clones cross-node; import reads the
		// qcow2 from each VM's own node and gets no such relief.
		if deps.Config.StemcellStrategy != config.StemcellStrategyImport {
			hint = " (a node-local pool works when vm_storage is shared: the single cache template clones to every node)"
		}
		tail := ""
		if deps.Config.VMStorage != "" {
			tail = fmt.Sprintf(" and keep vm_storage on %q for the template and VM disks", deps.Config.VMStorage)
		}
		return "", "", cpierrors.Cloud(
			"create_stemcell: stemcell storage %q (type=%q) is block-only and cannot hold stemcell qcow2 files; "+
				"point stemcell_storage at a file-capable pool (dir/NFS/CIFS/CephFS)%s%s",
			storage, info.Type, hint, tail,
		)
	}

	if validateErr := validateStemcellStorageShared(ctx, deps, storage); validateErr != nil {
		return "", "", validateErr
	}
	node = stemcellStorageOwningNode(ctx, deps, node, storage)
	if tnErr := validateTemplateNodeReachesStaging(ctx, deps, storage, node); tnErr != nil {
		return "", "", tnErr
	}
	return node, storage, nil
}

// validateTemplateNodeReachesStaging rejects a stemcell_template_node that
// cannot see the staged qcow2: with a node-local staging pool the file lands
// on stagingNode only, and attemptCreateTemplateVM's import-from on any other
// node fails opaquely at QEMU create time. Only enforced when replication is
// off (stemcell_replicate_local copies the qcow2 to every node, preserving
// that configuration's pre-existing behavior) and only on a positively
// node-local classification; an unknown or shared pool passes.
func validateTemplateNodeReachesStaging(ctx context.Context, deps Deps, storage, stagingNode string) error {
	tn := deps.Config.StemcellTemplateNode
	if tn == "" || tn == stagingNode || deps.Config.StemcellReplicateLocal {
		return nil
	}
	if shared, known := stemcellStorageIsShared(ctx, deps, storage); known && !shared {
		return cpierrors.Cloud(
			"create_stemcell: stemcell_template_node %q cannot build the cache template: the stemcell qcow2"+
				" stages on node %q's node-local storage %q, which node %q cannot read; unset"+
				" stemcell_template_node, set it to %q, or use a shared stemcell staging pool",
			tn, stagingNode, storage, tn, stagingNode,
		)
	}
	return nil
}

// stemcellStorageOwningNode returns the node that storage-scoped API calls
// derived from this stemcell's storage resolution — the multipart upload, the
// download-url call, and content listings — should be addressed to.
//
// PVE storages may carry a "nodes" restriction (storage.cfg); a
// storage-scoped API path addressed to a node outside that set fails with
// "storage not available on node". When the storage is unrestricted (empty
// Nodes, meaning available on every node) or the configured node is itself an
// owner, the configured node is kept unchanged. Otherwise the call is
// retargeted to the lexicographically first owning node — the same
// deterministic canonical ordering canonicalNodeSet applies to a storage's
// node set — and the retarget is logged at info.
//
// This changes API-path semantics only: the CPI still connects to the same
// configured PVE endpoint, which proxies the request to the node named in the
// path. A per-request host override that changes which node actually executes
// the transfer is a separate, audit-gated item and is deliberately not part of
// this addressing decision.
//
// Classification failure (liveStorageInfo not ok) keeps the configured node —
// fail open, matching every other liveStorageInfo consumer.
func stemcellStorageOwningNode(ctx context.Context, deps Deps, configuredNode, storage string) string {
	info, ok := liveStorageInfo(ctx, deps, storage)
	if !ok || len(info.Nodes) == 0 {
		return configuredNode
	}
	owners := append([]string(nil), info.Nodes...)
	sort.Strings(owners)
	for _, owner := range owners {
		if owner == configuredNode {
			return configuredNode
		}
	}
	deps.Log(ctx).Info("create_stemcell: storage is node-restricted and the configured node is not an owner; addressing storage-scoped calls to an owning node",
		log.String("storage", storage),
		log.String("configured_node", configuredNode),
		log.String("owning_node", owners[0]),
	)
	return owners[0]
}

// stemcellStorageOwnerSet returns the set of nodes allowed to host storage
// per its storage.cfg nodes restriction, or nil when the storage is
// unrestricted or its definition could not be read (fail-open, matching
// stemcellStorageOwningNode). The replication fan-outs use it to skip nodes
// that cannot see the storage at all — every upload or download addressed to
// one would fail and only add noise to the aggregate warning.
func stemcellStorageOwnerSet(ctx context.Context, deps Deps, storage string) map[string]bool {
	info, ok := liveStorageInfo(ctx, deps, storage)
	if !ok || len(info.Nodes) == 0 {
		return nil
	}
	owners := make(map[string]bool, len(info.Nodes))
	for _, n := range info.Nodes {
		owners[n] = true
	}
	return owners
}

// filterReplicaNodesToOwners drops replica candidates a node restriction on
// storage excludes, logging the skipped set once. Returns nodes unchanged
// when the storage is unrestricted.
func filterReplicaNodesToOwners(ctx context.Context, deps Deps, label, storage string, nodes []string) []string {
	owners := stemcellStorageOwnerSet(ctx, deps, storage)
	if owners == nil {
		return nodes
	}
	kept := nodes[:0:0]
	var skipped []string
	for _, n := range nodes {
		if owners[n] {
			kept = append(kept, n)
		} else {
			skipped = append(skipped, n)
		}
	}
	if len(skipped) > 0 {
		deps.Log(ctx).Info(label+": storage is node-restricted; skipping replication to nodes that cannot host it",
			log.String("storage", storage),
			log.String("skipped_nodes", strings.Join(skipped, ",")),
		)
	}
	return kept
}

// stemcellDedupResult bundles the outputs of buildAndDeduplicateStemcellCID
// into a single struct, keeping the return list under the 5-result linter limit.
type stemcellDedupResult struct {
	// QCow2Filename is the deterministic qcow2 filename (encodes name,
	// version, sha8) — the same value whether this is a fresh build (Found
	// false) or a dedup hit (Found true, matched by exactly this filename).
	// Callers compose the returned path-identity CID from
	// (storage, QCow2Filename) directly (pve.BuildHeavyStemcellCID /
	// pve.BuildLightStemcellCID); this struct carries no CID itself since the
	// CID's kind is a caller-level decision (mainline heavy-upload vs.
	// dedup-found-still-heavy — see create_stemcell.go's HandleCreateStemcell
	// doc comment).
	QCow2Filename string
	// Found is true when the volume already exists in PVE storage (dedup path).
	Found bool
	// UploadSourcePath is the local path to the qcow2 to upload (or the existing path on dedup).
	UploadSourcePath string
	// Cleanup releases any temporary resources (e.g. extracted tarball dir). Always non-nil.
	Cleanup func()
	// SHA256Hex is the full SHA-256 hex digest of the resolved disk image. May be empty
	// on hash failure (non-fatal; template tag is skipped in that case).
	SHA256Hex string
}

// buildAndDeduplicateStemcellCID covers steps 6-10 of the eleven-step flow:
// resolve the disk image from the tarball (or pass through a bare image),
// compute SHA-256, build the deterministic qcow2 filename, then check whether
// that volume already exists in PVE storage.
//
// When Found is true the caller must skip the upload step entirely — the
// qcow2 (identified by the SAME QCow2Filename FindStemcellByFilename scans
// for) already exists. When Found is false, UploadSourcePath points at the
// resolved image (already extracted for tarball inputs) so the upload step
// can reuse it without a second extraction pass. SHA256Hex carries the digest
// computed during image resolution so callers never re-read the multi-GiB
// image.
//
// Cleanup releases the staging tmpDir created by resolveStemcellImage (no-op
// for bare-image passthroughs); the caller owns its lifetime and MUST defer it.
//
// imagePath is the director-supplied local path. cp supplies name, version, and
// disk_format. deps.Config.StemcellStagingDir scopes file access when non-empty.
func buildAndDeduplicateStemcellCID(
	ctx context.Context,
	deps Deps,
	node, storage, imagePath string,
	cp stemcellCloudProps,
	logger *log.Logger,
) (stemcellDedupResult, error) {
	fail := func(err error) (stemcellDedupResult, error) {
		return stemcellDedupResult{Cleanup: func() {}}, err
	}

	// Step 6: Resolve disk image (extract from tarball if needed). The
	// returned cleanup is handed back to the caller — see doc comment.
	uploadSourcePath, cleanupExtract, _, extractedSHA, detectErr := resolveStemcellImage(
		imagePath, cp.DiskFormat, deps.Config.StemcellStagingDir, logger)
	if detectErr != nil {
		return fail(cpierrors.Wrap(detectErr, "create_stemcell: resolve image"))
	}

	// Step 7: Obtain SHA-256 of resolved disk image.
	// For tarball inputs resolveStemcellImage computed the SHA via TeeReader
	// during the single extraction pass. For bare images (qcow2 magic, raw
	// passthrough) extractedSHA is empty and a second-pass file read is used.
	sha256hex := extractedSHA
	if sha256hex == "" {
		var hashErr error
		sha256hex, hashErr = sha256FilePath(uploadSourcePath, deps.Config.StemcellStagingDir)
		if hashErr != nil {
			cleanupExtract()
			return fail(cpierrors.Wrap(hashErr, "create_stemcell: compute sha256"))
		}
	}

	// Step 7b: Verify expected digest when supplied in cloud_properties.
	// Heavy tarball path: source is local (non-retriable on mismatch).
	//
	// BOSH sets sha1/sha256 in cloud_properties to the digest of the original
	// stemcell tarball (.tgz), NOT the extracted inner disk image. When the input
	// is a tarball (uploadSourcePath differs from imagePath because an extraction
	// occurred), the expected digest must be compared against imagePath (the tarball)
	// rather than uploadSourcePath (the extracted .img). For bare images
	// (qcow2/raw passthrough where uploadSourcePath == imagePath), no extraction
	// happened and sha256hex of the file itself is correct.
	digestPath := uploadSourcePath
	digestSHA256 := sha256hex
	isTarball := uploadSourcePath != imagePath
	if isTarball {
		// Tarball input: BOSH digest covers the original .tgz.
		// Compute tarball hash separately; keep sha256hex (of the .img) for the
		// dedup CID/tag — that still encodes disk content identity.
		var tarbullErr error
		digestSHA256, tarbullErr = sha256FilePath(imagePath, deps.Config.StemcellStagingDir)
		if tarbullErr != nil {
			// Hash failure for the tarball is non-fatal for the upload path
			// itself (the upload uses the extracted .img, not the tarball).
			// The blank digest is only acceptable when the operator did not
			// pin an expected digest; when cloud_properties.sha256 is set,
			// verifyExpectedDigest below fails closed on the empty value
			// rather than skipping the requested integrity check.
			logger.Warn("create_stemcell: cannot compute tarball sha256 for digest verification",
				log.Err(tarbullErr),
				log.String("tarball", imagePath),
			)
			digestSHA256 = ""
		}
		digestPath = imagePath
	}
	if verifyErr := verifyExpectedDigest(ctx, logger, cp, digestSHA256, digestPath, deps.Config.StemcellStagingDir, stemcellSourceLocal); verifyErr != nil {
		cleanupExtract()
		return fail(verifyErr)
	}

	// Steps 8-9: Build the deterministic qcow2 filename.
	qcow2Filename := pve.BuildStemcellFilename(cp.Name, cp.Version, sha256hex)

	logger.Info("create_stemcell: resolved filename",
		log.String("qcow2", qcow2Filename),
		log.String(jsonKeySHA256, sha256hex),
	)

	// Step 10: Dedup — skip upload if volume already present. existing (when
	// non-empty) is the full volid FindStemcellByFilename matched; its
	// filename component is by construction qcow2Filename (the scan matches
	// on ":import/" + qcow2Filename), so no re-parse is needed.
	existing, findErr := pve.FindStemcellByFilename(ctx, deps.PVE, node, storage, qcow2Filename)
	if findErr != nil {
		cleanupExtract()
		return fail(cpierrors.Wrap(findErr, "create_stemcell: dedup lookup"))
	}
	if existing != "" {
		logger.Info("create_stemcell: stemcell already present, skipping upload",
			log.String("volume", existing),
			log.String("name", cp.Name),
			log.String("version", cp.Version),
		)
		return stemcellDedupResult{
			QCow2Filename:    qcow2Filename,
			Found:            true,
			UploadSourcePath: uploadSourcePath,
			Cleanup:          cleanupExtract,
			SHA256Hex:        sha256hex,
		}, nil
	}

	return stemcellDedupResult{
		QCow2Filename:    qcow2Filename,
		Found:            false,
		UploadSourcePath: uploadSourcePath,
		Cleanup:          cleanupExtract,
		SHA256Hex:        sha256hex,
	}, nil
}

// uploadAndReturnCID covers step 11 of the eleven-step flow: upload the qcow2
// image to PVE storage.
//
// qcow2Filename is the value returned by buildAndDeduplicateStemcellCID
// (QCow2Filename field) when Found was false. imagePath is the
// director-supplied local path and is used to set the upload staging-dir
// scope and to log the source for observability.
//
// The uploadStagingDir passed to uploadStemcellImage is set only when
// uploadSourcePath equals imagePath (bare qcow2 passthrough). When
// resolveStemcellImage extracted the image into a CPI-owned tmpDir the source
// path differs from imagePath and no staging-dir scoping applies.
//
// The function name is kept from its historical form (it no longer returns a
// CID — path-identity CIDs are computed by the caller from
// (storage, qcow2Filename) before upload begins) to minimize unrelated churn
// in this already-large rewrite; a nil error is the sole success signal.
func uploadAndReturnCID(
	ctx context.Context,
	deps Deps,
	node, storage, imagePath, uploadSourcePath, qcow2Filename string,
	logger *log.Logger,
) error {
	// uploadSourcePath was resolved by buildAndDeduplicateStemcellCID; its
	// underlying tmpDir is kept alive by the caller-owned cleanup deferred in
	// HandleCreateStemcell. No second extraction is needed.

	uploadStagingDir := ""
	if uploadSourcePath == imagePath {
		uploadStagingDir = deps.Config.StemcellStagingDir
	}
	if uploadErr := uploadStemcellImage(ctx, deps, node, storage, qcow2Filename, uploadSourcePath, uploadStagingDir); uploadErr != nil {
		return cpierrors.Wrap(uploadErr, "create_stemcell: upload qcow2")
	}
	logger.Info("create_stemcell: qcow2 uploaded",
		log.String("storage", storage),
		log.String("filename", qcow2Filename),
		log.String("source", imagePath),
	)

	// Source of truth for stemcell identity is the qcow2 filename
	// (encodes name/version/sha8) plus state held by the BOSH Director
	// (name, version, cloud_properties on the stemcell record). PVE's
	// content APIs don't accept arbitrary metadata for import volumes
	// (uploads validate file extension; notes are backup-only), so no
	// sidecar or volume-level annotation is written here.
	return nil
}

// isValidSHA256Hex reports whether s is a 64-character hex string (case
// accepted either way — content-identity comparisons elsewhere in this file
// are already case-insensitive via strings.EqualFold; this validator does not
// additionally insist on lowercase input, only on well-formed hex).
func isValidSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// handleLightStemcellPreUploaded resolves a pre-uploaded light stemcell:
// validates the operator-supplied image_id (a bare volid or a ":light:"
// path-identity CID) and cloud_properties.sha256, applies the storage policy,
// confirms the qcow2 is present on PVE, and returns the canonical ":light:"
// CID. The CPI never uploads, deletes, or rewrites the underlying volume; the
// operator owns its lifecycle for as long as any director holds a reference.
func handleLightStemcellPreUploaded(
	ctx context.Context,
	deps Deps,
	cp stemcellCloudProps,
	directorUUID string,
) (any, error) {
	// 0. sha256 is mandatory for preuploaded stemcells: content identity and
	// sha-tag dedup both depend on it, and requiring it here kills the
	// degenerate "bosh-stemcell-sha-" (empty-suffix) tag at the root rather
	// than working around it downstream.
	if !isValidSHA256Hex(cp.ExpectedSHA256) {
		return nil, cpierrors.Cloud(
			"create_stemcell: preuploaded stemcells must declare sha256 so content identity and dedup work "+
				"(cloud_properties.sha256 must be a 64-character hex string, got %q)", cp.ExpectedSHA256)
	}
	sha256hex := strings.ToLower(cp.ExpectedSHA256)

	// 1. Parse image_id. Accepted forms: a bare volid "<storage>:import/<file>",
	// or a ":light:"-prefixed path-identity CID. A ":heavy:" CID is rejected —
	// that kind asserts CPI ownership, which an operator-supplied preuploaded
	// image contradicts by definition.
	imageID := cp.ImageID
	var storage, volumePath string
	if pve.IsStemcellPathCID(imageID) {
		kind, s, vp, parseErr := pve.ParseStemcellPathCID(imageID)
		if parseErr != nil {
			return nil, cpierrors.Cloud(
				"create_stemcell: cloud_properties.image_id %q is not a valid path-identity CID: %s",
				imageID, parseErr.Error())
		}
		if kind != pve.StemcellKindLight {
			return nil, cpierrors.Cloud(
				"create_stemcell: cloud_properties.image_id %q has kind %q; preuploaded stemcells must use "+
					"a bare volid or a \":light:\" CID — \":heavy:\" identifies a CPI-owned image",
				imageID, kind)
		}
		storage, volumePath = s, vp
	} else {
		s, vp, parseErr := pve.ParseStemcellCID(imageID)
		if parseErr != nil {
			return nil, cpierrors.Cloud(
				"create_stemcell: cloud_properties.image_id %q is not a valid storage volid (expected \"<storage>:import/<file>\"): %s",
				imageID, parseErr.Error())
		}
		storage, volumePath = s, vp
	}

	// 2. Apply storage policy via ValidateLightStemcellStorage.
	policyDeps := newHandlerPolicyDeps(deps)
	chosenNode, policyErr := pve.ValidateLightStemcellStorage(ctx, policyDeps, storage, cp.Node, lightStemcellPolicyOpts(ctx, deps)...)
	if policyErr != nil {
		return nil, policyErr
	}

	// 3. Resolve the node to query for existence. chosenNode wins when non-empty;
	// fall back to config.Node (existing handler invariant: non-empty for normal path).
	node := chosenNode
	if node == "" && deps.Config != nil {
		node = deps.Config.Node
	}
	if node == "" {
		return nil, cpierrors.Cloud("create_stemcell: config.node must not be empty")
	}
	// When the storage carries a nodes restriction excluding the resolved
	// node, address the existence check to an owning node — otherwise the
	// lookup below reports "not found" for a qcow2 the operator DID upload,
	// just on a storage the resolved node cannot see. Same retarget the
	// heavy path applies via resolveStemcellStorageAndNode.
	if chosenNode == "" {
		node = stemcellStorageOwningNode(ctx, deps, node, storage)
	}

	// 4. Existence check — qcow2 filename is the trailing segment after "import/".
	// volumePath has the form "import/<file>" per the CID grammar's contract.
	qcow2Filename := strings.TrimPrefix(volumePath, "import/")
	if qcow2Filename == volumePath || qcow2Filename == "" {
		return nil, cpierrors.Cloud(
			"create_stemcell: cloud_properties.image_id volume path %q is not \"import/<file>\"", volumePath)
	}

	existing, findErr := pve.FindStemcellByFilename(ctx, deps.PVE, node, storage, qcow2Filename)
	if findErr != nil {
		return nil, cpierrors.Wrap(findErr,
			fmt.Sprintf("create_stemcell: light pre-uploaded existence check (storage=%q file=%q)", storage, qcow2Filename))
	}
	if existing == "" {
		return nil, cpierrors.Cloud(
			"create_stemcell: light stemcell image_id %q not found on storage %q node %q — "+
				"operator must upload via pvesm or PVE Upload API before referencing"+
				" (if the file lives on another node's copy of a node-local pool,"+
				" set cloud_properties.node to point the lookup at that node)",
			imageID, storage, node)
	}

	// 5. Ensure the per-cluster cache template and register this director's
	// reference. light-preuploaded: the operator owns this qcow2 — it is
	// never uploaded, reclaimed, or deleted by the CPI (D10).
	templateNode := node
	if deps.Config != nil && deps.Config.StemcellTemplateNode != "" {
		if tnErr := validateTemplateNodeReachesStaging(ctx, deps, storage, node); tnErr != nil {
			return nil, tnErr
		}
		templateNode = deps.Config.StemcellTemplateNode
	}
	stemcellCID := pve.BuildLightStemcellCID(storage, qcow2Filename)
	vmid, winnerNode, tmplErr := ensureTemplateAndRegisterRef(ctx, deps, deps.Log(ctx),
		templateNode, storage, qcow2Filename, sha256hex, "",
		pve.StemcellKindLight, stemcellCID, directorUUID, cp, cp.ImageID)
	if tmplErr != nil {
		return nil, fmt.Errorf("create_stemcell: light pre-uploaded: ensure template: %w", tmplErr)
	}

	deps.Log(ctx).Info("create_stemcell: light stemcell (pre-uploaded) template ready",
		log.String("image_id", imageID),
		log.String("storage", storage),
		log.String("node", node),
		log.Int64(metadataKeyVMID, vmid),
		log.String("template_node", winnerNode),
		log.String("cid", stemcellCID),
	)

	// Per-node replication (opt-in, default off; gate in
	// maybeReplicateTemplate). The operator owns the light qcow2, but the
	// cache template — and any per-node replica of it — is CPI-owned, and on
	// a multi-node cluster whose vm_storage is node-local the replicas are
	// what lets create_vm clone on every node. No upload source: the light
	// policy already requires the qcow2's pool to be reachable where
	// templates build, and replicateOneNode skips the per-node upload when
	// that pool classifies as shared.
	maybeReplicateTemplate(ctx, deps, winnerNode, storage, qcow2Filename, sha256hex,
		"", "", stemcellCID, directorUUID, pve.StemcellKindLight, cp, cp.ImageID)

	return stemcellCID, nil
}

// resolveFetchSource returns the source and reference for rawURL. When
// deps.FetchResolver is non-nil (tests), it replaces the default
// stemcellfetch.ResolveSourceWith call. The production path calls
// stemcellfetch.ResolveSourceWith directly so operator-tunable transport
// timeouts (jobs/pve_cpi/spec stemcell_fetch_*_timeout_sec) reach the https
// and bosh+blobstore sources.
func resolveFetchSource(deps Deps, rawURL string) (stemcellfetch.Source, stemcellfetch.Reference, error) {
	if deps.FetchResolver != nil {
		return deps.FetchResolver(rawURL)
	}
	tc := stemcellfetch.TransportConfig{
		DialTimeout:           time.Duration(deps.Config.StemcellFetchDialTimeoutSec) * time.Second,
		TLSHandshakeTimeout:   time.Duration(deps.Config.StemcellFetchTLSHandshakeTimeoutSec) * time.Second,
		ResponseHeaderTimeout: time.Duration(deps.Config.StemcellFetchResponseHeaderTimeoutSec) * time.Second,
		IdleConnTimeout:       time.Duration(deps.Config.StemcellFetchIdleConnTimeoutSec) * time.Second,
		BlockPrivateNetworks:  deps.Config.StemcellFetchBlockPrivateNetworks,
	}
	return stemcellfetch.ResolveSourceWith(rawURL, tc)
}

// serverSideFetchEligible reports whether an image_url stemcell can ride the
// PVE server-side download-url path instead of the CPI-side fetch+upload:
//
//   - a verified content identity must exist up front
//     (cloud_properties.sha256) because PVE, not the CPI, streams the bytes
//     and the download-url path derives filename, CID, and dedup identity
//     from that digest;
//   - the URL must be plain https:// — PVE's download-url endpoint speaks no
//     s3://, oci://, or bosh+blobstore: and attaches no auth headers;
//   - credential resolution (per-stemcell image_url_auth, then the config's
//     FetchCredentialDefaults longest-prefix match) must come up empty, or
//     the server-side fetch would silently drop the credentials the CPI-side
//     fetch would have sent;
//   - the operator must not have pinned a node (cloud_properties.node): the
//     server-side path resolves its own node from config and storage
//     ownership and would silently ignore the pin the CPI-side fetch honors
//     through ValidateLightStemcellStorage.
//
// A malformed auth payload also returns false: the CPI-side fetch surfaces
// that error with its full context instead.
func serverSideFetchEligible(deps Deps, cp stemcellCloudProps) bool {
	if !isValidSHA256Hex(cp.ExpectedSHA256) {
		return false
	}
	if cp.Node != "" {
		return false
	}
	if !strings.HasPrefix(strings.ToLower(cp.ImageURL), "https://") {
		return false
	}
	var defaults []config.FetchCredentialDefault
	if deps.Config != nil {
		defaults = deps.Config.FetchCredentialDefaults
	}
	creds, err := stemcellfetch.ResolveCredentials(cp.ImageURLAuth, defaults, cp.ImageURL)
	if err != nil {
		return false
	}
	return creds.Kind() == "none"
}

// handleLightStemcellFetch fetches a remote stemcell qcow2, uploads it to PVE
// storage, and returns the canonical light: CID. Entered when cloud_properties
// sets image_url.
//
// Flow:
//  1. Resolve source + credentials.
//  2. Apply storage policy (block-storage rejection, multi-node local pin).
//  3. Best-effort prefix dedup: scan storage for any file matching name+version
//     prefix before fetching (avoids a redundant network round-trip when the
//     same name+version was fetched before, regardless of sha8 match).
//  4. Stream remote body through io.TeeReader into a local temp file while
//     computing SHA-256 in one pass.
//  5. Build canonical filename from sha256; exact dedup check.
//  6. Upload temp file via existing uploadStemcellImage (retry-on-lock, UPID await).
//  7. Return the ":heavy:" CID (the CPI transferred the bytes — image_url
//     fetch is always CPI-owned, regardless of whether a dedup hit skipped
//     the actual download; the historical "light:" naming for this mode is
//     retired).
//
// Temp file is cleaned up on both success and failure.
func handleLightStemcellFetch(
	ctx context.Context,
	deps Deps,
	cp stemcellCloudProps,
	directorUUID string,
) (any, error) {
	// 1. Resolve source + credentials.
	src, ref, resolveErr := resolveFetchSource(deps, cp.ImageURL)
	if resolveErr != nil {
		return nil, cpierrors.Cloud("create_stemcell: resolve source for %q: %s", cp.ImageURL, resolveErr.Error())
	}

	// Credentials: cloud_properties.image_url_auth overrides config defaults;
	// longest-prefix match within FetchCredentialDefaults applies otherwise.
	creds, credErr := stemcellfetch.ResolveCredentials(cp.ImageURLAuth, deps.Config.FetchCredentialDefaults, cp.ImageURL)
	if credErr != nil {
		return nil, cpierrors.Cloud("create_stemcell: resolve credentials: %s", credErr.Error())
	}
	if creds.Kind() == "none" {
		deps.Log(ctx).Warn("create_stemcell: fetching stemcell without credentials",
			log.URL("image_url", cp.ImageURL),
		)
	}

	// 2. Resolve storage: StemcellStorage falls back to VMStorage.
	// Config.ApplyDefaults already applies this chain; guard here for callers
	// that bypass ApplyDefaults (e.g. minimal test configs).
	storage := deps.Config.StemcellStorage
	if storage == "" {
		storage = deps.Config.VMStorage
	}
	if storage == "" {
		return nil, cpierrors.Cloud("create_stemcell: no stemcell storage configured (stemcell_storage and vm_storage both empty)")
	}

	// Apply storage policy (block-type check, multi-node local-pin enforcement).
	policyDeps := newHandlerPolicyDeps(deps)
	chosenNode, policyErr := pve.ValidateLightStemcellStorage(ctx, policyDeps, storage, cp.Node, lightStemcellPolicyOpts(ctx, deps)...)
	if policyErr != nil {
		return nil, policyErr
	}
	node := chosenNode
	if node == "" && deps.Config != nil {
		node = deps.Config.Node
	}
	if node == "" {
		return nil, cpierrors.Cloud("create_stemcell: config.node must not be empty")
	}
	// Address storage-scoped calls (dedup scans, upload, and the server-side
	// download attempt below) to a node that actually owns the storage when it
	// carries a nodes restriction — same retarget the heavy path applies via
	// resolveStemcellStorageAndNode. An explicit cloud_properties.node pin
	// (chosenNode != "") outranks the restriction, matching the pre-uploaded
	// path: the operator's pin is never silently overridden.
	if chosenNode == "" {
		node = stemcellStorageOwningNode(ctx, deps, node, storage)
	}

	fetchTemplateNode := node
	if deps.Config != nil && deps.Config.StemcellTemplateNode != "" {
		if tnErr := validateTemplateNodeReachesStaging(ctx, deps, storage, node); tnErr != nil {
			return nil, tnErr
		}
		fetchTemplateNode = deps.Config.StemcellTemplateNode
	}

	// 3. Pre-fetch prefix dedup: scan storage for any import volume anchored on
	// the "bosh-stemcell-<name>-<version>-" prefix immediately after "import/"
	// with an exact "-<8hex>.qcow2" tail (fetchFindByPrefix; a
	// mere strings.Contains scan could false-positive on a different
	// stemcell's filename that happens to embed ours as a substring). We
	// don't know sha8 yet, so this is best-effort — it only fires when a
	// prior fetch for the same name+version already landed (regardless of
	// sha8). On a hit we skip the network fetch and build/reuse the template
	// from the existing qcow2. fetchFindByPrefix anchors on storage itself,
	// so any match it returns is guaranteed to belong to the target storage.
	prefix := stemcellfetch.FilenamePrefixForDedup(cp.Name, cp.Version)
	if existingVol, prefixErr := fetchFindByPrefix(ctx, deps, node, storage, prefix); prefixErr == nil && existingVol != "" {
		extractedName := fetchExtractFilename(existingVol)
		if extractedName != "" {
			deps.Log(ctx).Info("create_stemcell: light fetch — existing stemcell found by prefix, building template",
				log.String("volid", existingVol),
			)
			// sha256hex unknown (prefix-dedup, no download); the CPI still
			// uploaded these bytes on some prior fetch call — always :heavy:.
			// extractedName still carries the real sha8 from the original
			// upload's filename (BuildFetchedFilename baked it in at that
			// time), even though this call never re-read the bytes to confirm
			// the full digest — thread it through as knownSHA8 so the template
			// gets a correct "bosh-stemcell-sha-<sha8>" tag instead of no sha
			// tag at all. Without this, delete_stemcell's sha8-derived-from-CID
			// lookup (and create_vm's cache lookup) can never find this
			// template by tag, and a later exact-sha256 dedup hit for the same
			// content builds a second, differently-anchored template.
			prefixSHA8, prefixSHA8OK := extractSHA8FromFilename(extractedName)
			if !prefixSHA8OK {
				prefixSHA8 = ""
			}
			prefixCID := pve.BuildHeavyStemcellCID(storage, extractedName)
			prefixVMID, prefixWinnerNode, prefixTmplErr := ensureTemplateAndRegisterRef(ctx, deps, deps.Log(ctx),
				fetchTemplateNode, storage, extractedName, "", prefixSHA8,
				pve.StemcellKindHeavy, prefixCID, directorUUID, cp, cp.ImageURL)
			if prefixTmplErr != nil {
				return nil, fmt.Errorf("create_stemcell: light fetch prefix-dedup: ensure template: %w", prefixTmplErr)
			}
			deps.Log(ctx).Info("create_stemcell: light fetch (prefix-dedup) template ready",
				log.Int64(metadataKeyVMID, prefixVMID),
				log.String("template_node", prefixWinnerNode),
				log.String("cid", prefixCID),
			)
			return prefixCID, nil
		}
	}

	// 4. Fetch source body → local temp file + SHA-256 in flight.
	body, contentLength, fetchErr := src.Fetch(ctx, ref, creds)
	if fetchErr != nil {
		// WrapErrorKeepingClass: a transient fault against the image host (a
		// dropped connection, a timeout, a 5xx) stays retriable; a permanent
		// verdict (404, bad URL) stays permanent. A truncation twelve lines
		// below was already classified retriable while this was not.
		return nil, cpierrors.Wrap(pve.WrapErrorKeepingClass(fetchErr),
			fmt.Sprintf("create_stemcell: fetch %q", cp.ImageURL))
	}
	defer func() { _ = body.Close() }()

	tmpFile, tmpErr := os.CreateTemp("", "bosh-stemcell-fetch-*.qcow2")
	if tmpErr != nil {
		return nil, cpierrors.Wrap(tmpErr, "create_stemcell: create temp file for fetch staging")
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}()

	h := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmpFile, h), body)
	if copyErr != nil {
		// WrapErrorKeepingClass: a mid-stream drop (EOF, reset, timeout) is
		// the same transient class as the truncation check below and must not
		// surface permanent.
		return nil, cpierrors.Wrap(pve.WrapErrorKeepingClass(copyErr),
			"create_stemcell: stream fetched body to temp file")
	}
	sha256hex := hex.EncodeToString(h.Sum(nil))

	// Detect truncated download: when the server advertised a Content-Length and
	// fewer bytes arrived (body closed early without error), the resulting file is
	// corrupt. No expected digest means this would otherwise be silently accepted.
	// Network sources are retriable — a transient connection drop or proxy
	// truncation may clear on retry.
	if contentLength > 0 && written != contentLength {
		return nil, cpierrors.Retriable(
			"create_stemcell: light fetch truncated: expected %d bytes, got %d (retriable)",
			contentLength, written,
		)
	}

	// Sync to disk before uploadStemcellImage reopens the file for upload.
	if syncErr := tmpFile.Sync(); syncErr != nil {
		return nil, cpierrors.Wrap(syncErr, "create_stemcell: sync fetch temp file")
	}

	deps.Log(ctx).Info("create_stemcell: light fetch streamed to temp file",
		log.Int64("bytes_written", written),
		log.Int64("content_length", contentLength),
		log.String(jsonKeySHA256, sha256hex),
	)

	// Verify expected digest when supplied in cloud_properties.
	// Light-fetch path: source is network (retriable on mismatch).
	if verifyErr := verifyExpectedDigest(ctx, deps.Log(ctx), cp, sha256hex, tmpPath, "", stemcellSourceNetwork); verifyErr != nil {
		return nil, verifyErr
	}

	// 5. Build canonical filename + exact dedup check.
	qcow2Filename := stemcellfetch.BuildFetchedFilename(cp.Name, cp.Version, sha256hex)
	stemcellCID := pve.BuildHeavyStemcellCID(storage, qcow2Filename)
	existingVol, findErr := pve.FindStemcellByFilename(ctx, deps.PVE, node, storage, qcow2Filename)
	if findErr != nil {
		return nil, cpierrors.Wrap(findErr, "create_stemcell: light fetch dedup lookup")
	}

	if existingVol != "" {
		deps.Log(ctx).Info("create_stemcell: light fetch — SHA-matched existing stemcell, building template",
			log.String("volid", existingVol),
		)
		// SHA-dedup: qcow2 already on storage from a prior fetch. Build/reuse
		// the cache template and register this director's reference; the
		// source is never reclaimed (D10).
		dedupVMID, dedupWinnerNode, dedupTmplErr := ensureTemplateAndRegisterRef(ctx, deps, deps.Log(ctx),
			fetchTemplateNode, storage, qcow2Filename, sha256hex, "",
			pve.StemcellKindHeavy, stemcellCID, directorUUID, cp, cp.ImageURL)
		if dedupTmplErr != nil {
			return nil, fmt.Errorf("create_stemcell: light fetch SHA-dedup: ensure template: %w", dedupTmplErr)
		}
		deps.Log(ctx).Info("create_stemcell: light fetch (SHA-dedup) template ready",
			log.Int64(metadataKeyVMID, dedupVMID),
			log.String("template_node", dedupWinnerNode),
			log.String("cid", stemcellCID),
		)
		return stemcellCID, nil
	}

	// 6. Upload temp file under the final canonical filename. uploadStemcellImage
	// handles retry-on-lock and UPID await; it reopens tmpPath each attempt so
	// the PVE reader always sees a fresh stream from the beginning of the file.
	// tmpPath is a CPI-owned temp file (os.CreateTemp); not director-supplied.
	// stagingDir scoping is not applicable here — pass "" to use direct os.Open.
	if uploadErr := uploadStemcellImage(ctx, deps, node, storage, qcow2Filename, tmpPath, ""); uploadErr != nil {
		return nil, cpierrors.Wrap(uploadErr, "create_stemcell: light fetch upload")
	}

	// 7. Ensure the cache template from the freshly uploaded qcow2 and
	// register this director's reference. No post-freeze source deletion
	// (D10) — the qcow2 IS the stemcell identity for this :heavy: CID.
	fetchVMID, fetchWinnerNode, fetchTmplErr := ensureTemplateAndRegisterRef(ctx, deps, deps.Log(ctx),
		fetchTemplateNode, storage, qcow2Filename, sha256hex, "",
		pve.StemcellKindHeavy, stemcellCID, directorUUID, cp, cp.ImageURL)
	if fetchTmplErr != nil {
		return nil, fmt.Errorf("create_stemcell: light fetch: ensure template: %w", fetchTmplErr)
	}

	deps.Log(ctx).Info("create_stemcell: light stemcell (fetched) template ready",
		log.URL("image_url", cp.ImageURL),
		log.String("source_scheme", ref.Scheme),
		log.String("creds_kind", creds.Kind()),
		log.Int64(metadataKeyVMID, fetchVMID),
		log.String("template_node", fetchWinnerNode),
		log.String("cid", stemcellCID),
		log.Int64("bytes", written),
	)

	// Per-node replication (opt-in, default off; gate in
	// maybeReplicateTemplate) — tmpPath is still valid here (its deferred
	// removal runs on function return, after replicateStemcellToNodes's own
	// wg.Wait() completes), so every other cluster node gets its own upload
	// from the same bytes this call already fetched — without this, an
	// image_url stemcell on node-local storage would be usable only on
	// fetchTemplateNode. Kind heavy: the CPI uploaded these bytes, matching
	// the kind the fetch path's own ensureTemplateAndRegisterRef stamps.
	maybeReplicateTemplate(ctx, deps, fetchTemplateNode, storage, qcow2Filename,
		sha256hex, tmpPath, "", stemcellCID, directorUUID,
		pve.StemcellKindHeavy, cp, cp.ImageURL)

	return stemcellCID, nil
}

// isSHA8QCow2Tail reports whether tail is exactly 8 hex characters followed
// by ".qcow2" — the sha8+extension suffix pve.BuildStemcellFilename appends
// after the "bosh-stemcell-<name>-<version>-" prefix. A tail with extra
// characters before ".qcow2" (a longer name/version that happens to start
// with ours) or a non-hex sha8 is rejected — the anchoring half of the bug
// S10 fix (fetchFindByPrefix requires BOTH the prefix anchor and this exact
// tail shape, not a bare substring match).
func isSHA8QCow2Tail(tail string) bool {
	const suffix = ".qcow2"
	if !strings.HasSuffix(tail, suffix) {
		return false
	}
	sha8 := tail[:len(tail)-len(suffix)]
	if len(sha8) != 8 {
		return false
	}
	for i := 0; i < len(sha8); i++ {
		c := sha8[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// fetchFindByPrefix scans the named storage for an import volume anchored on
// prefix immediately after "<storage>:import/" with a tail matching
// isSHA8QCow2Tail. Used by handleLightStemcellFetch for the pre-fetch dedup
// check before SHA-256 is known.
//
// Anchoring on the full "<storage>:import/" + prefix — rather than a bare
// strings.Contains scan for ":import/<prefix>" anywhere in the volid (bug
// S10) — rejects a false-positive match against a DIFFERENT stemcell whose
// name/version happens to contain ours as a substring (e.g. "ubuntu-jammy"
// matching a stored "ubuntu-jammy-go_agent" filename) and guarantees any
// match belongs to the target storage, so callers no longer need a separate
// storage-prefix guard on the result.
//
// Returns ("", nil) when no match is found. Returns ("", err) only on PVE API
// failure.
func fetchFindByPrefix(ctx context.Context, deps Deps, node, storage, prefix string) (string, error) {
	if deps.PVE == nil || deps.PVE.Nodes() == nil {
		return "", fmt.Errorf("fetchFindByPrefix: nodes service unavailable")
	}
	content := pveStorageContentImport
	resp, err := deps.PVE.Nodes().ListStorageContent(ctx, node, storage, &sdknodes.ListStorageContentParams{
		Content: &content,
	})
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", nil
	}
	wantPrefix := storage + ":import/" + prefix
	for _, raw := range *resp {
		var item struct {
			VolID string `json:"volid"`
		}
		if jerr := json.Unmarshal(raw, &item); jerr != nil {
			continue
		}
		if !strings.HasPrefix(item.VolID, wantPrefix) {
			continue
		}
		tail := item.VolID[len(wantPrefix):]
		if !isSHA8QCow2Tail(tail) {
			continue
		}
		return item.VolID, nil
	}
	return "", nil
}

// fetchExtractFilename returns the filename component from a PVE volid of the
// form "<storage>:import/<filename>". Returns empty string when the volid does
// not match the expected pattern.
func fetchExtractFilename(volid string) string {
	idx := strings.IndexByte(volid, ':')
	if idx < 0 || idx == len(volid)-1 {
		return ""
	}
	path := volid[idx+1:]
	const pfx = "import/"
	if !strings.HasPrefix(path, pfx) {
		return ""
	}
	name := path[len(pfx):]
	if name == "" {
		return ""
	}
	return name
}

// handlerPolicyDeps adapts handlers.Deps to pve.PolicyDeps for storage policy
// validation. It exposes StorageInfo by calling ClusterStorage().ListStorage
// directly — same underlying call as StorageInfoCache.refresh, kept here to
// avoid changing the Deps surface for this handler only.
// ClusterNodeCount delegates to the existing clusterNodeCount helper.
type handlerPolicyDeps struct {
	deps Deps
}

// newHandlerPolicyDeps constructs the adapter. Exported as a tiny function so
// tests can substitute an alternative PolicyDeps implementation by building
// handleLightStemcellPreUploaded with a seam (see create_stemcell_wb_test.go).
func newHandlerPolicyDeps(deps Deps) pve.PolicyDeps {
	return &handlerPolicyDeps{deps: deps}
}

// StorageInfo lists cluster storage and returns the named entry's classification.
// Returns an error when ClusterStorage() is nil (mock tests that don't wire it)
// so the policy call fails clearly rather than panicking.
func (h *handlerPolicyDeps) StorageInfo(ctx context.Context, name string) (pve.StorageInfo, error) {
	if h.deps.PVE == nil || h.deps.PVE.ClusterStorage() == nil {
		return pve.StorageInfo{}, fmt.Errorf(
			"handlerPolicyDeps: ClusterStorage unavailable — wire deps.PVE.ClusterStorage or use a custom PolicyDeps in tests")
	}
	resp, err := h.deps.PVE.ClusterStorage().ListStorage(ctx, &sdkclusterstorage.ListStorageParams{})
	if err != nil {
		return pve.StorageInfo{}, pve.WrapError(err)
	}
	if resp == nil {
		return pve.StorageInfo{}, fmt.Errorf("handlerPolicyDeps: nil response from cluster storage list")
	}

	// Decode through pve.ParseStorageEntry — the SAME decoder
	// StorageInfoCache.refresh uses — rather than an inline field mapping. The
	// inline version this replaces populated only Name/Type/Shared/Nodes,
	// leaving the backing-identity fields (Path/Server/Export) empty, so
	// BackingKey() was "" for every entry the stemcell path produced: backing
	// identity was unusable here, and two unrelated storages both keyed ""
	// would compare equal to any consumer that did not special-case the empty
	// key.
	//
	// The whole index is decoded (not just the requested name) because the
	// duplicate-backing warning below needs every entry to be meaningful.
	all := make([]pve.StorageInfo, 0, len(*resp))
	for _, raw := range *resp {
		info, perr := pve.ParseStorageEntry(raw)
		if perr != nil {
			continue
		}
		all = append(all, info)
	}

	// `bosh upload-stemcell` before any deploy runs create_stemcell without
	// ever touching StorageInfoCache, so this is the only place the
	// duplicate-backing warning can fire on a stemcell-only workload. The
	// process-wide gate inside WarnDuplicateBackingStoragesOnce keeps it to a
	// single firing and prevents a double warn when a later deploy fills the
	// cache in the same process.
	pve.WarnDuplicateBackingStoragesOnce(ctx, all)

	for i := range all {
		if all[i].Name == name {
			return all[i], nil
		}
	}
	return pve.StorageInfo{}, fmt.Errorf("handlerPolicyDeps: storage %q not found in cluster storage list", name)
}

// ClusterNodeCount delegates to the existing clusterNodeCount helper.
func (h *handlerPolicyDeps) ClusterNodeCount(ctx context.Context) (int, error) {
	return clusterNodeCount(ctx, h.deps)
}

// validateStemcellStorageShared enforces that stemcell storage must be shared
// when the cluster has more than one node. Returns nil on single-node clusters
// (local storage is acceptable there) or when the storage is classified as shared.
//
// Shared classification: uses BackendResolver to obtain a Backend for storage;
// if Backend.Kind() == BackendLocal and the cluster has >1 node, returns a
// cloud error. A cluster-API failure is treated as "unknown" and logged at WARN
// rather than blocking the upload (fail-open for single-node setups where
// ListConfigNodes may not be available).
func validateStemcellStorageShared(ctx context.Context, deps Deps, storage string) error {
	backend, resolveErr := backendResolverOrDefault(deps).Resolve(ctx, storage)
	if resolveErr != nil {
		// Cannot classify storage. Warn and continue — safe for single-node.
		deps.Log(ctx).Warn("create_stemcell: cannot resolve storage backend; skipping shared-storage check",
			log.String("storage", storage),
			log.Err(resolveErr),
		)
		return nil
	}

	if backend.Kind() != pve.BackendLocal {
		// Shared storage — no restriction.
		return nil
	}

	// Storage is local. Check cluster size to decide whether to reject.
	clusterSize, sizeErr := clusterNodeCount(ctx, deps)
	if sizeErr != nil {
		deps.Log(ctx).Warn("create_stemcell: cannot determine cluster node count; skipping shared-storage check",
			log.String("storage", storage),
			log.Err(sizeErr),
		)
		return nil
	}

	if clusterSize > 1 {
		// When replication is opt-in enabled, skip the rejection: each cluster
		// node will receive its own copy of the template via replicateStemcellToNodes.
		if deps.Config != nil && deps.Config.StemcellReplicateLocal {
			return nil
		}
		// Single-shared-template topology: the qcow2 staging pool only needs
		// to be reachable from the node that builds the cache template. When
		// the template-DISK pool (vm_storage) classifies as shared, the one
		// template's disk clones to any node via cloneFromTemplate's
		// cross-node Target= redirect, so a node-local staging pool is fine.
		// The import strategy is excluded: it reads the qcow2 from each VM's
		// own node at create_vm time, which a local staging pool cannot
		// serve. Fail-closed on unknown vm_storage classification: the
		// operator keeps the actionable rejection below.
		if deps.Config != nil && deps.Config.StemcellStrategy != config.StemcellStrategyImport {
			if vmShared, known := stemcellStorageIsShared(ctx, deps, deps.Config.VMStorage); known && vmShared {
				deps.Log(ctx).Info("create_stemcell: stemcell staging storage is node-local but vm_storage is shared; "+
					"a single cache template will serve all nodes via cross-node clone "+
					"(import-strategy fallback is only available on the staging node)",
					log.String("stemcell_storage", storage),
					log.String("vm_storage", deps.Config.VMStorage),
				)
				return nil
			}
		}
		return cpierrors.Cloud(
			"create_stemcell: stemcell storage %q is local-only but the cluster has %d nodes; "+
				"set stemcell_replicate_local=true to replicate the template to each node, "+
				"use a shared storage pool (NFS, Ceph, CIFS, etc.) accessible from all cluster nodes, "+
				"or keep vm_storage on a shared pool (e.g. Ceph RBD) so a single template serves every node via cross-node clone",
			storage, clusterSize,
		)
	}

	// Single-node cluster — local storage is acceptable.
	return nil
}

// clusterNodeCount returns the number of nodes registered in the PVE cluster
// configuration via GET /cluster/config/nodes. Returns (0, err) on API failure.
func clusterNodeCount(ctx context.Context, deps Deps) (int, error) {
	if deps.PVE == nil || deps.PVE.Cluster() == nil {
		return 0, cpierrors.Cloud("clusterNodeCount: cluster service unavailable")
	}
	resp, err := deps.PVE.Cluster().ListConfigNodes(ctx)
	if err != nil {
		return 0, cpierrors.Wrap(pve.WrapError(err), "clusterNodeCount: list cluster config nodes")
	}
	if resp == nil {
		return 0, nil
	}
	return len(*resp), nil
}

// writeTarEntry creates dst, copies exactly hdr.Size bytes from r while
// computing a SHA-256 in one pass, and returns the hex digest plus bytes written.
//
// The output file is opened, written, and closed entirely within this function.
// Closing is deferred so the file handle is released even if code inserted
// between copy and close in future maintenance triggers an early return.
// The close error is captured into the named return; a prior copy error takes
// precedence.
//
// dst must be a CPI-owned path (e.g. filepath.Join(tmpDir, name)) where tmpDir
// was created by os.MkdirTemp. No os.Root scoping is applied here.
func writeTarEntry(hdr *tar.Header, r io.Reader, dst string) (sha256hex string, written int64, err error) {
	out, oerr := os.Create(dst) // #nosec G304 -- dst is filepath.Join(tmpDir, name); tmpDir is CPI-owned MkdirTemp; not director-supplied; os.Root scoping not applicable
	if oerr != nil {
		return "", 0, cpierrors.Cloud("resolveStemcellImage: create %s: %s", dst, oerr.Error())
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	h := sha256.New()
	tee := io.TeeReader(r, h)
	// Bound the per-file write at hdr.Size. archive/tar's reader is
	// already capped at the header-declared size, so a well-formed
	// entry copies exactly hdr.Size bytes and CopyN returns (n, nil).
	// If the tar stream is truncated, CopyN returns io.ErrUnexpectedEOF
	// after copying fewer than hdr.Size bytes; that is treated as an
	// error here so callers cannot upload a half-written disk image.
	n, cerr := io.CopyN(out, tee, hdr.Size)
	if cerr != nil && !errors.Is(cerr, io.EOF) {
		return "", n, cpierrors.Cloud(
			"resolveStemcellImage: copy %s (wrote %d of %d declared bytes): %s",
			dst, n, hdr.Size, cerr.Error())
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// tarCandidate records a file extracted from a stemcell tarball that may be
// the disk image. Two-pass selection: pass 1 extracts all candidates and
// computes each one's SHA-256 via TeeReader during the single copy; pass 2
// selects by preference order and returns the winner's pre-computed SHA so
// the caller skips a second file-read pass.
type tarCandidate struct {
	path      string
	size      int64
	isImg     bool   // true when name ends in .img
	sha256hex string // hex-encoded SHA-256 computed during extraction
}

// extractTarCandidates reads tr, extracting every regular file that is either
// an .img or larger than 1 MiB into tmpDir. It enforces:
//   - Negative tar header sizes → error (malformed/crafted archive).
//   - Cumulative extracted size ≤ MaxStemcellTotalExtract (tar-bomb guard).
//
// cleanup is called on every error path so the caller's deferred cleanup is
// also executed on failure. Returns a non-nil slice on success.
func extractTarCandidates(tr *tar.Reader, tmpDir string, cleanup func()) ([]tarCandidate, error) {
	var candidates []tarCandidate
	var totalExtracted int64
	// Destinations are basename-flattened (path-traversal defense below), so
	// two distinct entries whose basenames collide would write to one path:
	// the second os.Create truncates the first while both candidate records
	// keep their own size and SHA, decoupling the recorded digest from the
	// file's actual bytes. A legitimate BOSH stemcell has no duplicate
	// basenames, so a collision means a malformed or crafted archive.
	seenNames := make(map[string]struct{})
	for {
		hdr, terr := tr.Next()
		if errors.Is(terr, io.EOF) {
			break
		}
		if terr != nil {
			cleanup()
			return nil, cpierrors.StemcellInvalidTar("resolveStemcellImage: tar: %s", terr.Error())
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.Base(hdr.Name)
		isImg := strings.HasSuffix(strings.ToLower(name), ".img")
		// Reject malformed tar headers up front. archive/tar permits a
		// negative Size on synthetic entry types, but for TypeReg a
		// negative value indicates a crafted or corrupt archive and must
		// not be tolerated: it would skew the totalExtracted guard and
		// invalidate the io.CopyN bound below.
		if hdr.Size < 0 {
			cleanup()
			return nil, cpierrors.StemcellInvalidTar(
				"create_stemcell: malformed tar header (negative size %d for %s)",
				hdr.Size, hdr.Name)
		}
		// Skip obviously-not-disk small files that are neither .img nor large.
		if !isImg && hdr.Size < 1024*1024 {
			continue
		}
		// Tar-bomb guard: reject archives whose candidate entries sum to
		// more than MaxStemcellTotalExtract before any extraction begins.
		// hdr.Size is the declared size from the tar header; a malicious
		// archive could lie, so io.CopyN below also caps each file at
		// its declared size to detect bodies longer than the header.
		totalExtracted += hdr.Size
		if totalExtracted > MaxStemcellTotalExtract {
			cleanup()
			return nil, cpierrors.StemcellExtractCap(
				"create_stemcell: tarball entries exceed maximum %dGB; refusing to extract",
				MaxStemcellTotalExtract/(1024*1024*1024))
		}
		if _, dup := seenNames[name]; dup {
			cleanup()
			return nil, cpierrors.StemcellInvalidTar(
				"create_stemcell: tarball contains duplicate entry basename %q; refusing crafted or malformed archive", name)
		}
		seenNames[name] = struct{}{}
		dst := filepath.Join(tmpDir, name)
		entrySHA, _, writeErr := writeTarEntry(hdr, tr, dst)
		if writeErr != nil {
			cleanup()
			return nil, writeErr
		}
		candidates = append(candidates, tarCandidate{
			path:      dst,
			size:      hdr.Size,
			isImg:     isImg,
			sha256hex: entrySHA,
		})
	}
	return candidates, nil
}

// selectTarCandidate picks the best disk image from candidates:
//   - Prefers largest .img file; "root.img" wins ties.
//   - Falls back to largest non-.img candidate.
//
// Returns a cloud error (calling cleanup) when no usable candidate exists.
func selectTarCandidate(candidates []tarCandidate, imagePath string, cleanup func()) (imgPath, imgSHA string, err error) {
	var imgSize int64
	var fallbackPath string
	var fallbackSHA string
	var fallbackSize int64
	// Pass 2: prefer largest .img file; fall back to largest non-.img.
	// "root.img" is a standard BOSH stemcell name and is preferred if
	// multiple .img files share the same size.
	for _, c := range candidates {
		if c.isImg {
			if c.size > imgSize ||
				(c.size == imgSize && filepath.Base(c.path) == "root.img") {
				imgPath = c.path
				imgSHA = c.sha256hex
				imgSize = c.size
			}
		} else {
			if c.size > fallbackSize {
				fallbackPath = c.path
				fallbackSHA = c.sha256hex
				fallbackSize = c.size
			}
		}
	}
	if imgPath == "" {
		// No .img found; use largest non-.img candidate.
		imgPath = fallbackPath
		imgSHA = fallbackSHA
	}
	if imgPath == "" {
		// Neither an .img nor a non-trivial fallback file was extracted.
		// Returning here prevents the rest of the function from running
		// magic-byte detection against an empty path and uploading a
		// zero-byte file to PVE.
		cleanup()
		return "", "", cpierrors.StemcellNoCandidate(
			"create_stemcell: no usable disk image candidate in tarball %s",
			imagePath)
	}
	return imgPath, imgSHA, nil
}

// detectExtractedFormat reads the first 4 magic bytes of imgPath and maps them
// to a PVE disk format string. Accepted signatures:
//
//	qcow2 — 'Q','F','I',0xFB
//	gzip  — 0x1F,0x8B (nested gzip inside tar; treat as raw for PVE)
//	lz4   — 0x04,0x22,0x4D,0x18
//	raw   — any other content of sufficient size (≥ 1 MiB)
//
// Files that do not match any known signature are rejected to prevent
// accidentally uploading a manifest or other metadata file as the disk.
// cleanup is called on every error path.
func detectExtractedFormat(imgPath, defaultFormat string, cleanup func()) (string, error) {
	mf, merr := os.Open(imgPath) // #nosec G304 -- imgPath is filepath.Join(tmpDir, name); tmpDir is CPI-owned MkdirTemp; not director-supplied; os.Root scoping not applicable
	if merr != nil {
		// Cannot open extracted file — fall back to defaultFormat rather than
		// erroring. The subsequent upload will surface any real problem.
		return defaultFormat, nil
	}
	var magic [4]byte
	n, rerr := io.ReadFull(mf, magic[:])
	_ = mf.Close()
	if rerr != nil && n < 2 {
		cleanup()
		return "", cpierrors.Cloud(
			"create_stemcell: extracted image at %s is too small to identify (read %d bytes)",
			imgPath, n)
	}
	switch {
	case magic[0] == 'Q' && magic[1] == 'F' && magic[2] == 'I' && magic[3] == 0xFB:
		return diskFormatQCOW2, nil
	case magic[0] == 0x1F && magic[1] == 0x8B:
		// Nested gzip inside a tar — treat as raw; PVE handles decompression.
		return diskFormatRaw, nil
	case n >= 4 && magic[0] == 0x04 && magic[1] == 0x22 && magic[2] == 0x4D && magic[3] == 0x18:
		// LZ4 frame magic.
		return diskFormatRaw, nil
	default:
		// Require the file to be a known .img or large enough to plausibly
		// be a raw disk. If neither, it likely is not a disk image.
		fi, sterr := os.Stat(imgPath)
		if sterr != nil || fi.Size() < 1024*1024 {
			cleanup()
			return "", cpierrors.StemcellMagicMismatch(
				"create_stemcell: extracted image at %s has unknown magic bytes %x; expected qcow2/gzip/lz4/raw",
				imgPath, magic[:n])
		}
		return diskFormatRaw, nil
	}
}

// resolveGzipTar handles the gzip-tar branch of resolveStemcellImage. It seeks
// f back to the start, opens a gzip reader, creates a temp dir, extracts
// candidates, selects the best, detects format, and logs the result.
func resolveGzipTar(f *os.File, imagePath, defaultFormat string, logger *log.Logger) (string, func(), string, string, error) {
	noop := func() {}
	if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
		return "", noop, "", "", cpierrors.Cloud("resolveStemcellImage: seek: %s", seekErr.Error())
	}
	gz, gzErr := gzip.NewReader(f)
	if gzErr != nil {
		return "", noop, "", "", cpierrors.Cloud("resolveStemcellImage: gzip: %s", gzErr.Error())
	}
	defer func() { _ = gz.Close() }()

	// Find the largest regular file ending in .img (or just the first
	// regular file if no .img is present). Stemcells contain root.img
	// alongside small manifest files.
	tmpDir, mkErr := os.MkdirTemp("", "bosh-stemcell-extract-")
	if mkErr != nil {
		return "", noop, "", "", cpierrors.Cloud("resolveStemcellImage: mktemp: %s", mkErr.Error())
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	tr := tar.NewReader(gz)
	candidates, candErr := extractTarCandidates(tr, tmpDir, cleanup)
	if candErr != nil {
		return "", noop, "", "", candErr
	}
	if len(candidates) == 0 {
		cleanup()
		return "", noop, "", "", cpierrors.Cloud("resolveStemcellImage: no disk image inside tarball %s", imagePath)
	}

	imgPath, imgSHA, selErr := selectTarCandidate(candidates, imagePath, cleanup)
	if selErr != nil {
		return "", noop, "", "", selErr
	}

	format, fmtErr := detectExtractedFormat(imgPath, defaultFormat, cleanup)
	if fmtErr != nil {
		return "", noop, "", "", fmtErr
	}

	logger.Info("resolveStemcellImage: extracted",
		log.String("source", imagePath),
		log.String("disk", imgPath),
		log.String("format", format),
		log.String(jsonKeySHA256, imgSHA),
	)
	return imgPath, cleanup, format, imgSHA, nil
}

// resolveStemcellImage inspects imagePath and, if it is a gzipped tar (as
// produced by the BOSH stemcell builder for openstack/kvm), extracts the
// inner disk image to a temp file and returns its path. The returned cleanup
// function removes any temp files created; it is always safe to call.
//
// detectedFormat is the disk format inferred from the inner image's magic
// bytes ("qcow2" or "raw"). When the input is already a bare disk image
// (not a tarball), detectedFormat is also inferred from its magic.
// An empty detectedFormat means "could not infer; caller should fall back".
//
// extractedSHA256hex is the hex-encoded SHA-256 of the selected disk image
// computed during tarball extraction via TeeReader (single pass). The caller
// uses this value directly and skips a second file-read pass. An empty string
// is returned for the non-tarball path; the caller must call sha256FilePath.
//
// stagingDir is propagated from config.StemcellStagingDir. When non-empty,
// os.Open calls on the director-supplied imagePath are scoped via os.Root
// (openStagedFile). os.Create calls on CPI-owned tmpDir paths are unaffected
// (tmpDir is CPI-internal MkdirTemp; os.Root scoping is not applicable there).
// When stagingDir is empty, all os.Open calls use the direct path —
// byte-identical behavior to prior releases.
func resolveStemcellImage(imagePath, defaultFormat, stagingDir string, logger *log.Logger) (path string, cleanup func(), detectedFormat string, extractedSHA256hex string, err error) {
	noop := func() {}

	f, openErr := openStagedFile(stagingDir, imagePath)
	if openErr != nil {
		return "", noop, "", "", cpierrors.Cloud("resolveStemcellImage: open %s: %s", imagePath, openErr.Error())
	}
	defer func() { _ = f.Close() }()

	// Read enough bytes to identify gzip (2), QCOW2 (4), or plain tar (262).
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]

	// Bare QCOW2 magic: 'Q','F','I',0xFB. SHA computed by caller (direct pass).
	if n >= 4 && head[0] == 'Q' && head[1] == 'F' && head[2] == 'I' && head[3] == 0xFB {
		return imagePath, noop, diskFormatQCOW2, "", nil
	}

	// Gzip magic: 0x1F 0x8B
	if n >= 2 && head[0] == 0x1F && head[1] == 0x8B {
		return resolveGzipTar(f, imagePath, defaultFormat, logger)
	}

	// Not gzip, not qcow2 magic — treat as raw disk image. SHA computed by caller.
	logger.Info("resolveStemcellImage: passthrough as raw", log.String("source", imagePath))
	return imagePath, noop, diskFormatRaw, "", nil
}

// openStagedFile opens path for reading, scoped to stagingDir when non-empty.
//
// When stagingDir is empty the function calls os.Open(path) directly —
// byte-identical behavior to prior releases, matching the contract that
// an empty StemcellStagingDir preserves all existing code paths.
//
// When stagingDir is non-empty the function:
//  1. Opens an os.Root anchored at stagingDir.
//  2. Computes path's position relative to stagingDir via filepath.Rel.
//  3. Rejects the path when the relative result starts with ".." (path
//     escapes the root) or when Rel itself errors (different volume on Windows).
//  4. Calls root.Open(rel) which enforces kernel-level containment.
//
// The caller is responsible for closing the returned *os.File.
func openStagedFile(stagingDir, path string) (*os.File, error) {
	if stagingDir == "" {
		f, err := os.Open(path) // #nosec G304 -- stagingDir empty: byte-identical fallback; director-supplied path validated by validateStemcellImagePath upstream
		if err != nil {
			return nil, err
		}
		return f, nil
	}

	root, err := os.OpenRoot(stagingDir)
	if err != nil {
		return nil, fmt.Errorf("openStagedFile: open root %s: %w", stagingDir, err)
	}
	defer func() { _ = root.Close() }()

	rel, err := filepath.Rel(stagingDir, path)
	if err != nil {
		return nil, fmt.Errorf("openStagedFile: path %q cannot be made relative to staging dir %q: %w", path, stagingDir, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("openStagedFile: path %q escapes staging dir %q", path, stagingDir)
	}

	f, err := root.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("openStagedFile: %w", err)
	}
	return f, nil
}

// sha256FilePath returns the hex-encoded SHA-256 of the file at path.
// When stagingDir is non-empty, file access is scoped to that root via os.Root.
// When stagingDir is empty, os.Open is called directly (byte-identical behavior).
func sha256FilePath(path, stagingDir string) (string, error) {
	f, err := openStagedFile(stagingDir, path)
	if err != nil {
		return "", cpierrors.Cloud("sha256FilePath: open %s: %s", path, err.Error())
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", cpierrors.Cloud("sha256FilePath: read %s: %s", path, err.Error())
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// awaitPriorUploadTask re-awaits the upload task a prior attempt left
// unresolved (its await's own poll failed while the task ran). The task's
// outcome is the authority on what the retry may do next: a completed task
// means the upload already committed, so (true, nil) reports the goal
// reached; a task that RESOLVED with a failure verdict
// (pve.IsTaskExitVerdict) returns (false, nil), telling the caller the
// partial file is provably ours to sweep before re-uploading. Any other
// await failure means the task is still UNRESOLVED and possibly executing,
// so its error is returned instead: a retryable fault rides the loop's
// backoff into another re-await, and a poll timeout exits to the Director
// as retriable, matching a first attempt's await semantics. Retriability is
// deliberately NOT the discriminator here: a lock-timeout verdict is
// retryable yet fully resolved, and treating it as "still running" would
// wedge the loop re-polling a dead task instead of re-driving the upload.
func awaitPriorUploadTask(ctx context.Context, deps Deps, node, filename, upid string) (bool, error) {
	awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, deps.Log(ctx),
		pve.WithMaxWait(pve.StemcellMaxWait))
	if awaitErr == nil {
		deps.Log(ctx).Info("create_stemcell: the prior attempt's upload task completed; upload already committed",
			log.String("node", node),
			log.String("filename", filename),
		)
		return true, nil
	}
	if pve.IsTaskExitVerdict(awaitErr) {
		return false, nil
	}
	return false, awaitErr
}

// sweepUploadTarget clears the upload target name ahead of a re-upload. The
// caller invokes it only on evidence its own attempt may have written the
// file: a dropped POST response (PVE may have committed the file after the
// drop; the name is content-addressed, so re-uploading restores identical
// bytes) or an upload task that failed with a verdict (its partial file is
// ours). A POST rejected before writing (lock timeout) leaves nothing of
// ours behind, and sweeping there could delete a concurrent same-stemcell
// upload's file on no evidence at all. A duplicate import upload is rejected
// with HTTP 409, so the sweep must run before the re-upload; a sweep failing
// on the loop's own retryable fault class is returned to ride the backoff
// (pressing on would turn it into a permanent 409), and only a permanently
// failing sweep proceeds best-effort (nil), leaving the upload to surface
// the truth.
func sweepUploadTarget(ctx context.Context, deps Deps, node, storageName, filename string) error {
	volume := fmt.Sprintf("%s:%s/%s", storageName, pveStorageContentImport, filename)
	existed, delUPID, rmErr := deps.PVE.Storage().DeleteVolumeIfExistsAsync(ctx, node, storageName, volume)
	if rmErr == nil && delUPID != "" {
		rmErr = pve.AwaitTaskWithLogger(ctx, deps.PVE, node, delUPID, deps.Log(ctx))
	}
	if rmErr != nil {
		if pve.IsRetryableOrLockFault(rmErr) {
			return rmErr
		}
		deps.Log(ctx).Warn("create_stemcell: pre-retry upload cleanup failed; retrying the upload anyway",
			log.String("node", node),
			log.String("volume", volume),
			log.Err(rmErr),
		)
	} else if existed {
		deps.Log(ctx).Info("create_stemcell: cleared a committed partial upload before retrying",
			log.String("node", node),
			log.String("volume", volume),
		)
	}
	return nil
}

// uploadStemcellImage streams imagePath to the PVE storage upload endpoint,
// then waits for the resulting task to finish. content is fixed to "import";
// the file lands as a referenceable storage volume "<storage>:import/<filename>".
//
// stagingDir controls os.Root scoping for the open of imagePath:
//   - Non-empty: imagePath must reside under stagingDir; access is scoped via
//     os.Root (openStagedFile). Pass deps.Config.StemcellStagingDir only when
//     imagePath is a director-supplied path.
//   - Empty: os.Open is called directly — byte-identical behavior to prior
//     releases. Always pass "" when imagePath is a CPI-internal temp file.
func uploadStemcellImage(
	ctx context.Context,
	deps Deps,
	node, storageName, filename, imagePath, stagingDir string,
) error {
	if deps.PVE == nil || deps.PVE.Storage() == nil {
		return cpierrors.Cloud("uploadStemcellImage: storage service unavailable")
	}

	// Upload returns a UPID task identifier; wait for completion using
	// StemcellMaxWait (600s) to accommodate format conversion. Both
	// the multipart POST and the resulting task run under the per-storage
	// lockfile, so concurrent stemcell uploads against the same storage can
	// surface "can't lock file ... got timeout" on either side. Retry the
	// whole open+upload+await tuple on that signal; the body stream is
	// reopened from imagePath each attempt so PVE always sees a fresh
	// reader.
	//
	// Disable the SDK's inner HTTP retry: it replays multipart uploads by
	// re-reading req.Body, but the body has already been drained by attempt
	// 0. The replay sends an empty body with the original Content-Length,
	// which Go's transport rejects ("http: ContentLength=N with Body length
	// 0"). Our outer RetryOnTransientOrLock reopens the file each iteration
	// so transient failures retry with a fresh stream.
	uploadCtx := sdkclient.WithRetries(ctx, 0)
	firstAttempt := true
	pendingUPID := ""    // prior attempt's upload task whose completion is unresolved
	sweepNeeded := false // evidence exists that OUR prior attempt wrote (part of) the file
	rerr := pve.RetryOnTransientOrLock(ctx, deps.Log(ctx), "create_stemcell_upload", 0, func() error {
		if !firstAttempt {
			if pendingUPID != "" {
				committed, awaitErr := awaitPriorUploadTask(ctx, deps, node, filename, pendingUPID)
				if committed {
					return nil
				}
				if awaitErr != nil {
					return awaitErr
				}
				pendingUPID = ""
				sweepNeeded = true
			}
			if sweepNeeded {
				if sweepErr := sweepUploadTarget(ctx, deps, node, storageName, filename); sweepErr != nil {
					return sweepErr
				}
				sweepNeeded = false
			}
		}
		firstAttempt = false

		f, openErr := openStagedFile(stagingDir, imagePath)
		if openErr != nil {
			return cpierrors.Cloud("uploadStemcellImage: open %s: %s", imagePath, openErr.Error())
		}
		defer func() { _ = f.Close() }()

		upid, uerr := deps.PVE.Storage().Upload(uploadCtx, node, storageName, pveStorageContentImport, filename, f)
		if uerr != nil {
			if pve.IsTransportConnectionDrop(uerr) {
				sweepNeeded = true
			}
			return uerr
		}
		if upid == "" {
			return nil
		}
		pendingUPID = upid
		aerr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, deps.Log(ctx),
			pve.WithMaxWait(pve.StemcellMaxWait))
		if aerr == nil {
			pendingUPID = ""
		} else if pve.IsTaskExitVerdict(aerr) {
			// Resolved with a failure verdict: a retry must not re-poll the
			// dead task; its partial file is ours to sweep before re-uploading.
			pendingUPID = ""
			sweepNeeded = true
		}
		return aerr
	})
	if rerr != nil {
		// WrapErrorKeepingClass: rerr is the raw last error out of the retry
		// wrapper (or an already-classified await error); flattening it to a
		// permanent Cloud turned an exhausted-but-transient storage fault
		// into a failure the Director will not re-drive.
		return cpierrors.Wrap(pve.WrapErrorKeepingClass(rerr),
			fmt.Sprintf("uploadStemcellImage: upload to %s/%s", node, storageName))
	}
	return nil
}

// stemcellSource classifies where a stemcell image originates for error
// retriability decisions in verifyExpectedDigest.
type stemcellSource int

const (
	// stemcellSourceLocal means the image came from a director-supplied local
	// tarball or path. Digest mismatches are non-retriable: the file is
	// authoritative and retrying with identical bytes would produce the same
	// mismatch.
	stemcellSourceLocal stemcellSource = iota
	// stemcellSourceNetwork means the image was streamed from a remote source
	// (https/s3/blobstore/oci). Digest mismatches may be caused by in-transit
	// corruption; the director should retry to fetch a fresh copy.
	stemcellSourceNetwork
)

// verifyExpectedDigest compares a known hex digest against the expected values
// in cp. The function implements the following policy:
//
//   - SHA-256 takes precedence: if cp.ExpectedSHA256 is non-empty, only it is
//     checked (SHA-1 is ignored even when present).
//   - SHA-1 fallback: if cp.ExpectedSHA256 is empty but cp.ExpectedSHA1 is set,
//     sha1FilePath is computed and compared.
//   - No expected digest: a warning is logged and nil is returned (compute-only).
//
// Comparison is case-insensitive (both sides lowercased).
//
// Error retriability:
//   - stemcellSourceNetwork mismatch → cpierrors.Retriable
//   - stemcellSourceLocal mismatch   → cpierrors.Cloud (non-retriable)
//
// sha256hex is the already-computed SHA-256 hex for the resolved image (may be
// empty for light-preuploaded, in which case SHA-256 comparison is skipped and
// only SHA-1 is attempted when set). stagingDir scopes file access for SHA-1
// computation (mirrors sha256FilePath convention).
func verifyExpectedDigest(
	ctx context.Context,
	logger *log.Logger,
	cp stemcellCloudProps,
	sha256hex string,
	resolvedPath string,
	stagingDir string,
	src stemcellSource,
) error {
	_ = ctx // reserved for future use (e.g. deadline propagation)

	// SHA-256 check.
	if cp.ExpectedSHA256 != "" {
		if sha256hex == "" {
			// Fail closed: the operator explicitly requested integrity
			// verification, so an unavailable hash must block the upload
			// rather than silently skip the check — a read error on the
			// staging file would otherwise turn the integrity gate into a
			// no-op with only a Warn nobody watches. Retriable because the
			// underlying cause (transient I/O) usually clears.
			return cpierrors.Retriable(
				"create_stemcell: cloud_properties.sha256 is set (%s) but the actual digest could not be computed; refusing to upload unverified image",
				strings.ToLower(cp.ExpectedSHA256))
		}
		if !strings.EqualFold(sha256hex, cp.ExpectedSHA256) {
			msg := fmt.Sprintf(
				"create_stemcell: sha256 digest mismatch: expected %s, got %s",
				strings.ToLower(cp.ExpectedSHA256), strings.ToLower(sha256hex),
			)
			if src == stemcellSourceNetwork {
				return cpierrors.Retriable("%s", msg)
			}
			return cpierrors.Cloud("%s", msg)
		}
		logger.Info("create_stemcell: sha256 digest verified",
			log.String(jsonKeySHA256, sha256hex),
		)
		return nil
	}

	// SHA-1 check (only when SHA-256 not expected).
	if cp.ExpectedSHA1 != "" {
		actual, hashErr := sha1FilePath(resolvedPath, stagingDir)
		if hashErr != nil {
			// Fail closed, same rationale as the SHA-256 branch above.
			return cpierrors.Retriable(
				"create_stemcell: cloud_properties.sha1 is set but the actual digest could not be computed (%s); refusing to upload unverified image",
				hashErr.Error())
		}
		if !strings.EqualFold(actual, cp.ExpectedSHA1) {
			msg := fmt.Sprintf(
				"create_stemcell: sha1 digest mismatch: expected %s, got %s",
				strings.ToLower(cp.ExpectedSHA1), strings.ToLower(actual),
			)
			if src == stemcellSourceNetwork {
				return cpierrors.Retriable("%s", msg)
			}
			return cpierrors.Cloud("%s", msg)
		}
		logger.Info("create_stemcell: sha1 digest verified",
			log.String("sha1", actual),
		)
		return nil
	}

	// No expected digest provided — log unverified warning.
	logger.Warn("create_stemcell: stemcell integrity unverified (no expected digest in cloud_properties)")
	return nil
}

// sha1FilePath computes the SHA-1 hex digest of the file at path.
// When stagingDir is non-empty, file access is scoped via openStagedFile.
// When empty, os.Open is called directly.
//
//nolint:gosec // SHA-1 used only for operator-supplied expected-digest comparison, not for security
func sha1FilePath(path, stagingDir string) (string, error) {
	f, err := openStagedFile(stagingDir, path)
	if err != nil {
		return "", cpierrors.Cloud("sha1FilePath: open %s: %s", path, err.Error())
	}
	defer func() { _ = f.Close() }()
	h := cryptosha1.New() // #nosec G401 -- SHA-1 for operator-supplied expected-digest comparison only, not for security
	if _, err := io.Copy(h, f); err != nil {
		return "", cpierrors.Cloud("sha1FilePath: read %s: %s", path, err.Error())
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// listClusterNodes returns all node names from GET /cluster/config/nodes.
// Returns nil, nil on an empty list. Used by replication paths to enumerate
// target nodes.
func listClusterNodes(ctx context.Context, deps Deps) ([]string, error) {
	if deps.PVE == nil || deps.PVE.Cluster() == nil {
		return nil, cpierrors.Cloud("listClusterNodes: cluster service unavailable")
	}
	resp, err := deps.PVE.Cluster().ListConfigNodes(ctx)
	if err != nil {
		return nil, cpierrors.Wrap(err, "listClusterNodes: list cluster config nodes")
	}
	if resp == nil || len(*resp) == 0 {
		return nil, nil
	}
	// Each item is a json.RawMessage; extract the "name" field.
	type nodeItem struct {
		Name string `json:"name"`
	}
	nodes := make([]string, 0, len(*resp))
	for _, raw := range *resp {
		var item nodeItem
		if jerr := json.Unmarshal(raw, &item); jerr != nil || item.Name == "" {
			continue
		}
		nodes = append(nodes, item.Name)
	}
	return nodes, nil
}

// replicaOutcome records the terminal outcome of one node's replication
// attempt. Stage names the step the attempt ended at ("existing-check",
// "adopt", "upload", "inflight-gate", "ensure-template", "panic", or a success
// marker: "replicated" / "already-exists" / "adopted"). Err is nil for every
// successful outcome and non-nil exactly when the node ended up without a
// usable replica.
type replicaOutcome struct {
	Node  string
	Stage string
	Err   error
}

// logReplicationSummary emits exactly one summary line for a completed
// replication fan-out: warn when any node failed (failed count, failed node
// list, and each failed node's terminal stage and first error), info when
// every node succeeded. Replication is best-effort, so the warn is
// informational — the stemcell the caller returned is unaffected; the named
// nodes simply have no replica until a later create_stemcell re-drives them.
func logReplicationSummary(logger *log.Logger, label string, outcomes []replicaOutcome) {
	if len(outcomes) == 0 {
		return
	}
	fields := []log.Field{log.Int("replica_nodes", len(outcomes))}
	var failedNodes []string
	for _, o := range outcomes {
		if o.Err == nil {
			continue
		}
		failedNodes = append(failedNodes, o.Node)
		fields = append(fields, log.String("error_"+o.Node, o.Stage+": "+o.Err.Error()))
	}
	if len(failedNodes) == 0 {
		logger.Info(label+": all replicas succeeded", fields...)
		return
	}
	fields = append(fields,
		log.Int("failed", len(failedNodes)),
		log.String("failed_nodes", strings.Join(failedNodes, ",")),
	)
	logger.Warn(label+": some replicas failed (non-fatal; the stemcell itself succeeded — "+
		"affected nodes have no replica until a later create_stemcell re-drives them)", fields...)
}

// replicateStemcellToNodes uploads qcow2 and builds a template VM on every
// cluster node in targetNodes except for primaryNode (which already has the
// primary template). Each replica VM carries both the sha tag and a per-node
// tag "bosh-stemcell-node-<node>".
//
// This function is called only when StemcellReplicateLocal is true and the
// storage backend is node-local. On shared storage it is never called (the
// single primary template is accessible from all nodes via the shared pool).
//
// Each node's upload+ensureTemplateVM is attempted independently. A per-node
// failure is logged as a warning and does not abort replication to remaining
// nodes (best-effort semantics). The caller should treat a partial replication
// as a degraded-but-usable state: create_vm on a node without a replica will
// fail fast with a clear actionable error (from the create_vm guard), giving
// the operator a chance to re-run create_stemcell to complete replication.
//
// Concurrency is controlled by deps.Config.StemcellReplicationConcurrencyValue():
//   - 1 (default, nil/0 config) → serial, deterministic, byte-identical to prior releases.
//   - N > 1 → up to N nodes replicated concurrently via a bounded semaphore.
//
// Concurrency safety:
//   - uploadStemcellImage opens its own file handle per call — no shared *os.File.
//   - deps.Log(ctx).With(...) returns a new zap logger; zap is concurrency-safe.
//   - deps.Inflight.acquire keys by node name and uses sync.Mutex internally — safe
//     under concurrent different-node calls from multiple goroutines.
//   - VMID allocation uses AllocateWithRetry which regenerates on conflict — safe
//     under concurrent cluster-wide allocation from parallel goroutines.
//   - No mutable state is shared between goroutines beyond the outcomes
//     slice, where each goroutine writes only its own index and the slice is
//     read only after wg.Wait().
//
// kind is the primary template's path-identity kind and is stamped into each
// replica's provenance unchanged: a replica of a light stemcell's cache
// template is still CPI-owned (the operator owns only the qcow2), but its
// provenance must not claim the CPI uploaded the underlying bytes.
//
// After the fan-out completes, the per-node replicaOutcomes are condensed by
// logReplicationSummary into a single aggregate line. The outcomes carry
// observability only — failures stay best-effort and never propagate.
func replicateStemcellToNodes(
	ctx context.Context,
	deps Deps,
	primaryNode, storage, qcow2Filename, sha256hex string,
	targetNodes []string,
	uploadSourcePath, uploadStagingDir string,
	stemcellCID, creatingDirectorUUID string,
	kind pve.StemcellKind,
	cp stemcellCloudProps,
	source string,
) {
	sha8 := sha256hex
	if len(sha8) > 8 {
		sha8 = sha8[:8]
	}
	logger := deps.Log(ctx)

	// Determine worker pool size. 0 or absent resolves to 1 (serial).
	workerLimit := 1
	if deps.Config != nil {
		workerLimit = deps.Config.StemcellReplicationConcurrencyValue()
	}

	// Collect non-primary nodes to replicate, dropping nodes a storage
	// nodes-restriction excludes (an upload addressed to one can only fail).
	var replicaNodes []string
	for _, node := range targetNodes {
		if node != primaryNode {
			replicaNodes = append(replicaNodes, node)
		}
	}
	replicaNodes = filterReplicaNodesToOwners(ctx, deps, "create_stemcell: replication", storage, replicaNodes)
	if len(replicaNodes) == 0 {
		return
	}

	// sem is a buffered channel acting as a counting semaphore: at most
	// workerLimit goroutines hold a token simultaneously. With workerLimit=1
	// this is exactly the serial behavior of the original loop.
	sem := make(chan struct{}, workerLimit)

	// One slot per replica node; each goroutine writes only its own index, and
	// the slice is read only after wg.Wait(), so no lock is needed.
	outcomes := make([]replicaOutcome, len(replicaNodes))

	var wg sync.WaitGroup
	for i, node := range replicaNodes {
		i, node := i, node // capture per iteration
		nodeLogger := logger.With(log.String("replica_node", node))

		wg.Add(1)
		// Acquire a semaphore slot before launching the goroutine so the pool
		// is bounded by workerLimit active goroutines at any moment.
		sem <- struct{}{}

		go func() {
			// H2 (A13 review): this is the only `go func` in non-test CPI
			// code. Every other request path is protected by the dispatcher's
			// own recover (internal/cpi/dispatcher.go's Handle) and the
			// runCPI loop backstop (cmd/cpi/main.go's dispatchOne) — neither
			// covers a panic in a CHILD goroutine, which propagates past both
			// and crashes the entire process: stderr gets a stack trace,
			// stdout (the JSON-RPC response stream) gets nothing, and the
			// Director sees "unexpected end of input" for whatever request
			// happened to be in flight when this goroutine's stack unwound —
			// not necessarily this create_stemcell call itself. Recovering
			// here degrades a replica-node panic to a logged error, matching
			// replicateOneNode's own documented contract ("best-effort,
			// logged as warnings, never returned as errors") instead of
			// silently violating it by taking the whole process down.
			//
			// Defer registration order matters here: Go runs deferred calls
			// LIFO, so the recover MUST be registered LAST (after wg.Done and
			// the sem release below) so it fires FIRST during unwind — its
			// nodeLogger.Error call fully completes before wg.Done() signals
			// this goroutine as finished. Registering it first (so it runs
			// LAST) would let wg.Wait() in the caller return while the
			// recovered panic's log write is still in flight, racing any
			// caller code that reads/inspects shared state right after
			// replicateStemcellToNodes returns.
			defer wg.Done()
			defer func() { <-sem }() // release slot when node work is done
			defer func() {
				if r := recover(); r != nil {
					nodeLogger.Error(
						"create_stemcell: replica-node worker panicked (recovered) — this node's replica was not completed; re-run create_stemcell to retry it",
						log.Any("panic", r),
						log.String("stack", string(debug.Stack())),
					)
					outcomes[i] = replicaOutcome{
						Node:  node,
						Stage: "panic",
						Err:   fmt.Errorf("worker panicked: %v", r),
					}
				}
			}()

			outcomes[i] = replicateOneNode(ctx, deps, nodeLogger, node, storage,
				qcow2Filename, sha256hex, sha8, uploadSourcePath, uploadStagingDir,
				stemcellCID, creatingDirectorUUID, kind, cp, source)
		}()
	}
	wg.Wait()
	logReplicationSummary(logger, "create_stemcell: replication", outcomes)
}

// replicateOneNode performs the full upload+ensureTemplate sequence for a single
// replica node. It is called from a goroutine inside replicateStemcellToNodes.
// All failures are best-effort: logged as warnings, never returned as errors —
// the returned replicaOutcome names the terminal stage (and its error, when the
// node ended up without a usable replica) purely so the caller can log one
// aggregate summary. The function is self-contained — it holds no references to
// shared mutable state.
//
// Replicas do NOT register a director reference of their own: the returned
// CID's ref set lives on the primary (or cluster-scoped dedup-hit) cache
// template only. A replica is purely a per-node clone-speed optimization —
// destroying the primary's last ref sweeps every same-sha8 replica alongside
// it (delete_stemcell's cluster-wide sha-tag sweep), so a replica holding its
// own independent ref set would be redundant bookkeeping with no
// corresponding destroy path of its own.
func replicateOneNode(
	ctx context.Context,
	deps Deps,
	nodeLogger *log.Logger,
	node, storage,
	qcow2Filename, sha256hex, sha8,
	uploadSourcePath, uploadStagingDir string,
	stemcellCID, creatingDirectorUUID string,
	kind pve.StemcellKind,
	cp stemcellCloudProps,
	source string,
) replicaOutcome {
	// Check whether a replica already exists on this node (idempotent).
	existingVMID, alreadyExists, checkErr := pve.ResolveTemplateVMIDForNode(ctx, deps.PVE, node, sha8)
	if checkErr != nil {
		nodeLogger.Warn("create_stemcell: replication: cannot check existing replica (skipping node)",
			log.Err(checkErr),
		)
		return replicaOutcome{Node: node, Stage: "existing-check", Err: checkErr}
	}
	if alreadyExists {
		nodeLogger.Info("create_stemcell: replication: replica already exists (skipping upload)",
			log.Int(metadataKeyVMID, existingVMID),
		)
		return replicaOutcome{Node: node, Stage: "already-exists"}
	}

	// Adopt-and-wait on a racing concurrent replica clone: another CPI process may
	// already be building this same per-node replica (tagged but not yet frozen).
	// Probe for that in-flight winner BEFORE uploading our own copy; on adoption we
	// skip the upload+build entirely, avoiding a duplicate half-built replica and an
	// orphaned qcow2. Disabled (timeout 0) → skipped, byte-identical behaviour.
	if deps.Config != nil && deps.Config.ReplicaAdoptEnabled() {
		adoptTimeout := time.Duration(deps.Config.ReplicaAdoptTimeoutSecValue()) * time.Second
		adoptedVMID, adopted, adoptErr := pve.AdoptReplicaTemplate(ctx, deps.PVE, node, sha8, adoptTimeout)
		switch {
		case adoptErr != nil:
			// A winner was building but did not settle within the bound. Skip this
			// node rather than racing a duplicate build; create_stemcell replication
			// is best-effort and the node can be re-driven on the next deploy.
			nodeLogger.Warn("create_stemcell: replication: adopt-and-wait on racing replica timed out (skipping node)",
				log.Err(adoptErr),
			)
			return replicaOutcome{Node: node, Stage: "adopt", Err: adoptErr}
		case adopted:
			nodeLogger.Info("create_stemcell: replication: adopted in-flight replica from concurrent builder (skipping upload)",
				log.Int(metadataKeyVMID, adoptedVMID),
			)
			return replicaOutcome{Node: node, Stage: "adopted"}
		}
	}

	// Upload qcow2 to this node's local storage — unless the stemcell pool is
	// cluster-shared, where "import/<file>" already resolves to the same file
	// on every node and the template build below imports it directly (the
	// split configuration replication now serves: shared qcow2 pool, local
	// vm_storage). uploadStemcellImage opens its own file handle
	// (openStagedFile inside), so concurrent calls for different nodes read
	// the same source file independently without sharing an *os.File.
	if shared, known := stemcellStorageIsShared(ctx, deps, storage); known && shared {
		nodeLogger.Info("create_stemcell: replication: stemcell storage is shared; skipping per-node upload")
	} else if uploadSourcePath == "" {
		// No local bytes to re-upload and the pool is not known-shared: the
		// import volid cannot be made visible on this node. Light-preuploaded
		// only reaches replication via a shared pool, so this is a
		// classification failure, not a normal configuration.
		nodeLogger.Warn("create_stemcell: replication: no upload source and stemcell storage not " +
			"classifiable as shared; skipping node")
		return replicaOutcome{Node: node, Stage: "upload",
			Err: fmt.Errorf("no upload source and stemcell storage not classifiable as shared")}
	} else if uploadErr := uploadStemcellImage(ctx, deps, node, storage, qcow2Filename, uploadSourcePath, uploadStagingDir); uploadErr != nil {
		nodeLogger.Warn("create_stemcell: replication: upload failed (non-fatal; replica not created)",
			log.Err(uploadErr),
		)
		return replicaOutcome{Node: node, Stage: "upload", Err: uploadErr}
	}

	// Build template VM on this node. The replica carries both sha tag and
	// the per-node tag. We set cp.Node to the target so the replica is
	// pinned; ensureTemplateVM is called with node as templateNode.
	//
	// Per-node in-flight gate wrapped in an IIFE so defer fires at the
	// end of this node's work, avoiding the deferInLoop resource-leak pattern.
	// deps.Inflight.acquire is concurrency-safe (uses sync.Mutex internally);
	// different-node goroutines contend only on the registry mutex, not on each
	// other's semaphore channels.
	replicaCP := cp
	replicaCP.Node = node
	return func() replicaOutcome {
		if deps.Config != nil {
			replicaRelease, replicaInflightErr := deps.Inflight.acquire(ctx, node, deps.Config.MaxInflightPerNodeLimit())
			if replicaInflightErr != nil {
				nodeLogger.Warn("create_stemcell: replication: in-flight limit; skipping replica node",
					log.String("node", node),
					log.Err(replicaInflightErr),
				)
				return replicaOutcome{Node: node, Stage: "inflight-gate", Err: replicaInflightErr}
			}
			defer replicaRelease()
		}
		replicaVMID, tmplErr := ensureReplicaTemplateVM(ctx, deps, node, storage, qcow2Filename, sha256hex,
			kind, stemcellCID, creatingDirectorUUID, replicaCP, source)
		if tmplErr != nil {
			nodeLogger.Warn("create_stemcell: replication: ensure template failed (non-fatal; replica not created)",
				log.Err(tmplErr),
			)
			// Best-effort: delete the uploaded qcow2 to reclaim storage, but
			// ONLY when storage is positively classified node-local. On shared
			// storage — or when classification fails and the caller's own
			// policy is to fail open into replication anyway — "import/<file>"
			// resolves to the SAME file on every node: it is the one qcow2 the
			// CID this create_stemcell call already returned to the caller
			// names. Deleting it here would silently break a stemcell the
			// caller was told succeeded.
			if shared, known := stemcellStorageIsShared(ctx, deps, storage); known && !shared {
				volumePath := "import/" + qcow2Filename
				if _, delErr := deps.PVE.Storage().DeleteVolumeIfExists(ctx, node, storage, volumePath); delErr != nil {
					nodeLogger.Warn("create_stemcell: replication: cleanup of failed upload also failed (non-fatal)",
						log.Err(delErr),
					)
				}
			} else {
				nodeLogger.Warn("create_stemcell: replication: skipping upload cleanup — storage is shared or " +
					"its classification is unknown, and the returned stemcell CID may name this same file")
			}
			return replicaOutcome{Node: node, Stage: "ensure-template", Err: tmplErr}
		}
		nodeLogger.Info("create_stemcell: replication: replica template created",
			log.Int64(metadataKeyVMID, replicaVMID),
		)
		return replicaOutcome{Node: node, Stage: "replicated"}
	}()
}

// ensureReplicaTemplateVM is like ensureTemplateVM but tags the created VM with
// both the sha tag and the per-node replica tag "bosh-stemcell-node-<node>",
// and its dedup lookup (pve.ResolveTemplateVMIDForNode) stays node-scoped by
// design — a replica exists to serve clones ON this specific node, so its
// idempotency check must see this node's state, not the cluster's.
//
// The replica qcow2 IS a fresh CPI upload (uploaded just above in
// replicateStemcellToNodes) but, per D10, is never reclaimed after freeze —
// same no-post-freeze-deletion policy as the primary. This node's copy is a
// clone-speed optimization; delete_stemcell's cluster-wide sha-tag sweep
// removes it alongside the primary at last-ref, and self-healing re-upload on
// a future dedup miss is preferable to a reclaim-then-rebuild cycle on every
// replica.
func ensureReplicaTemplateVM(
	ctx context.Context,
	deps Deps,
	node, storage, qcow2Filename, sha256hex string,
	kind pve.StemcellKind,
	stemcellCID, creatingDirectorUUID string,
	cp stemcellCloudProps,
	source string,
) (int64, error) {
	sha8 := sha8Of(sha256hex)
	logger := deps.Log(ctx)

	// Build deterministic template name (same as primary, differentiator is tag).
	templateName := pve.BuildTemplateNameWithSHA(cp.Name, cp.Version, sha8)

	// Dedup: check sha tag + node tag combo (node-scoped; see doc comment).
	existingVMID, alreadyExists, checkErr := pve.ResolveTemplateVMIDForNode(ctx, deps.PVE, node, sha8)
	if checkErr != nil {
		return 0, fmt.Errorf("ensureReplicaTemplateVM: check existing %q node %q: %w", sha8, node, checkErr)
	}
	if alreadyExists {
		logger.Info("ensureReplicaTemplateVM: replica already exists",
			log.String("node", node),
			log.Int(metadataKeyVMID, existingVMID),
		)
		return int64(existingVMID), nil
	}

	importVolid := storage + ":import/" + qcow2Filename
	shaTag := ""
	if sha8 != "" {
		shaTag = stemcellSHATagPrefix + sha8
	}
	nodeTag := pve.ReplicaNodeTagForNode(node)

	isRetryable := func(e error) bool {
		return pve.IsVMIDConflict(e) || pve.IsStorageLockTimeout(e) || pve.IsTransientTransport(e)
	}

	rangeStart := deps.Config.StemcellTemplateVMIDRangeStart
	rangeEnd := deps.Config.StemcellTemplateVMIDRangeEnd

	spec := templateBuildSpec{
		TemplateName:         templateName,
		ImportVolid:          importVolid,
		ShaTag:               shaTag,
		SHA256Hex:            sha256hex,
		TargetStorage:        deps.Config.VMStorage,
		Kind:                 kind,
		CID:                  stemcellCID,
		CreatingDirectorUUID: creatingDirectorUUID,
		// ExtraBaseTags carries the per-node replica tag so attemptCreateTemplateVM
		// includes it in the base tag set alongside stemcellCacheTag/ShaTag.
		ExtraBaseTags: []string{nodeTag},
	}

	// The configured template pool must exist before the create loop:
	// attemptCreateTemplateVM passes it as the qemu-create "pool" param, and a
	// dedup hit on the primary can reach replica creation without ever running
	// ensureTemplateVM's own pool ensure. Idempotent when already ensured.
	if deps.Config.StemcellTemplatePool != "" {
		if ensureErr := pve.EnsurePoolExists(ctx, deps.PVE, deps.Config.StemcellTemplatePool,
			pve.PoolProvenance("")); ensureErr != nil {
			return 0, fmt.Errorf("ensureReplicaTemplateVM: ensure pool %q exists: %w",
				deps.Config.StemcellTemplatePool, ensureErr)
		}
	}

	// pve.WithStorageScan(node, deps.Config.VMStorage): mirrors the primary's
	// allocation guard (ensureTemplateVM) — without it, a replica VMID could
	// collide with a VM or template another AZ's cluster already owns on the
	// same shared storage.
	allocatedRaw, allocErr := pve.AllocateWithRetry(ctx, deps.PVE,
		func(candidate int) error {
			return attemptCreateTemplateVM(ctx, deps, logger, node, candidate, spec, cp, source)
		},
		isRetryable,
		0,
		pve.WithRange(rangeStart, rangeEnd),
		pve.WithStorageScan(node, deps.Config.VMStorage),
	)
	if allocErr != nil {
		return 0, fmt.Errorf("ensureReplicaTemplateVM: allocate+create replica VM node %q: %w", node, allocErr)
	}
	allocatedVMID := int64(allocatedRaw)

	// Freeze into template.
	freezeUPID, freezeErr := pve.MakeTemplate(ctx, deps.PVE, node, allocatedVMID)
	if freezeErr != nil {
		cleanupLeakedTemplateVM(ctx, deps, node, allocatedVMID, logger, "freeze")
		return 0, fmt.Errorf("ensureReplicaTemplateVM: freeze node=%q vmid=%d: %w", node, allocatedVMID, freezeErr)
	}
	if freezeUPID != "" {
		if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, freezeUPID, logger,
			pve.WithMaxWait(pve.StemcellMaxWait)); awaitErr != nil {
			cleanupLeakedTemplateVM(ctx, deps, node, allocatedVMID, logger, "await freeze")
			return 0, fmt.Errorf("ensureReplicaTemplateVM: await freeze node=%q vmid=%d upid=%s: %w",
				node, allocatedVMID, freezeUPID, awaitErr)
		}
	}

	// No source retention step: per D10, the per-node uploaded qcow2 is never
	// reclaimed after freeze (see doc comment above).
	logger.Info("ensureReplicaTemplateVM: replica template frozen",
		log.String("node", node),
		log.Int64(metadataKeyVMID, allocatedVMID),
	)
	return allocatedVMID, nil
}

// replicateServerDownloadToNodes replicates a server-side-downloaded
// (cloud_properties.source_url, PVE download-url API) stemcell to every other
// cluster node. Unlike replicateStemcellToNodes (tarball and light-fetch
// paths, which copy a CPI-local file to each replica node), the CPI never
// holds the image bytes locally for a source_url stemcell — PVE streamed them
// directly into storage on the primary node — so there is no local file to
// copy. The only way to place a copy on another node's local storage is to
// have PVE download it there too: each replica node gets its own
// CreateStorageDownloadUrl call against the same sourceURL, checksummed
// against sha256hex exactly like the primary download.
//
// Same best-effort contract as replicateStemcellToNodes: called only when
// deps.Config.StemcellReplicateLocal is true and storage is not shared
// (mirrors the mainline gate in HandleCreateStemcell); per-node failures are
// logged as warnings and never returned as errors or propagated to the
// create_stemcell caller.
func replicateServerDownloadToNodes(
	ctx context.Context,
	deps Deps,
	primaryNode, storage, qcow2Filename, sha256hex string,
	targetNodes []string,
	sourceURL string,
	stemcellCID, creatingDirectorUUID string,
	cp stemcellCloudProps,
) {
	sha8 := sha8Of(sha256hex)
	logger := deps.Log(ctx)

	workerLimit := 1
	if deps.Config != nil {
		workerLimit = deps.Config.StemcellReplicationConcurrencyValue()
	}

	var replicaNodes []string
	for _, node := range targetNodes {
		if node != primaryNode {
			replicaNodes = append(replicaNodes, node)
		}
	}
	replicaNodes = filterReplicaNodesToOwners(ctx, deps, "create_stemcell: server-download replication", storage, replicaNodes)
	if len(replicaNodes) == 0 {
		return
	}

	sem := make(chan struct{}, workerLimit)

	// Same single-writer-per-index discipline as replicateStemcellToNodes:
	// read only after wg.Wait().
	outcomes := make([]replicaOutcome, len(replicaNodes))

	var wg sync.WaitGroup
	for i, node := range replicaNodes {
		i, node := i, node // capture per iteration
		nodeLogger := logger.With(log.String("replica_node", node))

		wg.Add(1)
		sem <- struct{}{}

		go func() {
			// Mirrors replicateStemcellToNodes's own goroutine: this is
			// production CPI code running off the request goroutine, so an
			// unrecovered panic here would crash the whole process (see the
			// detailed rationale on replicateStemcellToNodes above).
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					nodeLogger.Error(
						"create_stemcell: server-download replica-node worker panicked (recovered) — "+
							"this node's replica was not completed; re-run create_stemcell to retry it",
						log.Any("panic", r),
						log.String("stack", string(debug.Stack())),
					)
					outcomes[i] = replicaOutcome{
						Node:  node,
						Stage: "panic",
						Err:   fmt.Errorf("worker panicked: %v", r),
					}
				}
			}()

			outcomes[i] = replicateOneNodeServerDownload(ctx, deps, nodeLogger, node, storage,
				qcow2Filename, sha256hex, sha8, sourceURL, stemcellCID, creatingDirectorUUID, cp)
		}()
	}
	wg.Wait()
	logReplicationSummary(logger, "create_stemcell: server-download replication", outcomes)
}

// replicateOneNodeServerDownload performs the download+ensureTemplate
// sequence for a single replica node in the server-download path. Called from
// a goroutine inside replicateServerDownloadToNodes. All failures are
// best-effort: logged as warnings, never returned as errors — the returned
// replicaOutcome names the terminal stage for the caller's aggregate summary.
//
// Like replicateOneNode, a replica built here does NOT register its own
// director reference — the returned CID's ref set lives on the primary
// template only.
// buildServerDownloadReplica ensures one node's replica template from a
// qcow2 already visible on that node — either the shared-pool arm of
// replicateOneNodeServerDownload (no per-node download happened, so
// onFailure is nil) or the post-download arm (onFailure is the download
// cleanup for this node's own local copy). Applies the per-node in-flight
// gate in both arms. Best-effort: failures are logged and never propagate;
// the returned replicaOutcome carries the terminal stage for the summary.
func buildServerDownloadReplica(
	ctx context.Context,
	deps Deps,
	nodeLogger *log.Logger,
	node, storage, qcow2Filename, sha256hex, sourceURL string,
	stemcellCID, creatingDirectorUUID string,
	cp stemcellCloudProps,
	onFailure func(filename string),
) replicaOutcome {
	replicaCP := cp
	replicaCP.Node = node
	if deps.Config != nil {
		replicaRelease, replicaInflightErr := deps.Inflight.acquire(ctx, node, deps.Config.MaxInflightPerNodeLimit())
		if replicaInflightErr != nil {
			nodeLogger.Warn("create_stemcell: server-download replication: in-flight limit; skipping replica node",
				log.String("node", node),
				log.Err(replicaInflightErr),
			)
			return replicaOutcome{Node: node, Stage: "inflight-gate", Err: replicaInflightErr}
		}
		defer replicaRelease()
	}
	replicaVMID, tmplErr := ensureReplicaTemplateVM(ctx, deps, node, storage, qcow2Filename, sha256hex,
		pve.StemcellKindHeavy, stemcellCID, creatingDirectorUUID, replicaCP, sourceURL)
	if tmplErr != nil {
		nodeLogger.Warn("create_stemcell: server-download replication: ensure template failed (non-fatal; replica not created)",
			log.Err(tmplErr),
		)
		if onFailure != nil {
			onFailure(qcow2Filename)
		}
		return replicaOutcome{Node: node, Stage: "ensure-template", Err: tmplErr}
	}
	nodeLogger.Info("create_stemcell: server-download replication: replica template created",
		log.Int64(metadataKeyVMID, replicaVMID),
	)
	return replicaOutcome{Node: node, Stage: "replicated"}
}

func replicateOneNodeServerDownload(
	ctx context.Context,
	deps Deps,
	nodeLogger *log.Logger,
	node, storage, qcow2Filename, sha256hex, sha8, sourceURL string,
	stemcellCID, creatingDirectorUUID string,
	cp stemcellCloudProps,
) replicaOutcome {
	// Idempotent: skip when a replica already exists on this node.
	existingVMID, alreadyExists, checkErr := pve.ResolveTemplateVMIDForNode(ctx, deps.PVE, node, sha8)
	if checkErr != nil {
		nodeLogger.Warn("create_stemcell: server-download replication: cannot check existing replica (skipping node)",
			log.Err(checkErr),
		)
		return replicaOutcome{Node: node, Stage: "existing-check", Err: checkErr}
	}
	if alreadyExists {
		nodeLogger.Info("create_stemcell: server-download replication: replica already exists (skipping download)",
			log.Int(metadataKeyVMID, existingVMID),
		)
		return replicaOutcome{Node: node, Stage: "already-exists"}
	}

	// Adopt-and-wait on a racing concurrent replica build (same mechanism as
	// replicateOneNode); disabled (timeout 0) → skipped, byte-identical.
	if deps.Config != nil && deps.Config.ReplicaAdoptEnabled() {
		adoptTimeout := time.Duration(deps.Config.ReplicaAdoptTimeoutSecValue()) * time.Second
		adoptedVMID, adopted, adoptErr := pve.AdoptReplicaTemplate(ctx, deps.PVE, node, sha8, adoptTimeout)
		switch {
		case adoptErr != nil:
			nodeLogger.Warn("create_stemcell: server-download replication: adopt-and-wait on racing replica timed out (skipping node)",
				log.Err(adoptErr),
			)
			return replicaOutcome{Node: node, Stage: "adopt", Err: adoptErr}
		case adopted:
			nodeLogger.Info("create_stemcell: server-download replication: adopted in-flight replica from concurrent builder (skipping download)",
				log.Int(metadataKeyVMID, adoptedVMID),
			)
			return replicaOutcome{Node: node, Stage: "adopted"}
		}
	}

	if deps.PVE == nil || deps.PVE.Nodes() == nil {
		nodeLogger.Warn("create_stemcell: server-download replication: nodes service unavailable (skipping node)")
		return replicaOutcome{Node: node, Stage: "download",
			Err: fmt.Errorf("nodes service unavailable")}
	}

	// Shared stemcell pool: the primary's download is already visible on this
	// node — skip the per-node re-download and build the template directly
	// (the split configuration replication now serves: shared qcow2 pool,
	// node-local vm_storage).
	if shared, known := stemcellStorageIsShared(ctx, deps, storage); known && shared {
		nodeLogger.Info("create_stemcell: server-download replication: stemcell storage is shared; skipping per-node download")
		return buildServerDownloadReplica(ctx, deps, nodeLogger, node, storage, qcow2Filename,
			sha256hex, sourceURL, stemcellCID, creatingDirectorUUID, cp, nil)
	}

	params := &sdknodes.CreateStorageDownloadUrlParams{
		Content:  pveStorageContentImport,
		Filename: qcow2Filename,
		Url:      sourceURL,
	}
	if sha256hex != "" {
		checksum := sha256hex
		algo := jsonKeySHA256
		params.Checksum = &checksum
		params.ChecksumAlgorithm = &algo
	}

	// cleanupOnFailure mirrors replicateOneNode's storage-classification
	// guard: only delete the just-downloaded volume when this storage is
	// positively
	// classified node-local. On shared storage — or an unclassifiable result
	// — "import/<file>" can be the SAME file the primary node's returned CID
	// names.
	cleanupOnFailure := func(filename string) {
		shared, known := stemcellStorageIsShared(ctx, deps, storage)
		if !known || shared {
			nodeLogger.Warn("create_stemcell: server-download replication: skipping download cleanup — " +
				"storage is shared or its classification is unknown")
			return
		}
		volumePath := "import/" + filename
		if _, delErr := deps.PVE.Storage().DeleteVolumeIfExists(ctx, node, storage, volumePath); delErr != nil {
			nodeLogger.Warn("create_stemcell: server-download replication: cleanup of failed download also failed (non-fatal)",
				log.Err(delErr),
			)
		}
	}

	resp, dlErr := deps.PVE.Nodes().CreateStorageDownloadUrl(ctx, node, storage, params)
	if dlErr != nil {
		nodeLogger.Warn("create_stemcell: server-download replication: download failed (non-fatal; replica not created)",
			log.Err(dlErr),
		)
		return replicaOutcome{Node: node, Stage: "download", Err: dlErr}
	}
	if resp != nil && len(*resp) > 0 {
		upid, upidErr := pve.UPIDFromRaw(*resp)
		if upidErr != nil {
			nodeLogger.Warn("create_stemcell: server-download replication: cannot parse task UPID (non-fatal; replica not created)",
				log.Err(upidErr),
			)
			return replicaOutcome{Node: node, Stage: "download", Err: upidErr}
		}
		if upid != "" {
			if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, nodeLogger,
				pve.WithMaxWait(pve.StemcellMaxWait)); awaitErr != nil {
				nodeLogger.Warn("create_stemcell: server-download replication: download task failed (non-fatal; replica not created)",
					log.Err(awaitErr),
				)
				cleanupOnFailure(qcow2Filename)
				return replicaOutcome{Node: node, Stage: "download", Err: awaitErr}
			}
		}
	}

	// PVE may normalize the requested filename (mirrors handleStemcellDownloadURL
	// step 6); confirm the actual volume name before building the template.
	actualFilename := qcow2Filename
	if foundVol, findErr := pve.FindStemcellByFilename(ctx, deps.PVE, node, storage, qcow2Filename); findErr == nil && foundVol == "" {
		prefix := stemcellDownloadURLPrefix(cp.Name, cp.Version)
		if prefixVol, prefixErr := fetchFindByPrefix(ctx, deps, node, storage, prefix); prefixErr == nil && prefixVol != "" {
			if name := fetchExtractFilename(prefixVol); name != "" {
				actualFilename = name
			}
		}
	}

	return buildServerDownloadReplica(ctx, deps, nodeLogger, node, storage, actualFilename,
		sha256hex, sourceURL, stemcellCID, creatingDirectorUUID, cp, cleanupOnFailure)
}
