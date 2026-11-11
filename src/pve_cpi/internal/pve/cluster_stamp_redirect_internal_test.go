package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// stampRedirectSecret stands in for the redirect's Location, which must never
// reach the error text.
const stampRedirectSecret = "SECRET-REDIRECT-TARGET"

// stampRefusal is a refused redirect as the SDK's client returns it from a
// certificates/info read.
func stampRefusal() error {
	return fmt.Errorf("request failed after 1 attempt(s), not retried: %w",
		&sdkclient.RedirectError{Status: http.StatusFound, From: "https://" + stampRedirectSecret + ":8006/api2/json"})
}

// A cluster root read that met a refused redirect keeps the refusal in its
// error chain and fails permanently, on every branch that can end the read.
// The node failures still carry only the fixed reasons, so the error never
// echoes the redirect.
func TestReadClusterStamp_RefusedRedirectIsPermanent(t *testing.T) {
	t.Parallel()
	refuseAll := func(string) (*sdknodes.ListCertificatesInfoResponse, error) { return nil, stampRefusal() }
	cases := map[string]*csClient{
		"every node redirects": {
			cluster: &lgCluster{nodes: []string{"pve1", "pve2"}},
			nodes:   &csNodes{answer: refuseAll},
		},
		"one node redirects and the other drops the connection": {
			cluster: &lgCluster{nodes: []string{"pve1", "pve2"}},
			nodes: &csNodes{answer: func(node string) (*sdknodes.ListCertificatesInfoResponse, error) {
				if node == "pve1" {
					return nil, stampRefusal()
				}
				return nil, errors.New("read tcp: connection reset by peer")
			}},
		},
		"the preferred node redirects and the membership read fails": {
			cluster: &lgCluster{configNodesFn: func() (*sdkcluster.ListConfigNodesResponse, error) {
				return nil, errors.New("dial tcp: connection reset")
			}},
			nodes: &csNodes{answer: refuseAll},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ReadClusterStamp(lgCtx(), c, "pve1")
			if err == nil {
				t.Fatal("want an error")
			}
			if !IsRedirectRefused(err) {
				t.Fatalf("the refusal left the error chain: %v", err)
			}
			if !cpierrors.IsType(err, cpierrors.TypeCloud) {
				t.Fatalf("err = %v, want a permanent cloud error", err)
			}
			if strings.Contains(err.Error(), stampRedirectSecret) {
				t.Fatalf("error carries the redirect's target: %s", err)
			}
		})
	}
}

// A node failure that is not a refused redirect keeps the read retriable and
// adds no refusal to the chain.
func TestReadClusterStamp_TransportFailureStaysRetriable(t *testing.T) {
	t.Parallel()
	c := &csClient{
		cluster: &lgCluster{nodes: []string{"pve1", "pve2"}},
		nodes: &csNodes{answer: func(string) (*sdknodes.ListCertificatesInfoResponse, error) {
			return nil, errors.New("read tcp: connection reset by peer")
		}},
	}
	_, err := ReadClusterStamp(lgCtx(), c, "pve1")
	if err == nil || IsRedirectRefused(err) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v, want a retriable error with no refusal", err)
	}
}

// The ownership check wraps a failed stamp read as retriable, and the
// Director still gets ok_to_retry false, because the refusal survives in the
// chain for the dispatcher to find.
func TestReadClusterStamp_RefusedRedirectReachesTheDirectorAsPermanent(t *testing.T) {
	t.Parallel()
	c := &csClient{
		cluster: &lgCluster{nodes: []string{"pve1", "pve2"}},
		nodes: &csNodes{answer: func(string) (*sdknodes.ListCertificatesInfoResponse, error) {
			return nil, stampRefusal()
		}},
	}
	d := cpi.NewDispatcherWithOptions(log.NewNopLogger(), cpi.WithTransientClassifier(IsTransientTransport))
	if err := d.Register("delete_vm", cpi.HandlerFunc(
		func(ctx context.Context, _ []json.RawMessage, _ jsonrpc.Context) (any, error) {
			_, err := ReadClusterStamp(ctx, c, "pve1")
			return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				"delete_vm: the cluster identity could not be read")
		})); err != nil {
		t.Fatal(err)
	}
	resp := d.Handle(lgCtx(), &jsonrpc.Request{
		Method:     "delete_vm",
		Arguments:  []json.RawMessage{},
		Context:    jsonrpc.Context{RequestID: "stamp-redirect"},
		APIVersion: 2,
	})
	if resp == nil || resp.Error == nil {
		t.Fatal("want an error response")
	}
	if resp.Error.OkToRetry {
		t.Fatalf("a refused redirect reached the Director with ok_to_retry true: %+v", resp.Error)
	}
	if strings.Contains(resp.Error.Message, stampRedirectSecret) {
		t.Fatalf("the wire message carries the redirect's target: %s", resp.Error.Message)
	}
}
