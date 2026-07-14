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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	stemcellfetch "github.com/fivetwenty-io/bosh-pve-cpi/internal/pve/stemcell_fetch"

	sdkclusterstorage "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	sdkclient "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/client"
)

// MaxStemcellTotalExtract caps cumulative extracted bytes from a stemcell
// tarball. A stemcell tarball contains one disk image (typically 3-10 GiB
// compressed to ~700 MiB) plus small metadata files. 32 GiB is a hard upper
// bound above any real stemcell; exceeding it indicates a tar-bomb or
// corrupted archive.
const MaxStemcellTotalExtract = 32 * 1024 * 1024 * 1024 // 32 GiB

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

// pveDiskFormat translates an advertised disk_format value (as reported by
// info.go's stemcell_formats list, e.g. "openstack-qcow2", "general-raw",
// "pve-qcow2") into the PVE-native enum accepted by qm's scsi[n] format=
// parameter ({qcow2, raw, vmdk}).
//
// Empty input yields empty output so callers can fall back to magic-byte
// detection. Unknown inputs also yield empty output so a bad metadata value
// surfaces as a detection-fallback rather than a silent PVE API rejection.
func pveDiskFormat(advertised string) string {
	switch strings.ToLower(advertised) {
	case "":
		return ""
	case diskFormatQCOW2, "openstack-qcow2", "general-qcow2", "pve-qcow2":
		return diskFormatQCOW2
	case "raw", "openstack-raw", "general-raw", "pve-raw":
		return diskFormatRaw
	case "vmdk":
		return "vmdk"
	default:
		return ""
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
	if v, ok := cp["sha256"].(string); ok {
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
// Returns: stemcell_cid string — "template:<vmid>" (e.g. "template:6042").
// The VMID identifies the frozen PVE template VM that backs this stemcell.
// Internally the qcow2 is uploaded to "<storage>:import/<filename>" first,
// then imported into the template VM; the upload volume is deleted after the
// template is frozen (for CPI-owned images) or retained (for operator-preuploaded
// light stemcells).
//
// All three stemcell paths (heavy tarball, light-preuploaded, light-fetch) build
// a frozen template VM and return "template:<vmid>". The "light:" CID prefix only
// appears in stemcell_cid values produced before this feature was introduced.
//
// Template creation is idempotent: if a template VM named
// "bosh-stemcell-<name>-<version>" already exists in the template VMID range,
// its VMID is reused and the upload is skipped.
//
// Twelve-step flow:
//
//  1. Validate args[0] image_path.
//  2. Parse cloud_properties → stemcellCloudProps.
//  3. Validate cloud_properties.name and cloud_properties.version are non-empty (required for template name and CID).
//  4. Determine storage: config.StemcellStorage (falls back to config.VMStorage if empty).
//  5. Validate storage is shared (required for multi-node clusters).
//  6. Extract/resolve the disk image from the tarball if needed.
//  7. Compute SHA-256 of the disk image.
//  8. Build qcow2 filename from name/version/sha8.
//  9. Dedup: FindTemplateByName — reuse existing template VMID and return "template:<vmid>" if found.
//  10. Upload qcow2 to storage; await task with StemcellMaxWait.
//  11. Import qcow2 into a new template VM; freeze it (MakeTemplate); tag with bosh-stemcell-sha-<sha8>.
//  12. Return "template:<vmid>".
//
// PVE's content APIs do not accept arbitrary metadata for import volumes
// (the upload endpoint validates file extension; the notes endpoint is
// backup-only). Stemcell identity is therefore carried entirely by the
// qcow2 filename — which encodes name, version, and sha8 — together with
// the cloud_properties record stored on the BOSH Director side.
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
//
// nolint:gocognit,gocyclo // Orchestration shell: light-vs-heavy dispatch then heavy-path phases (resolveStemcellStorageAndNode, buildAndDeduplicateStemcellCID, uploadAndReturnCID). Phase logic lives in extracted helpers. CPI v3 env-arg parsing adds one branch to the already-high count.
func HandleCreateStemcell(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, _ jsonrpc.Context) (any, error) {
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
			return handleLightStemcellPreUploaded(ctx, deps, cp)
		case "fetch":
			return handleLightStemcellFetch(ctx, deps, cp)
		case "server-download":
			return handleStemcellDownloadURL(ctx, deps, cp)
		}

		// Steps 4-5: Resolve target node+storage and validate shared constraint.
		node, storage, resolveErr := resolveStemcellStorageAndNode(ctx, deps)
		if resolveErr != nil {
			return nil, resolveErr
		}

		// Steps 6-10: Extract/hash image, build CID, dedup check.
		// Cleanup is the tmpDir teardown for the (possibly extracted) image; the
		// caller owns its lifetime so the upload step can reuse the same path.
		// sha256hex is returned alongside the CID so the template tag can be set
		// without a second file-read pass.
		dedup, buildErr := buildAndDeduplicateStemcellCID(
			ctx, deps, node, storage, imagePath, cp, deps.Log(ctx))
		if buildErr != nil {
			return nil, buildErr
		}
		defer dedup.Cleanup()

		stemcellCID := dedup.CID
		found := dedup.Found
		uploadSourcePath := dedup.UploadSourcePath
		sha256hex := dedup.SHA256Hex
		qcow2Filename := strings.TrimPrefix(stemcellCID, storage+":import/")
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

		if found {
			// Dedup: qcow2 already on storage. Build template from it (idempotent).
			// CPI does NOT own this pre-existing qcow2 → cpiOwnsSource=false so the
			// source is not deleted (it may be shared with other stemcell records).
			vmid, tmplErr := ensureTemplateVM(ctx, deps, templateNode, storage, qcow2Filename, sha256hex, false, cp, imagePath)
			if tmplErr != nil {
				return nil, fmt.Errorf("create_stemcell: ensure template (dedup path): %w", tmplErr)
			}

			// Per-node replication on dedup path: re-running create_stemcell when the
			// primary already exists must still converge any missing per-node replicas.
			// Only applicable when sha256hex is known (required for replica idempotency check).
			if deps.Config.StemcellReplicateLocal && sha256hex != "" {
				clusterNodes, listErr := listClusterNodes(ctx, deps)
				if listErr != nil {
					deps.Log(ctx).Warn("create_stemcell: replication (dedup): cannot list cluster nodes (skipping)",
						log.Err(listErr),
					)
				} else if len(clusterNodes) > 1 {
					uploadStagingDir := ""
					if uploadSourcePath == imagePath {
						uploadStagingDir = deps.Config.StemcellStagingDir
					}
					replicateStemcellToNodes(ctx, deps, templateNode, storage, qcow2Filename,
						sha256hex, clusterNodes, uploadSourcePath, uploadStagingDir, cp, imagePath)
				}
			}

			return pve.BuildTemplateStemcellCID(vmid), nil
		}

		// Step 11: Upload qcow2 to storage. uploadSourcePath is reused so the
		// tarball case extracts only once.
		if _, uploadErr := uploadAndReturnCID(ctx, deps, node, storage, imagePath, uploadSourcePath, cp, stemcellCID, deps.Log(ctx)); uploadErr != nil {
			return nil, uploadErr
		}

		// Step 12: Build template VM from the freshly uploaded qcow2.
		// heavy path: CPI uploaded the qcow2 → cpiOwnsSource=true; ensureTemplateVM
		// deletes it after freeze (reclaims storage; template disk is the live copy).
		vmid, tmplErr := ensureTemplateVM(ctx, deps, templateNode, storage, qcow2Filename, sha256hex, true, cp, imagePath)
		if tmplErr != nil {
			return nil, fmt.Errorf("create_stemcell: ensure template: %w", tmplErr)
		}

		// Per-node replication (opt-in, default off). When stemcell_replicate_local
		// is true and the storage is local, replicate the template to all other
		// cluster nodes. This is a best-effort fire-and-forget: individual node
		// failures are logged as warnings and do not fail create_stemcell.
		if deps.Config.StemcellReplicateLocal {
			clusterNodes, listErr := listClusterNodes(ctx, deps)
			if listErr != nil {
				deps.Log(ctx).Warn("create_stemcell: replication: cannot list cluster nodes (skipping replication)",
					log.Err(listErr),
				)
			} else if len(clusterNodes) > 1 {
				uploadStagingDir := ""
				if uploadSourcePath == imagePath {
					uploadStagingDir = deps.Config.StemcellStagingDir
				}
				replicateStemcellToNodes(ctx, deps, templateNode, storage, qcow2Filename,
					sha256hex, clusterNodes, uploadSourcePath, uploadStagingDir, cp, imagePath)
			}
		}

		return pve.BuildTemplateStemcellCID(vmid), nil
	})
}

