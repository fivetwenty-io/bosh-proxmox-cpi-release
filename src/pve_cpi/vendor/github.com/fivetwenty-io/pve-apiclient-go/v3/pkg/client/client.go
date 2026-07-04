package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	pvehttp "github.com/fivetwenty-io/pve-apiclient-go/v3/internal/http"
	issl "github.com/fivetwenty-io/pve-apiclient-go/v3/internal/ssl"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/auth"
	pmetrics "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/metrics"
)

// ErrAuthNotConfigured is returned when authentication is not configured.
var ErrAuthNotConfigured = errors.New("authentication not configured")

// Client defines the interface for interacting with the PVE API.
type Client interface {
	// HTTP Methods
	Get(path string, params map[string]interface{}) (interface{}, error)
	GetRaw(path string, params map[string]interface{}) (*Response, error)
	Post(path string, params map[string]interface{}) (interface{}, error)
	PostRaw(path string, params map[string]interface{}) (*Response, error)
	Put(path string, params map[string]interface{}) (interface{}, error)
	PutRaw(path string, params map[string]interface{}) (*Response, error)
	Delete(path string, params map[string]interface{}) (interface{}, error)
	DeleteRaw(path string, params map[string]interface{}) (*Response, error)

	// Context-aware HTTP Methods
	GetCtx(ctx context.Context, path string, params map[string]interface{}) (interface{}, error)
	GetRawCtx(ctx context.Context, path string, params map[string]interface{}) (*Response, error)
	PostCtx(ctx context.Context, path string, params map[string]interface{}) (interface{}, error)
	PostRawCtx(ctx context.Context, path string, params map[string]interface{}) (*Response, error)
	PutCtx(ctx context.Context, path string, params map[string]interface{}) (interface{}, error)
	PutRawCtx(ctx context.Context, path string, params map[string]interface{}) (*Response, error)
	DeleteCtx(ctx context.Context, path string, params map[string]interface{}) (interface{}, error)
	DeleteRawCtx(ctx context.Context, path string, params map[string]interface{}) (*Response, error)

	// Upload
	UploadCtx(ctx context.Context, path string, fields map[string]string, fileField, filename string, file io.Reader) (*Response, error)

	// Authentication
	Login() error
	Logout() error
	UpdateTicket(ticket string)
	UpdateCSRFToken(token string)

	// Configuration
	SetTimeout(timeout time.Duration)
	SetKeepAlive(connections int)

	// Logging configuration
	SetLogger(l Logger)
	SetLogConfig(cfg LogConfig)
	AddLogHook(h Hook)
	GetLogConfig() LogConfig

	// Metrics configuration
	SetMetrics(m *pmetrics.DefaultMetrics)

	// Two-Factor Authentication
	SetTFAHandler(h TFAHandler)

	// Cache control
	InvalidateCache(pattern string) int
	ClearCache()
	CacheStats() *CacheStats

	// Header control. SetHeader adds key/value to every outgoing request;
	// RemoveHeader removes a previously set header. Both are safe for
	// concurrent use. Useful for setting a custom User-Agent at construction
	// time, among other things.
	SetHeader(key, value string)
	RemoveHeader(key string)

	// Lifecycle. Close releases background resources (response-cache cleanup
	// goroutine and idle HTTP connections). Safe to call more than once.
	Close() error
}

// Response represents a response from the PVE API.
type Response struct {
	Data   interface{}
	Errors map[string]string
	Code   int
}

// client implements the Client interface.
type client struct {
	options    *Options
	httpClient HTTPClient
}

// HTTPClient defines the interface for HTTP operations.
type HTTPClient interface {
	Do(method, path string, params map[string]interface{}) (*Response, error)
	DoCtx(ctx context.Context, method, path string, params map[string]interface{}) (*Response, error)
	UploadCtx(ctx context.Context, path string, fields map[string]string, fileField, filename string, file io.Reader) (*Response, error)
	SetTimeout(timeout time.Duration)
	SetKeepAlive(connections int)
	SetHeader(key, value string)
	RemoveHeader(key string)
	InvalidateCache(pattern string) int
	ClearCache()
	CacheStats() *CacheStats
	Close() error
}

