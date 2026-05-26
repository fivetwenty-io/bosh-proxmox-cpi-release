package registry_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/registry"
)

// testServer creates an httptest.Server and returns it together with its URL.
// Callers must close the server after the test.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*registry.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return registry.NewClient(srv.URL, "user", "secret"), srv
}

// --------------------------------------------------------------------------
// PUT
// --------------------------------------------------------------------------

func TestPut_Success(t *testing.T) {
	type settings struct {
		AgentID string `json:"agent_id"`
	}

	var gotMethod, gotPath, gotAuth string
	var gotEnvelope map[string]string

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")

		if err := json.NewDecoder(r.Body).Decode(&gotEnvelope); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	cfg := settings{AgentID: "abc-123"}
	if err := client.Put(context.Background(), "100", cfg); err != nil {
		t.Fatalf("Put returned unexpected error: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Errorf("method: got %q, want PUT", gotMethod)
	}
	if gotPath != "/instances/100/settings" {
		t.Errorf("path: got %q, want /instances/100/settings", gotPath)
	}
	// Verify the settings field is a JSON string (not nested object).
	settingsStr, ok := gotEnvelope["settings"]
	if !ok {
		t.Fatal("envelope missing 'settings' key")
	}
	var parsed settings
	if err := json.Unmarshal([]byte(settingsStr), &parsed); err != nil {
		t.Fatalf("settings value is not valid JSON: %v", err)
	}
	if parsed.AgentID != "abc-123" {
		t.Errorf("agent_id: got %q, want abc-123", parsed.AgentID)
	}
	// Verify Basic Auth header is present.
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Errorf("Authorization header: got %q, want Basic ...", gotAuth)
	}
}

func TestPut_Non2xx(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	})

	err := client.Put(context.Background(), "100", map[string]string{"k": "v"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status 500: %v", err)
	}
}

func TestPut_EmptyInstanceID(t *testing.T) {
	client := registry.NewClient("http://localhost:25777", "u", "p")
	err := client.Put(context.Background(), "", map[string]string{})
	if err == nil {
		t.Fatal("expected error for empty instanceID")
	}
}

// --------------------------------------------------------------------------
// GET
// --------------------------------------------------------------------------

func TestGet_Success(t *testing.T) {
	inner := map[string]any{
		"agent_id": "xyz-789",
		"mbus":     "nats://host:4222",
	}
	innerJSON, _ := json.Marshal(inner)
	envelope := map[string]string{"settings": string(innerJSON)}
	envBody, _ := json.Marshal(envelope)

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(envBody) //nolint:errcheck
	})

	raw, err := client.Get(context.Background(), "100")
	if err != nil {
		t.Fatalf("Get returned unexpected error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("returned RawMessage is not valid JSON: %v", err)
	}
	if got["agent_id"] != "xyz-789" {
		t.Errorf("agent_id: got %v, want xyz-789", got["agent_id"])
	}
}

func TestGet_404(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	raw, err := client.Get(context.Background(), "100")
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if raw != nil {
		t.Errorf("expected nil RawMessage on error, got %s", raw)
	}
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention not-found/404: %v", err)
	}
}

func TestGet_Non2xx(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	})

	_, err := client.Get(context.Background(), "100")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should mention status 503: %v", err)
	}
}

func TestGet_EmptyInstanceID(t *testing.T) {
	client := registry.NewClient("http://localhost:25777", "u", "p")
	_, err := client.Get(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty instanceID")
	}
}

// --------------------------------------------------------------------------
// DELETE
// --------------------------------------------------------------------------

func TestDelete_Success(t *testing.T) {
	var gotMethod, gotPath string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})

	if err := client.Delete(context.Background(), "100"); err != nil {
		t.Fatalf("Delete returned unexpected error: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method: got %q, want DELETE", gotMethod)
	}
	if gotPath != "/instances/100/settings" {
		t.Errorf("path: got %q, want /instances/100/settings", gotPath)
	}
}

func TestDelete_404(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	// 404 must be treated as success (idempotent).
	if err := client.Delete(context.Background(), "100"); err != nil {
		t.Fatalf("Delete on 404 should return nil (idempotent), got: %v", err)
	}
}

func TestDelete_500(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "crash", http.StatusInternalServerError)
	})

	err := client.Delete(context.Background(), "100")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status 500: %v", err)
	}
}

func TestDelete_EmptyInstanceID(t *testing.T) {
	client := registry.NewClient("http://localhost:25777", "u", "p")
	err := client.Delete(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty instanceID")
	}
}

// --------------------------------------------------------------------------
// Auth header encoding
// --------------------------------------------------------------------------

func TestAuthHeader_Encoded(t *testing.T) {
	const user, pass = "admin", "s3cr3t!"
	var gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := registry.NewClient(srv.URL, user, pass)
	_ = client.Put(context.Background(), "1", struct{}{})

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	if gotAuth != want {
		t.Errorf("Authorization: got %q, want %q", gotAuth, want)
	}
}

