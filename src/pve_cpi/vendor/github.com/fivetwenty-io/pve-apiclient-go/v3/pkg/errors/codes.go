package errors

// HTTP status codes commonly used by PVE API.
const (
	// Success codes.
	StatusOK        = 200
	StatusCreated   = 201
	StatusAccepted  = 202
	StatusNoContent = 204

	// Client error codes.
	StatusBadRequest          = 400
	StatusUnauthorized        = 401
	StatusPaymentRequired     = 402
	StatusForbidden           = 403
	StatusNotFound            = 404
	StatusMethodNotAllowed    = 405
	StatusNotAcceptable       = 406
	StatusConflict            = 409
	StatusGone                = 410
	StatusUnprocessableEntity = 422
	StatusTooManyRequests     = 429

	// Server error codes.
	StatusInternalServerError = 500
	StatusNotImplemented      = 501
	StatusBadGateway          = 502
	StatusServiceUnavailable  = 503
	StatusGatewayTimeout      = 504
)

// PVE-specific error codes.
const (
	// Authentication and authorization.
	CodeAuthenticationFailed = 401
	CodePermissionDenied     = 403
	CodeTFARequired          = 730

	// Parameter errors.
	CodeInvalidParameter = 400
	CodeMissingParameter = 422

	// Resource errors.
	CodeResourceNotFound = 404
	CodeResourceLocked   = 423
	CodeResourceInUse    = 409
	CodeResourceExists   = 409
	CodeQuotaExceeded    = 507

	// Operation errors.
	CodeOperationTimeout    = 504
	CodeOperationFailed     = 500
	CodeOperationNotAllowed = 405

	// Configuration errors.
	CodeConfigurationError = 500
	CodeInvalidConfig      = 422
)

// GetErrorMessage returns a human-readable message for an error code.
func GetErrorMessage(code int) string {
	errorCodeToMessage := map[int]string{
		StatusOK:                  "Success",
		StatusCreated:             "Created",
		StatusAccepted:            "Accepted",
		StatusNoContent:           "No Content",
		StatusBadRequest:          "Bad Request",
		StatusUnauthorized:        "Unauthorized",
		StatusPaymentRequired:     "Payment Required",
		StatusForbidden:           "Forbidden",
		StatusNotFound:            "Not Found",
		StatusMethodNotAllowed:    "Method Not Allowed",
		StatusNotAcceptable:       "Not Acceptable",
		StatusConflict:            "Conflict",
		StatusGone:                "Gone",
		StatusUnprocessableEntity: "Unprocessable Entity",
		StatusTooManyRequests:     "Too Many Requests",
		StatusInternalServerError: "Internal Server Error",
		StatusNotImplemented:      "Not Implemented",
		StatusBadGateway:          "Bad Gateway",
		StatusServiceUnavailable:  "Service Unavailable",
		StatusGatewayTimeout:      "Gateway Timeout",
		CodeTFARequired:           "Two-Factor Authentication Required",
		CodeResourceLocked:        "Resource Locked",
		CodeQuotaExceeded:         "Quota Exceeded",
	}

	if msg, ok := errorCodeToMessage[code]; ok {
		return msg
	}

	return "Unknown Error"
}

// IsSuccessCode returns true if the status code indicates success.
func IsSuccessCode(code int) bool {
	return code >= 200 && code < 300
}

// IsClientErrorCode returns true if the status code indicates a client error.
func IsClientErrorCode(code int) bool {
	return code >= 400 && code < 500
}

// IsServerErrorCode returns true if the status code indicates a server error.
func IsServerErrorCode(code int) bool {
	return code >= 500 && code < 600
}

// IsRetryableCode returns true if the error code indicates the request can be retried.
func IsRetryableCode(code int) bool {
	switch code {
	case StatusTooManyRequests,
		StatusServiceUnavailable,
		StatusGatewayTimeout,
		StatusBadGateway,
		CodeResourceLocked:
		return true
	default:
		return false
	}
}
