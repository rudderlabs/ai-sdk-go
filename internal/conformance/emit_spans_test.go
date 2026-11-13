package conformance

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

// Replay provider fixtures through the real Go SDK clients with spans enabled.
func TestProviderEmitSpans(t *testing.T) {
	names := strings.Fields("openai_chat openai_chat_stream openai_responses openai_responses_stream anthropic_create anthropic_create_stream anthropic_stream_helper openai_429 anthropic_429 openai_timeout openai_stream_break openai_responses_stream_no_terminal anthropic_stream_no_message_stop openai_responses_cancelled openai_chat_stream_tool_then_error openai_chat_stream_usage_then_error openai_500 anthropic_529 openai_responses_failed anthropic_stream_partial_delta anthropic_stream_abort_mid anthropic_stream_error_keeps_tokens openai_429_wrapped_twice openai_chat_stream_error_wrapped_twice openai_chat_stream_usage_param_rejected google_generate_plain google_vertex_generate_plain google_generate_stream google_stream_tool_call google_429 google_400 google_403 google_stream_code_execution bedrock_converse_stream_tools bedrock_converse_stream_break bedrock_converse_stream_no_message_stop")
	selected := map[string]bool{}
	for _, n := range names {
		selected[n] = true
	}
	for _, sc := range loadFixture(t) {
		if !selected[sc.Name] {
			continue
		}
		delete(selected, sc.Name)
		t.Run(sc.Name, func(t *testing.T) {
			rec := &recorder{}
			sc.Config["emit_spans"] = true
			sc.Config["emit_llm_calls"] = true
			configure(t, sc.Config, rec)
			llm.Reset()
			var callMu sync.Mutex
			observed := map[string]rudderstackai.LLMCall{}
			recordWrapper := hook.Tracing.RecordWrapper
			hook.Tracing.RecordWrapper = func(i, value, span any, status int) bool {
				if s, ok := span.(*rudderstackai.Span); ok && s != nil {
					callMu.Lock()
					observed[s.SpanID] = value.(rudderstackai.LLMCall)
					callMu.Unlock()
				}
				return recordWrapper(i, value, span, status)
			}
			t.Cleanup(func() { hook.Tracing.RecordWrapper = recordWrapper })
			for route, replies := range sc.Replies {
				for _, rp := range replies {
					raw, _ := json.Marshal(rp)
					var reply testkit.Reply
					if err := json.Unmarshal(raw, &reply); err != nil {
						t.Fatal(err)
					}
					llm.Script(route, reply)
				}
			}
			r := &run{t: t, llm: llm}
			if err := r.steps(context.Background(), sc.Steps); err != nil {
				t.Fatal(err)
			}
			rudderstackai.Flush(context.Background())
			spans := map[any]bool{}
			callEvents := map[any]obj{}
			for _, m := range rec.all() {
				if m.Event == "ai_llm_response_received" {
					callEvents[m.Properties["span_id"]] = jsonRoundTrip(m.Properties).(obj)
				}
			}
			calls := []any{}
			for _, m := range rec.all() {
				if m.Event == "ai_otel_span" {
					id := m.Properties["span_id"]
					if spans[id] {
						t.Fatal("span emitted twice")
					}
					spans[id] = true
					callMu.Lock()
					call, ok := observed[id.(string)]
					callMu.Unlock()
					if !ok {
						t.Fatal("span has no observed provider completion")
					}
					p := jsonRoundTrip(m.Properties).(obj)
					want := obj{"gen_ai.operation.name": "chat", "rudderstack.span_source": "wrapper", "gen_ai.provider.name": call.Provider, "gen_ai.request.model": call.Model, "gen_ai.response.model": call.Model}
					if call.RequestedModel != "" {
						want["gen_ai.request.model"] = call.RequestedModel
					}
					if call.ResponseID != "" {
						want["gen_ai.response.id"] = call.ResponseID
					}
					if !call.UsageUnknown {
						want["gen_ai.usage.input_tokens"] = call.InputTokens
						want["gen_ai.usage.output_tokens"] = call.OutputTokens
					}
					if call.FinishReason != "" {
						want["gen_ai.response.finish_reasons"] = []string{call.FinishReason}
					}
					if m.UserId != "" {
						want["user.id"] = m.UserId
					}
					if m.UserId == "" && m.AnonymousId != "" {
						want["rudderstack.anonymous_id"] = m.AnonymousId
					}
					for attr, key := range map[string]string{"rudderstack.interaction_id": "interaction_id", "gen_ai.conversation.id": "conversation_id"} {
						if p[key] != nil {
							want[attr] = p[key]
						}
					}
					if c := callEvents[id]; c != nil {
						if v, ok := c["input_messages"]; ok {
							want["gen_ai.input.messages"] = v
						}
						parts := []any{}
						if v, ok := c["output_text"]; ok {
							parts = append(parts, obj{"type": "text", "content": v})
						}
						if tools, ok := c["output_tool_calls"].([]any); ok {
							for _, raw := range tools {
								tool := raw.(obj)
								part := obj{"type": "tool_call", "name": tool["name"]}
								if args, ok := tool["arguments"]; ok {
									part["arguments"] = args
								}
								parts = append(parts, part)
							}
						}
						if len(parts) > 0 {
							want["gen_ai.output.messages"] = []any{obj{"role": "assistant", "parts": parts}}
						}
					}
					encoded := p["otlp"].(obj)["resourceSpans"].([]any)[0].(obj)["scopeSpans"].([]any)[0].(obj)["spans"].([]any)[0].(obj)
					got := obj{}
					for _, raw := range encoded["attributes"].([]any) {
						kv := raw.(obj)
						got[kv["key"].(string)] = decodeProviderAnyValue(kv["value"].(obj))
					}
					if !reflect.DeepEqual(jsonRoundTrip(got), jsonRoundTrip(want)) {
						t.Errorf("full attributes: got %v want %v", got, want)
					}

					if m.Context.Extra["wrapperLibrary"].(obj)["integration"] != "tracing" {
						t.Fatal("wrong span integration")
					}
					if err := testkit.ValidateEvent(m.Event, jsonRoundTrip(m.Properties).(obj)); err != nil {
						t.Fatal(err)
					}
				}
				if m.Event == "ai_llm_response_received" {
					calls = append(calls, m.Properties["span_id"])
				}
			}
			if len(spans) == 0 || len(spans) != len(calls) {
				t.Fatalf("spans=%d calls=%d", len(spans), len(calls))
			}
			for _, id := range calls {
				if !spans[id] {
					t.Fatalf("call references missing span %v", id)
				}
			}
		})
	}
	if len(selected) > 0 {
		t.Fatalf("missing fixture scenarios: %v", selected)
	}
}

func decodeProviderAnyValue(v obj) any {
	for _, key := range []string{"stringValue", "boolValue", "doubleValue"} {
		if x, ok := v[key]; ok {
			return x
		}
	}
	if x, ok := v["intValue"]; ok {
		return json.Number(x.(string))
	}
	if x, ok := v["arrayValue"].(obj); ok {
		out := []any{}
		for _, raw := range x["values"].([]any) {
			out = append(out, decodeProviderAnyValue(raw.(obj)))
		}
		return out
	}
	if x, ok := v["kvlistValue"].(obj); ok {
		out := obj{}
		for _, raw := range x["values"].([]any) {
			kv := raw.(obj)
			out[kv["key"].(string)] = decodeProviderAnyValue(kv["value"].(obj))
		}
		return out
	}
	return nil
}
