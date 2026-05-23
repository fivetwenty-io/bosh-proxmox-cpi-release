// Package handlers contains the 22 BOSH CPI v2 method implementations.
package handlers

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	sdkclient "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/client"
)

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

// HandleCreateStemcell returns a Handler for the BOSH CPI create_stemcell method.
//
// Arguments (positional JSON array):
//
//	[0] image_path      string — absolute local path to stemcell disk image (or tarball).
//	[1] cloud_properties object — stemcell.MF cloud_properties section (may be omitted).
//
// Returns: stemcell_cid string — "<storage>:import/<filename>" (e.g. "stemcell-store:import/bosh-stemcell-ubuntu-jammy-1.438-a1b2c3d4.qcow2").
//
// Thirteen-step direct-upload flow:
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
		uploadSourcePath, cleanupExtract, detectedFormat, detectErr := resolveStemcellImage(
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
		// Step 7: Compute SHA-256 of resolved disk image
		// ----------------------------------------------------------------
		sha256hex, hashErr := sha256FilePath(uploadSourcePath)
		if hashErr != nil {
			return nil, cpierrors.Wrap(hashErr, "create_stemcell: compute sha256")
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
		// Step 11: Dedup — skip upload if volume already present
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
		// Step 12: Upload qcow2 image
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
		return 0, fmt.Errorf("clusterNodeCount: list cluster config nodes: %w", err)
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
func resolveStemcellImage(imagePath, defaultFormat string, logger *log.Logger) (string, func(), string, error) {
	noop := func() {}

	f, err := os.Open(imagePath)
	if err != nil {
		return "", noop, "", cpierrors.Cloud("resolveStemcellImage: open %s: %s", imagePath, err.Error())
	}
	defer func() { _ = f.Close() }()

	// Read enough bytes to identify gzip (2), QCOW2 (4), or plain tar (262).
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]

	// Bare QCOW2 magic: 'Q','F','I',0xFB
	if n >= 4 && head[0] == 'Q' && head[1] == 'F' && head[2] == 'I' && head[3] == 0xFB {
		return imagePath, noop, "qcow2", nil
	}

	// Gzip magic: 0x1F 0x8B
	if n >= 2 && head[0] == 0x1F && head[1] == 0x8B {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return "", noop, "", cpierrors.Cloud("resolveStemcellImage: seek: %s", err.Error())
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			return "", noop, "", cpierrors.Cloud("resolveStemcellImage: gzip: %s", err.Error())
		}
		defer func() { _ = gz.Close() }()

		tr := tar.NewReader(gz)
		// Find the largest regular file ending in .img (or just the first
		// regular file if no .img is present). Stemcells contain root.img
		// alongside small manifest files.
		tmpDir, err := os.MkdirTemp("", "bosh-stemcell-extract-")
		if err != nil {
			return "", noop, "", cpierrors.Cloud("resolveStemcellImage: mktemp: %s", err.Error())
		}
		cleanup := func() { _ = os.RemoveAll(tmpDir) }

		var imgPath string
		var imgSize int64
		for {
			hdr, terr := tr.Next()
			if terr == io.EOF {
				break
			}
			if terr != nil {
				cleanup()
				return "", noop, "", cpierrors.Cloud("resolveStemcellImage: tar: %s", terr.Error())
			}
			if hdr.Typeflag != tar.TypeReg {
				continue
			}
			name := filepath.Base(hdr.Name)
			isImg := strings.HasSuffix(strings.ToLower(name), ".img") || name == "root.img"
			// Skip obviously-not-disk small files.
			if !isImg && hdr.Size < 1024*1024 {
				continue
			}
			dst := filepath.Join(tmpDir, name)
			out, oerr := os.Create(dst)
			if oerr != nil {
				cleanup()
				return "", noop, "", cpierrors.Cloud("resolveStemcellImage: create %s: %s", dst, oerr.Error())
			}
			if _, cerr := io.Copy(out, tr); cerr != nil {
				_ = out.Close()
				cleanup()
				return "", noop, "", cpierrors.Cloud("resolveStemcellImage: copy %s: %s", dst, cerr.Error())
			}
			_ = out.Close()
			if hdr.Size > imgSize {
				imgPath = dst
				imgSize = hdr.Size
			}
		}
		if imgPath == "" {
			cleanup()
			return "", noop, "", cpierrors.Cloud("resolveStemcellImage: no disk image inside tarball %s", imagePath)
		}

		// Detect format of extracted file via magic bytes. Read only the
		// first 4 bytes — qcow2 magic is QFI\xFB — instead of slurping the
		// entire (multi-GB) disk image into memory, which previously OOM'd
		// resource-constrained directors (D-07).
		format := defaultFormat
		if mf, merr := os.Open(imgPath); merr == nil {
			var magic [4]byte
			if _, rerr := io.ReadFull(mf, magic[:]); rerr == nil {
				if magic[0] == 'Q' && magic[1] == 'F' && magic[2] == 'I' && magic[3] == 0xFB {
					format = "qcow2"
				} else {
					format = "raw"
				}
			}
			_ = mf.Close()
		}
		logger.Info("resolveStemcellImage: extracted",
			log.String("source", imagePath),
			log.String("disk", imgPath),
			log.String("format", format),
		)
		return imgPath, cleanup, format, nil
	}

	// Not gzip, not qcow2 magic — treat as raw disk image.
	logger.Info("resolveStemcellImage: passthrough as raw", log.String("source", imagePath))
	return imagePath, noop, "raw", nil
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
	// StemcellMaxWait (600s) to accommodate format conversion (D-06). Both
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
