// Package registry provides an HTTP client for the BOSH registry service.
// The registry stores agent settings keyed by instance ID and exposes a
// simple REST API used by CPIs to configure BOSH agents on new VMs.
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
// uses a 30-second timeout with no connection keep-alive limit.
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

// Put serialises settings to JSON, wraps it in the registry envelope, and
// sends a PUT to /instances/{instanceID}/settings. Non-2xx responses are
// returned as a CloudError containing the HTTP status and response body.
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return cpierrors.Cloud("registry: Put: build request: %s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.user, c.pass)

	resp, err := c.http.Do(req)
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

	resp, err := c.http.Do(req)
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
// responses are returned as a CloudError.
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

	resp, err := c.http.Do(req)
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
