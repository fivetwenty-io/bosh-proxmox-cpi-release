package http

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"crypto/x509"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/internal/constants"
	issl "github.com/fivetwenty-io/pve-apiclient-go/v3/internal/ssl"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/auth"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/cache"
	apierrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
	pmetrics "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/metrics"
)

// Client implements the HTTP client for PVE API communication.
type Client struct {
	baseURL       string
	httpClient    *http.Client
	authenticator auth.Authenticator
	middleware    []Middleware
	timeout       time.Duration
	maxRetries    int
	retryDelay    time.Duration

	metrics   ClientMetrics
	logger    Logger
	logConfig LogConfig
	hooks     []Hook

	// Optional Prometheus-friendly metrics collector
	prom *pmetrics.DefaultMetrics

	// Optional TFA handler for auto-completion of two-factor challenges
	tfaHandler auth.TFAHandler

	// Auto-login state management
	options        *Options
	loginMutex     sync.Mutex
	loginAttempted bool

	// Custom headers applied to every request.
	headerMu sync.RWMutex
	headers  map[string]string

	// Caching
	cache *cache.Cache
}

// Middleware defines a function that can modify requests or responses.
type Middleware func(*http.Request, Handler) (*http.Response, error)

// Handler represents the next handler in the middleware chain.
type Handler func(*http.Request) (*http.Response, error)

// NewClient creates a new HTTP client for PVE API.
func NewClient(options *Options) (*Client, error) {
	transport := createHTTPTransport(options)

	if options.Protocol == protoHTTPS {
		tlsConfig, err := configureTLS(options)
		if err != nil {
			return nil, err
		}

		transport.TLSClientConfig = tlsConfig
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   options.Timeout,
	}

	authenticator := createAuthenticator(options, httpClient)

	client := &Client{
		baseURL:       options.BaseURL(),
		httpClient:    httpClient,
		authenticator: authenticator,
		timeout:       options.Timeout,
		maxRetries:    constants.DefaultMaxRetries,
		retryDelay:    time.Second,
		logConfig:     defaultLogConfig(),
		options:       options,
	}

	// Initialize cache if configured
	if options.CacheConfig != nil && options.CacheConfig.Enabled {
		client.cache = cache.NewCache(*options.CacheConfig)

		// Add caching middleware BEFORE auth (cached responses bypass auth)
		client.middleware = []Middleware{
			client.cachingMiddleware,
			client.authMiddleware,
			client.retryMiddleware,
			client.loggingMiddleware,
		}
	} else {
		client.middleware = []Middleware{
			client.authMiddleware,
			client.retryMiddleware,
			client.loggingMiddleware,
		}
	}

	return client, nil
}

func createHTTPTransport(options *Options) *http.Transport {
	// Resolve per-host idle conn cap: explicit knob when set, else match KeepAlive
	// (preserves current default behaviour when knob is zero).
	maxIdlePerHost := options.KeepAlive
	if options.MaxIdleConnsPerHost > 0 {
		maxIdlePerHost = options.MaxIdleConnsPerHost
	}

	// Resolve idle-connection timeout: explicit knob when set, else LongTimeout()
	// (preserves current default behaviour when knob is zero).
	idleConnTimeout := constants.LongTimeout()
	if options.IdleConnTimeoutSec > 0 {
		idleConnTimeout = time.Duration(options.IdleConnTimeoutSec) * time.Second
	}

	t := &http.Transport{
		MaxIdleConns:        options.KeepAlive,
		MaxIdleConnsPerHost: maxIdlePerHost,
		IdleConnTimeout:     idleConnTimeout,
		DisableCompression:  false,
	}

	// TLS handshake timeout: only set when the knob is non-zero; leaving it zero
	// preserves Go's default (no explicit handshake deadline).
	if options.TLSHandshakeTimeoutSec > 0 {
		t.TLSHandshakeTimeout = time.Duration(options.TLSHandshakeTimeoutSec) * time.Second
	}

	// Dial context: set only when at least one dial-level knob is non-zero so that
	// the zero-knob path leaves DialContext nil (byte-identical to the prior transport).
	if options.DialTimeoutSec > 0 || options.TCPKeepAliveSec > 0 {
		dialer := &net.Dialer{}
		if options.DialTimeoutSec > 0 {
			dialer.Timeout = time.Duration(options.DialTimeoutSec) * time.Second
		}

		if options.TCPKeepAliveSec > 0 {
			dialer.KeepAlive = time.Duration(options.TCPKeepAliveSec) * time.Second
		}

		t.DialContext = dialer.DialContext
	}

	return t
}

func configureTLS(options *Options) (*tls.Config, error) {
	tlsConfig, err := createTLSConfig(options.SSLOptions)
	if err != nil {
		return nil, err
	}

	err = configureCAPool(tlsConfig, options.SSLOptions)
	if err != nil {
		return nil, err
	}

	err = configureClientCertificates(tlsConfig, options.SSLOptions)
	if err != nil {
		return nil, err
	}

	configureFingerprintVerification(tlsConfig, options)

	return tlsConfig, nil
}

func configureCAPool(tlsConfig *tls.Config, sslOptions *SSLOptions) error {
	if sslOptions != nil && sslOptions.CACert != "" {
		pool, err := issl.LoadCACertificate(sslOptions.CACert)
		if err != nil {
			return fmt.Errorf("failed to load CA certificate: %w", err)
		}

		tlsConfig.RootCAs = pool
	}

	return nil
}

