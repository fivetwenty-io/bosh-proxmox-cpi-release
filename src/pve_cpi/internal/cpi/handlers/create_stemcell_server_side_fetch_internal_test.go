package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

func ssfCloudProps() stemcellCloudProps {
	return stemcellCloudProps{
		Name:           "bosh-stemcell",
		Version:        "1.2.3",
		ImageURL:       "https://example.test/stemcell.qcow2",
		ExpectedSHA256: strings.Repeat("ab", 32),
	}
}

// TestServerSideFetchEligibleHappyPath: https URL, sha256, no credentials.
func TestServerSideFetchEligibleHappyPath(t *testing.T) {
	t.Parallel()
	deps := Deps{Config: &config.CPIConfig{}}
	if !serverSideFetchEligible(deps, ssfCloudProps()) {
		t.Fatal("expected eligible: https URL with sha256 and no credentials")
	}
}

// TestServerSideFetchEligibleRequiresSHA256: no digest means the download-url
// path has no content identity to build on.
func TestServerSideFetchEligibleRequiresSHA256(t *testing.T) {
	t.Parallel()
	deps := Deps{Config: &config.CPIConfig{}}
	cp := ssfCloudProps()
	cp.ExpectedSHA256 = ""
	if serverSideFetchEligible(deps, cp) {
		t.Fatal("expected ineligible without sha256")
	}
	cp.ExpectedSHA256 = "not-hex"
	if serverSideFetchEligible(deps, cp) {
		t.Fatal("expected ineligible with malformed sha256")
	}
}

// TestServerSideFetchEligibleRequiresHTTPS: PVE's download-url endpoint
// cannot speak the CPI's other fetch schemes.
func TestServerSideFetchEligibleRequiresHTTPS(t *testing.T) {
	t.Parallel()
	deps := Deps{Config: &config.CPIConfig{}}
	for _, url := range []string{
		"s3://bucket/stemcell.qcow2",
		"oci://registry.test/repo:tag",
		"bosh+blobstore://blob/id",
		"http://example.test/stemcell.qcow2",
	} {
		cp := ssfCloudProps()
		cp.ImageURL = url
		if serverSideFetchEligible(deps, cp) {
			t.Fatalf("expected ineligible for %q", url)
		}
	}
}

// TestServerSideFetchEligibleRejectsPerStemcellAuth: server-side download
// would silently drop the credentials.
func TestServerSideFetchEligibleRejectsPerStemcellAuth(t *testing.T) {
	t.Parallel()
	deps := Deps{Config: &config.CPIConfig{}}
	cp := ssfCloudProps()
	cp.ImageURLAuth = json.RawMessage(`{"type":"basic","username":"u","password":"p"}`)
	if serverSideFetchEligible(deps, cp) {
		t.Fatal("expected ineligible with per-stemcell auth")
	}
}

// TestServerSideFetchEligibleRejectsConfigDefaultAuth: a longest-prefix
// credential default matching the URL also disqualifies the server-side path.
func TestServerSideFetchEligibleRejectsConfigDefaultAuth(t *testing.T) {
	t.Parallel()
	deps := Deps{Config: &config.CPIConfig{
		FetchCredentialDefaults: []config.FetchCredentialDefault{
			{
				URLPrefix: "https://example.test/",
				Auth:      json.RawMessage(`{"type":"bearer","token":"tok"}`),
			},
		},
	}}
	if serverSideFetchEligible(deps, ssfCloudProps()) {
		t.Fatal("expected ineligible when a config credential default matches")
	}
	// A default for a different prefix leaves the URL unauthenticated.
	deps.Config.FetchCredentialDefaults[0].URLPrefix = "https://other.test/"
	if !serverSideFetchEligible(deps, ssfCloudProps()) {
		t.Fatal("expected eligible when no credential default matches")
	}
}

// TestServerSideFetchEligibleMalformedAuth: a malformed auth payload defers
// to the CPI-side fetch, which reports the error with full context.
func TestServerSideFetchEligibleMalformedAuth(t *testing.T) {
	t.Parallel()
	deps := Deps{Config: &config.CPIConfig{}}
	cp := ssfCloudProps()
	cp.ImageURLAuth = json.RawMessage(`{"type":"unknown-kind"}`)
	if serverSideFetchEligible(deps, cp) {
		t.Fatal("expected ineligible on malformed auth payload")
	}
}
