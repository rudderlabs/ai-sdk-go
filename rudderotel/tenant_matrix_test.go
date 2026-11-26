package rudderotel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	analytics "github.com/rudderlabs/analytics-go/v4"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// Spans retain tenant ownership across pause, end, and restore transitions in either completion order.
func TestDuplicateIDTenantTransitionMatrix(t *testing.T) {
	type row struct {
		duplicate                           bool
		order, initial, transition, routing string
		simple, emit                        bool
	}
	var rows []row
	for _, duplicate := range []bool{false, true} {
		for _, order := range []string{"A-first", "B-first"} {
			for _, initial := range []string{"open", "paused"} {
				for _, transition := range []string{"pause", "end", "restore-A", "restore-B"} {
					for _, routing := range []string{"detached", "matching-ambient", "foreign-ambient"} {
						for _, simple := range []bool{false, true} {
							for _, emit := range []bool{false, true} {
								rows = append(rows, row{duplicate, order, initial, transition, routing, simple, emit})
							}
						}
					}
				}
			}
		}
	}
	for _, tc := range rows {
		duplicate, order, initial, transition, routing, simple, emit := tc.duplicate, tc.order, tc.initial, tc.transition, tc.routing, tc.simple, tc.emit
		t.Run(fmt.Sprintf("duplicate=%v/%s/%s/%s/%s/simple=%v/emit=%v", duplicate, order, initial, transition, routing, simple, emit), func(t *testing.T) {
			provider, processor, _ := testProvider(t, Options{Simple: simple, ProductEvents: true, CaptureContent: rudderstackai.Ptr(true)})
			configureSpans(t, rudderstackai.WithEmitSpans(emit), rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true))
			clients := make([]analytics.Client, 2)
			events := make([]func() []object, 2)
			owners := make([]*rudderstackai.Interaction, 2)
			contexts := make([]context.Context, 2)
			snapshots := make([]rudderstackai.PausedInteraction, 2)
			for tenant := range clients {
				clients[tenant], events[tenant] = recorder(t)
			}
			start := func(tenant int) {
				contexts[tenant], owners[tenant] = rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{
					InteractionID: "1", UserID: "shared-user", AnonymousID: "shared-anonymous", ConversationID: "shared-conversation", Analytics: clients[tenant], Classify: rudderstackai.Ptr(false),
				})
				t.Cleanup(func() { owners[tenant].End(rudderstackai.EndOptions{}) })
			}
			if duplicate && order == "B-first" {
				start(1)
			}
			start(0)
			if duplicate && order == "A-first" {
				start(1)
			}
			pause := func(tenant int) {
				var ok bool
				snapshots[tenant], ok = owners[tenant].Pause(rudderstackai.PauseOptions{})
				if !ok {
					t.Fatal("pause failed")
				}
			}
			if initial == "paused" {
				pause(0)
				if duplicate {
					pause(1)
				}
			}
			foreignClient, foreignEvents := recorder(t)
			foreignCtx, foreign := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: "foreign", UserID: "foreign-user", Analytics: foreignClient, Classify: rudderstackai.Ptr(false)})
			defer foreign.End(rudderstackai.EndOptions{})
			spans := make([]trace.Span, 2)
			begin := func(tenant int, phase string) trace.Span {
				ctx := context.Background()
				switch routing {
				case "matching-ambient":
					ctx = contexts[tenant]
				case "foreign-ambient":
					ctx = foreignCtx
				}
				label := fmt.Sprintf("%c_PRIVATE_%s", 'A'+tenant, phase)
				_, span := provider.Tracer("tenant-matrix").Start(ctx, label, trace.WithAttributes(
					attribute.String(InteractionIDAttribute, "1"), attribute.String("gen_ai.operation.name", "chat"),
					attribute.String("gen_ai.request.model", "mock"), attribute.String("gen_ai.prompt.0.content", label+"_INPUT"), attribute.String("gen_ai.completion.0.content", label+"_OUTPUT"),
				))
				return span
			}
			spans[0] = begin(0, "BEFORE")
			if duplicate {
				spans[1] = begin(1, "BEFORE")
			}
			target := 1
			if !duplicate || transition == "restore-A" {
				target = 0
			}
			var restored *rudderstackai.Interaction
			switch transition {
			case "pause":
				if !owners[target].Paused() {
					pause(target)
				}
			case "end":
				if duplicate {
					owners[target].End(rudderstackai.EndOptions{})
				}
			case "restore-A", "restore-B":
				if !owners[target].Paused() {
					pause(target)
				}
				data, err := json.Marshal(snapshots[target])
				if err != nil {
					t.Fatal(err)
				}
				var snapshot rudderstackai.PausedInteraction
				if err := json.Unmarshal(data, &snapshot); err != nil {
					t.Fatal(err)
				}
				restoredCtx, restoredOwner := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{Resume: &snapshot, Analytics: clients[target], Classify: rudderstackai.Ptr(false)})
				restored = restoredOwner
				if !duplicate {
					contexts[target] = restoredCtx
				}
				t.Cleanup(func() { restored.End(rudderstackai.EndOptions{}) })
			}
			after := begin(0, "AFTER")
			for _, span := range spans {
				if span != nil {
					span.End()
				}
			}
			after.End()
			if restored != nil {
				restored.End(rudderstackai.EndOptions{})
			}
			for _, owner := range owners {
				if owner != nil {
					owner.End(rudderstackai.EndOptions{})
				}
			}
			if err := processor.ForceFlush(context.Background()); err != nil {
				t.Fatal(err)
			}
			for tenant, get := range events {
				calls := 0
				for _, event := range get() {
					if strings.Contains(render(event), fmt.Sprintf("%c_PRIVATE_", 'A'+(1-tenant))) {
						t.Errorf("foreign identity/input/output: %s", render(event))
					}
					if event["event"] == "ai_llm_response_received" {
						calls++
						props := event["properties"].(object)
						if event["userId"] != "shared-user" || event["anonymousId"] != "shared-anonymous" || props["conversation_id"] != "shared-conversation" {
							t.Errorf("owner identity lost: %s", render(event))
						}
						if !strings.Contains(render(event), "_INPUT") || !strings.Contains(render(event), "_OUTPUT") {
							t.Errorf("owner content lost: %s", render(event))
						}
					}
				}
				want := 0
				if tenant == 0 && !duplicate {
					want = 2
				}
				if tenant == 0 && duplicate && transition == "end" {
					want = 1
				}
				if duplicate && routing == "matching-ambient" {
					if tenant == 0 {
						want = 2
					} else if transition != "end" {
						want = 1
					}
				}
				if calls != want {
					t.Errorf("tenant %c calls=%d, want %d", 'A'+tenant, calls, want)
				}
			}
			for _, event := range foreignEvents() {
				if strings.Contains(render(event), "_PRIVATE_") {
					t.Errorf("foreign ambient client received content: %s", render(event))
				}
			}
		})
	}
}