func configureClientCertificates(tlsConfig *tls.Config, sslOptions *SSLOptions) error {
	if sslOptions != nil && sslOptions.ClientCert != "" && sslOptions.ClientKey != "" {
		cert, err := tls.LoadX509KeyPair(sslOptions.ClientCert, sslOptions.ClientKey)
		if err != nil {
			return fmt.Errorf("failed to load client certificates: %w", err)
		}

		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	return nil
}

func configureFingerprintVerification(tlsConfig *tls.Config, options *Options) {
	if !fingerprintVerificationEnabled(options) {
		return
	}

	tlsConfig.InsecureSkipVerify = true
	// TLS session resumption skips VerifyPeerCertificate on the resumed
	// handshake, which would let a previously-pinned-then-revoked certificate
	// (or a session ticket obtained before pinning was configured) bypass the
	// fingerprint check below. Disable resumption so every connection is
	// re-verified against the pin.
	tlsConfig.SessionTicketsDisabled = true

	fingerprintVerifier := buildFingerprintVerifier(options)

	tlsConfig.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return issl.ErrNoCertificatesProvided
		}

		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("failed to parse certificate: %w", err)
		}

		return fingerprintVerifier.VerifyCertificateForHost(cert, options.Host)
	}
}

// fingerprintVerificationEnabled reports whether any fingerprint-pinning knob
// is in use, in which case standard certificate-chain verification is
// replaced by fingerprint pinning (InsecureSkipVerify + VerifyPeerCertificate).
func fingerprintVerificationEnabled(options *Options) bool {
	return len(options.CachedFingerprints) > 0 ||
		options.ManualVerification ||
		options.VerifyFingerprintCallback != nil ||
		options.ManualVerifyCallback != nil ||
		options.FingerprintCachePath != ""
}

// buildFingerprintVerifier constructs the certificate-fingerprint verifier
// used for pinning. When FingerprintCachePath is set, trust is persisted
// across process restarts via a Trust-On-First-Use cache keyed by Host/Port
// (see issl.FingerprintCache.NewVerifierWithCache); any fingerprint accepted
// through ManualVerifyCallback is written back to that cache. Otherwise the
// verifier is memory-only for this client's lifetime.
func buildFingerprintVerifier(options *Options) *issl.FingerprintVerifier {
	var verifier *issl.FingerprintVerifier

	if options.FingerprintCachePath != "" {
		fpCache := issl.NewFingerprintCache(options.FingerprintCachePath)

		// A missing or corrupt cache file must not block client construction;
		// Load leaves fpCache with whatever entries it managed to decode (empty
		// on failure), so verification falls back to treating every fingerprint
		// as unknown rather than failing startup.
		_ = fpCache.Load()

		verifier = fpCache.NewVerifierWithCache(options.Host, options.Port, options.ManualVerifyCallback)
	} else {
		verifier = issl.NewFingerprintVerifier()
	}

	seedTrustedFingerprints(verifier, options.CachedFingerprints)
	configureVerifierCallbacks(verifier, options)

	return verifier
}

func seedTrustedFingerprints(verifier *issl.FingerprintVerifier, cachedFingerprints map[string]bool) {
	var fps []string

	for fp, trusted := range cachedFingerprints {
		if trusted {
			fps = append(fps, fp)
		}
	}

	if len(fps) > 0 {
		verifier.AddTrustedFingerprints(fps)
	}
}

func configureVerifierCallbacks(verifier *issl.FingerprintVerifier, options *Options) {
	// NewVerifierWithCache already enables manual verification when
	// FingerprintCachePath is set; only ever turn it on here, never back off,
	// so that path is not silently undone. Configuring ManualVerifyCallback
	// without also setting ManualVerification implies manual mode: a caller
	// supplying a decision callback clearly wants it consulted.
	if options.ManualVerification || options.FingerprintCachePath != "" || options.ManualVerifyCallback != nil {
		verifier.SetManualVerification(true)
	}

	if options.RegisterFingerprintCallback != nil {
		verifier.SetRegisterCallback(options.RegisterFingerprintCallback)
	}

	if options.VerifyFingerprintCallback != nil {
		verifier.SetVerifyCallback(options.VerifyFingerprintCallback)
	}

	// NewVerifierWithCache already wires ManualVerifyCallback (so accepted
	// fingerprints persist to the cache); for the memory-only path it must be
	// wired here instead.
	if options.FingerprintCachePath == "" && options.ManualVerifyCallback != nil {
		verifier.SetManualVerifyCallback(options.ManualVerifyCallback)
	}
}

