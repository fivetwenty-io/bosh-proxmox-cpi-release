package endpointcache

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

// endpointMessagesGolden is the text of every failure messageScenarios makes
// for an endpoint store. It was taken from the code before a store had a
// display name, so a match shows that those messages are byte-identical.
var endpointMessagesGolden = []string{
	"directory open mode: cache directory <dir> (override layer): endpoint cache: unsafe permissions: <dir> has mode 0755 and must grant no group or other access",
	"directory path not clean: cache directory <dir>/. (override layer): endpoint cache: unsafe file type: <dir>/. is not a clean path",
	"directory without owner access: cache directory <dir> (override layer): endpoint cache: unsafe permissions: <dir> has mode 0500 and its owner needs read, write, and search access",
	"opened directory open mode: endpoint cache: unsafe permissions: <dir> has mode 0755 and must grant no group or other access",
	"no directory: endpoint cache: no cache directory",
	"secret open mode: endpoint cache: refusing directory secret <dir>/secret: endpoint cache: unsafe permissions: <dir>/secret has mode 0644 and must grant no group or other access",
	"secret lock busy: endpoint cache: directory secret lock is busy",
	"lock file open mode: endpoint cache: unsafe permissions: <dir>/<lock> has mode 0644 and must grant no group or other access",
	"skipped write: endpoint cache lock wait expired; skipping the cache write",
	"lock wait abandoned: endpoint cache: lock wait ended with the caller's context: context deadline exceeded",
	"damaged file: endpoint cache: invalid file: malformed envelope: invalid character 'o' in literal null (expecting 'u')",
	"file for another key: endpoint cache: file key does not match",
	"newer file version: endpoint cache: unsupported file version: version 2",
	"file open mode: endpoint cache: unsafe permissions: <dir>/<file> has mode 0644 and must grant no group or other access",
	"read on a closed store: endpoint cache: store is closed",
	"update on a closed store: endpoint cache: store is closed",
	"discovery lock on a closed store: endpoint cache: store is closed",
	"sweep on a closed store: endpoint cache: store is closed",
}

// sentinelGolden is the wording of each cause the endpoint cache has always
// printed, with the cause it must match under errors.Is.
var sentinelGolden = []struct {
	err  error
	text string
	base error
}{
	{errCacheOwnership, "endpoint cache: unsafe ownership", privatefile.ErrUnsafeOwnership},
	{errCacheMode, "endpoint cache: unsafe permissions", privatefile.ErrUnsafeMode},
	{errCacheFileType, "endpoint cache: unsafe file type", privatefile.ErrUnsafeFileType},
	{ErrLockReplaced, "endpoint cache: lock file kept changing while it was locked", nil},
	{ErrLockWaitAbandoned, "endpoint cache: lock wait ended with the caller's context", nil},
	{errLockBusy, "endpoint cache: cache file lock is busy", nil},
	{ErrSecretBusy, "endpoint cache: directory secret lock is busy", nil},
	{errSecretLength, "endpoint cache: directory secret has the wrong length", privatefile.ErrCorrupt},
	{ErrFileInvalid, "endpoint cache: invalid file", privatefile.ErrCorrupt},
	{ErrKeyMismatch, "endpoint cache: file key does not match", ErrFileInvalid},
	{ErrUnsupportedVersion, "endpoint cache: unsupported file version", ErrFileInvalid},
}

