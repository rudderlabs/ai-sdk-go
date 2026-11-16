package rudderanthropic_test

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// A context near the span budget cannot hold the whole span message: the call keeps the OTel span.
func TestMessageSpanBudgetNearTheLimit(t *testing.T) {
	for _, c := range []struct {
		pad int
		fit bool
	}{{31_000, false}, {1_000, true}} {
		llm, rec := setup(t, rudderstackai.WithEmitSpans(true))
		var fit []bool
		hook.Tracing.AdoptOTelCall = func(_ context.Context, _ any, _ uint64, fits func(hook.OTelCall) bool) (hook.OTelCall, bool) {
			o := hook.OTelCall{TraceID: strings.Repeat("a", 32), SpanID: strings.Repeat("b", 16), ParentID: strings.Repeat("c", 16), Adopted: true}
			ok := fits(o)
			fit = append(fit, ok)
			return o, ok
		}
		llm.Script("anthropic.messages", jsonReply(t, map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-haiku-4-5",
			"content": []any{map[string]any{"type": "text", "text": "Hello"}}, "stop_reason": "end_turn",
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		}))
		ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Context: map[string]any{"pad": strings.Repeat("x", c.pad)}})
		client := newClient(llm)
		if _, err := client.Messages.New(ctx, anthropic.MessageNewParams{}, body(t, request(nil))); err != nil {
			t.Fatal(err)
		}
		i.End(rudderstackai.EndOptions{})
		if len(fit) != 1 || fit[0] != c.fit {
			t.Errorf("pad %d: fit checks %v, want [%v]", c.pad, fit, c.fit)
		}
		if sent := len(rec.events(t)["ai_otel_span"]); (sent == 1) != c.fit {
			t.Errorf("pad %d: %d SDK spans sent, want %v", c.pad, sent, c.fit)
		}
	}
}