// ensureTemplateVM builds or reuses a frozen PVE template VM for a stemcell.
//
// Sequence:
//  1. BuildTemplateName(cp.Name, cp.Version) → deterministic name for idempotency.
//  2. FindTemplateByName on templateNode — if found, return existing VMID immediately.
//     Source qcow2 is NOT deleted on the reuse path regardless of cpiOwnsSource.
//  3. Allocate a new VMID in [config.StemcellTemplateVMIDRangeStart, …End] via
//     AllocateWithRetry, same retry/conflict pattern as create_vm.
//  4. QEMU().Create with import-from=<storage>:import/<qcow2Filename>; no NIC,
//     no agent, onboot=0; tag "bosh-stemcell-sha-<sha8>" where sha8 = first 8 chars
//     of sha256hex; await UPID with StemcellMaxWait.
//  5. MakeTemplate → freeze VM; await UPID if non-empty.
//  6. Pool assignment (D config): if deps.Config.StemcellTemplatePool != "", assign
//     the frozen template VM to that PVE resource pool via AssignVMToPool.
//     Pool assignment failure is fatal — the operator explicitly requested the pool
//     and a missing pool means a misconfiguration that must be visible immediately.
//     The template VMID is still usable if the caller handles the error separately,
//     but this function returns the error so the operator sees it right away.
//  7. Source retention: if cpiOwnsSource, delete the raw qcow2 at
//     <storage>:import/<qcow2Filename> via Storage().DeleteVolumeIfExists. Delete
//     failure is logged as a warning and is not fatal — CID is returned regardless.
//     If !cpiOwnsSource (light-preuploaded) the source is never deleted.
//
// Returns the VMID of the frozen template on success.
//
// Error contract:
//   - FindTemplateByName API failure → wrapped error returned.
//   - AllocateWithRetry exhausted → error returned.
//   - QEMU.Create failure → error returned (cleanup attempted inside AllocateWithRetry retry).
//   - MakeTemplate failure → error returned (template not safe to use; source NOT deleted).
//   - AssignVMToPool failure (when StemcellTemplatePool != "") → error returned (fatal misconfiguration).
//   - qcow2 delete failure (cpiOwnsSource=true) → warning logged, vmid still returned.
//
//nolint:gocognit // Multi-step allocation+freeze+cleanup; phases are load-bearing and cannot be further decomposed without losing clarity.
func ensureTemplateVM(
	ctx context.Context,
	deps Deps,
	templateNode, storage, qcow2Filename, sha256hex string,
	cpiOwnsSource bool,
	cp stemcellCloudProps,
	source string,
) (vmid int64, err error) {
	logger := deps.Log(ctx)

	// Step 1: Build deterministic template name (idempotency key).
	templateName := pve.BuildTemplateName(cp.Name, cp.Version)

	// Step 2a: Prefer the stable sha-tag identity. The bosh-stemcell-sha-<sha8>
	// tag is derived from the disk content, so it survives changes to the
	// template-name derivation (e.g. the dot→dash DNS-safe rename) that would
	// otherwise orphan an identical-disk template and create a duplicate. Only
	// attempted when sha256hex is known — the light-preuploaded path has no
	// local image to hash and falls through to the name lookup below.
	if sha256hex != "" {
		shaTag8 := sha256hex
		if len(shaTag8) > 8 {
			shaTag8 = shaTag8[:8]
		}
		shaVMID, shaFound, shaErr := pve.FindTemplateBySHATag(ctx, deps.PVE, templateNode, shaTag8)
		if shaErr != nil {
			return 0, fmt.Errorf("ensureTemplateVM: lookup existing template by sha tag %q: %w", shaTag8, shaErr)
		}
		if shaFound {
			logger.Info("ensureTemplateVM: reusing existing template (matched by sha tag)",
				log.String("sha8", shaTag8),
				log.Int64(metadataKeyVMID, shaVMID),
			)
			registerStemcellRef(ctx, deps, logger, templateNode, shaVMID)
			return shaVMID, nil
		}
	}

	// Step 2b: Fall back to the deterministic name. Covers the light-preuploaded
	// path (no sha available) and any template created before sha tagging.
	existingVMID, found, findErr := pve.FindTemplateByName(ctx, deps.PVE, templateNode, templateName)
	if findErr != nil {
		return 0, fmt.Errorf("ensureTemplateVM: lookup existing template %q: %w", templateName, findErr)
	}
	if found {
		logger.Info("ensureTemplateVM: reusing existing template",
			log.String("name", templateName),
			log.Int64(metadataKeyVMID, existingVMID),
		)
		registerStemcellRef(ctx, deps, logger, templateNode, existingVMID)
		return existingVMID, nil
	}

	// Step 3-4: Allocate VMID + create VM with import-from.
	sha8 := sha256hex
	if len(sha8) > 8 {
		sha8 = sha8[:8]
	}
	shaTag := "bosh-stemcell-sha-" + sha8
	// import-from volid: "<storage>:import/<filename>"
	importVolid := storage + ":import/" + qcow2Filename

	isRetryable := func(e error) bool {
		return pve.IsVMIDConflict(e) || pve.IsStorageLockTimeout(e) || pve.IsTransientTransport(e)
	}

	rangeStart := deps.Config.StemcellTemplateVMIDRangeStart
	rangeEnd := deps.Config.StemcellTemplateVMIDRangeEnd

	allocatedRaw, allocErr := pve.AllocateWithRetry(ctx, deps.PVE,
		func(candidate int) error {
			return attemptCreateTemplateVM(ctx, deps, logger, templateNode, candidate, templateName, importVolid, shaTag, deps.Config.VMStorage, cp, source, nil)
		},
		isRetryable,
		0, // use AllocateWithRetry default (3 attempts)
		pve.WithRange(rangeStart, rangeEnd),
	)
	if allocErr != nil {
		return 0, fmt.Errorf("ensureTemplateVM: allocate+create template VM: %w", allocErr)
	}
	allocatedVMID := int64(allocatedRaw)

	logger.Info("ensureTemplateVM: template VM created, freezing",
		log.String("name", templateName),
		log.Int64(metadataKeyVMID, allocatedVMID),
	)

	// Step 5: Freeze the VM into a PVE template.
	freezeUPID, freezeErr := pve.MakeTemplate(ctx, deps.PVE, templateNode, allocatedVMID)
	if freezeErr != nil {
		return 0, fmt.Errorf("ensureTemplateVM: freeze template vmid=%d: %w", allocatedVMID, freezeErr)
	}
	if freezeUPID != "" {
		if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, templateNode, freezeUPID, logger,
			pve.WithMaxWait(pve.StemcellMaxWait)); awaitErr != nil {
			return 0, fmt.Errorf("ensureTemplateVM: await freeze task vmid=%d upid=%s: %w",
				allocatedVMID, freezeUPID, awaitErr)
		}
	}

	logger.Info("ensureTemplateVM: template frozen",
		log.String("name", templateName),
		log.Int64(metadataKeyVMID, allocatedVMID),
	)

	// Step 5b: Race reconciliation. The Step-2 dedup lookup and this freeze are
	// not atomic: a concurrent create_stemcell for the same stemcell can pass its
	// own lookup (seeing no frozen template, because ours was not yet frozen) and
	// create a second template in the gap. Now that our template is frozen — and
	// therefore visible to every scanner — re-scan and converge on the lowest
	// VMID. If an older (lower-VMID) twin exists, we lost the race: delete the
	// template we just created and return the survivor. This makes concurrent
	// create_stemcell calls idempotent without cross-process locking.
	winnerVMID := allocatedVMID
	lostRace := false
	if survivor, recErr := reconcileTemplateRace(ctx, deps, templateNode, templateName, sha256hex); recErr != nil {
		// Non-fatal: a failed re-scan leaves our freshly-frozen template in place.
		// A later create_stemcell will reconcile via the Step-2 lookup.
		logger.Warn("ensureTemplateVM: race reconcile scan failed (non-fatal; keeping new template)",
			log.Int64(metadataKeyVMID, allocatedVMID),
			log.Err(recErr),
		)
	} else if survivor != 0 && survivor < allocatedVMID {
		lostRace = true
		winnerVMID = survivor
		logger.Info("ensureTemplateVM: lost create race, deleting duplicate and reusing survivor",
			log.Int64("deleted_vmid", allocatedVMID),
			log.Int64("survivor_vmid", survivor),
		)
		if delErr := deleteTemplateVM(ctx, deps, templateNode, allocatedVMID, logger); delErr != nil {
			logger.Warn("ensureTemplateVM: failed to delete duplicate template after lost race (non-fatal)",
				log.Int64(metadataKeyVMID, allocatedVMID),
				log.Err(delErr),
			)
		}
	}

	// Step 6: Pool assignment — assign the frozen template to the configured
	// PVE resource pool when StemcellTemplatePool is set. Pool assignment uses
	// PUT /pools/{poolid} with vms=[vmid] via pve.AssignVMToPool.
	//
	// Failure is fatal: the operator explicitly named a pool; a missing pool or
	// auth failure indicates misconfiguration that must surface immediately rather
	// than leaving a template silently outside the expected pool. The template VM
	// was already frozen and is usable, but returning an error ensures the CPI
	// reports a clear failure so the operator can fix the config and retry (the
	// idempotency check in step 2 will reuse the existing template on the next call).
	// Skipped when we lost the race: the survivor was already pool-assigned by
	// the call that created it, and our template is being deleted.
	if !lostRace && deps.Config.StemcellTemplatePool != "" {
		if poolErr := pve.AssignVMToPool(ctx, deps.PVE, deps.Config.StemcellTemplatePool, allocatedVMID); poolErr != nil {
			return 0, fmt.Errorf("ensureTemplateVM: assign template vmid=%d to pool %q: %w",
				allocatedVMID, deps.Config.StemcellTemplatePool, poolErr)
		}
		logger.Info("ensureTemplateVM: template assigned to pool",
			log.String("pool", deps.Config.StemcellTemplatePool),
			log.Int64(metadataKeyVMID, allocatedVMID),
		)
	}

	// Step 8: retention — delete source qcow2 only when the CPI owns it.
	// DeleteVolumeIfExists takes the storage pool and the volume PATH component
	// ("import/<file>") as separate args — same contract delete_stemcell uses.
	// Passing the full "<storage>:import/<file>" volid here double-prefixes the
	// storage and the volume is never matched (silent best-effort no-op).
	if cpiOwnsSource {
		volumePath := "import/" + qcow2Filename
		_, delErr := deps.PVE.Storage().DeleteVolumeIfExists(ctx, templateNode, storage, volumePath)
		if delErr != nil {
			logger.Warn("ensureTemplateVM: best-effort qcow2 delete failed (non-fatal)",
				log.String("volume", volumePath),
				log.Err(delErr),
			)
		} else {
			logger.Info("ensureTemplateVM: source qcow2 deleted",
				log.String("volume", volumePath),
			)
		}
	}

	return winnerVMID, nil
}