func createAuthenticator(options *Options, httpClient *http.Client) auth.Authenticator { //nolint:ireturn // Factory function pattern
	switch {
	case options.APIToken != "":
		token, err := auth.ParseAPIToken(options.APIToken)
		if err != nil {
			// Caller cannot receive this error through NewClient currently; surface it via a
			// sentinel authenticator that always returns an error on Authenticate(). This keeps
			// the public API unchanged (NewClient returns (*Client, error) — callers that pass
			// a malformed token will discover the problem on first use or via Authenticate()).
			return auth.NewInvalidAuthenticator(fmt.Errorf("invalid APIToken %q: %w", options.APIToken, err))
		}

		return auth.NewAPITokenAuthenticator(token, options.APITokenName)

	case options.Username != "":
		credentials := &auth.Credentials{
			Username: options.Username,
			Password: options.Password,
			Realm:    "pam",
		}

		if parts := strings.Split(options.Username, "@"); len(parts) == constants.ExpectedPartsCount {
			credentials.Username = parts[0]
			credentials.Realm = parts[1]
		}

		return auth.NewTicketAuthenticator(options.BaseURL(), credentials, httpClient, options.CookieName, options.PVENewFormat)

	case options.Ticket != "":
		// A pre-existing ticket was supplied without a token or username. Build a
		// ticket authenticator seeded with it so requests are authenticated and
		// the ticket can later be updated via SetTicket / renewed if it ages out.
		// options.CSRFToken (when supplied alongside the ticket) is carried from
		// construction rather than requiring a post-hoc UpdateCSRFToken call.
		return auth.NewTicketAuthenticatorFromTicket(
			options.BaseURL(),
			options.Ticket,
			options.CSRFToken,
			options.Username,
			nil,
			httpClient,
			options.CookieName,
			options.PVENewFormat,
		)
	}

	return nil
}

// SetTicketValue updates the active ticket on a ticket-based authenticator.
// It is used to propagate an externally supplied ticket and CSRF token (for
// example after an out-of-band login) onto the live authenticator so subsequent
// requests carry the new credentials. It is a no-op when the configured
// authenticator is not ticket based.
func (c *Client) SetTicketValue(ticketValue, csrfToken string) {
	ticketAuth, ok := c.authenticator.(*auth.TicketAuthenticator)
	if !ok {
		return
	}

	existing := ticketAuth.GetTicket()

	username := ""
	if existing != nil {
		username = existing.Username
	}

	if csrfToken == "" && existing != nil {
		csrfToken = existing.CSRFToken
	}

	validUntil := time.Now().Add(constants.TicketValidity())

	createdAt, parseErr := auth.ParseTicketTimestamp(ticketValue)
	if parseErr == nil {
		validUntil = createdAt.Add(constants.TicketValidity())
	}

	ticketAuth.SetTicket(&auth.Ticket{
		Value:      ticketValue,
		CSRFToken:  csrfToken,
		Username:   username,
		ValidUntil: validUntil,
	})
}

// SetCSRFToken updates only the CSRF prevention token on a ticket-based
// authenticator, preserving the current ticket value. It is a no-op when the
// authenticator is not ticket based or no ticket is currently set.
func (c *Client) SetCSRFToken(csrfToken string) {
	ticketAuth, ok := c.authenticator.(*auth.TicketAuthenticator)
	if !ok {
		return
	}

	existing := ticketAuth.GetTicket()
	if existing == nil {
		return
	}

	ticketAuth.SetTicket(&auth.Ticket{
		Value:      existing.Value,
		CSRFToken:  csrfToken,
		Username:   existing.Username,
		ValidUntil: existing.ValidUntil,
	})
}

func createTLSConfig(sslOptions *SSLOptions) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: false,
		MinVersion:         tls.VersionTLS12,
	}

	if sslOptions == nil {
		return tlsConfig, nil
	}

	if sslOptions.VerifyMode == SSLVerifyNone {
		tlsConfig.InsecureSkipVerify = true
	}

	if sslOptions.ClientCert == "" || sslOptions.ClientKey == "" {
		return tlsConfig, nil
	}

	cert, err := tls.LoadX509KeyPair(sslOptions.ClientCert, sslOptions.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load client certificates: %w", err)
	}

	tlsConfig.Certificates = []tls.Certificate{cert}

	return tlsConfig, nil
}

// Do performs an HTTP request with the specified method, path, and parameters.
func (c *Client) Do(method, path string, params map[string]interface{}) (*Response, error) {
	return c.DoWithContext(context.Background(), method, path, params)
}

// DoWithContext performs an HTTP request with context.
func (c *Client) DoWithContext(ctx context.Context, method, path string, params map[string]interface{}) (*Response, error) {
	req, err := c.buildRequestWithContext(ctx, method, path, params)
	if err != nil {
		return nil, err
	}

	start := time.Now()

	c.recordRequestStart(req)

	resp, err := c.executeWithMiddleware(req)
	if err != nil {
		c.recordRequestError(start)

		return nil, err
	}

	defer func() {
		closeErr := resp.Body.Close()
		if closeErr != nil {
			if c.logger != nil && c.logConfig.Enabled {
				c.logger.Warn("failed to close response body", map[string]interface{}{
					logFieldError: closeErr.Error(),
				})
			}
		}
	}()

	r, bodyBytes, perr := c.parseResponse(resp)
	c.recordRequestComplete(req, resp, bodyBytes, start, perr)

	return r, perr
}

// UploadWithContext performs a multipart upload to the given path.
func (c *Client) UploadWithContext(ctx context.Context, path string, fields map[string]string, fileField, filename string, file io.Reader) (*Response, error) {
	req, err := c.buildUploadRequest(ctx, path, fields, fileField, filename, file)
	if err != nil {
		return nil, err
	}

	start := time.Now()

	c.recordRequestStart(req)

	resp, err := c.executeWithMiddleware(req)
	if err != nil {
		c.recordRequestError(start)

		return nil, err
	}

	defer func() {
		closeErr := resp.Body.Close()
		if closeErr != nil {
			if c.logger != nil && c.logConfig.Enabled {
				c.logger.Warn("failed to close response body", map[string]interface{}{
					logFieldError: closeErr.Error(),
				})
			}
		}
	}()

	r, bodyBytes, perr := c.parseResponse(resp)
	c.recordRequestComplete(req, resp, bodyBytes, start, perr)

	return r, perr
}

