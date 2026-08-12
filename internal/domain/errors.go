package domain

import (
	"errors"
	"fmt"
)

// Code is the public, stable error classification. Retry behaviour is driven
// off Code, never off message text.
type Code string

const (
	CodeValidation  Code = "validation" // bad input, permanent
	CodeNotFound    Code = "not_found"  // entity missing, permanent
	CodeConflict    Code = "conflict"   // state/version conflict, permanent
	CodeForbidden   Code = "forbidden"  // allowlist/suspension, permanent
	CodeConsent     Code = "consent_required"
	CodeQuota       Code = "quota_exceeded"
	CodeUnsupported Code = "unsupported" // e.g. image is not a receipt
	CodeTransient   Code = "transient"   // retry with backoff
	CodeKillSwitch  Code = "kill_switch" // feature flag disabled the path
	CodeInternal    Code = "internal"
)

// Error is the single wrapped error type crossing layer boundaries.
type Error struct {
	Code    Code
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// E builds an *Error wrapping cause.
func E(code Code, msg string, cause error) *Error {
	return &Error{Code: code, Message: msg, Err: cause}
}

// Ef builds an *Error with a formatted message.
func Ef(code Code, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Err: cause}
}

// CodeOf extracts the Code of the first *Error in the chain, or CodeInternal.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

// IsCode reports whether err carries the given code anywhere in its chain.
func IsCode(err error, code Code) bool { return CodeOf(err) == code }

// Retryable reports whether a failed job carrying err should be retried.
func Retryable(err error) bool { return CodeOf(err) == CodeTransient }
