package spreadclaim

import (
	"os"
	"strings"
	"sync"
)

// hostBootIdentity names this boot of the host and this process's PID
// namespace, as "<boot id>/<pid namespace>", read once. Two claims with the
// same identity were made by processes that share a PID space, so a PID one
// of them records can be checked by the other. The boot id changes on every
// boot, so a claim from an earlier boot names a process that is gone. A host
// that reports no boot id, such as macOS, gets "", which turns the check off.
var hostBootIdentity = sync.OnceValue(func() string {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	boot := strings.TrimSpace(string(raw))
	if boot == "" {
		return ""
	}
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		ns = "?"
	}
	return boot + "/" + ns
})

// bootID returns the boot id part of a boot identity.
func bootID(identity string) string {
	boot, _, _ := strings.Cut(identity, "/")
	return boot
}
