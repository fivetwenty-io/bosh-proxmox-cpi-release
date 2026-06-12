package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// registerStemcellRef appends the stemcell CID for templateVMID to the
// StemcellRefs CSV in the template's description under a per-VMID cluster lock.
// When the CID is already present the write is skipped (idempotent).
//
// All failures are non-fatal: registerStemcellRef is best-effort metadata.
// The template is fully created and frozen before this is called; the CID has
// already been returned to the caller. A failed registration means the ref count
// may be understated, but the conservative-delete rule prevents premature
// destruction when refs are missing.
func registerStemcellRef(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	templateNode string,
	templateVMID int64,
) {
	if deps.PVE == nil {
		return
	}

	pools := deps.PVE.Pools()
	stemcellCID := pve.BuildTemplateStemcellCID(templateVMID)
	vmidInt := int(templateVMID) //nolint:gosec // VMID is bounded by PVE valid range (1–999999999)
	lockOwner := fmt.Sprintf("create_stemcell/ref/%d", templateVMID)

	lockErr := withVMIDLock(ctx, pools, vmidInt, lockOwner, logger, func() error {
		return stemcellRefRMW(ctx, deps, logger, templateNode, templateVMID, stemcellCID,
			func(refs []string) []string {
				// Append only if not already present (idempotent).
				for _, r := range refs {
					if r == stemcellCID {
						return refs
					}
				}
				return append(refs, stemcellCID)
			})
	})
	if lockErr != nil {
		if logger != nil {
			logger.Warn("registerStemcellRef: lock/RMW failed (non-fatal)",
				log.Int64("vmid", templateVMID),
				log.Err(lockErr),
			)
		}
	}
}

// gatedDeregisterAndDestroyRef removes stemcellCID from the StemcellRefs CSV
// in the template's description AND destroys the template VM — all under a
// single per-VMID cluster lock. Keeping destroy inside the lock prevents a
// race where a concurrent registerStemcellRef acquires the lock after the RMW
// but before DeleteQemu, re-appends a CID, and the destroy then lands on a
// template that now carries a live reference.
//
// Return semantics:
//   - (true, nil):  template destroyed successfully (was last ref).
//   - (false, nil): refs remain (other references exist) OR refs are missing,
//     empty, or unparseable (conservative: do NOT destroy).
//   - (false, err): PVE API error; caller propagates as retriable/cloud error.
//
// Conservative rule: if the description is non-JSON, or is JSON but
// stemcell_refs is absent or empty, the ref count is unknown (pre-refs
// template, or create_stemcell crashed before writing refs). The template is
// preserved — never destroyed on unknown state.
func gatedDeregisterAndDestroyRef(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	templateNode string,
	templateVMID int64,
	stemcellCID string,
) (destroyed bool, err error) {
	if deps.PVE == nil {
		return false, nil
	}
	if deps.PVE.QEMU() == nil {
		return false, nil
	}

	pools := deps.PVE.Pools()
	vmidInt := int(templateVMID) //nolint:gosec // VMID is bounded by PVE valid range (1–999999999)
	lockOwner := fmt.Sprintf("delete_stemcell/ref/%d", templateVMID)

	var didDestroy bool
	lockErr := withVMIDLock(ctx, pools, vmidInt, lockOwner, logger, func() error {
		// --- Step 1: read current description under the lock ---
		cfg, cfgErr := deps.PVE.QEMU().Config(ctx, templateNode, int(templateVMID)) //nolint:gosec // VMID is bounded by PVE valid range (1–999999999), int conversion is safe
		if cfgErr != nil {
			if pve.IsNotFound(cfgErr) {
				// Template already gone — treat as destroyed (idempotent).
				didDestroy = true
				return nil
			}
			return cfgErr
		}

		description := ""
		if v, ok := cfg["description"]; ok {
			if s, ok2 := v.(string); ok2 {
				description = s
			}
		}

		prov, jsonOK := parseStemcellProvenanceFromDescription(description)
		if !jsonOK {
			// Description is not JSON at all (non-7.48 operator notes or truly
			// missing provenance). Conservative: do not destroy — we cannot
			// determine the ref count and do not want to raze a live template.
			if logger != nil {
				logger.Warn("delete_stemcell: template description is not JSON (conservative: not destroying)",
					log.Int64("vmid", templateVMID),
					log.String("node", templateNode),
				)
			}
			didDestroy = false
			return nil
		}

		// --- Step 2: compute updated refs ---
		refs := ParseStemcellRefs(prov.StemcellRefs)
		// Refs absent or empty in parseable JSON: ref count unknown
		// (pre-refs template or create_stemcell crashed before writing refs).
		// Conservative: preserve the template.
		if len(refs) == 0 {
			if logger != nil {
				logger.Warn("delete_stemcell: stemcell_refs absent/empty (conservative: not destroying)",
					log.Int64("vmid", templateVMID),
					log.String("node", templateNode),
				)
			}
			didDestroy = false
			return nil
		}
		// Remove this CID from the list.
		updated := make([]string, 0, len(refs))
		for _, r := range refs {
			if r != stemcellCID {
				updated = append(updated, r)
			}
		}

		if len(updated) > 0 {
			// Other refs remain — write updated list and do NOT destroy.
			prov.StemcellRefs = FormatStemcellRefs(updated)
			if writeErr := writeStemcellProvenanceDescription(ctx, deps, templateNode, templateVMID, prov); writeErr != nil {
				return writeErr
			}
			didDestroy = false
			return nil
		}

		// All refs gone — clear the field, then destroy below.
		prov.StemcellRefs = ""
		if writeErr := writeStemcellProvenanceDescription(ctx, deps, templateNode, templateVMID, prov); writeErr != nil {
			return writeErr
		}

		// --- Step 3: destroy the template VM while the lock is still held ---
		// Holding the lock here prevents a concurrent registerStemcellRef from
		// appending a new CID between the RMW and the DeleteQemu call.
		if destroyErr := destroyTemplateVM(ctx, deps, templateNode, templateVMID, stemcellCID); destroyErr != nil {
			return destroyErr
		}
		didDestroy = true
		return nil
	})

	if lockErr != nil {
		return false, lockErr
	}
	return didDestroy, nil
}

