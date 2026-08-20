package pve_test

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"reflect"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// FuzzParseEncodedDiskCID feeds arbitrary byte slices (as strings) to
// pve.ParseEncodedDiskCID, the parse boundary for every disk_cid the BOSH
// Director hands back to detach_disk, resize_disk, set_disk_metadata,
// delete_disk, has_disk, attach_disk, snapshot_disk, update_disk, and
// create_vm's fault-domain co-location scan. ParseEncodedDiskCID must never
// panic, and its two documented outcomes each carry their own contract:
//
//   - err == nil: bareCID is non-empty (the envelope decoder itself rejects
//     an empty "v" field), and re-encoding (bareCID, meta) via
//     pve.EncodeDiskCID produces a CID that itself parses back to the exact
//     same (bareCID, meta) pair — round-trip totality.
//   - err != nil: bareCID == "" and meta == nil (the decoder never returns a
//     partial result alongside an error).
func FuzzParseEncodedDiskCID(f *testing.F) {
	// Valid pvd- envelope: nil meta.
	f.Add(mustFuzzEncode(f, "local-lvm:vm-100-disk-0", nil))

	// Valid pvd- envelope: full meta (pool, node, AZ, opts, anchor, format, ID).
	f.Add(mustFuzzEncode(f, "data:vm-9003-disk-0", &pve.DiskCIDMeta{
		Pool:   "data",
		Node:   "pve1",
		AZ:     "z1",
		Opts:   map[string]string{"iothread": "1", "cache": "writeback"},
		Anchor: true,
		Format: "qcow2",
		ID:     "bpd-0123456789abcdef",
	}))

	// Valid pvd- envelope: anchor-only, format-only, and ID-only metas — each
	// single field must survive marshalDiskCIDEnvelope's all-zero check on
	// its own.
	f.Add(mustFuzzEncode(f, "local-lvm:vm-9001-disk-0", &pve.DiskCIDMeta{Anchor: true}))
	f.Add(mustFuzzEncode(f, "local-lvm:vm-9002-disk-0", &pve.DiskCIDMeta{Format: "raw"}))
	f.Add(mustFuzzEncode(f, "local-lvm:vm-9004-disk-0", &pve.DiskCIDMeta{ID: "bpd-fedcba9876543210"}))

	// An envelope whose stable ID exceeds PVE's 20-byte drive-serial cap: a
	// hard decode error, never a partial result.
	f.Add("pvd-" + base64.RawURLEncoding.EncodeToString(
		[]byte(`{"v":"local-lvm:vm-1-disk-0","m":{"id":"bpd-00112233445566778899aabb"}}`)))

	// Valid pvd- envelope: path-form volid (dir-style storage, embeds "/" and ".").
	f.Add(mustFuzzEncode(f, "local:9001/vm-9001-disk-0.qcow2", &pve.DiskCIDMeta{Pool: "local"}))

	// Valid pvz- (compressed) envelope: forces the gzip decode path.
	f.Add(mustFuzzEncodeCompressed(f, "ceph-rbd-nvme-tier1:300/vm-300-disk-0.qcow2", &pve.DiskCIDMeta{
		Pool: "ceph-rbd-nvme-tier1",
		Node: "prod-pmx-node-07",
		AZ:   "az-rack-2",
		Opts: map[string]string{
			"iothread": "1", "cache": "writeback", "discard": "on", "ssd": "1",
			"mbps_rd": "120", "mbps_wr": "120", "iops_rd": "8000", "iops_wr": "8000",
		},
	}))

	// Empty input.
	f.Add("")

	// Garbage: no recognized prefix at all.
	f.Add("garbage-not-a-cid")
	f.Add("local-lvm:vm-100-disk-0") // bare volid — no envelope prefix, hard error

	// Truncated / malformed base64 payloads.
	f.Add("pvd-")
	f.Add("pvd-!!!notbase64")
	f.Add("pvd-YWJj") // valid base64 ("abc") but not JSON
	f.Add("pvz-")
	f.Add("pvz-!!!notbase64")

	// A pvz- payload that is valid base64url but not a gzip stream at all.
	f.Add("pvz-" + base64.RawURLEncoding.EncodeToString([]byte("plainbytesnotgzip")))

	// A storage literally named "pvd-"/"pvz-": bare CID starting with the
	// envelope prefix but containing ':' (outside the base64url alphabet).
	f.Add("pvd-foo:vm-100-disk-0")
	f.Add("pvz-foo:vm-100-disk-0")

	// Legacy pipe-suffixed form from releases predating the envelope: no
	// longer decodable — pre-release software carries no backward
	// compatibility requirement.
	f.Add("local-lvm:vm-100-disk-0|eyJwb29sIjoiZGF0YSJ9")

	// >255-character input in both accepted forms, and as pure garbage.
	longBare := "s"
	for i := 0; i < 9; i++ {
		longBare += longBare
	} // 512 "s" characters
	f.Add(mustFuzzEncode(f, longBare+":vm-1-disk-0", nil))
	longGarbage := "x"
	for i := 0; i < 9; i++ {
		longGarbage += longGarbage
	}
	f.Add("pvd-" + longGarbage)

	// A pvz- payload whose decompressed content exceeds the decompression-bomb
	// cap: gzip of 10 MiB of zeroes.
	f.Add(mustFuzzGzipCID(f, bytes.Repeat([]byte("0"), 10<<20)))

	// A pvd- envelope with an empty "v" field.
	f.Add("pvd-" + base64.RawURLEncoding.EncodeToString([]byte(`{"v":""}`)))
	// A pvd- envelope whose payload is valid base64url + valid JSON but not
	// an object with "v" (a JSON array).
	f.Add("pvd-" + base64.RawURLEncoding.EncodeToString([]byte(`[1,2,3]`)))

	f.Fuzz(func(t *testing.T, cid string) {
		bareCID, meta, err := pve.ParseEncodedDiskCID(cid)

		if err != nil {
			if bareCID != "" {
				t.Fatalf("ParseEncodedDiskCID(%q): non-empty bareCID %q alongside non-nil err %v", cid, bareCID, err)
			}
			if meta != nil {
				t.Fatalf("ParseEncodedDiskCID(%q): non-nil meta %+v alongside non-nil err %v", cid, meta, err)
			}
			return
		}

		if bareCID == "" {
			t.Fatalf("ParseEncodedDiskCID(%q): empty bareCID with nil err", cid)
		}

		// Round-trip totality: re-encoding the decoded (bareCID, meta) via
		// EncodeDiskCID must produce a CID that decodes back to the exact
		// same pair. EncodeDiskCID never errors here because bareCID is
		// guaranteed non-empty by the check above.
		reencoded, encErr := pve.EncodeDiskCID(bareCID, meta)
		if encErr != nil {
			t.Fatalf("ParseEncodedDiskCID(%q): re-encode of decoded (bareCID=%q, meta=%+v) failed: %v", cid, bareCID, meta, encErr)
		}
		gotBareCID, gotMeta, reErr := pve.ParseEncodedDiskCID(reencoded)
		if reErr != nil {
			t.Fatalf("ParseEncodedDiskCID(%q): re-encoded CID %q failed to parse: %v", cid, reencoded, reErr)
		}
		if gotBareCID != bareCID {
			t.Fatalf("ParseEncodedDiskCID(%q): round-trip bareCID mismatch: got %q, want %q", cid, gotBareCID, bareCID)
		}
		if !metaEqual(gotMeta, meta) {
			t.Fatalf("ParseEncodedDiskCID(%q): round-trip meta mismatch: got %+v, want %+v", cid, gotMeta, meta)
		}
	})
}

