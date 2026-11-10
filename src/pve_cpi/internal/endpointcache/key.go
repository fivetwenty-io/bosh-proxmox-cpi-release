package endpointcache

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

const (
	// keyVersion starts the string a key digest is computed over. A change to
	// what the key holds or how it is written changes this label, so files
	// written under the old form are never found under the new one.
	keyVersion = "pve-cpi endpoint-cache key v1"

	// credentialIDLabel is the HKDF info string of the credential identifier,
	// and a file kind's label, followed by " v" and the file's schema version,
	// starts the info string of its HMAC key. Distinct labels keep the
	// published identifier and the secret HMAC keys independent although all of
	// them come from the same credential and salt.
	credentialIDLabel = "pve-cpi endpoint-cache id v1" // #nosec G101 -- HKDF info label, not a credential

	// credentialIDBytes is the length of the credential identifier, which the
	// key holds as 16 hex characters.
	credentialIDBytes = 8
	// hmacKeyBytes is the length of the HMAC-SHA256 key.
	hmacKeyBytes = sha256.Size

	// SystemCA is the key's CA digest when the entry sets no CA certificate and
	// verifies against the system trust store.
	SystemCA = "system"

	// fileDigestHex is how many hex characters of the key digest a file name
	// carries.
	fileDigestHex = 16
	// maxNameHost bounds the sanitized host at the start of a file name.
	maxNameHost = 63
	// fallbackNameHost names a file whose host keeps no character through
	// sanitizing, such as the IPv6 literal "::".
	fallbackNameHost = "endpoint"

	fileSuffix         = ".json"
	lockSuffix         = ".lock"
	discoverLockSuffix = ".discover.lock"
)

// ErrInvalidKey means the endpoint configuration or the credential cannot
// form a cache key. The caller runs without a cache.
var ErrInvalidKey = errors.New("endpoint cache: invalid cache key input")

// KeyConfig is the part of an entry's effective config that selects its cache
// file. Storage names and every other policy field stay out, so a placement
// change does not fragment the cache.
type KeyConfig struct {
	// Endpoints is the ordered endpoint list, each entry a host and an explicit
	// port as the config layer resolves them. The order is the preference
	// order and the first entry is the seed, so it is kept as given. The
	// caller passes the configured addresses of the client's endpoint plan,
	// not cfg.Endpoints, because a config that sets only pve.host leaves
	// cfg.Endpoints empty and is exactly the one-endpoint case that discovers.
	Endpoints []string
	// VerifySSL is the entry's effective verify_ssl.
	VerifySSL bool
	// CACert is the entry's CA certificate PEM, or empty when it verifies
	// against the system trust store.
	CACert string
	// User is the user, such as cpi@pve, that the password belongs to, or the
	// API token ID, such as cpi@pve!discovery, that the token secret belongs
	// to. It goes into the credential identifier with the secret, so two users
	// who share one password keep separate files, and it is never stored.
	User string
}

// Key identifies one endpoint configuration and credential. Every cache file
// holds its key in its payload, and a reader refuses a file whose key differs
// from its own, so two configurations that share a file name by chance never
// read each other's pins.
type Key struct {
	// Endpoints is the normalized endpoint list in its configured order, each
	// entry in net.JoinHostPort form.
	Endpoints []string `json:"endpoints"`
	// VerifySSL is the entry's effective verify_ssl.
	VerifySSL bool `json:"verify_ssl"`
	// CASHA256 is the lowercase hex SHA-256 of the CA PEM, or SystemCA.
	CASHA256 string `json:"ca_sha256"`
	// CredentialID is 16 hex characters derived from the user and the
	// credential with the directory secret as the HKDF salt, so it reveals
	// nothing that helps a password guess without the secret.
	CredentialID string `json:"credential_id"`
}

// Equal reports whether k and other name the same configuration and
// credential.
func (k Key) Equal(other Key) bool {
	return slices.Equal(k.Endpoints, other.Endpoints) && k.VerifySSL == other.VerifySSL &&
		k.CASHA256 == other.CASHA256 && k.CredentialID == other.CredentialID
}

// Canonical returns the versioned string the key digest is computed over: the
// version label, then one line per field in a fixed order. The endpoint list
// is prefixed with its length and every string is quoted, so two different
// keys never share a string.
func (k Key) Canonical() string {
	var b strings.Builder
	b.WriteString(keyVersion)
	b.WriteString("\nendpoints ")
	b.WriteString(strconv.Itoa(len(k.Endpoints)))
	for _, endpoint := range k.Endpoints {
		b.WriteString("\n")
		b.WriteString(strconv.Quote(endpoint))
	}
	b.WriteString("\nverify_ssl ")
	b.WriteString(strconv.FormatBool(k.VerifySSL))
	b.WriteString("\nca_sha256 ")
	b.WriteString(strconv.Quote(k.CASHA256))
	b.WriteString("\ncredential_id ")
	b.WriteString(strconv.Quote(k.CredentialID))
	return b.String()
}

// Digest returns the SHA-256 of the canonical key string.
func (k Key) Digest() [sha256.Size]byte {
	return sha256.Sum256([]byte(k.Canonical()))
}

// FileName returns the cache file's name: the sanitized host of the first
// endpoint, a hyphen, 16 hex characters of the key digest, and ".json".
func (k Key) FileName() string { return k.baseName() + fileSuffix }

// LockName returns the name of the lock file that writers of the cache file
// hold.
func (k Key) LockName() string { return k.baseName() + lockSuffix }

// DiscoverLockName returns the name of the lock file that serializes discovery
// for the key.
func (k Key) DiscoverLockName() string { return k.baseName() + discoverLockSuffix }