// SetAttributes is an ordinary OTel API: absent-at-start IDs can be supplied
// later, but end-time lookup must still reject duplicates in either registry.
func TestLateInteractionIDRequiresUniqueOwner(t *testing.T) {
	for _, state := range []string{"unique", "open", "paused", "ended", "restored"} {
		for _, simple := range []bool{false, true} {
			for _, emit := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/simple=%v/emit=%v", state, simple, emit), func(t *testing.T) {
					provider, processor, _ := testProvider(t, Options{Simple: simple, ProductEvents: true, CaptureContent: rudderstackai.Ptr(true)})
					configureSpans(t, rudderstackai.WithEmitSpans(emit), rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true))
					clientA, eventsA := recorder(t)
					clientB, eventsB := recorder(t)
					_, span := provider.Tracer("late-id").Start(context.Background(), "late ID", trace.WithAttributes(
						attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.request.model", "mock"),
						attribute.String("gen_ai.prompt.0.content", "A_PRIVATE_INPUT"), attribute.String("gen_ai.completion.0.content", "A_PRIVATE_OUTPUT"),
					))
					opts := rudderstackai.InteractionOptions{InteractionID: "late", UserID: "shared-user", AnonymousID: "shared-anonymous", ConversationID: "shared-conversation", Analytics: clientA, Classify: rudderstackai.Ptr(false)}
					_, a := rudderstackai.StartInteraction(context.Background(), opts)
					defer a.End(rudderstackai.EndOptions{})
					var b *rudderstackai.Interaction
					if state != "unique" {
						opts.Analytics = clientB
						_, b = rudderstackai.StartInteraction(context.Background(), opts)
						defer b.End(rudderstackai.EndOptions{})
						switch state {
						case "paused", "restored":
							if _, ok := a.Pause(rudderstackai.PauseOptions{}); !ok {
								t.Fatal("pause A failed")
							}
							snapshot, ok := b.Pause(rudderstackai.PauseOptions{})
							if !ok {
								t.Fatal("pause B failed")
							}
							if state == "restored" {
								data, err := json.Marshal(snapshot)
								if err != nil {
									t.Fatal(err)
								}
								if err := json.Unmarshal(data, &snapshot); err != nil {
									t.Fatal(err)
								}
								// Without a pause token (an older SDK version) the restore takes no slot,
								// so B stays live and the late ID stays ambiguous.
								snapshot.PauseToken = ""
								_, restored := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{Resume: &snapshot, Analytics: clientB, Classify: rudderstackai.Ptr(false)})
								restored.End(rudderstackai.EndOptions{})
							}
						case "ended":
							b.End(rudderstackai.EndOptions{})
						}
					}
					span.SetAttributes(attribute.String(InteractionIDAttribute, "late"))
					span.End()
					a.End(rudderstackai.EndOptions{})
					if err := processor.ForceFlush(context.Background()); err != nil {
						t.Fatal(err)
					}
					calls := 0
					for _, event := range eventsA() {
						if event["event"] == "ai_llm_response_received" {
							calls++
							if !strings.Contains(render(event), "A_PRIVATE_INPUT") || !strings.Contains(render(event), "A_PRIVATE_OUTPUT") {
								t.Errorf("own call lost content: %s", render(event))
							}
						}
					}
					want := 0
					if state == "unique" || state == "ended" {
						want = 1
					}
					if calls != want {
						t.Errorf("calls=%d, want %d", calls, want)
					}
					for _, event := range eventsB() {
						if strings.Contains(render(event), "A_PRIVATE_") {
							t.Errorf("late ID delivered to foreign client: %s", render(event))
						}
					}
				})
			}
		}
	}
}
