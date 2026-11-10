package endpointcache

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

// haProfileOptions is the option set the HA profile cache opens its store with.
func haProfileOptions() StoreOptions {
	return StoreOptions{FilePrefix: HAProfilePrefix, Label: "pve-cpi ha-profile", Version: 1, DisplayName: HAProfileName}
}

func TestKindForDefaultsToTheEndpointCacheFile(t *testing.T) {
	kind, err := kindFor(StoreOptions{})
	if err != nil {
		t.Fatalf("kindFor: %v", err)
	}
	if kind != endpointKind {
		t.Errorf("default kind %+v, want %+v", kind, endpointKind)
	}
	if endpointKind.prefix != "" || endpointKind.label != "pve-cpi endpoint-cache" || endpointKind.version != 1 {
		t.Errorf("endpoint kind %+v changed the label, prefix, or version", endpointKind)
	}
}

func TestKindForNamesTheFileAndDefaultsTheVersionToOne(t *testing.T) {
	kind, err := kindFor(StoreOptions{FilePrefix: HAProfilePrefix, Label: "pve-cpi ha-profile", DisplayName: HAProfileName})
	if err != nil {
		t.Fatalf("kindFor: %v", err)
	}
	if kind.version != 1 {
		t.Errorf("an unset version gave %d, want the literal 1", kind.version)
	}
	if defaultKindVersion != 1 {
		t.Errorf("defaultKindVersion = %d, want 1", defaultKindVersion)
	}
	if kind.name != "HA profile cache" {
		t.Errorf("name %q, want HA profile cache", kind.name)
	}
	if endpointKind.name != "endpoint cache" {
		t.Errorf("endpoint kind name %q, want endpoint cache", endpointKind.name)
	}
}

func TestKindForDefaultsTheDisplayNameToTheEndpointCache(t *testing.T) {
	kind, err := kindFor(StoreOptions{FilePrefix: HAProfilePrefix, Label: "pve-cpi ha-profile"})
	if err != nil || kind.name != cacheLabel {
		t.Fatalf("an empty display name gave %q, err %v, want %q", kind.name, err, cacheLabel)
	}
	kind, err = kindFor(StoreOptions{DisplayName: cacheLabel})
	if err != nil || kind != endpointKind {
		t.Fatalf("the endpoint name on the endpoint kind gave %+v, err %v", kind, err)
	}
}

func TestKindForValidatesTheOptions(t *testing.T) {
	long := strings.Repeat("a", maxFileLabel+1)
	cases := map[string]struct {
		opts StoreOptions
		ok   bool
	}{
		"ha profile":                {haProfileOptions(), true},
		"ha profile version unset":  {StoreOptions{FilePrefix: HAProfilePrefix, Label: "pve-cpi ha-profile"}, true},
		"ha profile version 2":      {StoreOptions{FilePrefix: HAProfilePrefix, Label: "pve-cpi ha-profile", Version: 2}, true},
		"label only":                {StoreOptions{Label: "pve-cpi ha-profile"}, false},
		"version only":              {StoreOptions{Version: 2}, false},
		"prefix only":               {StoreOptions{FilePrefix: HAProfilePrefix}, false},
		"unknown prefix":            {StoreOptions{FilePrefix: "other-", Label: "pve-cpi other"}, false},
		"prefix without hyphen":     {StoreOptions{FilePrefix: "ha-profile", Label: "pve-cpi ha-profile"}, false},
		"uppercase prefix":          {StoreOptions{FilePrefix: "HA-PROFILE-", Label: "pve-cpi ha-profile"}, false},
		"endpoint label":            {StoreOptions{FilePrefix: HAProfilePrefix, Label: "pve-cpi endpoint-cache"}, false},
		"endpoint label extended":   {StoreOptions{FilePrefix: HAProfilePrefix, Label: "pve-cpi endpoint-cache v1"}, false},
		"uppercase label":           {StoreOptions{FilePrefix: HAProfilePrefix, Label: "Pve ha"}, false},
		"label with a control byte": {StoreOptions{FilePrefix: HAProfilePrefix, Label: "pve ha\n"}, false},
		"label with a double space": {StoreOptions{FilePrefix: HAProfilePrefix, Label: "pve  ha"}, false},
		"label too long":            {StoreOptions{FilePrefix: HAProfilePrefix, Label: long}, false},
		"negative version":          {StoreOptions{FilePrefix: HAProfilePrefix, Label: "pve-cpi ha-profile", Version: -1}, false},
		"display name only":         {StoreOptions{DisplayName: HAProfileName}, false},
		"display name with a break": {StoreOptions{FilePrefix: HAProfilePrefix, Label: "pve-cpi ha-profile", DisplayName: "HA\nprofile"}, false},
		"display name with a colon": {StoreOptions{FilePrefix: HAProfilePrefix, Label: "pve-cpi ha-profile", DisplayName: "HA: profile"}, false},
		"display name too long": {
			StoreOptions{FilePrefix: HAProfilePrefix, Label: "pve-cpi ha-profile", DisplayName: strings.Repeat("a", maxDisplayName+1)}, false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			kind, err := kindFor(tc.opts)
			if tc.ok {
				if err != nil {
					t.Fatalf("kindFor: %v", err)
				}
				if kind.prefix != tc.opts.FilePrefix || kind.label != tc.opts.Label || kind.version < 1 {
					t.Errorf("kind %+v does not follow the options %+v", kind, tc.opts)
				}
				return
			}
			if !errors.Is(err, ErrInvalidFileKind) {
				t.Fatalf("kindFor: %v, want ErrInvalidFileKind", err)
			}
		})
	}
}

