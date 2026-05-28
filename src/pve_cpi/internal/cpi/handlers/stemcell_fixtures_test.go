// Package handlers_test — stemcell test fixture builders shared across
// create_stemcell_test.go and related handler test files.
package handlers_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
)

// stemcellFixtureOpts controls the shape of a stemcell image fixture produced
// by newStemcellFixture.
//
// Format selects the outer container type written to disk:
//   - "raw"    — bare image file (no tar wrapper)
//   - "tgz"    — gzip+tar archive containing a single "root.img" entry
//   - ""       — defaults to "raw"
//
// Magic selects the inner image magic bytes written into the disk image:
//   - "correct" — qcow2 magic (0x51 0x46 0x49 0xFB)
//   - "wrong"   — non-qcow2, non-gzip bytes (Java class file: CA FE BA BE)
//   - "missing" — 512 zero bytes (no magic)
//   - ""        — defaults to "wrong" (raw non-qcow2 content)
//
// SizeBytes controls how many bytes the image body occupies (default 512).
// Must be >= 4 to accommodate magic bytes.
//
// InvalidTar, when true with Format="tgz", writes a tar entry with a
// deliberately corrupted header so archive/tar.Next returns an error.
//
// NegativeSize, when true with Format="tgz", writes a tar entry whose Size
// field contains the GNU base-256 encoding of -1.
//
// NoCandidate, when true with Format="tgz", populates the archive with a
// non-.img file ("manifest.json") so resolveStemcellImage finds no usable
// candidate.
//
// EscapeRoot, when true with Format="tgz", adds an entry whose path traverses
// outside the staging root ("../escape.img").
type stemcellFixtureOpts struct {
	Format       string // "raw", "tgz", "" → defaults to "raw"
	Magic        string // "correct", "wrong", "missing", "" → defaults to "wrong"
	SizeBytes    int    // >= 4; defaults to 512
	InvalidTar   bool
	NegativeSize bool
	NoCandidate  bool
	EscapeRoot   bool
}

// newStemcellFixture writes a stemcell image fixture to a temp file and returns
// its path. The file is cleaned up when t ends.
//
// All inputs are validated; t.Fatal is called on invalid combinations.
func newStemcellFixture(t *testing.T, opts stemcellFixtureOpts) string {
	t.Helper()

	if opts.Format == "" {
		opts.Format = "raw"
	}
	if opts.Magic == "" {
		opts.Magic = "wrong"
	}
	if opts.SizeBytes <= 0 {
		opts.SizeBytes = 512
	}
	if opts.SizeBytes < 4 {
		t.Fatalf("newStemcellFixture: SizeBytes must be >= 4, got %d", opts.SizeBytes)
	}

	switch opts.Format {
	case "raw":
		return writeRawFixture(t, opts)
	case "tgz":
		return writeTgzFixture(t, opts)
	default:
		t.Fatalf("newStemcellFixture: unknown Format %q", opts.Format)
		return ""
	}
}

// writeRawFixture writes a bare image file (no tar wrapper).
func writeRawFixture(t *testing.T, opts stemcellFixtureOpts) string {
	t.Helper()
	body := makeImageBody(opts)
	f, err := os.CreateTemp(t.TempDir(), "stemcell-*.img")
	if err != nil {
		t.Fatalf("newStemcellFixture(raw): create temp: %v", err)
	}
	if _, err := f.Write(body); err != nil {
		t.Fatalf("newStemcellFixture(raw): write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("newStemcellFixture(raw): close: %v", err)
	}
	return f.Name()
}

// writeTgzFixture writes a gzip+tar archive.
func writeTgzFixture(t *testing.T, opts stemcellFixtureOpts) string {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "stemcell-*.tgz")
	if err != nil {
		t.Fatalf("newStemcellFixture(tgz): create temp: %v", err)
	}

	gw, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		_ = f.Close()
		t.Fatalf("newStemcellFixture(tgz): gzip writer: %v", err)
	}
	tw := tar.NewWriter(gw)

	switch {
	case opts.InvalidTar:
		// Write a truncated, corrupt gzip stream that archive/tar.Reader
		// will reject. Close gzip without closing tar so the stream is
		// malformed enough for tar.Next to error.
		_ = gw.Close()
		_ = f.Close()
		return f.Name()

	case opts.NegativeSize:
		// Write a header with GNU base-256 negative size encoding.
		// Delegates to the lower-level builder for byte-exact control.
		_ = tw.Close()
		_ = gw.Close()
		_ = f.Close()
		return makeNegativeSizeTar(t, "root.img")

	case opts.NoCandidate:
		// Populate with a non-.img file so no candidate is found.
		addTarEntry(t, tw, "manifest.json", []byte(`{"name":"ubuntu"}`))

	case opts.EscapeRoot:
		// Entry whose path traverses outside the staging root.
		body := makeImageBody(opts)
		addTarEntry(t, tw, "../escape.img", body)

	default:
		body := makeImageBody(opts)
		addTarEntry(t, tw, "root.img", body)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("newStemcellFixture(tgz): close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("newStemcellFixture(tgz): close gzip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("newStemcellFixture(tgz): close file: %v", err)
	}
	return f.Name()
}

