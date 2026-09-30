package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// storageJournalDescriptionLimit caps one rendered error description in
// bytes, matching the limit the allocation audit applies to its findings.
const storageJournalDescriptionLimit = 200

// storageJournalFixedError is an error whose text this package wrote and
// which embeds nothing it read, so the CLI may print it as it is.
type storageJournalFixedError string

func (e storageJournalFixedError) Error() string { return string(e) }

// storageJournalSentinels are the journal's own error classes. Each renders
// as its fixed text, followed by the fixed detail the journal put after it.
var storageJournalSentinels = []error{
	aj.ErrNotInitialized, aj.ErrConflict, aj.ErrReconciliationRequired,
	aj.ErrAuthority, aj.ErrCorrupt, aj.ErrClosed,
}

// storageJournalFail prints message and, when err is not nil, a description of
// err that is safe to show. It never prints err.Error() for an error it does
// not recognise, because transport and decode errors can carry response
// bodies, file content, and credentials.
func storageJournalFail(stderr io.Writer, message string, err error) {
	if detail := describeStorageJournalError(err); detail != "" {
		message += ": " + detail
	}
	fmt.Fprintln(stderr, message)
}

// describeStorageJournalError renders the journal, filesystem, decode, and
// CLI errors the storage-journal commands meet, and hands everything else to
// pve.DescribeAuditError. The result is scrubbed, flattened to one line, and
// capped in length.
func describeStorageJournalError(err error) string {
	if err == nil {
		return ""
	}
	return boundStorageJournalText(storageJournalErrorText(err))
}

//nolint:gocyclo // One flat classification table reads better than nested helpers.
func storageJournalErrorText(err error) string {
	var unsafe *aj.UnsafePathError
	if errors.As(err, &unsafe) {
		return unsafe.Error()
	}
	var durability *aj.DurabilityError
	if errors.As(err, &durability) {
		return "journal durability failure; reconcile before mutation (" + storageJournalErrorText(durability.Err) + ")"
	}
	for _, sentinel := range storageJournalSentinels {
		if errors.Is(err, sentinel) {
			return storageJournalLead(err, sentinel.Error())
		}
	}
	if errors.Is(err, pve.ErrStorageClusterIdentity) {
		return storageJournalWithCause(storageJournalIdentityText(err), err)
	}
	var fixed storageJournalFixedError
	if errors.As(err, &fixed) {
		return storageJournalWithCause(string(fixed), err)
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return fmt.Sprintf("JSON syntax error at byte offset %d", syntax.Offset)
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return fmt.Sprintf("JSON field %s has the wrong type (%s)", typeErr.Field, typeErr.Value)
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		var errno syscall.Errno
		if errors.As(pathErr.Err, &errno) {
			return pathErr.Op + " " + pathErr.Path + ": " + errno.Error()
		}
		return pathErr.Op + " " + pathErr.Path + " failed"
	}
	var unknownUser user.UnknownUserError
	if errors.As(err, &unknownUser) {
		return "unknown user " + string(unknownUser)
	}
	var numErr *strconv.NumError
	if errors.As(err, &numErr) {
		return "not a decimal number"
	}
	for _, prefix := range []string{"journal: ", "journal provisioning: "} {
		if strings.HasPrefix(err.Error(), prefix) {
			return storageJournalLead(err, strings.TrimSuffix(prefix, ": "))
		}
	}
	return pve.DescribeAuditError(err)
}

// storageJournalLead returns the fixed lead of a journal error: lead itself,
// followed by the next segment of the error's first line when the text
// starts with lead. The journal formats its errors as "<fixed>: <fixed
// detail>: <wrapped cause>", so the segment after lead is still text the
// journal wrote, while the wrapped cause may carry file content. When the
// text does not start with lead, a caller wrapped the error, and only lead
// is safe to show.
func storageJournalLead(err error, lead string) string {
	text, _, _ := strings.Cut(err.Error(), "\n")
	rest, ok := strings.CutPrefix(text, lead)
	if !ok {
		return lead
	}
	detail, found := strings.CutPrefix(rest, ": ")
	if !found || detail == "" {
		return lead
	}
	detail, _, _ = strings.Cut(detail, ": ")
	return lead + ": " + detail
}

