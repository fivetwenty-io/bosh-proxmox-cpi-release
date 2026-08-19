package auth

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

var (
	ErrInvalidAPIToken         = errors.New("invalid API token")
	ErrEmptyTokenString        = errors.New("empty token string")
	ErrInvalidTokenFormat      = errors.New("invalid token format")
	ErrInvalidTokenIDFormat    = errors.New("invalid token ID format")
	ErrTokenSecretEmpty        = errors.New("token secret cannot be empty")
	ErrTokenIDEmpty            = errors.New("token ID cannot be empty")
	ErrTokenIDMissingUserRealm = errors.New("token ID must contain user@realm")
	ErrTokenIDMissingTokenID   = errors.New("token ID must contain !tokenid after realm")
	ErrUserPartEmpty           = errors.New("user part cannot be empty")
	ErrRealmPartEmpty          = errors.New("realm part cannot be empty")
	ErrTokenNamePartEmpty      = errors.New("token name part cannot be empty")
)

// tokenFormatRegex matches already-formatted authorization headers (e.g., "PVEAPIToken=..." or "Bearer ...").
// Pattern: word characters followed by = or space.
var tokenFormatRegex = regexp.MustCompile(`^\w+(?:=| )`)

// APITokenAuthenticator provides API token-based authentication for PVE.
//
// It is safe for concurrent use: the token may be replaced via SetToken while
// other goroutines read it through GetToken/GetHeaders, mirroring the locking
// discipline of TicketAuthenticator.
type APITokenAuthenticator struct {
	mu        sync.RWMutex
	token     *Token
	tokenName string // Name prefix for Authorization header (default: "PVEAPIToken")
}

// NewAPITokenAuthenticator creates a new API token authenticator.
// The tokenName parameter specifies the prefix for the Authorization header.
// If empty, defaults to "PVEAPIToken".
func NewAPITokenAuthenticator(token *Token, tokenName string) *APITokenAuthenticator {
	if tokenName == "" {
		tokenName = "PVEAPIToken"
	}

	return &APITokenAuthenticator{
		token:     token,
		tokenName: tokenName,
	}
}

// NewAPITokenAuthenticatorFromString creates a new API token authenticator from a string.
// The token string may be in the format "user@realm!tokenid=secret" (PVE) or
// "user@realm!tokenid:secret" (PBS/PDM); see ParseAPIToken.
// Uses default token name "PVEAPIToken".
func NewAPITokenAuthenticatorFromString(tokenString string) (*APITokenAuthenticator, error) {
	token, err := ParseAPIToken(tokenString)
	if err != nil {
		return nil, err
	}

	return NewAPITokenAuthenticator(token, ""), nil
}

// Authenticate performs the authentication process.
// For API tokens, this is a no-op as tokens don't require login.
func (ata *APITokenAuthenticator) Authenticate() error {
	ata.mu.RLock()
	defer ata.mu.RUnlock()

	if !ata.hasValidTokenLocked() {
		return ErrInvalidAPIToken
	}

	return nil
}

// IsAuthenticated checks if the authenticator has a valid token.
func (ata *APITokenAuthenticator) IsAuthenticated() bool {
	ata.mu.RLock()
	defer ata.mu.RUnlock()

	return ata.hasValidTokenLocked()
}

// GetHeaders returns the authentication headers for API token auth.
// Transparently adds the token name prefix if not already present in the format.
func (ata *APITokenAuthenticator) GetHeaders() map[string]string {
	ata.mu.RLock()
	defer ata.mu.RUnlock()

	if !ata.hasValidTokenLocked() {
		return nil
	}

	// Build the authorization header value
	authHeader := fmt.Sprintf("%s%s%s", ata.token.ID, secretSeparator(ata.tokenName), ata.token.Secret)

	// Check if the token is already formatted (starts with word char followed by = or space)
	// This matches Perl implementation behavior: only add prefix if not already present
	if !tokenFormatRegex.MatchString(authHeader) {
		authHeader = fmt.Sprintf("%s=%s", ata.tokenName, authHeader)
	}

	return map[string]string{
		"Authorization": authHeader,
	}
}

