package endpointcache

import (
	"regexp"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/privatefile"
)

const (
	// HAProfileName is the display name of the HA profile cache. A store for
	// that file passes it as StoreOptions.DisplayName, and the directory choice
	// passes it as Options.DisplayName, so every message about the file names it.
	HAProfileName = "HA profile cache"

	// maxDisplayName bounds a display name.
	maxDisplayName = 64
)

// displayNamePattern is the shape of a display name: words of letters and
// digits joined by single spaces or hyphens. It keeps a name from carrying a
// line break or a colon into a log line.
var displayNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*(?:[ -][A-Za-z0-9]+)*$`)

// validDisplayName reports whether name may start a message. The empty name is
// valid and means the default.
func validDisplayName(name string) bool {
	return name == "" || (len(name) <= maxDisplayName && displayNamePattern.MatchString(name))
}

// texts holds the causes and private-file policy that name one kind of store
// in the messages of its lock files, directory secret, directory, and files.
// The endpoint cache's texts are the package's own sentinels, so its messages
// are the ones the package has always printed. Another kind's sentinels print
// its name in place of the endpoint cache's and match the endpoint ones under
// errors.Is, so a caller that tests a cause does not care which kind met it.
type texts struct {
	// name is what the messages call the store.
	name string

	// ownership, mode, and fileType are the unsafe path causes.
	ownership, mode, fileType error
	// lockReplaced, lockAbandoned, lockBusy, and secretBusy are the lock causes.
	lockReplaced, lockAbandoned, lockBusy, secretBusy error
	// secretLength is the directory secret's wrong length cause.
	secretLength error
	// invalid, keyMismatch, and unsupported are the file's read failures.
	invalid, keyMismatch, unsupported error

	// policy checks the directory, the lock files, and, with its limits
	// extended, the secret and the cache file.
	policy privatefile.Policy

	// judged and judgedUID name the user a probe judges as, which the policy
	// also carries. Options.texts sets them.
	judged    bool
	judgedUID int
}

// endpointTexts are the endpoint cache's texts.
var endpointTexts = &texts{
	name:          cacheLabel,
	ownership:     errCacheOwnership,
	mode:          errCacheMode,
	fileType:      errCacheFileType,
	lockReplaced:  ErrLockReplaced,
	lockAbandoned: ErrLockWaitAbandoned,
	lockBusy:      errLockBusy,
	secretBusy:    ErrSecretBusy,
	secretLength:  errSecretLength,
	invalid:       ErrFileInvalid,
	keyMismatch:   ErrKeyMismatch,
	unsupported:   ErrUnsupportedVersion,
	policy:        cachePrivatePolicy,
}

// textsFor returns the texts of the store called name. The empty name, the
// endpoint cache's own name, and a name that is not a valid display name give
// the endpoint cache's texts.
func textsFor(name string) *texts {
	if name == "" || name == cacheLabel || !validDisplayName(name) {
		return endpointTexts
	}
	t := &texts{
		name:          name,
		ownership:     renamed(name, errCacheOwnership),
		mode:          renamed(name, errCacheMode),
		fileType:      renamed(name, errCacheFileType),
		lockReplaced:  renamed(name, ErrLockReplaced),
		lockAbandoned: renamed(name, ErrLockWaitAbandoned),
		lockBusy:      renamed(name, errLockBusy),
		secretBusy:    renamed(name, ErrSecretBusy),
		secretLength:  renamed(name, errSecretLength),
		invalid:       renamed(name, ErrFileInvalid),
		keyMismatch:   renamed(name, ErrKeyMismatch),
		unsupported:   renamed(name, ErrUnsupportedVersion),
	}
	t.policy = cachePrivatePolicy
	t.policy.Label = name
	t.policy.Ownership = t.ownership
	t.policy.Mode = t.mode
	t.policy.FileType = t.fileType
	return t
}

// renamed returns an error that prints base's text with name in place of the
// endpoint cache's label and matches base under errors.Is. Taking the text
// from base keeps one wording for both kinds of store.
func renamed(name string, base error) error {
	return privatefile.Sentinel(name+strings.TrimPrefix(base.Error(), cacheLabel), base)
}
