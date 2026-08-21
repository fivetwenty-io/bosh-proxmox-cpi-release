package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/url"
	"strings"
)

var (
	ErrUnsupportedMethod = errors.New("unsupported method")
	// ErrFileShorterThanDeclared is returned when a size-aware upload reader
	// yields fewer bytes than the size it declared. The multipart body's
	// Content-Length is computed from the declared size, so a short read would
	// otherwise stall the request waiting for bytes that never come.
	ErrFileShorterThanDeclared = errors.New("file yielded fewer bytes than its declared size")
)

// formFile is one file part of a multipart upload. When sized is true the
// exact byte count is known up front and the part can be streamed; otherwise
// the whole body must be buffered to learn its length.
type formFile struct {
	filename string
	r        io.Reader
	size     int64
	sized    bool
}

// SizedReader pairs a reader with its exact byte count so multipart uploads
// can stream it with an explicit Content-Length instead of buffering the whole
// payload in memory to measure it. The declared size must match the bytes the
// reader yields exactly; a short reader fails the upload with
// ErrFileShorterThanDeclared, and surplus bytes are not read.
type SizedReader struct {
	r    io.Reader
	size int64
}

// NewSizedReader wraps r with its exact byte count. Pass the result to any
// upload path that accepts an io.Reader to opt in to streaming.
func NewSizedReader(r io.Reader, size int64) *SizedReader {
	return &SizedReader{r: r, size: size}
}

func (s *SizedReader) Read(p []byte) (int, error) {
	return s.r.Read(p) //nolint:wrapcheck // io.Reader contract: pass io.EOF and source errors through verbatim
}

// Size returns the exact number of bytes the reader will yield.
func (s *SizedReader) Size() int64 { return s.size }

// RequestBuilder helps construct HTTP requests for the PVE API.
type RequestBuilder struct {
	method      string
	baseURL     string
	path        string
	queryParams url.Values
	formParams  url.Values
	jsonBody    interface{}
	headers     map[string]string
	files       map[string]formFile
}

// NewRequestBuilder creates a new request builder.
func NewRequestBuilder(method, baseURL, path string) *RequestBuilder {
	return &RequestBuilder{
		method:      method,
		baseURL:     baseURL,
		path:        path,
		queryParams: url.Values{},
		formParams:  url.Values{},
		headers:     make(map[string]string),
		files:       make(map[string]formFile),
	}
}

// AddQueryParam adds a query parameter to the request.
func (rb *RequestBuilder) AddQueryParam(key string, value interface{}) *RequestBuilder {
	addEncodedParam(rb.queryParams, key, value)

	return rb
}

// AddQueryParams adds multiple query parameters.
func (rb *RequestBuilder) AddQueryParams(params map[string]interface{}) *RequestBuilder {
	for key, value := range params {
		rb.AddQueryParam(key, value)
	}

	return rb
}

// AddFormParam adds a form parameter to the request.
func (rb *RequestBuilder) AddFormParam(key string, value interface{}) *RequestBuilder {
	addEncodedParam(rb.formParams, key, value)

	return rb
}

// AddFormParams adds multiple form parameters.
func (rb *RequestBuilder) AddFormParams(params map[string]interface{}) *RequestBuilder {
	for key, value := range params {
		rb.AddFormParam(key, value)
	}

	return rb
}

// SetJSONBody sets the JSON body for the request.
func (rb *RequestBuilder) SetJSONBody(body interface{}) *RequestBuilder {
	rb.jsonBody = body

	return rb
}

// AddHeader adds a header to the request.
func (rb *RequestBuilder) AddHeader(key, value string) *RequestBuilder {
	rb.headers[key] = value

	return rb
}

// AddHeaders adds multiple headers to the request.
func (rb *RequestBuilder) AddHeaders(headers map[string]string) *RequestBuilder {
	for key, value := range headers {
		rb.headers[key] = value
	}

	return rb
}