// SetHeader sets a header applied to every subsequent request.
func (c *Client) SetHeader(key, value string) {
	c.headerMu.Lock()
	defer c.headerMu.Unlock()

	if c.headers == nil {
		c.headers = make(map[string]string)
	}

	c.headers[key] = value
}

// RemoveHeader removes a previously set custom header.
func (c *Client) RemoveHeader(key string) {
	c.headerMu.Lock()
	defer c.headerMu.Unlock()

	delete(c.headers, key)
}

// SetTimeout sets the request timeout.
func (c *Client) SetTimeout(timeout time.Duration) {
	c.timeout = timeout
	c.httpClient.Timeout = timeout
}

// SetKeepAlive updates the number of idle keep-alive connections retained by
// the live transport (MaxIdleConns, and MaxIdleConnsPerHost when no explicit
// MaxIdleConnsPerHost override was configured), taking effect for subsequent
// requests without reconstructing the client. It is a no-op if the transport
// is not the *http.Transport this client constructs (never true in practice:
// NewClient always builds one via createHTTPTransport).
func (c *Client) SetKeepAlive(connections int) {
	if c.options != nil {
		c.options.KeepAlive = connections
	}

	t, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		return
	}

	t.MaxIdleConns = connections

	if c.options == nil || c.options.MaxIdleConnsPerHost <= 0 {
		t.MaxIdleConnsPerHost = connections
	}
}

// SetMaxRetries sets the maximum number of retries.
func (c *Client) SetMaxRetries(retries int) {
	c.maxRetries = retries
}

// SetRetryDelay sets the delay between retries.
func (c *Client) SetRetryDelay(delay time.Duration) {
	c.retryDelay = delay
}

// ClientMetrics holds simple counters and durations.
type ClientMetrics struct {
	mu            sync.Mutex
	Requests      int64
	Errors        int64
	TotalDuration time.Duration
}

func (m *ClientMetrics) addRequest(duration time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Requests++
	if err != nil {
		m.Errors++
	}

	m.TotalDuration += duration
}

// Metrics returns a snapshot of client metrics.
func (c *Client) Metrics() ClientMetrics {
	c.metrics.mu.Lock()
	defer c.metrics.mu.Unlock()

	return ClientMetrics{Requests: c.metrics.Requests, Errors: c.metrics.Errors, TotalDuration: c.metrics.TotalDuration}
}

// SetMetrics attaches a Prometheus-friendly metrics collector.
func (c *Client) SetMetrics(m *pmetrics.DefaultMetrics) { c.prom = m }

// SetTFAHandler installs a handler to automatically complete two-factor authentication challenges.
func (c *Client) SetTFAHandler(h auth.TFAHandler) { c.tfaHandler = h }

// Authenticate performs explicit authentication with the PVE API.
// This is a public wrapper around ensureAuthentication for use by the client package.
func (c *Client) Authenticate() error {
	return c.ensureAuthentication()
}

// Logout invalidates the current session if using ticket-based authentication.
func (c *Client) Logout() error {
	if c.authenticator == nil {
		return nil
	}

	err := c.authenticator.Logout()
	if err != nil {
		return fmt.Errorf("failed to logout: %w", err)
	}

	return nil
}

// Close releases resources held by the client: it stops the response-cache
// cleanup goroutine (if caching is enabled) and closes idle HTTP connections.
// It is safe to call Close more than once. After Close the client should not
// be used for further requests.
func (c *Client) Close() error {
	if c.cache != nil {
		c.cache.Close()
	}

	if c.httpClient != nil {
		c.httpClient.CloseIdleConnections()
	}

	return nil
}

// InvalidateCache removes cache entries matching the given pattern.
// Pattern supports wildcard (*) at the end, e.g., "/nodes/*" invalidates all node paths.
// Returns the number of entries invalidated.
func (c *Client) InvalidateCache(pattern string) int {
	if c.cache != nil {
		return c.cache.Invalidate(pattern)
	}

	return 0
}

// ClearCache removes all cached entries.
func (c *Client) ClearCache() {
	if c.cache != nil {
		c.cache.Clear()
	}
}

// CacheStats returns current cache statistics.
// Returns nil if caching is not enabled.
func (c *Client) CacheStats() *cache.CacheStats {
	if c.cache != nil {
		stats := c.cache.Stats()

		return &stats
	}

	return nil
}

// isAuthenticated checks if the client is currently authenticated.
func (c *Client) isAuthenticated() bool {
	if c.authenticator == nil {
		return false
	}

	return c.authenticator.IsAuthenticated()
}

// needsLogin determines if automatic login should be attempted.
// Auto-login only applies to username/password authentication, not API tokens or pre-existing tickets.
func (c *Client) needsLogin() bool {
	if c.options == nil {
		return false
	}
	// Only auto-login for username/password auth (not API tokens or pre-existing tickets)
	return c.options.Username != "" &&
		c.options.Password != "" &&
		c.options.APIToken == "" &&
		c.options.Ticket == ""
}

func (c *Client) buildUploadRequest(ctx context.Context, path string, fields map[string]string, fileField, filename string, file io.Reader) (*http.Request, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	fullURL := c.baseURL + path

	requestBuilder := NewRequestBuilder("POST", c.baseURL, path)
	for k, v := range fields {
		requestBuilder.AddFormParam(k, v)
	}

	requestBuilder.AddFile(fileField, filename, file)

	body, contentType, err := requestBuilder.BuildBody()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "pve-apiclient-go/1.0")
	c.applyCustomHeaders(req)

	return req, nil
}

