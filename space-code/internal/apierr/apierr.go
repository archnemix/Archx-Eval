// Package apierr defines the single error type the HTTP layer knows how to
// render. Business logic returns *apierr.Error directly so that status codes,
// machine-readable codes and Retry-After values stay out of the handlers.
package apierr

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Error is a transport-agnostic API failure.
type Error struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// BadRequest reports a malformed or invalid payload.
func BadRequest(msg string) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "BAD_REQUEST", Message: msg}
}

// InvalidIdentity reports unusable device identity data.
func InvalidIdentity(msg string) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "IDENTITY_INVALID", Message: msg}
}

// Forbidden reports a policy refusal with no retry guidance (e.g. known VPN).
func Forbidden(code, msg string) *Error {
	return &Error{Status: http.StatusForbidden, Code: code, Message: msg}
}

// Limited reports a rate/ban refusal with a retry hint.
func Limited(code, msg string, retry time.Duration) *Error {
	if retry < time.Second {
		retry = time.Second
	}
	return &Error{
		Status:     http.StatusTooManyRequests,
		Code:       code,
		Message:    msg,
		RetryAfter: retry,
	}
}

// Conflict reports a state clash that is not a rate limit.
func Conflict(code, msg string) *Error {
	return &Error{Status: http.StatusConflict, Code: code, Message: msg}
}

// NotFound reports an unknown resource.
func NotFound(msg string) *Error {
	return &Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: msg}
}

// Internal reports an unexpected server-side failure.
func Internal(msg string) *Error {
	return &Error{Status: http.StatusInternalServerError, Code: "INTERNAL", Message: msg}
}

// Unavailable reports a dependency we could not reach.
func Unavailable(msg string) *Error {
	return &Error{Status: http.StatusServiceUnavailable, Code: "DEPENDENCY_UNAVAILABLE", Message: msg}
}

// As extracts an *Error from err, if there is one.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