// AddFile adds a file to be uploaded with the given filename.
//
// When the reader's exact byte count is determinable — a *SizedReader, or a
// stat-able regular file such as *os.File — the multipart body is streamed
// with an explicit Content-Length. Otherwise the whole file is buffered in
// memory to measure it (the historical behavior); wrap large readers of known
// size with NewSizedReader, or use AddFileWithSize, to avoid the buffer.
func (rb *RequestBuilder) AddFile(fieldName, filename string, file io.Reader) *RequestBuilder {
	if size, ok := detectReaderSize(file); ok {
		return rb.AddFileWithSize(fieldName, filename, file, size)
	}

	rb.files[fieldName] = formFile{filename: filename, r: file}

	return rb
}

// AddFileWithSize adds a file whose exact byte count is known, allowing the
// multipart body to be streamed with an explicit Content-Length instead of
// buffered. The reader must yield exactly size bytes; fewer fails the upload
// with ErrFileShorterThanDeclared and surplus bytes are not read. A negative
// size means unknown and falls back to the buffered AddFile behavior.
func (rb *RequestBuilder) AddFileWithSize(fieldName, filename string, file io.Reader, size int64) *RequestBuilder {
	if size < 0 {
		rb.files[fieldName] = formFile{filename: filename, r: file}

		return rb
	}

	rb.files[fieldName] = formFile{filename: filename, r: file, size: size, sized: true}

	return rb
}

// detectReaderSize reports the exact number of bytes r will yield, when that
// can be determined without consuming it: an explicit *SizedReader wrapper, or
// a stat-able regular file (*os.File and equivalents), adjusted for the
// current seek offset so a partially-read file declares only its remainder.
func detectReaderSize(r io.Reader) (int64, bool) {
	if sr, ok := r.(*SizedReader); ok {
		if sr.size < 0 {
			return 0, false
		}

		return sr.size, true
	}

	type statter interface {
		Stat() (fs.FileInfo, error)
	}

	st, ok := r.(statter)
	if !ok {
		return 0, false
	}

	info, err := st.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return 0, false
	}

	size := info.Size()

	if seeker, ok := r.(io.Seeker); ok {
		offset, seekErr := seeker.Seek(0, io.SeekCurrent)
		if seekErr != nil {
			return 0, false
		}

		size -= offset
	}

	if size < 0 {
		return 0, false
	}

	return size, true
}

// Build constructs the final URL for the request.
func (rb *RequestBuilder) BuildURL() string {
	// Ensure path starts with /
	if !strings.HasPrefix(rb.path, "/") {
		rb.path = "/" + rb.path
	}

	fullURL := rb.baseURL + rb.path

	// Add query parameters
	if len(rb.queryParams) > 0 {
		fullURL += "?" + rb.queryParams.Encode()
	}

	return fullURL
}

// BuildBody constructs the request body. It returns the body reader, its
// content type, and the exact body length in bytes; a length of -1 means the
// length is unknown to the builder (bodyless requests, or a buffered body
// whose length net/http derives from its concrete type).
func (rb *RequestBuilder) BuildBody() (io.Reader, string, int64, error) {
	// Handle different body types based on method and content
	switch rb.method {
	case "GET", "DELETE":
		// No body for GET and DELETE
		return nil, "", -1, nil

	case "POST", "PUT", "PATCH":
		// Check if we have files to upload
		if len(rb.files) > 0 {
			return rb.buildMultipartBody()
		}

		// Check if we have JSON body
		if rb.jsonBody != nil {
			body, err := json.Marshal(rb.jsonBody)
			if err != nil {
				return nil, "", -1, fmt.Errorf("failed to marshal JSON body: %w", err)
			}

			return bytes.NewReader(body), contentTypeJSON, int64(len(body)), nil
		}

		// Default to form-encoded body
		if len(rb.formParams) > 0 {
			body := rb.formParams.Encode()

			return strings.NewReader(body), contentTypeFormURLEncoded, int64(len(body)), nil
		}

		// No body
		return nil, "", -1, nil

	default:
		return nil, "", -1, fmt.Errorf("%w: %s", ErrUnsupportedMethod, rb.method)
	}
}

