package conformance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
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
			calls := []any{}
			for _, m := range rec.all() {
				if m.Event == "ai_otel_span" {
					id := m.Properties["span_id"]
					if spans[id] {
						t.Fatal("span emitted twice")
					}
					spans[id] = true
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
