package rudderstackai

import (
	"context"
	"testing"
	"time"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func TestExecutingContextFallback(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	old := hook.Tracing.AdoptOTelCall
	hook.Tracing.AdoptOTelCall = func(context.Context, any, uint64, func(hook.OTelCall) bool) (hook.OTelCall, bool) {
		return hook.OTelCall{}, false
	}
	t.Cleanup(func() { hook.Tracing.AdoptOTelCall = old })
	v := &fixtureSerializer{Output: "context"}
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false), Context: map[string]any{"v": v}})
	v.Calls = 0
	_, s := beginWrapperSpan(ctx, i, time.Now())
	s.End(nil)
	if v.Calls != 1 {
		t.Fatalf("context calls=%d", v.Calls)
	}
}
func TestExecutingDisabledSpanSerializers(t *testing.T) {
	for _, op := range []string{"chat", "execute_tool"} {
		t.Run(op, func(t *testing.T) {
			setupAI(t, WithEmitSpans(true), WithCaptureContent(false))
			v := &fixtureSerializer{Output: "private"}
			ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			_, s := StartSpan(ctx, SpanOptions{Name: "test", Operation: op, Attributes: map[string]any{"input.value": v, "gen_ai.tool.call.arguments": v}})
			s.End(nil)
			i.End(EndOptions{})
			if v.Calls != 0 {
				t.Fatalf("disabled serializers=%d", v.Calls)
			}
		})
	}
}

func TestExecutingContextRejectedCandidate(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	old := hook.Tracing.AdoptOTelCall
	hook.Tracing.AdoptOTelCall = func(ctx context.Context, owner any, seq uint64, admit func(hook.OTelCall) bool) (hook.OTelCall, bool) {
		admit(hook.OTelCall{TraceID: "11111111111111111111111111111111", SpanID: "2222222222222222"})
		return hook.OTelCall{}, false
	}
	t.Cleanup(func() { hook.Tracing.AdoptOTelCall = old })
	v := &fixtureSerializer{Output: "context"}
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false), Context: map[string]any{"v": v}})
	v.Calls = 0
	_, s := beginWrapperSpan(ctx, i, time.Now())
	s.End(nil)
	if v.Calls != 1 {
		t.Fatalf("rejected candidate captured %d times", v.Calls)
	}
}
