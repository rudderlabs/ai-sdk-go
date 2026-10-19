// Package hook connects the integration packages to the interaction internals without widening
// the public API: the root package sets these functions in its init, and internal/kit wraps them
// with typed signatures. The interaction is passed as any to avoid an import cycle.
package hook

import (
	"context"
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
