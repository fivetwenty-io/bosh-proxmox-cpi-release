package handlers

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
	case "linux", "ubuntu", "centos", "rhel", "debian", "fedora", "alpine", "l26":
		return "l26"
	case "linux24", "l24":
		return "l24"
	case "windows", "win", "win10":
		return "win10"
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
	case "qcow2", "openstack-qcow2", "general-qcow2", "pve-qcow2":
		return "qcow2"
	case "raw", "openstack-raw", "general-raw", "pve-raw":
		return "raw"
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
}

// validateLightMutex returns an error when both ImageID and ImageURL are set.
// The two fields are mutually exclusive: ImageID points to an already-uploaded
// volume; ImageURL triggers a fetch. Supplying both is an operator error.
func (p stemcellCloudProps) validateLightMutex() error {
	if p.ImageID != "" && p.ImageURL != "" {
		return cpierrors.Cloud(
			"create_stemcell: cloud_properties.image_id and cloud_properties.image_url are mutually exclusive")
	}
	return nil
}

// IsLight reports whether the stemcell is a light stemcell (no local tarball
// required). True when either ImageID or ImageURL is set.
func (p stemcellCloudProps) IsLight() bool {
	return p.ImageID != "" || p.ImageURL != ""
}

// LightMode returns the light-stemcell variant string:
//   - "preuploaded" when ImageID is set (operator pre-placed volume)
//   - "fetch"       when ImageURL is set (CPI fetches from remote URL)
//   - ""            when neither is set (heavy stemcell, normal tarball upload)
func (p stemcellCloudProps) LightMode() string {
	if p.ImageID != "" {
		return "preuploaded"
	}
	if p.ImageURL != "" {
		return "fetch"
	}
	return ""
}

