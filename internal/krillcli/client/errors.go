package client

import (
	"fmt"
	"time"
)

// APIError is a refused request. Code is the server's machine-readable
// classification; HTTPStatus is kept because two of the statuses the CLI must
// tell apart do not come with a JSON body at all.
type APIError struct {
	Code       string
	Message    string
	RequestID  string
	HTTPStatus int
	// RetryAfter is set on 429 and 503, where the server says how long to
	// wait rather than leaving the client to guess.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("%s: %s (request %s)", e.Code, e.Message, e.RequestID)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *APIError) IsNotFound() bool     { return e.HTTPStatus == 404 }
func (e *APIError) IsForbidden() bool    { return e.HTTPStatus == 403 }
func (e *APIError) IsUnauthorized() bool { return e.HTTPStatus == 401 }
func (e *APIError) IsConflict() bool     { return e.HTTPStatus == 409 }
func (e *APIError) IsRateLimited() bool  { return e.HTTPStatus == 429 }
func (e *APIError) IsTooLarge() bool     { return e.HTTPStatus == 413 }

// IsRetryable reports whether waiting and repeating the SAME request is
// sensible. 503 means authentication is temporarily unavailable and 429 means
// slow down; everything else is a decision the server will make the same way
// again.
//
// 409 is deliberately excluded even though it is transient: another deploy is
// in flight, and the right response is to wait for THAT deployment and then
// act once, not to resend. Retrying a deploy is not free — each accepted one
// rewrites the application's tag.
func (e *APIError) IsRetryable() bool {
	return e.HTTPStatus == 503 || e.HTTPStatus == 429
}
