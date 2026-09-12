package config

// CaptureISOStoragePolicy records the operator/default selection before runtime
// resolution changes ISOStorage. ApplyDefaults captures after its legacy ISO default. It is idempotent, including for empty
// selections. Call only during initialization or on a request-owned copy.
func (c *CPIConfig) CaptureISOStoragePolicy() {
	if c == nil || c.isoStoragePolicyCaptured {
		return
	}
	c.isoStorageOriginal = c.ISOStorage
	c.isoStoragePolicyCaptured = true
}

// OriginalISOStorage returns the original operator pin/sentinel, independently
// of the effective runtime pool in ISOStorage. Uncaptured config literals fall
// back to ISOStorage. The accessor does not mutate shared configuration.
//
// JSON snapshots omit private bookkeeping: read this accessor on the original
// request config before serializing a planner/intent snapshot. A JSON round trip
// of a resolved config cannot reconstruct whether its effective pool was a pin.
func (c *CPIConfig) OriginalISOStorage() string {
	if c == nil {
		return ""
	}
	if c.isoStoragePolicyCaptured {
		return c.isoStorageOriginal
	}
	return c.ISOStorage
}

// SetISOStoragePolicy replaces original and effective values for an explicit
// per-request iso_storage override. It must only receive a request-owned config.
func (c *CPIConfig) SetISOStoragePolicy(value string) {
	c.ISOStorage = value
	c.isoStorageOriginal = value
	c.isoStoragePolicyCaptured = true
}

// WithResolvedISOStorage returns a storage-policy-isolated request copy with a
// validated concrete ISO target. It preserves original policy and all private
// config bookkeeping, unlike a JSON copy. The existing agent.NewAgent factory
// can consume this copy without changing shared Deps.Config or its boot agent.
// Other legacy config maps retain CloneStoragePlacement's read-only contract.
func (c *CPIConfig) WithResolvedISOStorage(value string) CPIConfig {
	result := c.CloneStoragePlacement()
	result.CaptureISOStoragePolicy()
	result.ISOStorage = value
	return result
}
