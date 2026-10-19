package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/version"
)

// maxVersionConfigBytes bounds how much of the job config `cpi --version`
// reads. A rendered cpi.json is a few kilobytes; a file past this size is not
// one, and the version falls back to the package version.
const maxVersionConfigBytes = 4 << 20

// releaseVersionLabel renders the release version for the startup log, which
// reads "unknown" when the config names no plausible release.
func releaseVersionLabel(release string) string {
	if rel := version.ReleaseVersion(release); rel != "" {
		return rel
	}
	return "unknown"
}

// releaseVersionFromConfig returns the release_version the job template
// rendered into the CPI config at path, or "" when the config carries none.
// It reads only that key and validates nothing else, so `cpi --version`
// answers even when the rest of the config would fail to load.
func releaseVersionFromConfig(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- path is an operator-supplied CLI arg; trust boundary
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxVersionConfigBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if len(raw) > maxVersionConfigBytes {
		return "", fmt.Errorf("%s is larger than %d bytes, so it is not a CPI config", path, maxVersionConfigBytes)
	}
	var cfg struct {
		ReleaseVersion string `json:"release_version"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg.ReleaseVersion, nil
}
