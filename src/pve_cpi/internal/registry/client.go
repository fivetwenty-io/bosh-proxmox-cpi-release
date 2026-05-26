// Package registry provides an HTTP client for the BOSH registry service.
// The registry stores agent settings keyed by instance ID and exposes a
// simple REST API used by CPIs to configure BOSH agents on new VMs.
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// settingsEnvelope is the JSON envelope used for PUT bodies and GET responses.
// The BOSH registry wraps the agent settings JSON as a string value, not nested JSON.
type settingsEnvelope struct {
	Settings string `json:"settings"`
}

// Client is an HTTP client for the BOSH registry service.
// Instances are safe for concurrent use after construction.
type Client struct {
	endpoint string
	user     string
	pass     string
	http     *http.Client
}

// NewClient constructs a Client for the registry at endpoint.
// Trailing slashes on endpoint are trimmed. The underlying http.Client
// uses a 30-second per-attempt timeout with no connection keep-alive limit.
func NewClient(endpoint, user, pass string) *Client {
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		user:     user,
		pass:     pass,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// retryMaxAttempts is the total number of attempts (1 initial + 2 retries).
const retryMaxAttempts = 3

// retryBaseDelay is the base backoff interval before the first retry.
const retryBaseDelay = 200 * time.Millisecond

// isRetriable reports whether the HTTP response or transport error warrants a retry.
//
// Retried:
//   - net.Error with Timeout() == true (ETIMEDOUT, transport-layer deadline)
//   - io.ErrUnexpectedEOF (server closed the connection mid-response)
//   - syscall.ECONNRESET (connection reset by peer), including wrapped variants
//   - HTTP 5xx status codes
//   - HTTP 408 (Request Timeout)
//
// Not retried:
//   - context.Canceled or context.DeadlineExceeded (caller cancelled)
//   - HTTP 4xx except 408
//   - HTTP 2xx (success)
func isRetriable(resp *http.Response, err error) bool {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return true
		}
		if errors.Is(err, syscall.ECONNRESET) {
			return true
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return true
		}
		// Catch ECONNRESET wrapped inside *net.OpError.
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			if errors.Is(opErr.Err, syscall.ECONNRESET) {
				return true
			}
		}
		return false
	}
	if resp == nil {
		return false
	}
	if resp.StatusCode == http.StatusRequestTimeout { // 408
		return true
	}
	return resp.StatusCode >= 500
}

// backoffDelay returns the jittered exponential delay for attempt index i
// (0-based: i=0 is the delay before the first retry).
// Formula: base * 2^i * jitter, where jitter is uniform in [0.75, 1.25).
func backoffDelay(i int) time.Duration {
	base := retryBaseDelay
	for j := 0; j < i; j++ {
		base *= 2
	}
	jitter := 0.75 + rand.Float64()*0.5
	return time.Duration(float64(base) * jitter)
}

// doWithRetry executes req with up to retryMaxAttempts total attempts.
// Between attempts it waits for a jittered exponential backoff, or returns
// immediately if ctx is already cancelled.
//
// The 30-second http.Client.Timeout applies per attempt, not to the total.
// The request body is re-wound via req.GetBody between retries; callers must
// ensure req.GetBody is set when the method has a body (Put sets it automatically
// using bytes.NewReader; Get and Delete have no body).
func (c *Client) doWithRetry(req *http.Request) (*http.Response, error) {
	var (
		resp *http.Response
		err  error
	)
	for attempt := 0; attempt < retryMaxAttempts; attempt++ {
		if attempt > 0 {
			// Re-wind the request body for the retry attempt.
			if req.GetBody != nil {
				body, gberr := req.GetBody()
				if gberr != nil {
					return nil, gberr
				}
				req.Body = body
			}
			// Wait for backoff interval or context cancellation.
			delay := backoffDelay(attempt - 1)
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(delay):
			}
		}

		// Drain and close the previous failed response body before retrying.
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			resp = nil
		}

		resp, err = c.http.Do(req)
		if !isRetriable(resp, err) {
			return resp, err
		}
	}
	return resp, err
}

