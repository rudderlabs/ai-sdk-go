package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/rudderlabs/ai-sdk-go/internal/structuredoutput"
)

func TestStructuredOutputLifecycle(t *testing.T) {
	raw, err := os.ReadFile("spec/fixtures/structured-output-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name           string
			Request        map[string]any
			Tool           ToolCall `json:"response_tool"`
			Result         json.RawMessage
			ResultName     string `json:"result_name"`
			Pause, Emitted bool
			InProcess      bool `json:"in_process"`
			EmptyResult    bool `json:"empty_result"`
			Suppressed     bool `json:"suppressed_event"`
			NoTool         bool `json:"no_tool"`
			Expected       struct {
				Forced *string `json:"forced_tool"`
				Count  int     `json:"tool_call_count"`
				Events int     `json:"tool_events"`
			}
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			forced := structuredoutput.ForcedToolName(c.Request)
			want := ""
			if c.Expected.Forced != nil {
				want = *c.Expected.Forced
			}
			if forced != want {
				t.Fatalf("forced = %q; want %q", forced, want)
			}
			rule := ToolEventRule{Event: "schema_done"}
			if c.Suppressed {
				rule.When = func(map[string]any, any) bool { return false }
			}
			r := setupAI(t, WithToolBreakdown(true), WithLLMCallEvents(true), WithEventRules(EventRules{Tools: map[string]ToolEventRule{"Schema": rule, "lookup": {Event: "lookup_done"}}}))
			logs := captureLogs(t)
			t.Setenv("RUDDERSTACK_AI_DEBUG", "1")
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o", InputTokens: 10, OutputTokens: 5})
			if !c.NoTool {
				if c.Emitted {
					call := c.Tool
					call.Error = Ptr(false)
					i.RecordToolCalls(call)
				}
				call := c.Tool
				call.StructuredCandidate = forced != "" && call.Name == forced
				i.RecordToolCalls(call)
			}
			if c.Pause {
				snapshot, ok := i.Pause(PauseOptions{Reason: PauseApproval})
				if !ok {
					t.Fatal("pause failed")
				}
				if len(r.named("schema_done")) != 0 || strings.Contains(logs.String(), "structured output: forced tool with no result") {
					t.Fatal("pause decided candidate")
				}
				if lastResponse(t, r)["tool_call_count"] != 1 {
					t.Fatal("pause did not provisionally count candidate")
				}
				if len(snapshot.PendingTools) != 1 || snapshot.PendingTools[0]["structured_candidate"] != true {
					t.Fatal("candidate missing from snapshot")
				}
				encoded, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(encoded), "answer") {
					t.Fatal("snapshot leaked tool arguments")
				}
				if c.InProcess {
					i.Resume()
				} else {
					var restored PausedInteraction
					if err := json.Unmarshal(encoded, &restored); err != nil {
						t.Fatal(err)
					}
					_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &restored})
				}
			}
			if c.EmptyResult {
				i.RecordToolResult(c.Tool.ID, ToolResult{})
			}
			if c.Result != nil {
				i.recordToolResult(c.Tool.ID, ToolResult{Result: c.Result}, true, c.ResultName)
			}
			i.End(EndOptions{})
			i.End(EndOptions{})
			final := lastResponse(t, r)
			if final["tool_call_count"] != c.Expected.Count {
				t.Errorf("count=%v; want %d", final["tool_call_count"], c.Expected.Count)
			}
			breakdown := 0
			if rows, ok := final["tool_breakdown"].([]map[string]any); ok {
				for _, row := range rows {
					breakdown += row["call_count"].(int)
				}
			}
			if breakdown != c.Expected.Count {
				t.Errorf("breakdown=%d; want %d", breakdown, c.Expected.Count)
			}
			if events := len(r.named("schema_done")) + len(r.named("lookup_done")); events != c.Expected.Events {
				t.Errorf("tool events=%d; want %d", events, c.Expected.Events)
			}
			excluded := !c.NoTool && forced == c.Tool.Name && c.Result == nil && !c.EmptyResult && (!c.Emitted || c.Suppressed)
			lines := strings.Count(logs.String(), "structured output: forced tool with no result")
			wantLogs := 0
			if excluded {
				wantLogs = 1
				if !strings.Contains(logs.String(), "tool_name="+c.Tool.Name) || !strings.Contains(logs.String(), "tool_call_id="+c.Tool.ID) {
					t.Error("log lacks identity")
				}
			}
			if lines != wantLogs {
				t.Errorf("exclusion logs=%d; want %d", lines, wantLogs)
			}
			first := r.named("ai_response_received")[0].Properties
			if first["llm_call_count"] != 1 || first["total_input_tokens"] != 10 || first["total_output_tokens"] != 5 || len(r.named("ai_llm_response_received")) != 1 {
				t.Fatal("model accounting changed")
			}
		})
	}
}

func TestStructuredOutputOutcomeBeforeCandidate(t *testing.T) {
	for _, events := range []bool{false, true} {
		for _, source := range []string{"result", "empty result", "execution span", "inline result", "emitted event"} {
			if !events && source == "emitted event" {
				continue
			}
			t.Run(fmt.Sprintf("%s/events=%t", source, events), func(t *testing.T) {
				opts := []ConfigOption{WithToolBreakdown(true)}
				if events {
					opts = append(opts, WithEventRules(EventRules{Tools: map[string]ToolEventRule{"Schema": {Event: "schema_done"}}}))
				}
				r := setupAI(t, opts...)
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
				call := ToolCall{ID: "schema", Name: "Schema"}
				i.RecordToolCalls(call)
				switch source {
				case "result":
					i.RecordToolResult(call.ID, ToolResult{Result: map[string]any{"ok": true}})
				case "empty result":
					i.RecordToolResult(call.ID, ToolResult{})
				case "execution span":
					i.recordToolCalls(true, call)
				case "inline result":
					outcome := call
					outcome.Result = map[string]any{"ok": true}
					i.RecordToolCalls(outcome)
				case "emitted event":
					outcome := call
					outcome.Error = Ptr(false)
					i.RecordToolCalls(outcome)
					if len(r.named("schema_done")) != 1 {
						t.Fatal("event not emitted before recognition")
					}
				}
				call.StructuredCandidate = true
				i.RecordToolCalls(call)
				i.End(EndOptions{})
				if lastResponse(t, r)["tool_call_count"] != 1 {
					t.Fatal("observed execution/outcome lost when candidate recognized later")
				}
				rows := lastResponse(t, r)["tool_breakdown"].([]map[string]any)
				if len(rows) != 1 || rows[0]["call_count"] != 1 {
					t.Fatalf("breakdown = %v", rows)
				}
				wantEvents := 0
				if events {
					wantEvents = 1
				}
				if got := len(r.named("schema_done")); got != wantEvents {
					t.Fatalf("events = %d; want %d", got, wantEvents)
				}
			})
		}
	}
}