// --------------------------------------------------------------------------
// Trailing slash on endpoint
// --------------------------------------------------------------------------

func TestEndpointTrimmed(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Endpoint with trailing slash — should not produce double slash in URL.
	client := registry.NewClient(srv.URL+"/", "u", "p")
	_ = client.Put(context.Background(), "42", struct{}{})

	if strings.Contains(gotPath, "//") {
		t.Errorf("path contains double slash: %q", gotPath)
	}
	if gotPath != "/instances/42/settings" {
		t.Errorf("path: got %q, want /instances/42/settings", gotPath)
	}
}

// --------------------------------------------------------------------------
// Context cancellation
// --------------------------------------------------------------------------

func TestContextCancellation(t *testing.T) {
	// Server that delays long enough for the context to expire.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	client := registry.NewClient(srv.URL, "u", "p")
	err := client.Put(ctx, "100", struct{}{})
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
	// Error string should mention context or deadline.
	if !strings.Contains(err.Error(), "context") &&
		!strings.Contains(err.Error(), "deadline") &&
		!strings.Contains(err.Error(), "canceled") &&
		!strings.Contains(err.Error(), "timed out") {
		t.Logf("error (acceptable, just checking): %v", err)
	}
}

// --------------------------------------------------------------------------
// Content-Type
// --------------------------------------------------------------------------

func TestPut_ContentTypeJSON(t *testing.T) {
	var gotCT string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	})

	_ = client.Put(context.Background(), "1", map[string]string{"k": "v"})
	if !strings.HasPrefix(gotCT, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", gotCT)
	}
}

// --------------------------------------------------------------------------
// Retry on transient failures
// --------------------------------------------------------------------------

// TestPut_RetriesOnTransient verifies that Put retries on a 503 and succeeds
// on the subsequent 200, issuing exactly 2 HTTP requests total.
func TestPut_RetriesOnTransient(t *testing.T) {
	var callCount int
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	err := client.Put(context.Background(), "100", map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("Put returned unexpected error after retry: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 HTTP calls (1 failure + 1 success), got %d", callCount)
	}
}

// TestPut_AllRetriesExhausted_5xx verifies Put returns an error after the
// retry budget is exhausted by persistent 5xx responses, and that exactly
// 3 HTTP requests are issued (1 initial + 2 retries).
func TestPut_AllRetriesExhausted_5xx(t *testing.T) {
	var callCount int
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	})

	err := client.Put(context.Background(), "100", map[string]string{"k": "v"})
	if err == nil {
		t.Fatal("expected error after retry budget exhausted, got nil")
	}
	if callCount != 3 {
		t.Errorf("expected 3 HTTP calls (1 + 2 retries), got %d", callCount)
	}
}

// TestPut_NoRetryOn4xx verifies that Put does not retry on a 400 Bad Request.
func TestPut_NoRetryOn4xx(t *testing.T) {
	var callCount int
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		http.Error(w, "bad request", http.StatusBadRequest)
	})

	err := client.Put(context.Background(), "100", map[string]string{"k": "v"})
	if err == nil {
		t.Fatal("expected error on 400, got nil")
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 HTTP call for non-retryable 400, got %d", callCount)
	}
}

// TestGet_RetriesOnTransient verifies that Get retries on a 503 response.
func TestGet_RetriesOnTransient(t *testing.T) {
	inner := map[string]any{"agent_id": "xyz"}
	innerJSON, _ := json.Marshal(inner)
	envelope := map[string]string{"settings": string(innerJSON)}
	envBody, _ := json.Marshal(envelope)

	var callCount int
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(envBody) //nolint:errcheck
	})

	raw, err := client.Get(context.Background(), "100")
	if err != nil {
		t.Fatalf("Get returned unexpected error after retry: %v", err)
	}
	if raw == nil {
		t.Fatal("expected non-nil RawMessage after retry")
	}
	if callCount != 2 {
		t.Errorf("expected 2 HTTP calls, got %d", callCount)
	}
}

// TestDelete_RetriesOnTransient verifies that Delete retries on a 503 response.
func TestDelete_RetriesOnTransient(t *testing.T) {
	var callCount int
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	err := client.Delete(context.Background(), "100")
	if err != nil {
		t.Fatalf("Delete returned unexpected error after retry: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 HTTP calls, got %d", callCount)
	}
}

// --------------------------------------------------------------------------
// Large instance ID (path injection guard)
// --------------------------------------------------------------------------

func TestPut_InstanceIDInPath(t *testing.T) {
	var gotPath string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})

	_ = client.Put(context.Background(), "vm-1234", map[string]string{})
	wantPath := fmt.Sprintf("/instances/vm-1234/settings")
	if gotPath != wantPath {
		t.Errorf("path: got %q, want %q", gotPath, wantPath)
	}
}
