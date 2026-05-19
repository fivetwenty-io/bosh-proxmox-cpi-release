package http

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/internal/constants"
)

var (
	ErrRequestTimeout = errors.New("request timeout")
)

// MiddlewareFunc is a function that wraps an HTTP handler.
type MiddlewareFunc func(http.Handler) http.Handler

// Chain creates a middleware chain.
type Chain struct {
	middlewares []MiddlewareFunc
}

// NewChain creates a new middleware chain.
func NewChain(middlewares ...MiddlewareFunc) *Chain {
	return &Chain{
		middlewares: middlewares,
	}
}

// Then wraps the final handler with the middleware chain.
func (c *Chain) Then(handler http.Handler) http.Handler {
	for i := len(c.middlewares) - 1; i >= 0; i-- {
		handler = c.middlewares[i](handler)
	}

	return handler
}

// Append adds middleware to the chain.
func (c *Chain) Append(middlewares ...MiddlewareFunc) *Chain {
	newMiddlewares := make([]MiddlewareFunc, len(c.middlewares)+len(middlewares))
	copy(newMiddlewares, c.middlewares)
	copy(newMiddlewares[len(c.middlewares):], middlewares)

	return &Chain{middlewares: newMiddlewares}
}

// RateLimitMiddleware implements rate limiting.
type RateLimitMiddleware struct {
	requestsPerSecond int
	burst             int
	lastRequest       time.Time
	tokens            int
}

// NewRateLimitMiddleware creates a new rate limit middleware.
func NewRateLimitMiddleware(requestsPerSecond, burst int) *RateLimitMiddleware {
	return &RateLimitMiddleware{
		requestsPerSecond: requestsPerSecond,
		burst:             burst,
		tokens:            burst,
		lastRequest:       time.Now(),
	}
}

// Apply applies rate limiting to a request.
func (rl *RateLimitMiddleware) Apply(req *http.Request, next Handler) (*http.Response, error) {
	now := time.Now()
	elapsed := now.Sub(rl.lastRequest)
	rl.lastRequest = now

	// Add tokens based on elapsed time
	tokensToAdd := int(elapsed.Seconds() * float64(rl.requestsPerSecond))

	rl.tokens += tokensToAdd
	if rl.tokens > rl.burst {
		rl.tokens = rl.burst
	}

	// Check if we have tokens available
	if rl.tokens <= 0 {
		// Wait for a token to become available
		waitTime := time.Second / time.Duration(rl.requestsPerSecond)
		time.Sleep(waitTime)

		rl.tokens = 1
	}

	// Consume a token
	rl.tokens--

	// Execute the request
	return next(req)
}

// LoggingMiddleware implements request/response logging.
type LoggingMiddleware struct {
	logger     *log.Logger
	logBody    bool
	maxBodyLog int
}

// NewLoggingMiddleware creates a new logging middleware.
func NewLoggingMiddleware(logger *log.Logger) *LoggingMiddleware {
	if logger == nil {
		logger = log.New(log.Writer(), "[PVE] ", log.LstdFlags)
	}

	return &LoggingMiddleware{
		logger:     logger,
		logBody:    false,
		maxBodyLog: constants.DefaultBufferSize,
	}
}

// Apply applies logging to a request.
func (lm *LoggingMiddleware) Apply(req *http.Request, next Handler) (*http.Response, error) {
	start := time.Now()

	// Log request
	lm.logger.Printf("→ %s %s", req.Method, req.URL.String())

	// Log request body if enabled
	if lm.logBody && req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err == nil {
			if len(body) > lm.maxBodyLog {
				lm.logger.Printf("  Body: %s... (truncated)", body[:lm.maxBodyLog])
			} else {
				lm.logger.Printf("  Body: %s", body)
			}

			req.Body = io.NopCloser(bytes.NewReader(body))
		}
	}

	// Execute request
	resp, err := next(req)
	duration := time.Since(start)

	// Log response
	if err != nil {
		lm.logger.Printf("← ERROR after %v: %v", duration, err)

		return nil, err
	}

	lm.logger.Printf("← %d %s (%v)", resp.StatusCode, http.StatusText(resp.StatusCode), duration)

	// Log response body if enabled
	if lm.logBody && resp.Body != nil {
		body, err := io.ReadAll(resp.Body)
		if err == nil {
			if len(body) > lm.maxBodyLog {
				lm.logger.Printf("  Body: %s... (truncated)", body[:lm.maxBodyLog])
			} else {
				lm.logger.Printf("  Body: %s", body)
			}

			resp.Body = io.NopCloser(bytes.NewReader(body))
		}
	}

	return resp, nil
}

