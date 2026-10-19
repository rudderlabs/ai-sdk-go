package rudderstackai

import (
	"context"
	"net/http"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

// ContextValues are request-wide defaults for the interactions, integrations, [TrackAction] and
// [TrackScore] calls made with a context from [WithValues]. Empty fields inherit from an
// enclosing WithValues.
type ContextValues struct {
	UserID         string
	AnonymousID    string
	ConversationID string
	Feature        string
	// AccountID is the customer, workspace or tenant the work is for (account_id).
	AccountID string
	// OnBehalfOf is the human who requested or owns the work (on_behalf_of); not the actor.
	OnBehalfOf string
	// Surface is where the user met the AI.
	Surface Surface
	// Analytics is the RudderStack client for events made under this context.
	Analytics analytics.Client
	// Context is extra message context merged into every event, such as {"app": {"build": "7"}}.
	Context map[string]any
	// Request is the incoming request: its RudderStack JavaScript SDK cookies supply
	// anonymousId, context.sessionId and a fallback userId. Explicit values always win.
	Request *http.Request
	// Cookies is the same cookies as a raw Cookie header. Wins over Request.
	Cookies string
	// CookieValues is the same cookies by name (raw values). Wins over Cookies.
	CookieValues map[string]string
}

// contextValues is the merged, validated form of ContextValues.
type contextValues struct {
	userID, anonymousID, conversationID, feature, accountID, onBehalfOf string
	surface                                                             Surface
	analytics                                                           analytics.Client
	context                                                             map[string]any
	cookies                                                             map[string]string
	identityFromCookies                                                 *bool
	// disconnected reports whether the client of the request went away ([Middleware]).
	disconnected func() bool
}

type ctxKey int

const (
	keyValues ctxKey = iota
	keyInteraction
	keyUntracked
)

func valuesFrom(ctx context.Context) *contextValues {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(keyValues).(*contextValues)
	return v
}

// WithValues returns a context carrying request-wide defaults: identity, conversation, feature,
// the RudderStack client and extra message context. Values left empty inherit from the parent.
//
//	ctx = rudderstackai.WithValues(r.Context(), rudderstackai.ContextValues{UserID: user.ID, Request: r})
func WithValues(ctx context.Context, v ContextValues) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	merged := contextValues{}
	if parent := valuesFrom(ctx); parent != nil {
		merged = *parent
	}
	header := v.Cookies
	if header == "" {
		header = requestCookies(v.Request)
	}
	cookies := rudderstackCookies(header, v.CookieValues)
	inherit(&merged.userID, v.UserID)
	inherit(&merged.anonymousID, v.AnonymousID)
	inherit(&merged.conversationID, v.ConversationID)
	inherit(&merged.feature, v.Feature)
	inherit(&merged.accountID, v.AccountID)
	inherit(&merged.onBehalfOf, v.OnBehalfOf)
	if v.Surface != "" {
		merged.surface = v.Surface
	}
	if v.Analytics != nil {
		merged.analytics = v.Analytics
	}
	if v.Context != nil {
		merged.context = v.Context
	}
	if cookies != nil {
		merged.cookies = cookies
	}
	return context.WithValue(ctx, keyValues, &merged)
}

func inherit(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

// FromContext returns the interaction a context carries (from [StartInteraction],
// [RunInteraction] or [RunTask]), or nil.
func FromContext(ctx context.Context) *Interaction {
	if ctx == nil {
		return nil
	}
	i, _ := ctx.Value(keyInteraction).(*Interaction)
	return i
}

// withUntracked marks a context whose model calls (the SDK's own: the classifier, an evaluator)
// are never tracked.
func withUntracked(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, keyInteraction, (*Interaction)(nil))
	return context.WithValue(ctx, keyUntracked, true)
}

func isUntracked(ctx context.Context) bool {
	b, _ := ctx.Value(keyUntracked).(bool)
	return b
}
