package rudderstackai

import (
	"context"
	"testing"
)

func TestSerializerManualSpanCallbackGuard(t *testing.T) {
	for _, action := range []string{"pause", "end"} {
		t.Run(action, func(t *testing.T) {
			setupAI(t, WithEmitSpans(true), WithCaptureContent(true))
			ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			accepted := false
			v := &fixtureSerializer{Output: map[string]any{"public": "visible"}}
			v.Action = func() {
				if action == "pause" {
					_, accepted = i.Pause(PauseOptions{})
				} else {
					i.End(EndOptions{})
					accepted = i.Ended()
				}
			}
			_, s := StartSpan(ctx, SpanOptions{Name: "lookup", Operation: "execute_tool", Attributes: map[string]any{"input.value": v}})
			s.End(nil)
			if v.Calls != 1 || accepted {
				t.Fatalf("calls=%d accepted=%v", v.Calls, accepted)
			}
			i.End(EndOptions{})
		})
	}
}

func TestSerializerCanEndAnotherInteraction(t *testing.T) {
	setupAI(t, WithCaptureContent(true), WithLLMCallEvents(true))
	_, owner := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	_, other := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	v := &fixtureSerializer{Output: map[string]any{"public": "visible"}}
	v.Action = func() { other.End(EndOptions{}) }
	owner.RecordCall(fixtureCall(v))
	if v.Calls != 1 || !other.Ended() {
		t.Fatal("serializer refused End on another interaction")
	}
	owner.End(EndOptions{})
}
