package agent

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// deriveMBusFromBlobstore returns a NATS URL derived from the host portion of
// the blobstore endpoint when both NATS and the DAV blobstore are colocated on
// the same machine (the typical BOSH topology — director VM in normal
// deployments, the create-env machine during bootstrap).
//
// It returns "" when:
//   - bs.Options is nil or has no string "endpoint" entry.
//   - The endpoint cannot be parsed as a URL.
//   - The endpoint hostname is empty, a literal "localhost", or any IP that
//     reports IsLoopback() or IsUnspecified() (this covers 127.0.0.0/8,
//     ::1, 0.0.0.0, ::, and IPv4-mapped IPv6 loopback like ::ffff:127.0.0.1).
//
// On every failure path the caller is expected to leave MBus empty rather than
// synthesize a non-routable URL — a loud "agent never connects" failure beats
// a silent misroute.
func deriveMBusFromBlobstore(bs BlobstoreSpec) string {
	if bs.Options == nil {
		return ""
	}
	endpoint, ok := bs.Options["endpoint"].(string)
	if !ok || endpoint == "" {
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if host == "" {
		return ""
	}
	if strings.EqualFold(host, "localhost") {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		// IsLoopback handles 127.0.0.0/8, ::1, and the IPv4-mapped form
		// ::ffff:127.0.0.1 (net.IP.IsLoopback unwraps via To4 internally).
		if ip.IsLoopback() || ip.IsUnspecified() {
			return ""
		}
	}
	return fmt.Sprintf("nats://%s:4222", host)
}
