package errors

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/internal/constants"
)

// Sentinel errors for HTTP status classes. Use errors.Is to test wrapped APIErrors.
var (
	// ErrUnauthorized matches any 401 response.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrForbidden matches any 403 response.
	ErrForbidden = errors.New("forbidden")
	// ErrNotFound matches any 404 response.
	ErrNotFound = errors.New("not found")
	// ErrConflict matches any 409 response.
	ErrConflict = errors.New("conflict")
	// ErrServer matches any 5xx response.
	ErrServer = errors.New("server error")
)

// APIError represents a general API error from PVE.
type APIError struct {
	Message  string            `json:"message"`
	Code     int               `json:"code"`
	Errors   map[string]string `json:"errors,omitempty"`
	File     string            `json:"file,omitempty"`
	Line     int               `json:"line,omitempty"`
	HTTPCode int               `json:"-"`

	// sentinel is the wrapped sentinel for errors.Is matching.
	sentinel error
}

// Unwrap returns the wrapped sentinel error, enabling errors.Is chains.
func (e *APIError) Unwrap() error {
	return e.sentinel
}

// Error implements the error interface.
func (e *APIError) Error() string {
	if len(e.Errors) > 0 {
		errStrs := make([]string, 0, len(e.Errors))
		for field, msg := range e.Errors {
			errStrs = append(errStrs, fmt.Sprintf("%s: %s", field, msg))
		}

		return fmt.Sprintf("%s (code: %d, errors: %s)", e.Message, e.Code, strings.Join(errStrs, ", "))
	}

	return fmt.Sprintf("%s (code: %d)", e.Message, e.Code)
}

// IsNotFound returns true if the error indicates a resource was not found.
func (e *APIError) IsNotFound() bool {
	return e.HTTPCode == constants.HTTPStatusNotFound || e.Code == constants.HTTPStatusNotFound
}

// IsUnauthorized returns true if the error indicates unauthorized access.
func (e *APIError) IsUnauthorized() bool {
	return e.HTTPCode == constants.HTTPStatusUnauthorized || e.Code == constants.HTTPStatusUnauthorized
}

// IsForbidden returns true if the error indicates forbidden access.
func (e *APIError) IsForbidden() bool {
	return e.HTTPCode == constants.HTTPStatusForbidden || e.Code == constants.HTTPStatusForbidden
}

// PermissionError represents a permission-related error.
type PermissionError struct {
	APIError

	What string `json:"what"` // What resource/action was denied
}

// Error implements the error interface.
func (e *PermissionError) Error() string {
	if e.What != "" {
		return fmt.Sprintf("permission denied for %s: %s", e.What, e.APIError.Error())
	}

	return "permission denied: " + e.APIError.Error()
}

// ParameterError represents a parameter validation error.
type ParameterError struct {
	APIError

	Usage string `json:"usage"` // Expected parameter usage
}

// Error implements the error interface.
func (e *ParameterError) Error() string {
	if e.Usage != "" {
		return fmt.Sprintf("parameter error: %s (usage: %s)", e.APIError.Error(), e.Usage)
	}

	return "parameter error: " + e.APIError.Error()
}

// AuthenticationError represents an authentication failure.
type AuthenticationError struct {
	APIError

	Realm string `json:"realm,omitempty"` // Authentication realm
	TFA   bool   `json:"tfa,omitempty"`   // Whether TFA is required
}

// Error implements the error interface.
func (e *AuthenticationError) Error() string {
	msg := "authentication failed"
	if e.Realm != "" {
		msg += " for realm " + e.Realm
	}

	if e.TFA {
		msg += " (TFA required)"
	}

	if e.Message != "" {
		msg += ": " + e.Message
	}

	return msg
}

// TFARequiredError indicates that two-factor authentication is required.
type TFARequiredError struct {
	Ticket    string   `json:"ticket"`    // Partial ticket for TFA
	Challenge string   `json:"challenge"` // TFA challenge (if any)
	Types     []string `json:"types"`     // Available TFA types
}

// Error implements the error interface.
func (e *TFARequiredError) Error() string {
	return fmt.Sprintf("two-factor authentication required (available types: %s)", strings.Join(e.Types, ", "))
}

// ConnectionError represents a connection-related error.
type ConnectionError struct {
	Host    string
	Port    int
	Message string
	Cause   error
}

// Error implements the error interface.
func (e *ConnectionError) Error() string {
	msg := fmt.Sprintf("connection to %s:%d failed", e.Host, e.Port)
	if e.Message != "" {
		msg += ": " + e.Message
	}

	if e.Cause != nil {
		msg += fmt.Sprintf(" (caused by: %v)", e.Cause)
	}

	return msg
}