// makeImageBody returns image bytes for the given opts.Magic and opts.SizeBytes.
func makeImageBody(opts stemcellFixtureOpts) []byte {
	b := make([]byte, opts.SizeBytes)
	switch opts.Magic {
	case "correct":
		// qcow2 magic: "QFI\xfb"
		b[0] = 0x51 // Q
		b[1] = 0x46 // F
		b[2] = 0x49 // I
		b[3] = 0xFB
	case "wrong":
		// Java class file magic — clearly not a disk image.
		b[0] = 0xCA
		b[1] = 0xFE
		b[2] = 0xBA
		b[3] = 0xBE
	case "missing":
		// Zero bytes — no magic.
	default:
		panic(fmt.Sprintf("makeImageBody: unknown magic %q", opts.Magic))
	}
	return b
}

// addTarEntry writes one regular file entry to tw.
func addTarEntry(t *testing.T, tw *tar.Writer, name string, data []byte) {
	t.Helper()
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     int64(len(data)),
		Mode:     0o644,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("addTarEntry: write header %s: %v", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatalf("addTarEntry: write data %s: %v", name, err)
	}
}

// ============================================================
// Core fixture builders (used directly or via newStemcellFixture)
// ============================================================

// tempImageFile creates a temp file with fixed deterministic non-qcow2 bytes
// and returns its path. Content is non-qcow2 so format detection returns "raw".
//
// Migrated from create_stemcell_test.go.
func tempImageFile(t *testing.T) string {
	t.Helper()
	return newStemcellFixture(t, stemcellFixtureOpts{
		Format:    "raw",
		Magic:     "wrong",
		SizeBytes: 24,
	})
}

// makeStemcellTar builds a gzip+tar archive containing the given files and
// writes it to a temp file. Each entry in files maps filename → content bytes.
//
// Migrated from create_stemcell_test.go.
func makeStemcellTar(t *testing.T, files map[string][]byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stemcell-*.tgz")
	if err != nil {
		t.Fatalf("makeStemcellTar: create: %v", err)
	}

	gw, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		t.Fatalf("makeStemcellTar: gzip writer: %v", err)
	}
	tw := tar.NewWriter(gw)

	for name, data := range files {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Size:     int64(len(data)),
			Mode:     0o644,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("makeStemcellTar: write header %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("makeStemcellTar: write data %s: %v", name, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("makeStemcellTar: close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("makeStemcellTar: close gzip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("makeStemcellTar: close file: %v", err)
	}
	return f.Name()
}

// qcow2Bytes returns a minimal valid-magic qcow2 header padded to size bytes.
//
// Migrated from create_stemcell_test.go.
func qcow2Bytes(size int) []byte {
	b := make([]byte, size)
	b[0] = 'Q'
	b[1] = 'F'
	b[2] = 'I'
	b[3] = 0xFB
	return b
}

// makeNegativeSizeTar builds a gzip+tar whose sole entry carries a GNU
// base-256 negative Size field. Used to test the invalid-tar-header guard.
//
// Migrated from create_stemcell_test.go.
func makeNegativeSizeTar(t *testing.T, name string) string {
	t.Helper()
	// Step 1: build a valid 1-block ustar archive (no body) with the Writer.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     0,
		Mode:     0o644,
		Format:   tar.FormatUSTAR,
	}
	if werr := tw.WriteHeader(hdr); werr != nil {
		t.Fatalf("makeNegativeSizeTar: write header: %v", werr)
	}
	if cerr := tw.Close(); cerr != nil {
		t.Fatalf("makeNegativeSizeTar: close writer: %v", cerr)
	}

	raw := buf.Bytes()
	if len(raw) < 512 {
		t.Fatalf("makeNegativeSizeTar: writer produced %d bytes; expected >= 512", len(raw))
	}

	// Step 2: overwrite the Size field (bytes 124..135 of the first header)
	// with GNU base-256 -1 (0xFF repeated; high bit signals binary).
	for i := 124; i < 136; i++ {
		raw[i] = 0xFF
	}

	// Step 3: blank the checksum field (148..155) to spaces, recompute the
	// 6-octal-digit checksum, then write it back as "NNNNNN\x00 ".
	for i := 148; i < 156; i++ {
		raw[i] = ' '
	}
	var sum uint32
	for i := 0; i < 512; i++ {
		sum += uint32(raw[i])
	}
	copy(raw[148:154], fmtOctal(sum, 6))
	raw[154] = 0x00
	raw[155] = ' '

	// Step 4: wrap in gzip and write to a temp file.
	f, err := os.CreateTemp(t.TempDir(), "stemcell-negsize-*.tgz")
	if err != nil {
		t.Fatalf("makeNegativeSizeTar: create: %v", err)
	}
	gw, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		t.Fatalf("makeNegativeSizeTar: gzip: %v", err)
	}
	if _, werr := gw.Write(raw); werr != nil {
		t.Fatalf("makeNegativeSizeTar: gzip write: %v", werr)
	}
	if cerr := gw.Close(); cerr != nil {
		t.Fatalf("makeNegativeSizeTar: gzip close: %v", cerr)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatalf("makeNegativeSizeTar: file close: %v", cerr)
	}
	return f.Name()
}

// fmtOctal renders v as an n-digit zero-padded octal string.
//
// Migrated from create_stemcell_test.go.
func fmtOctal(v uint32, n int) string {
	buf := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		buf[i] = byte('0' + (v & 0o7))
		v >>= 3
	}
	return string(buf)
}

// ============================================================
// SHA computation helper — used by dedup tests
// ============================================================

// computeFileSHA hashes the contents of path and returns the hex digest.
// Mirrors the production sha256FilePath helper so tests can predict the CID.
//
// Migrated from create_stemcell_test.go.
func computeFileSHA(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
