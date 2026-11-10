package endpointcache

// deriveKeys builds the key for cfg and the HMAC key for its file at the
// current file version. See deriveKeysAt. Only tests call it, because the
// store derives its keys for the file kind it opens.
func deriveKeys(cfg KeyConfig, dirSecret keyMaterial, credential string) (Key, keyMaterial, error) {
	return deriveKeysAt(cfg, dirSecret, credential, fileVersion)
}

// deriveKeysAt builds the keys of an endpoint cache file of the given schema
// version, so a test can show that a schema bump changes the HMAC key. Only
// tests call it.
func deriveKeysAt(cfg KeyConfig, dirSecret keyMaterial, credential string, version int) (Key, keyMaterial, error) {
	return deriveKeysFor(cfg, dirSecret, credential, fileKind{label: endpointLabel, version: version})
}
