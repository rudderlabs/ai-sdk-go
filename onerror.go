package rudderstackai

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Kinds of [ErrorInfo]: what went wrong inside the SDK. Identical in Node.js and Python.
const (
	// KindInternal is a failure (panic) in the SDK's own code; the host's call is unaffected.
	KindInternal = "internal"
	// KindUserCallback is a hook you configured (redact, identify, event rules, classifier,
	// evaluator, trace ID) that failed or panicked; the SDK used its documented fallback.
	KindUserCallback = "user_callback"
	// KindLimitReached is a memory cap of [Limits] that was hit.
	KindLimitReached = "limit_reached"
	// KindRedactionTimeout is a redaction pattern that ran out of time; the field was sent as
	// "[redaction timed out]".
	KindRedactionTimeout = "redaction_timeout"
	// KindFlushTimeout is a [Flush], [Shutdown] or [RunTask] flush that hit its deadline.
	KindFlushTimeout = "flush_timeout"
	// KindFlushFailed is your RudderStack client's own Flush failing during a [Flush], [Shutdown]
	// or [RunTask] flush; the error passed is your client's, unchanged.
	KindFlushFailed = "flush_failed"
	// KindClientOutdated is a RudderStack client too old for what the SDK asked of it.
	KindClientOutdated = "client_outdated"
)

// ErrorInfo describes an error reported to the [WithOnError] hook.
type ErrorInfo struct {
	// Kind is one of the Kind constants.
	Kind string
	// Where names the SDK step or the hook that failed.
	Where string
}

// onErrorEvery rate-limits the hook: one call per kind in this window.
const onErrorEvery = time.Minute

// WithOnError sets a hook that sees errors the SDK contained: it is called at most once per kind
// a minute, and a panic in it is swallowed. Without it, each kind is logged once (as a slog
// warning, with the error's type, never its text); RUDDERSTACK_AI_DEBUG logs every occurrence at
// debug level. Nil removes it.
func WithOnError(hook func(err error, info ErrorInfo)) ConfigOption {
	return func(c *config, _ *pending) error {
		c.onError = hook
		return nil
	}
}

var reported = struct {
	mu   sync.Mutex
	last map[string]time.Time
}{last: map[string]time.Time{}}

// report hands a contained error to the hook, or logs it. Never panics.
func report(kind, where string, err error) {
	defer func() { _ = recover() }()
	debugf("contained an error", "kind", kind, "where", where, "error", err)
	hook := cfg().onError
	if hook == nil {
		warnOnce("error:"+kind, fmt.Sprintf("%s error in %s (%T); set WithOnError or RUDDERSTACK_AI_DEBUG for details", kind, where, err), 0)
		return
	}
	now := time.Now()
	reported.mu.Lock()
	last, seen := reported.last[kind]
	again := !seen || now.Sub(last) >= onErrorEvery
	if again {
		reported.last[kind] = now
	}
	reported.mu.Unlock()
	if again {
		hook(err, ErrorInfo{Kind: kind, Where: where})
	}
}

// panicError is the error a contained panic is reported as.
type panicError struct{ value any }

func (e *panicError) Error() string { return fmt.Sprintf("panic: %v", e.value) }

// recovered turns a recovered panic value into an error.
func recovered(r any) error { return &panicError{value: r} }

// callback runs a hook you configured; false when it panicked or returned an error, which is
// reported as user_callback.
func callback(where string, fn func() error) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			report(KindUserCallback, where, recovered(r))
			ok = false
		}
	}()
	if err := fn(); err != nil {
		report(KindUserCallback, where, err)
		return false
	}
	return true
}

// callerOf names the function that called the frame named fn (for example "safely"), without
// its package: where a contained panic happened.
func callerOf(fn string) string {
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	found := false
	for {
		f, more := frames.Next()
		if found {
			name := f.Function[strings.LastIndex(f.Function, "/")+1:]
			return name[strings.Index(name, ".")+1:]
		}
		found = strings.HasSuffix(f.Function, "."+fn)
		if !more {
			return "unknown"
		}
	}
}
