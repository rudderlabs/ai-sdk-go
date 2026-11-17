package rudderotel

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
	"weak"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func privateOwnerAttrs() []attribute.KeyValue {
	return []attribute.KeyValue{attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.provider.name", "openai"), attribute.String("gen_ai.request.model", "private-model"), attribute.Int("gen_ai.usage.input_tokens", 7), attribute.String("gen_ai.output.messages", `[{"role":"assistant","parts":[{"type":"text","content":"tenant-a-secret"}]}]`)}
}

// An explicit interaction_id routes an async span at start; reusing that ID must
// never reroute the already-started call into the next tenant at end.
func TestNamedSpanTenantIsolation(t *testing.T) {
	for _, lifecycle := range []string{"paused", "ended", "open"} {
		for _, ambient := range []bool{false, true} {
			for _, simple := range []bool{true, false} {
				for _, emit := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/ambient=%v/simple=%v/emit=%v", lifecycle, ambient, simple, emit), func(t *testing.T) {
						provider, p, _ := testProvider(t, Options{Simple: simple, ProductEvents: true, CaptureContent: rudderstackai.Ptr(true)})
						configureSpans(t, rudderstackai.WithEmitSpans(emit), rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true))
						clientA, eventsA := recorder(t)
						clientB, eventsB := recorder(t)
						_, a := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: "shared", UserID: "tenant-a", ConversationID: "conv-a", Analytics: clientA, Classify: rudderstackai.Ptr(false)})
						ctx := context.Background()
						if ambient {
							var c *rudderstackai.Interaction
							ctx, c = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{InteractionID: "ambient", UserID: "tenant-c", Classify: rudderstackai.Ptr(false)})
							defer c.End(rudderstackai.EndOptions{})
						}
						attrs := append(privateOwnerAttrs(), attribute.String(InteractionIDAttribute, "shared"), attribute.String("user.id", "tenant-a"), attribute.String("gen_ai.conversation.id", "conv-a"))
						_, span := provider.Tracer("async").Start(ctx, "tenant-a-private", trace.WithAttributes(attrs...))
						switch lifecycle {
						case "paused":
							if _, ok := a.Pause(rudderstackai.PauseOptions{}); !ok {
								t.Fatal("pause failed")
							}
						case "ended":
							a.End(rudderstackai.EndOptions{})
						}
						_, b := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: "shared", UserID: "tenant-b", ConversationID: "conv-b", Analytics: clientB, Classify: rudderstackai.Ptr(false)})
						span.End()
						if err := p.ForceFlush(context.Background()); err != nil {
							t.Fatal(err)
						}
						b.End(rudderstackai.EndOptions{})
						a.End(rudderstackai.EndOptions{})
						for _, e := range eventsB() {
							if e["event"] == "ai_llm_response_received" || e["event"] == "ai_response_received" && e["properties"].(object)["llm_call_count"] != float64(0) || strings.Contains(render(e), "tenant-a-secret") {
								t.Errorf("cross-tenant call/content delivered to tenant-b: %s", render(e))
							}
						}
						calls := 0
						for _, e := range eventsA() {
							if e["event"] == "ai_llm_response_received" {
								calls++
								if e["userId"] != "tenant-a" || e["properties"].(object)["conversation_id"] != "conv-a" || !strings.Contains(render(e), "tenant-a-secret") {
									t.Errorf("original owner lost identity/content: %s", render(e))
								}
							}
						}
						want := 1
						if lifecycle == "ended" {
							want = 0
						}
						if calls != want {
							t.Errorf("original owner calls=%d, want %d", calls, want)
						}
					})
				}
			}
		}
	}
}

func TestProductSpanUnknownOwnerResolvedAtEnd(t *testing.T) {
	for _, lateID := range []bool{false, true} {
		t.Run(fmt.Sprintf("late-id=%v", lateID), func(t *testing.T) {
			provider, p, events := testProvider(t, Options{Simple: true, ProductEvents: true})
			configureSpans(t, rudderstackai.WithLLMCallEvents(true))
			attrs := privateOwnerAttrs()
			if !lateID {
				attrs = append(attrs, attribute.String(InteractionIDAttribute, "unknown-at-start"))
			}
			_, span := provider.Tracer("async").Start(context.Background(), "unknown", trace.WithAttributes(attrs...))
			_, owner := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: "unknown-at-start", UserID: "owner", Classify: rudderstackai.Ptr(false)})
			span.SetAttributes(attribute.String(InteractionIDAttribute, owner.ID()))
			span.End()
			owner.End(rudderstackai.EndOptions{})
			if err := p.ForceFlush(context.Background()); err != nil {
				t.Fatal(err)
			}
			calls := 0
			for _, e := range events() {
				if e["event"] == "ai_llm_response_received" && e["userId"] == "owner" {
					calls++
				}
			}
			if calls != 1 {
				t.Fatalf("unknown owner calls=%d, want 1", calls)
			}
		})
	}
}

