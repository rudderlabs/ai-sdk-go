package rudderotel

// Shared fixture, tracing path: the exporter blocks on a full client queue and never drops.

import (
	"context"
	"slices"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

func TestSharedClientQueueFullFixtureTracing(t *testing.T) {
	var f struct {
		Cases []struct {
			Name       string `json:"name"`
			Path       string `json:"path"`
			Capacity   int    `json:"capacity"`
			ExpectedGo struct {
				Delivered []string `json:"delivered"`
				Errors    []string `json:"errors"`
			} `json:"expected_go"`
		} `json:"cases"`
	}
	loadCases(t, "../spec/fixtures/client-queue-full-v0.1.json", &f)
	ran := 0
	for _, c := range f.Cases {
		if c.Path != "tracing" {
			continue // the root package runs the other paths
		}
		ran++
		t.Run(c.Name, func(t *testing.T) {
			if !slices.Equal(c.ExpectedGo.Delivered, []string{"ai_otel_span"}) || len(c.ExpectedGo.Errors) != 0 {
				t.Fatalf("expected_go must deliver every event and report no errors: %+v", c.ExpectedGo)
			}
			hook.Reset()
			t.Cleanup(hook.Reset)
			q := testkit.NewQueueFull(c.Capacity)
			var mu sync.Mutex
			var errs []string
			if err := rudderstackai.Configure(rudderstackai.WithAnalytics(q),
				rudderstackai.WithOnError(func(_ error, info rudderstackai.ErrorInfo) {
					mu.Lock()
					defer mu.Unlock()
					errs = append(errs, info.Kind)
				})); err != nil {
				t.Fatal(err)
			}
			p, err := NewSpanProcessor(Options{Analytics: q, ExportAllSpans: true, Simple: true})
			if err != nil {
				t.Fatal(err)
			}
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(p), sdktrace.WithResource(resource.Empty()))
			got := q.Run(t, 1, func() {
				_, span := provider.Tracer("queue-full").Start(context.Background(), "step")
				span.End()
			})
			if !slices.Equal(got, c.ExpectedGo.Delivered) {
				t.Errorf("delivered = %v, want %v", got, c.ExpectedGo.Delivered)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(errs) != 0 {
				t.Errorf("errors = %v, want none", errs)
			}
		})
	}
	if ran == 0 {
		t.Fatal("no tracing cases")
	}
}
