package allocationjournal

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// VMRetentionEvidence is embedded at the top level of a durable verification.
// It proves VM absence and disposition of surviving allocation-owned artifacts,
// without claiming absence of the whole allocation.
type VMRetentionEvidence struct {
	VMID              int      `json:"vmid"`
	RetainedArtifacts []Target `json:"retained_artifacts"`
}

func validateVMRetention(r Record) error {
	if r.Kind != "vm" {
		return fmt.Errorf("%w: retained VM state requires VM allocation", ErrCorrupt)
	}
	var proof *Verification
	for i := len(r.Verifications) - 1; i >= 0; i-- {
		v := &r.Verifications[i]
		if v.VMAbsenceVerified && v.ArtifactDispositionVerified && !v.AbsenceVerified && v.EvidenceJSON != "" {
			proof = v
			break
		}
	}
	if proof == nil {
		return fmt.Errorf("%w: durable VM absence and retention proof required", ErrCorrupt)
	}
	var evidence VMRetentionEvidence
	if err := json.Unmarshal([]byte(proof.EvidenceJSON), &evidence); err != nil || evidence.VMID <= 0 || len(evidence.RetainedArtifacts) == 0 {
		return fmt.Errorf("%w: exact retained artifact evidence required", ErrCorrupt)
	}
	seen := map[Target]bool{}
	for _, target := range evidence.RetainedArtifacts {
		if target.External || !nonblank(target.Node) || !nonblank(target.Storage) || !nonblank(target.Backing) || !strings.HasPrefix(target.IntendedVolume, target.Storage+":") || len(target.IntendedVolume) <= len(target.Storage)+1 || seen[target] {
			return fmt.Errorf("%w: invalid retained artifact target", ErrCorrupt)
		}
		seen[target] = true
		matched := false
		for stepIndex := range r.Steps {
			step := r.Steps[stepIndex]
			if step.Attempt == r.ActiveAttempt() && !step.Target.External && step.State == Observed && step.Target.Node == target.Node && step.Target.Storage == target.Storage && step.Target.Backing == target.Backing && (step.Target.IntendedVolume == target.IntendedVolume || slices.Contains(step.VolIDs, target.IntendedVolume)) {
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("%w: retained artifact is outside observed owned targets", ErrCorrupt)
		}
	}
	matchedVM := false
	for stepIndex := range r.Steps {
		step := r.Steps[stepIndex]
		if step.Attempt == r.ActiveAttempt() && !step.Target.External && step.Target.VMID == evidence.VMID {
			matchedVM = true
		}
	}
	if !matchedVM {
		return fmt.Errorf("%w: retention proof VM identity mismatch", ErrCorrupt)
	}
	return nil
}
