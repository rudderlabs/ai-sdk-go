package rudderstackai

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"weak"
)

func TestExecutingRuleSliceSeed(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprint(spans), func(t *testing.T) {
			a := &fixtureSerializer{Output: "A"}
			b := &fixtureSerializer{Output: "B"}
			r := setupAI(t, WithEmitSpans(spans), WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "done", Properties: func(map[string]any, any) map[string]any { return map[string]any{"a": a, "b": b} }}}}))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup", Result: []any{[]byte("private"), a, b}, Error: Ptr(false)})
			i.End(EndOptions{})
			p := r.props(t, "done")
			if p["a"] != "A" || p["b"] != "B" || a.Calls != 1 || b.Calls != 1 {
				t.Fatalf("props=%v counts=%d/%d", p, a.Calls, b.Calls)
			}
		})
	}
}
func TestExecutingToolRetention(t *testing.T) {
	for _, rule := range []bool{false, true} {
		t.Run(fmt.Sprint(rule), func(t *testing.T) {
			options := []ConfigOption{WithCaptureContent(true)}
			if rule {
				options = append(options, WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "done"}}}))
			}
			setupAI(t, options...)
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			ref := func() weak.Pointer[fixtureSerializer] {
				v := &fixtureSerializer{Fields: make([]byte, 1<<20), Output: "visible"}
				i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup", Result: v, Error: Ptr(false)})
				return weak.Make(v)
			}()
			fixtureGC()
			if ref.Value() != nil {
				t.Fatal("settled tool retains original result")
			}
			i.End(EndOptions{})
			runtime.KeepAlive(i)
		})
	}
}
func TestExecutingFailureIsolationAndSchema(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprint(spans), func(t *testing.T) {
			r := setupAI(t, WithEmitSpans(spans), WithCaptureContent(true), WithLLMCallEvents(true), WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "done", Properties: func(map[string]any, any) map[string]any {
				return map[string]any{"custom": &fixtureSerializer{Failure: "panic"}, "valid": "visible"}
			}}}}))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			i.RecordCall(LLMCall{Provider: "openai", Model: "m", InputTokens: 2, OutputTokens: 1, InputMessages: []GenAIMessage{{Role: "user", Parts: []GenAIPart{{"type": "text", "content": &fixtureSerializer{Failure: "error"}}, {"type": "text", "content": "visible"}}}}, OutputToolCalls: []GenAIToolCall{{Name: "lookup", Arguments: &fixtureSerializer{Failure: "invalid_json"}}}})
			i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup", Result: &fixtureSerializer{Failure: "panic"}, Error: Ptr(false)})
			i.End(EndOptions{})
			calls := r.named("ai_llm_response_received")
			if len(calls) != 1 || calls[0].Properties["input_tokens"] != 2 {
				t.Fatalf("lost accounting %v", calls)
			}
			p := r.props(t, "done")
			if p["custom"] != nil || p["valid"] != "visible" {
				t.Fatalf("failure discarded sibling %v", p)
			}
			for _, event := range r.all() {
				if err := validateEvent(event.Event, event.Properties); err != nil {
					t.Fatalf("%s: %v", event.Event, err)
				}
			}
		})
	}
}

func TestExecutingRuleMapKeys(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprint(spans), func(t *testing.T) {
			r := setupAI(t, WithEmitSpans(spans), WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "done", Properties: func(map[string]any, any) map[string]any {
				return map[string]any{"custom": map[reviewTextKey]any{12345: "visible"}}
			}}}}))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup", Result: "ok", Error: Ptr(false)})
			i.End(EndOptions{})
			if got := mustJSON(r.props(t, "done")["custom"]); got != `{"public":"visible"}` {
				t.Fatalf("key privacy=%s", got)
			}
		})
	}
}
