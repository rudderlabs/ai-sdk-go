package conformance

import (
	"context"
	"fmt"
	"testing"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

func TestProviderWrappedTwiceEmitsOneSpan(t *testing.T) {
	routes := map[string]string{
		"openai_chat":                   "openai.chat",
		"openai_chat_stream":            "openai.chat",
		"openai_responses":              "openai.responses",
		"openai_responses_stream":       "openai.responses",
		"anthropic_create":              "anthropic.messages",
		"anthropic_create_stream":       "anthropic.messages",
		"google_generate_plain":         "google.generate",
		"google_vertex_generate_plain":  "google.generate",
		"google_generate_stream":        "google.stream",
		"bedrock_converse_tool_use":     "bedrock.converse",
		"bedrock_converse_stream_tools": "bedrock.converse_stream",
	}
	for _, sc := range loadFixture(t) {
		route, selected := routes[sc.Name]
		if !selected {
			continue
		}
		delete(routes, sc.Name)
		for _, ephemeral := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ephemeral=%v", sc.Name, ephemeral), func(t *testing.T) {
				rec := &recorder{}
				configure(t, obj{"emit_spans": true, "emit_llm_calls": true}, rec)
				llm.Reset()
				for _, raw := range sc.Replies[route] {
					var reply testkit.Reply
					if err := remarshal(raw, &reply); err != nil {
						t.Fatal(err)
					}
					llm.Script(route, reply)
				}
				var step obj
				for _, outer := range sc.Steps {
					for _, inner := range objs0(outer["steps"]) {
						if inner["op"] == "call" {
							if step != nil {
								t.Fatal("fixture must contain exactly one provider call")
							}
							step = inner
						}
					}
				}
				if step == nil {
					t.Fatal("fixture has no provider call")
				}
				step["wrap_twice"] = true
				r := &run{t: t, llm: llm}
				ctx := context.Background()
				var interaction *rudderstackai.Interaction
				if !ephemeral {
					ctx, interaction = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "double-wrap"})
					defer interaction.End(rudderstackai.EndOptions{})
				}
				if err := apis[str(step["api"])](ctx, r, step); err != nil {
					t.Fatal(err)
				}
				if interaction != nil {
					interaction.End(rudderstackai.EndOptions{})
				}
				rudderstackai.Flush(ctx)
				if requests := llm.Requests(route); len(requests) != 1 {
					t.Fatalf("provider requests=%d, want 1", len(requests))
				}
				events := map[string][]obj{}
				for _, event := range rec.all() {
					props := jsonRoundTrip(event.Properties).(obj)
					if err := testkit.ValidateEvent(event.Event, props); err != nil {
						t.Fatal(err)
					}
					events[event.Event] = append(events[event.Event], props)
				}
				for _, name := range []string{"ai_otel_span", "ai_llm_response_received", "ai_response_received"} {
					if got := len(events[name]); got != 1 {
						t.Fatalf("%s events=%d, want 1", name, got)
					}
				}
				span, call := events["ai_otel_span"][0], events["ai_llm_response_received"][0]
				id, ok := span["span_id"].(string)
				if !ok || id == "" || call["span_id"] != id || call["interaction_id"] != span["interaction_id"] {
					t.Fatalf("span and product call do not join: span=%v call=%v", span, call)
				}
				if count := events["ai_response_received"][0]["llm_call_count"]; count != float64(1) {
					t.Fatalf("product llm_call_count=%v, want 1", count)
				}
			})
		}
	}
	if len(routes) != 0 {
		t.Fatalf("missing fixture scenarios: %v", routes)
	}
}
