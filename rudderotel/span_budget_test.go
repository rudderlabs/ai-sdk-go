package rudderotel

import (
	"context"
	"encoding/json"
	"math"
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

// A message context that cannot be encoded makes the SDK span unsendable, so it never covers or
// replaces the OTel span: the app's model span reaches RudderStack as without dedup, in every
// processor mode, for inner coverage and outer adoption.
func TestSpanBudgetUnencodableContextKeepsTheOTelSpan(t *testing.T) {
	for name, value := range map[string]any{
		"NaN": math.NaN(), "+Inf": math.Inf(1), "-Inf": math.Inf(-1),
		"invalid json.Number in a map":  map[string]any{"n": json.Number("not-a-number")},
		"invalid json.Number in a list": []any{1, json.Number("1e")},
	} {
		for _, inner := range []bool{true, false} {
			for _, simple := range []bool{true, false} {
				t.Run(name+"/inner="+strconvBool(inner)+"/simple="+strconvBool(simple), func(t *testing.T) {
					provider, _, events := testProvider(t, Options{Simple: simple})
					configureSpans(t)
					ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Context: map[string]any{"bad": value}})
					model := trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"))
					var app trace.Span
					if inner {
						wctx, s := hook.Tracing.BeginWrapper(ctx, i, time.Now())
						_, app = provider.Tracer("app").Start(wctx, "chat app", model)
						app.End()
						hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "w", InputTokens: 1, OutputTokens: 1}, s, 200)
					} else {
						var octx context.Context
						octx, app = provider.Tracer("app").Start(ctx, "chat app", model)
						wrapperIn(octx, t, i)
						app.End()
					}
					i.End(rudderstackai.EndOptions{})
					if err := provider.ForceFlush(context.Background()); err != nil {
						t.Fatal(err)
					}
					n := 0
					for _, e := range spanEvents(events()) {
						if p := e["properties"].(object); p["span_name"] == "chat app" && p["span_id"] == app.SpanContext().SpanID().String() {
							n++
						}
					}
					if n != 1 {
						t.Errorf("the app's model span was exported %d times, want once", n)
					}
				})
			}
		}
	}
}
