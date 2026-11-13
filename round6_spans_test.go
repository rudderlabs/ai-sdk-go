package rudderstackai

import (
	"context"
	"testing"
	"time"
)

func TestEndedInteractionNewSpansUseCurrentContext(t *testing.T) {
	for _, wrapper := range []bool{false, true} {
		t.Run(testBoolName(wrapper), func(t *testing.T) {
			aliceClient := setupAI(t, WithEmitSpans(true))
			bobClient := newRecorder(t)
			ctx, alice := StartInteraction(context.Background(), InteractionOptions{UserID: "alice"})
			ctx, parent := StartSpan(ctx, SpanOptions{Name: "alice parent"})
			defer parent.End(nil)
			alice.End(EndOptions{})
			ctx = WithValues(ctx, ContextValues{UserID: "bob", Analytics: bobClient})
			var s *Span
			if wrapper {
				_, s = beginWrapperSpan(ctx, alice, time.Now())
			} else {
				_, s = StartSpan(ctx, SpanOptions{Name: "late"})
			}
			if s == nil {
				t.Fatal("late span disabled")
			}
			s.End(nil)
			spans := bobClient.named("ai_otel_span")
			if len(spans) != 1 {
				t.Fatalf("Bob spans=%d Alice spans=%d", len(spans), len(aliceClient.named("ai_otel_span")))
			}
			if spans[0].UserId != "bob" || s.interactionID != "" || s.parentID != "" || s.TraceID == parent.TraceID {
				t.Fatalf("ended owner captured: %+v", spans[0])
			}
		})
	}
}

func TestWalkerCountsPointerAndEmbeddedStructHops(t *testing.T) {
	type Node struct {
		*Node
		Value string `json:",omitempty"`
	}
	for _, embedded := range []bool{false, true} {
		t.Run(testBoolName(embedded), func(t *testing.T) {
			leaf := &Node{Value: "too deep"}
			for range 100 {
				leaf = &Node{Node: leaf}
			}
			if embedded {
				got := stripBinary(leaf).(map[string]any)
				if got["Value"] == "too deep" {
					t.Fatalf("embedded chain bypassed limit: %v", got)
				}
			} else {
				var value any = "too deep"
				for range 100 {
					v := value
					value = &v
				}
				if got := stripBinary(value); got != nil {
					t.Fatalf("pointer chain bypassed limit: %v", got)
				}
			}
		})
	}
}

func TestWalkerDepthMatchesMainBoundary(t *testing.T) {
	type Node struct {
		*Node
		Value string `json:",omitempty"`
	}
	leaf := &Node{Value: "within main's limit"}
	for range 60 {
		leaf = &Node{Node: leaf}
	}
	if got := stripBinary(leaf).(map[string]any)["Value"]; got != "within main's limit" {
		t.Fatalf("embedded hops differ from main: %v", got)
	}
	var value any = "past main's limit"
	for range 40 {
		v := value
		value = &v
	}
	if got := stripBinary(value); got != nil {
		t.Fatalf("pointer/interface hops differ from main: %v", got)
	}
}
