package rudderbedrock

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

// A context near the span budget cannot hold the whole span message: the call keeps the OTel span.
func TestConverseSpanBudgetNearTheLimit(t *testing.T) {
	for _, c := range []struct {
		pad int
		fit bool
	}{{31_000, false}, {1_000, true}} {
		rec := setup(t)
		if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
			t.Fatal(err)
		}
		var fit []bool
		hook.Tracing.AdoptOTelCall = func(_ context.Context, _ any, _ uint64, fits func(hook.OTelCall) bool) (hook.OTelCall, bool) {
			o := hook.OTelCall{TraceID: strings.Repeat("a", 32), SpanID: strings.Repeat("b", 16), ParentID: strings.Repeat("c", 16), Adopted: true}
			ok := fits(o)
			fit = append(fit, ok)
			return o, ok
		}
		llm.ScriptBedrock("bedrock.converse", testkit.BedrockReply{RequestID: "req-1", Response: raw(`{"output":{"message":{"role":"assistant","content":[{"text":"Hello"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1}}`)})
		ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Context: map[string]any{"pad": strings.Repeat("x", c.pad)}})
		if _, err := client().Converse(ctx, &bedrockruntime.ConverseInput{ModelId: aws.String(model), Messages: []types.Message{userText("hi")}}); err != nil {
			t.Fatal(err)
		}
		i.End(rudderstackai.EndOptions{})
		if len(fit) != 1 || fit[0] != c.fit {
			t.Errorf("pad %d: fit checks %v, want [%v]", c.pad, fit, c.fit)
		}
		if sent := len(rec.named(t, "ai_otel_span")); (sent == 1) != c.fit {
			t.Errorf("pad %d: %d SDK spans sent, want %v", c.pad, sent, c.fit)
		}
	}
}