func TestProductSpanChangedIDCannotReroute(t *testing.T) {
	for _, operation := range []string{"chat", "execute_tool"} {
		for _, ambient := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ambient=%v", operation, ambient), func(t *testing.T) {
				provider, p, _ := testProvider(t, Options{Simple: true, ProductEvents: true})
				configureSpans(t, rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true))
				clientA, _ := recorder(t)
				clientB, eventsB := recorder(t)
				actx, a := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: "owner-a", UserID: "tenant-a", Analytics: clientA, Classify: rudderstackai.Ptr(false)})
				defer a.End(rudderstackai.EndOptions{})
				ctx := context.Background()
				if ambient {
					ctx = actx
				}
				// Pin the owner before either the AI role or output is known.
				_, span := provider.Tracer("async").Start(ctx, "late-role", trace.WithAttributes(attribute.String(InteractionIDAttribute, a.ID())))
				_, b := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: "owner-b", UserID: "tenant-b", Analytics: clientB, Classify: rudderstackai.Ptr(false)})
				span.SetAttributes(attribute.String(InteractionIDAttribute, b.ID()), attribute.String("gen_ai.operation.name", operation), attribute.String("gen_ai.completion.0.content", "tenant-a-secret"), attribute.String("gen_ai.tool.name", "private-tool"), attribute.String("gen_ai.tool.call.result", "tenant-a-secret"))
				span.End()
				b.End(rudderstackai.EndOptions{})
				if err := p.ForceFlush(context.Background()); err != nil {
					t.Fatal(err)
				}
				for _, e := range eventsB() {
					if e["event"] == "ai_response_received" {
						props := e["properties"].(object)
						if props["llm_call_count"] != float64(0) || props["tool_call_count"] != float64(0) {
							t.Errorf("changed ID rerouted product accounting: %s", render(e))
						}
					}
					if e["event"] == "ai_llm_response_received" || strings.Contains(render(e), "tenant-a-secret") || strings.Contains(render(e), "private-tool") {
						t.Errorf("changed ID rerouted private product data: %s", render(e))
					}
				}
			})
		}
	}
}

// Only the snapshot and span survive the request. Product delivery must keep the
// resolved owner alive until End, including spans that acquire their AI role late.
func TestPausedInFlightSpanOwnerDelivery(t *testing.T) {
	for _, routing := range []string{"ambient-late", "detached", "foreign-ambient"} {
		for _, simple := range []bool{true, false} {
			for _, emit := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/simple=%v/emit=%v", routing, simple, emit), func(t *testing.T) {
					provider, p, events := testProvider(t, Options{Simple: simple, ProductEvents: true})
					configureSpans(t, rudderstackai.WithEmitSpans(emit), rudderstackai.WithLLMCallEvents(true))
					snapshot, ref, span := func() (rudderstackai.PausedInteraction, weak.Pointer[rudderstackai.Interaction], trace.Span) {
						actx, a := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: "shared", UserID: "tenant-a", ConversationID: "conv-a", Classify: rudderstackai.Ptr(false)})
						ctx := context.Background()
						attrs := append(privateOwnerAttrs(), attribute.String(InteractionIDAttribute, "shared"))
						if routing == "ambient-late" {
							ctx = actx
							attrs = nil
						}
						if routing == "foreign-ambient" {
							var c *rudderstackai.Interaction
							ctx, c = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{InteractionID: "ambient", UserID: "tenant-c", Classify: rudderstackai.Ptr(false)})
							t.Cleanup(func() { c.End(rudderstackai.EndOptions{}) })
						}
						_, span := provider.Tracer("async").Start(ctx, "in-flight", trace.WithAttributes(attrs...))
						snapshot, ok := a.Pause(rudderstackai.PauseOptions{})
						if !ok {
							t.Fatal("pause failed")
						}
						return snapshot, weak.Make(a), span
					}()
					runtime.GC()
					runtime.GC()
					if ref.Value() == nil {
						t.Error("in-flight span lost its paused owner after GC")
					}
					span.SetAttributes(privateOwnerAttrs()...)
					span.End()
					// Span calls are finalized at a segment boundary, as on base.
					// A successful report resumes A; pause it again to flush and release it.
					if a := ref.Value(); a != nil {
						if a.Paused() {
							t.Error("in-flight call did not resume its owner")
						}
						a.Pause(rudderstackai.PauseOptions{})
					}
					if err := p.ForceFlush(context.Background()); err != nil {
						t.Fatal(err)
					}
					calls := 0
					for _, e := range events() {
						if e["event"] == "ai_llm_response_received" {
							calls++
							if e["userId"] != "tenant-a" || e["properties"].(object)["conversation_id"] != "conv-a" {
								t.Errorf("foreign delivery: %s", render(e))
							}
						}
					}
					if calls != 1 {
						t.Fatalf("calls=%d, want original owner's in-flight call", calls)
					}
					// The finished span/cache must retain no strong owner.
					deadline := time.Now().Add(5 * time.Second)
					for ref.Value() != nil {
						if time.Now().After(deadline) {
							t.Fatal("finished span retained paused owner")
						}
						runtime.GC()
						time.Sleep(time.Millisecond)
					}
					runtime.KeepAlive(snapshot)
					runtime.KeepAlive(span)
				})
			}
		}
	}
}
