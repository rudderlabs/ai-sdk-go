package rudderstackai

import (
	"context"
	"testing"
	"time"
)

func TestToolSpanMatchesMissingLabels(t *testing.T) {
	for _, c := range []struct {
		name, wrapperAgent, spanAgent, wrapperParent, spanParent string
		want                                                     int
	}{
		{"wrapper missing", "", "agent", "", "parent", 1},
		{"span missing", "agent", "", "parent", "", 1},
		{"agent conflict", "a", "b", "", "", 2},
		{"parent conflict", "", "", "a", "b", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			setupAI(t)
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			defer i.End(EndOptions{})
			i.RecordToolCalls(ToolCall{ID: "wrapper", Name: "lookup", AgentName: c.wrapperAgent, ParentTool: c.wrapperParent})
			i.recordToolCalls(true, ToolCall{Name: "lookup", AgentName: c.spanAgent, ParentTool: c.spanParent, Duration: Ptr(time.Second)})
			if len(i.tools) != c.want {
				t.Fatalf("tools=%d want %d", len(i.tools), c.want)
			}
			if c.want == 1 && (i.tools[0].agentName != first(c.wrapperAgent, c.spanAgent) || i.tools[0].parentTool != first(c.wrapperParent, c.spanParent)) {
				t.Fatal("missing labels were not filled")
			}
		})
	}
}

func TestSuccessfulSpanReplacesEmptyAnswer(t *testing.T) {
	for _, last := range []LLMCall{
		{Integration: "tracing", ResponseID: "empty", FinishReason: "stop"},
		{Integration: "tracing", ResponseID: "tools", FinishReason: "tool_calls"},
	} {
		t.Run(last.ResponseID, func(t *testing.T) {
			r := setupAI(t, WithCaptureContent(true))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			i.RecordCall(LLMCall{Integration: "tracing", ResponseID: "first", OutputText: "stale answer"})
			i.RecordCall(last)
			i.End(EndOptions{})
			if text, ok := r.response(t)["response_text"]; ok {
				t.Fatalf("stale response_text=%v", text)
			}
		})
	}
}

func TestSpanFirstWrapperSameResponseID(t *testing.T) {
	setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	defer i.End(EndOptions{})
	i.RecordCall(LLMCall{Integration: "tracing", Provider: "openai", ResponseID: "response", InputTokens: 1, OutputTokens: 2})
	call := LLMCall{Integration: "openai", Provider: "openai", ResponseID: "response", InputTokens: 100, OutputTokens: 20}
	if !i.RecordCall(call) {
		t.Fatal("wrapper after span rejected; wrapper tools would be lost")
	}
	i.RecordToolCalls(ToolCall{ID: "wrapper-tool"})
	if i.RecordCall(call) {
		t.Fatal("repeated wrapper accepted")
	}
	if len(i.calls) != 1 || i.calls[0].inputTokens != 100 || i.calls[0].outputTokens != 20 || len(i.tools) != 1 {
		t.Fatalf("merged call=%+v tools=%d", i.calls[0], len(i.tools))
	}
}

func TestToolSpanSkipsSettledResult(t *testing.T) {
	setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	defer i.End(EndOptions{})
	i.RecordToolCalls(ToolCall{ID: "completed", Name: "lookup"}, ToolCall{ID: "pending", Name: "lookup"})
	i.RecordToolResult("completed", ToolResult{Result: "done"})
	i.recordToolCalls(true, ToolCall{Name: "lookup", Error: Ptr(false), Duration: Ptr(time.Second)})
	if len(i.tools) != 2 || i.toolsByID["completed"].duration != nil || !i.toolsByID["pending"].settled {
		t.Fatal("tool span matched a settled tool instead of the pending tool")
	}
}

func TestRejectedSpanPreservesLatestAnswer(t *testing.T) {
	for _, text := range []string{"", "stale answer"} {
		t.Run(text, func(t *testing.T) {
			r := setupAI(t, WithCaptureContent(true))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			i.RecordCall(LLMCall{Integration: "tracing", ResponseID: "first", OutputText: "first answer"})
			i.RecordCall(LLMCall{Integration: "tracing", ResponseID: "latest", OutputText: "latest answer"})
			if i.RecordCall(LLMCall{Integration: "tracing", ResponseID: "first", OutputText: text}) {
				t.Fatal("duplicate span accepted")
			}
			i.End(EndOptions{})
			if got := r.response(t)["response_text"]; got != "latest answer" {
				t.Fatalf("response_text=%v want latest answer", got)
			}
		})
	}
}

func TestLateSpanResumesFromStart(t *testing.T) {
	for _, c := range []struct {
		name   string
		offset time.Duration
	}{
		{"after pause", time.Second}, {"before pause", -time.Second}, {"missing start", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			setupAI(t)
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			defer i.End(EndOptions{})
			if _, ok := i.Pause(PauseOptions{Reason: "approval"}); !ok {
				t.Fatal("pause failed")
			}
			paused := time.Now().Add(-3 * time.Second)
			i.mu.Lock()
			i.pausedAt = paused
			i.mu.Unlock()
			start := paused.Add(c.offset)
			if c.offset == 0 {
				start = time.Time{}
			}
			before := time.Now()
			i.RecordCall(LLMCall{Integration: "tracing", StartedAt: start, EndedAt: time.Now()})
			if c.offset > 0 {
				if !i.started.Equal(start) || i.pausedTotal != c.offset {
					t.Fatalf("started=%v want %v paused=%v want %v", i.started, start, i.pausedTotal, c.offset)
				}
			} else if i.started.Before(before) {
				t.Fatalf("invalid start used: %v", i.started)
			}
		})
	}
}
