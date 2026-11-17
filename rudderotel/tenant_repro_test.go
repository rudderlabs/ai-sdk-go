package rudderotel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// Keep registration order A then B in BOTH restore directions. Accept a lost
// detached follow-up; matching ambient follow-ups must reach their own tenant.
func TestFixedOrderSnapshotTenantIsolation(t *testing.T) {
	for _, restoredTenant := range []string{"A", "B"} {
		for _, routing := range []string{"detached", "foreign-ambient", "matching-ambient"} {
			for _, simple := range []bool{false, true} {
				for _, emit := range []bool{false, true} {
					t.Run(fmt.Sprintf("restore=%s/%s/simple=%v/emit=%v", restoredTenant, routing, simple, emit), func(t *testing.T) {
						provider, p, _ := testProvider(t, Options{Simple: simple, ProductEvents: true, CaptureContent: rudderstackai.Ptr(true)})
						configureSpans(t, rudderstackai.WithEmitSpans(emit), rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true))
						clientA, eventsA := recorder(t)
						clientB, eventsB := recorder(t)
						clientC, eventsC := recorder(t)
						cctx, c := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: "foreign", UserID: "foreign", Analytics: clientC, Classify: rudderstackai.Ptr(false)})
						defer c.End(rudderstackai.EndOptions{})
						for _, id := range []string{"1", "2"} {
							opts := rudderstackai.InteractionOptions{InteractionID: id, UserID: "user-1", AnonymousID: "anon-1", ConversationID: "conversation-1", Analytics: clientA, Classify: rudderstackai.Ptr(false)}
							actx, a := rudderstackai.StartInteraction(context.Background(), opts)
							defer a.End(rudderstackai.EndOptions{})
							asnapshot, ok := a.Pause(rudderstackai.PauseOptions{})
							if !ok {
								t.Fatal("pause A failed")
							}
							opts.Analytics = clientB
							bctx, b := rudderstackai.StartInteraction(context.Background(), opts)
							defer b.End(rudderstackai.EndOptions{})
							bsnapshot, ok := b.Pause(rudderstackai.PauseOptions{})
							if !ok {
								t.Fatal("pause B failed")
							}
							snapshot, restoreClient := asnapshot, clientA
							followup, followupCtx, tenant := b, bctx, "B"
							foreignEvents, ownEvents := eventsA, eventsB
							if restoredTenant == "B" {
								snapshot, restoreClient = bsnapshot, clientB
								followup, followupCtx, tenant = a, actx, "A"
								foreignEvents, ownEvents = eventsB, eventsA
							}
							data, err := json.Marshal(snapshot)
							if err != nil {
								t.Fatal(err)
							}
							if err := json.Unmarshal(data, &snapshot); err != nil {
								t.Fatal(err)
							}
							_, restored := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{Resume: &snapshot, Analytics: restoreClient, Classify: rudderstackai.Ptr(false)})
							restored.End(rudderstackai.EndOptions{})
							ctx := context.Background()
							switch routing {
							case "foreign-ambient":
								ctx = cctx
							case "matching-ambient":
								ctx = followupCtx
							}
							input := tenant + "_PRIVATE_INPUT_" + id
							output := tenant + "_PRIVATE_OUTPUT_" + id
							_, span := provider.Tracer("snapshot").Start(ctx, "private follow-up", trace.WithAttributes(
								attribute.String(InteractionIDAttribute, followup.ID()),
								attribute.String("gen_ai.operation.name", "chat"),
								attribute.String("gen_ai.provider.name", "openai"),
								attribute.String("gen_ai.request.model", "mock"),
								attribute.String("gen_ai.prompt.0.content", input),
								attribute.String("gen_ai.completion.0.content", output),
							))
							span.End()
							a.End(rudderstackai.EndOptions{})
							b.End(rudderstackai.EndOptions{})
							if err := p.ForceFlush(context.Background()); err != nil {
								t.Fatal(err)
							}
							for _, get := range []func() []object{foreignEvents, eventsC} {
								for _, e := range get() {
									if strings.Contains(render(e), input) || strings.Contains(render(e), output) {
										t.Errorf("FOREIGN delivery after restore %s, id=%s: %s", restoredTenant, id, render(e))
									}
								}
							}
							received := false
							for _, e := range ownEvents() {
								received = received || strings.Contains(render(e), output)
							}
							if routing == "matching-ambient" && !received {
								t.Errorf("id=%s matching ambient owner lost its follow-up", id)
							}
						}
					})
				}
			}
		}
	}
}

// Main routes the detached duplicate ID to older A. The closure drops it
// because the ID names two live owners. A matching ambient owner still wins.
func TestOlderOpenOwnerAmbiguousAtStart(t *testing.T) {
	for _, routing := range []string{"detached", "matching-ambient"} {
		for _, simple := range []bool{false, true} {
			for _, emit := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/simple=%v/emit=%v", routing, simple, emit), func(t *testing.T) {
					provider, p, _ := testProvider(t, Options{Simple: simple, ProductEvents: true, CaptureContent: rudderstackai.Ptr(true)})
					configureSpans(t, rudderstackai.WithEmitSpans(emit), rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true))
					clientA, eventsA := recorder(t)
					clientB, eventsB := recorder(t)
					opts := rudderstackai.InteractionOptions{InteractionID: "1", UserID: "user-1", AnonymousID: "anon-1", ConversationID: "conversation-1", Analytics: clientA, Classify: rudderstackai.Ptr(false)}
					actx, a := rudderstackai.StartInteraction(context.Background(), opts)
					defer a.End(rudderstackai.EndOptions{})
					opts.Analytics = clientB
					_, b := rudderstackai.StartInteraction(context.Background(), opts)
					defer b.End(rudderstackai.EndOptions{})
					ctx := context.Background()
					if routing == "matching-ambient" {
						ctx = actx
					}
					_, span := provider.Tracer("main-parity").Start(ctx, "A private", trace.WithAttributes(
						attribute.String(InteractionIDAttribute, "1"),
						attribute.String("gen_ai.operation.name", "chat"),
						attribute.String("gen_ai.request.model", "mock"),
						attribute.String("gen_ai.prompt.0.content", "A_PRIVATE_INPUT"),
						attribute.String("gen_ai.completion.0.content", "A_PRIVATE_OUTPUT"),
					))
					span.End()
					a.End(rudderstackai.EndOptions{})
					b.End(rudderstackai.EndOptions{})
					if err := p.ForceFlush(context.Background()); err != nil {
						t.Fatal(err)
					}
					for _, e := range eventsB() {
						if e["event"] == "ai_llm_response_received" || strings.Contains(render(e), "A_PRIVATE_") {
							t.Errorf("FOREIGN delivery to B: %s", render(e))
						}
					}
					calls := 0
					for _, e := range eventsA() {
						if e["event"] == "ai_llm_response_received" {
							calls++
							if !strings.Contains(render(e), "A_PRIVATE_INPUT") || !strings.Contains(render(e), "A_PRIVATE_OUTPUT") {
								t.Errorf("matching ambient owner lost content: %s", render(e))
							}
						}
					}
					want := 0
					if routing == "matching-ambient" {
						want = 1
					}
					if calls != want {
						t.Errorf("A calls=%d, want %d", calls, want)
					}
				})
			}
		}
	}
}
