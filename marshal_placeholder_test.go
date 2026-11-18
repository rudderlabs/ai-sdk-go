package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rudderlabs/ai-sdk-go/internal/spanmodel"
)

type failingCaptureText struct {
	Secret string
	Panic  bool
	Calls  int
}

func (v *failingCaptureText) MarshalText() ([]byte, error) {
	v.Calls++
	if v.Panic {
		panic("text serializer")
	}
	return nil, fmt.Errorf("text serializer")
}

func TestCaptureFailurePlaceholders(t *testing.T) {
	for _, spans := range []bool{false, true} {
		for _, kind := range []string{"json-error", "json-panic", "text-error", "text-panic"} {
			t.Run(fmt.Sprintf("%t/%s", spans, kind), func(t *testing.T) {
				rec := setupAI(t, WithEmitSpans(spans), WithCaptureContent(true), WithLLMCallEvents(true))
				j := &captureSerializer{Hidden: "HIDDEN_CANARY", Fail: true, Panic: strings.HasSuffix(kind, "panic")}
				x := &failingCaptureText{Secret: "HIDDEN_CANARY", Panic: strings.HasSuffix(kind, "panic")}
				var value any = j
				if strings.HasPrefix(kind, "text") {
					value = x
				}
				ctx, owner := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
				owner.RecordCall(LLMCall{Provider: "openai", Model: "m", InputMessages: []GenAIMessage{{Role: "tool", Parts: []GenAIPart{{"type": "tool_call_response", "response": value}}}}, OutputToolCalls: []GenAIToolCall{{Name: "lookup", Arguments: map[string]any{"failed": value}}}})
				_, span := StartSpan(ctx, SpanOptions{Name: "retrieval", Operation: "retrieval", Attributes: map[string]any{"input.value": value}})
				span.End(nil)
				owner.End(EndOptions{})
				events := rec.named("ai_llm_response_received")
				if len(events) != 1 {
					t.Fatalf("calls=%d", len(events))
				}
				wire, _ := json.Marshal(events[0].Properties)
				for _, want := range []string{`"response":null`, `"failed":null`} {
					if !strings.Contains(string(wire), want) {
						t.Fatalf("missing %s: %s", want, wire)
					}
				}
				all, _ := json.Marshal(rec.named("ai_otel_span"))
				if strings.Contains(string(wire)+string(all), "HIDDEN_CANARY") {
					t.Fatalf("hidden fields captured: %s %s", wire, all)
				}
				if spans && !strings.Contains(string(all), `"key":"input.value"`) {
					t.Fatalf("span placeholder omitted: %s", all)
				}
				if j.Calls.Load()+int32(x.Calls) != map[bool]int32{false: 1, true: 2}[spans] {
					t.Fatalf("json=%d text=%d", j.Calls.Load(), x.Calls)
				}
			})
		}
	}
}

// External span attributes and events use this mapper even with SDK spans off.
func TestCaptureFailureSpanEvents(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprint(spans), func(t *testing.T) {
			setupAI(t, WithEmitSpans(spans), WithCaptureContent(true))
			j := &captureSerializer{Hidden: "HIDDEN_CANARY", Fail: true}
			x := &failingCaptureText{Secret: "HIDDEN_CANARY", Panic: true}
			attributes := map[string]any{"input.value": map[string]any{"json": j, "text": x}}
			captured := captureValue(map[string]any{"attributes": attributes, "events": attributes}).value.(map[string]any)
			track, err := spanmodel.ToTrack(spanmodel.Span{
				Name: "retrieval", Start: time.Now(), End: time.Now(), Attributes: captured["attributes"].(map[string]any),
				Events: []spanmodel.Event{{Name: "gen_ai.content", Time: time.Now(), Attributes: captured["events"].(map[string]any)}},
			}, spanmodel.Options{})
			if err != nil || track == nil {
				t.Fatalf("track=%v err=%v", track, err)
			}
			wire, _ := json.Marshal(track.Properties)
			// One copy in span attributes and one in the event, both explicit nulls.
			if strings.Contains(string(wire), "HIDDEN_CANARY") || strings.Count(string(wire), `"key":"json","value":{}`) != 2 || strings.Count(string(wire), `"key":"text","value":{}`) != 2 {
				t.Fatalf("placeholders=%s", wire)
			}
			if j.Calls.Load() != 1 || x.Calls != 1 {
				t.Fatalf("json=%d text=%d", j.Calls.Load(), x.Calls)
			}
		})
	}
}

type captureSerializer struct {
	Hidden string
	Wire   string
	Fail   bool
	Panic  bool
	Calls  atomic.Int32
}

func (s *captureSerializer) MarshalJSON() ([]byte, error) {
	n := s.Calls.Add(1)
	if s.Panic {
		panic("serializer")
	}
	if s.Fail {
		return nil, errors.New("serializer")
	}
	return fmt.Appendf(nil, `{"visible":"%s-%d"}`, s.Wire, n), nil
}
