package api

// Code classifies an API error onto a stable, machine-readable string so both
// the REST and MCP adapters can map it to their own transport-specific shape
// (an HTTP status for REST, an error payload for MCP) without re-deriving it
// from prose.
type Code string

const (
	CodeNotFound  Code = "not_found"
	CodeForbidden Code = "forbidden"
	CodeInvalid   Code = "invalid"
	CodeConflict  Code = "conflict"
	// CodeUnavailable: the request was refused for now (a full deploy queue,
	// the organization's deploy limit, a shutdown) and can be retried as is.
	CodeUnavailable Code = "unavailable"
	CodeInternal    Code = "internal"
)

// Error is the single error shape both adapters map from.
type Error struct {
	Code    Code
	Message string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

func NotFound(msg string) *Error  { return &Error{Code: CodeNotFound, Message: msg} }
func Invalid(msg string) *Error   { return &Error{Code: CodeInvalid, Message: msg} }
func Forbidden(msg string) *Error { return &Error{Code: CodeForbidden, Message: msg} }
func Conflict(msg string) *Error  { return &Error{Code: CodeConflict, Message: msg} }
func Unavailable(msg string) *Error {
	return &Error{Code: CodeUnavailable, Message: msg}
}
