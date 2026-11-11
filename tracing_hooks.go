package rudderstackai

import (
	"context"
	"strconv"

	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func init() {
	hook.Tracing.Identity = tracingIdentity
	hook.Tracing.Open = func(id string) any {
		open.mu.Lock()
		defer open.mu.Unlock()
		for e := open.list.Front(); e != nil; e = e.Next() {
			i := e.Value.(*Interaction)
			if i.id == id {
				return i
			}
		}
		return nil
	}
	hook.Tracing.AdoptAnalytics = func(i any, c analytics.Client) { i.(*Interaction).adoptAnalytics(c) }
	hook.Tracing.RecordTool = func(i, call any) { i.(*Interaction).recordToolCalls(true, call.(ToolCall)) }
	hook.Tracing.Analytics = func() analytics.Client { return cfg().analytics }
	hook.Tracing.CaptureContent = func() bool { return cfg().captureContent }
	hook.Tracing.Redact = redactText
	hook.Tracing.RememberClient = rememberClient
	hook.Tracing.Wait = waitBackgroundUntil
	hook.Tracing.FlushClient = func(ctx context.Context, client analytics.Client) bool {
		finished := waitBackgroundUntil(ctx) && hook.WaitTracked(ctx)
		drained, finish := flushClients(ctx, "tracing.flush", []analytics.Client{client})
		return finishFlush(ctx, "tracing.flush", finished && drained, finish)
	}
}

func tracingIdentity(ctx context.Context, traceID string) (tags map[string]string, system, suppressed bool, owner any) {
	tags = map[string]string{}
	if ctx == nil {
		ctx = context.Background()
	}
	i := FromContext(ctx)
	if isUntracked(ctx) || (i != nil && i.untracked) {
		return tags, false, true, nil
	}
	put := func(k, v string) {
		if v != "" {
			tags[k] = v
		}
	}
	if i != nil {
		i.mu.Lock()
		defer i.mu.Unlock()
		put("user.id", i.userID)
		put("rudderstack.anonymous_id", i.anonymousID)
		put("gen_ai.conversation.id", i.conversationID)
		put("rudderstack.interaction_id", i.id)
		if traceID != "" && i.traceID == "" {
			i.traceID = traceID
		}
		return tags, i.trigger == TriggerSystem, false, i
	}
	if cv := valuesFrom(ctx); cv != nil {
		id := resolveIdentity(identityInputs{}, cv, cfg())
		put("user.id", id.userID)
		put("rudderstack.anonymous_id", id.anonymousID)
		put("gen_ai.conversation.id", cv.conversationID)
		if id.sessionID != 0 {
			put("session.id", strconv.FormatInt(id.sessionID, 10))
		}
	}
	return tags, false, false, nil
}
