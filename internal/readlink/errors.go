package readlink

import (
	"errors"
	"fmt"
)

// Error is a read_link failure with a stable machine-readable code. Messages are
// safe to show to a model: they never include cookies, tokens, or page content.
type Error struct {
	Code       string
	Message    string
	HTTPStatus int
	Details    map[string]any
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

func newError(code, message string, status int) *Error {
	return &Error{Code: code, Message: message, HTTPStatus: status}
}

func (e *Error) withDetail(key string, value any) *Error {
	copied := *e
	copied.Details = map[string]any{}
	for k, v := range e.Details {
		copied.Details[k] = v
	}
	copied.Details[key] = value
	return &copied
}

// AsError converts any error to a public *Error. Unknown errors collapse to a
// generic INTERNAL_ERROR so internals never leak to the caller.
func AsError(err error) *Error {
	var typed *Error
	if errors.As(err, &typed) {
		return typed
	}
	return newError("INTERNAL_ERROR", "read_link failed; inspect the local service state and retry later.", 500)
}
