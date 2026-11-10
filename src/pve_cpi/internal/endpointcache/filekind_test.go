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
)

// haProfileOptions is the option set the HA profile cache opens its store with.
func haProfileOptions() StoreOptions {
	return StoreOptions{FilePrefix: HAProfilePrefix, Label: "pve-cpi ha-profile", Version: 1}
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
