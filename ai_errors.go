package rudderstackai

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
)

// Error types a tool_breakdown entry may use; anything else is api_error.
var toolErrorTypes = set("rate_limit", "timeout", "cancelled", "denied", "auth_error", "invalid_request", "connection_error", "api_error")

func normalizeToolErrorType(v string) string {
	if toolErrorTypes[v] {
		return v
	}
	return "api_error"
}

var errorClassifiers struct {
	mu  sync.RWMutex
	fns []func(error) (string, bool)
}

// registerErrorClassifier adds a provider-specific mapping (an integration's error type), tried
// before the generic rules.
func registerErrorClassifier(fn func(error) (string, bool)) {
	errorClassifiers.mu.Lock()
	errorClassifiers.fns = append(errorClassifiers.fns, fn)
	errorClassifiers.mu.Unlock()
}

// ErrorTypeOf maps an error to a spec error_type: rate_limit (HTTP 429, throttling), timeout
// (deadline exceeded, HTTP 408 and 504), cancelled (context cancelled), auth_error (401, 403),
// invalid_request (400, 422), connection_error or api_error. The error chain is unwrapped, so a
// retry library's error wrapping a provider error maps as the provider error.
func ErrorTypeOf(err error) string {
	if err == nil {
		return ""
	}
	errorClassifiers.mu.RLock()
	fns := errorClassifiers.fns
	errorClassifiers.mu.RUnlock()
	for _, fn := range fns {
		if t, ok := safeClassify(fn, err); ok {
			return t
		}
	}
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		if t := errorTypeForName(coded.ErrorCode()); t != "" {
			return t
		}
	}
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) {
		if t := ErrorTypeForStatus(status.HTTPStatusCode()); t != "" {
			return t
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	var nerr net.Error
	if errors.As(err, &nerr) {
		if nerr.Timeout() {
			return "timeout"
		}
		return "connection_error"
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return "connection_error"
	}
	return "api_error"
}

func safeClassify(fn func(error) (string, bool), err error) (t string, ok bool) {
	defer func() {
		if recover() != nil {
			t, ok = "", false
		}
	}()
	return fn(err)
}

// ErrorTypeForStatus maps an HTTP status to an error_type, or "" for a status that names none.
func ErrorTypeForStatus(status int) string {
	switch status {
	case 429:
		return "rate_limit"
	case 408, 504:
		return "timeout"
	case 401, 403:
		return "auth_error"
	case 400, 422:
		return "invalid_request"
	}
	return ""
}

// errorTypeForName maps an error name or code (AWS ThrottlingException, AccessDeniedException,
// ValidationException) to an error_type, or "".
func errorTypeForName(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "throttling"), strings.Contains(n, "ratelimit"), n == "insufficient_quota", n == "rate_limit_exceeded":
		return "rate_limit"
	case strings.Contains(n, "timeout"):
		return "timeout"
	case strings.Contains(n, "accessdenied"), strings.Contains(n, "authentication"), strings.Contains(n, "permissiondenied"):
		return "auth_error"
	case strings.Contains(n, "validationexception"):
		return "invalid_request"
	}
	return ""
}

// Caps a run stops at before finishing: its answer is incomplete, not failed.
var caps = set("max_turns", "max_budget_usd", "length")

// statusForErrorType is the response_status an error type ends an answer with.
func statusForErrorType(t string) ResponseStatus {
	switch {
	case t == "timeout":
		return StatusTimeout
	case t == "cancelled":
		return StatusCancelled
	case caps[t]:
		return StatusIncomplete
	}
	return StatusError
}