// storageJournalIdentityText keeps the lines of a cluster identity error.
// Those errors carry only node names and fixed reasons, never PVE response
// text, and a failure on several nodes joins one line per node.
func storageJournalIdentityText(err error) string {
	var lines []string
	for line := range strings.SplitSeq(err.Error(), "\n") {
		if !strings.HasPrefix(line, "storage cluster identity: ") {
			return "storage cluster identity unverified"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "; ")
}

// storageJournalWithCause appends the PVE cause beneath err when
// pve.DescribeAuditError can name one.
func storageJournalWithCause(text string, err error) string {
	if cause := pve.DescribeAuditError(err); cause != "unclassified error" {
		return text + " (" + cause + ")"
	}
	return text
}

// boundStorageJournalText scrubs, flattens, and caps a rendered description,
// in that order, so a cut can never split a credential the scrubber would
// otherwise have matched.
func boundStorageJournalText(s string) string {
	s = storageJournalLine(log.ScrubMessage(s))
	if len(s) <= storageJournalDescriptionLimit {
		return s
	}
	cut := storageJournalDescriptionLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// storageJournalLine makes s safe to print as one line: it replaces invalid
// UTF-8 and every control character, including line breaks and terminal
// escapes, and trims the ends.
func storageJournalLine(s string) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// storageJournalHost is what the CLI knows about the user it runs as and the
// command it was given, so that a permission failure can name the command to
// rerun as the right user. Tests supply their own, because CI runs as root and
// cannot give a file to another owner portably.
type storageJournalHost struct {
	euid    int
	lookup  func(uid string) (*user.User, error)
	argv    []string
	ownerOf func(path string) (int, bool)
	// groupReader returns the UID of the account named like the group of a
	// group-readable path. BOSH renders job config root-owned and readable by
	// the vcap group, so that account, not the root owner, is the one to name.
	groupReader func(path string) (int, bool)
	readable    func(path string) error
}

func newStorageJournalHost(args []string) storageJournalHost {
	executable, err := storageAuditExecutable()
	if err != nil || executable == "" {
		executable = "cpi"
	}
	return storageJournalHost{
		euid:        os.Geteuid(),
		lookup:      user.LookupId,
		argv:        append([]string{executable, "storage-journal"}, args...),
		ownerOf:     storageJournalPathOwner,
		groupReader: storageJournalGroupReader,
		readable:    storageJournalReadable,
	}
}

// storageJournalPathOwner returns the owner of path or, when path cannot be
// examined, of its nearest ancestor that can. A permission failure on a
// journal usually comes from a private parent directory, and that parent
// belongs to the journal's owner.
func storageJournalPathOwner(path string) (int, bool) {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		if info, err := os.Stat(current); err == nil {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return -1, false
			}
			return int(stat.Uid), true
		}
		if filepath.Dir(current) == current {
			return -1, false
		}
	}
}

// storageJournalGroupReader returns the UID of the account whose name matches
// the group of path, when that group may read path. It reports false when the
// group cannot read path or no account carries the group's name.
func storageJournalGroupReader(path string) (int, bool) {
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o040 == 0 {
		return -1, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, false
	}
	group, err := user.LookupGroupId(strconv.FormatUint(uint64(stat.Gid), 10))
	if err != nil {
		return -1, false
	}
	account, err := user.Lookup(group.Name)
	if err != nil {
		return -1, false
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return -1, false
	}
	return uid, true
}

func storageJournalReadable(path string) error {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied CLI config path; opened only to test access
	if err != nil {
		return err
	}
	return f.Close()
}

// journalOwnerHint names the user that owns a journal path and the command
// that reruns this invocation as that user. It returns "" when the effective
// UID already is the owner, when the owner is unknown, or when there is no
// command to repeat. It fires for any other user, not only root, because an
// operator's own account is as unable to open a vcap journal as root is.
func journalOwnerHint(dirUID, euid int, lookup func(string) (*user.User, error), argv []string) string {
	return storageJournalRerunHint("owned by", dirUID, euid, lookup, argv)
}

// storageJournalRerunHint says how uid relates to a path and gives the command
// that reruns this invocation as uid, or "" when there is nothing to rerun.
func storageJournalRerunHint(relation string, uid, euid int, lookup func(string) (*user.User, error), argv []string) string {
	if uid < 0 || uid == euid || len(argv) == 0 {
		return ""
	}
	name, target := fmt.Sprintf("uid %d", uid), fmt.Sprintf("#%d", uid)
	if account, err := lookup(strconv.Itoa(uid)); err == nil && account != nil && account.Username != "" {
		name, target = account.Username, account.Username
	}
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = shellQuoteArg(arg)
	}
	return fmt.Sprintf("%s %s; rerun as that user: sudo -u %s %s", relation, name, shellQuoteArg(target), strings.Join(quoted, " "))
}

