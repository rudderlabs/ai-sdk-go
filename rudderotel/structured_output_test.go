package rudderotel

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func TestStructuredOutputSpans(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name             string
			Attributes       map[string]any
			ResultAttributes map[string]any `json:"result_attributes"`
			Count            int            `json:"expected_tool_call_count"`
			NoExclusion      bool           `json:"no_exclusion"`
		} `json:"span_cases"`
	}
	loadCases(t, "../spec/fixtures/structured-output-v0.1.json", &fixture)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			hook.Reset()
			t.Cleanup(hook.Reset)
			logs := &structuredLog{}
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(previous) })
			t.Setenv("RUDDERSTACK_AI_DEBUG", "1")
			rec, events := recorder(t)
			if err := rudderstackai.Configure(rudderstackai.WithAnalytics(rec), rudderstackai.WithToolBreakdown(true), rudderstackai.WithEventRules(rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{"Schema": {Event: "schema_done"}}})); err != nil {
				t.Fatal(err)
			}
			_, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			span := func(extra map[string]any) {
				attrs := map[string]any{"rudderstack.interaction_id": i.ID(), "gen_ai.operation.name": "chat", "gen_ai.request.model": "gpt-4o", "gen_ai.usage.input_tokens": 10, "gen_ai.usage.output_tokens": 5}
				for k, v := range extra {
					attrs[k] = v
				}
				var a []attribute.KeyValue
				for k, v := range attrs {
					switch x := v.(type) {
					case string:
						a = append(a, attribute.String(k, x))
					case int:
						a = append(a, attribute.Int(k, x))
					}
				}
				s := tracetest.SpanStub{Name: "chat", StartTime: time.Now().Add(-time.Millisecond), EndTime: time.Now(), Attributes: a}.Snapshot()
				recordSpan(s, rec, i)
			}
			span(c.Attributes)
			if c.ResultAttributes != nil {
				span(c.ResultAttributes)
			}
			i.End(rudderstackai.EndOptions{})
			count, toolEvents := -1, 0
			for _, e := range events() {
				if e["event"] == "schema_done" {
					toolEvents++
				}
				if e["event"] == "ai_response_received" {
					count = int(e["properties"].(object)["tool_call_count"].(float64))
				}
			}
			wantLogs := 0
			if c.Count == 0 && !c.NoExclusion {
				wantLogs = 1
			}
			if got := strings.Count(logs.String(), "structured output: forced tool with no result"); got != wantLogs {
				t.Errorf("exclusion logs=%d; want %d", got, wantLogs)
			}
			if wantLogs == 1 && (!strings.Contains(logs.String(), "tool_name=Schema") || !strings.Contains(logs.String(), "tool_call_id=call-schema")) {
				t.Error("log lacks tool identity")
			}
			if count != c.Count || toolEvents != c.Count {
				t.Errorf("count/events = %d/%d; want %d", count, toolEvents, c.Count)
			}
		})
	}
}

type structuredLog struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *structuredLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(p)
}
func (l *structuredLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.buffer.String() }
