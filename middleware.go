package rudderstackai

import (
	"net/http"
	"sync/atomic"
)

// Middleware is net/http middleware: every interaction, integration call and [TrackAction] in the
// request takes anonymousId, context.sessionId and a fallback userId from the RudderStack
// JavaScript SDK cookies, as with WithValues(ctx, ContextValues{Request: r}). An interaction that
// ends while the client is gone (the request context is cancelled before the handler returns)
// is sent with client_disconnected.
//
//	http.Handle("/chat", rudderstackai.Middleware(chatHandler))
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var served atomic.Bool
		reqCtx := r.Context()
		ctx := WithValues(reqCtx, ContextValues{Request: r})
		if cv := valuesFrom(ctx); cv != nil {
			// net/http also cancels the context once the handler returns: that is not a disconnect.
			cv.disconnected = func() bool { return !served.Load() && reqCtx.Err() != nil }
		}
		defer served.Store(true)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
