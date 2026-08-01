package config

// Cross-package test seams for the context-override field registry. The
// handlers package's requestOverrideCacheKey test iterates the authoritative
// field list to assert every overridable field perturbs the cache key, and
// the in-package sync test asserts slice ⇔ map agreement; both need read
// access to the unexported registry. Copies are returned so no test can
// mutate the registry. Never referenced from production code paths.

// ContextOverrideFieldOrderForTest returns a copy of
// contextOverrideFieldOrder.
func ContextOverrideFieldOrderForTest() []string {
	out := make([]string, len(contextOverrideFieldOrder))
	copy(out, contextOverrideFieldOrder)
	return out
}

// ContextOverrideFieldKeysForTest returns the key set of
// contextOverrideFields.
func ContextOverrideFieldKeysForTest() []string {
	keys := make([]string, 0, len(contextOverrideFields))
	for k := range contextOverrideFields {
		keys = append(keys, k)
	}
	return keys
}
