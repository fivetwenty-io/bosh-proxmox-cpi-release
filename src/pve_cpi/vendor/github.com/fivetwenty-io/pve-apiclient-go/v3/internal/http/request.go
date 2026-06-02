package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/url"
	"strings"
)

var (
	ErrUnsupportedMethod = errors.New("unsupported method")
)

// RequestBuilder helps construct HTTP requests for the PVE API.
type RequestBuilder struct {
	method      string
	baseURL     string
	path        string
	queryParams url.Values
	formParams  url.Values
	jsonBody    interface{}
	headers     map[string]string
	files       map[string]struct {
		filename string
		r        io.Reader
	}
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
		files: make(map[string]struct {
			filename string
			r        io.Reader
		}),
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
func (rb *RequestBuilder) AddFile(fieldName, filename string, file io.Reader) *RequestBuilder {
	rb.files[fieldName] = struct {
		filename string
		r        io.Reader
	}{filename: filename, r: file}

	return rb
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

// BuildBody constructs the request body.
func (rb *RequestBuilder) BuildBody() (io.Reader, string, error) {
	// Handle different body types based on method and content
	switch rb.method {
	case "GET", "DELETE":
		// No body for GET and DELETE
		return nil, "", nil

	case "POST", "PUT", "PATCH":
		// Check if we have files to upload
		if len(rb.files) > 0 {
			return rb.buildMultipartBody()
		}

		// Check if we have JSON body
		if rb.jsonBody != nil {
			body, err := json.Marshal(rb.jsonBody)
			if err != nil {
				return nil, "", fmt.Errorf("failed to marshal JSON body: %w", err)
			}

			return bytes.NewReader(body), contentTypeJSON, nil
		}

		// Default to form-encoded body
		if len(rb.formParams) > 0 {
			body := rb.formParams.Encode()

			return strings.NewReader(body), contentTypeFormURLEncoded, nil
		}

		// No body
		return nil, "", nil

	default:
		return nil, "", fmt.Errorf("%w: %s", ErrUnsupportedMethod, rb.method)
	}
}

// buildMultipartBody builds a multipart form body for file uploads.
func (rb *RequestBuilder) buildMultipartBody() (io.Reader, string, error) {
	var buffer bytes.Buffer

	writer := multipart.NewWriter(&buffer)

	// Add form parameters
	for key, values := range rb.formParams {
		for _, value := range values {
			err := writer.WriteField(key, value)
			if err != nil {
				return nil, "", fmt.Errorf("failed to write field %s: %w", key, err)
			}
		}
	}

	// Add files
	for fieldName, fileData := range rb.files {
		part, err := writer.CreateFormFile(fieldName, fileData.filename)
		if err != nil {
			return nil, "", fmt.Errorf("failed to create form file %s: %w", fieldName, err)
		}

		_, err = io.Copy(part, fileData.r)
		if err != nil {
			return nil, "", fmt.Errorf("failed to copy file %s: %w", fieldName, err)
		}
	}

	err := writer.Close()
	if err != nil {
		return nil, "", fmt.Errorf("failed to close multipart writer: %w", err)
	}

	return &buffer, writer.FormDataContentType(), nil
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
			"User-Agent": "pve-apiclient-go/1.0",
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