func (c *Client) recordRequestStart(req *http.Request) {
	if c.prom != nil {
		c.prom.ActiveConnections.Inc()

		if req != nil && req.ContentLength > 0 {
			c.prom.BytesSent.Add(req.ContentLength)
		}
	}
}

func (c *Client) recordRequestError(start time.Time) {
	if c.prom != nil {
		c.prom.RequestsTotal.Inc()
		c.prom.RequestsFailedTotal.Inc()
		c.prom.RequestDuration.Observe(time.Since(start).Seconds())
		c.prom.ActiveConnections.Dec()
	}
}

func (c *Client) recordRequestComplete(req *http.Request, resp *http.Response, bodyBytes []byte, start time.Time, perr error) {
	dur := time.Since(start)
	c.metrics.addRequest(dur, perr)

	if c.prom != nil {
		c.prom.RequestsTotal.Inc()

		if perr != nil || resp.StatusCode >= 400 {
			c.prom.RequestsFailedTotal.Inc()
		}

		c.prom.RequestDuration.Observe(dur.Seconds())

		if bodyBytes != nil {
			c.prom.BytesReceived.Add(int64(len(bodyBytes)))
		}

		c.prom.ActiveConnections.Dec()
	}

	c.logResponse(req, resp, bodyBytes, start, perr)
}

func (c *Client) logResponse(req *http.Request, resp *http.Response, bodyBytes []byte, start time.Time, perr error) {
	if c.logger == nil || !c.logConfig.Enabled {
		return
	}

	fields := map[string]interface{}{
		logFieldMethod: req.Method,
		logFieldURL:    req.URL.String(),
		"status":       resp.StatusCode,
		"duration":     int64(time.Since(start) / time.Millisecond),
	}

	if c.logConfig.LogResponseHeader {
		fields["resp_headers"] = redact(resp.Header, c.logConfig.RedactHeaders)
	}

	if c.logConfig.LogResponseBody && len(bodyBytes) > 0 {
		limit := c.logConfig.MaxBodyBytes
		if limit <= 0 || limit > len(bodyBytes) {
			limit = len(bodyBytes)
		}

		fields["resp_body"] = string(bodyBytes[:limit])
	}

	if perr != nil {
		c.logger.Error("http.response", fields)
	} else {
		c.logger.Info("http.response", fields)
	}
}

func (c *Client) buildRequestWithContext(ctx context.Context, method, path string, params map[string]interface{}) (*http.Request, error) {
	// Ensure path starts with /
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	fullURL := c.baseURL + path

	var (
		body        io.Reader
		contentType string
	)

	switch method {
	case "GET", "DELETE":
		// Add parameters to query string using the Proxmox-aware encoder
		// (bool→0/1, slice→repeated, time.Time→unix, nested map→k=v,k=v).
		if len(params) > 0 {
			query := url.Values{}
			for key, value := range params {
				addEncodedParam(query, key, value)
			}

			fullURL += "?" + query.Encode()
		}
	case "POST", "PUT":
		// Encode parameters as form data using the Proxmox-aware encoder.
		if len(params) > 0 {
			formData := url.Values{}
			for key, value := range params {
				addEncodedParam(formData, key, value)
			}

			body = strings.NewReader(formData.Encode())
			contentType = contentTypeFormURLEncoded
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Set content type if needed
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	// Set standard headers
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "pve-apiclient-go/1.0")
	c.applyCustomHeaders(req)

	return req, nil
}

func (c *Client) executeWithMiddleware(req *http.Request) (*http.Response, error) {
	// Create the final handler that executes the actual request
	finalHandler := func(r *http.Request) (*http.Response, error) {
		return c.httpClient.Do(r)
	}

	// Build the middleware chain in reverse order
	handler := finalHandler

	for i := len(c.middleware) - 1; i >= 0; i-- {
		middleware := c.middleware[i]
		currentHandler := handler
		handler = func(r *http.Request) (*http.Response, error) {
			return middleware(r, currentHandler)
		}
	}

	// Execute the chain
	return handler(req)
}

func (c *Client) parseResponse(resp *http.Response) (*Response, []byte, error) {
	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Check for non-success status codes
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, body, fmt.Errorf("API request failed: %w", apierrors.ParseAPIError(resp.StatusCode, body))
	}

	// Parse JSON response
	var result struct {
		Data    interface{}       `json:"data"`
		Success int               `json:"success,omitempty"`
		Message string            `json:"message,omitempty"`
		Errors  map[string]string `json:"errors,omitempty"`
	}

	err = json.Unmarshal(body, &result)
	if err != nil {
		// If JSON parsing fails, return raw body as data
		return &Response{
			Data: string(body),
			Code: resp.StatusCode,
		}, body, fmt.Errorf("failed to unmarshal response body: %w", err)
	}

	// Check for API-level errors
	if result.Success == 0 && result.Message != "" {
		return nil, body, &apierrors.APIError{
			Message:  result.Message,
			Code:     resp.StatusCode,
			Errors:   result.Errors,
			HTTPCode: resp.StatusCode,
		}
	}

	return &Response{
		Data:   result.Data,
		Errors: result.Errors,
		Code:   resp.StatusCode,
	}, body, nil
}

