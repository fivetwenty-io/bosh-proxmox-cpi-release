package pvetest_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/pvetest"
)

func TestRoutes_DefaultLoginAnswersThePasswordClient(t *testing.T) {
	t.Parallel()
	ca := pvetest.NewCA(t)
	s := pvetest.NewLive(t, ca.NewLeaf(t, pvetest.LeafOptions{}), nil)
	verify := true
	cfg := &config.CPIConfig{
		Host:         s.Host,
		Port:         s.Port,
		User:         "root",
		Realm:        "pam",
		Password:     "secret",
		VerifySSL:    &verify,
		PVECACertPEM: string(ca.CertPEM()),
	}
	c, err := pve.NewClientWithTracer(cfg, log.NewNopLogger(), nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err = c.Nodes().ListNodes(context.Background()); err != nil {
		t.Fatalf("ListNodes after a password login: %v", err)
	}

	seen := s.Seen()
	if len(seen) < 2 || seen[0].Method != http.MethodPost || seen[0].Path != pvetest.APIPrefix+"/access/ticket" {
		t.Fatalf("Seen = %+v, want the login first", seen)
	}
	if !strings.Contains(string(seen[0].Body), "username=root") {
		t.Fatalf("login body = %q, want the user name", seen[0].Body)
	}
	if cookie := seen[len(seen)-1].Header.Get("Cookie"); !strings.Contains(cookie, "PVEAuthCookie=") {
		t.Fatalf("the request after the login carried Cookie %q, want the ticket", cookie)
	}
}
