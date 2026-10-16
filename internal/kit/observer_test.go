package kit

import (
	"context"
	"reflect"
	"testing"
	"time"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func ptr[T any](v T) *T { return &v }

// observer opens a streamed call: in an active interaction, or ephemeral when active is false.
func observer(t *testing.T, active bool) (*recorder, *Observer, *rudderstackai.Interaction) {
	t.Helper()
	rec := newRecorder(t)
	ctx := context.Background()
	if active {
		ctx, _ = interaction(rec)
	}
	i, ephemeral := ForCall(ctx, "kit-test", rec)
	c := &Call{Interaction: i, Ephemeral: ephemeral, Provider: "openai", Integration: "kit-test", Model: "gpt-4o-mini", StartedAt: time.Now()}
	return rec, NewObserver(c), i
}

func TestObserverCompleted(t *testing.T) {
	rec, o, _ := observer(t, false)
	o.ResponseID, o.Model = "resp-1", "gpt-4o-mini-2024-07-18"
	o.InputTokens, o.OutputTokens, o.ReasoningTokens = ptr(30), ptr(12), ptr(4)
	o.CallCount, o.CachedInputTokens, o.FinishReason = 2, 10, "stop"
	o.CostUSD, o.ServiceTier = ptr(0.25), "default"
	o.ServerToolCalls = map[string]int{"web_search": 1}
	o.NoteTool("t", "call_1", "lookup", `{"q":1}`, "")
	o.FirstToken()
	o.FirstToken()
	o.Done = true
	if !o.Complete() {
		t.Fatal("Complete after Done")
	}
	o.Finish(Completed, "")
	o.Finish(Errored, "api_error")
	want(t, rec.props(t, "ai_llm_response_received"), map[string]any{
		"model": "gpt-4o-mini-2024-07-18", "input_tokens": 30, "output_tokens": 12,
		"reasoning_tokens": 4, "cached_input_tokens": 10, "cost_usd": 0.25, "response_id": "resp-1", "finish_reason": "stop",
		"server_tool_call_count": 1, "error_type": nil,
	})
	resp := rec.props(t, "ai_response_received")
	want(t, resp, map[string]any{"response_status": "success", "llm_call_count": 2, "tool_call_count": 1, "total_cost_usd": 0.25})
	if _, ok := resp["time_to_first_token_ms"]; !ok {
		t.Error("time_to_first_token_ms missing")
	}
}

func TestObserverTruncated(t *testing.T) {
	rec, o, i := observer(t, true)
	o.InputTokens, o.OutputTokens = ptr(8), ptr(3)
	o.Incomplete = true
	o.Finish(Completed, "")
	// A stream cut off before its terminal event has no reliable usage.
	want(t, rec.props(t, "ai_llm_response_received"), map[string]any{"error_type": "api_error", "input_tokens": 0, "output_tokens": 0})
	i.End(rudderstackai.EndOptions{})
	want(t, rec.props(t, "ai_response_received"), map[string]any{"response_status": "error", "error_type": "api_error"})
}

func TestObserverErroredWithTokens(t *testing.T) {
	rec, o, _ := observer(t, false)
	o.InputTokens, o.OutputTokens, o.ReasoningTokens = ptr(9), ptr(3), ptr(1)
	o.CachedInputTokens, o.CacheWriteTokens = 2, 1
	o.ResponseID = "resp-err"
	o.NoteTool("0", "call_2", "search", nil, "")
	o.Finish(Errored, "rate_limit")
	want(t, rec.props(t, "ai_llm_response_received"), map[string]any{
		"error_type": "rate_limit", "input_tokens": 9, "output_tokens": 3, "reasoning_tokens": 1,
		"cached_input_tokens": 2, "cache_write_tokens": 1, "response_id": "resp-err", "cost_usd": nil,
	})
	want(t, rec.props(t, "ai_response_received"), map[string]any{"response_status": "error", "error_type": "rate_limit", "tool_call_count": 1})
}

func TestObserverErroredActive(t *testing.T) {
	rec, o, i := observer(t, true)
	o.Finish(Errored, "timeout")
	rec.none(t, "ai_response_received")
	i.End(rudderstackai.EndOptions{})
	want(t, rec.props(t, "ai_response_received"), map[string]any{"response_status": "timeout", "error_type": "timeout"})
}

func TestObserverCancelled(t *testing.T) {
	for name, finish := range map[string]func(*Observer){
		"cancelled":            func(o *Observer) { o.Finish(Cancelled, "") },
		"errored as cancelled": func(o *Observer) { o.Finish(Errored, "cancelled") },
	} {
		t.Run(name, func(t *testing.T) {
			rec, o, _ := observer(t, false)
			o.InputTokens, o.CostUSD = ptr(5), ptr(0.1)
			finish(o)
			want(t, rec.props(t, "ai_llm_response_received"), map[string]any{"error_type": "cancelled", "cost_usd": nil})
			want(t, rec.props(t, "ai_response_received"), map[string]any{"response_status": "cancelled", "error_type": "cancelled", "total_cost_usd": nil})
		})
	}
}

func TestObserverRepeatedResponse(t *testing.T) {
	rec, o, i := observer(t, true)
	i.RecordCall(rudderstackai.LLMCall{Provider: "openai", Model: "gpt-4o-mini", ResponseID: "dup", InputTokens: 1})
	o.ResponseID = "dup"
	o.NoteTool("0", "call_3", "lookup", nil, "")
	o.Finish(Completed, "")
	i.End(rudderstackai.EndOptions{})
	// The repeated response is not counted, nor are its tools.
	want(t, rec.props(t, "ai_response_received"), map[string]any{"llm_call_count": 1, "tool_call_count": 0})
}

func TestObserverEndWhileOpen(t *testing.T) {
	cases := []struct {
		name string
		done bool
		want map[string]any
	}{
		{"terminal event seen", true, map[string]any{"error_type": nil, "input_tokens": 4}},
		{"still streaming", false, map[string]any{"error_type": "cancelled"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, o, i := observer(t, true)
			o.InputTokens, o.Done = ptr(4), c.done
			i.End(rudderstackai.EndOptions{})
			want(t, rec.props(t, "ai_llm_response_received"), c.want)
			rec.props(t, "ai_response_received")
			o.Finish(Completed, "")
			rec.props(t, "ai_llm_response_received")
		})
	}
}

func TestObserverEphemeralEndWhileOpen(t *testing.T) {
	rec, o, i := observer(t, false)
	o.Done = true
	i.End(rudderstackai.EndOptions{})
	rec.props(t, "ai_llm_response_received")
	want(t, rec.props(t, "ai_response_received"), map[string]any{"response_status": "success"})
}

func TestObserverTools(t *testing.T) {
	_, o, _ := observer(t, true)
	if o.HasTools() {
		t.Fatal("no tools yet")
	}
	o.NoteToolArgs("missing", `{"a":1}`)
	o.NoteTool("0", "", "", nil, "")
	o.NoteToolArgs("0", `{"city":`)
	o.NoteToolArgs("0", "")
	o.NoteToolArgs("0", `"Paris"}`)
	o.NoteTool("0", "call_4", "weather", nil, "geo")
	o.NoteTool("0", "call_other", "renamed", nil, "other")
	o.NoteTool("1", "call_5", "search", map[string]any{"q": "x"}, "")
	o.NoteTool("1", "", "", map[string]any{"q": "y"}, "")
	if !o.HasTools() || len(o.tools) != 2 {
		t.Fatalf("tools = %d", len(o.tools))
	}
	first := o.tools[0]
	if first.id != "call_4" || first.name != "weather" || first.mcpServer != "geo" {
		t.Errorf("first tool = %+v", first)
	}
	if got := ParseToolValue(first.args); !reflect.DeepEqual(got, map[string]any{"city": "Paris"}) {
		t.Errorf("streamed args = %v", got)
	}
	if got := o.tools[1].args; !reflect.DeepEqual(got, map[string]any{"q": "y"}) {
		t.Errorf("later args = %v", got)
	}
}

func TestObserverText(t *testing.T) {
	_, o, _ := observer(t, true)
	o.NoteText("dropped")
	if o.text.Len() != 0 {
		t.Fatal("text kept without a classifier or evaluator")
	}
	o.keepText = true
	o.NoteText("Hello")
	o.NoteText("")
	o.NoteText(" world")
	if got := o.text.String(); got != "Hello world" {
		t.Fatalf("text = %q", got)
	}
}
