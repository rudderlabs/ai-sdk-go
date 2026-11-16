package rudderstackai

import (
	"context"
	"testing"
)

func TestSharedIntegrationSpansFixture(t *testing.T) {
	var fixture struct {
		Version   int `json:"fixture_version"`
		Scenarios []struct{ Name, Integration string }
	}
	loadFixture(t, "spec/fixtures/integration-spans-v0.1.json", &fixture)
	if fixture.Version != 1 || len(fixture.Scenarios) == 0 {
		t.Fatal("missing integration span scenarios or unsupported fixture version")
	}
	limits := map[string]string{
		"langchain":        "Go has no LangChain integration; framework callbacks are available in Node and Python",
		"openai-agents":    "Go has no OpenAI Agents SDK integration; available in Node and Python",
		"claude-agent-sdk": "Go has no Claude Agent SDK integration; available in Node and Python",
		"vercel-ai-sdk":    "Go has no Vercel AI SDK integration; available only in Node",
	}
	for _, sc := range fixture.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			if reason, ok := limits[sc.Integration]; ok {
				t.Skip("platform limit: " + reason)
			}
			t.Fatalf("integration %q needs a Go driver or an explicit platform limit", sc.Integration)
		})
	}
}

func TestSpanSourceSchema(t *testing.T) {
	r := setupAI(t, WithEmitSpans(true))
	_, span := StartSpan(context.Background(), SpanOptions{Name: "agent", Operation: "invoke_agent"})
	span.End(nil)
	props := r.named("ai_otel_span")[0].Properties
	encoded := props["otlp"].(map[string]any)["resourceSpans"].([]map[string]any)[0]["scopeSpans"].([]map[string]any)[0]["spans"].([]map[string]any)[0]
	for _, kv := range encoded["attributes"].([]map[string]any) {
		if kv["key"] != "rudderstack.span_source" {
			continue
		}
		for _, source := range []string{"wrapper", "manual", "integration", "unknown"} {
			t.Run(source, func(t *testing.T) {
				kv["value"] = map[string]any{"stringValue": source}
				err := validateEvent("ai_otel_span", props)
				if source == "unknown" {
					if err == nil {
						t.Fatal("schema accepted an unknown span source")
					}
				} else if err != nil {
					t.Fatal(err)
				}
			})
		}
		return
	}
	t.Fatal("span source attribute missing")
}
