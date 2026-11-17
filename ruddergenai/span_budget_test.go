package ruddergenai_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/genai"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/ruddergenai"
)

// A context near the span budget cannot hold the whole span message: the call keeps the OTel span.
func TestGenerateContentSpanBudgetNearTheLimit(t *testing.T) {
	for _, c := range []struct {
		pad int
		fit bool
	}{{31_000, false}, {1_000, true}} {
		rec := setup(t, rudderstackai.WithEmitSpans(true))
		var fit []bool
		hook.Tracing.AdoptOTelCall = func(_ context.Context, _ any, _ uint64, fits func(hook.OTelCall) bool) (hook.OTelCall, bool) {
			o := hook.OTelCall{TraceID: strings.Repeat("a", 32), SpanID: strings.Repeat("b", 16), ParentID: strings.Repeat("c", 16), Adopted: true}
			ok := fits(o)
			fit = append(fit, ok)
			return o, ok
		}
		llm.Script("google.generate", jsonReply(reply([]any{m{"text": "Hello"}}, m{"promptTokenCount": 1, "candidatesTokenCount": 1}, "STOP")))
		client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
		ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Context: map[string]any{"pad": strings.Repeat("x", c.pad)}})
		if _, err := client.Models.GenerateContent(ctx, model, genai.Text("Hi"), nil); err != nil {
			t.Fatal(err)
		}
		i.End(rudderstackai.EndOptions{})
		if len(fit) != 1 || fit[0] != c.fit {
			t.Errorf("pad %d: fit checks %v, want [%v]", c.pad, fit, c.fit)
		}
		sent := 0
		for _, e := range rec.events(t) {
			if e.Event == "ai_otel_span" {
				sent++
			}
		}
		if (sent == 1) != c.fit {
			t.Errorf("pad %d: %d SDK spans sent, want %v", c.pad, sent, c.fit)
		}
	}
}