// stemcellRefRMW reads the description for templateVMID, applies fn to the
// current refs slice, and writes the modified description back. Called under
// a per-VMID cluster lock by registerStemcellRef. fn receives the parsed
// refs (never nil) and must return the desired new refs slice.
func stemcellRefRMW(
	ctx context.Context,
	deps Deps,
	logger *log.Logger,
	templateNode string,
	templateVMID int64,
	stemcellCID string,
	fn func(refs []string) []string,
) error {
	if deps.PVE == nil || deps.PVE.QEMU() == nil {
		return nil
	}
	cfg, cfgErr := deps.PVE.QEMU().Config(ctx, templateNode, int(templateVMID)) //nolint:gosec // VMID is bounded by PVE valid range (1–999999999), int conversion is safe
	if cfgErr != nil {
		if pve.IsNotFound(cfgErr) {
			// Template already gone; nothing to update.
			return nil
		}
		return cfgErr
	}

	description := ""
	if v, ok := cfg["description"]; ok {
		if s, ok2 := v.(string); ok2 {
			description = s
		}
	}

	// Parse existing provenance (or start from zero-value on first call for
	// pre-7.48 templates that have no description JSON).
	prov, _ := parseStemcellProvenanceFromDescription(description)
	refs := ParseStemcellRefs(prov.StemcellRefs)
	updated := fn(refs)

	// Guard: ensure at minimum the current CID is present. Handles pre-7.48
	// templates created before refs were written at creation time.
	if len(updated) == 0 {
		updated = []string{stemcellCID}
	}

	prov.StemcellRefs = FormatStemcellRefs(updated)
	newDesc, marshalErr := marshalStemcellProvenance(prov)
	if marshalErr != nil {
		return marshalErr
	}

	vmCIDStr := strconv.FormatInt(templateVMID, 10)
	if logger != nil {
		logger.Debug("stemcellRefRMW: writing updated refs",
			log.Int64("vmid", templateVMID),
			log.String("refs", prov.StemcellRefs),
		)
	}
	return deps.PVE.Nodes().UpdateQemuConfig(ctx, templateNode, vmCIDStr,
		&sdknodes.UpdateQemuConfigParams{Description: &newDesc})
}

// writeStemcellProvenanceDescription marshals prov and writes it as the
// template's description. Callers must hold the per-VMID cluster lock.
func writeStemcellProvenanceDescription(
	ctx context.Context,
	deps Deps,
	templateNode string,
	templateVMID int64,
	prov stemcellProvenance,
) error {
	newDesc, marshalErr := marshalStemcellProvenance(prov)
	if marshalErr != nil {
		return marshalErr
	}
	vmCIDStr := strconv.FormatInt(templateVMID, 10)
	return deps.PVE.Nodes().UpdateQemuConfig(ctx, templateNode, vmCIDStr,
		&sdknodes.UpdateQemuConfigParams{Description: &newDesc})
}

// marshalStemcellProvenance serializes a stemcellProvenance to JSON for writing
// to the template description. Returns an error only if json.Marshal fails
// (which cannot happen for a struct with string-only fields, but is surfaced for
// correctness).
func marshalStemcellProvenance(p stemcellProvenance) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