func TestOpenStoreRefusesAnInvalidFileKindAndClosesTheRoot(t *testing.T) {
	dir := newStoreDir(t)
	root := storeTestRoot(t, dir)
	_, err := OpenStore(context.Background(), root, storeTestConfig(), storeTestCredential,
		StoreOptions{FilePrefix: HAProfilePrefix, Label: "pve-cpi endpoint-cache"})
	if !errors.Is(err, ErrInvalidFileKind) {
		t.Fatalf("OpenStore: %v, want ErrInvalidFileKind", err)
	}
	if _, statErr := root.Stat("."); statErr == nil {
		t.Error("OpenStore left the root open after refusing the options")
	}
	if got := storeDirNames(t, dir); len(got) != 0 {
		t.Errorf("a refused kind created %v, want nothing", got)
	}
}

func TestADefaultStoreAndAnExplicitEndpointStoreMakeIdenticalFiles(t *testing.T) {
	dir := newStoreDir(t)
	def := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	storePut(t, def, "same")
	want := readStoreFile(t, filepath.Join(dir, def.FileName()))

	explicit, err := openStoreOfKind(context.Background(), storeTestRoot(t, dir), storeTestConfig(), storeTestCredential,
		StoreOptions{}, fileKind{label: "pve-cpi endpoint-cache", version: 1})
	if err != nil {
		t.Fatalf("openStoreOfKind: %v", err)
	}
	t.Cleanup(func() { _ = explicit.Close() })
	if explicit.FileName() != def.FileName() || explicit.kind.lockName(explicit.Key()) != def.Key().LockName() {
		t.Errorf("explicit names %s and %s differ from %s and %s",
			explicit.FileName(), explicit.kind.lockName(explicit.Key()), def.FileName(), def.Key().LockName())
	}
	if err := os.Remove(filepath.Join(dir, def.FileName())); err != nil {
		t.Fatal(err)
	}
	storePut(t, explicit, "same")
	if got := readStoreFile(t, filepath.Join(dir, explicit.FileName())); !bytes.Equal(got, want) {
		t.Errorf("explicit store wrote\n%s\nthe default store wrote\n%s", got, want)
	}
	var read storeTestPayload
	if err := def.Read(&read); err != nil || read.Note != "same" {
		t.Errorf("default store reading the explicit store's file: %+v, %v", read, err)
	}
}

