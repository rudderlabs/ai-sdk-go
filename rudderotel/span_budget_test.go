package rudderotel

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// A model call whose message context nears the span budget used to lose both spans: the check
// measured the context and identity only, so the SDK span covered or replaced the OTel span and
// then failed to fit at delivery. Exactly one span must reach RudderStack, the SDK's only when
// its whole message fits.
func TestSpanBudgetKeepsOneSpanNearTheLimit(t *testing.T) {
	for _, c := range []struct {
		pad    int
		sdkOne bool
	}{{31_000, false}, {31_500, false}, {1_000, true}} {
		for _, inner := range []bool{true, false} {
			name := "outer"
			if inner {
				name = "inner"
			}
			t.Run(name+"/"+itoa(c.pad), func(t *testing.T) {
				provider, _, events := testProvider(t, Options{Simple: true})
				configureSpans(t)
				ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Context: map[string]any{"pad": strings.Repeat("x", c.pad)}})
				model := trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"))
				if inner {
					wctx, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
					_, app := provider.Tracer("app").Start(wctx, "chat gpt", model)
					app.End()
					hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "w", InputTokens: 1, OutputTokens: 1}, s, 200)
				} else {
					octx, app := provider.Tracer("app").Start(ctx, "chat gpt", model)
					wrapperIn(octx, t, i)
					app.End()
				}
				i.End(rudderstackai.EndOptions{})
				spans := spanEvents(events())
				if len(spans) != 1 {
					t.Fatalf("want one span, got %d", len(spans))
				}
				if got := isSDKSpan(spans[0]); got != c.sdkOne {
					t.Errorf("exported span is the SDK's: %v, want %v", got, c.sdkOne)
				}
			})
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
