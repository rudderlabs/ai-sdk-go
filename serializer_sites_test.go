package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

type finalReviewPrivateContext struct{ Secret string }

func (finalReviewPrivateContext) MarshalJSON() ([]byte, error) {
	return []byte(`{"public":"safe"}`), nil
}

func TestFinalReviewPrivateSpanContext(t *testing.T) {
	setupAI(t, WithEmitSpans(true), WithCaptureContent(false))
	dp := dataplane.Start()
	defer dp.Close()
	sender, err := analytics.NewWithConfig("test-key", analytics.Config{DataPlaneUrl: dp.URL, BatchSize: 1, NoProxySupport: true, Interval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err = Configure(WithAnalytics(sender)); err != nil {
		t.Fatal(err)
	}
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false), Context: map[string]any{"custom": finalReviewPrivateContext{Secret: "PRIVATE_CONTEXT_CANARY"}}})
	_, s := StartSpan(ctx, SpanOptions{Name: "lookup", Operation: "execute_tool"})
	s.End(nil)
	i.End(EndOptions{})
	Flush(context.Background())
	if err = sender.Close(); err != nil {
		t.Fatal(err)
	}
	events := dp.Wait(3, 5*time.Second)
	if len(events) != 3 {
		t.Fatalf("got %d events", len(events))
	}
	for _, event := range events {
		name := event.Event
		b, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: private=%t safe=%t", name, strings.Contains(string(b), "PRIVATE_CONTEXT_CANARY"), strings.Contains(string(b), "safe"))
		if strings.Contains(string(b), "PRIVATE_CONTEXT_CANARY") {
			t.Error("serializer-hidden context leaked on", name)
		}
	}
}

type finalReviewSharedSerializer struct{ Calls int }

func (v *finalReviewSharedSerializer) MarshalJSON() ([]byte, error) {
	return testkit.CaptureJSON(&v.Calls)
}
func TestFinalReviewToolEventSerializerCount(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprint(spans), func(t *testing.T) {
			v := &finalReviewSharedSerializer{}
			r := setupAI(t, WithEmitSpans(spans), WithCaptureContent(true), WithLLMCallEvents(true), WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "looked_up", Properties: func(args map[string]any, result any) map[string]any {
				return map[string]any{"input": args["value"], "output": result}
			}}}}))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup", Args: map[string]any{"value": v}})
			i.RecordToolResult("t", ToolResult{Result: v, Error: Ptr(false)})
			i.RecordCall(LLMCall{Provider: "openai", Model: "m", OutputToolCalls: []GenAIToolCall{{Name: "lookup", Arguments: v}}})
			i.End(EndOptions{})
			event, _ := json.Marshal(r.named("looked_up")[0].Properties)
			call, _ := json.Marshal(r.named("ai_llm_response_received")[0].Properties["output_tool_calls"])
			t.Logf("serializer calls=%d; tool event=%s; call=%s", v.Calls, event, call)
			if v.Calls != 3 {
				t.Error("tool event capture bypassed shared serializer result")
			}
		})
	}
}