// metaEqual compares two *pve.DiskCIDMeta for the round-trip property.
//
// Two normalizations are required because EncodeDiskCID's documented
// contract ("a nil or all-zero meta is omitted from the payload", disk.go)
// means the encoder cannot tell apart a nil meta from a non-nil meta whose
// every field is its zero value — both encode identically (the "m" key is
// omitted entirely) and both therefore decode back as a nil meta:
//
//  1. A nil pointer and a pointer to an all-zero DiskCIDMeta (Pool/Node/AZ
//     all "" and Opts nil-or-empty) are equal.
//  2. Within two non-zero metas, a nil Opts map and an empty (non-nil) Opts
//     map are equal (same omitempty reasoning applied to that one field).
func metaEqual(a, b *pve.DiskCIDMeta) bool {
	aZero, bZero := isZeroDiskCIDMeta(a), isZeroDiskCIDMeta(b)
	if aZero || bZero {
		return aZero == bZero
	}
	if a.Pool != b.Pool || a.Node != b.Node || a.AZ != b.AZ || a.Anchor != b.Anchor || a.Format != b.Format || a.ID != b.ID {
		return false
	}
	if len(a.Opts) == 0 && len(b.Opts) == 0 {
		return true
	}
	return reflect.DeepEqual(a.Opts, b.Opts)
}

// isZeroDiskCIDMeta reports whether m is nil or every field holds its zero
// value (an empty, possibly non-nil, Opts map counts as zero).
func isZeroDiskCIDMeta(m *pve.DiskCIDMeta) bool {
	return m == nil || (m.Pool == "" && m.Node == "" && m.AZ == "" && len(m.Opts) == 0 && !m.Anchor && m.Format == "" && m.ID == "")
}

// mustFuzzEncode builds a pvd- seed corpus entry, failing the fuzz setup (not
// a fuzz iteration) if the encoder itself errors — every seed here passes a
// non-empty bareCID, so an error would indicate a bug in the seed data, not
// the property under test.
func mustFuzzEncode(f *testing.F, bareCID string, meta *pve.DiskCIDMeta) string {
	f.Helper()
	got, err := pve.EncodeDiskCID(bareCID, meta)
	if err != nil {
		f.Fatalf("EncodeDiskCID(%q): %v", bareCID, err)
	}
	return got
}

// mustFuzzEncodeCompressed builds a seed corpus entry via
// EncodeDiskCIDCompressed. The caller supplies a bareCID/meta combination
// whose plain pvd- form overflows DiskCIDLengthTarget, so the encoder emits
// the pvz- form — seeding the fuzzer's corpus with the compressed decode path
// alongside the plain pvd- seeds above.
func mustFuzzEncodeCompressed(f *testing.F, bareCID string, meta *pve.DiskCIDMeta) string {
	f.Helper()
	got, err := pve.EncodeDiskCIDCompressed(bareCID, meta)
	if err != nil {
		f.Fatalf("EncodeDiskCIDCompressed(%q): %v", bareCID, err)
	}
	return got
}

// mustFuzzGzipCID builds a raw "pvz-<base64url(gzip(data))>" seed corpus
// entry directly (bypassing the JSON envelope), for seeding decompression-cap
// and malformed-payload edge cases that EncodeDiskCIDCompressed cannot
// produce on its own (it always emits valid JSON inside the gzip stream).
func mustFuzzGzipCID(f *testing.F, data []byte) string {
	f.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		f.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		f.Fatalf("gzip close: %v", err)
	}
	return "pvz-" + base64.RawURLEncoding.EncodeToString(buf.Bytes())
}
