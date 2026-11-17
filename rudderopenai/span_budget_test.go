package rudderopenai_test

import (
	"context"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// adoptProbe stands in for the OpenTelemetry span processor: it offers the call an OTel span's
// IDs and records whether the SDK span's whole message fit under them.
func adoptProbe(fit *[]bool) {
	hook.Tracing.AdoptOTelCall = func(_ context.Context, _ any, _ uint64, fits func(hook.OTelCall) bool) (hook.OTelCall, bool) {
		o := hook.OTelCall{TraceID: strings.Repeat("a", 32), SpanID: strings.Repeat("b", 16), ParentID: strings.Repeat("c", 16), Adopted: true}
		ok := fits(o)
		*fit = append(*fit, ok)
		return o, ok
	}
}

// A context near the span budget cannot hold the whole span message: the call keeps the OTel span.
func TestChatSpanBudgetNearTheLimit(t *testing.T) {
	for _, c := range []struct {
		pad int
		fit bool
	}{{31_000, false}, {1_000, true}} {
		rec, llm := setup(t, rudderstackai.WithEmitSpans(true))
		var fit []bool
		adoptProbe(&fit)
		llm.Script("openai.chat", reply(t, `{"json":{"id":"c1","model":"gpt-4.1-mini","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}}`))
		client := newClient(llm)
		ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u1", Context: map[string]any{"pad": strings.Repeat("x", c.pad)}})
		if _, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body(chatBody)); err != nil {
			t.Fatal(err)
		}
		i.End(rudderstackai.EndOptions{})
		if len(fit) != 1 || fit[0] != c.fit {
			t.Errorf("pad %d: fit checks %v, want [%v]", c.pad, fit, c.fit)
		}
		sent := 0
		for _, e := range rec.events(t) {
			if e.name == "ai_otel_span" {
				sent++
			}
		}
		if (sent == 1) != c.fit {
			t.Errorf("pad %d: %d SDK spans sent, want %v", c.pad, sent, c.fit)
		}
	}
}
