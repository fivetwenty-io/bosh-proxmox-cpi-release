package client

import (
	"io"

	pvehttp "github.com/fivetwenty-io/proxmox-apiclient-go/v3/internal/http"
)

// ErrHostOverrideFingerprint is returned when a request carries a WithHost
// override while TLS fingerprint pinning is enabled; see WithHost.
var ErrHostOverrideFingerprint = pvehttp.ErrHostOverrideFingerprint

// NewSizedReader wraps r with its exact byte count so file uploads stream the
// multipart body with an explicit Content-Length instead of buffering the
// whole payload in memory to measure it. An *os.File needs no wrapping — its
// size is detected via Stat — but any other reader of known size should be
// wrapped before being passed to an upload method. The declared size must
// match the bytes r yields exactly: a short reader fails the upload, and
// surplus bytes are not read.
//
// A streamed upload body cannot be replayed by the client's internal retry or
// 401 re-authentication paths (the stream is consumed by the first attempt);
// callers needing retries should re-open the source and call the upload again.
func NewSizedReader(r io.Reader, size int64) io.Reader {
	return pvehttp.NewSizedReader(r, size)
}