// MetricsMiddleware collects metrics about requests.
type MetricsMiddleware struct {
	totalRequests  int64
	totalErrors    int64
	totalDuration  time.Duration
	requestsByPath map[string]int64
	errorsByPath   map[string]int64
	durationByPath map[string]time.Duration
}

// NewMetricsMiddleware creates a new metrics middleware.
func NewMetricsMiddleware() *MetricsMiddleware {
	return &MetricsMiddleware{
		requestsByPath: make(map[string]int64),
		errorsByPath:   make(map[string]int64),
		durationByPath: make(map[string]time.Duration),
	}
}

// Apply collects metrics for a request.
func (mm *MetricsMiddleware) Apply(req *http.Request, next Handler) (*http.Response, error) {
	start := time.Now()
	path := req.URL.Path

	// Execute request
	resp, err := next(req)
	duration := time.Since(start)

	// Update metrics
	mm.totalRequests++
	mm.totalDuration += duration
	mm.requestsByPath[path]++
	mm.durationByPath[path] += duration

	if err != nil || (resp != nil && resp.StatusCode >= 400) {
		mm.totalErrors++
		mm.errorsByPath[path]++
	}

	return resp, err
}

// GetMetrics returns current metrics.
func (mm *MetricsMiddleware) GetMetrics() map[string]interface{} {
	avgDuration := time.Duration(0)
	if mm.totalRequests > 0 {
		avgDuration = mm.totalDuration / time.Duration(mm.totalRequests)
	}

	return map[string]interface{}{
		"total_requests":   mm.totalRequests,
		"total_errors":     mm.totalErrors,
		"total_duration":   mm.totalDuration.String(),
		"average_duration": avgDuration.String(),
		"requests_by_path": mm.requestsByPath,
		"errors_by_path":   mm.errorsByPath,
		"duration_by_path": mm.durationByPath,
	}
}

// TimeoutMiddleware implements request timeouts.
type TimeoutMiddleware struct {
	timeout time.Duration
}

// NewTimeoutMiddleware creates a new timeout middleware.
func NewTimeoutMiddleware(timeout time.Duration) *TimeoutMiddleware {
	return &TimeoutMiddleware{
		timeout: timeout,
	}
}

// Apply applies a timeout to a request.
func (tm *TimeoutMiddleware) Apply(req *http.Request, next Handler) (*http.Response, error) {
	// Create a channel for the response
	type result struct {
		resp *http.Response
		err  error
	}

	resultChan := make(chan result, 1)

	// Execute request in goroutine
	go func() {
		resp, err := next(req) //nolint:bodyclose // Middleware passes response body through
		resultChan <- result{resp, err}
	}()

	// Wait for response or timeout
	select {
	case res := <-resultChan:
		return res.resp, res.err
	case <-time.After(tm.timeout):
		// Clean up any response that might arrive later
		go func() {
			res := <-resultChan
			if res.resp != nil && res.resp.Body != nil {
				_ = res.resp.Body.Close()
			}
		}()

		return nil, fmt.Errorf("%w after %v", ErrRequestTimeout, tm.timeout)
	}
}

// HeaderMiddleware adds or modifies headers.
type HeaderMiddleware struct {
	headers map[string]string
}

// NewHeaderMiddleware creates a new header middleware.
func NewHeaderMiddleware(headers map[string]string) *HeaderMiddleware {
	return &HeaderMiddleware{
		headers: headers,
	}
}

// Apply adds headers to a request.
func (hm *HeaderMiddleware) Apply(req *http.Request, next Handler) (*http.Response, error) {
	// Add headers
	for key, value := range hm.headers {
		req.Header.Set(key, value)
	}

	// Execute request
	return next(req)
}

// CompressionMiddleware handles response compression.
type CompressionMiddleware struct {
	acceptedEncodings []string
}

// NewCompressionMiddleware creates a new compression middleware.
func NewCompressionMiddleware() *CompressionMiddleware {
	return &CompressionMiddleware{
		acceptedEncodings: []string{"gzip", "deflate"},
	}
}

// Apply adds compression headers to a request.
func (cm *CompressionMiddleware) Apply(req *http.Request, next Handler) (*http.Response, error) {
	// Add Accept-Encoding header
	req.Header.Set("Accept-Encoding", "gzip, deflate")

	// Execute request
	resp, err := next(req)
	if err != nil {
		return nil, err
	}

	// Response decompression is handled automatically by Go's HTTP client
	return resp, nil
}
