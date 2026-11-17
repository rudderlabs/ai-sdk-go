package rudderotel

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
	"weak"

	analytics "github.com/rudderlabs/analytics-go/v4"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func privateOwnerAttrs() []attribute.KeyValue {
	return []attribute.KeyValue{attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.provider.name", "openai"), attribute.String("gen_ai.request.model", "private-model"), attribute.Int("gen_ai.usage.input_tokens", 7), attribute.String("gen_ai.output.messages", `[{"role":"assistant","parts":[{"type":"text","content":"tenant-a-secret"}]}]`)}
}

// An explicit interaction_id routes an async span at start; reusing that ID must
// never reroute the already-started call into the next tenant at end.
func TestNamedSpanTenantIsolation(t *testing.T) {
	for _, lifecycle := range []string{"paused-before-start", "paused", "ended", "open"} {
		for _, ambient := range []bool{false, true} {
			for _, simple := range []bool{true, false} {
				for _, emit := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/ambient=%v/simple=%v/emit=%v", lifecycle, ambient, simple, emit), func(t *testing.T) {
						provider, p, _ := testProvider(t, Options{Simple: simple, ProductEvents: true, CaptureContent: rudderstackai.Ptr(true)})
						configureSpans(t, rudderstackai.WithEmitSpans(emit), rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true))
						clientA, eventsA := recorder(t)
						clientB, eventsB := recorder(t)
						_, a := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: "shared", UserID: "tenant-a", ConversationID: "conv-a", Analytics: clientA, Classify: rudderstackai.Ptr(false)})
						if lifecycle == "paused-before-start" {
							if _, ok := a.Pause(rudderstackai.PauseOptions{}); !ok {
								t.Fatal("pause failed")
							}
						}
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
	for _, routing := range []string{"ambient-late", "detached", "foreign-ambient", "detached-paused-late"} {
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
						var snapshot rudderstackai.PausedInteraction
						var ok bool
						if routing == "detached-paused-late" {
							snapshot, ok = a.Pause(rudderstackai.PauseOptions{})
							if !ok {
								t.Fatal("pause failed")
							}
							attrs = []attribute.KeyValue{attribute.String(InteractionIDAttribute, "shared")}
						}
						_, span := provider.Tracer("async").Start(ctx, "in-flight", trace.WithAttributes(attrs...))
						if routing != "detached-paused-late" {
							snapshot, ok = a.Pause(rudderstackai.PauseOptions{})
						}
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

// An unrelated tenant's completed run must not remove the still-live paused owner.
func TestReviewPausedOwnerSurvivesForeignEnd(t *testing.T) {
	for _, simple := range []bool{true, false} {
		for _, emit := range []bool{false, true} {
			for _, ambient := range []bool{false, true} {
				t.Run(fmt.Sprintf("simple=%v/emit=%v/ambient=%v", simple, emit, ambient), func(t *testing.T) {
					provider, p, _ := testProvider(t, Options{Simple: simple, ProductEvents: true, CaptureContent: rudderstackai.Ptr(true)})
					configureSpans(t, rudderstackai.WithEmitSpans(emit), rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true))
					clientA, eventsA := recorder(t)
					clientB, _ := recorder(t)
					clientC, eventsC := recorder(t)
					_, a := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: "shared", UserID: "tenant-a", Analytics: clientA, Classify: rudderstackai.Ptr(false)})
					defer a.End(rudderstackai.EndOptions{})
					if _, ok := a.Pause(rudderstackai.PauseOptions{}); !ok {
						t.Fatal("pause failed")
					}
					_, b := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: "shared", UserID: "tenant-b", Analytics: clientB, Classify: rudderstackai.Ptr(false)})
					b.End(rudderstackai.EndOptions{})
					ctx := context.Background()
					if ambient {
						var foreign *rudderstackai.Interaction
						ctx, foreign = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{InteractionID: "foreign", UserID: "foreign", Classify: rudderstackai.Ptr(false)})
						defer foreign.End(rudderstackai.EndOptions{})
					}
					_, span := provider.Tracer("review").Start(ctx, "a-private", trace.WithAttributes(append(privateOwnerAttrs(), attribute.String(InteractionIDAttribute, "shared"))...))
					_, c := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: "shared", UserID: "tenant-c", Analytics: clientC, Classify: rudderstackai.Ptr(false)})
					span.End()
					a.End(rudderstackai.EndOptions{})
					c.End(rudderstackai.EndOptions{})
					if err := p.ForceFlush(context.Background()); err != nil {
						t.Fatal(err)
					}
					for _, e := range eventsC() {
						if e["event"] == "ai_llm_response_received" || strings.Contains(render(e), "tenant-a-secret") {
							t.Errorf("C received A private output: %s", render(e))
						}
					}
					calls := 0
					for _, e := range eventsA() {
						if e["event"] == "ai_llm_response_received" {
							calls++
						}
					}
					if calls != 1 {
						t.Errorf("A calls=%d, want 1", calls)
					}
				})
			}
		}
	}
}