// parseStemcellCloudProps extracts known fields from cloud_properties.
// Missing or unrecognized keys are silently ignored; defaults apply.
func parseStemcellCloudProps(cp map[string]any) stemcellCloudProps {
	p := stemcellCloudProps{
		DiskFormat: "qcow2",
		OSType:     "l26",
	}

	if v, ok := cp["disk_format"].(string); ok && v != "" {
		p.DiskFormat = v
	}
	if v, ok := cp["os_type"].(string); ok && v != "" {
		p.OSType = normalizeOSType(v)
	}
	if v, ok := cp["name"].(string); ok {
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

	return cpierrors.Cloud(
		"create_stemcell: imagePath %q outside permitted staging root", imagePath)
}

// HandleCreateStemcell returns a Handler for the BOSH CPI create_stemcell method.
//
// Arguments (positional JSON array):
//
//	[0] image_path      string — absolute local path to stemcell disk image (or tarball).
//	[1] cloud_properties object — stemcell.MF cloud_properties section (may be omitted).
//
// Returns: stemcell_cid string — "<storage>:import/<filename>" (e.g. "stemcell-store:import/bosh-stemcell-ubuntu-jammy-1.438-a1b2c3d4.qcow2").
//
// Eleven-step direct-upload flow:
//
//  1. Validate args[0] image_path.
//  2. Parse cloud_properties → stemcellCloudProps.
//  3. Validate cloud_properties.name and cloud_properties.version are non-empty (required for CID).
//  4. Determine storage: config.StemcellStorage (falls back to config.VMStorage if empty).
//  5. Validate storage is shared (required for multi-node clusters).
//  6. Extract/resolve the disk image from the tarball if needed.
//  7. Compute SHA-256 of the disk image.
//  8. Build qcow2 filename from name/version/sha8.
//  9. Build stemcell CID.
//  10. Dedup: FindStemcellByFilename — return existing CID if found.
//  11. Upload qcow2 to storage; await task with StemcellMaxWait.
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
		}

		// ----------------------------------------------------------------
		// Step 4: Determine node and storage
		// ----------------------------------------------------------------
		node := deps.Config.Node
		if node == "" {
			return nil, cpierrors.Cloud("create_stemcell: config.node must not be empty")
		}

		storage := deps.Config.StemcellStorage
		if storage == "" {
			storage = deps.Config.VMStorage
		}
		if storage == "" {
			return nil, cpierrors.Cloud("create_stemcell: no stemcell storage configured (stemcell_storage and vm_storage both empty)")
		}

		// ----------------------------------------------------------------
		// Step 5: Validate storage is shared
		// ----------------------------------------------------------------
		if validateErr := validateStemcellStorageShared(ctx, deps, storage); validateErr != nil {
			return nil, validateErr
		}

		// ----------------------------------------------------------------
		// Step 6: Resolve disk image (extract from tarball if needed)
		// ----------------------------------------------------------------
		uploadSourcePath, cleanupExtract, detectedFormat, extractedSHA, detectErr := resolveStemcellImage(
			imagePath, cp.DiskFormat, deps.Config.StemcellStagingDir, deps.Logger)
		if detectErr != nil {
			return nil, cpierrors.Wrap(detectErr, "create_stemcell: resolve image")
		}
		defer cleanupExtract()

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

		// ----------------------------------------------------------------
		// Step 7: Obtain SHA-256 of resolved disk image
		// ----------------------------------------------------------------
		// For tarball inputs resolveStemcellImage computed the SHA via TeeReader
		// during the single extraction pass. For bare images (qcow2 magic, raw
		// passthrough) extractedSHA is empty and a second-pass file read is used.
		sha256hex := extractedSHA
		if sha256hex == "" {
			var hashErr error
			sha256hex, hashErr = sha256FilePath(uploadSourcePath, deps.Config.StemcellStagingDir)
			if hashErr != nil {
				return nil, cpierrors.Wrap(hashErr, "create_stemcell: compute sha256")
			}
		}

		// ----------------------------------------------------------------
		// Steps 8–9: Build filename and CID
		// ----------------------------------------------------------------
		qcow2Filename := pve.BuildStemcellFilename(cp.Name, cp.Version, sha256hex)
		cid := pve.BuildStemcellCID(storage, qcow2Filename)

		deps.Logger.Info("create_stemcell: resolved filenames",
			log.String("qcow2", qcow2Filename),
			log.String("cid", cid),
			log.String("sha256", sha256hex),
		)

		// ----------------------------------------------------------------
		// Step 10: Dedup — skip upload if volume already present
		// ----------------------------------------------------------------
		existing, findErr := pve.FindStemcellByFilename(ctx, deps.PVE, node, storage, qcow2Filename)
		if findErr != nil {
			return nil, cpierrors.Wrap(findErr, "create_stemcell: dedup lookup")
		}
		if existing != "" {
			deps.Logger.Info("create_stemcell: stemcell already present, returning existing CID",
				log.String("cid", existing),
				log.String("name", cp.Name),
				log.String("version", cp.Version),
			)
			return existing, nil
		}

		// ----------------------------------------------------------------
		// Step 11: Upload qcow2 image
		// ----------------------------------------------------------------
		// uploadStagingDir is set only when uploadSourcePath is the director-supplied
		// imagePath (bare qcow2 passthrough). When resolveStemcellImage extracted a
		// file into a CPI-owned tmpDir, uploadSourcePath differs from imagePath and
		// no staging-dir scoping is needed (the file is already CPI-controlled).
		uploadStagingDir := ""
		if uploadSourcePath == imagePath {
			uploadStagingDir = deps.Config.StemcellStagingDir
		}
		if uploadErr := uploadStemcellImage(ctx, deps, node, storage, qcow2Filename, uploadSourcePath, uploadStagingDir); uploadErr != nil {
			return nil, cpierrors.Wrap(uploadErr, "create_stemcell: upload qcow2")
		}
		deps.Logger.Info("create_stemcell: qcow2 uploaded",
			log.String("volume", cid),
			log.String("source", imagePath),
		)

		// Source of truth for stemcell identity is the qcow2 filename
		// (encodes name/version/sha8) plus state held by the BOSH Director
		// (name, version, cloud_properties on the stemcell record). PVE's
		// content APIs don't accept arbitrary metadata for import volumes
		// (uploads validate file extension; notes are backup-only), so no
		// sidecar or volume-level annotation is written here.

		deps.Logger.Info("create_stemcell: stemcell ready",
			log.String("stemcell_cid", cid),
			log.String("name", cp.Name),
			log.String("version", cp.Version),
		)
		return cid, nil
	})
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

	// 6. Build canonical light: CID.
	lightCID := pve.BuildLightStemcellCID(storage, qcow2Filename)

	deps.Logger.Info("create_stemcell: light stemcell (pre-uploaded) accepted",
		log.String("image_id", imageID),
		log.String("storage", storage),
		log.String("node", node),
		log.String("cid", lightCID),
	)
	return lightCID, nil
}

