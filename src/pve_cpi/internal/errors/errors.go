// Package errors defines BOSH CPI error types with canonical type strings,
// ok_to_retry defaults, and JSON-RPC payload serialization.
//
// Every error maps to a "Bosh::Clouds::<Name>" type string as required by
// the BOSH CPI JSON-RPC error envelope specification.
package errors

import (
	"errors"
	"fmt"
)

// Type is the canonical BOSH CPI error type string sent in JSON-RPC error payloads.
type Type string

const (
	// TypeCloud is the generic IaaS error; not retriable.
	TypeCloud Type = "Bosh::Clouds::CloudError"

	// TypeRetriableCloud is the base retriable IaaS error; retriable.
	TypeRetriableCloud Type = "Bosh::Clouds::RetriableCloudError"

	// TypeVMNotFound signals a VM CID not found in IaaS; not retriable.
	TypeVMNotFound Type = "Bosh::Clouds::VMNotFound"

	// TypeDiskNotFound signals a disk CID not found in IaaS; not retriable by default.
	TypeDiskNotFound Type = "Bosh::Clouds::DiskNotFound"

	// TypeNotImplemented signals a CPI method is not implemented; not retriable.
	TypeNotImplemented Type = "Bosh::Clouds::NotImplemented"

	// TypeNotSupported signals an unsupported operation (e.g., shrink disk); not retriable.
	TypeNotSupported Type = "Bosh::Clouds::NotSupported"
)

// Error is the unified BOSH CPI error value. All constructors return *Error.
// Use Type(), OkToRetry(), and RPCPayload() at the JSON-RPC layer.
type Error struct {
	typ       Type
	msg       string
	cause     error
	retriable bool
}

// Error implements the error interface. When a cause is chained it is appended
// after a colon separator so errors.Unwrap chains work alongside readable strings.
func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s", e.msg, e.cause.Error())
	}
	return e.msg
}

// Unwrap returns the wrapped cause, enabling errors.Is / errors.As traversal.
func (e *Error) Unwrap() error {
	return e.cause
}

// Type returns the canonical BOSH CPI type string for this error.
func (e *Error) Type() Type {
	return e.typ
}

// OkToRetry reports whether the BOSH Director should retry the request with
// identical arguments after receiving this error.
func (e *Error) OkToRetry() bool {
	return e.retriable
}

// RPCPayload returns the map that serializes into the "error" field of a BOSH
// CPI JSON-RPC response envelope:
//
//	{"type": "Bosh::Clouds::...", "message": "...", "ok_to_retry": <bool>}
func (e *Error) RPCPayload() map[string]any {
	return map[string]any{
		"type":        string(e.typ),
		"message":     e.msg,
		"ok_to_retry": e.retriable,
	}
}

// --------------------------------------------------------------------------
// Constructors
// --------------------------------------------------------------------------

// Cloud returns a non-retriable generic CloudError.
func Cloud(format string, args ...any) *Error {
	return &Error{
		typ:       TypeCloud,
		msg:       fmt.Sprintf(format, args...),
		retriable: false,
	}
}

// Retriable returns a retriable RetriableCloudError.
func Retriable(format string, args ...any) *Error {
	return &Error{
		typ:       TypeRetriableCloud,
		msg:       fmt.Sprintf(format, args...),
		retriable: true,
	}
}

// VMNotFound returns a non-retriable VMNotFound error for the given VM CID.
func VMNotFound(vmCID string) *Error {
	return &Error{
		typ:       TypeVMNotFound,
		msg:       fmt.Sprintf("VM not found: %s", vmCID),
		retriable: false,
	}
}

// DiskNotFound returns a non-retriable DiskNotFound error for the given disk CID.
func DiskNotFound(diskCID string) *Error {
	return &Error{
		typ:       TypeDiskNotFound,
		msg:       fmt.Sprintf("disk not found: %s", diskCID),
		retriable: false,
	}
}

// NotImplemented returns a non-retriable error indicating the CPI method has
// no implementation.
func NotImplemented(method string) *Error {
	return &Error{
		typ:       TypeNotImplemented,
		msg:       fmt.Sprintf("method not implemented: %s", method),
		retriable: false,
	}
}

// NotSupported returns a non-retriable error indicating the operation is
// explicitly unsupported, with a human-readable reason.
func NotSupported(operation, reason string) *Error {
	return &Error{
		typ:       TypeNotSupported,
		msg:       fmt.Sprintf("operation not supported: %s (%s)", operation, reason),
		retriable: false,
	}
}

// --------------------------------------------------------------------------
// Wrap helpers
// --------------------------------------------------------------------------

// Wrap wraps err as a CloudError with the given message. If err is already a
// *Error its type and retriable flag are preserved; msg is prepended.
func Wrap(err error, msg string) *Error {
	var typed *Error
	if errors.As(err, &typed) {
		return &Error{
			typ:       typed.typ,
			msg:       msg,
			cause:     err,
			retriable: typed.retriable,
		}
	}
	return &Error{
		typ:       TypeCloud,
		msg:       msg,
		cause:     err,
		retriable: false,
	}
}

// WrapAs wraps err as the given Type. The retriable flag is set to the default
// for that type (true only for TypeRetriableCloud; false for all others).
func WrapAs(err error, typ Type, msg string) *Error {
	return &Error{
		typ:       typ,
		msg:       msg,
		cause:     err,
		retriable: typ == TypeRetriableCloud,
	}
}

// --------------------------------------------------------------------------
// Inspection helpers
// --------------------------------------------------------------------------

// IsType reports whether any error in err's chain is a *Error with the given Type.
func IsType(err error, typ Type) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.typ == typ
	}
	return false
}

// IsNotFound reports whether err (or any error in its chain) is a VMNotFound
// or DiskNotFound error.
func IsNotFound(err error) bool {
	return IsType(err, TypeVMNotFound) || IsType(err, TypeDiskNotFound)
}