func TestAnHAProfileStoreUsesItsOwnNames(t *testing.T) {
	dir := newStoreDir(t)
	endpoint := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	ha := openTestStore(t, dir, storeTestCredential, haProfileOptions())
	if !ha.Key().Equal(endpoint.Key()) {
		t.Error("the two kinds of file for one entry must share one key")
	}
	if !regexp.MustCompile(`^ha-profile-pve1\.dc1\.example\.com-[0-9a-f]{16}\.json$`).MatchString(ha.FileName()) {
		t.Errorf("HA profile file name %q has the wrong shape", ha.FileName())
	}
	if ha.FileName() != HAProfilePrefix+endpoint.FileName() {
		t.Errorf("HA profile file %q is not the prefixed endpoint file %q", ha.FileName(), endpoint.FileName())
	}
	storePut(t, ha, "profile")
	storePut(t, endpoint, "endpoint")
	want := []string{
		secretFile, secretLockFile, endpoint.FileName(), endpoint.Key().LockName(),
		ha.FileName(), HAProfilePrefix + ha.Key().LockName(),
	}
	if got := storeDirNames(t, dir); !sameNames(got, want) {
		t.Errorf("directory holds %v, want %v", got, want)
	}
	var got storeTestPayload
	if err := ha.Read(&got); err != nil || got.Note != "profile" {
		t.Errorf("HA store Read: %+v, %v", got, err)
	}
	if err := endpoint.Read(&got); err != nil || got.Note != "endpoint" {
		t.Errorf("endpoint store Read: %+v, %v", got, err)
	}
}

func TestADifferentSchemaVersionIsADifferentHMACKey(t *testing.T) {
	dir := newStoreDir(t)
	v1 := openTestStore(t, dir, storeTestCredential, haProfileOptions())
	storePut(t, v1, "v1")
	opts := haProfileOptions()
	opts.Version = 2
	v2 := openTestStore(t, dir, storeTestCredential, opts)
	var got storeTestPayload
	if err := v2.Read(&got); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("version 2 store reading a version 1 file: %v, want ErrUnsupportedVersion", err)
	}

	// Relabel the version 1 file as version 2 so the version check passes. The
	// version 2 store must still refuse it, because its HMAC key differs, and
	// it must refuse it on the checksum and not on the version.
	path := filepath.Join(dir, v1.FileName())
	original := readStoreFile(t, path)
	relabeled := bytes.Replace(original, []byte(`"version":1`), []byte(`"version":2`), 1)
	if bytes.Equal(relabeled, original) {
		t.Fatalf("the file %s carries no version member to relabel", original)
	}
	writeStoreFile(t, path, relabeled, 0o600)
	got = storeTestPayload{}
	err := v2.Read(&got)
	if !errors.Is(err, ErrFileInvalid) || errors.Is(err, ErrUnsupportedVersion) || got.Note != "" {
		t.Fatalf("version 2 store reading a relabeled file: %+v, %v, want an HMAC failure and not a version one", got, err)
	}
	// The same bytes verify under the version 1 key once the label is restored.
	writeStoreFile(t, path, original, 0o600)
	if err := v1.Read(&got); err != nil || got.Note != "v1" {
		t.Fatalf("version 1 store reading its own file: %+v, %v", got, err)
	}
}

func TestAFileOfOneKindNeverVerifiesAsTheOther(t *testing.T) {
	dir := newStoreDir(t)
	endpoint := openTestStore(t, dir, storeTestCredential, StoreOptions{})
	ha := openTestStore(t, dir, storeTestCredential, haProfileOptions())
	storePut(t, endpoint, "endpoint")
	storePut(t, ha, "profile")

	endpointPath := filepath.Join(dir, endpoint.FileName())
	haPath := filepath.Join(dir, ha.FileName())
	endpointBytes := readStoreFile(t, endpointPath)
	haBytes := readStoreFile(t, haPath)

	writeStoreFile(t, endpointPath, haBytes, 0o600)
	var got storeTestPayload
	if err := endpoint.Read(&got); !errors.Is(err, ErrFileInvalid) {
		t.Errorf("endpoint store reading an HA profile file: %v, want ErrFileInvalid", err)
	}
	writeStoreFile(t, haPath, endpointBytes, 0o600)
	got = storeTestPayload{}
	if err := ha.Read(&got); !errors.Is(err, ErrFileInvalid) {
		t.Errorf("HA store reading an endpoint file: %v, want ErrFileInvalid", err)
	}
	if got.Note != "" {
		t.Errorf("HA store filled the payload from an endpoint file: %+v", got)
	}
}

