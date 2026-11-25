package rudderotel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// toolOutputCheck records spans into a fresh interaction declaring lookup_done, then checks
// lookup_done and lookup's tool_breakdown entry. It returns lookup_done's properties.
func toolOutputCheck(t *testing.T, record func(i *rudderstackai.Interaction, span func(sdktrace.Status, ...attribute.KeyValue)), status string, errorType any) (event object) {
	t.Helper()
	hook.Reset()
	t.Cleanup(hook.Reset)
	rec, events := recorder(t)
	if err := rudderstackai.Configure(rudderstackai.WithAnalytics(rec), rudderstackai.WithToolBreakdown(true), rudderstackai.WithEventRules(rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{"lookup": {Event: "lookup_done"}}})); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = rudderstackai.Configure(rudderstackai.WithToolBreakdown(false), rudderstackai.WithEventRules(rudderstackai.EventRules{}))
	})
	_, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	record(i, func(s sdktrace.Status, attrs ...attribute.KeyValue) {
		attrs = append(attrs, attribute.String("rudderstack.interaction_id", i.ID()))
		recordSpan(tracetest.SpanStub{Name: "span", StartTime: time.Now().Add(-time.Millisecond), EndTime: time.Now(), Attributes: attrs, Status: s}.Snapshot(), rec, i)
	})
	i.End(rudderstackai.EndOptions{})
	errors, sent := 0, 0
	if status == "error" {
		errors = 1
	}
	for _, e := range events() {
		p := e["properties"].(object)
		switch e["event"] {
		case "lookup_done":
			sent++
			event = p
			if p["tool_status"] != status || p["error_type"] != errorType || p["tool_call_id"] != "call-1" {
				t.Errorf("lookup_done %v", p)
			}
		case "ai_response_received":
			for _, entry := range p["tool_breakdown"].([]any) {
				if b := entry.(object); b["tool_name"] == "lookup" && b["error_count"] != float64(errors) {
					t.Errorf("tool_breakdown %v", b)
				}
			}
		}
	}
	if sent != 1 {
		t.Errorf("lookup_done sent %d times", sent)
	}
	return event
}

// Tool span results are decoded before the check, as Node.js does, so only the decoded value is read.
func TestToolSpanOutputFailure(t *testing.T) {
	large, _ := json.Marshal(map[string]any{"successful": false, "data": strings.Repeat("x", 65536)})
	for _, c := range []struct {
		name, result, status string
		spanStatus           codes.Code
		errorType            any
	}{
		{"successful false", `{"successful":false}`, "error", codes.Unset, "api_error"},
		{"larger than the raw parse bound", string(large), "error", codes.Unset, "api_error"},
		{"double-encoded", `"{\"successful\":false}"`, "success", codes.Unset, nil},
		{"successful true", `{"successful":true}`, "success", codes.Unset, nil},
		{"span error keeps its outcome", `{"successful":false}`, "error", codes.Error, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			event := toolOutputCheck(t, func(_ *rudderstackai.Interaction, span func(sdktrace.Status, ...attribute.KeyValue)) {
				span(sdktrace.Status{Code: c.spanStatus}, attribute.String("gen_ai.operation.name", "execute_tool"), attribute.String("gen_ai.tool.name", "lookup"),
					attribute.String("gen_ai.tool.call.id", "call-1"), attribute.String("gen_ai.tool.call.result", c.result))
			}, c.status, c.errorType)
			// The span's measured duration survives the classification.
			if _, ok := event["tool_duration_ms"].(float64); !ok {
				t.Errorf("tool_duration_ms %v", event["tool_duration_ms"])
			}
		})
	}
}

// A model span's input passes tool responses as they are, so a JSON string is decoded for the check only.
func TestModelInputToolResponseFailure(t *testing.T) {
	for _, c := range []struct{ name, response string }{
		{"object", `{"successful": false}`},
		{"JSON string", `"{\"successful\": false}"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			toolOutputCheck(t, func(i *rudderstackai.Interaction, span func(sdktrace.Status, ...attribute.KeyValue)) {
				i.RecordToolCalls(rudderstackai.ToolCall{ID: "call-1", Name: "lookup"})
				span(sdktrace.Status{}, attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.request.model", "gpt-4o"),
					attribute.String("gen_ai.request.tool_choice", `{"type": "function", "function": {"name": "Schema"}}`),
					attribute.String("gen_ai.input.messages", `[{"role": "tool", "parts": [{"type": "tool_call_response", "id": "call-1", "response": `+c.response+`}]}]`))
			}, "error", "api_error")
		})
	}
}
