package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type review3258Dual struct {
	Secret string `json:"-"`
}

func (v review3258Dual) MarshalText() ([]byte, error) { return []byte(v.Secret), nil }
func (*review3258Dual) MarshalJSON() ([]byte, error)  { return []byte(`"PUBLIC"`), nil }

func TestReview3258PointerJSONPrecedence(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprintf("spans=%t", spans), func(t *testing.T) {
			r := setupAI(t, WithEmitSpans(spans), WithLLMCallEvents(true), WithCaptureContent(true))
			v := &struct{ Child review3258Dual }{Child: review3258Dual{Secret: "PRIVATE_CUSTOMER_VALUE"}}
			wire, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			i.RecordCall(LLMCall{Provider: "openai", Model: "m", OutputToolCalls: []GenAIToolCall{{Name: "lookup", Arguments: v}}})
			i.End(EndOptions{})
			got, _ := json.Marshal(r.named("ai_llm_response_received")[0].Properties["output_tool_calls"])
			t.Logf("encoding/json=%s; captured=%s", wire, got)
			if strings.Contains(string(got), "PRIVATE_CUSTOMER_VALUE") {
				t.Fatal("private value escaped the selected JSON serializer")
			}
		})
	}
}

type review3258NonComparable struct {
	Values []string
	Calls  *int
}

func (v review3258NonComparable) MarshalJSON() ([]byte, error) {
	*v.Calls++
	return []byte(fmt.Sprintf(`"SERIALIZED_%d"`, *v.Calls)), nil
}
func TestReview3258NonComparableSerializerOnce(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprintf("spans=%t", spans), func(t *testing.T) {
			r := setupAI(t, WithEmitSpans(spans), WithLLMCallEvents(true), WithCaptureContent(true))
			n := 0
			var v any = &review3258NonComparable{Values: []string{"safe"}, Calls: &n}
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			i.RecordCall(LLMCall{Provider: "openai", Model: "m", InputMessages: []GenAIMessage{{Role: "tool", Parts: []GenAIPart{{"type": "tool_call_response", "response": v}}}}, OutputToolCalls: []GenAIToolCall{{Name: "lookup", Arguments: v}}})
			i.End(EndOptions{})
			got, _ := json.Marshal(r.named("ai_llm_response_received")[0].Properties)
			t.Logf("serializer calls=%d; first=%t second=%t", n, strings.Contains(string(got), "SERIALIZED_1"), strings.Contains(string(got), "SERIALIZED_2"))
			if n != 1 {
				t.Fatal("same captured value serialized repeatedly")
			}
		})
	}
}

type review3258SharedSerializer struct{ Calls int }

func (v *review3258SharedSerializer) MarshalJSON() ([]byte, error) {
	v.Calls++
	return []byte(fmt.Sprintf(`"CAPTURE_%d"`, v.Calls)), nil
}
func TestReview3258SerializerAcrossTwinAndRepeat(t *testing.T) {
	for _, spans := range []bool{false, true} {
		for _, order := range []string{"wrapper-span", "span-wrapper", "repeat"} {
			t.Run(fmt.Sprintf("spans=%t/%s", spans, order), func(t *testing.T) {
				r := setupAI(t, WithEmitSpans(spans), WithLLMCallEvents(true), WithCaptureContent(true))
				v := &review3258SharedSerializer{}
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
				c := LLMCall{Provider: "openai", Model: "m", ResponseID: "same", InputTokens: 10, OutputToolCalls: []GenAIToolCall{{Name: "lookup", Arguments: v}}, Integration: "openai"}
				if order == "span-wrapper" {
					c.Integration = "tracing"
				}
				i.RecordCall(c)
				switch order {
				case "span-wrapper":
					c.Integration = "openai"
				case "wrapper-span":
					c.Integration = "tracing"
				}
				i.RecordCall(c)
				i.End(EndOptions{})
				got, _ := json.Marshal(r.named("ai_llm_response_received")[0].Properties["output_tool_calls"])
				t.Logf("serializer calls=%d; events=%d; content=%s", v.Calls, len(r.named("ai_llm_response_received")), got)
				if v.Calls != 2 {
					t.Fatal("single retained call invokes serializer twice")
				}
			})
		}
	}
}

type captureAddressableJSON struct{ Calls *int }

func (v *captureAddressableJSON) MarshalJSON() ([]byte, error) {
	*v.Calls++
	return []byte(`{"public":true}`), nil
}
func TestCaptureAddressablePointerMemo(t *testing.T) {
	setupAI(t, WithEmitSpans(true))
	calls := 0
	values := []captureAddressableJSON{{Calls: &calls}}
	_ = captureValue([]any{values, values})
	if calls != 1 {
		t.Fatalf("calls=%d want=1", calls)
	}
}
