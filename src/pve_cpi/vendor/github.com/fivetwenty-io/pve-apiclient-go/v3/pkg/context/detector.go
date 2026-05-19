package context

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	// Scoring weights for detection checks.
	pveDirectoryScore      = 3 // High confidence indicator
	pveshBinaryScore       = 2 // Medium confidence indicator
	pveManagerPackageScore = 3 // High confidence indicator
	nodeRegistrationScore  = 3 // High confidence indicator

	// Scoring thresholds for execution mode determination.
	localModeThreshold   = 6 // 6+ points indicates local execution
	unknownModeThreshold = 3 // 3-5 points indicates unknown/inconclusive
)

// ExecutionMode indicates where the client is running.
type ExecutionMode int

const (
	// ExecutionModeRemote indicates the client is running remotely (not on a PVE node).
	ExecutionModeRemote ExecutionMode = iota

	// ExecutionModeLocal indicates the client is running on a PVE node.
	ExecutionModeLocal

	// ExecutionModeUnknown indicates detection failed or is inconclusive.
	ExecutionModeUnknown
)

// String returns a string representation of the ExecutionMode.
func (m ExecutionMode) String() string {
	switch m {
	case ExecutionModeRemote:
		return "remote"
	case ExecutionModeLocal:
		return "local"
	case ExecutionModeUnknown:
		return "unknown"
	default:
		return "invalid"
	}
}

var (
	// ErrNotOnPVENode indicates the client is not running on a PVE node.
	ErrNotOnPVENode = errors.New("not running on PVE node")

	// ErrDetectionFailed indicates context detection failed.
	ErrDetectionFailed = errors.New("execution context detection failed")
)

// Detector checks the execution environment to determine if running on a PVE node.
type Detector struct {
	// Configurable paths for testing
	pvePath      string
	pveshPath    string
	dpkgPath     string
	hostnameFunc func() (string, error)
}

// DetectorOption is a functional option for configuring a Detector.
type DetectorOption func(*Detector)

// WithPVEPath sets a custom PVE directory path (for testing).
func WithPVEPath(path string) DetectorOption {
	return func(d *Detector) {
		d.pvePath = path
	}
}

// WithPVESHPath sets a custom pvesh binary path (for testing).
func WithPVESHPath(path string) DetectorOption {
	return func(d *Detector) {
		d.pveshPath = path
	}
}

// WithDpkgPath sets a custom dpkg binary path (for testing).
func WithDpkgPath(path string) DetectorOption {
	return func(d *Detector) {
		d.dpkgPath = path
	}
}

// WithHostnameFunc sets a custom hostname function (for testing).
func WithHostnameFunc(fn func() (string, error)) DetectorOption {
	return func(d *Detector) {
		d.hostnameFunc = fn
	}
}

// NewDetector creates a new Detector with default paths.
func NewDetector(opts ...DetectorOption) *Detector {
	detector := &Detector{
		pvePath:      "/etc/pve",
		pveshPath:    "/usr/bin/pvesh",
		dpkgPath:     "/usr/bin/dpkg",
		hostnameFunc: os.Hostname,
	}

	for _, opt := range opts {
		opt(detector)
	}

	return detector
}

// DetectMode determines the execution context using multiple checks.
// It returns ExecutionModeLocal if running on a PVE node,
// ExecutionModeRemote if running remotely, or ExecutionModeUnknown if inconclusive.
func (d *Detector) DetectMode() ExecutionMode {
	score := 0

	// Check 1: /etc/pve directory exists and is accessible (HIGH confidence)
	if d.checkPVEDirectory() {
		score += pveDirectoryScore
	}

	// Check 2: pvesh binary exists (MEDIUM confidence)
	if d.checkPVESH() {
		score += pveshBinaryScore
	}

	// Check 3: pve-manager package installed (HIGH confidence)
	if d.checkPVEManager() {
		score += pveManagerPackageScore
	}

	// Check 4: hostname matches registered node (HIGH confidence)
	if d.checkNodeRegistration() {
		score += nodeRegistrationScore
	}

	// Scoring thresholds:
	// 0-2: Remote (low/no PVE indicators)
	// 3-5: Unknown (some indicators, inconclusive)
	// 6+: Local (multiple strong indicators)
	if score >= localModeThreshold {
		return ExecutionModeLocal
	} else if score >= unknownModeThreshold {
		return ExecutionModeUnknown
	}

	return ExecutionModeRemote
}

// IsLocal returns true if the detector determines we're running on a PVE node.
func (d *Detector) IsLocal() bool {
	return d.DetectMode() == ExecutionModeLocal
}

// IsRemote returns true if the detector determines we're running remotely.
func (d *Detector) IsRemote() bool {
	return d.DetectMode() == ExecutionModeRemote
}

// GetNodeName returns the local PVE node name if running on a PVE node.
// Returns an error if not running locally or if hostname cannot be determined.
func (d *Detector) GetNodeName() (string, error) {
	if d.DetectMode() != ExecutionModeLocal {
		return "", ErrNotOnPVENode
	}

	hostname, err := d.hostnameFunc()
	if err != nil {
		return "", err
	}

	return hostname, nil
}

// checkPVEDirectory verifies that /etc/pve exists and is accessible.
// This is the PVE cluster configuration filesystem.
func (d *Detector) checkPVEDirectory() bool {
	stat, err := os.Stat(d.pvePath)
	if err != nil {
		return false
	}

	return stat.IsDir()
}

// checkPVESH verifies that the pvesh binary exists.
// pvesh is the PVE shell utility, present on all PVE nodes.
func (d *Detector) checkPVESH() bool {
	_, err := os.Stat(d.pveshPath)

	return err == nil
}

// checkPVEManager verifies that the pve-manager package is installed.
// This is the core PVE management package.
func (d *Detector) checkPVEManager() bool {
	if d.dpkgPath == "" {
		return false
	}

	// Check if dpkg exists first
	_, err := os.Stat(d.dpkgPath)
	if err != nil {
		return false
	}

	// #nosec G204 -- dpkgPath is validated via os.Stat above and controlled through
	// DetectorOption configuration. In production, it defaults to the constant "/usr/bin/dpkg".
	// The package argument "pve-manager" is a hardcoded constant, not user input.
	cmd := exec.CommandContext(context.Background(), d.dpkgPath, "-s", "pve-manager")
	err = cmd.Run()

	return err == nil
}

// checkNodeRegistration verifies that the hostname matches a registered PVE node.
// Checks if /etc/pve/nodes/<hostname> directory exists.
func (d *Detector) checkNodeRegistration() bool {
	hostname, err := d.hostnameFunc()
	if err != nil {
		return false
	}

	nodePath := filepath.Join(d.pvePath, "nodes", hostname)
	stat, err := os.Stat(nodePath)

	return err == nil && stat.IsDir()
}

// Detect is a convenience function that creates a detector and returns the mode.
func Detect() ExecutionMode {
	return NewDetector().DetectMode()
}

// IsRunningOnPVENode is a convenience function that returns true if running on a PVE node.
func IsRunningOnPVENode() bool {
	return NewDetector().IsLocal()
}
