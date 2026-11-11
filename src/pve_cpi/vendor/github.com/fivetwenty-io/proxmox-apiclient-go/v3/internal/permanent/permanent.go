// Package permanent marks errors that a retry loop must not repeat. The HTTP
// client, the pool, and the WebSocket client share the marker, so an error one
// of them refuses on purpose stops all of them.
package permanent

// marker is the method an error carries to tell a retry loop that repeating
// the request cannot succeed. Mark adds it to any error, and an error type of
// a caller's own can implement it directly.
type marker interface {
	Permanent() bool
}

// markedError wraps an error with the permanent marker.
type markedError struct {
	err error
}

func (e *markedError) Error() string { return e.err.Error() }

func (e *markedError) Unwrap() error { return e.err }

func (e *markedError) Permanent() bool { return true }

// Mark marks err as one a retry loop must not repeat. The marker survives the
// wrapping that net/http and the retry middleware add, and errors.Is and
// errors.As still reach err. A nil err stays nil.
func Mark(err error) error {
	if err == nil {
		return nil
	}

	return &markedError{err: err}
}

// Is reports whether err, or any error it wraps, carries the permanent marker
// with a true answer. It follows both Unwrap() error and Unwrap() []error, so
// an errors.Join of several errors is permanent when any member is. A wrapper
// that answers false does not hide a true answer deeper in the chain.
func Is(err error) bool {
	if err == nil {
		return false
	}

	if m, ok := err.(marker); ok && m.Permanent() {
		return true
	}

	switch wrapped := err.(type) { //nolint:errorlint // walks the chain by hand
	case interface{ Unwrap() error }:
		return Is(wrapped.Unwrap())
	case interface{ Unwrap() []error }:
		for _, member := range wrapped.Unwrap() {
			if Is(member) {
				return true
			}
		}
	}

	return false
}