// Re-export logging types for the public API.
type (
	Logger     = pvehttp.Logger
	LogConfig  = pvehttp.LogConfig
	Hook       = pvehttp.Hook
	TFAHandler = auth.TFAHandler
)

// Authenticator defines the interface for authentication operations.
type Authenticator interface {
	Login(username, password string) (ticket string, csrf string, err error)
	Logout(ticket string) error
	IsValid() bool
	GetHeaders() map[string]string
}

// NewClient creates a new PVE API client with the given options.
func NewClient(opts Options) (Client, error) { //nolint:ireturn // Factory function pattern
	err := opts.Validate()
	if err != nil {
		return nil, err
	}

	opts.setDefaults()

	// Create the HTTP client
	httpClient, err := createHTTPClient(&opts)
	if err != nil {
		return nil, err
	}

	c := &client{
		options:    &opts,
		httpClient: httpClient,
	}

	return c, nil
}

// Get performs a GET request to the specified path.
func (c *client) Get(path string, params map[string]interface{}) (interface{}, error) {
	resp, err := c.GetRaw(path, params)
	if err != nil {
		return nil, err
	}

	return resp.Data, nil
}

// SetMetrics attaches a Prometheus-friendly metrics collector. Optional.
func (c *client) SetMetrics(m *pmetrics.DefaultMetrics) {
	if a, ok := c.httpClient.(*internalHTTPAdapter); ok && a.inner != nil {
		a.inner.SetMetrics(m)
	}
}

// GetRaw performs a GET request and returns the raw response.
func (c *client) GetRaw(path string, params map[string]interface{}) (*Response, error) {
	return c.call("GET", path, params)
}

// GetRawCtx performs a GET request with context and returns the raw response.
func (c *client) GetRawCtx(ctx context.Context, path string, params map[string]interface{}) (*Response, error) {
	return c.callCtx(ctx, "GET", path, params)
}

// GetCtx performs a GET request with context to the specified path.
func (c *client) GetCtx(ctx context.Context, path string, params map[string]interface{}) (interface{}, error) {
	resp, err := c.GetRawCtx(ctx, path, params)
	if err != nil {
		return nil, err
	}

	return resp.Data, nil
}

// Post performs a POST request to the specified path.
func (c *client) Post(path string, params map[string]interface{}) (interface{}, error) {
	resp, err := c.PostRaw(path, params)
	if err != nil {
		return nil, err
	}

	return resp.Data, nil
}

// PostRaw performs a POST request and returns the raw response.
func (c *client) PostRaw(path string, params map[string]interface{}) (*Response, error) {
	return c.call("POST", path, params)
}

// PostRawCtx performs a POST request with context and returns the raw response.
func (c *client) PostRawCtx(ctx context.Context, path string, params map[string]interface{}) (*Response, error) {
	return c.callCtx(ctx, "POST", path, params)
}

// PostCtx performs a POST request with context to the specified path.
func (c *client) PostCtx(ctx context.Context, path string, params map[string]interface{}) (interface{}, error) {
	resp, err := c.PostRawCtx(ctx, path, params)
	if err != nil {
		return nil, err
	}

	return resp.Data, nil
}

// Put performs a PUT request to the specified path.
func (c *client) Put(path string, params map[string]interface{}) (interface{}, error) {
	resp, err := c.PutRaw(path, params)
	if err != nil {
		return nil, err
	}

	return resp.Data, nil
}

// PutRaw performs a PUT request and returns the raw response.
func (c *client) PutRaw(path string, params map[string]interface{}) (*Response, error) {
	return c.call("PUT", path, params)
}

// PutRawCtx performs a PUT request with context and returns the raw response.
func (c *client) PutRawCtx(ctx context.Context, path string, params map[string]interface{}) (*Response, error) {
	return c.callCtx(ctx, "PUT", path, params)
}

// PutCtx performs a PUT request with context to the specified path.
func (c *client) PutCtx(ctx context.Context, path string, params map[string]interface{}) (interface{}, error) {
	resp, err := c.PutRawCtx(ctx, path, params)
	if err != nil {
		return nil, err
	}

	return resp.Data, nil
}

// Delete performs a DELETE request to the specified path.
func (c *client) Delete(path string, params map[string]interface{}) (interface{}, error) {
	resp, err := c.DeleteRaw(path, params)
	if err != nil {
		return nil, err
	}

	return resp.Data, nil
}