func TestEndpointStoreMessagesStayByteIdentical(t *testing.T) {
	for _, tc := range sentinelGolden {
		if tc.err.Error() != tc.text {
			t.Errorf("cause printed %q, want %q", tc.err.Error(), tc.text)
		}
		if tc.base != nil && !errors.Is(tc.err, tc.base) {
			t.Errorf("%q does not match its cause %v", tc.text, tc.base)
		}
	}

	// The default options, an explicit endpoint name, and an unusable name all
	// select the endpoint cache's texts.
	for _, name := range []string{"", cacheLabel, "bad: name", "bad\nname"} {
		if got := textsFor(name); got != endpointTexts || got.name != cacheLabel {
			t.Errorf("textsFor(%q) = %+v, want the endpoint cache's texts", name, got)
		}
	}

	want := endpointMessagesGolden
	if got := messageScenarios(t, StoreOptions{}, ""); !slices.Equal(got, want) {
		t.Errorf("default options changed a message:\n got %q\nwant %q", got, want)
	}
	explicit := StoreOptions{DisplayName: cacheLabel}
	if got := messageScenarios(t, explicit, cacheLabel); !slices.Equal(got, want) {
		t.Errorf("the explicit endpoint name changed a message:\n got %q\nwant %q", got, want)
	}
}

// TestAnHAProfileStoreNamesItselfInLockSecretAndDirectoryMessages runs the
// same failures against a profile store and shows that each message is the
// endpoint cache's with the profile cache's name in its place and nothing else
// changed, and that the profile store's causes still match the endpoint cache's.
func TestAnHAProfileStoreNamesItselfInLockSecretAndDirectoryMessages(t *testing.T) {
	got := messageScenarios(t, haProfileOptions(), HAProfileName)
	if len(got) != len(endpointMessagesGolden) {
		t.Fatalf("%d scenarios, want %d", len(got), len(endpointMessagesGolden))
	}
	for i, line := range endpointMessagesGolden {
		want := strings.ReplaceAll(line, cacheLabel, HAProfileName)
		if got[i] != want {
			t.Errorf("scenario %d\n got %q\nwant %q", i, got[i], want)
		}
		if strings.Contains(got[i], cacheLabel) {
			t.Errorf("scenario %d still says %q: %q", i, cacheLabel, got[i])
		}
	}
}

func TestAnHAProfileStoresCausesMatchTheEndpointCachesUnderErrorsIs(t *testing.T) {
	profile := textsFor(HAProfileName)
	if profile == endpointTexts || profile.name != HAProfileName {
		t.Fatalf("textsFor(%q) = %+v", HAProfileName, profile)
	}
	pairs := []struct{ own, endpoint error }{
		{profile.ownership, errCacheOwnership}, {profile.mode, errCacheMode}, {profile.fileType, errCacheFileType},
		{profile.lockReplaced, ErrLockReplaced}, {profile.lockAbandoned, ErrLockWaitAbandoned},
		{profile.lockBusy, errLockBusy}, {profile.secretBusy, ErrSecretBusy}, {profile.secretLength, errSecretLength},
		{profile.invalid, ErrFileInvalid}, {profile.keyMismatch, ErrKeyMismatch}, {profile.unsupported, ErrUnsupportedVersion},
	}
	for _, p := range pairs {
		if !errors.Is(p.own, p.endpoint) {
			t.Errorf("%q does not match %q", p.own, p.endpoint)
		}
		if want := strings.Replace(p.endpoint.Error(), cacheLabel, HAProfileName, 1); p.own.Error() != want {
			t.Errorf("cause printed %q, want %q", p.own.Error(), want)
		}
	}
	if profile.policy.Label != HAProfileName {
		t.Errorf("policy label %q, want %q", profile.policy.Label, HAProfileName)
	}
}

func TestAnHAProfileStoreWrapsItsLockReplacedAndBusyErrors(t *testing.T) {
	const name = "k.lock"
	dir := newStoreDir(t)
	root := openStoreRoot(t, dir)
	takes := 0
	profile := textsFor(HAProfileName)
	_, held, err := profile.heldLock(root, name, tryLockThen(root, name, &takes, func(*privatefile.Lock) {
		replaceLockFile(t, dir, name)
	}))
	if held || err == nil || !errors.Is(err, ErrLockReplaced) ||
		err.Error() != "HA profile cache: lock file kept changing while it was locked: "+name {
		t.Fatalf("heldLock: held %v, err %v, want the profile cache's lock replaced error naming the file", held, err)
	}
}
