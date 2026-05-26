// Package handlers contains the 22 BOSH CPI v2 method implementations.
package handlers

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"

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

	return p
}

// stemcellStagingRoots returns the absolute, cleaned set of directories under
// which an incoming image_path is permitted to resolve. The BOSH director stages
// stemcell tarballs in its scratch area (os.TempDir() on the CPI host), so that
// root is always permitted. Additional roots may be added here in the future
// (e.g. an explicit `stemcell_staging_dir` config field) without changing
// callers.
func stemcellStagingRoots() []string {
	roots := []string{os.TempDir()}
	// Resolve and clean each root once. Best-effort EvalSymlinks: if the root
	// itself is a symlink (e.g. /tmp -> /private/tmp on macOS), comparison must
	// use the realpath so a resolved image_path under the realpath matches.
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
		// Step 1: Validate arg 0 — image_path
		// ----------------------------------------------------------------
		if len(args) < 1 {
			return nil, cpierrors.Cloud("create_stemcell: missing required argument image_path")
		}
		var imagePath string
		if err := json.Unmarshal(args[0], &imagePath); err != nil || imagePath == "" {
			return nil, cpierrors.Cloud("create_stemcell: image_path must be a non-empty string")
		}

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
			imagePath, cp.DiskFormat, deps.Logger)
		if detectErr != nil {
			return nil, cpierrors.Wrap(detectErr, "create_stemcell: resolve image")
		}
		defer cleanupExtract()

		// User-supplied disk_format wins when present; aliases like
		// "openstack-qcow2" or "general-raw" are translated to PVE-native
		// enum (qcow2/raw/vmdk). Unknown aliases fall back to magic-byte detection.
		uploadFormat := pveDiskFormat(cp.DiskFormat)
		if uploadFormat == "" {
			uploadFormat = detectedFormat
		}

		// ----------------------------------------------------------------
		// Step 7: Obtain SHA-256 of resolved disk image
		// ----------------------------------------------------------------
		// For tarball inputs resolveStemcellImage computed the SHA via TeeReader
		// during the single extraction pass. For bare images (qcow2 magic, raw
		// passthrough) extractedSHA is empty and a second-pass file read is used.
		sha256hex := extractedSHA
		if sha256hex == "" {
			var hashErr error
			sha256hex, hashErr = sha256FilePath(uploadSourcePath)
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
		if uploadErr := uploadStemcellImage(ctx, deps, node, storage, qcow2Filename, uploadSourcePath); uploadErr != nil {
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
func resolveStemcellImage(imagePath, defaultFormat string, logger *log.Logger) (path string, cleanup func(), detectedFormat string, extractedSHA256hex string, err error) {
	noop := func() {}

	f, openErr := os.Open(imagePath)
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
		if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
			return "", noop, "", "", cpierrors.Cloud("resolveStemcellImage: seek: %s", seekErr.Error())
		}
		gz, gzErr := gzip.NewReader(f)
		if gzErr != nil {
			return "", noop, "", "", cpierrors.Cloud("resolveStemcellImage: gzip: %s", gzErr.Error())
		}
		defer func() { _ = gz.Close() }()

		tr := tar.NewReader(gz)
		// Find the largest regular file ending in .img (or just the first
		// regular file if no .img is present). Stemcells contain root.img
		// alongside small manifest files.
		tmpDir, mkErr := os.MkdirTemp("", "bosh-stemcell-extract-")
		if mkErr != nil {
			return "", noop, "", "", cpierrors.Cloud("resolveStemcellImage: mktemp: %s", mkErr.Error())
		}
		cleanup := func() { _ = os.RemoveAll(tmpDir) }

		// tarCandidate records a file extracted from the tarball that may be
		// the disk image. Two-pass selection: pass 1 extracts all candidates
		// and computes each one's SHA-256 via TeeReader during the single copy;
		// pass 2 selects by preference order and returns the winner's pre-computed
		// SHA so the caller skips a second file-read pass.
		type tarCandidate struct {
			path      string
			size      int64
			isImg     bool   // true when name ends in .img
			sha256hex string // hex-encoded SHA-256 computed during extraction
		}
		var candidates []tarCandidate
		var totalExtracted int64
		for {
			hdr, terr := tr.Next()
			if terr == io.EOF {
				break
			}
			if terr != nil {
				cleanup()
				return "", noop, "", "", cpierrors.Cloud("resolveStemcellImage: tar: %s", terr.Error())
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
				return "", noop, "", "", cpierrors.Cloud(
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
				return "", noop, "", "", cpierrors.Cloud(
					"create_stemcell: tarball entries exceed maximum %dGB; refusing to extract",
					MaxStemcellTotalExtract/(1024*1024*1024))
			}
			dst := filepath.Join(tmpDir, name)
			out, oerr := os.Create(dst)
			if oerr != nil {
				cleanup()
				return "", noop, "", "", cpierrors.Cloud("resolveStemcellImage: create %s: %s", dst, oerr.Error())
			}
			h := sha256.New()
			tee := io.TeeReader(tr, h)
			// Bound the per-file write at hdr.Size. archive/tar's reader is
			// already capped at the header-declared size, so a well-formed
			// entry copies exactly hdr.Size bytes and CopyN returns (n, nil).
			// If the tar stream is truncated, CopyN returns io.ErrUnexpectedEOF
			// after copying fewer than hdr.Size bytes; that is treated as an
			// error here so callers cannot upload a half-written disk image.
			written, cerr := io.CopyN(out, tee, hdr.Size)
			if cerr != nil && cerr != io.EOF {
				_ = out.Close()
				cleanup()
				return "", noop, "", "", cpierrors.Cloud(
					"resolveStemcellImage: copy %s (wrote %d of %d declared bytes): %s",
					dst, written, hdr.Size, cerr.Error())
			}
			_ = out.Close()
			candidates = append(candidates, tarCandidate{
				path:      dst,
				size:      hdr.Size,
				isImg:     isImg,
				sha256hex: hex.EncodeToString(h.Sum(nil)),
			})
		}
		if len(candidates) == 0 {
			cleanup()
			return "", noop, "", "", cpierrors.Cloud("resolveStemcellImage: no disk image inside tarball %s", imagePath)
		}

		// Pass 2: prefer largest .img file; fall back to largest non-.img.
		// "root.img" is a standard BOSH stemcell name and is preferred if
		// multiple .img files share the same size.
		var imgPath string
		var imgSHA string
		var imgSize int64
		var fallbackPath string
		var fallbackSHA string
		var fallbackSize int64
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
			return "", noop, "", "", cpierrors.Cloud(
				"create_stemcell: no usable disk image candidate in tarball %s",
				imagePath)
		}

		// Detect format and validate magic bytes. Read only the first 4 bytes
		// to avoid OOM on multi-GB images. Accepted signatures:
		//   qcow2  — 'Q','F','I',0xFB
		//   gzip   — 0x1F,0x8B (compressed raw; treat as raw)
		//   lz4    — 0x04,0x22,0x4D,0x18
		//   raw    — any other non-zero content of sufficient size (>= 1 MiB)
		//
		// Files that do not match any known signature are rejected to prevent
		// accidentally uploading a manifest or other metadata file as the disk.
		format := defaultFormat
		if mf, merr := os.Open(imgPath); merr == nil {
			var magic [4]byte
			n, rerr := io.ReadFull(mf, magic[:])
			_ = mf.Close()
			if rerr != nil && n < 2 {
				cleanup()
				return "", noop, "", "", cpierrors.Cloud(
					"create_stemcell: extracted image at %s is too small to identify (read %d bytes)",
					imgPath, n)
			}
			switch {
			case magic[0] == 'Q' && magic[1] == 'F' && magic[2] == 'I' && magic[3] == 0xFB:
				format = "qcow2"
			case magic[0] == 0x1F && magic[1] == 0x8B:
				// Nested gzip inside a tar — treat as raw; PVE handles decompression.
				format = "raw"
			case n >= 4 && magic[0] == 0x04 && magic[1] == 0x22 && magic[2] == 0x4D && magic[3] == 0x18:
				// LZ4 frame magic.
				format = "raw"
			default:
				// Require the file to be a known .img or large enough to plausibly
				// be a raw disk. If neither, it likely is not a disk image.
				fi, sterr := os.Stat(imgPath)
				if sterr != nil || fi.Size() < 1024*1024 {
					cleanup()
					return "", noop, "", "", cpierrors.Cloud(
						"create_stemcell: extracted image at %s has unknown magic bytes %x; expected qcow2/gzip/lz4/raw",
						imgPath, magic[:n])
				}
				format = "raw"
			}
		}
		logger.Info("resolveStemcellImage: extracted",
			log.String("source", imagePath),
			log.String("disk", imgPath),
			log.String("format", format),
			log.String("sha256", imgSHA),
		)
		return imgPath, cleanup, format, imgSHA, nil
	}

	// Not gzip, not qcow2 magic — treat as raw disk image. SHA computed by caller.
	logger.Info("resolveStemcellImage: passthrough as raw", log.String("source", imagePath))
	return imagePath, noop, "raw", "", nil
}

// sha256FilePath returns the hex-encoded SHA-256 of the file at path.
func sha256FilePath(path string) (string, error) {
	f, err := os.Open(path)
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
func uploadStemcellImage(
	ctx context.Context,
	deps Deps,
	node, storageName, filename, imagePath string,
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
		f, openErr := os.Open(imagePath)
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
