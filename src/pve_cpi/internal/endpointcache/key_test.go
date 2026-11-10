package endpointcache

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

func TestDeriveKeysFollowsHKDFWithTheDirectorySecretAsSalt(t *testing.T) {
	secret := storeTestSecret(0x5a)
	key, mac := mustDerive(t, storeTestConfig(), secret, storeTestCredential)

	wantID := hex.EncodeToString(rfc5869([]byte(storeTestCredential), secret, "pve-cpi endpoint-cache id v1", 8))
	if key.CredentialID != wantID {
		t.Errorf("credential id %s, want %s", key.CredentialID, wantID)
	}
	if want := rfc5869([]byte(storeTestCredential), secret, "pve-cpi endpoint-cache v1", 32); !bytes.Equal(mac, want) {
		t.Error("HMAC key is not HKDF-SHA256 of the credential with the directory secret and the file label")
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
}

func TestDeriveKeysRefusesInputsThatCannotFormAKey(t *testing.T) {
	cases := map[string]struct {
		cfg        KeyConfig
		secret     keyMaterial
		credential string
	}{
		"no credential":       {storeTestConfig(), storeTestSecret(1), ""},
		"short secret":        {storeTestConfig(), storeTestSecret(1)[:31], storeTestCredential},
		"no endpoints":        {KeyConfig{VerifySSL: true}, storeTestSecret(1), storeTestCredential},
		"endpoint no port":    {KeyConfig{Endpoints: []string{"pve1.example.com"}}, storeTestSecret(1), storeTestCredential},
		"endpoint bad host":   {KeyConfig{Endpoints: []string{"pve 1.example.com:8006"}}, storeTestSecret(1), storeTestCredential},
		"endpoint bad port":   {KeyConfig{Endpoints: []string{"pve1.example.com:http"}}, storeTestSecret(1), storeTestCredential},
		"second entry broken": {KeyConfig{Endpoints: []string{"pve1:8006", "pve2"}}, storeTestSecret(1), storeTestCredential},
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
	}, secret, storeTestCredential)
	want := []string{"pve1.dc1.example.com:8006", "[fd00::1]:8007", "10.0.0.5:8006"}
	if strings.Join(key.Endpoints, ",") != strings.Join(want, ",") {
		t.Errorf("endpoints %v, want %v", key.Endpoints, want)
	}
	if key.CASHA256 != SystemCA {
		t.Errorf("ca_sha256 %q, want %q with no CA", key.CASHA256, SystemCA)
	}
	same, _ := mustDerive(t, KeyConfig{Endpoints: want}, secret, storeTestCredential)
	if !same.Equal(key) || same.Digest() != key.Digest() {
		t.Error("two spellings of one endpoint list made different keys")
	}
}

func TestKeyDigestCoversEveryField(t *testing.T) {
	secret := storeTestSecret(9)
	base := KeyConfig{Endpoints: []string{"pve1.example.com:8006", "pve2.example.com:8006"}, VerifySSL: true}
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
	if credKey.Digest() == baseKey.Digest() || credKey.FileName() == baseKey.FileName() {
		t.Error("a different credential shares the key and file name")
	}

	ca := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	caKey, _ := mustDerive(t, KeyConfig{Endpoints: base.Endpoints, CACert: ca}, secret, storeTestCredential)
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
		key, _ := mustDerive(t, KeyConfig{Endpoints: []string{endpoint}}, storeTestSecret(3), storeTestCredential)
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