// buildMultipartBody builds a multipart form body for file uploads. When every
// file part carries a known size the body is streamed with an exact,
// precomputed length; otherwise the whole body is buffered to measure it.
func (rb *RequestBuilder) buildMultipartBody() (io.Reader, string, int64, error) {
	if rb.allFilesSized() {
		return rb.buildStreamingMultipartBody()
	}

	var buffer bytes.Buffer

	writer := multipart.NewWriter(&buffer)

	err := rb.writeMultipartParts(writer)
	if err != nil {
		return nil, "", -1, err
	}

	err = writer.Close()
	if err != nil {
		return nil, "", -1, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	return &buffer, writer.FormDataContentType(), int64(buffer.Len()), nil
}

// allFilesSized reports whether every file part declared an exact size.
func (rb *RequestBuilder) allFilesSized() bool {
	for _, fileData := range rb.files {
		if !fileData.sized {
			return false
		}
	}

	return true
}

// buildStreamingMultipartBody streams the multipart body through a pipe
// instead of buffering it, returning the exact total body length so the
// request can carry an explicit Content-Length (PVE's proxy rejects chunked
// transfer-encoding). The length is computed up front by writing the framing —
// field parts, file part headers, closing boundary — through a counting
// writer with the boundary pinned to the one the streaming writer will use,
// then adding the declared file sizes.
func (rb *RequestBuilder) buildStreamingMultipartBody() (io.Reader, string, int64, error) {
	var framing int64

	meta := multipart.NewWriter(countingWriter{n: &framing})
	boundary := meta.Boundary()

	for key, values := range rb.formParams {
		for _, value := range values {
			err := meta.WriteField(key, value)
			if err != nil {
				return nil, "", -1, fmt.Errorf("failed to write field %s: %w", key, err)
			}
		}
	}

	var filesTotal int64

	for fieldName, fileData := range rb.files {
		_, err := meta.CreateFormFile(fieldName, fileData.filename)
		if err != nil {
			return nil, "", -1, fmt.Errorf("failed to create form file %s: %w", fieldName, err)
		}

		filesTotal += fileData.size
	}

	err := meta.Close()
	if err != nil {
		return nil, "", -1, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	pipeReader, pipeWriter := io.Pipe()

	go func() {
		writer := multipart.NewWriter(pipeWriter)

		// Pin the boundary to the one the length was computed against so the
		// streamed framing bytes match the precomputed total exactly.
		writeErr := writer.SetBoundary(boundary)
		if writeErr == nil {
			writeErr = rb.writeMultipartParts(writer)
		}

		if writeErr == nil {
			writeErr = writer.Close()
		}

		// CloseWithError(nil) closes the pipe normally; a non-nil error is
		// surfaced to the reading side (the HTTP transport), which aborts the
		// request instead of hanging on a short body. Either way the goroutine
		// terminates: writes fail with ErrClosedPipe once the reader is closed,
		// so an abandoned request cannot leak it.
		_ = pipeWriter.CloseWithError(writeErr)
	}()

	return pipeReader, meta.FormDataContentType(), framing + filesTotal, nil
}

// writeMultipartParts writes the form fields and file parts to writer. Sized
// file parts are copied with an exact byte budget so the bytes written always
// match the length the body declared: a short reader is an error, surplus
// bytes are not read.
func (rb *RequestBuilder) writeMultipartParts(writer *multipart.Writer) error {
	for key, values := range rb.formParams {
		for _, value := range values {
			err := writer.WriteField(key, value)
			if err != nil {
				return fmt.Errorf("failed to write field %s: %w", key, err)
			}
		}
	}

	for fieldName, fileData := range rb.files {
		part, err := writer.CreateFormFile(fieldName, fileData.filename)
		if err != nil {
			return fmt.Errorf("failed to create form file %s: %w", fieldName, err)
		}

		if fileData.sized {
			copied, copyErr := io.CopyN(part, fileData.r, fileData.size)
			if errors.Is(copyErr, io.EOF) {
				return fmt.Errorf("file %s yielded %d of %d declared bytes: %w",
					fieldName, copied, fileData.size, ErrFileShorterThanDeclared)
			}

			if copyErr != nil {
				return fmt.Errorf("failed to copy file %s: %w", fieldName, copyErr)
			}

			continue
		}

		_, err = io.Copy(part, fileData.r)
		if err != nil {
			return fmt.Errorf("failed to copy file %s: %w", fieldName, err)
		}
	}

	return nil
}

// countingWriter counts bytes written to it, discarding the data.
type countingWriter struct{ n *int64 }

func (w countingWriter) Write(p []byte) (int, error) {
	*w.n += int64(len(p))

	return len(p), nil
}

// RequestConfig contains configuration for building requests.
type RequestConfig struct {
	// BaseURL is the base URL for all requests
	BaseURL string

	// DefaultHeaders are headers added to every request
	DefaultHeaders map[string]string

	// QueryEncoder can customize how query parameters are encoded
	QueryEncoder func(url.Values) string

	// BodyEncoder can customize how the body is encoded
	BodyEncoder func(interface{}) ([]byte, error)
}

// DefaultRequestConfig returns the default request configuration.
func DefaultRequestConfig() *RequestConfig {
	return &RequestConfig{
		DefaultHeaders: map[string]string{
			"Accept":     "application/json",
			"User-Agent": "proxmox-apiclient-go/1.0",
		},
		QueryEncoder: func(v url.Values) string {
			return v.Encode()
		},
		BodyEncoder: json.Marshal,
	}
}

// PathBuilder helps construct API paths with parameters.
type PathBuilder struct {
	segments []string
}

// NewPathBuilder creates a new path builder.
func NewPathBuilder() *PathBuilder {
	return &PathBuilder{
		segments: []string{},
	}
}

// Add adds a path segment.
func (pb *PathBuilder) Add(segment string) *PathBuilder {
	pb.segments = append(pb.segments, segment)

	return pb
}

// AddFormat adds a formatted path segment.
func (pb *PathBuilder) AddFormat(format string, args ...interface{}) *PathBuilder {
	segment := fmt.Sprintf(format, args...)

	return pb.Add(segment)
}

// Build constructs the final path.
func (pb *PathBuilder) Build() string {
	return "/" + strings.Join(pb.segments, "/")
}

// Common PVE API paths.
const (
	PathAccessTicket  = "/access/ticket"
	PathAccessTFA     = "/access/tfa"
	PathAccessUsers   = "/access/users"
	PathAccessGroups  = "/access/groups"
	PathAccessACL     = "/access/acl"
	PathAccessDomains = "/access/domains"
	PathAccessRoles   = "/access/roles"
	PathCluster       = "/cluster"
	PathClusterStatus = "/cluster/status"
	PathClusterConfig = "/cluster/config"
	PathClusterTasks  = "/cluster/tasks"
	PathNodes         = "/nodes"
	PathStorage       = "/storage"
	PathVersion       = "/version"
)

// BuildNodePath builds a path for a specific node.
func BuildNodePath(node string, segments ...string) string {
	pb := NewPathBuilder().Add("nodes").Add(node)
	for _, segment := range segments {
		pb.Add(segment)
	}

	return pb.Build()
}

// BuildVMPath builds a path for a specific VM.
func BuildVMPath(node string, vmid int, segments ...string) string {
	pb := NewPathBuilder().Add("nodes").Add(node).Add("qemu").AddFormat("%d", vmid)
	for _, segment := range segments {
		pb.Add(segment)
	}

	return pb.Build()
}

// BuildContainerPath builds a path for a specific container.
func BuildContainerPath(node string, vmid int, segments ...string) string {
	pb := NewPathBuilder().Add("nodes").Add(node).Add("lxc").AddFormat("%d", vmid)
	for _, segment := range segments {
		pb.Add(segment)
	}

	return pb.Build()
}

// BuildStoragePath builds a path for storage operations.
func BuildStoragePath(storage string, segments ...string) string {
	pb := NewPathBuilder().Add("storage").Add(storage)
	for _, segment := range segments {
		pb.Add(segment)
	}

	return pb.Build()
}
