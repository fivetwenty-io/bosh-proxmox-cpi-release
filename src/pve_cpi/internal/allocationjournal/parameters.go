package allocationjournal

import (
	"bytes"
	"encoding/json"
	"fmt"
)

const maxMutationParametersBytes = 64 << 10

// MutationParameters canonicalizes concrete infrastructure identities. Only the
// documented route, HA rule and volume-birth absence fields are accepted; callers must never supply
// request bodies or credentials. Parameters are immutable from the planned step.
func MutationParameters(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	canonical, err := canonicalEvidence(raw)
	if err != nil {
		return nil, err
	}
	if err := validateMutationParameters(canonical); err != nil {
		return nil, err
	}
	return canonical, nil
}

func validateMutationParameters(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > maxMutationParametersBytes {
		return fmt.Errorf("%w: mutation parameters exceed bound", ErrCorrupt)
	}
	canonical, err := canonicalEvidence(raw)
	if err != nil || !bytes.Equal(canonical, raw) {
		return fmt.Errorf("%w: mutation parameters must be canonical object JSON", ErrCorrupt)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return ErrCorrupt
	}
	allowed := map[string]bool{"version": true, "kind": true, "vnet": true, "subnet": true, "cidr": true, "rule": true, "type": true, "resources": true, "nodes": true, "strict": true, "affinity": true, "disable": true, "comment": true, "original_resources": true, "desired_resources": true, "digest": true, "absence_verified": true, "source_allocation": true, "source_step": true}
	var version int
	var kind string
	if json.Unmarshal(fields["version"], &version) != nil || version != 1 || json.Unmarshal(fields["kind"], &kind) != nil || !nonblank(kind) {
		return fmt.Errorf("%w: mutation parameters require version and kind", ErrCorrupt)
	}
	if kind == "ephemeral_birth" || kind == "persistent_birth" {
		var absent bool
		if len(fields) != 3 || json.Unmarshal(fields["absence_verified"], &absent) != nil || !absent {
			return fmt.Errorf("%w: volume birth requires exact absence observation", ErrCorrupt)
		}
	} else if _, ok := fields["absence_verified"]; ok {
		return fmt.Errorf("%w: absence observation only valid for volume birth", ErrCorrupt)
	}
	for key, value := range fields {
		if !allowed[key] {
			return fmt.Errorf("%w: unsupported mutation parameter field", ErrCorrupt)
		}
		var scalar any
		if json.Unmarshal(value, &scalar) != nil {
			return ErrCorrupt
		}
		switch scalar.(type) {
		case string, bool, float64:
		default:
			return fmt.Errorf("%w: mutation parameter fields must be scalar", ErrCorrupt)
		}
	}
	return nil
}
