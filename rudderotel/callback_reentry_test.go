package rudderotel

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestSDK6111TracingCallbacksReenter(t *testing.T) {
	for _, kind := range []string{"filter", "resolve_identity", "extra_properties"} {
		for _, threaded := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/same", true: "/goroutine"}[threaded], func(t *testing.T) {
				var owner *rudderstackai.Interaction
				var fired atomic.Bool
				progress := make(chan bool, 1)
				invoke := func() {
					if fired.CompareAndSwap(false, true) {
						if threaded {
							done := make(chan struct{})
							go func() { defer close(done); owner.RecordCompaction() }()
							select {
							case <-done:
								progress <- true
							case <-time.After(time.Second):
								progress <- false
							}
						} else {
							owner.RecordCompaction()
							progress <- true
						}
					}
				}
				opts := Options{Simple: true}
				switch kind {
				case "filter":
					opts.Filter = func(sdktrace.ReadOnlySpan) bool { invoke(); return true }
				case "resolve_identity":
					opts.ResolveIdentity = func(sdktrace.ReadOnlySpan) Identity { invoke(); return Identity{UserID: "u"} }
				case "extra_properties":
					opts.ExtraPropertiesFrom = func(sdktrace.ReadOnlySpan) map[string]any { invoke(); return nil }
				}
				provider, _, _ := testProvider(t, opts)
				ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
				owner = i
				_, span := provider.Tracer("test").Start(ctx, "chat", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
				done := make(chan struct{})
				go func() { defer close(done); span.End() }()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("tracing callback deadlocked")
				}
				select {
				case ok := <-progress:
					if !ok {
						t.Fatal("callback blocked")
					}
				case <-time.After(time.Second):
					t.Fatal("callback not invoked")
				}
				i.End(rudderstackai.EndOptions{})
			})
		}
	}
}