// reconcileTemplateRace returns the lowest VMID of a frozen template matching
// the stemcell identity, used after freeze to detect a concurrently-created
// duplicate. It prefers the stable sha tag (when sha256hex is known) and falls
// back to the deterministic name. A return of (0, nil) means no matching
// template was visible — treated by the caller as "no duplicate".
func reconcileTemplateRace(ctx context.Context, deps Deps, templateNode, templateName, sha256hex string) (int64, error) {
	if sha256hex != "" {
		sha8 := sha256hex
		if len(sha8) > 8 {
			sha8 = sha8[:8]
		}
		vmid, found, err := pve.FindTemplateBySHATag(ctx, deps.PVE, templateNode, sha8)
		if err != nil {
			return 0, err
		}
		if found {
			return vmid, nil
		}
		return 0, nil
	}

	vmid, found, err := pve.FindTemplateByName(ctx, deps.PVE, templateNode, templateName)
	if err != nil {
		return 0, err
	}
	if found {
		return vmid, nil
	}
	return 0, nil
}

// deleteTemplateVM destroys a template VM (purge + destroy unreferenced disks)
// and awaits the destroy task. A not-found result is treated as success: the
// VM is already gone, which is the desired end state. Used by the race-reconcile
// path to remove a duplicate template after losing a concurrent create.
func deleteTemplateVM(ctx context.Context, deps Deps, node string, vmid int64, logger *log.Logger) error {
	purge := true
	destroyDisks := true
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

// attemptCreateTemplateVM builds CreateQemuParams for a minimal template VM and
// calls QEMU().Create + await. Called on each AllocateWithRetry attempt. Returns
// an error on any failure so AllocateWithRetry can retry on conflict.
//
// Template VM characteristics (differs from create_vm):
//   - No NIC (net0 absent) — templates carry only the root disk.
//   - No QEMU guest agent — agent=0 (template is frozen; agent not needed).
//   - onboot=0 — templates must not auto-start.
//   - virtio0: import-from= with format=qcow2 and size=5G default.
//   - Tags: "bosh-stemcell-sha-<sha8>" for content-based template dedup lookup.
//
// shaTag must be the pure "bosh-stemcell-sha-<sha8>" tag for the primary path.
// extraBaseTags holds any additional identity tags (e.g. the per-node replica
// tag) that belong in the base set alongside shaTag regardless of provenance
// mode. When nil, only shaTag appears in the base set.
//
// When deps.Config.StemcellProvenanceEnabled() is true, additional provenance
// tags (marker, name, version, director) are merged into the tags field and a
// JSON provenance block is written to the description field. When disabled the
// createParams are byte-identical to the pre-provenance behaviour: tags equals
// shaTag for the primary path and "shaTag;extraBaseTags[0]" for the replica.
//
// cp and source are used only when provenance is enabled. source is the
// human-readable origin label (image_path, image_id, or image_url).
func attemptCreateTemplateVM(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	node string,
	candidate int,
	templateName, importVolid, shaTag, targetStorage string,
	cp stemcellCloudProps,
	source string,
	extraBaseTags []string,
) error {
	// virtio0: allocate the template's root disk on targetStorage (the VM/images
	// storage). PVE requires the "<storage>:<size>" form — a bare "0" is parsed
	// as a volume ID and rejected ("unable to parse volume ID '0'"). targetStorage
	// MUST support the "images" content type and is intentionally distinct from
	// the import-from source (importVolid lives on StemcellStorage, which only
	// needs "import"); no single PVE storage need support both — mirrors the
	// create_vm import-from path. size=5G matches defaultStemcellDiskGiB; PVE
	// will not shrink below the imported image's actual size.
	virtio0Val := fmt.Sprintf("%s:0,import-from=%s,format=%s,size=%dG",
		targetStorage, importVolid, diskFormatQCOW2, defaultStemcellDiskGiB)

	// baseTags is the ordered set of identity tags that always appear in the
	// template's tags field regardless of provenance mode.
	// ownershipTag ("bosh-cpi") is prepended so operators can filter PVE UI /
	// scripts to CPI-managed templates only. For the primary path the
	// remaining tags are [shaTag]; for replicas the per-node tag is appended.
	baseTags := make([]string, 0, 2+len(extraBaseTags))
	baseTags = append(baseTags, ownershipTag, shaTag)
	baseTags = append(baseTags, extraBaseTags...)

	createParams := map[string]any{
		metadataKeyVMID: candidate,
		metadataKeyName: templateName,
		"ostype":        osTypeLinux26,
		"scsihw":        "virtio-scsi-pci",
		diskKeyVirtio0:  virtio0Val,
		"boot":          "order=" + diskKeyVirtio0,
		"agent":         "enabled=0",
		"onboot":        0,
		jsonKeyTags:          strings.Join(baseTags, ";"),
	}

	// initialCID is the BOSH stemcell CID for this template ("template:<vmid>").
	// Written into stemcell_refs so delete_stemcell can track reference counts.
	// Computed here where the candidate VMID is known.
	initialCID := pve.BuildTemplateStemcellCID(int64(candidate))

	// director tag tokens: sanitize key and value; build "key-value" tokens;
	// drop any token where either side sanitizes to "".
	var directorTagTokens []string
	if len(cp.DirectorTags) > 0 {
		for k, v := range cp.DirectorTags {
			sk := sanitizeTagValue(k)
			sv := sanitizeTagValue(v)
			if sk == "" || sv == "" {
				continue
			}
			directorTagTokens = append(directorTagTokens, sk+"-"+sv)
		}
	}

	if deps.Config.StemcellProvenanceEnabled() {
		// sha8 is derived from shaTag which is always the pure
		// "bosh-stemcell-sha-<sha8>" tag; TrimPrefix is safe here.
		sha8 := strings.TrimPrefix(shaTag, stemcellSHATagPrefix)
		notes, notesErr := buildStemcellProvenanceNotes(cp, sha8, source, deps.Config.StemcellDirectorID(), time.Now().UTC(), initialCID, cp.DirectorTags)
		if notesErr != nil {
			logger.Warn("attemptCreateTemplateVM: provenance notes build failed (skipping description)",
				log.Err(notesErr),
			)
		} else {
			createParams["description"] = notes
		}
		provTags := buildStemcellProvenanceTags(cp, deps.Config.StemcellDirectorID())
		allTags := mergeTagList(baseTags, provTags, 0)
		createParams[jsonKeyTags] = mergeTagList(strings.Split(allTags, ";"), directorTagTokens, maxTagLength)
	} else {
		// Even when full provenance is disabled, write a minimal notes JSON that
		// records stemcell_refs so delete_stemcell can gate template destruction on
		// reference count. This keeps byte-identical behaviour for all existing
		// fields while adding the new ref-tracking capability.
		minimalNotes, notesErr := json.Marshal(stemcellProvenance{StemcellRefs: initialCID})
		if notesErr != nil {
			logger.Warn("attemptCreateTemplateVM: minimal refs notes build failed (skipping description)",
				log.Err(notesErr),
			)
		} else {
			createParams["description"] = string(minimalNotes)
		}
		// Merge director tag tokens into base tags regardless of provenance mode.
		if len(directorTagTokens) > 0 {
			existingTagStr, _ := createParams[jsonKeyTags].(string)
			createParams[jsonKeyTags] = mergeTagList(strings.Split(existingTagStr, ";"), directorTagTokens, maxTagLength)
		}
	}

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
				log.String("error", cerr.Error()),
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
					log.String("error", werr.Error()),
				)
			}
			return werr
		}
	}

	logger.Info("ensureTemplateVM: VM disk imported",
		log.Int("vmid_attempted", candidate),
		log.String("upid", upid),
	)
	return nil
}

