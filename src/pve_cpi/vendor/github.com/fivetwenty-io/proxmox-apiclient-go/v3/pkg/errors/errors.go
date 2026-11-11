package errors

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/internal/constants"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/internal/safetext"
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

	// droppedErrors counts the entries of Errors that were cut because the
	// server sent more than MaxErrorItems.
	droppedErrors int
}

// NewEnvelopeError builds the APIError for a reply whose status says success
// but whose envelope reports a failure. The message and the field errors are
// text the server chose, so they get the same escaping and the same length cap
// as the error ParseAPIError builds from a failure status.
func NewEnvelopeError(statusCode int, message string, fieldErrors map[string]string) *APIError {
	apiErr := &APIError{
		Message:  message,
		Code:     statusCode,
		Errors:   fieldErrors,
		HTTPCode: statusCode,
		sentinel: sentinelFor(statusCode),
	}
	apiErr.escapeServerText()

	return apiErr
}

// Unwrap returns the wrapped sentinel error, enabling errors.Is chains.
func (e *APIError) Unwrap() error {
	return e.sentinel
}

// Error implements the error interface. It prints at most MaxErrorItems of the
// field errors, in the order of their names, then how many more there were, and
// it is never longer than MaxErrorTextBytes.
func (e *APIError) Error() string {
	more := e.droppedErrors

	if len(e.Errors) > 0 {
		fields := make([]string, 0, len(e.Errors))
		for field := range e.Errors {
			fields = append(fields, field)
		}

		sort.Strings(fields)

		if len(fields) > MaxErrorItems {
			more += len(fields) - MaxErrorItems
			fields = fields[:MaxErrorItems]
		}

		errStrs := make([]string, 0, len(fields)+1)
		for _, field := range fields {
			errStrs = append(errStrs, fmt.Sprintf("%s: %s", field, e.Errors[field]))
		}

		if more > 0 {
			errStrs = append(errStrs, moreMarker(more))
		}

		return capErrorText(fmt.Sprintf("%s (code: %d, errors: %s)", e.Message, e.Code, strings.Join(errStrs, ", ")))
	}

	if more > 0 {
		return capErrorText(fmt.Sprintf("%s (code: %d, errors: %s)", e.Message, e.Code, moreMarker(more)))
	}

	return capErrorText(fmt.Sprintf("%s (code: %d)", e.Message, e.Code))
}

// moreMarker names how many items an error left out.
func moreMarker(count int) string {
	return fmt.Sprintf("and %d more", count)
}

// capErrorText cuts text at MaxErrorTextBytes, on a character boundary, and ends
// it with a marker when it cut. The wrapper types call it on the error they
// build, so no error is longer than the cap by much.
func capErrorText(text string) string {
	if len(text) <= MaxErrorTextBytes {
		return text
	}

	cut := MaxErrorTextBytes - len(truncationMarker)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}

	return text[:cut] + truncationMarker
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
		return fmt.Sprintf("permission denied for %s: %s", safetext.Multiline(e.What), e.APIError.Error())
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
		return fmt.Sprintf("parameter error: %s (usage: %s)", e.APIError.Error(), safetext.Multiline(e.Usage))
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
		msg += " for realm " + safetext.Multiline(e.Realm)
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

	// droppedTypes counts the types that were cut because the server sent more
	// than MaxErrorItems.
	droppedTypes int
}

// NewTFARequiredError builds the error for a login reply that asks for a second
// factor. The ticket, the challenge, and the types are text the server chose,
// so all three are escaped as the error ParseAPIError builds from a 401 reply
// escapes them. The types are also limited to MaxErrorItems entries and cut at
// MaxServerTextBytes each. The ticket and the challenge are kept whole, because
// the caller sends them back to finish the login. The types slice is copied, so
// the caller's slice stays as it was.
func NewTFARequiredError(ticket, challenge string, types []string) *TFARequiredError {
	tfaErr := &TFARequiredError{
		Ticket:    ticket,
		Challenge: challenge,
		Types:     append([]string(nil), types...),
	}
	tfaErr.escapeServerText()

	return tfaErr
}