// DeleteRaw performs a DELETE request and returns the raw response.
func (c *client) DeleteRaw(path string, params map[string]interface{}) (*Response, error) {
	return c.call("DELETE", path, params)
}

// DeleteRawCtx performs a DELETE request with context and returns the raw response.
func (c *client) DeleteRawCtx(ctx context.Context, path string, params map[string]interface{}) (*Response, error) {
	return c.callCtx(ctx, "DELETE", path, params)
}

// DeleteCtx performs a DELETE request with context to the specified path.
func (c *client) DeleteCtx(ctx context.Context, path string, params map[string]interface{}) (interface{}, error) {
	resp, err := c.DeleteRawCtx(ctx, path, params)
	if err != nil {
		return nil, err
	}

	return resp.Data, nil
}

// UploadCtx uploads a file with context.
func (c *client) UploadCtx(ctx context.Context, path string, fields map[string]string, fileField, filename string, file io.Reader) (*Response, error) {
	resp, err := c.httpClient.UploadCtx(ctx, path, fields, fileField, filename, file)
	if err != nil {
		return nil, fmt.Errorf("failed to upload file %q to path %q: %w", filename, path, err)
	}

	return resp, nil
}

// Login authenticates with the PVE API.
// This method is useful when you want to explicitly login before making API calls,
// or when AutoLogin is disabled (default) and you're using username/password authentication.
func (c *client) Login() error {
	if a, ok := c.httpClient.(*internalHTTPAdapter); ok && a.inner != nil {
		err := a.inner.Authenticate()
		if err != nil {
			return fmt.Errorf("failed to authenticate with PVE API: %w", err)
		}

		return nil
	}

	return ErrAuthNotConfigured
}

// Logout logs out from the PVE API.
func (c *client) Logout() error {
	if a, ok := c.httpClient.(*internalHTTPAdapter); ok && a.inner != nil {
		err := a.inner.Logout()
		if err != nil {
			return fmt.Errorf("failed to logout from PVE API: %w", err)
		}
	}

	c.options.Ticket = ""
	c.options.CSRFToken = ""

	return nil
}

// UpdateTicket updates the authentication ticket.
// The new ticket is propagated to the live authenticator so subsequent requests
// carry it; the cached CSRF token is reused unless separately updated.
func (c *client) UpdateTicket(ticket string) {
	c.options.Ticket = ticket

	if a, ok := c.httpClient.(*internalHTTPAdapter); ok && a.inner != nil {
		a.inner.SetTicketValue(ticket, c.options.CSRFToken)
	}
}

// UpdateCSRFToken updates the CSRF prevention token.
// The new token is propagated to the live authenticator, preserving the
// current ticket value.
func (c *client) UpdateCSRFToken(token string) {
	c.options.CSRFToken = token

	if a, ok := c.httpClient.(*internalHTTPAdapter); ok && a.inner != nil {
		a.inner.SetCSRFToken(token)
	}
}

// SetTimeout sets the request timeout, applying it to the live HTTP client
// (both the request deadline and the underlying transport) so it takes
// effect for subsequent requests without reconstructing the client.
func (c *client) SetTimeout(timeout time.Duration) {
	c.options.Timeout = timeout
	c.httpClient.SetTimeout(timeout)
}

// SetKeepAlive sets the number of keep-alive connections, applying it to the
// live transport so it takes effect for subsequent requests without
// reconstructing the client.
func (c *client) SetKeepAlive(connections int) {
	c.options.KeepAlive = connections
	c.httpClient.SetKeepAlive(connections)
}

// SetLogger installs a structured logger for HTTP requests.
func (c *client) SetLogger(l Logger) {
	if a, ok := c.httpClient.(*internalHTTPAdapter); ok && a.inner != nil {
		a.inner.SetLogger(l)
	}
}

// SetLogConfig configures logging behavior (redaction, body sampling, etc.).
func (c *client) SetLogConfig(cfg LogConfig) {
	if a, ok := c.httpClient.(*internalHTTPAdapter); ok && a.inner != nil {
		a.inner.SetLogConfig(cfg)
	}
}

// AddLogHook adds a logging event hook.
func (c *client) AddLogHook(h Hook) {
	if a, ok := c.httpClient.(*internalHTTPAdapter); ok && a.inner != nil {
		a.inner.AddHook(h)
	}
}

