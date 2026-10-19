package version

import "fmt"

// Version is the semantic version string, defaulting to "dev" when not set via ldflags.
// A BOSH package build sets it to the package fingerprint.
var Version = "dev"

// Commit is the short git commit SHA, defaulting to "unknown" when not set via ldflags.
var Commit = "unknown"

// BuildDate is the RFC3339 UTC build timestamp, defaulting to "unknown" when not set via ldflags.
var BuildDate = "unknown"

// maxReleaseVersionLen bounds a release version we print. BOSH release
// versions are short; anything longer did not come from a release.
const maxReleaseVersionLen = 64

// String returns the full version string including version, commit, and build date.
func String() string {
	return fmt.Sprintf("bosh-proxmox-cpi %s (%s, built %s)", Version, Commit, BuildDate)
}

// StringWithRelease returns the version string that names the BOSH release:
// "bosh-proxmox-cpi <release> (package <Version>, built <BuildDate>)". The
// release version comes from the job config, so it is checked with
// ReleaseVersion first; when it is empty or not a plausible release version,
// the result is String(), the output of a build that knows no release.
func StringWithRelease(release string) string {
	rel := ReleaseVersion(release)
	if rel == "" {
		return String()
	}
	return fmt.Sprintf("bosh-proxmox-cpi %s (package %s, built %s)", rel, Version, BuildDate)
}

// ReleaseVersion returns release when it looks like a BOSH release version,
// and "" otherwise. A plausible version is 1 to 64 characters from
// [A-Za-z0-9._+-]. A value outside that shape is dropped rather than cleaned
// up, so a garbled config never prints as a release version.
func ReleaseVersion(release string) string {
	if release == "" || len(release) > maxReleaseVersionLen {
		return ""
	}
	for i := 0; i < len(release); i++ {
		c := release[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '+', c == '-':
		default:
			return ""
		}
	}
	return release
}

// Short returns the Version string only.
func Short() string {
	return Version
}