// secretSeparator returns the character joining token ID and secret in the
// Authorization header value. The Perl-based PVE API expects
// "user@realm!tokenid=secret"; the Rust-based products (PBS, PDM) split the
// value on ':' (proxmox-auth-api reads the token as TOKENID:TOKENSECRET), so
// their token names select the colon form. Unknown token names keep the PVE
// form for backward compatibility.
func secretSeparator(tokenName string) string {
	switch tokenName {
	case "PBSAPIToken", "PDMAPIToken":
		return ":"
	default:
		return "="
	}
}

// Refresh is a no-op for API tokens as they don't expire.
func (ata *APITokenAuthenticator) Refresh() error {
	return nil
}

// Logout is a no-op for API tokens.
func (ata *APITokenAuthenticator) Logout() error {
	return nil
}

// GetToken returns the API token.
func (ata *APITokenAuthenticator) GetToken() *Token {
	ata.mu.RLock()
	defer ata.mu.RUnlock()

	return ata.token
}

// SetToken sets the API token.
func (ata *APITokenAuthenticator) SetToken(token *Token) {
	ata.mu.Lock()
	defer ata.mu.Unlock()

	ata.token = token
}

// hasValidTokenLocked reports whether a usable token is present. The caller must
// hold at least the read lock.
func (ata *APITokenAuthenticator) hasValidTokenLocked() bool {
	return ata.token != nil && ata.token.ID != "" && ata.token.Secret != ""
}

// ParseAPIToken parses an API token string into a Token struct.
// Accepts both "user@realm!tokenid=secret" (PVE) and
// "user@realm!tokenid:secret" (PBS/PDM) forms.
func ParseAPIToken(tokenString string) (*Token, error) {
	if tokenString == "" {
		return nil, ErrEmptyTokenString
	}

	// Split on the first '=' (PVE form) or ':' (PBS/PDM form), whichever
	// comes first, to separate ID and secret.
	sep := strings.IndexAny(tokenString, "=:")
	if sep < 0 {
		return nil, fmt.Errorf("%w: expected 'user@realm!tokenid=secret' or 'user@realm!tokenid:secret'", ErrInvalidTokenFormat)
	}

	tokenID := tokenString[:sep]
	secret := tokenString[sep+1:]

	// Validate token ID format (should contain @ and !)
	if !strings.Contains(tokenID, "@") || !strings.Contains(tokenID, "!") {
		return nil, fmt.Errorf("%w: expected 'user@realm!tokenid'", ErrInvalidTokenIDFormat)
	}

	// Validate secret is not empty
	if secret == "" {
		return nil, ErrTokenSecretEmpty
	}

	return &Token{
		ID:     tokenID,
		Secret: secret,
	}, nil
}

// FormatAPIToken formats a Token into a string using the "=" separator, the
// canonical internal/PVE representation. The result round-trips through
// ParseAPIToken, which also accepts the ":" separator PBS/PDM use.
func FormatAPIToken(token *Token) string {
	if token == nil {
		return ""
	}

	return fmt.Sprintf("%s=%s", token.ID, token.Secret)
}

// ValidateTokenID validates the format of a token ID.
// Valid format: user@realm!tokenid.
func ValidateTokenID(tokenID string) error {
	if tokenID == "" {
		return ErrTokenIDEmpty
	}

	// Check for @ sign (user@realm)
	atIndex := strings.Index(tokenID, "@")
	if atIndex < 1 {
		return ErrTokenIDMissingUserRealm
	}

	// Check for ! sign (realm!tokenid)
	exclamIndex := strings.Index(tokenID, "!")
	if exclamIndex <= atIndex {
		return ErrTokenIDMissingTokenID
	}

	// Extract parts
	user := tokenID[:atIndex]
	realm := tokenID[atIndex+1 : exclamIndex]
	tokenName := tokenID[exclamIndex+1:]

	// Validate parts
	if user == "" {
		return ErrUserPartEmpty
	}

	if realm == "" {
		return ErrRealmPartEmpty
	}

	if tokenName == "" {
		return ErrTokenNamePartEmpty
	}

	return nil
}