// GetLogConfig returns the current logging config snapshot.
func (c *client) GetLogConfig() LogConfig {
	if a, ok := c.httpClient.(*internalHTTPAdapter); ok && a.inner != nil {
		return a.inner.LogConfig()
	}

	var zero LogConfig

	return zero
}

// SetTFAHandler installs a TFA handler used by the underlying HTTP client to auto-complete challenges.
func (c *client) SetTFAHandler(h TFAHandler) {
	if a, ok := c.httpClient.(*internalHTTPAdapter); ok && a.inner != nil {
		a.inner.SetTFAHandler(h)
	}
}

// InvalidateCache removes cache entries matching the given pattern.
func (c *client) InvalidateCache(pattern string) int {
	return c.httpClient.InvalidateCache(pattern)
}

// ClearCache removes all cached entries.
func (c *client) ClearCache() {
	c.httpClient.ClearCache()
}

// CacheStats returns current cache statistics.
func (c *client) CacheStats() *CacheStats {
	return c.httpClient.CacheStats()
}

// SetHeader adds key/value to every outgoing request. Safe for concurrent use.
func (c *client) SetHeader(key, value string) {
	c.httpClient.SetHeader(key, value)
}

// RemoveHeader removes a previously set custom header. Safe for concurrent use.
func (c *client) RemoveHeader(key string) {
	c.httpClient.RemoveHeader(key)
}

// Close releases background resources held by the client (the response-cache
// cleanup goroutine and idle HTTP connections). It is safe to call more than
// once. After Close the client should not be used for further requests.
func (c *client) Close() error {
	err := c.httpClient.Close()
	if err != nil {
		return fmt.Errorf("closing client: %w", err)
	}

	return nil
}

func (c *client) call(method, path string, params map[string]interface{}) (*Response, error) {
	// Make the HTTP request
	resp, err := c.httpClient.Do(method, path, params)
	if err != nil {
		return nil, fmt.Errorf("failed to execute %s request to %q: %w", method, path, err)
	}

	return resp, nil
}

func (c *client) callCtx(ctx context.Context, method, path string, params map[string]interface{}) (*Response, error) {
	resp, err := c.httpClient.DoCtx(ctx, method, path, params)
	if err != nil {
		return nil, fmt.Errorf("failed to execute %s request to %q with context: %w", method, path, err)
	}

	return resp, nil
}

// createHTTPClient creates the HTTP client based on options.
func createHTTPClient(opts *Options) (HTTPClient, error) { //nolint:ireturn // Factory function pattern
	// Wire to the internal HTTP client implementation
	ihc, err := internalHTTPNew(opts)
	if err != nil {
		return nil, err
	}

	return &internalHTTPAdapter{inner: ihc}, nil
}

// simpleHTTPClient is a basic HTTP client implementation.
// internalHTTPAdapter adapts the internal HTTP client to this package's HTTPClient interface.
type internalHTTPAdapter struct{ inner *pvehttp.Client }

func (a *internalHTTPAdapter) Do(method, path string, params map[string]interface{}) (*Response, error) {
	r, err := a.inner.Do(method, path, params)
	if err != nil {
		return nil, fmt.Errorf("HTTP %s request to %q failed: %w", method, path, err)
	}

	return &Response{Data: r.Data, Errors: r.Errors, Code: r.Code}, nil
}

func (a *internalHTTPAdapter) DoCtx(ctx context.Context, method, path string, params map[string]interface{}) (*Response, error) {
	r, err := a.inner.DoWithContext(ctx, method, path, params)
	if err != nil {
		return nil, fmt.Errorf("HTTP %s request to %q with context failed: %w", method, path, err)
	}

	return &Response{Data: r.Data, Errors: r.Errors, Code: r.Code}, nil
}

func (a *internalHTTPAdapter) UploadCtx(ctx context.Context, path string, fields map[string]string, fileField, filename string, file io.Reader) (*Response, error) {
	r, err := a.inner.UploadWithContext(ctx, path, fields, fileField, filename, file)
	if err != nil {
		return nil, fmt.Errorf("HTTP upload to %q failed for file %q: %w", path, filename, err)
	}

	return &Response{Data: r.Data, Errors: r.Errors, Code: r.Code}, nil
}
func (a *internalHTTPAdapter) SetTimeout(timeout time.Duration) {
	if a.inner != nil {
		a.inner.SetTimeout(timeout)
	}
}