// ensureAuthentication handles initial authentication including TFA if needed.
func (c *Client) ensureAuthentication() error {
	if c.authenticator == nil || c.authenticator.IsAuthenticated() {
		return nil
	}

	err := c.authenticator.Authenticate()
	if err == nil {
		return nil
	}

	// Try to handle TFA if configured
	authErr := c.handleTFAAuthentication(err)
	if authErr != nil {
		return authErr
	}

	// Check if authentication succeeded after TFA
	if !c.authenticator.IsAuthenticated() {
		return fmt.Errorf("authentication failed: %w", err)
	}

	return nil
}

// handleTFAAuthentication processes TFA challenges if a TFA handler is configured.
func (c *Client) handleTFAAuthentication(authErr error) error {
	if c.tfaHandler == nil {
		return nil
	}

	var tferr *apierrors.TFARequiredError
	if !errors.As(authErr, &tferr) {
		return nil
	}

	ticketAuth, ok := c.authenticator.(*auth.TicketAuthenticator)
	if !ok {
		return nil
	}

	challenge := &auth.TFAChallenge{
		Ticket:    tferr.Ticket,
		Challenge: tferr.Challenge,
		Types:     tferr.Types,
	}

	response, err := c.tfaHandler.HandleTFAChallenge(challenge)
	if err != nil {
		return fmt.Errorf("tfa handling failed: %w", err)
	}

	_, err = ticketAuth.CompleteTFA(challenge, response)
	if err != nil {
		return fmt.Errorf("tfa completion failed: %w", err)
	}

	return nil
}

// applyAuthHeaders adds authentication headers to the request.
func (c *Client) applyAuthHeaders(req *http.Request) {
	if c.authenticator == nil {
		return
	}

	headers := c.authenticator.GetHeaders()
	for key, value := range headers {
		req.Header.Set(key, value)
	}
}

// applyCustomHeaders sets caller-configured headers on the request. It runs
// after the standard headers so callers may override defaults such as
// User-Agent; authentication headers are applied later and always win.
func (c *Client) applyCustomHeaders(req *http.Request) {
	c.headerMu.RLock()
	defer c.headerMu.RUnlock()

	for key, value := range c.headers {
		req.Header.Set(key, value)
	}
}

// handleAuthenticationRetry handles 401 responses by refreshing auth and retrying.
func (c *Client) handleAuthenticationRetry(req *http.Request, resp *http.Response, next Handler) (*http.Response, error) {
	if resp.StatusCode != http.StatusUnauthorized || c.authenticator == nil {
		return resp, nil
	}

	// Static-credential authenticators (API tokens, or the invalid
	// authenticator standing in for a misconfiguration) cannot obtain fresh
	// credentials. A 401 means the credentials themselves are rejected, so
	// retrying would replay the same value and 401 again — and routing through
	// Refresh() would mask the real 401 behind a synthetic re-auth error.
	// Surface the original 401 response to the caller untouched.
	if !canReauthenticate(c.authenticator) {
		return resp, nil
	}

	// Close the response body
	_ = resp.Body.Close()

	// A 401 means the credentials the request carried were rejected, even if the
	// locally cached ticket still looks valid. A plain Refresh() is a no-op while
	// the ticket is locally unexpired, which would make us replay the same
	// rejected credentials and loop on 401. Force a fresh authentication so the
	// single retry below carries new credentials.
	err := c.forceReauthenticate()
	if err != nil {
		return nil, fmt.Errorf("failed to refresh authentication: %w", err)
	}

	// Update headers with new authentication
	c.applyAuthHeaders(req)

	// Rewind the body so the retried request resends it intact (the first send
	// drained it). GetBody is populated for the in-memory bodies this client
	// builds; if absent there is no body to rewind.
	if req.GetBody != nil {
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return nil, fmt.Errorf("failed to rewind request body after re-auth: %w", bodyErr)
		}

		req.Body = body
	}

	// Retry the request exactly once with refreshed credentials.
	return next(req)
}

// canReauthenticate reports whether the authenticator can obtain fresh
// credentials after a 401. Ticket authentication can re-login; static API
// tokens and the invalid (misconfigured) authenticator cannot, so retrying a
// 401 with them only replays rejected credentials.
func canReauthenticate(a auth.Authenticator) bool {
	_, ok := a.(*auth.TicketAuthenticator)

	return ok
}

// forceReauthenticate re-establishes credentials after a 401. For ticket-based
// authentication it forces a renewal (RefreshForce) regardless of local ticket
// validity, since the server has rejected the current ticket. For other
// authenticator types it falls back to Refresh.
func (c *Client) forceReauthenticate() error {
	if c.authenticator == nil {
		return nil
	}

	if ta, ok := c.authenticator.(*auth.TicketAuthenticator); ok {
		err := ta.RefreshForce()
		if err != nil {
			return fmt.Errorf("forced re-authentication failed: %w", err)
		}

		return nil
	}

	err := c.authenticator.Refresh()
	if err != nil {
		return fmt.Errorf("re-authentication failed: %w", err)
	}

	return nil
}

// cachedResponse wraps an HTTP response for caching.
type cachedResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// CacheSize implements cache.Sizer so the cache charges this entry its actual
// body and header footprint instead of JSON-encoding the whole value (which
// would base64-inflate Body) just to measure it.
func (r *cachedResponse) CacheSize() int64 {
	size := int64(len(r.Body))
	for key, values := range r.Headers {
		size += int64(len(key))
		for _, value := range values {
			size += int64(len(value))
		}
	}

	return size
}