// resolveStemcellStorageAndNode resolves the target PVE node and storage for a
// heavy-stemcell upload (steps 4-5 of the eleven-step flow).
//
// node comes from deps.Config.Node (required; empty is a cloud error).
// storage is deps.Config.StemcellStorage with a fallback to VMStorage (both
// empty is a cloud error). After the storage name is determined,
// validateStemcellStorageShared enforces that local-only storage is rejected
// when the cluster has more than one node.
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

	if validateErr := validateStemcellStorageShared(ctx, deps, storage); validateErr != nil {
		return "", "", validateErr
	}
	return node, storage, nil
}

// stemcellDedupResult bundles the outputs of buildAndDeduplicateStemcellCID
// into a single struct, keeping the return list under the 5-result linter limit.
type stemcellDedupResult struct {
	// CID is the stemcell content-ID (<storage>:import/<filename>).
	CID string
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
// compute SHA-256, build the deterministic qcow2 filename and CID, then check
// whether that volume already exists in PVE storage.
//
// When Found is true, CID is the existing CID and the caller must return it
// immediately without uploading. When Found is false, CID is the one to be
// created by uploadAndReturnCID, and UploadSourcePath points at the resolved
// image (already extracted for tarball inputs) so the upload step can reuse it
// without a second extraction pass. SHA256Hex carries the digest computed
// during image resolution so callers never re-read the multi-GiB image.
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
	uploadSourcePath, cleanupExtract, detectedFormat, extractedSHA, detectErr := resolveStemcellImage(
		imagePath, cp.DiskFormat, deps.Config.StemcellStagingDir, logger)
	if detectErr != nil {
		return fail(cpierrors.Wrap(detectErr, "create_stemcell: resolve image"))
	}

	// User-supplied disk_format wins when present; aliases like
	// "openstack-qcow2" or "general-raw" are translated to PVE-native
	// enum (qcow2/raw/vmdk). Unknown aliases fall back to magic-byte detection.
	// uploadFormat is resolved here for future use when the upload API gains
	// a format= parameter; PVE currently infers format from file content.
	_ = func() string {
		if f := pveDiskFormat(cp.DiskFormat); f != "" {
			return f
		}
		return detectedFormat
	}()

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
			// (the upload uses the extracted .img, not the tarball). But it
			// means we cannot verify the expected digest, so warn and skip
			// verification rather than blocking a valid stemcell upload.
			logger.Warn("create_stemcell: cannot compute tarball sha256 for digest verification (skipping check)",
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

	// Steps 8-9: Build filename and CID.
	qcow2Filename := pve.BuildStemcellFilename(cp.Name, cp.Version, sha256hex)
	cid := pve.BuildStemcellCID(storage, qcow2Filename)

	logger.Info("create_stemcell: resolved filenames",
		log.String("qcow2", qcow2Filename),
		log.String("cid", cid),
		log.String("sha256", sha256hex),
	)

	// Step 10: Dedup — skip upload if volume already present.
	existing, findErr := pve.FindStemcellByFilename(ctx, deps.PVE, node, storage, qcow2Filename)
	if findErr != nil {
		cleanupExtract()
		return fail(cpierrors.Wrap(findErr, "create_stemcell: dedup lookup"))
	}
	if existing != "" {
		logger.Info("create_stemcell: stemcell already present, returning existing CID",
			log.String("cid", existing),
			log.String("name", cp.Name),
			log.String("version", cp.Version),
		)
		return stemcellDedupResult{
			CID:              existing,
			Found:            true,
			UploadSourcePath: uploadSourcePath,
			Cleanup:          cleanupExtract,
			SHA256Hex:        sha256hex,
		}, nil
	}

	return stemcellDedupResult{
		CID:              cid,
		Found:            false,
		UploadSourcePath: uploadSourcePath,
		Cleanup:          cleanupExtract,
		SHA256Hex:        sha256hex,
	}, nil
}

// uploadAndReturnCID covers step 11 of the eleven-step flow: upload the qcow2
// image to PVE storage and return the canonical stemcell CID.
//
// stemcellCID must be the value returned by buildAndDeduplicateStemcellCID when
// found was false; it encodes storage, name, version, and sha8. imagePath is the
// director-supplied local path and is used to set the upload staging-dir scope
// and to log the source for observability. cp provides name and version for the
// final "stemcell ready" log line.
//
// The uploadStagingDir passed to uploadStemcellImage is set only when
// uploadSourcePath equals imagePath (bare qcow2 passthrough). When
// resolveStemcellImage extracted the image into a CPI-owned tmpDir the source
// path differs from imagePath and no staging-dir scoping applies.
func uploadAndReturnCID(
	ctx context.Context,
	deps Deps,
	node, storage, imagePath, uploadSourcePath string,
	cp stemcellCloudProps,
	stemcellCID string,
	logger *log.Logger,
) (string, error) {
	// Re-derive qcow2 filename from the CID; BuildStemcellCID produces
	// "<storage>:import/<filename>" so strip the prefix.
	prefix := storage + ":import/"
	qcow2Filename := strings.TrimPrefix(stemcellCID, prefix)

	// uploadSourcePath was resolved by buildAndDeduplicateStemcellCID; its
	// underlying tmpDir is kept alive by the caller-owned cleanup deferred in
	// HandleCreateStemcell. No second extraction is needed.

	uploadStagingDir := ""
	if uploadSourcePath == imagePath {
		uploadStagingDir = deps.Config.StemcellStagingDir
	}
	if uploadErr := uploadStemcellImage(ctx, deps, node, storage, qcow2Filename, uploadSourcePath, uploadStagingDir); uploadErr != nil {
		return "", cpierrors.Wrap(uploadErr, "create_stemcell: upload qcow2")
	}
	logger.Info("create_stemcell: qcow2 uploaded",
		log.String("volume", stemcellCID),
		log.String("source", imagePath),
	)

	// Source of truth for stemcell identity is the qcow2 filename
	// (encodes name/version/sha8) plus state held by the BOSH Director
	// (name, version, cloud_properties on the stemcell record). PVE's
	// content APIs don't accept arbitrary metadata for import volumes
	// (uploads validate file extension; notes are backup-only), so no
	// sidecar or volume-level annotation is written here.

	logger.Info("create_stemcell: stemcell ready",
		log.String("stemcell_cid", stemcellCID),
		log.String("name", cp.Name),
		log.String("version", cp.Version),
	)
	return stemcellCID, nil
}

// handleLightStemcellPreUploaded resolves a pre-uploaded light stemcell:
// validates the operator-supplied image_id (PVE volid), applies the storage
// policy, confirms the qcow2 is present on PVE, and returns the canonical
// light: CID. The CPI never uploads, deletes, or rewrites the underlying
// volume; the operator owns its lifecycle.
func handleLightStemcellPreUploaded(
	ctx context.Context,
	deps Deps,
	cp stemcellCloudProps,
) (any, error) {
	// 1. Parse image_id as a volid: "<storage>:import/<file>".
	// Operator may include the "light:" prefix — strip it to be forgiving.
	// ParseStemcellCID enforces canonical form below.
	imageID := cp.ImageID
	rawImageID := pve.StripLightPrefix(imageID)
	storage, volumePath, parseErr := pve.ParseStemcellCID(rawImageID)
	if parseErr != nil {
		return nil, cpierrors.Cloud(
			"create_stemcell: cloud_properties.image_id %q is not a valid storage volid (expected \"<storage>:import/<file>\"): %s",
			imageID, parseErr.Error())
	}

	// 2. Apply storage policy via ValidateLightStemcellStorage.
	policyDeps := newHandlerPolicyDeps(deps)
	chosenNode, policyErr := pve.ValidateLightStemcellStorage(ctx, policyDeps, storage, cp.Node)
	if policyErr != nil {
		return nil, policyErr
	}

	// 4. Resolve the node to query for existence. chosenNode wins when non-empty;
	// fall back to config.Node (existing handler invariant: non-empty for normal path).
	node := chosenNode
	if node == "" && deps.Config != nil {
		node = deps.Config.Node
	}
	if node == "" {
		return nil, cpierrors.Cloud("create_stemcell: config.node must not be empty")
	}

	// 5. Existence check — qcow2 filename is the trailing segment after "import/".
	// volumePath has the form "import/<file>" per ParseStemcellCID contract.
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
				"operator must upload via pvesm or PVE Upload API before referencing",
			imageID, storage, node)
	}

	// 6. Build template VM from the operator-placed qcow2.
	// light-preuploaded: CPI does NOT own this qcow2 → cpiOwnsSource=false
	// (operator placed it; must not be deleted). sha256hex is unavailable
	// at this point (no local file, no stream); pass "" so the template tag
	// field is empty (the template name+idempotency key is the lookup anchor).
	// templateNode: StemcellTemplateNode falls back to config.Node.
	templateNode := node
	if deps.Config != nil && deps.Config.StemcellTemplateNode != "" {
		templateNode = deps.Config.StemcellTemplateNode
	}
	vmid, tmplErr := ensureTemplateVM(ctx, deps, templateNode, storage, qcow2Filename, "", false, cp, cp.ImageID)
	if tmplErr != nil {
		return nil, fmt.Errorf("create_stemcell: light pre-uploaded: ensure template: %w", tmplErr)
	}

	templateCID := pve.BuildTemplateStemcellCID(vmid)
	deps.Log(ctx).Info("create_stemcell: light stemcell (pre-uploaded) template ready",
		log.String("image_id", imageID),
		log.String("storage", storage),
		log.String("node", node),
		log.String("cid", templateCID),
	)
	return templateCID, nil
}

