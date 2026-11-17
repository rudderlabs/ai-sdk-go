package rudderotel

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// Background work may start after its interaction ends. A matching ended
// context must never route that work to a tenant that reused the same IDs.
func TestEndedAmbientSpanTenantIsolation(t *testing.T) {
	for _, lifecycle := range []string{"end", "run-return"} {
		for _, op := range []string{"chat", "execute_tool"} {
			for _, simple := range []bool{false, true} {
				for _, emit := range []bool{false, true} {
					for _, paused := range []bool{false, true} {
						for _, explicit := range []bool{false, true} {
							t.Run(fmt.Sprintf("%s/%s/simple=%v/emit=%v/paused=%v/explicit=%v", lifecycle, op, simple, emit, paused, explicit), func(t *testing.T) {
								provider, p, _ := testProvider(t, Options{Simple: simple, ProductEvents: true, CaptureContent: rudderstackai.Ptr(true)})
								configureSpans(t, rudderstackai.WithEmitSpans(emit), rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true))
								clientA, eventsA := recorder(t)
								clientB, eventsB := recorder(t)
								opts := rudderstackai.InteractionOptions{InteractionID: "ended-shared", UserID: "same-user", AnonymousID: "same-anonymous", ConversationID: "same-conversation", Analytics: clientA, Classify: rudderstackai.Ptr(false)}
								var actx context.Context
								var a *rudderstackai.Interaction
								if lifecycle == "run-return" {
									if err := rudderstackai.RunInteraction(context.Background(), opts, func(ctx context.Context, i *rudderstackai.Interaction) error {
										actx, a = ctx, i
										return nil
									}); err != nil {
										t.Fatal(err)
									}
								} else {
									actx, a = rudderstackai.StartInteraction(context.Background(), opts)
									a.End(rudderstackai.EndOptions{})
								}
								if !a.Ended() {
									t.Fatal("ambient interaction did not end")
								}
								if err := p.ForceFlush(context.Background()); err != nil {
									t.Fatal(err)
								}
								beforeA := len(eventsA())
								opts.Analytics = clientB
								_, b := rudderstackai.StartInteraction(context.Background(), opts)
								defer b.End(rudderstackai.EndOptions{})
								if paused {
									if _, ok := b.Pause(rudderstackai.PauseOptions{}); !ok {
										t.Fatal("pause failed")
									}
								}
								attrs := []attribute.KeyValue{
									attribute.String("gen_ai.operation.name", op),
									attribute.String("gen_ai.provider.name", "openai"),
									attribute.String("gen_ai.request.model", "mock"),
									attribute.Int("gen_ai.usage.input_tokens", 17),
									attribute.Int("gen_ai.usage.output_tokens", 19),
									attribute.String("gen_ai.prompt.0.content", "A_PRIVATE_ASYNC_INPUT"),
									attribute.String("gen_ai.completion.0.content", "A_PRIVATE_ASYNC_OUTPUT"),
									attribute.String("gen_ai.tool.name", "lookup"),
									attribute.String("gen_ai.tool.call.id", "ended-tool"),
									attribute.String("gen_ai.tool.call.arguments", `{"secret":"A_PRIVATE_ASYNC_ARGS"}`),
									attribute.String("gen_ai.tool.call.result", `{"secret":"A_PRIVATE_ASYNC_RESULT"}`),
								}
								if explicit {
									attrs = append(attrs, attribute.String(InteractionIDAttribute, a.ID()))
								}
								done := make(chan struct{})
								go func() {
									defer close(done)
									_, span := provider.Tracer("async").Start(actx, "ended continuation", trace.WithAttributes(attrs...))
									span.End()
								}()
								<-done
								if err := p.ForceFlush(context.Background()); err != nil {
									t.Fatal(err)
								}
								if b.Paused() != paused {
									t.Error("foreign span changed the other tenant's pause state")
								}
								b.End(rudderstackai.EndOptions{})
								if err := p.ForceFlush(context.Background()); err != nil {
									t.Fatal(err)
								}
								if afterA := len(eventsA()); afterA != beforeA {
									t.Errorf("ended tenant received events after its end: before=%d after=%d", beforeA, afterA)
								}
								for _, e := range eventsB() {
									if strings.Contains(render(e), "A_PRIVATE_ASYNC") || e["event"] == "ai_llm_response_received" || e["event"] == "ai_tool_called" {
										t.Errorf("other tenant received ended ambient work: %s", render(e))
									}
									if e["event"] == "ai_response_received" || e["event"] == "ai_interaction_paused" {
										props := e["properties"].(object)
										for _, key := range []string{"llm_call_count", "tool_call_count", "input_tokens", "output_tokens", "total_input_tokens", "total_output_tokens", "total_tokens", "cost_usd", "credits"} {
											if value, ok := props[key]; ok && value != float64(0) {
												t.Errorf("other tenant received %s=%v: %s", key, value, render(e))
											}
										}
									}
								}
							})
						}
					}
				}
			}
		}
	}
}