func TestSweepRemovesAnAgedHAProfileFileAndKeepsAFreshOne(t *testing.T) {
	dir, root := sweepDir(t)
	const base = "ha-profile-" + sweepBase
	aged := 31 * 24 * time.Hour
	sweepFile(t, dir, base+".json", aged)
	sweepFile(t, dir, base+".lock", aged)
	sweepFile(t, dir, base+".discover.lock", aged)
	const freshBase = "ha-profile-pve9.dc1.example.com-0123456789abcdef"
	sweepFile(t, dir, freshBase+".json", 29*24*time.Hour)
	sweepFile(t, dir, freshBase+".lock", aged)
	result := runSweep(t, root)
	want := []string{base + ".discover.lock", base + ".json", base + ".lock"}
	if !sameNames(result.Removed, want) {
		t.Errorf("removed %v, want %v", result.Removed, want)
	}
	if got := storeDirNames(t, dir); !sameNames(got, []string{freshBase + ".json", freshBase + ".lock"}) {
		t.Errorf("left %v, want the fresh file and the lock beside it", got)
	}
}

func TestSweepNamesCoverAHAProfileFileWithALongHost(t *testing.T) {
	host := strings.Repeat("a", maxNameHost)
	name := HAProfilePrefix + host + "-0123456789abcdef.json"
	dir, root := sweepDir(t)
	sweepFile(t, dir, name, 31*24*time.Hour)
	if result := runSweep(t, root); !sameNames(result.Removed, []string{name}) {
		t.Errorf("removed %v, want %s", result.Removed, name)
	}
}

func TestSweepLeavesAnUpperCaseHAProfileNameAlone(t *testing.T) {
	dir, root := sweepDir(t)
	const name = "HA-PROFILE-" + sweepKey
	sweepFile(t, dir, name, 90*24*time.Hour)
	if result := runSweep(t, root); len(result.Removed) != 0 {
		t.Errorf("sweep removed %v, want no name the cache does not make", result.Removed)
	}
}

func sameNames(a, b []string) bool {
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	slices.Sort(x)
	slices.Sort(y)
	return strings.Join(x, "\x00") == strings.Join(y, "\x00")
}

