// Package version holds build-time metadata injected via -ldflags.
package version

import "fmt"

// Version is the semantic version string, defaulting to "dev" when not set via ldflags.
var Version = "dev"

// Commit is the short git commit SHA, defaulting to "unknown" when not set via ldflags.
var Commit = "unknown"

// BuildDate is the RFC3339 UTC build timestamp, defaulting to "unknown" when not set via ldflags.
var BuildDate = "unknown"

// String returns the full version string including version, commit, and build date.
func String() string {
	return fmt.Sprintf("bosh-pve-cpi %s (%s, built %s)", Version, Commit, BuildDate)
}

// Short returns the Version string only.
func Short() string {
	return Version
}
