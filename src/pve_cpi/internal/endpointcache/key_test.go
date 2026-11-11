package endpointcache

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func storeTestSecret(fill byte) keyMaterial {
	return keyMaterial(bytes.Repeat([]byte{fill}, secretBytes))
}

func mustDerive(t *testing.T, cfg KeyConfig, secret keyMaterial, credential string) (Key, keyMaterial) {
	t.Helper()
	key, mac, err := deriveKeys(cfg, secret, credential)
	if err != nil {
		t.Fatalf("deriveKeys: %v", err)
	}
	return key, mac
}

// rfc5869 is HKDF-SHA256 written out from RFC 5869 for a single output block,
// so the test checks the derivation against the standard rather than against
// the code under test.
func rfc5869(ikm, salt []byte, info string, length int) []byte {
	extract := hmac.New(sha256.New, salt)
	extract.Write(ikm)
	prk := extract.Sum(nil)
	expand := hmac.New(sha256.New, prk)
	expand.Write([]byte(info))
	expand.Write([]byte{1})
	return expand.Sum(nil)[:length]
}

// lengthPrefixed writes each part after its length as eight big-endian bytes,
// spelled out byte by byte so the test does not share the code's encoder.
func lengthPrefixed(parts ...string) []byte {
	var out []byte
	for _, part := range parts {
		n := uint64(len(part))
		out = append(
			out,
			byte(n>>56),
			byte(n>>48),
			byte(n>>40),
			byte(n>>32),
			byte(n>>24),
			byte(n>>16),
			byte(n>>8),
			byte(n),
		)
		out = append(out, part...)
	}
	return out
}

func TestDeriveKeysFollowsHKDFWithTheDirectorySecretAsSalt(t *testing.T) {
	secret := storeTestSecret(0x5a)
	key, mac := mustDerive(t, storeTestConfig(), secret, storeTestCredential)

	ikm := lengthPrefixed(storeTestUser, storeTestCredential)
	wantID := hex.EncodeToString(rfc5869(ikm, secret, "pve-cpi endpoint-cache id v1", 8))
	if key.CredentialID != wantID {
		t.Errorf("credential id %s, want %s", key.CredentialID, wantID)
	}
	digest := key.Digest()
	if want := rfc5869(ikm, secret, "pve-cpi endpoint-cache v1"+string(digest[:]), 32); !bytes.Equal(mac, want) {
		t.Error("HMAC key is not HKDF-SHA256 of the user and credential with the directory secret, " +
			"the versioned file label, and the key digest")
	}
	if len(key.CredentialID) != 16 {
		t.Errorf("credential id %q has %d characters, want 16", key.CredentialID, len(key.CredentialID))
	}
	if strings.HasPrefix(hex.EncodeToString(mac), key.CredentialID) {
		t.Error("the published identifier is a prefix of the HMAC key")
	}

	other, otherMAC := mustDerive(t, storeTestConfig(), storeTestSecret(0x5b), storeTestCredential)
	if other.CredentialID == key.CredentialID || bytes.Equal(otherMAC, mac) {
		t.Error("another directory secret derived the same identifier or HMAC key")
	}
	rotated, rotatedMAC := mustDerive(t, storeTestConfig(), secret, storeTestCredential+"-rotated")
	if rotated.CredentialID == key.CredentialID || bytes.Equal(rotatedMAC, mac) {
		t.Error("another credential derived the same identifier or HMAC key")
	}
	_, nextMAC, err := deriveKeysAt(storeTestConfig(), secret, storeTestCredential, fileVersion+1)
	if err != nil {
		t.Fatal(err)
	}
	digestInfo := "pve-cpi endpoint-cache v" + fmt.Sprint(fileVersion+1) + string(digest[:])
	if want := rfc5869(ikm, secret, digestInfo, 32); !bytes.Equal(nextMAC, want) || bytes.Equal(nextMAC, mac) {
		t.Error("the next schema version does not derive its own HMAC key from its own label")
	}
}