// Unwrap returns the underlying error.
func (e *ConnectionError) Unwrap() error {
	return e.Cause
}

// SSLError represents an SSL/TLS related error.
type SSLError struct {
	Host        string
	Fingerprint string
	Message     string
	Cause       error
}

// Error implements the error interface.
func (e *SSLError) Error() string {
	msg := "SSL error for " + e.Host
	if e.Fingerprint != "" {
		msg += fmt.Sprintf(" (fingerprint: %s)", e.Fingerprint)
	}

	if e.Message != "" {
		msg += ": " + e.Message
	}

	if e.Cause != nil {
		msg += fmt.Sprintf(" (caused by: %v)", e.Cause)
	}

	return msg
}

// Unwrap returns the underlying error.
func (e *SSLError) Unwrap() error {
	return e.Cause
}

// TimeoutError represents a request timeout.
type TimeoutError struct {
	Operation string
	Duration  string
}

// Error implements the error interface.
func (e *TimeoutError) Error() string {
	return fmt.Sprintf("operation %s timed out after %s", e.Operation, e.Duration)
}

// ParseAPIError attempts to parse an error response into an appropriate error type.
// The returned error wraps a sentinel (ErrUnauthorized, ErrForbidden, ErrNotFound,
// ErrConflict, ErrServer) so callers can use errors.Is for status-class checks while
// still extracting full detail via errors.As(*APIError).
func ParseAPIError(statusCode int, body []byte) error {
	var apiErr APIError

	err := json.Unmarshal(body, &apiErr)
	if err != nil {
		return newGenericError(statusCode, body)
	}

	apiErr.HTTPCode = statusCode
	apiErr.sentinel = sentinelFor(statusCode)

	return dispatchByStatus(statusCode, body, apiErr)
}

// newGenericError builds an APIError for non-JSON or empty bodies.
func newGenericError(statusCode int, body []byte) *APIError {
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", statusCode)
	}

	return &APIError{
		Message:  msg,
		Code:     statusCode,
		HTTPCode: statusCode,
		sentinel: sentinelFor(statusCode),
	}
}

// dispatchByStatus returns the specialised error type for known status codes.
func dispatchByStatus(statusCode int, body []byte, apiErr APIError) error {
	switch statusCode {
	case constants.HTTPStatusUnauthorized:
		return dispatchUnauthorized(body, apiErr)
	case constants.HTTPStatusForbidden:
		return &PermissionError{APIError: apiErr}
	case constants.HTTPStatusBadRequest:
		return &ParameterError{APIError: apiErr}
	default:
		return &apiErr
	}
}

// dispatchUnauthorized returns TFARequiredError when the body contains a ticket,
// otherwise returns AuthenticationError.
func dispatchUnauthorized(body []byte, apiErr APIError) error {
	var tfaErr TFARequiredError
	if json.Unmarshal(body, &tfaErr) == nil && tfaErr.Ticket != "" {
		return &tfaErr
	}

	return &AuthenticationError{APIError: apiErr}
}

// sentinelFor maps an HTTP status code to its sentinel error.
// Returns nil for unrecognised codes.
func sentinelFor(statusCode int) error {
	switch {
	case statusCode == constants.HTTPStatusUnauthorized:
		return ErrUnauthorized
	case statusCode == constants.HTTPStatusForbidden:
		return ErrForbidden
	case statusCode == constants.HTTPStatusNotFound:
		return ErrNotFound
	case statusCode == StatusConflict:
		return ErrConflict
	case statusCode >= StatusInternalServerError && statusCode < 600:
		return ErrServer
	default:
		return nil
	}
}

// IsAPIError checks if an error is an APIError or one of its subtypes.
func IsAPIError(err error) bool {
	var (
		apiErr   *APIError
		permErr  *PermissionError
		paramErr *ParameterError
		authErr  *AuthenticationError
	)

	return errors.As(err, &apiErr) ||
		errors.As(err, &permErr) ||
		errors.As(err, &paramErr) ||
		errors.As(err, &authErr)
}

// IsConnectionError checks if an error is a ConnectionError.
func IsConnectionError(err error) bool {
	var connErr *ConnectionError

	return errors.As(err, &connErr)
}

// IsSSLError checks if an error is an SSLError.
func IsSSLError(err error) bool {
	var sslErr *SSLError

	return errors.As(err, &sslErr)
}

// IsTimeoutError checks if an error is a TimeoutError.
func IsTimeoutError(err error) bool {
	var timeoutErr *TimeoutError

	return errors.As(err, &timeoutErr)
}

// IsTFARequired checks if an error indicates TFA is required.
func IsTFARequired(err error) bool {
	var tfaErr *TFARequiredError

	return errors.As(err, &tfaErr)
}