// Put serialises settings to JSON, wraps it in the registry envelope, and
// sends a PUT to /instances/{instanceID}/settings. Non-2xx responses are
// returned as a CloudError containing the HTTP status and response body.
// Transient failures (5xx, network timeout, ECONNRESET) are retried up to
// retryMaxAttempts times with jittered exponential backoff.
func (c *Client) Put(ctx context.Context, instanceID string, settings any) error {
	if instanceID == "" {
		return cpierrors.Cloud("registry: Put: instanceID must not be empty")
	}

	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		return cpierrors.Cloud("registry: Put: marshal settings: %s", err.Error())
	}

	env := settingsEnvelope{Settings: string(settingsJSON)}
	body, err := json.Marshal(env)
	if err != nil {
		return cpierrors.Cloud("registry: Put: marshal envelope: %s", err.Error())
	}

	url := fmt.Sprintf("%s/instances/%s/settings", c.endpoint, instanceID)
	bodyReader := bytes.NewReader(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bodyReader)
	if err != nil {
		return cpierrors.Cloud("registry: Put: build request: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.user, c.pass)
	// GetBody allows doWithRetry to rewind the body between attempts.
	req.GetBody = func() (io.ReadCloser, error) {
		if _, seekErr := bodyReader.Seek(0, io.SeekStart); seekErr != nil {
			return nil, seekErr
		}
		return io.NopCloser(bodyReader), nil
	}

	resp, err := c.doWithRetry(req)
	if err != nil {
		return cpierrors.Cloud("registry: Put: %s", err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return cpierrors.Cloud(
			"registry: Put %s: unexpected status %d: %s",
			instanceID, resp.StatusCode, strings.TrimSpace(string(respBody)),
		)
	}
	return nil
}

// Get fetches /instances/{instanceID}/settings and returns the raw JSON of
// the settings value (the string stored inside the envelope, re-parsed as
// json.RawMessage so callers can Unmarshal into concrete types).
// A 404 response returns a CloudError; callers may test with cpierrors.IsType.
// Transient failures are retried up to retryMaxAttempts times.
func (c *Client) Get(ctx context.Context, instanceID string) (json.RawMessage, error) {
	if instanceID == "" {
		return nil, cpierrors.Cloud("registry: Get: instanceID must not be empty")
	}

	url := fmt.Sprintf("%s/instances/%s/settings", c.endpoint, instanceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, cpierrors.Cloud("registry: Get: build request: %s", err.Error())
	}
	req.SetBasicAuth(c.user, c.pass)

	resp, err := c.doWithRetry(req)
	if err != nil {
		return nil, cpierrors.Cloud("registry: Get: %s", err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, cpierrors.Cloud("registry: Get %s: not found (404)", instanceID)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, cpierrors.Cloud(
			"registry: Get %s: unexpected status %d: %s",
			instanceID, resp.StatusCode, strings.TrimSpace(string(respBody)),
		)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, cpierrors.Cloud("registry: Get %s: read body: %s", instanceID, err.Error())
	}

	var env settingsEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, cpierrors.Cloud("registry: Get %s: unmarshal envelope: %s", instanceID, err.Error())
	}

	// env.Settings is the JSON-encoded string; parse it so callers get raw JSON.
	return json.RawMessage(env.Settings), nil
}

// Delete sends DELETE /instances/{instanceID}/settings.
// A 404 response is treated as success (idempotent). Non-2xx, non-404
// responses are returned as a CloudError. Transient failures are retried
// up to retryMaxAttempts times with jittered exponential backoff.
func (c *Client) Delete(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return cpierrors.Cloud("registry: Delete: instanceID must not be empty")
	}

	url := fmt.Sprintf("%s/instances/%s/settings", c.endpoint, instanceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return cpierrors.Cloud("registry: Delete: build request: %s", err.Error())
	}
	req.SetBasicAuth(c.user, c.pass)

	resp, err := c.doWithRetry(req)
	if err != nil {
		return cpierrors.Cloud("registry: Delete: %s", err.Error())
	}
	defer resp.Body.Close()

	// 404 is idempotent — the record is already gone.
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return cpierrors.Cloud(
			"registry: Delete %s: unexpected status %d: %s",
			instanceID, resp.StatusCode, strings.TrimSpace(string(respBody)),
		)
	}
	return nil
}