// TestDeriveKeysSeparatesUsersWhoShareASecret derives keys for two users with
// one password, and for pairs whose plain concatenation is the same. Each gets
// its own identifier and HMAC key, and one user and secret derive the same
// values every time.
func TestDeriveKeysSeparatesUsersWhoShareASecret(t *testing.T) {
	secret := storeTestSecret(0x6b)
	derive := func(user, credential string) (Key, keyMaterial) {
		cfg := storeTestConfig()
		cfg.User = user
		return mustDerive(t, cfg, secret, credential)
	}
	root, rootMAC := derive("root@pam", "hunter2")
	cpi, cpiMAC := derive("cpi@pve", "hunter2")
	if root.CredentialID == cpi.CredentialID || bytes.Equal(rootMAC, cpiMAC) || root.FileName() == cpi.FileName() {
		t.Error("two users with one password share an identifier, an HMAC key, or a file name")
	}
	again, againMAC := derive("root@pam", "hunter2")
	if again.CredentialID != root.CredentialID || !bytes.Equal(againMAC, rootMAC) || !again.Equal(root) {
		t.Error("one user and secret derived different values")
	}
	ab, abMAC := derive("ab", "c")
	a, aMAC := derive("a", "bc")
	if ab.CredentialID == a.CredentialID || bytes.Equal(abMAC, aMAC) {
		t.Error("the user and the secret run together")
	}
}

func TestDeriveKeysRefusesInputsThatCannotFormAKey(t *testing.T) {
	cases := map[string]struct {
		cfg        KeyConfig
		secret     keyMaterial
		credential string
	}{
		"no credential": {storeTestConfig(), storeTestSecret(1), ""},
		"no user": {
			KeyConfig{Endpoints: storeTestConfig().Endpoints},
			storeTestSecret(1),
			storeTestCredential,
		},
		"short secret": {storeTestConfig(), storeTestSecret(1)[:31], storeTestCredential},
		"no endpoints": {
			KeyConfig{VerifySSL: true, User: storeTestUser},
			storeTestSecret(1),
			storeTestCredential,
		},
		"endpoint no port": {
			KeyConfig{Endpoints: []string{"pve1.example.com"}, User: storeTestUser},
			storeTestSecret(1),
			storeTestCredential,
		},
		"endpoint bad host": {
			KeyConfig{Endpoints: []string{"pve 1.example.com:8006"}, User: storeTestUser},
			storeTestSecret(1),
			storeTestCredential,
		},
		"endpoint bad port": {
			KeyConfig{Endpoints: []string{"pve1.example.com:http"}, User: storeTestUser},
			storeTestSecret(1),
			storeTestCredential,
		},
		"second entry broken": {
			KeyConfig{Endpoints: []string{"pve1:8006", "pve2"}, User: storeTestUser},
			storeTestSecret(1),
			storeTestCredential,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := deriveKeys(tc.cfg, tc.secret, tc.credential); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("deriveKeys: %v, want ErrInvalidKey", err)
			}
		})
	}
}

func TestKeyNormalizesEndpointsAndKeepsTheirOrder(t *testing.T) {
	secret := storeTestSecret(7)
	key, _ := mustDerive(t, KeyConfig{
		Endpoints: []string{"PVE1.DC1.Example.COM.:8006", "[FD00:0:0::0001]:8007", "10.0.0.5:8006"},
		User:      storeTestUser,
	}, secret, storeTestCredential)
	want := []string{"pve1.dc1.example.com:8006", "[fd00::1]:8007", "10.0.0.5:8006"}
	if strings.Join(key.Endpoints, ",") != strings.Join(want, ",") {
		t.Errorf("endpoints %v, want %v", key.Endpoints, want)
	}
	if key.CASHA256 != SystemCA {
		t.Errorf("ca_sha256 %q, want %q with no CA", key.CASHA256, SystemCA)
	}
	same, _ := mustDerive(t, KeyConfig{Endpoints: want, User: storeTestUser}, secret, storeTestCredential)
	if !same.Equal(key) || same.Digest() != key.Digest() {
		t.Error("two spellings of one endpoint list made different keys")
	}
}

