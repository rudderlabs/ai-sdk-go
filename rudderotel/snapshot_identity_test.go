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

func TestDuplicateIdentitySnapshotNeverTakesForeignOwner(t *testing.T) {
	for _, restoreTenant := range []string{"A", "B"} {
		for _, simple := range []bool{true, false} {
			for _, emit := range []bool{false, true} {
				t.Run(fmt.Sprintf("restore=%s/simple=%v/emit=%v", restoreTenant, simple, emit), func(t *testing.T) {
					provider, p, _ := testProvider(t, Options{Simple: simple, ProductEvents: true, CaptureContent: rudderstackai.Ptr(true)})
					configureSpans(t, rudderstackai.WithEmitSpans(emit), rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true))
					clientA, eventsA := recorder(t)
					clientB, eventsB := recorder(t)
					restoreClient, foreignEvents, followupEvents := clientA, eventsA, eventsB
					followupClient, followupTenant := clientB, "B"
					if restoreTenant == "B" {
						restoreClient, foreignEvents, followupEvents = clientB, eventsB, eventsA
						followupClient, followupTenant = clientA, "A"
					}
					// Both original paused owners remain live after the ambiguous
					// restore: the detached follow-up must drop in both directions.
					opts := rudderstackai.InteractionOptions{
						InteractionID: "same-id", UserID: "user-1", AnonymousID: "anon-1", ConversationID: "conversation-1",
						Analytics: restoreClient, Classify: rudderstackai.Ptr(false),
					}
					_, original := rudderstackai.StartInteraction(context.Background(), opts)
					defer original.End(rudderstackai.EndOptions{})
					snapshot, ok := original.Pause(rudderstackai.PauseOptions{})
					if !ok {
						t.Fatal("pause original failed")
					}
					opts.Analytics = followupClient
					_, followup := rudderstackai.StartInteraction(context.Background(), opts)
					defer followup.End(rudderstackai.EndOptions{})
					if _, ok := followup.Pause(rudderstackai.PauseOptions{}); !ok {
						t.Fatal("pause follow-up owner failed")
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
					secret := followupTenant + "_PRIVATE_IDENTICAL_SNAPSHOT_FIELDS"
					_, span := provider.Tracer("snapshot").Start(context.Background(), "private follow-up", trace.WithAttributes(
						attribute.String(InteractionIDAttribute, followup.ID()),
						attribute.String("gen_ai.operation.name", "chat"),
						attribute.String("gen_ai.provider.name", "openai"),
						attribute.String("gen_ai.request.model", "mock"),
						attribute.String("gen_ai.completion.0.content", secret),
					))
					span.End()
					original.End(rudderstackai.EndOptions{})
					followup.End(rudderstackai.EndOptions{})
					if err := p.ForceFlush(context.Background()); err != nil {
						t.Fatal(err)
					}
					for _, e := range foreignEvents() {
						if strings.Contains(render(e), secret) {
							t.Errorf("foreign client received private output: %s", render(e))
						}
					}
					received := false
					for _, e := range followupEvents() {
						received = received || strings.Contains(render(e), secret)
					}
					if received {
						t.Error("ambiguous follow-up should drop")
					}
				})
			}
		}
	}
}
