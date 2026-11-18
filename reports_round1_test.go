package rudderstackai

import (
	"context"
	"fmt"
	"testing"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func TestRound1CaptureOffReportSiblings(t *testing.T) {
	for _, spans := range []bool{false, true} {
		for _, site := range []string{"combined", "provider_history", "provider_named_history", "tracing", "after_end", "unmatched_rule"} {
			t.Run(fmt.Sprintf("%s/spans=%v", site, spans), func(t *testing.T) {
				setupAI(t, WithCaptureContent(false), WithEmitSpans(spans))
				if site == "unmatched_rule" {
					_ = Configure(WithEventRules(EventRules{Tools: map[string]ToolEventRule{"another_tool": {Event: "other"}}}))
				}
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
				args, result := &review84Count{}, &review84Count{}
				call := ToolCall{ID: "t", Name: "lookup", Args: map[string]any{"value": args}}
				switch site {
				case "combined":
					call.Result = result
					i.RecordToolCalls(call)
				case "tracing":
					call.Result = result
					i.recordToolCalls(true, call)
				case "after_end":
					i.End(EndOptions{})
					call.Result = result
					i.RecordToolCalls(call)
				case "provider_history":
					i.RecordToolCalls(call)
					hook.RecordToolResult(i, "t", ToolResult{Result: result}, true)
				case "provider_named_history":
					call.ID = ""
					call.StructuredCandidate = true
					i.RecordToolCalls(call)
					hook.RecordToolResult(i, "", ToolResult{Result: result}, true, "lookup")
				default:
					i.RecordToolCalls(call)
					i.RecordToolResult("t", ToolResult{Result: result})
				}
				i.End(EndOptions{})
				if args.Calls != 0 || result.Calls != 0 {
					t.Fatalf("args=%d result=%d", args.Calls, result.Calls)
				}
			})
		}
	}
}
