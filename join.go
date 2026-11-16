package rudderstackai

import (
	"context"
	"time"
)

// JoinOptions configures [JoinInteraction].
type JoinOptions struct {
	// ParentSpanID, with WithEmitSpans(true): the SpanID of an SDK span of that interaction (a
	// StartSpan handle, which may have ended). SDK spans started directly with the returned
	// context nest under it, ahead of the context's SDK or OpenTelemetry parent.
	ParentSpanID string
}

// joinWarnEvery is the shortest gap between two warnings of one kind, so a hot worker cannot
// flood the log.
const joinWarnEvery = time.Minute

// JoinInteraction returns ctx joined to the interaction open in this process under
// interactionID, for work that runs on a context that lost it: a goroutine started from
// context.Background(), a callback library, an in-process job queue. Wrapper calls, StartSpan
// and RecordCall with the returned context report into that interaction, which wins over any
// interaction ctx carries. An unknown or ended ID logs a rate-limited warning and returns ctx
// unchanged; a malformed ParentSpanID warns and is ignored. A nil ctx is
// context.Background(). It never panics.
func JoinInteraction(ctx context.Context, interactionID string, o JoinOptions) (out context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() {
		if r := recover(); r != nil {
			report(KindInternal, "join", recovered(r))
			out = ctx
		}
	}()
	i := openByID(interactionID)
	scope, ok := i.joinScope()
	if !ok {
		warnOnce("join:unknown", "JoinInteraction: no open interaction has this ID in this process; the context is unchanged", joinWarnEvery)
		return ctx
	}
	// The interaction's own scope and identity win over the caller's values.
	joined := context.WithValue(context.WithValue(ctx, keyValues, scope), keyInteraction, i)
	if !emitSpans.Load() || o.ParentSpanID == "" {
		return joined
	}
	if !validSDKSpanID(o.ParentSpanID) {
		warnOnce("join:parent", "JoinInteraction: ParentSpanID must be 16 lowercase hex characters, not all zero; ignored", joinWarnEvery)
		return joined
	}
	return context.WithValue(joined, sdkSpanKey{}, joinedParent(i, o.ParentSpanID))
}

// joinedParent is a synthetic SDK parent in the interaction's SDK trace: never sent or ended.
func joinedParent(i *Interaction, spanID string) *Span {
	i.mu.Lock()
	if i.sdkTraceID == "" {
		i.sdkTraceID = randomSpanID(16)
	}
	traceID := i.sdkTraceID
	i.mu.Unlock()
	return &Span{TraceID: traceID, SpanID: spanID, interaction: i, seq: spanSeq.Add(1)}
}

// joinScope is the context values a join restores: the scope the interaction started in, with
// its own identity, conversation and run labels. Not ok once it is pausing, paused or ended,
// checked under its lock.
func (i *Interaction) joinScope() (*contextValues, bool) {
	if i == nil {
		return nil, false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.ended || i.pausing || !i.pausedAt.IsZero() {
		return nil, false
	}
	cv := contextValues{}
	if i.scope != nil {
		cv = *i.scope
	}
	cv.userID, cv.anonymousID, cv.conversationID = i.userID, i.anonymousID, i.conversationID
	cv.feature, cv.surface = i.feature, i.surface
	cv.accountID, cv.onBehalfOf = i.accountID, i.onBehalfOf
	cv.agentVersion, cv.variant = i.agentVersion, i.variant
	if i.resumed {
		// As RunInteraction scopes a resumed run: the snapshot's identity, not this request's.
		cv.cookies, cv.disconnected = nil, nil
		cv.identityFromCookies = Ptr(false)
	}
	return &cv, true
}

// openByID is the most recently started open (not paused) interaction with this ID in this
// process, so a reused explicit ID resolves as in Node.js and Python.
func openByID(id string) *Interaction {
	open.mu.Lock()
	defer open.mu.Unlock()
	for e := open.list.Back(); e != nil; e = e.Prev() {
		if i := e.Value.(*Interaction); i.id == id {
			return i
		}
	}
	return nil
}
