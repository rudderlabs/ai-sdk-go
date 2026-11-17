package rudderotel

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/ruddertest"
)

func TestRestoreSpanRoutingWithReusedID(t *testing.T) {
	for _, order := range [][]int{{0}, {1}, {0, 1}, {1, 0}} {
		for _, simple := range []bool{true, false} {
			t.Run(fmt.Sprintf("%v/simple=%v", order, simple), func(t *testing.T) {
				hook.Reset()
				t.Cleanup(hook.Reset)
				var clients [2]*ruddertest.Recorder
				var events [2]func() []object
				for n := range clients {
					clients[n], events[n] = recorder(t)
				}
				if err := rudderstackai.Configure(rudderstackai.WithAnalytics(clients[0]), rudderstackai.WithEmitSpans(true), rudderstackai.WithLLMCallEvents(true), rudderstackai.WithCaptureContent(true)); err != nil {
					t.Fatal(err)
				}
				processor, err := NewSpanProcessor(Options{Analytics: clients[0], Simple: simple, ProductEvents: true, CaptureContent: rudderstackai.Ptr(true)})
				if err != nil {
					t.Fatal(err)
				}
				provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(processor))
				t.Cleanup(func() {
					if err := provider.Shutdown(context.Background()); err != nil {
						t.Error(err)
					}
				})
				var saved [2]context.Context
				var original, target [2]*rudderstackai.Interaction
				var snapshots [2]rudderstackai.PausedInteraction
				for n, user := range []string{"tenant-a", "tenant-b"} {
					ctx := rudderstackai.WithValues(context.Background(), rudderstackai.ContextValues{Analytics: clients[n]})
					_, original[n] = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{InteractionID: "shared", UserID: user, ConversationID: user + "-conv", Classify: rudderstackai.Ptr(false)})
					saved[n] = rudderstackai.JoinInteraction(context.Background(), "shared", rudderstackai.JoinOptions{})
					target[n] = original[n]
				}
				for n := range original {
					snapshot, ok := original[n].Pause(rudderstackai.PauseOptions{})
					if !ok {
						t.Fatal("pause failed")
					}
					raw, err := json.Marshal(snapshot)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(raw, &snapshots[n]); err != nil {
						t.Fatal(err)
					}
				}
				for _, n := range order {
					ctx := rudderstackai.WithValues(context.Background(), rudderstackai.ContextValues{Analytics: clients[n]})
					_, target[n] = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{Resume: &snapshots[n], Classify: rudderstackai.Ptr(false)})
				}
				for n := range saved {
					if rudderstackai.FromContext(saved[n]) != target[n] {
						t.Errorf("tenant %d context did not follow its own restore", n)
					}
					_, span := provider.Tracer("app").Start(saved[n], fmt.Sprintf("private-%d", n), trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.provider.name", "openai"), attribute.String("gen_ai.request.model", "gpt-4.1-mini"), attribute.Int("gen_ai.usage.input_tokens", 5), attribute.Int("gen_ai.usage.output_tokens", 2), attribute.String("gen_ai.output.messages", fmt.Sprintf("customer-private-%d", n))))
					span.End()
				}
				if err := provider.ForceFlush(context.Background()); err != nil {
					t.Fatal(err)
				}
				for n := range target {
					target[n].End(rudderstackai.EndOptions{})
					original[n].End(rudderstackai.EndOptions{})
				}
				rudderstackai.Flush(context.Background())
				// The OTel exporter has its own fixed client; product calls use the owning
				// interaction's client. Verify exporter identities separately from call routing.
				spans := [2]int{}
				for _, event := range events[0]() {
					if event["event"] != "ai_otel_span" {
						continue
					}
					props := event["properties"].(object)
					n := -1
					for owner := range clients {
						if props["span_name"] == fmt.Sprintf("private-%d", owner) {
							n = owner
						}
					}
					if n < 0 {
						t.Errorf("unexpected span: %v", props)
						continue
					}
					spans[n]++
					if event["userId"] != []string{"tenant-a", "tenant-b"}[n] || props["conversation_id"] != []string{"tenant-a-conv", "tenant-b-conv"}[n] {
						t.Errorf("span %d has foreign identity: %v", n, event)
					}
				}
				for n := range clients {
					calls := 0
					for _, event := range events[n]() {
						if event["event"] != "ai_llm_response_received" {
							continue
						}
						calls++
						props := event["properties"].(object)
						if event["userId"] != []string{"tenant-a", "tenant-b"}[n] || props["conversation_id"] != []string{"tenant-a-conv", "tenant-b-conv"}[n] {
							t.Errorf("client %d received foreign identity: %v", n, event)
						}
					}
					if spans[n] != 1 || calls != 1 {
						t.Errorf("tenant %d spans=%d calls=%d, want 1 each", n, spans[n], calls)
					}
				}
			})
		}
	}
}
