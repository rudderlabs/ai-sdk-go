// Package hook connects the integration packages to the interaction internals without widening
// the public API: the root package sets these functions in its init, and internal/kit wraps them
// with typed signatures. The interaction is passed as any to avoid an import cycle.
package hook

import (
	"context"
	"sync"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

// Set by package rudderstackai.
var (
	// ForCall returns the interaction a model call starting now reports into, and whether it is
	// ephemeral (opened for this call; the integration must end it). Nil for untracked calls.
	ForCall func(ctx context.Context, integration string, client analytics.Client) (interaction any, ephemeral bool)
	// KeepMessages keeps a request's messages for the classifier and evaluator.
	KeepMessages func(interaction any, messages any)
	// KeepsText reports whether the interaction keeps prompt and answer text.
	KeepsText func(interaction any) bool
	// FirstToken marks the first streamed token.
	FirstToken func(interaction any, at time.Time)
	// HintOutcome sets the status End reports unless a call is recorded afterwards.
	HintOutcome func(interaction any, status string, errorType string)
	// OnBeforeEnd runs fn just before ai_response_received is built.
	OnBeforeEnd func(interaction any, fn func())
	// StatusForErrorType maps an error type to a response status.
	StatusForErrorType func(errorType string) string
	// RegisterErrorClassifier adds an integration's error mapping.
	RegisterErrorClassifier func(fn func(error) (string, bool))
	// Untracked reports whether calls with this context are the SDK's own.
	Untracked func(ctx context.Context) bool
	// Reset restores the defaults (tests only).
	Reset func()
)

var tracked struct {
	mu   sync.Mutex
	n    int
	idle chan struct{}
}

// Track registers integration work that Flush waits for; call done when it finishes.
func Track() (done func()) {
	tracked.mu.Lock()
	if tracked.n == 0 {
		tracked.idle = make(chan struct{})
	}
	tracked.n++
	tracked.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			tracked.mu.Lock()
			if tracked.n--; tracked.n == 0 {
				close(tracked.idle)
			}
			tracked.mu.Unlock()
		})
	}
}

// WaitTracked waits for tracked work, at most d. It reports whether all of it finished.
func WaitTracked(d time.Duration) bool {
	tracked.mu.Lock()
	if tracked.n == 0 {
		tracked.mu.Unlock()
		return true
	}
	idle := tracked.idle
	tracked.mu.Unlock()
	select {
	case <-idle:
		return true
	case <-time.After(d):
		return false
	}
}
