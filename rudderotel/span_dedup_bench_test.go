package rudderotel

import (
	"context"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/ruddertest"
)

// BenchmarkOnStartSpansOff is the processor's per-span start cost inside an interaction with
// spans off, for a non-AI and an AI span.
func BenchmarkOnStartSpansOff(b *testing.B) {
	r, err := ruddertest.NewRecorder(filepath.Join(b.TempDir(), "events.jsonl"))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	p, err := NewSpanProcessor(Options{Analytics: r, Simple: true})
	if err != nil {
		b.Fatal(err)
	}
	source := sdktrace.NewTracerProvider()
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	defer i.End(rudderstackai.EndOptions{})
	for _, c := range []struct {
		name string
		attr attribute.KeyValue
	}{
		{"non-ai", attribute.String("http.request.method", "GET")},
		{"ai", attribute.String("gen_ai.operation.name", "chat")},
	} {
		b.Run(c.name, func(b *testing.B) {
			_, s := source.Tracer("bench").Start(ctx, "span", trace.WithAttributes(c.attr))
			span := s.(sdktrace.ReadWriteSpan)
			b.ReportAllocs()
			for b.Loop() {
				p.OnStart(ctx, span)
			}
		})
	}
}
