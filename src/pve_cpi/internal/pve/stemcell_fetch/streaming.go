package stemcellfetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
)

// StreamingSink wraps an io.Reader with an in-flight SHA-256 hasher. The
// caller reads from Reader and consumes the streamed body; once the read is
// complete, Sum() returns the final SHA-256 hex digest.
//
// Usage:
//
//	sink := NewStreamingSink(body)
//	// Stream sink.Reader to PVE Upload API (or any consumer)
//	sha256hex, err := sink.Sum(), <check reader error>
//
// The sink does NOT buffer the stream — the hash is computed via io.TeeReader
// so memory footprint is bounded by the consumer's read buffer size.
type StreamingSink struct {
	source    io.Reader
	hasher    hash.Hash
	counter   byteCounter
	Reader    io.Reader // pass to consumer (PVE upload streaming)
	bytesRead int64
}

// NewStreamingSink wraps source so reads from sink.Reader simultaneously
// update the SHA-256 hasher. The source is not closed by the sink; caller
// retains close responsibility.
func NewStreamingSink(source io.Reader) *StreamingSink {
	s := &StreamingSink{
		source: source,
		hasher: sha256.New(),
	}
	s.counter = byteCounter{hasher: s.hasher, total: &s.bytesRead}
	s.Reader = io.TeeReader(source, &s.counter)
	return s
}

// Sum returns the hex-encoded SHA-256 of all bytes read through sink.Reader
// up to this point. Safe to call once the stream has been fully consumed.
func (s *StreamingSink) Sum() string {
	return hex.EncodeToString(s.hasher.Sum(nil))
}

// BytesRead returns the cumulative byte count streamed through the sink.
func (s *StreamingSink) BytesRead() int64 {
	return s.bytesRead
}

// byteCounter is a Writer that forwards writes to a hasher and increments a
// total-byte counter. Used as the io.TeeReader destination so every read from
// StreamingSink.Reader is hashed transparently.
type byteCounter struct {
	hasher hash.Hash
	total  *int64
}

func (b *byteCounter) Write(p []byte) (int, error) {
	n, err := b.hasher.Write(p)
	if n > 0 && b.total != nil {
		*b.total += int64(n)
	}
	return n, err
}

// PVEUploader is the narrow interface required for a streaming multipart
// upload to PVE's /upload endpoint. Production wiring uses the Storage
// service returned by the PVE client; tests substitute a fake implementation.
type PVEUploader interface {
	// Upload streams body into <storage> on <node> as the given content type
	// and filename. Returns the PVE UPID for async task tracking, or an empty
	// string on synchronous completion (i.e. the node processed it inline).
	Upload(ctx context.Context, node, storage, content, filename string, body io.Reader) (upid string, err error)
}

// StreamToPVE streams source into PVE storage under partialFilename while
// computing SHA-256 in flight. It returns the hex digest, the PVE UPID (may
// be empty on synchronous uploads), and the total bytes transferred.
//
// Caller responsibilities:
//   - close source after this call returns
//   - if upid is non-empty, await the upload task via pve.AwaitTask before
//     treating the volume as ready
//   - rename the partial volume to its final canonical name using the
//     returned sha256hex to construct that name via BuildFetchedFilename
//   - on error, clean up any orphan .partial volume on PVE storage
//
// Failure modes:
//   - nil uploader → error "PVEUploader is nil"
//   - empty partialFilename → error "partialFilename is empty"
//   - uploader.Upload error → wrapped error with node/storage/filename context
func StreamToPVE(
	ctx context.Context,
	uploader PVEUploader,
	node, storage, partialFilename string,
	source io.Reader,
) (sha256hex string, upid string, bytesUploaded int64, err error) {
	if uploader == nil {
		return "", "", 0, fmt.Errorf("stemcell_fetch: PVEUploader is nil")
	}
	if partialFilename == "" {
		return "", "", 0, fmt.Errorf("stemcell_fetch: partialFilename is empty")
	}

	sink := NewStreamingSink(source)
	upid, err = uploader.Upload(ctx, node, storage, "import", partialFilename, sink.Reader)
	if err != nil {
		return "", "", sink.BytesRead(), fmt.Errorf(
			"stemcell_fetch: upload to %s:%s (%s): %w", node, storage, partialFilename, err)
	}
	return sink.Sum(), upid, sink.BytesRead(), nil
}