func storageJournalUserName(uid int, lookup func(string) (*user.User, error)) string {
	if account, err := lookup(strconv.Itoa(uid)); err == nil && account != nil && account.Username != "" {
		return account.Username
	}
	return fmt.Sprintf("uid %d", uid)
}

// storageJournalAccessMessage names why the journal at dir could not be
// opened for namespace, when the reason is one an operator acts on without
// reading the error: the directory was never provisioned, the namespace was
// never enrolled, or the journal belongs to another user. It reports false
// for every other failure, which the caller then describes.
func storageJournalAccessMessage(dir, namespace string, err error, host storageJournalHost) (string, bool) {
	var unsafe *aj.UnsafePathError
	if errors.As(err, &unsafe) && errors.Is(unsafe.Cause, aj.ErrUnsafeOwnership) {
		if hint := journalOwnerHint(unsafe.UID, unsafe.EUID, host.lookup, host.argv); hint != "" {
			return fmt.Sprintf("journal %s is %s", unsafe.Path, hint), true
		}
		return "", false
	}
	if errors.Is(err, fs.ErrPermission) {
		if owner, ok := host.ownerOf(dir); ok {
			if hint := journalOwnerHint(owner, host.euid, host.lookup, host.argv); hint != "" {
				return fmt.Sprintf("journal %s is %s", dir, hint), true
			}
		}
		return fmt.Sprintf("journal %s is not accessible to %s", dir, storageJournalUserName(host.euid, host.lookup)), true
	}
	if errors.Is(err, fs.ErrNotExist) {
		if _, statErr := os.Lstat(dir); errors.Is(statErr, fs.ErrNotExist) {
			return fmt.Sprintf("journal directory %s is missing; run provision-journal", dir), true
		}
		return storageJournalNotEnrolled(dir, namespace), true
	}
	if errors.Is(err, aj.ErrNotInitialized) {
		return storageJournalNotEnrolled(dir, namespace), true
	}
	return "", false
}

func storageJournalNotEnrolled(dir, namespace string) string {
	return fmt.Sprintf("namespace %s is not enrolled in %s; run audit-enrollment, then initialize", namespace, dir)
}

// storageJournalOpenFailure prints the access message for err when there is
// one, and otherwise message with a description of err.
func storageJournalOpenFailure(stderr io.Writer, message, dir, namespace string, err error, host storageJournalHost) {
	if access, ok := storageJournalAccessMessage(dir, namespace, err, host); ok {
		fmt.Fprintln(stderr, access)
		return
	}
	storageJournalFail(stderr, message, err)
}

// storageJournalConfigProblem reports why the CLI cannot read the config at
// path, or "" when it can. It reads only file metadata and access, never the
// content, because configuration errors can echo configuration values. A
// rendered job config is not world-readable, so an operator who is neither
// root nor the CPI's user fails here first, and the owner hint fires here too.
func storageJournalConfigProblem(path string, host storageJournalHost) string {
	info, err := os.Stat(path)
	if err == nil && !info.Mode().IsRegular() {
		return fmt.Sprintf("config %s is not a regular file", path)
	}
	if err == nil {
		err = host.readable(path)
	}
	switch {
	case err == nil:
		return ""
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("config %s not found", path)
	case errors.Is(err, fs.ErrPermission):
		message := fmt.Sprintf("config %s is not readable by %s", path, storageJournalUserName(host.euid, host.lookup))
		if host.groupReader != nil {
			if reader, ok := host.groupReader(path); ok {
				if hint := storageJournalRerunHint("readable by group", reader, host.euid, host.lookup, host.argv); hint != "" {
					return message + "; it is " + hint
				}
			}
		}
		if owner, ok := host.ownerOf(path); ok {
			if hint := journalOwnerHint(owner, host.euid, host.lookup, host.argv); hint != "" {
				message += "; it is " + hint
			}
		}
		return message
	default:
		return fmt.Sprintf("config %s could not be read: %s", path, describeStorageJournalError(err))
	}
}