// resolveFetchSource returns the source and reference for rawURL. When
// deps.FetchResolver is non-nil (tests), it replaces the default
// stemcellfetch.ResolveSource package function. The production path uses
// stemcellfetch.ResolveSourceWith so operator-tunable transport timeouts
// (jobs/pve_cpi/spec stemcell_fetch_*_timeout_sec) reach the https and
// bosh+blobstore sources.
func resolveFetchSource(deps Deps, rawURL string) (stemcellfetch.Source, stemcellfetch.Reference, error) {
	if deps.FetchResolver != nil {
		return deps.FetchResolver(rawURL)
	}
	tc := stemcellfetch.TransportConfig{
		DialTimeout:           time.Duration(deps.Config.StemcellFetchDialTimeoutSec) * time.Second,
		TLSHandshakeTimeout:   time.Duration(deps.Config.StemcellFetchTLSHandshakeTimeoutSec) * time.Second,
		ResponseHeaderTimeout: time.Duration(deps.Config.StemcellFetchResponseHeaderTimeoutSec) * time.Second,
		IdleConnTimeout:       time.Duration(deps.Config.StemcellFetchIdleConnTimeoutSec) * time.Second,
	}
	return stemcellfetch.ResolveSourceWith(rawURL, tc)
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
//  7. Return light: CID.
//
// Temp file is cleaned up on both success and failure.
func handleLightStemcellFetch(
	ctx context.Context,
	deps Deps,
	cp stemcellCloudProps,
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
			log.String("image_url", cp.ImageURL),
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
	chosenNode, policyErr := pve.ValidateLightStemcellStorage(ctx, policyDeps, storage, cp.Node)
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

	// 3. Pre-fetch prefix dedup: scan storage for any import volume matching the
	// bosh-stemcell-<name>-<version>- prefix. We don't know sha8 yet, so this is
	// best-effort — it only fires when a prior fetch for the same name+version
	// already landed (regardless of sha8). On a hit we skip the network fetch and
	// build/reuse the template from the existing qcow2.
	prefix := stemcellfetch.FilenamePrefixForDedup(cp.Name, cp.Version)
	if existingVol, prefixErr := fetchFindByPrefix(ctx, deps, node, storage, prefix); prefixErr == nil && existingVol != "" {
		// Guard: only short-circuit when the found volid belongs to the target storage.
		// A volid from a different storage would produce a mismatched CID.
		if strings.HasPrefix(existingVol, storage+":") {
			extractedName := fetchExtractFilename(existingVol)
			if extractedName != "" {
				deps.Log(ctx).Info("create_stemcell: light fetch — existing stemcell found by prefix, building template",
					log.String("volid", existingVol),
				)
				// sha256hex unknown (prefix-dedup, no download); pass "" for tag (non-fatal).
				// light-fetch prefix-dedup: the qcow2 was originally uploaded by the CPI
				// (fetch path), but on this dedup hit we have no sha256hex and treat the
				// existing volume as not-CPI-owned to avoid deleting it (it may already
				// have a template pointing at it). cpiOwnsSource=false is conservative
				// and correct: the volume exists, the template lookup will find it.
				prefixTemplateNode := node
				if deps.Config != nil && deps.Config.StemcellTemplateNode != "" {
					prefixTemplateNode = deps.Config.StemcellTemplateNode
				}
				prefixVMID, prefixTmplErr := ensureTemplateVM(ctx, deps, prefixTemplateNode, storage, extractedName, "", false, cp, cp.ImageURL)
				if prefixTmplErr != nil {
					return nil, fmt.Errorf("create_stemcell: light fetch prefix-dedup: ensure template: %w", prefixTmplErr)
				}
				return pve.BuildTemplateStemcellCID(prefixVMID), nil
			}
		}
	}

	// 4. Fetch source body → local temp file + SHA-256 in flight.
	body, contentLength, fetchErr := src.Fetch(ctx, ref, creds)
	if fetchErr != nil {
		return nil, cpierrors.Cloud("create_stemcell: fetch %q: %s", cp.ImageURL, fetchErr.Error())
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
		return nil, cpierrors.Cloud("create_stemcell: stream fetched body to temp file: %s", copyErr.Error())
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
		log.String("sha256", sha256hex),
	)

	// Verify expected digest when supplied in cloud_properties.
	// Light-fetch path: source is network (retriable on mismatch).
	if verifyErr := verifyExpectedDigest(ctx, deps.Log(ctx), cp, sha256hex, tmpPath, "", stemcellSourceNetwork); verifyErr != nil {
		return nil, verifyErr
	}

	// 5. Build canonical filename + exact dedup check.
	qcow2Filename := stemcellfetch.BuildFetchedFilename(cp.Name, cp.Version, sha256hex)
	existingVol, findErr := pve.FindStemcellByFilename(ctx, deps.PVE, node, storage, qcow2Filename)
	if findErr != nil {
		return nil, cpierrors.Wrap(findErr, "create_stemcell: light fetch dedup lookup")
	}

	fetchTemplateNode := node
	if deps.Config != nil && deps.Config.StemcellTemplateNode != "" {
		fetchTemplateNode = deps.Config.StemcellTemplateNode
	}

	if existingVol != "" {
		deps.Log(ctx).Info("create_stemcell: light fetch — SHA-matched existing stemcell, building template",
			log.String("volid", existingVol),
		)
		// SHA-dedup: qcow2 already on storage. Build/reuse template.
		// cpiOwnsSource=false: qcow2 exists and is the authoritative copy; we do
		// not delete it (another stemcell record or clone may reference it).
		dedupVMID, dedupTmplErr := ensureTemplateVM(ctx, deps, fetchTemplateNode, storage, qcow2Filename, sha256hex, false, cp, cp.ImageURL)
		if dedupTmplErr != nil {
			return nil, fmt.Errorf("create_stemcell: light fetch SHA-dedup: ensure template: %w", dedupTmplErr)
		}
		return pve.BuildTemplateStemcellCID(dedupVMID), nil
	}

	// 6. Upload temp file under the final canonical filename. uploadStemcellImage
	// handles retry-on-lock and UPID await; it reopens tmpPath each attempt so
	// the PVE reader always sees a fresh stream from the beginning of the file.
	// tmpPath is a CPI-owned temp file (os.CreateTemp); not director-supplied.
	// stagingDir scoping is not applicable here — pass "" to use direct os.Open.
	if uploadErr := uploadStemcellImage(ctx, deps, node, storage, qcow2Filename, tmpPath, ""); uploadErr != nil {
		return nil, cpierrors.Wrap(uploadErr, "create_stemcell: light fetch upload")
	}

	// 7. Build template VM from the uploaded qcow2.
	// light-fetch: CPI uploaded the qcow2 → cpiOwnsSource=true; ensureTemplateVM
	// deletes it after freeze (reclaims storage; template disk is the live copy).
	fetchVMID, fetchTmplErr := ensureTemplateVM(ctx, deps, fetchTemplateNode, storage, qcow2Filename, sha256hex, true, cp, cp.ImageURL)
	if fetchTmplErr != nil {
		return nil, fmt.Errorf("create_stemcell: light fetch: ensure template: %w", fetchTmplErr)
	}

	templateCID := pve.BuildTemplateStemcellCID(fetchVMID)
	deps.Log(ctx).Info("create_stemcell: light stemcell (fetched) template ready",
		log.String("image_url", cp.ImageURL),
		log.String("source_scheme", ref.Scheme),
		log.String("creds_kind", creds.Kind()),
		log.String("cid", templateCID),
		log.Int64("bytes", written),
	)
	return templateCID, nil
}

// fetchFindByPrefix scans the named storage for any import volume whose volid
// contains ":import/<prefix>". Used by handleLightStemcellFetch for the
// pre-fetch dedup check before SHA-256 is known.
//
// Returns ("", nil) when no match is found. Returns ("", err) only on PVE API
// failure. The caller is responsible for verifying the returned volid belongs
// to the target storage before using it.
func fetchFindByPrefix(ctx context.Context, deps Deps, node, storage, prefix string) (string, error) {
	if deps.PVE == nil || deps.PVE.Nodes() == nil {
		return "", fmt.Errorf("fetchFindByPrefix: nodes service unavailable")
	}
	content := "import"
	resp, err := deps.PVE.Nodes().ListStorageContent(ctx, node, storage, &sdknodes.ListStorageContentParams{
		Content: &content,
	})
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", nil
	}
	needle := ":import/" + prefix
	for _, raw := range *resp {
		var item struct {
			VolID string `json:"volid"`
		}
		if jerr := json.Unmarshal(raw, &item); jerr != nil {
			continue
		}
		if strings.Contains(item.VolID, needle) {
			return item.VolID, nil
		}
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

	// Parse raw JSON entries identical to StorageInfoCache.refresh. Each item is
	// a json.RawMessage; we decode only the subset pve.StorageInfo needs.
	var entry struct {
		Storage string `json:"storage"`
		Type    string `json:"type"`
		Shared  *int   `json:"shared,omitempty"`
		Nodes   string `json:"nodes,omitempty"`
	}
	for _, raw := range *resp {
		if jerr := json.Unmarshal(raw, &entry); jerr != nil {
			continue
		}
		if entry.Storage != name {
			continue
		}
		info := pve.StorageInfo{
			Name: entry.Storage,
			Type: entry.Type,
		}
		if entry.Shared != nil && *entry.Shared != 0 {
			info.Shared = true
		}
		if entry.Nodes != "" {
			for _, part := range strings.Split(entry.Nodes, ",") {
				part = strings.TrimSpace(part)
				if part != "" {
					info.Nodes = append(info.Nodes, part)
				}
			}
		}
		return info, nil
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
		return cpierrors.Cloud(
			"create_stemcell: stemcell storage %q is local-only but the cluster has %d nodes; "+
				"set stemcell_replicate_local=true to replicate the template to each node, "+
				"or use a shared storage pool (NFS, Ceph, CIFS, etc.) accessible from all cluster nodes",
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
		log.String("sha256", imgSHA),
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
	rerr := pve.RetryOnTransientOrLock(ctx, deps.Log(ctx), "create_stemcell_upload", 0, func() error {
		f, openErr := openStagedFile(stagingDir, imagePath)
		if openErr != nil {
			return cpierrors.Cloud("uploadStemcellImage: open %s: %s", imagePath, openErr.Error())
		}
		defer func() { _ = f.Close() }()

		upid, uerr := deps.PVE.Storage().Upload(uploadCtx, node, storageName, "import", filename, f)
		if uerr != nil {
			return uerr
		}
		if upid == "" {
			return nil
		}
		return pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, deps.Log(ctx),
			pve.WithMaxWait(pve.StemcellMaxWait))
	})
	if rerr != nil {
		return cpierrors.Cloud("uploadStemcellImage: upload to %s/%s: %s", node, storageName, rerr.Error())
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
			// Cannot verify because we have no computed hash. Treat as unverified
			// (caller already warned) — same as no-digest path.
			logger.Warn("create_stemcell: cannot verify sha256: computed hash unavailable",
				log.String("expected_sha256", cp.ExpectedSHA256),
			)
			return nil
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
			log.String("sha256", sha256hex),
		)
		return nil
	}

	// SHA-1 check (only when SHA-256 not expected).
	if cp.ExpectedSHA1 != "" {
		actual, hashErr := sha1FilePath(resolvedPath, stagingDir)
		if hashErr != nil {
			// Hash computation failure: warn and skip (cannot block upload on it).
			logger.Warn("create_stemcell: cannot compute sha1 for expected-digest check",
				log.Err(hashErr),
			)
			return nil
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
//   - No mutable state is shared between goroutines; all results are logged directly.
func replicateStemcellToNodes(
	ctx context.Context,
	deps Deps,
	primaryNode, storage, qcow2Filename, sha256hex string,
	targetNodes []string,
	uploadSourcePath, uploadStagingDir string,
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

	// Collect non-primary nodes to replicate.
	var replicaNodes []string
	for _, node := range targetNodes {
		if node != primaryNode {
			replicaNodes = append(replicaNodes, node)
		}
	}
	if len(replicaNodes) == 0 {
		return
	}

	// sem is a buffered channel acting as a counting semaphore: at most
	// workerLimit goroutines hold a token simultaneously. With workerLimit=1
	// this is exactly the serial behavior of the original loop.
	sem := make(chan struct{}, workerLimit)

	var wg sync.WaitGroup
	for _, node := range replicaNodes {
		node := node // capture per iteration
		nodeLogger := logger.With(log.String("replica_node", node))

		wg.Add(1)
		// Acquire a semaphore slot before launching the goroutine so the pool
		// is bounded by workerLimit active goroutines at any moment.
		sem <- struct{}{}

		go func() {
			defer wg.Done()
			defer func() { <-sem }() // release slot when node work is done

			replicateOneNode(ctx, deps, nodeLogger, node, storage,
				qcow2Filename, sha256hex, sha8, uploadSourcePath, uploadStagingDir, cp, source)
		}()
	}
	wg.Wait()
}

// replicateOneNode performs the full upload+ensureTemplate sequence for a single
// replica node. It is called from a goroutine inside replicateStemcellToNodes.
// All failures are best-effort: logged as warnings, never returned as errors.
// The function is self-contained — it holds no references to shared mutable state.
func replicateOneNode(
	ctx context.Context,
	deps Deps,
	nodeLogger *log.Logger,
	node, storage,
	qcow2Filename, sha256hex, sha8,
	uploadSourcePath, uploadStagingDir string,
	cp stemcellCloudProps,
	source string,
) {
	// Check whether a replica already exists on this node (idempotent).
	existingVMID, alreadyExists, checkErr := pve.ResolveTemplateVMIDForNode(ctx, deps.PVE, node, sha8)
	if checkErr != nil {
		nodeLogger.Warn("create_stemcell: replication: cannot check existing replica (skipping node)",
			log.Err(checkErr),
		)
		return
	}
	if alreadyExists {
		nodeLogger.Info("create_stemcell: replication: replica already exists (skipping upload)",
			log.Int(metadataKeyVMID, existingVMID),
		)
		return
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
			return
		case adopted:
			nodeLogger.Info("create_stemcell: replication: adopted in-flight replica from concurrent builder (skipping upload)",
				log.Int(metadataKeyVMID, adoptedVMID),
			)
			return
		}
	}

	// Upload qcow2 to this node's local storage. uploadStemcellImage opens its
	// own file handle (openStagedFile inside), so concurrent calls for different
	// nodes read the same source file independently without sharing an *os.File.
	if uploadErr := uploadStemcellImage(ctx, deps, node, storage, qcow2Filename, uploadSourcePath, uploadStagingDir); uploadErr != nil {
		nodeLogger.Warn("create_stemcell: replication: upload failed (non-fatal; replica not created)",
			log.Err(uploadErr),
		)
		return
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
	func() {
		if deps.Config != nil {
			replicaRelease, replicaInflightErr := deps.Inflight.acquire(ctx, node, deps.Config.MaxInflightPerNodeLimit())
			if replicaInflightErr != nil {
				nodeLogger.Warn("create_stemcell: replication: in-flight limit; skipping replica node",
					log.String("node", node),
					log.Err(replicaInflightErr),
				)
				return
			}
			defer replicaRelease()
		}
		replicaVMID, tmplErr := ensureReplicaTemplateVM(ctx, deps, node, storage, qcow2Filename, sha256hex, replicaCP, source)
		if tmplErr != nil {
			nodeLogger.Warn("create_stemcell: replication: ensure template failed (non-fatal; replica not created)",
				log.Err(tmplErr),
			)
			// Best-effort: delete the uploaded qcow2 to reclaim storage.
			volumePath := "import/" + qcow2Filename
			if _, delErr := deps.PVE.Storage().DeleteVolumeIfExists(ctx, node, storage, volumePath); delErr != nil {
				nodeLogger.Warn("create_stemcell: replication: cleanup of failed upload also failed (non-fatal)",
					log.Err(delErr),
				)
			}
			return
		}
		nodeLogger.Info("create_stemcell: replication: replica template created",
			log.Int64(metadataKeyVMID, replicaVMID),
		)
	}()
}

// ensureReplicaTemplateVM is like ensureTemplateVM but tags the created VM with
// both the sha tag and the per-node replica tag "bosh-stemcell-node-<node>".
// The combined tag string lets ResolveTemplateVMIDForNode distinguish replicas
// from the primary template when scanning a node's QEMU list.
//
// The replica qcow2 is CPI-owned (uploaded just above in replicateStemcellToNodes)
// so cpiOwnsSource=true; ensureTemplateVM will delete it after freeze.
func ensureReplicaTemplateVM(
	ctx context.Context,
	deps Deps,
	node, storage, qcow2Filename, sha256hex string,
	cp stemcellCloudProps,
	source string,
) (int64, error) {
	sha8 := sha256hex
	if len(sha8) > 8 {
		sha8 = sha8[:8]
	}
	logger := deps.Log(ctx)

	// Build deterministic template name (same as primary, differentiator is tag).
	templateName := pve.BuildTemplateName(cp.Name, cp.Version)

	// Dedup: check sha tag + node tag combo.
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
	shaTag := "bosh-stemcell-sha-" + sha8
	nodeTag := pve.ReplicaNodeTagForNode(node)

	isRetryable := func(e error) bool {
		return pve.IsVMIDConflict(e) || pve.IsStorageLockTimeout(e) || pve.IsTransientTransport(e)
	}

	rangeStart := deps.Config.StemcellTemplateVMIDRangeStart
	rangeEnd := deps.Config.StemcellTemplateVMIDRangeEnd

	// extraBaseTags carries the per-node replica tag so attemptCreateTemplateVM
	// includes it in the base tag set alongside shaTag for both the OFF path
	// (byte-identical "shaTag;nodeTag" join) and the ON path (merged with provTags).
	extraBaseTags := []string{nodeTag}

	allocatedRaw, allocErr := pve.AllocateWithRetry(ctx, deps.PVE,
		func(candidate int) error {
			return attemptCreateTemplateVM(ctx, deps, logger, node, candidate, templateName, importVolid, shaTag, deps.Config.VMStorage, cp, source, extraBaseTags)
		},
		isRetryable,
		0,
		pve.WithRange(rangeStart, rangeEnd),
	)
	if allocErr != nil {
		return 0, fmt.Errorf("ensureReplicaTemplateVM: allocate+create replica VM node %q: %w", node, allocErr)
	}
	allocatedVMID := int64(allocatedRaw)

	// Freeze into template.
	freezeUPID, freezeErr := pve.MakeTemplate(ctx, deps.PVE, node, allocatedVMID)
	if freezeErr != nil {
		return 0, fmt.Errorf("ensureReplicaTemplateVM: freeze node=%q vmid=%d: %w", node, allocatedVMID, freezeErr)
	}
	if freezeUPID != "" {
		if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, freezeUPID, logger,
			pve.WithMaxWait(pve.StemcellMaxWait)); awaitErr != nil {
			return 0, fmt.Errorf("ensureReplicaTemplateVM: await freeze node=%q vmid=%d upid=%s: %w",
				node, allocatedVMID, freezeUPID, awaitErr)
		}
	}

	// Delete the per-node uploaded qcow2 (CPI-owned). Best-effort.
	volumePath := "import/" + qcow2Filename
	if _, delErr := deps.PVE.Storage().DeleteVolumeIfExists(ctx, node, storage, volumePath); delErr != nil {
		logger.Warn("ensureReplicaTemplateVM: best-effort qcow2 delete failed (non-fatal)",
			log.String("node", node),
			log.String("volume", volumePath),
			log.Err(delErr),
		)
	}

	logger.Info("ensureReplicaTemplateVM: replica template frozen",
		log.String("node", node),
		log.Int64(metadataKeyVMID, allocatedVMID),
	)
	return allocatedVMID, nil
}
