package stemcellfetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

// fakeUploader captures upload call arguments and streams from body to f.body.
type fakeUploader struct {
	calledWith struct{ node, storage, content, filename string }
	body       []byte
	upid       string
	err        error
}

func (f *fakeUploader) Upload(_ context.Context, node, storage, content, filename string, body io.Reader) (string, error) {
	f.calledWith.node = node
	f.calledWith.storage = storage
	f.calledWith.content = content
	f.calledWith.filename = filename
	if f.err != nil {
		// Drain nothing so BytesRead stays at 0 for tests that check partial progress.
		return "", f.err
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	f.body = data
	return f.upid, nil
}

// TestStreamingSink_HashesCorrectly writes a known string into a StreamingSink
// and verifies Sum() matches crypto/sha256.
func TestStreamingSink_HashesCorrectly(t *testing.T) {
	t.Parallel()
	payload := "hello, bosh stemcell fetch"
	sink := NewStreamingSink(strings.NewReader(payload))

	out, err := io.ReadAll(sink.Reader)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	if string(out) != payload {
		t.Fatalf("reader output %q; want %q", out, payload)
	}

	want := sha256Hex([]byte(payload))
	got := sink.Sum()
	if got != want {
		t.Errorf("Sum() = %q; want %q", got, want)
	}
	if sink.BytesRead() != int64(len(payload)) {
		t.Errorf("BytesRead() = %d; want %d", sink.BytesRead(), len(payload))
	}
}

// TestStreamingSink_ChunkedReads reads in small 32-byte chunks and verifies the
// final hash is identical to a single-read result.
func TestStreamingSink_ChunkedReads(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("abcdefghijklmnopqrstuvwxyz012345"), 8) // 256 bytes
	sink := NewStreamingSink(bytes.NewReader(payload))

	buf := make([]byte, 32)
	var accumulated []byte
	for {
		n, err := sink.Reader.Read(buf)
		if n > 0 {
			accumulated = append(accumulated, buf[:n]...)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}

	if !bytes.Equal(accumulated, payload) {
		t.Fatal("accumulated data does not match original payload")
	}

	want := sha256Hex(payload)
	got := sink.Sum()
	if got != want {
		t.Errorf("chunked Sum() = %q; want %q", got, want)
	}
	if sink.BytesRead() != int64(len(payload)) {
		t.Errorf("BytesRead() = %d; want %d", sink.BytesRead(), len(payload))
	}
}

// TestStreamToPVE_HappyPath verifies StreamToPVE returns correct sha256, upid,
// and byte count on a successful upload.
func TestStreamToPVE_HappyPath(t *testing.T) {
	t.Parallel()
	payload := []byte("this is a fake stemcell qcow2 body")
	uploader := &fakeUploader{upid: "UPID:testnode:00001234:00000001:67890ABC:upload:local:root@pam:"}

	sha256hex, upid, bytesUploaded, err := StreamToPVE(
		context.Background(),
		uploader,
		"testnode", "local", "bosh-stemcell-ubuntu-jammy-1.438-00000000.qcow2.partial",
		bytes.NewReader(payload),
	)

	if err != nil {
		t.Fatalf("StreamToPVE error: %v", err)
	}

	want := sha256Hex(payload)
	if sha256hex != want {
		t.Errorf("sha256hex = %q; want %q", sha256hex, want)
	}
	if upid != uploader.upid {
		t.Errorf("upid = %q; want %q", upid, uploader.upid)
	}
	if bytesUploaded != int64(len(payload)) {
		t.Errorf("bytesUploaded = %d; want %d", bytesUploaded, len(payload))
	}

	// Verify the uploader received the correct arguments.
	if uploader.calledWith.node != "testnode" {
		t.Errorf("uploader node = %q; want testnode", uploader.calledWith.node)
	}
	if uploader.calledWith.storage != "local" {
		t.Errorf("uploader storage = %q; want local", uploader.calledWith.storage)
	}
	if uploader.calledWith.content != "import" {
		t.Errorf("uploader content = %q; want import", uploader.calledWith.content)
	}
	if uploader.calledWith.filename != "bosh-stemcell-ubuntu-jammy-1.438-00000000.qcow2.partial" {
		t.Errorf("uploader filename = %q; want .partial name", uploader.calledWith.filename)
	}

	// Uploader must have received the exact bytes.
	if !bytes.Equal(uploader.body, payload) {
		t.Errorf("uploader body mismatch: got %d bytes; want %d", len(uploader.body), len(payload))
	}
}

// TestStreamToPVE_UploaderError verifies that an uploader error is wrapped
// with node/storage/filename context.
func TestStreamToPVE_UploaderError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("PVE API 500")
	uploader := &fakeUploader{err: sentinel}

	_, _, _, err := StreamToPVE(
		context.Background(),
		uploader,
		"node1", "ceph-pool", "stem.qcow2.partial",
		strings.NewReader("data"),
	)

	if err == nil {
		t.Fatal("expected error; got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain does not contain sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), "node1") {
		t.Errorf("error missing node context: %v", err)
	}
	if !strings.Contains(err.Error(), "ceph-pool") {
		t.Errorf("error missing storage context: %v", err)
	}
	if !strings.Contains(err.Error(), "stem.qcow2.partial") {
		t.Errorf("error missing filename context: %v", err)
	}
}

// TestStreamToPVE_NilUploader verifies the nil-uploader guard.
func TestStreamToPVE_NilUploader(t *testing.T) {
	t.Parallel()
	_, _, _, err := StreamToPVE(
		context.Background(),
		nil,
		"node", "storage", "file.partial",
		strings.NewReader("data"),
	)
	if err == nil {
		t.Fatal("expected error for nil uploader; got nil")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("expected 'nil' in error message; got %q", err.Error())
	}
}

// TestStreamToPVE_EmptyPartialFilename verifies the empty-filename guard.
func TestStreamToPVE_EmptyPartialFilename(t *testing.T) {
	t.Parallel()
	uploader := &fakeUploader{}
	_, _, _, err := StreamToPVE(
		context.Background(),
		uploader,
		"node", "storage", "",
		strings.NewReader("data"),
	)
	if err == nil {
		t.Fatal("expected error for empty partialFilename; got nil")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected 'empty' in error message; got %q", err.Error())
	}
}

// sha256Hex is a test helper that computes the hex-encoded SHA-256 of b.
func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