func TestKeyDigestCoversEveryField(t *testing.T) {
	secret := storeTestSecret(9)
	base := KeyConfig{
		Endpoints: []string{"pve1.example.com:8006", "pve2.example.com:8006"},
		VerifySSL: true,
		User:      storeTestUser,
	}
	baseKey, _ := mustDerive(t, base, secret, storeTestCredential)
	if !strings.HasPrefix(baseKey.Canonical(), "pve-cpi endpoint-cache key v1\n") {
		t.Errorf("canonical key %q does not start with its version", baseKey.Canonical())
	}
	if again, _ := mustDerive(t, base, secret, storeTestCredential); again.Canonical() != baseKey.Canonical() {
		t.Error("canonical key is not deterministic")
	}
	variants := map[string]func(*KeyConfig){
		"order":      func(c *KeyConfig) { c.Endpoints = []string{c.Endpoints[1], c.Endpoints[0]} },
		"port":       func(c *KeyConfig) { c.Endpoints = []string{"pve1.example.com:8007", c.Endpoints[1]} },
		"extra":      func(c *KeyConfig) { c.Endpoints = append(c.Endpoints, "pve3.example.com:8006") },
		"verify_ssl": func(c *KeyConfig) { c.VerifySSL = false },
		"ca":         func(c *KeyConfig) { c.CACert = "-----BEGIN CERTIFICATE-----\n" },
	}
	for name, change := range variants {
		cfg := base
		cfg.Endpoints = append([]string(nil), base.Endpoints...)
		change(&cfg)
		key, _ := mustDerive(t, cfg, secret, storeTestCredential)
		if key.Digest() == baseKey.Digest() || key.Equal(baseKey) {
			t.Errorf("%s: changing it left the key unchanged", name)
		}
	}
	credKey, _ := mustDerive(t, base, secret, "another-token")
	if credKey.Digest() == baseKey.Digest() || credKey.FileName() == baseKey.FileName() || credKey.Equal(baseKey) {
		t.Error("a different credential shares the key and file name")
	}
	userCfg := base
	userCfg.User = "someone-else@pve"
	if userKey, _ := mustDerive(
		t,
		userCfg,
		secret,
		storeTestCredential,
	); userKey.Equal(baseKey) || userKey.Digest() == baseKey.Digest() {
		t.Error("a different user shares the key")
	}

	ca := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	caKey, _ := mustDerive(
		t,
		KeyConfig{Endpoints: base.Endpoints, CACert: ca, User: storeTestUser},
		secret,
		storeTestCredential,
	)
	sum := sha256.Sum256([]byte(ca))
	if caKey.CASHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("ca_sha256 %q is not the SHA-256 of the PEM", caKey.CASHA256)
	}
}

func TestKeyCanonicalIsUnambiguous(t *testing.T) {
	a := Key{Endpoints: []string{"a:1", "b:2"}, CASHA256: SystemCA, CredentialID: "00"}
	b := Key{Endpoints: []string{"a:1\"\n\"b:2"}, CASHA256: SystemCA, CredentialID: "00"}
	c := Key{Endpoints: []string{"a:1"}, CASHA256: "b:2", CredentialID: "00"}
	if a.Canonical() == b.Canonical() || a.Canonical() == c.Canonical() || b.Canonical() == c.Canonical() {
		t.Error("two different keys share a canonical string")
	}
}

func TestFileNamesCarryTheSanitizedHostAndDigest(t *testing.T) {
	key, _ := mustDerive(t, storeTestConfig(), storeTestSecret(3), storeTestCredential)
	digest := key.Digest()
	base := "pve1.dc1.example.com-" + hex.EncodeToString(digest[:])[:16]
	if got := key.FileName(); got != base+".json" {
		t.Errorf("file name %q, want %q", got, base+".json")
	}
	if got := key.LockName(); got != base+".lock" {
		t.Errorf("lock name %q, want %q", got, base+".lock")
	}
	if got := key.DiscoverLockName(); got != base+".discover.lock" {
		t.Errorf("discover lock name %q, want %q", got, base+".discover.lock")
	}

	pattern := regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}-[0-9a-f]{16}\.json$`)
	for _, endpoint := range []string{
		"[fd00::1]:8006", "[::]:8006", "under_score.example.com:8006", "10.0.0.5:8006",
		strings.Repeat("a", 70) + ".example.com:8006",
	} {
		key, _ := mustDerive(
			t,
			KeyConfig{Endpoints: []string{endpoint}, User: storeTestUser},
			storeTestSecret(3),
			storeTestCredential,
		)
		if name := key.FileName(); !pattern.MatchString(name) {
			t.Errorf("%s: file name %q does not match %v", endpoint, name, pattern)
		}
	}
}

func TestSanitizeHost(t *testing.T) {
	cases := map[string]string{
		"pve1.dc1.example.com":   "pve1.dc1.example.com",
		"fd00::1":                "fd001",
		"::":                     "endpoint",
		"":                       "endpoint",
		"under_score.example":    "underscore.example",
		".hidden":                "hidden",
		"-.-leading":             "leading",
		"..":                     "endpoint",
		"Upper.Example":          "pper.xample",
		strings.Repeat("b", 100): strings.Repeat("b", 63),
	}
	for in, want := range cases {
		if got := sanitizeHost(in); got != want {
			t.Errorf("sanitizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}
