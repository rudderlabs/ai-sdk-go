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
	if !i.joinable() {
		warnOnce("join:unknown", "JoinInteraction: no open interaction has this ID in this process; the context is unchanged", joinWarnEvery)
		return ctx
	}
	joined := context.WithValue(ctx, keyInteraction, i)
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

// joinable reports, under its lock, that it is neither pausing, paused nor ended.
func (i *Interaction) joinable() bool {
	if i == nil {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return !i.ended && !i.pausing && i.pausedAt.IsZero()
}

// openByID is the open (not paused) interaction with this ID in this process.
func openByID(id string) *Interaction {
	open.mu.Lock()
	defer open.mu.Unlock()
	for e := open.list.Front(); e != nil; e = e.Next() {
		if i := e.Value.(*Interaction); i.id == id {
			return i
		}
	}
	return nil
}
