package kit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

// toolOutputRules declares lookup_done and tool_breakdown, and collects the results rules see.
func toolOutputRules(t *testing.T) *[]any {
	t.Helper()
	seen := &[]any{}
	rule := rudderstackai.ToolEventRule{Event: "lookup_done", Properties: func(_ map[string]any, result any) map[string]any {
		*seen = append(*seen, result)
		return nil
	}}
	if err := rudderstackai.Configure(rudderstackai.WithToolBreakdown(true), rudderstackai.WithEventRules(rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{"lookup": rule}})); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = rudderstackai.Configure(rudderstackai.WithToolBreakdown(false), rudderstackai.WithEventRules(rudderstackai.EventRules{}))
	})
	return seen
}

func jsonText(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// checkToolOutcome checks lookup_done and the one-call tool_breakdown for a success or a vendor failure.
func checkToolOutcome(t *testing.T, rec *recorder, failed bool) {
	t.Helper()
	event, breakdown := map[string]any{"tool_status": "success", "error_type": nil, "tool_call_id": "call-1"}, `[{"call_count":1,"error_count":0,"tool_name":"lookup"}]`
	if failed {
		event = map[string]any{"tool_status": "error", "error_type": "api_error", "tool_call_id": "call-1"}
		breakdown = `[{"call_count":1,"error_count":1,"error_types":{"api_error":1},"tool_name":"lookup"}]`
	}
	want(t, rec.props(t, "lookup_done"), event)
	response := rec.props(t, "ai_response_received")
	if got := jsonText(response["tool_breakdown"]); got != breakdown || jsonText(response["tool_call_count"]) != "1" {
		t.Errorf("tool_breakdown %s, tool_call_count %v; want %s, 1", got, response["tool_call_count"], breakdown)
	}
}

func TestToolOutputFixture(t *testing.T) {
	for _, c := range testkit.ToolOutputCases() {
		t.Run(c.Name, func(t *testing.T) {
			if c.Expected.Error != (c.Expected.ErrorType == "api_error") {
				t.Fatalf("fixture expected %+v", c.Expected)
			}
			for _, mode := range []ToolOutput{RawOutput, DecodedOutput} {
				result := c.Output
				if mode == DecodedOutput {
					// Wrappers that decode for rules classify that decoded value.
					result = ParseToolValue(c.Output)
				}
				if got := ToolOutputFailed(result, mode); got != c.Expected.Error {
					t.Errorf("mode %d: ToolOutputFailed = %t", mode, got)
				}
				rec := newRecorder(t)
				seen := toolOutputRules(t)
				_, i := interaction(rec)
				i.RecordToolCalls(rudderstackai.ToolCall{ID: "call-1", Name: "lookup"})
				RecordToolOutput(i, "call-1", rudderstackai.ToolResult{Error: rudderstackai.Ptr(false), Result: result}, true, mode)
				i.End(rudderstackai.EndOptions{})
				checkToolOutcome(t, rec, c.Expected.Error)
				// Rules see the wrapper's result as it was, never the convention's own decode.
				if len(*seen) != 1 || jsonText((*seen)[0]) != jsonText(result) {
					t.Errorf("mode %d: rule saw %v, want %v", mode, *seen, result)
				}
			}
		})
	}
}

func TestToolOutputKeepsFrameworkFailure(t *testing.T) {
	for _, errorType := range []string{"denied", "timeout", ""} {
		rec := newRecorder(t)
		toolOutputRules(t)
		_, i := interaction(rec)
		i.RecordToolCalls(rudderstackai.ToolCall{ID: "call-1", Name: "lookup"})
		RecordToolOutput(i, "call-1", rudderstackai.ToolResult{Error: rudderstackai.Ptr(true), ErrorType: errorType, Result: map[string]any{"successful": false}}, true, RawOutput)
		i.End(rudderstackai.EndOptions{})
		// Only a vendor failure sets the declared event's error_type.
		if _, ok := rec.props(t, "lookup_done")["error_type"]; ok {
			t.Errorf("%q: declared event has error_type", errorType)
		}
		types := map[string]any{}
		if errorType != "" {
			types[errorType] = 1
		}
		breakdown := rec.props(t, "ai_response_received")["tool_breakdown"].([]map[string]any)
		if len(breakdown) != 1 || jsonText(breakdown[0]["error_count"]) != "1" || (errorType != "" && jsonText(breakdown[0]["error_types"]) != jsonText(types)) {
			t.Errorf("%q: tool_breakdown %s", errorType, jsonText(breakdown))
		}
	}
}

func TestToolOutputParseBound(t *testing.T) {
	failure := `{"successful":false}`
	for _, c := range []struct {
		output string
		failed bool
	}{
		{strings.Repeat(" ", 65536-len(failure)) + failure, true},
		{strings.Repeat(" ", 65537-len(failure)) + failure, false},
		// Astral characters are two UTF-16 units, as Node.js and Python measure them.
		{`{"successful":false,"data":"` + strings.Repeat("\U0001F600", (65536-28-2)/2) + `"}`, true},
		{`{"successful":false,"data":"` + strings.Repeat("\U0001F600", 40000) + `"}`, false},
	} {
		if got := ToolOutputFailed(c.output, RawOutput); got != c.failed {
			t.Errorf("length %d: got %t", len(c.output), got)
		}
	}
	// A decoded value keeps its full shape: only the convention's own parse is bounded.
	large := map[string]any{"successful": false, "data": strings.Repeat("x", 65536)}
	if !ToolOutputFailed(ParseToolValue(jsonText(large)), DecodedOutput) {
		t.Error("decoded oversized output not classified")
	}
}

// pauseClient runs reenter while the paused answer is delivered.
type pauseClient struct {
	*recorder
	reenter func()
}

func (c *pauseClient) Enqueue(m analytics.Message) error {
	err := c.recorder.Enqueue(m)
	if e, ok := m.(analytics.Track); ok && e.Event == "ai_response_received" && e.Properties["response_status"] == "paused" {
		c.reenter()
	}
	return err
}

func TestToolOutputFailureSurvivesPause(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprint(restore), func(t *testing.T) {
			rec := newRecorder(t)
			toolOutputRules(t)
			client := &pauseClient{recorder: rec}
			_, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u1", Analytics: client})
			// Recorded during paused delivery, so the rule outcome is frozen into the snapshot.
			client.reenter = func() {
				i.RecordToolCalls(rudderstackai.ToolCall{ID: "late-tool", Name: "lookup"})
				RecordToolOutput(i, "late-tool", rudderstackai.ToolResult{Error: rudderstackai.Ptr(false), Result: map[string]any{"successful": false, "error": "raw-error"}}, true, DecodedOutput)
			}
			s, ok := i.Pause(rudderstackai.PauseOptions{})
			snapshot := jsonText(s)
			if !ok || strings.Contains(snapshot, "raw-error") || len(s.PendingTools) != 1 || jsonText(s.PendingTools[0]["rule_outcome"].(map[string]any)["error_type"]) != `"api_error"` {
				t.Fatalf("snapshot %s", snapshot)
			}
			if restore {
				var stored rudderstackai.PausedInteraction
				if err := json.Unmarshal([]byte(snapshot), &stored); err != nil {
					t.Fatal(err)
				}
				_, i = rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{Resume: &stored, Analytics: rec})
			} else {
				i.Resume()
			}
			i.End(rudderstackai.EndOptions{})
			want(t, rec.props(t, "lookup_done"), map[string]any{"tool_status": "error", "error_type": "api_error"})
		})
	}
}
