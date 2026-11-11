package http

import (
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/internal/permanent"
)

// Permanent marks err as one the retry middleware must not retry. A
// WrapTransport wrapper returns it from RoundTrip when it refuses a request on
// purpose, because the middleware otherwise attempts a refused GET again until
// its retry count runs out. The marker survives the wrapping that net/http
// and the middleware add, and errors.Is and errors.As still reach err. A nil
// err stays nil.
func Permanent(err error) error { return permanent.Mark(err) } //nolint:wrapcheck // marks err, so it is the same error

// IsPermanent reports whether err, or an error it wraps, carries the permanent
// marker. An error type of a caller's own is permanent when it has a
// Permanent() bool method that returns true. The check follows errors.Join
// and every wrapped member, so a member that answers false does not hide one
// that answers true.
func IsPermanent(err error) bool { return permanent.Is(err) }