// fetchResolverOverride is set by tests to inject a stub source resolver.
// Production code leaves it nil and uses stemcellfetch.ResolveSource directly.
// This package-level seam avoids bloating Deps for a single test concern.
var fetchResolverOverride func(rawURL string) (stemcellfetch.Source, stemcellfetch.Reference, error)

// resolveFetchSource calls the override when set (tests), otherwise the real implementation.
func resolveFetchSource(rawURL string) (stemcellfetch.Source, stemcellfetch.Reference, error) {
	if fetchResolverOverride != nil {
		return fetchResolverOverride(rawURL)
	}
	return stemcellfetch.ResolveSource(rawURL)
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
	src, ref, resolveErr := resolveFetchSource(cp.ImageURL)
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
		deps.Logger.Warn("create_stemcell: fetching stemcell without credentials",
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
	// already landed (regardless of sha8). On a hit we skip the network fetch.
	prefix := stemcellfetch.FilenamePrefixForDedup(cp.Name, cp.Version)
	if existingVol, prefixErr := fetchFindByPrefix(ctx, deps, node, storage, prefix); prefixErr == nil && existingVol != "" {
		// Guard: only short-circuit when the found volid belongs to the target storage.
		// A volid from a different storage would produce a mismatched light: CID.
		if strings.HasPrefix(existingVol, storage+":") {
			extractedName := fetchExtractFilename(existingVol)
			if extractedName != "" {
				deps.Logger.Info("create_stemcell: light fetch — existing stemcell found by prefix, skipping fetch",
					log.String("volid", existingVol),
				)
				return pve.BuildLightStemcellCID(storage, extractedName), nil
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

	// Sync to disk before uploadStemcellImage reopens the file for upload.
	if syncErr := tmpFile.Sync(); syncErr != nil {
		return nil, cpierrors.Wrap(syncErr, "create_stemcell: sync fetch temp file")
	}

	deps.Logger.Info("create_stemcell: light fetch streamed to temp file",
		log.Int64("bytes_written", written),
		log.Int64("content_length", contentLength),
		log.String("sha256", sha256hex),
	)

	// 5. Build canonical filename + exact dedup check.
	qcow2Filename := stemcellfetch.BuildFetchedFilename(cp.Name, cp.Version, sha256hex)
	existingVol, findErr := pve.FindStemcellByFilename(ctx, deps.PVE, node, storage, qcow2Filename)
	if findErr != nil {
		return nil, cpierrors.Wrap(findErr, "create_stemcell: light fetch dedup lookup")
	}
	if existingVol != "" {
		deps.Logger.Info("create_stemcell: light fetch — SHA-matched existing stemcell, skipping upload",
			log.String("volid", existingVol),
		)
		return pve.BuildLightStemcellCID(storage, qcow2Filename), nil
	}

	// 6. Upload temp file under the final canonical filename. uploadStemcellImage
	// handles retry-on-lock and UPID await; it reopens tmpPath each attempt so
	// the PVE reader always sees a fresh stream from the beginning of the file.
	// tmpPath is a CPI-owned temp file (os.CreateTemp); not director-supplied.
	// stagingDir scoping is not applicable here — pass "" to use direct os.Open.
	if uploadErr := uploadStemcellImage(ctx, deps, node, storage, qcow2Filename, tmpPath, ""); uploadErr != nil {
		return nil, cpierrors.Wrap(uploadErr, "create_stemcell: light fetch upload")
	}

	lightCID := pve.BuildLightStemcellCID(storage, qcow2Filename)
	deps.Logger.Info("create_stemcell: light stemcell (fetched) ready",
		log.String("image_url", cp.ImageURL),
		log.String("source_scheme", ref.Scheme),
		log.String("creds_kind", creds.Kind()),
		log.String("cid", lightCID),
		log.Int64("bytes", written),
	)
	return lightCID, nil
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
		return pve.StorageInfo{}, err
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
		deps.Logger.Warn("create_stemcell: cannot resolve storage backend; skipping shared-storage check",
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
		deps.Logger.Warn("create_stemcell: cannot determine cluster node count; skipping shared-storage check",
			log.String("storage", storage),
			log.Err(sizeErr),
		)
		return nil
	}

	if clusterSize > 1 {
		return cpierrors.Cloud(
			"create_stemcell: stemcell storage %q is local-only but the cluster has %d nodes; "+
				"stemcell_storage must be a shared storage pool (NFS, Ceph, CIFS, etc.) "+
				"accessible from all cluster nodes",
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
		return 0, cpierrors.Wrap(err, "clusterNodeCount: list cluster config nodes")
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
			return nil, cpierrors.Cloud("resolveStemcellImage: tar: %s", terr.Error())
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
			return nil, cpierrors.Cloud(
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
			return nil, cpierrors.Cloud(
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
		return "", "", cpierrors.Cloud(
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
		return "qcow2", nil
	case magic[0] == 0x1F && magic[1] == 0x8B:
		// Nested gzip inside a tar — treat as raw; PVE handles decompression.
		return "raw", nil
	case n >= 4 && magic[0] == 0x04 && magic[1] == 0x22 && magic[2] == 0x4D && magic[3] == 0x18:
		// LZ4 frame magic.
		return "raw", nil
	default:
		// Require the file to be a known .img or large enough to plausibly
		// be a raw disk. If neither, it likely is not a disk image.
		fi, sterr := os.Stat(imgPath)
		if sterr != nil || fi.Size() < 1024*1024 {
			cleanup()
			return "", cpierrors.Cloud(
				"create_stemcell: extracted image at %s has unknown magic bytes %x; expected qcow2/gzip/lz4/raw",
				imgPath, magic[:n])
		}
		return "raw", nil
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
		return imagePath, noop, "qcow2", "", nil
	}

	// Gzip magic: 0x1F 0x8B
	if n >= 2 && head[0] == 0x1F && head[1] == 0x8B {
		return resolveGzipTar(f, imagePath, defaultFormat, logger)
	}

	// Not gzip, not qcow2 magic — treat as raw disk image. SHA computed by caller.
	logger.Info("resolveStemcellImage: passthrough as raw", log.String("source", imagePath))
	return imagePath, noop, "raw", "", nil
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
	rerr := pve.RetryOnTransientOrLock(ctx, deps.Logger, "create_stemcell_upload", 0, func() error {
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
		return pve.AwaitTaskWithLogger(ctx, deps.PVE, node, upid, deps.Logger,
			pve.WithMaxWait(pve.StemcellMaxWait))
	})
	if rerr != nil {
		return cpierrors.Cloud("uploadStemcellImage: upload to %s/%s: %s", node, storageName, rerr.Error())
	}
	return nil
}
