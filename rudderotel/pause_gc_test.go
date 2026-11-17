package rudderotel

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"testing"
	"time"
	"weak"

	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// Without product recording, even an in-flight span must not retain its owner.
func TestPausedSnapshotDoesNotRetainSpanOwner(t *testing.T) {
	for _, mode := range []string{"identity", "dedup", "finished-wrapper"} {
		for _, simple := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/simple=%v", mode, simple), func(t *testing.T) {
				provider, p, events := testProvider(t, Options{Simple: simple})
				configureSpans(t, rudderstackai.WithLimits(rudderstackai.Limits{MaxOpenInteractions: 2}), rudderstackai.WithEmitSpans(mode != "identity"))
				snapshot, ownerRef, span := func() (rudderstackai.PausedInteraction, weak.Pointer[rudderstackai.Interaction], trace.Span) {
					ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: "shared", UserID: "tenant-a", Classify: rudderstackai.Ptr(false)})
					if mode == "finished-wrapper" {
						wrapperIn(ctx, t, i)
					}
					_, span := provider.Tracer("app").Start(ctx, "model", trace.WithAttributes(modelSpanAttr))
					snapshot, ok := i.Pause(rudderstackai.PauseOptions{})
					if !ok {
						t.Fatal("pause failed")
					}
					return snapshot, weak.Make(i), span
				}()
				deadline := time.Now().Add(5 * time.Second)
				for ownerRef.Value() != nil {
					if time.Now().After(deadline) {
						t.Fatal("span registries retained the dropped paused interaction")
					}
					runtime.GC()
					time.Sleep(time.Millisecond)
				}
				// A new tenant reusing the ID must not receive the collected owner's late span.
				_, other := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{InteractionID: "shared", UserID: "tenant-b", Classify: rudderstackai.Ptr(false)})
				if other.Ended() {
					t.Fatal("second tenant was not admitted")
				}
				span.End()
				if err := p.ForceFlush(context.Background()); err != nil {
					t.Fatal(err)
				}
				other.End(rudderstackai.EndOptions{})
				for _, e := range events() {
					if e["event"] == "ai_response_received" && e["userId"] == "tenant-b" && e["properties"].(object)["llm_call_count"] != float64(0) {
						t.Fatal("late span was redirected to another tenant reusing the ID")
					}
				}
				raw, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				var stored rudderstackai.PausedInteraction
				if err := json.Unmarshal(raw, &stored); err != nil {
					t.Fatal(err)
				}
				_, restored := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{Resume: &stored, Classify: rudderstackai.Ptr(false)})
				if restored.Ended() {
					t.Fatal("JSON restore lost the collected owner's slot")
				}
				restored.End(rudderstackai.EndOptions{})
				runtime.KeepAlive(span)
			})
		}
	}
}
