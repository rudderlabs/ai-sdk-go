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
		{Integration: "tracing", ResponseID: "first", FinishReason: "stop"},
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
