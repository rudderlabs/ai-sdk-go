package rudderstackai

import (
	"context"
	"testing"
	"time"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func TestSpanIdentityCapturedFromOwner(t *testing.T) {
	r := setupAI(t, WithEmitSpans(true))
	ctx, alice := StartInteraction(context.Background(), InteractionOptions{AnonymousID: "alice", ConversationID: "alice-conversation"})
	_, s := beginWrapperSpan(ctx, alice, time.Now())
	alice.End(EndOptions{})
	_, bob := StartInteraction(WithValues(ctx, ContextValues{UserID: "bob"}), InteractionOptions{UserID: "bob"})
	defer bob.End(EndOptions{})
	// A raw body can be consumed after the owner ends, under another interaction.
	hook.Tracing.FinishRaw(s, "openai", "m", 200, time.Now(), nil)
	event := r.named("ai_otel_span")[0]
	p := event.Properties
	a := spanAttributes(p)
	if event.AnonymousId != "alice" || event.UserId != "" || p["interaction_id"] != alice.id || p["conversation_id"] != "alice-conversation" {
		t.Fatalf("owner identity lost: %v", p)
	}
	if _, ok := a["user.id"]; ok {
		t.Fatalf("finishing context supplied user: %v", a)
	}
}

func TestSpanParentsStayWithinInteraction(t *testing.T) {
	for _, task := range []bool{false, true} {
		for _, wrapper := range []bool{false, true} {
			t.Run(testBoolName(task)+"/wrapper="+testBoolName(wrapper), func(t *testing.T) {
				r := setupAI(t, WithEmitSpans(true))
				ctx, alice := StartInteraction(context.Background(), InteractionOptions{AnonymousID: "alice"})
				defer alice.End(EndOptions{})
				ctx, parent := StartSpan(ctx, SpanOptions{Name: "alice"})
				defer parent.End(nil)
				check := func(ctx context.Context, owner *Interaction) error {
					var child *Span
					if wrapper {
						_, child = beginWrapperSpan(ctx, owner, time.Now())
					} else {
						_, child = StartSpan(ctx, SpanOptions{Name: "child"})
					}
					if wrapper {
						hook.Tracing.FinishRaw(child, "openai", "m", 200, time.Now(), nil)
					} else {
						child.End(nil)
					}
					_, sibling := StartSpan(ctx, SpanOptions{Name: "sibling"})
					sibling.End(nil)
					event := r.named("ai_otel_span")[0]
					p := event.Properties
					if child.TraceID == parent.TraceID || child.parentID != "" || sibling.TraceID != child.TraceID {
						t.Errorf("foreign parent inherited or interaction trace unstable: child=%s parent=%s sibling=%s", child.TraceID, parent.TraceID, sibling.TraceID)
					}
					if event.AnonymousId == "alice" || event.AnonymousId == parent.TraceID {
						t.Errorf("mapper joined interactions: %v", p)
					}
					if task && event.AnonymousId != systemAnonymousID {
						t.Errorf("system task mapper fallback=%v want product fallback %s", event.AnonymousId, systemAnonymousID)
					}
					return nil
				}
				if task {
					if err := RunTask(ctx, "unrelated", TaskOptions{NoParent: true, Flush: Ptr(false)}, check); err != nil {
						t.Fatal(err)
					}
				} else {
					ctx, bob := StartInteraction(ctx, InteractionOptions{UserID: "bob"})
					defer bob.End(EndOptions{})
					_ = check(ctx, bob)
				}
			})
		}
	}
}

func testBoolName(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