func (c *Client) cachingMiddleware(req *http.Request, next Handler) (*http.Response, error) {
	// Only cache GET requests
	if req.Method != http.MethodGet || c.cache == nil {
		return next(req)
	}

	// Generate cache key from URL
	cacheKey := cache.GenerateKeyFromURL(req.Method, req.URL.String())

	// Check cache
	if cached, found := c.cache.Get(cacheKey); found {
		if resp, ok := cached.(*cachedResponse); ok {
			// Convert cached response back to http.Response
			httpResp := &http.Response{
				StatusCode: resp.StatusCode,
				Header:     resp.Headers,
				Body:       io.NopCloser(bytes.NewReader(resp.Body)),
				Request:    req,
			}

			return httpResp, nil
		}
	}

	// Cache miss - execute request
	resp, err := next(req)
	if err != nil {
		return nil, err
	}

	// Cache successful responses (2xx status codes)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Read response body
		bodyBytes, err := io.ReadAll(resp.Body)

		_ = resp.Body.Close()
		if err != nil {
			// If we can't read body, return response without caching
			resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))

			return resp, fmt.Errorf("failed to read response body: %w", err)
		}

		// Create cached response
		cached := &cachedResponse{
			StatusCode: resp.StatusCode,
			Headers:    resp.Header.Clone(),
			Body:       bodyBytes,
		}

		// Store in cache with default TTL
		c.cache.Set(cacheKey, cached, 0)

		// Restore body for return
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	return resp, nil
}

func (c *Client) authMiddleware(req *http.Request, next Handler) (*http.Response, error) {
	// Check if ticket needs renewal before making the request.
	// PVE tickets have 2-hour validity, but should be renewed after 1 hour
	// to prevent expiration during long-running operations.
	c.renewTicketIfNeeded()

	// Handle auto-login or standard authentication
	err := c.handleAuthentication()
	if err != nil {
		return nil, err
	}

	// Add authentication headers
	c.applyAuthHeaders(req)

	// Execute the request
	resp, err := next(req)
	if err != nil {
		return nil, err
	}

	// Handle authentication retry if needed
	return c.handleAuthenticationRetry(req, resp, next)
}

// renewTicketIfNeeded checks and renews the ticket if it's approaching expiration.
func (c *Client) renewTicketIfNeeded() {
	ticketAuth, ok := c.authenticator.(*auth.TicketAuthenticator)
	if !ok {
		return
	}

	ticket := ticketAuth.GetTicket()
	if ticket == nil || !ticket.ShouldRenew(time.Hour) {
		return
	}

	// Ticket is approaching expiration (> 1 hour old), renew it proactively
	err := ticketAuth.RefreshForce()
	if err != nil {
		// Log the renewal failure but don't fail the request
		// The ticket may still be valid enough to complete this request
		c.logTicketRenewalFailure(err)
	}
}

// logTicketRenewalFailure logs ticket renewal failures if logging is enabled.
func (c *Client) logTicketRenewalFailure(err error) {
	if c.logger == nil || !c.logConfig.Enabled {
		return
	}

	c.logger.Warn("automatic ticket renewal failed", map[string]interface{}{
		"error": err.Error(),
	})
}

// handleAuthentication processes auto-login or standard authentication.
func (c *Client) handleAuthentication() error {
	if !c.shouldAutoLogin() {
		return c.ensureAuthentication()
	}

	return c.performAutoLogin()
}

// shouldAutoLogin checks if auto-login should be attempted.
func (c *Client) shouldAutoLogin() bool {
	return c.options != nil && c.options.AutoLogin && !c.isAuthenticated() && c.needsLogin()
}

// performAutoLogin handles the auto-login logic with mutex protection.
func (c *Client) performAutoLogin() error {
	c.loginMutex.Lock()

	// Double-check after acquiring lock (another goroutine may have logged in)
	if c.isAuthenticated() || c.loginAttempted {
		c.loginMutex.Unlock()

		return nil
	}

	c.loginAttempted = true
	c.loginMutex.Unlock() // Unlock before authentication to allow other operations

	err := c.ensureAuthentication()
	if err != nil {
		return fmt.Errorf("auto-login failed: %w", err)
	}

	return nil
}

// pveConnectionPseudoStatus is the non-standard status PVE proxies emit when an
// upstream connection cannot be established. It must be treated as a connection
// failure rather than fed back into the HTTP-status retry path (which would
// otherwise loop on it).
const pveConnectionPseudoStatus = 596

