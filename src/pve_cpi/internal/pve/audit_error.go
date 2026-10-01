package pve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// auditDescriptionLimit caps one rendered error description in bytes. An
// audit finding carries at most one, and findings are joined into Director
// errors, CLI output, and retained evidence.
const auditDescriptionLimit = 200

// AuditVisibilityError names the grant an allocation audit found missing. Its
// text carries only a privilege name and an ACL path, so an audit finding may
// disclose it. An empty Privilege means PVE hid the path entirely.
type AuditVisibilityError struct {
	Privilege  string
	Path       string
	Propagated bool
}

func (e *AuditVisibilityError) Error() string {
	switch {
	case e.Privilege == "":
		return "allocation audit visibility is restricted at " + e.Path
	case e.Propagated:
		return fmt.Sprintf("allocation audit requires propagated %s at %s", e.Privilege, e.Path)
	default:
		return fmt.Sprintf("allocation audit requires %s at %s", e.Privilege, e.Path)
	}
}

// AuditVisibilityReadError is a read the audit visibility proof could not
// complete, or an answer it could not parse. Its text is fixed and never
// carries the cause, so err.Error() stays safe to log. DescribeAuditError
// renders the read, the path, and the class of the cause instead, and Unwrap
// exposes the cause to errors.Is and errors.As. An empty Read means the
// client has no visibility reader at all.
type AuditVisibilityReadError struct {
	// Read names what the proof was reading, such as "effective permissions"
	// or "ACL entries".
	Read string
	// Path is the ACL path the read asked about, or the API path it read.
	Path string
	// Malformed marks an answer that arrived but could not be parsed.
	Malformed bool
	Cause     error
	// text is the fixed text Error returns.
	text string
}

func (e *AuditVisibilityReadError) Error() string {
	if e.text == "" {
		return "allocation audit visibility read failed"
	}
	return e.text
}

func (e *AuditVisibilityReadError) Unwrap() error { return e.Cause }

// ErrAuditVisibilityReaderUnavailable reports a client with no audit
// visibility reader, which is a CPI defect.
var ErrAuditVisibilityReaderUnavailable error = &AuditVisibilityReadError{text: "allocation audit visibility reader unavailable"}

// GuestEnumerationError names the cluster members whose guest listing failed.
// Its own text carries only node names; Cause holds the per-node failure when
// one API verdict decided the classification.
type GuestEnumerationError struct {
	Nodes []string
	Cause error
}

func (e *GuestEnumerationError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("could not list guests on node %s; refusing to decide from a partial fleet: %s", strings.Join(e.Nodes, ","), e.Cause.Error())
	}
	return fmt.Sprintf("could not list guests on node(s) %s; refusing to decide from a partial fleet", strings.Join(e.Nodes, ","))
}

func (e *GuestEnumerationError) Unwrap() error { return e.Cause }

// knownProvenanceErrors are the fixed texts the provenance parsers return.
// None of them embeds description content, so an audit may repeat them.
var knownProvenanceErrors = map[string]bool{
	"ambiguous storage allocation provenance":      true,
	"malformed storage allocation provenance":      true,
	"invalid storage allocation provenance":        true,
	"invalid managed disk provenance":              true,
	"invalid managed disk volume identity":         true,
	"malformed disk provenance sentinel":           true,
	"ambiguous disk provenance sentinel":           true,
	"ambiguous JSON provenance":                    true,
	"trailing JSON provenance":                     true,
	"malformed JSON provenance":                    true,
	"duplicate field":                              true,
	"malformed managed disk provenance":            true,
	"empty managed disk provenance key":            true,
	"malformed parked allocation provenance":       true,
	"conflicting managed disk provenance carriers": true,
}

// DescribeAuditError renders err as text that is safe to retain in an audit
// finding, a Director error, or CLI output. It renders only fields whose
// content it knows: an API error's status and message, a connection
// endpoint, a timeout or cancellation class, a missing audit grant, the read
// and path of a failed visibility read, the nodes of a failed guest
// enumeration, and the parsers' fixed provenance texts.
// Anything else renders as "unclassified error"; it never falls back to
// err.Error(), because transport text can carry response bodies and
// credentials. The result is scrubbed, stripped of control characters, and
// capped at auditDescriptionLimit bytes, in that order, so a cut can never
// split a credential the scrubber would otherwise have matched.
func DescribeAuditError(err error) string {
	if err == nil {
		return ""
	}
	return boundAuditDescription(describeAuditErrorClass(err))
}

func describeAuditErrorClass(err error) string {
	var visibility *AuditVisibilityError
	if errors.As(err, &visibility) {
		return visibility.Error()
	}
	// A read failure must be recognized before the context checks, or a
	// timed-out read would lose its read and path.
	var read *AuditVisibilityReadError
	if errors.As(err, &read) {
		switch {
		case read.Read == "":
			return "the audit visibility reader is unavailable (CPI defect)"
		case read.Malformed:
			return fmt.Sprintf("PVE returned malformed %s at %s", read.Read, read.Path)
		case read.Cause != nil:
			return fmt.Sprintf("could not read %s at %s (%s)", read.Read, read.Path, describeAuditErrorClass(read.Cause))
		default:
			return fmt.Sprintf("could not read %s at %s", read.Read, read.Path)
		}
	}
	var enumeration *GuestEnumerationError
	if errors.As(err, &enumeration) {
		if enumeration.Cause != nil {
			return fmt.Sprintf("could not list guests on node %s (%s)", strings.Join(enumeration.Nodes, ","), describeAuditErrorClass(enumeration.Cause))
		}
		return "could not list guests on node(s) " + strings.Join(enumeration.Nodes, ",")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context deadline exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "context canceled"
	}
	var timeout *sdkerrors.TimeoutError
	if errors.As(err, &timeout) {
		return "request timed out"
	}
	var connection *sdkerrors.ConnectionError
	if errors.As(err, &connection) {
		return fmt.Sprintf("connection to %s:%d failed", connection.Host, connection.Port)
	}
	if code, message, ok := auditAPIErrorFields(err); ok {
		return fmt.Sprintf("HTTP %d: %s", code, message)
	}
	if knownProvenanceErrors[err.Error()] {
		return err.Error()
	}
	return "unclassified error"
}

// auditAPIErrorFields reads the status and message from every SDK API error
// shape. The message is usually PVE's, but for a non-JSON body the SDK keeps
// the raw body there, such as an intermediary's error page, which is why the
// caller scrubs and caps it. The subtypes embed APIError by value, so a bare
// errors.As against *APIError would miss them (see apiHTTPCode).
func auditAPIErrorFields(err error) (int, string, bool) {
	var apiErr *sdkerrors.APIError
	if errors.As(err, &apiErr) {
		return apiErr.HTTPCode, apiErr.Message, true
	}
	var permErr *sdkerrors.PermissionError
	if errors.As(err, &permErr) {
		return permErr.HTTPCode, permErr.Message, true
	}
	var paramErr *sdkerrors.ParameterError
	if errors.As(err, &paramErr) {
		return paramErr.HTTPCode, paramErr.Message, true
	}
	var authErr *sdkerrors.AuthenticationError
	if errors.As(err, &authErr) {
		return authErr.HTTPCode, authErr.Message, true
	}
	return 0, "", false
}

// boundAuditDescription scrubs, flattens, and caps a rendered description.
func boundAuditDescription(s string) string {
	s = log.ScrubMessage(s)
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) <= auditDescriptionLimit {
		return s
	}
	cut := auditDescriptionLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
