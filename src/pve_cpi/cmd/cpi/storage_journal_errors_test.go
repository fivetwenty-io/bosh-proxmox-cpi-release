package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

const (
	hintTestExecutable = "/var/vcap/packages/pve_cpi/bin/cpi"
	hintTestConfig     = "/var/vcap/jobs/pve_cpi/config/cpi.json"
)

var hintTestArgv = []string{hintTestExecutable, "storage-journal", "audit", "--config", hintTestConfig}

func hintTestLookup(uid string) (*user.User, error) {
	switch uid {
	case "0":
		return &user.User{Uid: uid, Username: "root"}, nil
	case "501":
		return &user.User{Uid: uid, Username: "operator"}, nil
	case "1000":
		return &user.User{Uid: uid, Username: "vcap"}, nil
	}
	return nil, user.UnknownUserIdError(4242)
}

// hintTestHost is a host whose identity the test chooses, because CI runs as
// root and cannot give a file to another owner portably.
func hintTestHost(euid int) storageJournalHost {
	return storageJournalHost{
		euid:     euid,
		lookup:   hintTestLookup,
		argv:     hintTestArgv,
		ownerOf:  func(string) (int, bool) { return 1000, true },
		readable: func(string) error { return nil },
	}
}

func TestJournalOwnerHint(t *testing.T) {
	const rerun = "sudo -u vcap /var/vcap/packages/pve_cpi/bin/cpi storage-journal audit --config /var/vcap/jobs/pve_cpi/config/cpi.json"
	for _, tc := range []struct {
		name        string
		owner, euid int
		argv        []string
		want        string
	}{
		{"root running a vcap journal", 1000, 0, hintTestArgv, "owned by vcap; rerun as that user: " + rerun},
		{"another user running a vcap journal", 1000, 501, hintTestArgv, "owned by vcap; rerun as that user: " + rerun},
		{"vcap running a root journal", 0, 1000, hintTestArgv, "owned by root; rerun as that user: sudo -u root " + strings.TrimPrefix(rerun, "sudo -u vcap ")},
		{"the owner itself", 1000, 1000, hintTestArgv, ""},
		{"an owner without an account", 4242, 0, hintTestArgv, "owned by uid 4242; rerun as that user: sudo -u '#4242' " + strings.TrimPrefix(rerun, "sudo -u vcap ")},
		{"no ownership metadata", -1, 0, hintTestArgv, ""},
		{"no command to repeat", 1000, 0, nil, ""},
		{"arguments that need quoting", 1000, 0, []string{"/opt/bosh cpi/cpi", "storage-journal", "audit", "--config", "/home/op/it's.json"}, `owned by vcap; rerun as that user: sudo -u vcap '/opt/bosh cpi/cpi' storage-journal audit --config '/home/op/it'\''s.json'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := journalOwnerHint(tc.owner, tc.euid, hintTestLookup, tc.argv); got != tc.want {
				t.Fatalf("hint = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDescribeStorageJournalError(t *testing.T) {
	var syntax *json.SyntaxError
	if err := json.Unmarshal([]byte("corrupt-secret"), &struct{}{}); !errors.As(err, &syntax) {
		t.Fatalf("fixture is not a syntax error: %v", err)
	}
	corrupt := fmt.Errorf("%w: malformed envelope: %w", aj.ErrCorrupt, errors.New("invalid character 'c' near corrupt-secret"))
	for _, tc := range []struct {
		name    string
		err     error
		want    string
		exclude string
	}{
		{"nil", nil, "", ""},
		{"ownership", &aj.UnsafePathError{Path: "/var/vcap/store/pve_cpi/allocations", UID: 1000, EUID: 0, Cause: aj.ErrUnsafeOwnership}, "journal: unsafe ownership: /var/vcap/store/pve_cpi/allocations is owned by uid 1000, not effective uid 0", ""},
		{"corrupt evidence keeps only the fixed lead", corrupt, "invalid journal evidence: malformed envelope", "secret"},
		{"a sentinel wrapped by a caller", fmt.Errorf("record secret-record: %w", aj.ErrAuthority), "journal authority mismatch or recovery required", "secret"},
		{"not initialized", aj.ErrNotInitialized, "journal authority is not initialized; historical provenance audit required", ""},
		{"durability", &aj.DurabilityError{Err: &fs.PathError{Op: "rename", Path: "/j/.tmp-1", Err: syscall.ENOSPC}}, "journal durability failure; reconcile before mutation (rename /j/.tmp-1: no space left on device)", ""},
		{"a path error with an errno", &fs.PathError{Op: "open", Path: "/j/authority.json", Err: syscall.EACCES}, "open /j/authority.json: permission denied", ""},
		{"a path error without an errno", &fs.PathError{Op: "open", Path: "/j/x", Err: errors.New("secret transport text")}, "open /j/x failed", "secret"},
		{"a journal fixed text", errors.Join(errors.New("journal: directory changed while opening"), errors.New("secret close text")), "journal: directory changed while opening", "secret"},
		{"a provisioning fixed text", errors.New("journal provisioning: parent directory permits untrusted replacement"), "journal provisioning: parent directory permits untrusted replacement", ""},
		{"a JSON syntax error", syntax, "JSON syntax error at byte offset 1", "secret"},
		{"a JSON type error", &json.UnmarshalTypeError{Value: "number", Field: "storage_allocation_journal_dir"}, "JSON field storage_allocation_journal_dir has the wrong type (number)", ""},
		{"a CLI fixed error with a PVE cause", fmt.Errorf("%w: %w", storageJournalFixedError("PVE node enumeration failed"), &sdkerrors.APIError{HTTPCode: 596, Message: "tls handshake failed"}), "PVE node enumeration failed (HTTP 596: tls handshake failed)", ""},
		{"a CLI fixed error alone", storageJournalFixedError("PVE listed no nodes"), "PVE listed no nodes", ""},
		{"a PVE API error", &sdkerrors.APIError{HTTPCode: 403, Message: "Permission check failed"}, "HTTP 403: Permission check failed", ""},
		{"an unknown user", user.UnknownUserError("vcap"), "unknown user vcap", ""},
		{"anything else", errors.New("transport response secret-password"), "unclassified error", "secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := describeStorageJournalError(tc.err)
			if got != tc.want {
				t.Fatalf("description = %q, want %q", got, tc.want)
			}
			if tc.exclude != "" && strings.Contains(got, tc.exclude) {
				t.Fatalf("description %q leaked %q", got, tc.exclude)
			}
		})
	}
}

func TestStorageJournalFailAppendsTheDescription(t *testing.T) {
	var stderr bytes.Buffer
	storageJournalFail(&stderr, "retained journal audit failed", &sdkerrors.APIError{HTTPCode: 500, Message: "cfs lock timeout"})
	if got := stderr.String(); got != "retained journal audit failed: HTTP 500: cfs lock timeout\n" {
		t.Fatalf("failure line = %q", got)
	}
	stderr.Reset()
	storageJournalFail(&stderr, "audit output could not be written", nil)
	if got := stderr.String(); got != "audit output could not be written\n" {
		t.Fatalf("failure line without an error = %q", got)
	}
}

func TestStorageJournalAccessMessageTellsEnrollmentFailuresApart(t *testing.T) {
	base := provisionTemp(t)
	provisioned := filepath.Join(base, "allocations")
	if err := os.Mkdir(provisioned, 0o700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(base, "never-provisioned")
	_, missingBaseErr := aj.InspectEnrollment(missing, "director")
	_, missingNamespaceErr := aj.InspectEnrollment(provisioned, "director")
	for _, tc := range []struct {
		name string
		dir  string
		err  error
		host storageJournalHost
		want string
	}{
		{"missing base directory", missing, missingBaseErr, hintTestHost(0), "journal directory " + missing + " is missing; run provision-journal"},
		{"missing namespace directory", provisioned, missingNamespaceErr, hintTestHost(0), "namespace director is not enrolled in " + provisioned + "; run audit-enrollment, then initialize"},
		{"missing authority", provisioned, aj.ErrNotInitialized, hintTestHost(0), "namespace director is not enrolled in " + provisioned + "; run audit-enrollment, then initialize"},
		{"owned by another user", provisioned, &aj.UnsafePathError{Path: provisioned, UID: 1000, EUID: 0, Cause: aj.ErrUnsafeOwnership}, hintTestHost(0),
			"journal " + provisioned + " is owned by vcap; rerun as that user: sudo -u vcap " + strings.Join(hintTestArgv, " ")},
		{"access denied", provisioned, &fs.PathError{Op: "lstat", Path: provisioned, Err: syscall.EACCES}, hintTestHost(501),
			"journal " + provisioned + " is owned by vcap; rerun as that user: sudo -u vcap " + strings.Join(hintTestArgv, " ")},
		{"access denied by an unknown owner", provisioned, &fs.PathError{Op: "lstat", Path: provisioned, Err: syscall.EACCES},
			storageJournalHost{euid: 501, lookup: hintTestLookup, argv: hintTestArgv, ownerOf: func(string) (int, bool) { return -1, false }},
			"journal " + provisioned + " is not accessible to operator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := storageJournalAccessMessage(tc.dir, "director", tc.err, tc.host)
			if !ok || got != tc.want {
				t.Fatalf("message = %q (%t), want %q", got, ok, tc.want)
			}
		})
	}
	for _, err := range []error{
		fmt.Errorf("%w: invalid authority enrollment", aj.ErrCorrupt),
		aj.ErrAuthority,
		&aj.UnsafePathError{Path: provisioned, UID: 0, EUID: 0, Mode: os.ModeDir | 0o755, WantDirectory: true, Cause: aj.ErrUnsafeMode},
		&aj.UnsafePathError{Path: provisioned, UID: 1000, EUID: 1000, Cause: aj.ErrUnsafeOwnership},
	} {
		if got, ok := storageJournalAccessMessage(provisioned, "director", err, hintTestHost(0)); ok {
			t.Fatalf("%v classified as an access failure: %q", err, got)
		}
	}
}

func TestStorageJournalConfigProblem(t *testing.T) {
	base := provisionTemp(t)
	present := filepath.Join(base, "cpi.json")
	if err := os.WriteFile(present, []byte(`{"password":"must-not-print"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(base, "absent.json")
	denied := func(path string) error { return &fs.PathError{Op: "open", Path: path, Err: syscall.EACCES} }
	for _, tc := range []struct {
		name string
		path string
		host storageJournalHost
		want string
	}{
		{"readable", present, hintTestHost(1000), ""},
		{"missing", missing, hintTestHost(0), "config " + missing + " not found"},
		{"unreadable by another user", present, storageJournalHost{euid: 501, lookup: hintTestLookup, argv: hintTestArgv, ownerOf: func(string) (int, bool) { return 1000, true }, readable: denied},
			"config " + present + " is not readable by operator; it is owned by vcap; rerun as that user: sudo -u vcap " + strings.Join(hintTestArgv, " ")},
		{"root-owned and readable by the vcap group", present, storageJournalHost{euid: 501, lookup: hintTestLookup, argv: hintTestArgv, ownerOf: func(string) (int, bool) { return 0, true }, groupReader: func(string) (int, bool) { return 1000, true }, readable: denied},
			"config " + present + " is not readable by operator; it is readable by group vcap; rerun as that user: sudo -u vcap " + strings.Join(hintTestArgv, " ")},
		{"unreadable by its owner", present, storageJournalHost{euid: 1000, lookup: hintTestLookup, argv: hintTestArgv, ownerOf: func(string) (int, bool) { return 1000, true }, readable: denied},
			"config " + present + " is not readable by vcap"},
		{"a directory", base, storageJournalHost{euid: 1000, lookup: hintTestLookup, argv: hintTestArgv, ownerOf: func(string) (int, bool) { return 1000, true }, readable: func(string) error { return nil }},
			"config " + base + " is not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := storageJournalConfigProblem(tc.path, tc.host)
			if got != tc.want {
				t.Fatalf("problem = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "must-not-print") {
				t.Fatal("config pre-check printed a configuration value")
			}
		})
	}
}
