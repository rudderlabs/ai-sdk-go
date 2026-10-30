package rudderstackai

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestFitPropertiesKeepsSmallEvents(t *testing.T) {
	p := map[string]any{"prompt_text": "hi", "interaction_id": "i"}
	if got := fitProperties("ai_user_prompt_created", p); got["prompt_text"] != "hi" {
		t.Fatalf("got %v", got)
	}
}

func TestFitPropertiesDropsTextFirst(t *testing.T) {
	big := strings.Repeat("x", 20_000)
	p := map[string]any{"response_text": big, "prompt_text": big, "interaction_id": "i"}
	got := fitProperties("ai_response_received", p)
	if _, ok := got["response_text"]; ok {
		t.Fatal("response_text kept")
	}
	if _, ok := got["prompt_text"]; !ok {
		t.Fatal("prompt_text dropped although the event fit without response_text")
	}
	if _, ok := p["response_text"]; !ok {
		t.Fatal("the caller's map changed")
	}
}

func TestFitPropertiesDropsFeedbackText(t *testing.T) {
	p := map[string]any{"action_details": map[string]any{"feedback_text": strings.Repeat("x", 31_000), "feedback_type": "thumbs"}}
	got := fitProperties("ai_user_action", p)
	details := got["action_details"].(map[string]any)
	if _, ok := details["feedback_text"]; ok || details["feedback_type"] != "thumbs" {
		t.Fatalf("details = %v", details)
	}
}

func TestFitPropertiesTrimsBreakdownsFromTheEnd(t *testing.T) {
	entries := func(n int) []map[string]any {
		out := make([]map[string]any, n)
		for i := range out {
			out[i] = map[string]any{"tool_name": strings.Repeat("t", 100), "call_count": i}
		}
		return out
	}
	p := map[string]any{"tool_breakdown": entries(400), "usage_breakdown": entries(10), "llm_call_count": 1}
	got := fitProperties("ai_response_received", p)
	tools := got["tool_breakdown"].([]map[string]any)
	if len(tools) == 0 || len(tools) >= 400 || tools[0]["call_count"] != 0 {
		t.Fatalf("tool_breakdown has %d entries", len(tools))
	}
	if len(got["usage_breakdown"].([]map[string]any)) != 10 || got["llm_call_count"] != 1 {
		t.Fatal("usage_breakdown or metrics changed")
	}
	if propertiesBytes(got) > maxPropertiesBytes {
		t.Fatal("still over budget")
	}

	// Breakdowns emptied entirely are removed; what still does not fit is sent trimmed.
	p = map[string]any{"tool_breakdown": entries(1), "intent": strings.Repeat("x", 31_000)}
	got = fitProperties("ai_response_received", p)
	if _, ok := got["tool_breakdown"]; ok {
		t.Fatal("empty tool_breakdown kept")
	}
	if got["intent"] == nil {
		t.Fatal("other properties dropped")
	}
}

func TestPropertiesBytesUnserializable(t *testing.T) {
	if propertiesBytes(map[string]any{"f": func() {}}) != 0 {
		t.Fatal("unserializable properties count as 0")
	}
}

// MCP events go through the same budget, and their client is flushed by Flush.
func TestEmitMCPFitsAndRemembersClient(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)
	s := &sink{}
	emit(s, "mcp_tool_called", who{}, map[string]any{"tool_name": "t", "response_text": strings.Repeat("x", 31_000)})
	got := s.named("mcp_tool_called")
	if len(got) != 1 || got[0].Properties["response_text"] != nil || got[0].Properties["tool_name"] != "t" {
		t.Fatalf("events = %v", got)
	}
	FlushTimeout(time.Second)
	if s.flushes.Load() != 1 {
		t.Fatal("MCP client not flushed")
	}
}

func TestFitToolEventPropertiesDropsCustom(t *testing.T) {
	p := map[string]any{"tool_name": "t", "big": strings.Repeat("x", 31_000), "small": 1}
	got := fitToolEventProperties("order_placed", p, []string{"big", "small"}, nil)
	if _, ok := got["big"]; ok || got["small"] != nil || got["tool_name"] != "t" {
		t.Fatalf("got %v", got)
	}
	if p["big"] == nil {
		t.Fatal("the caller's map changed")
	}
	small := map[string]any{"tool_name": "t", "small": 1}
	if got := fitToolEventProperties("order_placed", small, []string{"small"}, nil); got["small"] != 1 {
		t.Fatal("a fitting event lost its properties")
	}
}

func TestDebugEnv(t *testing.T) {
	l := &logBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	debugf("quiet", "error", errors.New("secret token"))
	if l.String() != "" {
		t.Fatalf("logged without RUDDERSTACK_AI_DEBUG: %s", l)
	}
	t.Setenv("RUDDERSTACK_AI_DEBUG", "1")
	debugf("dropped", "event", "e", "error", errors.New("secret token"))
	out := l.String()
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "[rudderstack-ai] dropped") || !strings.Contains(out, "*errors.errorString") {
		t.Fatalf("debug output: %s", out)
	}
	if strings.Contains(out, "secret token") {
		t.Fatal("error text logged")
	}
}
