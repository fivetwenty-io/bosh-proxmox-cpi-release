// Package context provides execution context detection for Proxmox VE environments.
//
// This package helps determine whether the client is running on a Proxmox VE node
// (local execution) or remotely (e.g., from a developer workstation or CI system).
//
// # Execution Modes
//
// The package defines three execution modes:
//
//   - ExecutionModeLocal: Running on a PVE node with direct access to cluster config
//   - ExecutionModeRemote: Running remotely, requiring API authentication
//   - ExecutionModeUnknown: Detection inconclusive (treated as remote for safety)
//
// # Detection Strategy
//
// The Detector uses multiple checks with confidence scoring:
//
//   - PVE directory check (/etc/pve exists): HIGH confidence (+3 points)
//   - PVE shell check (pvesh binary exists): MEDIUM confidence (+2 points)
//   - Package check (pve-manager installed): HIGH confidence (+3 points)
//   - Node registration (/etc/pve/nodes/<hostname>): HIGH confidence (+3 points)
//
// Scoring thresholds:
//
//   - 0-2 points: Remote
//   - 3-5 points: Unknown (inconclusive)
//   - 6+ points: Local
//
// # Usage
//
// Simple detection:
//
//	if context.IsRunningOnPVENode() {
//	    fmt.Println("Running on PVE node")
//	} else {
//	    fmt.Println("Running remotely")
//	}
//
// Detailed detection:
//
//	detector := context.NewDetector()
//	mode := detector.DetectMode()
//
//	switch mode {
//	case context.ExecutionModeLocal:
//	    nodeName, _ := detector.GetNodeName()
//	    fmt.Printf("Running on PVE node: %s\n", nodeName)
//	case context.ExecutionModeRemote:
//	    fmt.Println("Running remotely")
//	case context.ExecutionModeUnknown:
//	    fmt.Println("Cannot determine execution mode")
//	}
//
// # Integration with Client
//
// The client package uses this for automatic context detection:
//
//	client, _ := pve.NewClient(pve.Options{
//	    Host:           "localhost", // or node name
//	    AutoDetectMode: true,        // Enable auto-detection
//	})
//
// When auto-detection is enabled, the client can optimize behavior:
//   - Local mode: Can read tickets directly from /etc/pve/priv
//   - Remote mode: Must use API authentication
//
// # Testing
//
// The Detector supports dependency injection for testing:
//
//	detector := context.NewDetector(
//	    context.WithPVEPath("/tmp/mock-pve"),
//	    context.WithHostnameFunc(func() (string, error) {
//	        return "test-node", nil
//	    }),
//	)
package context
