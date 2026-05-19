package auth

// InvalidAuthenticator is an Authenticator that always returns a fixed error.
// Used when authentication configuration is malformed at construction time
// so the error surfaces on first use rather than changing the NewClient signature.
type InvalidAuthenticator struct {
	err error
}

// NewInvalidAuthenticator returns an Authenticator that always fails with err.
// err must be non-nil.
func NewInvalidAuthenticator(err error) *InvalidAuthenticator {
	return &InvalidAuthenticator{err: err}
}

// Authenticate always returns the construction-time error.
func (a *InvalidAuthenticator) Authenticate() error {
	return a.err
}

// IsAuthenticated always returns false.
func (a *InvalidAuthenticator) IsAuthenticated() bool {
	return false
}

// GetHeaders always returns nil — no valid credentials exist.
func (a *InvalidAuthenticator) GetHeaders() map[string]string {
	return nil
}

// Refresh always returns the construction-time error.
func (a *InvalidAuthenticator) Refresh() error {
	return a.err
}

// Logout is a no-op because no session was ever established.
func (a *InvalidAuthenticator) Logout() error {
	return nil
}