func TestAnHAProfileStoreNamesItselfInItsMessages(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, haProfileOptions())
	storePut(t, s, "profile")
	path := filepath.Join(dir, s.FileName())
	const name = "HA profile cache"
	mustName := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), "endpoint cache") {
			t.Errorf("%s: %v, want a message that names the %s and not the endpoint cache", what, err, name)
		}
	}

	var got storeTestPayload
	writeStoreFile(t, path, []byte("not json"), 0o600)
	err := s.Read(&got)
	mustName("a damaged file", err)
	if !errors.Is(err, ErrFileInvalid) {
		t.Errorf("a damaged file: %v, want it to match ErrFileInvalid", err)
	}

	other := s.Key()
	other.CredentialID = "0000000000000000"
	if err := s.policy.WriteEnvelope(s.root, s.name, s.kind.version, storeTestPayload{Key: other}, s.ops); err != nil {
		t.Fatal(err)
	}
	err = s.Read(&got)
	mustName("a file for another key", err)
	if !errors.Is(err, ErrKeyMismatch) || !errors.Is(err, ErrFileInvalid) {
		t.Errorf("a file for another key: %v, want ErrKeyMismatch", err)
	}

	if err := s.policy.WriteEnvelope(s.root, s.name, s.kind.version+1, storeTestPayload{Key: s.Key()}, s.ops); err != nil {
		t.Fatal(err)
	}
	err = s.Read(&got)
	mustName("a newer version", err)
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("a newer version: %v, want ErrUnsupportedVersion", err)
	}

	storePut(t, s, "again")
	if err := os.Chmod(path, 0o644); err != nil { // #nosec G302 -- the test opens the mode on purpose
		t.Fatal(err)
	}
	err = s.Read(&got)
	mustName("an open mode", err)
	if !errors.Is(err, privatefile.ErrUnsafeMode) || !errors.Is(err, errCacheMode) {
		t.Errorf("an open mode: %v, want the endpoint cache's mode refusal", err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	mustName("Read on a closed store", s.Read(&got))
	_, err = Update(context.Background(), s, func(*storeTestPayload) (*storeTestPayload, error) { return nil, nil })
	mustName("Update on a closed store", err)
	_, _, err = s.TryLockDiscovery(context.Background())
	mustName("TryLockDiscovery on a closed store", err)

	endpoint := openTestStore(t, newStoreDir(t), storeTestCredential, StoreOptions{})
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Read(&got); err == nil || !strings.HasPrefix(err.Error(), "endpoint cache: ") {
		t.Errorf("the endpoint cache's message %v changed", err)
	}
	var none *Store
	if err := none.Read(&got); err == nil || !strings.HasPrefix(err.Error(), "endpoint cache: ") {
		t.Errorf("a nil store's message %v, want the endpoint cache's label", err)
	}
}

func TestAnHAProfileStoreLogsASkippedWriteAsTheHAProfileCache(t *testing.T) {
	dir := newStoreDir(t)
	logger, observed := log.NewObservedLogger(log.LevelDebug)
	opts := haProfileOptions()
	opts.Logger = logger
	s := openTestStore(t, dir, storeTestCredential, opts)
	s.lockWait = 150 * time.Millisecond
	holder := startStoreChild(t, storeChildLock, dir, storeChildLockEnv+"="+s.kind.lockName(s.Key()))
	if got := holder.line(t); got != "locked" {
		t.Fatalf("child said %q, want locked", got)
	}
	written, err := Update(context.Background(), s, func(*storeTestPayload) (*storeTestPayload, error) {
		return &storeTestPayload{Key: s.Key()}, nil
	})
	if err != nil || written {
		t.Fatalf("Update under a held lock: written %v, err %v", written, err)
	}
	entries := observed.All()
	if len(entries) != 1 || entries[0].Message != "HA profile cache lock wait expired; skipping the cache write" ||
		entries[0].Attrs["file"] != s.FileName() {
		t.Fatalf("logged %+v, want one skip line that names the HA profile cache", entries)
	}
	holder.finish(t)
}

func TestStoreSweepRemovesOrphansAndKeepsItsOwnFileAndLock(t *testing.T) {
	dir := newStoreDir(t)
	s := openTestStore(t, dir, storeTestCredential, haProfileOptions())
	storePut(t, s, "profile")
	lockName := s.kind.lockName(s.Key())
	if got := storeDirNames(t, dir); !slices.Contains(got, lockName) || !slices.Contains(got, s.FileName()) {
		t.Fatalf("directory holds %v, want the store's file %s and lock %s", got, s.FileName(), lockName)
	}
	age := func(name string, by time.Duration) {
		t.Helper()
		when := sweepNow.Add(-by)
		if err := os.Chtimes(filepath.Join(dir, name), when, when); err != nil {
			t.Fatal(err)
		}
	}
	age(s.FileName(), 90*24*time.Hour)
	age(lockName, 90*24*time.Hour)
	const orphan = "ha-profile-" + sweepBase + ".json"
	const fresh = "ha-profile-pve9.dc1.example.com-0123456789abcdef.json"
	sweepFile(t, dir, orphan, 40*24*time.Hour)
	sweepFile(t, dir, fresh, time.Hour)
	sweepFile(t, dir, sweepTemp, 2*time.Hour)

	result := s.Sweep(sweepNow)
	if result.Err != nil {
		t.Fatalf("Sweep: %v", result.Err)
	}
	if !sameNames(result.Removed, []string{orphan, sweepTemp}) {
		t.Errorf("removed %v, want the aged orphan and the old temporary file", result.Removed)
	}
	want := []string{secretFile, secretLockFile, s.FileName(), lockName, fresh}
	if got := storeDirNames(t, dir); !sameNames(got, want) {
		t.Errorf("directory holds %v, want %v", got, want)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if result := s.Sweep(sweepNow); result.Err == nil || !strings.Contains(result.Err.Error(), "HA profile cache") {
		t.Errorf("Sweep on a closed store: %v, want an error that names the HA profile cache", result.Err)
	}
}