func (c *Client) retryMiddleware(req *http.Request, next Handler) (*http.Response, error) {
	retries, delay, forceRetry := c.resolveRetryPolicy(req)

	// Whether this request may be retried at all. Idempotent methods are always
	// eligible; non-idempotent methods only when the caller explicitly opted in,
	// because a retry can duplicate server-side side effects (e.g. VM create).
	retryAllowed := isIdempotentMethod(req.Method) || forceRetry

	// Buffer the body once up front so every allowed retry resends it intact.
	// req.GetBody is populated by http.NewRequestWithContext for the in-memory
	// body readers this client uses; capture it before the first send because
	// the first send drains req.Body.
	getBody, err := c.captureRequestBody(req)
	if err != nil {
		return nil, err
	}

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			time.Sleep(applyRetryJitter(delay * time.Duration(attempt)))

			rewindErr := rewindRequestBody(req, getBody)
			if rewindErr != nil {
				return nil, rewindErr
			}
		}

		resp, doErr := next(req)
		if doErr != nil {
			if attempt >= retries || !retryAllowed {
				return nil, fmt.Errorf("request failed after %d attempt(s): %w", attempt+1, doErr)
			}

			continue
		}

		// Treat the PVE pseudo-status as a connection failure: never feed it back
		// into the status retry loop. It may be retried like a network error only
		// when the request is retry-eligible and attempts remain.
		if resp.StatusCode == pveConnectionPseudoStatus {
			_ = resp.Body.Close()

			if attempt >= retries || !retryAllowed {
				return nil, &apierrors.ConnectionError{
					Host:    req.URL.Hostname(),
					Message: "PVE reported an upstream connection failure (596)",
				}
			}

			continue
		}

		if retryAllowed && attempt < retries && apierrors.IsRetryableCode(resp.StatusCode) {
			_ = resp.Body.Close()

			continue
		}

		// Success, non-retryable status, or a retryable status with no attempts
		// left / not eligible for retry: hand the response back to the caller so
		// parseResponse can surface the appropriate error.
		return resp, nil
	}
}

// retryJitterPercent bounds the +/- random jitter applied to retry backoff
// delays (see applyRetryJitter), mirroring the poll-interval jitter approach
// in pkg/api/tasks.
const retryJitterPercent = 20

// applyRetryJitter randomizes a backoff delay by up to +/- retryJitterPercent%
// so concurrent clients retrying the same transient failure do not all wake
// up and retry in lockstep. It returns the unmodified delay if it is zero or
// negative, or if the random source is unavailable.
func applyRetryJitter(delay time.Duration) time.Duration {
	if delay <= 0 {
		return delay
	}

	delta := int64(delay) * retryJitterPercent / constants.JitterPercentage
	if delta == 0 {
		return delay
	}

	offset, err := rand.Int(rand.Reader, big.NewInt(2*delta+1))
	if err != nil {
		return delay
	}

	adjusted := int64(delay) + offset.Int64() - delta
	if adjusted < 1 {
		adjusted = 1
	}

	return time.Duration(adjusted)
}

// resolveRetryPolicy returns the effective retry count, delay, and force-retry
// flag for a request, applying any per-request context overrides.
func (c *Client) resolveRetryPolicy(req *http.Request) (int, time.Duration, bool) {
	retries := c.maxRetries
	delay := c.retryDelay
	force := false

	if opts := FromContext(req.Context()); opts != nil {
		if opts.Retries != nil {
			retries = *opts.Retries
		}

		if opts.RetryDelay != nil {
			delay = *opts.RetryDelay
		}

		if opts.ForceRetry != nil {
			force = *opts.ForceRetry
		}
	}

	if retries < 0 {
		retries = 0
	}

	return retries, delay, force
}

// captureRequestBody returns a function that produces a fresh ReadCloser for the
// request body, used to rewind the body before each retry. For bodyless requests
// it returns nil. It surfaces any error so a buffering failure is not silently
// converted into an empty-body retry.
func (c *Client) captureRequestBody(req *http.Request) (func() (io.ReadCloser, error), error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil //nolint:nilnil // nil getter signals "no body"; callers handle nil getter explicitly
	}

	if req.GetBody != nil {
		return req.GetBody, nil
	}

	// Fallback: buffer the body bytes ourselves. Reading here does not lose the
	// first attempt because we restore req.Body immediately afterward.
	bodyBytes, readErr := io.ReadAll(req.Body)

	_ = req.Body.Close()

	if readErr != nil {
		return nil, fmt.Errorf("failed to buffer request body for retry: %w", readErr)
	}

	req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	req.ContentLength = int64(len(bodyBytes))

	return func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(bodyBytes)), nil
	}, nil
}

// rewindRequestBody resets req.Body to a fresh copy before a retry attempt.
func rewindRequestBody(req *http.Request, getBody func() (io.ReadCloser, error)) error {
	if getBody == nil {
		return nil
	}

	body, err := getBody()
	if err != nil {
		return fmt.Errorf("failed to rewind request body for retry: %w", err)
	}

	req.Body = body

	return nil
}

// isIdempotentMethod reports whether an HTTP method is safe to auto-retry.
func isIdempotentMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

func (c *Client) loggingMiddleware(req *http.Request, next Handler) (*http.Response, error) {
	opts := FromContext(req.Context())
	if opts != nil && opts.Logging != nil && !*opts.Logging {
		return next(req)
	}

	start := time.Now()

	c.logRequest(req, "http.request", opts.Fields)
	resp, err := next(req)
	duration := time.Since(start)

	event := &Event{Method: req.Method, URL: req.URL.String(), Duration: duration, Err: err}
	if resp != nil {
		event.Status = resp.StatusCode
	}

	if c.logger != nil && c.logConfig.Enabled {
		fields := map[string]interface{}{
			logFieldMethod: event.Method,
			logFieldURL:    event.URL,
			"status":       event.Status,
			"duration":     int64(event.Duration / time.Millisecond),
		}
		// propagate request fields
		for k, v := range opts.Fields {
			fields[k] = v
		}

		if err != nil {
			c.logger.Error("http.response", fields)
		} else {
			c.logger.Info("http.response", fields)
		}
	}

	c.fireHook(event)

	return resp, err
}