func (a *internalHTTPAdapter) SetKeepAlive(connections int) {
	if a.inner != nil {
		a.inner.SetKeepAlive(connections)
	}
}

func (a *internalHTTPAdapter) SetHeader(key, value string) {
	if a.inner != nil {
		a.inner.SetHeader(key, value)
	}
}

func (a *internalHTTPAdapter) RemoveHeader(key string) {
	if a.inner != nil {
		a.inner.RemoveHeader(key)
	}
}

func (a *internalHTTPAdapter) InvalidateCache(pattern string) int {
	if a.inner != nil {
		return a.inner.InvalidateCache(pattern)
	}

	return 0
}

func (a *internalHTTPAdapter) ClearCache() {
	if a.inner != nil {
		a.inner.ClearCache()
	}
}

func (a *internalHTTPAdapter) CacheStats() *CacheStats {
	if a.inner != nil {
		return a.inner.CacheStats()
	}

	return nil
}

func (a *internalHTTPAdapter) Close() error {
	if a.inner != nil {
		err := a.inner.Close()
		if err != nil {
			return fmt.Errorf("closing internal HTTP client: %w", err)
		}
	}

	return nil
}

// internalHTTPNew constructs the real internal HTTP client.
func internalHTTPNew(opts *Options) (*pvehttp.Client, error) {
	// Map client.Options to internal/http.Options
	var ssl *pvehttp.SSLOptions
	if opts.SSLOptions != nil {
		ssl = &pvehttp.SSLOptions{
			VerifyHostname: opts.SSLOptions.VerifyHostname,
			VerifyMode:     pvehttp.SSLVerifyMode(opts.SSLOptions.VerifyMode),
			CACert:         opts.SSLOptions.CACert,
			ClientCert:     opts.SSLOptions.ClientCert,
			ClientKey:      opts.SSLOptions.ClientKey,
		}
	}

	var manualVerifyCallback func(issl.ManualVerificationRequest) bool

	if opts.ManualVerifyCallback != nil {
		cb := opts.ManualVerifyCallback
		manualVerifyCallback = func(req issl.ManualVerificationRequest) bool {
			return cb(FingerprintVerificationRequest{
				Fingerprint: req.Fingerprint,
				Certificate: req.Certificate,
				Host:        req.Host,
			})
		}
	}

	iopts := &pvehttp.Options{
		Host:                        opts.Host,
		Port:                        opts.Port,
		Protocol:                    opts.Protocol,
		Username:                    opts.Username,
		Password:                    opts.Password,
		APIToken:                    opts.APIToken,
		APITokenName:                opts.APITokenName,
		Ticket:                      opts.Ticket,
		CSRFToken:                   opts.CSRFToken,
		AutoLogin:                   opts.AutoLogin,
		SSLOptions:                  ssl,
		Timeout:                     opts.Timeout,
		KeepAlive:                   opts.KeepAlive,
		DialTimeoutSec:              opts.DialTimeoutSec,
		TLSHandshakeTimeoutSec:      opts.TLSHandshakeTimeoutSec,
		MaxIdleConnsPerHost:         opts.MaxIdleConnsPerHost,
		IdleConnTimeoutSec:          opts.IdleConnTimeoutSec,
		TCPKeepAliveSec:             opts.TCPKeepAliveSec,
		CacheConfig:                 opts.CacheConfig,
		CookieName:                  opts.CookieName,
		PVENewFormat:                opts.PVENewFormat,
		CachedFingerprints:          opts.CachedFingerprints,
		ManualVerification:          opts.ManualVerification,
		RegisterFingerprintCallback: opts.RegisterFingerprintCallback,
		VerifyFingerprintCallback:   opts.VerifyFingerprintCallback,
		ManualVerifyCallback:        manualVerifyCallback,
		FingerprintCachePath:        opts.FingerprintCachePath,
	}

	c, err := pvehttp.NewClient(iopts)
	if err != nil {
		return nil, fmt.Errorf("failed to create internal HTTP client: %w", err)
	}

	return c, nil
}
