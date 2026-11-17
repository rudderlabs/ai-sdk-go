package rudderotel

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	ai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func TestPlainProcessorFailedTwinIsNotBilled(t *testing.T) {
	for _, failure := range []string{"api_error", "timeout", "rate_limit", "cancelled"} {
		for _, spanFirst := range []bool{false, true} {
			for _, emit := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/span-first=%t/spans=%t", failure, spanFirst, emit), func(t *testing.T) {
					hook.Reset()
					t.Cleanup(hook.Reset)
					r, events := recorder(t)
					if err := ai.Configure(ai.WithAnalytics(r), ai.WithLLMCallEvents(true), ai.WithEmitSpans(emit), ai.WithPrices(map[string]ai.ModelPrice{"gpt-4o": {TokenPrice: ai.TokenPrice{InputPerMtok: 1000000}}}), ai.WithCredits(func(ai.CreditCall) (*float64, error) { return ai.Ptr(7.0), nil })); err != nil {
						t.Fatal(err)
					}
					processor, err := NewSpanProcessor(Options{Analytics: r, Simple: true, ProductEvents: true})
					if err != nil {
						t.Fatal(err)
					}
					provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(processor))
					t.Cleanup(func() {
						if err := provider.Shutdown(context.Background()); err != nil {
							t.Error(err)
						}
					})
					tracer := provider.Tracer("test")
					ctx, i := ai.StartInteraction(context.Background(), ai.InteractionOptions{UserID: "u", NoPromptEvent: true})
					start := time.Now()
					end := start.Add(10 * time.Millisecond)
					wrapper := func() {
						i.RecordCall(ai.LLMCall{Provider: "openai", Model: "gpt-4o", Integration: "openai", StartedAt: start, EndedAt: end, ErrorType: failure, UsageUnknown: true})
					}
					span := func() {
						_, s := tracer.Start(ctx, "chat gpt-4o", trace.WithTimestamp(start), trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.provider.name", "openai"), attribute.String("gen_ai.request.model", "gpt-4o"), attribute.Int("gen_ai.usage.input_tokens", 10), attribute.Int("gen_ai.usage.output_tokens", 2)))
						s.End(trace.WithTimestamp(end))
					}
					if spanFirst {
						span()
						wrapper()
					} else {
						wrapper()
						span()
					}
					i.End(ai.EndOptions{})
					var calls, responses int
					for _, event := range events() {
						props, _ := event["properties"].(map[string]any)
						switch event["event"] {
						case "ai_llm_response_received":
							calls++
							if props["error_type"] != failure || props["cost_usd"] != nil || props["credits"] != nil {
								t.Fatal(props)
							}
						case "ai_response_received":
							responses++
							if props["error_type"] != failure || props["total_cost_usd"] != nil || props["total_credits"] != nil || props["llm_call_count"] != float64(1) || props["total_input_tokens"] != float64(10) {
								t.Fatal(props)
							}
						}
					}
					if calls != 1 || responses != 1 {
						t.Fatalf("calls=%d responses=%d", calls, responses)
					}
				})
			}
		}
	}
}