// Error implements the error interface. It prints at most MaxErrorItems of the
// types, in the order the server sent them, then how many more there were, and
// it is never longer than MaxErrorTextBytes.
func (e *TFARequiredError) Error() string {
	types := e.Types
	more := e.droppedTypes

	if len(types) > MaxErrorItems {
		more += len(types) - MaxErrorItems
		types = types[:MaxErrorItems]
	}

	listed := strings.Join(types, ", ")

	if more > 0 {
		if listed != "" {
			listed += ", "
		}

		listed += moreMarker(more)
	}

	return capErrorText("two-factor authentication required (available types: " + listed + ")")
}

// escapeServerText escapes the text fields the server chose, as
// APIError.escapeServerText does. It keeps the first MaxErrorItems types and
// cuts each at MaxServerTextBytes. The ticket and the challenge are values the
// caller sends back to finish the login, so they are escaped and not cut.
func (e *TFARequiredError) escapeServerText() {
	e.Ticket = safetext.Multiline(e.Ticket)
	e.Challenge = safetext.Multiline(e.Challenge)

	if len(e.Types) > MaxErrorItems {
		e.droppedTypes += len(e.Types) - MaxErrorItems
		e.Types = e.Types[:MaxErrorItems:MaxErrorItems]
	}

	for i, kind := range e.Types {
		e.Types[i] = safetext.CappedMultiline(kind, MaxServerTextBytes)
	}
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
	apiErr.escapeServerText()

	return dispatchByStatus(statusCode, body, apiErr)
}

// MaxServerTextBytes is the most bytes of escaped text that an error keeps from
// one server-chosen string. It bounds the message of a reply that is not JSON,
// such as a proxy's error page, and the message, each field name, each field
// message, and the file of a reply that is.
const MaxServerTextBytes = safetext.MaxServerTextBytes

// MaxErrorItems is the most items an error keeps and prints from a list the
// server sent, meaning the entries of the errors map and the TFA types. An error
// that cut items says how many more there were.
const MaxErrorItems = 32

// MaxErrorTextBytes is the most bytes that the Error method of an API error or
// a TFA error returns. It is above what MaxErrorItems items of ordinary size
// need, and below the size at which a log line or a terminal is flooded.
const MaxErrorTextBytes = safetext.MaxErrorTextBytes

// truncationMarker ends an error text that was cut.
const truncationMarker = "...(truncated)"

// MaxGenericMessageBytes is the most bytes of a body that is not JSON that an
// error message keeps. It is enough to show the title of a proxy's error page.
const MaxGenericMessageBytes = MaxServerTextBytes

// escapeServerText replaces, in every text field the server chose, each control
// character other than newline and tab, each text direction override, and each
// invalid byte with its escape sequence, so printing the error cannot rewrite
// a terminal or hide text. Each field is also cut at MaxServerTextBytes of
// escaped text, with a marker when it was cut. Newlines stay, because PVE
// writes multi-line messages. A program that logs the error on one line should
// quote it.
func (e *APIError) escapeServerText() {
	e.Message = safetext.CappedMultiline(e.Message, MaxServerTextBytes)
	e.File = safetext.CappedMultiline(e.File, MaxServerTextBytes)

	if e.Errors == nil {
		return
	}

	fields := make([]string, 0, len(e.Errors))
	for field := range e.Errors {
		fields = append(fields, field)
	}

	// Map order is random, so the entries that are kept are the first by name.
	sort.Strings(fields)

	if len(fields) > MaxErrorItems {
		e.droppedErrors += len(fields) - MaxErrorItems
		fields = fields[:MaxErrorItems]
	}

	escaped := make(map[string]string, len(fields))
	for _, field := range fields {
		escaped[safetext.CappedMultiline(field, MaxServerTextBytes)] =
			safetext.CappedMultiline(e.Errors[field], MaxServerTextBytes)
	}

	e.Errors = escaped
}

// newGenericError builds an APIError for non-JSON or empty bodies. The message
// is the body cut at MaxGenericMessageBytes, and escaped.
func newGenericError(statusCode int, body []byte) *APIError {
	msg := strings.TrimSpace(safetext.CappedMultilineBytes(bytes.TrimSpace(body), MaxGenericMessageBytes))
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
		tfaErr.escapeServerText()

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
