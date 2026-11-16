package spreadclaim

import (
	"os"
	"strings"
	"sync"
)

// hostBootIdentity names this boot of the host and this process's PID
// namespace, as "<boot id>/<pid namespace>", read once. Two claims with the
// same identity were made by processes that share a PID space, so a PID one
// of them records can be checked by the other. A host that reports no boot id,
// such as macOS, gets "", which turns the check off, and so does a process
// that cannot read its PID namespace, since a made-up namespace could match
// another process's that differs.
var hostBootIdentity = sync.OnceValue(func() string {
	return bootIdentity(os.ReadFile, os.Readlink)
})

// bootIdentity is hostBootIdentity's read, through readFile and readlink.
func bootIdentity(readFile func(string) ([]byte, error), readlink func(string) (string, error)) string {
	raw, err := readFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	boot := strings.TrimSpace(string(raw))
	if boot == "" {
		return ""
	}
	ns, err := readlink("/proc/self/ns/pid")
	if err != nil || ns == "" {
		return ""
	}
	return boot + "/" + ns
}
