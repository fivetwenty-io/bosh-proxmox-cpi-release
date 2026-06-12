package handlers

import "strings"

// mergeHotplugToken adds or removes token from the comma-separated PVE hotplug
// string current. Rules:
//   - add=true: token appended when absent; no-op when already present (idempotent).
//   - add=false: token removed when present; no-op when absent.
//   - Empty tokens produced by stripping are discarded; leading/trailing commas
//     are never emitted.
//   - Order of existing tokens is preserved.
//   - current="" is handled for both add and remove.
//   - Matching is case-sensitive: PVE hotplug tokens are lowercase by
//     convention ("disk", "network", "usb", "cpu", "memory"), and PVE itself
//     rejects uppercase variants, so no case folding is performed.
func mergeHotplugToken(current, token string, add bool) string {
	if token == "" {
		return current
	}
	parts := splitHotplug(current)

	// Locate the token in the existing list.
	found := -1
	for i, p := range parts {
		if p == token {
			found = i
			break
		}
	}

	if add {
		if found >= 0 {
			// Already present — idempotent, return as-is.
			return joinHotplug(parts)
		}
		parts = append(parts, token)
		return joinHotplug(parts)
	}

	// Remove: token absent → no-op.
	if found < 0 {
		return joinHotplug(parts)
	}
	parts = append(parts[:found], parts[found+1:]...)
	return joinHotplug(parts)
}

// splitHotplug splits a PVE hotplug comma-string, dropping empty segments.
func splitHotplug(s string) []string {
	if s == "" {
		return nil
	}
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// joinHotplug joins tokens back into a PVE hotplug comma-string.
func joinHotplug(parts []string) string {
	return strings.Join(parts, ",")
}