// Completing A's in-flight call must not reclaim B's reused ID before B's next call.
func TestReviewPinnedResumePreservesReplacementSpan(t *testing.T) {
	for _, simple := range []bool{true, false} {
		for _, emit := range []bool{false, true} {
			for _, replacementPaused := range []bool{false, true} {
				for _, ambient := range []string{"none", "foreign", "matching"} {
					t.Run(fmt.Sprintf("simple=%v/emit=%v/replacement-paused=%v/ambient=%v", simple, emit, replacementPaused, ambient), func(t *testing.T) {
						provider, p, _ := testProvider(t, Options{Simple: simple, ProductEvents: true, CaptureContent: rudderstackai.Ptr(true)})
						configureSpans(t, rudderstackai.WithEmitSpans(emit), rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true))
						clientA, eventsA := recorder(t)
						clientB, eventsB := recorder(t)
						_, a := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: "shared", UserID: "tenant-a", Analytics: clientA, Classify: rudderstackai.Ptr(false)})
						_, first := provider.Tracer("review").Start(context.Background(), "a-private", trace.WithAttributes(append(privateOwnerAttrs(), attribute.String(InteractionIDAttribute, "shared"))...))
						a.Pause(rudderstackai.PauseOptions{})
						bctx, b := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: "shared", UserID: "tenant-b", Analytics: clientB, Classify: rudderstackai.Ptr(false)})
						if replacementPaused {
							b.Pause(rudderstackai.PauseOptions{})
						}
						first.End()
						ctx := context.Background()
						switch ambient {
						case "matching":
							ctx = bctx
						case "foreign":
							var foreign *rudderstackai.Interaction
							ctx, foreign = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{InteractionID: "foreign", UserID: "foreign", Classify: rudderstackai.Ptr(false)})
							defer foreign.End(rudderstackai.EndOptions{})
						}
						_, second := provider.Tracer("review").Start(ctx, "b-private", trace.WithAttributes(append(privateOwnerAttrs(), attribute.String(InteractionIDAttribute, "shared"), attribute.String("gen_ai.output.messages", `[{"role":"assistant","parts":[{"type":"text","content":"tenant-b-secret"}]}]`))...))
						second.End()
						a.End(rudderstackai.EndOptions{})
						b.End(rudderstackai.EndOptions{})
						if err := p.ForceFlush(context.Background()); err != nil {
							t.Fatal(err)
						}
						for _, e := range eventsA() {
							if strings.Contains(render(e), "tenant-b-secret") {
								t.Errorf("A received B private output: %s", render(e))
							}
						}
						for _, e := range eventsB() {
							if strings.Contains(render(e), "tenant-a-secret") {
								t.Errorf("B received A private output: %s", render(e))
							}
						}
						for _, events := range []func() []object{eventsA, eventsB} {
							calls := 0
							for _, e := range events() {
								if e["event"] == "ai_llm_response_received" {
									calls++
								}
							}
							if calls != 1 {
								t.Errorf("tenant calls=%d, want 1", calls)
							}
						}
					})
				}
			}
		}
	}
}

func TestSequentialIDsAcrossThreeTenants(t *testing.T) {
	for _, simple := range []bool{true, false} {
		for _, emit := range []bool{false, true} {
			for _, ambient := range []bool{false, true} {
				t.Run(fmt.Sprintf("simple=%v/emit=%v/foreign-ambient=%v", simple, emit, ambient), func(t *testing.T) {
					provider, p, _ := testProvider(t, Options{Simple: simple, ProductEvents: true, CaptureContent: rudderstackai.Ptr(true)})
					configureSpans(t, rudderstackai.WithEmitSpans(emit), rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true))
					clients := make([]analytics.Client, 3)
					events := make([]func() []object, 3)
					for tenant := range clients {
						clients[tenant], events[tenant] = recorder(t)
					}
					ctx := context.Background()
					if ambient {
						var foreign *rudderstackai.Interaction
						ctx, foreign = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{InteractionID: "ambient", UserID: "foreign", Classify: rudderstackai.Ptr(false)})
						defer foreign.End(rudderstackai.EndOptions{})
					}
					for _, id := range []string{"1", "2"} {
						start := func(tenant int) *rudderstackai.Interaction {
							_, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: id, UserID: fmt.Sprintf("tenant-%d", tenant), Analytics: clients[tenant], Classify: rudderstackai.Ptr(false)})
							t.Cleanup(func() { i.End(rudderstackai.EndOptions{}) })
							return i
						}
						pause := func(i *rudderstackai.Interaction) {
							if _, ok := i.Pause(rudderstackai.PauseOptions{}); !ok {
								t.Fatal("pause failed")
							}
						}
						span := func(tenant int) trace.Span {
							attrs := append(privateOwnerAttrs(), attribute.String(InteractionIDAttribute, id), attribute.String("gen_ai.output.messages", fmt.Sprintf(`[{"role":"assistant","parts":[{"type":"text","content":"tenant-%d-secret"}]}]`, tenant)))
							_, s := provider.Tracer("async").Start(ctx, "private", trace.WithAttributes(attrs...))
							return s
						}
						a := start(0)
						first := span(0)
						pause(a)
						b := start(1)
						pause(b)
						first.End() // Resumes A without reclaiming B's lookup.
						second := span(1)
						c := start(2)
						pause(c)
						second.End()                      // Resumes B without reclaiming paused C.
						b.End(rudderstackai.EndOptions{}) // Must preserve paused C.
						third := span(2)
						a.End(rudderstackai.EndOptions{})
						third.End()
						c.End(rudderstackai.EndOptions{})
					}
					if err := p.ForceFlush(context.Background()); err != nil {
						t.Fatal(err)
					}
					for tenant, recorded := range events {
						calls := 0
						for _, e := range recorded() {
							for foreign := range events {
								if foreign != tenant && strings.Contains(render(e), fmt.Sprintf("tenant-%d-secret", foreign)) {
									t.Errorf("tenant %d received tenant %d output: %s", tenant, foreign, render(e))
								}
							}
							if e["event"] == "ai_llm_response_received" {
								calls++
								if !strings.Contains(render(e), fmt.Sprintf("tenant-%d-secret", tenant)) {
									t.Errorf("owner lost private output: %s", render(e))
								}
							}
						}
						want := 2
						if calls != want {
							t.Errorf("tenant %d calls=%d, want %d", tenant, calls, want)
						}
					}
				})
			}
		}
	}
}