func (k Key) baseName() string {
	host := ""
	if len(k.Endpoints) > 0 {
		host = k.Endpoints[0]
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
	}
	digest := k.Digest()
	return sanitizeHost(host) + "-" + hex.EncodeToString(digest[:])[:fileDigestHex]
}

// sanitizeHost keeps only the lowercase letters, digits, dots, and hyphens of
// host, cut to 63 characters, so an operator can tell which file belongs to
// which cluster. Leading dots and hyphens are dropped as well, so the name
// can never look like a hidden or temporary file or an option.
func sanitizeHost(host string) string {
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		}
	}
	out := strings.TrimLeft(b.String(), ".-")
	if len(out) > maxNameHost {
		out = out[:maxNameHost]
	}
	if out == "" {
		return fallbackNameHost
	}
	return out
}

// normalizeEndpoints returns the endpoint list in the form the key holds. An
// entry must carry its own port, because the config layer resolves every
// entry's port before a client is built, and guessing one here could key two
// different endpoints alike.
func normalizeEndpoints(endpoints []string) ([]string, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("%w: no endpoints", ErrInvalidKey)
	}
	out := make([]string, 0, len(endpoints))
	for i, entry := range endpoints {
		if _, _, err := net.SplitHostPort(entry); err != nil {
			return nil, fmt.Errorf("%w: endpoint %d has no port", ErrInvalidKey, i)
		}
		host, port, ok := config.NormalizeEndpoint(entry)
		if !ok {
			return nil, fmt.Errorf("%w: endpoint %d is not a host and port", ErrInvalidKey, i)
		}
		out = append(out, net.JoinHostPort(host, strconv.Itoa(port)))
	}
	return out, nil
}

// caDigest returns the key's CA field for the CA PEM pem.
func caDigest(pem string) string {
	if pem == "" {
		return SystemCA
	}
	sum := sha256.Sum256([]byte(pem))
	return hex.EncodeToString(sum[:])
}

// deriveKeys builds the key for cfg and the HMAC key for its file at the
// current file version. See deriveKeysAt.
func deriveKeys(cfg KeyConfig, dirSecret keyMaterial, credential string) (Key, keyMaterial, error) {
	return deriveKeysAt(cfg, dirSecret, credential, fileVersion)
}

// deriveKeysAt builds the key for cfg and the HMAC key for a file of the given
// schema version from the directory secret, the user, and the credential,
// which is the token secret or the password. Both derivations are HKDF-SHA256
// over the length-prefixed user followed by the length-prefixed credential,
// with the directory secret as the salt, so neither the identifier nor a file
// can be used to test password guesses without the secret. The HMAC key's info
// is the versioned label followed by the 32-byte key digest, so a schema bump
// changes the key and a file moved from another key fails its HMAC.
func deriveKeysAt(cfg KeyConfig, dirSecret keyMaterial, credential string, version int) (Key, keyMaterial, error) {
	return deriveKeysFor(cfg, dirSecret, credential, fileKind{label: endpointLabel, version: version})
}

// deriveKeysFor is deriveKeysAt for a file of any kind. The credential
// identifier and the key digest do not depend on the kind, so every kind of
// file for one entry shares one key, while the HMAC key takes the kind's label
// and schema version, so no kind's file verifies as another's.
func deriveKeysFor(cfg KeyConfig, dirSecret keyMaterial, credential string, kind fileKind) (Key, keyMaterial, error) {
	if len(dirSecret) != secretBytes {
		return Key{}, nil, fmt.Errorf("%w: directory secret has the wrong length", ErrInvalidKey)
	}
	if cfg.User == "" {
		return Key{}, nil, fmt.Errorf("%w: no user or token ID", ErrInvalidKey)
	}
	if credential == "" {
		return Key{}, nil, fmt.Errorf("%w: no credential", ErrInvalidKey)
	}
	endpoints, err := normalizeEndpoints(cfg.Endpoints)
	if err != nil {
		return Key{}, nil, err
	}
	ikm := credentialInput(cfg.User, credential)
	defer clear(ikm)
	id, err := hkdf.Key(sha256.New, ikm, dirSecret, credentialIDLabel, credentialIDBytes)
	if err != nil {
		return Key{}, nil, fmt.Errorf("endpoint cache: derive credential identifier: %w", err)
	}
	key := Key{
		Endpoints:    endpoints,
		VerifySSL:    cfg.VerifySSL,
		CASHA256:     caDigest(cfg.CACert),
		CredentialID: hex.EncodeToString(id),
	}
	mac, err := hkdf.Key(sha256.New, ikm, dirSecret, hmacKeyInfo(kind.label, kind.version, key.Digest()), hmacKeyBytes)
	if err != nil {
		return Key{}, nil, fmt.Errorf("endpoint cache: derive file key: %w", err)
	}
	return key, keyMaterial(mac), nil
}

// credentialInput is the HKDF input keying material: the user and the
// credential, each preceded by its length as eight big-endian bytes, so no two
// pairs share an input.
func credentialInput(user, credential string) []byte {
	ikm := make([]byte, 0, 16+len(user)+len(credential))
	ikm = binary.BigEndian.AppendUint64(ikm, uint64(len(user)))
	ikm = append(ikm, user...)
	ikm = binary.BigEndian.AppendUint64(ikm, uint64(len(credential)))
	return append(ikm, credential...)
}

// hmacKeyInfo is the HKDF info of the HMAC key for a file with the given label,
// schema version, and key digest. The digest has a fixed length, so the label
// and the digest cannot run together. The endpoint cache's label gives the
// string "pve-cpi endpoint-cache v<version>" followed by the digest.
func hmacKeyInfo(label string, version int, digest [sha256.Size]byte) string {
	return label + " v" + strconv.Itoa(version) + string(digest[:])
}
