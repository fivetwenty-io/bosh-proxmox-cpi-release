package handlers_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// A ticket login that met a redirect reaches the start as a ConnectionError
// under the SDK's "auto-login failed" prefix, which the start fallback's text
// checks read as transient. The refusal is the same on every node, so the
// create must stop on the winner with a permanent error and never start the
// guest on the alternate.
func TestCreateVM_Fallback_RedirectRefusedStart_NoFallback(t *testing.T) {
	t.Parallel()

	var startCalls atomic.Int32
	var startNodes []string
	refusal := &sdkclient.RedirectError{Status: http.StatusFound, From: "https://pve1:8006/api2/json/access/ticket"}

	q := &vmMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			return "UPID:pve:create:ok", nil
		},
		startFn: func(_ context.Context, node string, _ int) (string, error) {
			startCalls.Add(1)
			startNodes = append(startNodes, node)
			return "", fmt.Errorf("auto-login failed: %w",
				&sdkerrors.ConnectionError{Message: "login request failed", Cause: refusal})
		},
	}
	n := &vmMockNodes{}
	a := &vmMockAgent{}

	deps := buildFallbackDeps(q, n, a, 2)
	h := handlers.HandleCreateVM(deps)

	_, err := h.Handle(context.Background(), fallbackArgs(), mkCtx("fallback-start-redirect"))
	if err == nil {
		t.Fatal("create_vm succeeded, want the redirect refusal")
	}
	if !errors.Is(err, sdkclient.ErrRedirectRefused) {
		t.Errorf("err = %v, want it to carry the redirect refusal", err)
	}
	var cpiErr *cpierrors.Error
	if !errors.As(err, &cpiErr) || cpiErr.OkToRetry() {
		t.Errorf("err = %v, want a permanent CPI error", err)
	}
	if got := startCalls.Load(); got != 1 || len(startNodes) != 1 {
		t.Errorf("start ran %d times on %v, want once on the winner", got, startNodes)
	}
}
