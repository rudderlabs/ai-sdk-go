package rudderotel

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	ai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/serializerfixture"
)

func TestSerializerCapture(t *testing.T) {
	b, err := os.ReadFile("../spec/fixtures/serializer-capture-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []serializerfixture.Case `json:"cases"`
	}
	if err = json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		if !c.AppliesToGo() {
			continue
		}
		c, err := c.ResolveGo()
		if err != nil {
			t.Fatal(err)
		}
		if c.Setup["scenario"] != "exporter_hook" {
			continue
		}
		t.Run(c.ID, func(t *testing.T) {
			hook.Reset()
			t.Cleanup(hook.Reset)
			exporter := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(exporter))
			defer func() { _ = tp.Shutdown(context.Background()) }()
			_, span := tp.Tracer("fixture").Start(context.Background(), "lookup")
			span.SetAttributes(attribute.String("custom", "visible"))
			span.End()
			original := exporter.Ended()[0]
			client, events := recorder(t)
			seen := false
			opts := Options{Analytics: client, ExportAllSpans: true, Simple: true}
			inspect := func(s sdktrace.ReadOnlySpan) {
				seen = true
				if s != original {
					t.Fatal("exporter callback received replacement span")
				}
			}
			switch c.Setup["hook"] {
			case "filter":
				opts.Filter = func(s sdktrace.ReadOnlySpan) bool { inspect(s); return true }
			case "identity":
				opts.ResolveIdentity = func(s sdktrace.ReadOnlySpan) Identity { inspect(s); return Identity{UserID: "u"} }
			case "extra_properties":
				opts.ExtraPropertiesFrom = func(s sdktrace.ReadOnlySpan) map[string]any { inspect(s); return map[string]any{"extra": "visible"} }
			case "span_error_type":
				// Use the fixture's original attributes with the built-in classifier.
				stub := tracetest.SpanStubFromReadOnlySpan(original)
				for key, value := range c.Setup["span_attributes"].(map[string]any) {
					stub.Attributes = append(stub.Attributes, attribute.String(key, value.(string)))
				}
				stub.Status = sdktrace.Status{Code: codes.Error}
				original = stub.Snapshot()
				if got := spanErrorType(original); got != c.Expect["error_type"] {
					t.Fatalf("error type=%q want=%v", got, c.Expect["error_type"])
				}
				seen = true
			default:
				t.Fatalf("unknown exporter hook %q", c.Setup["hook"])
			}
			e, err := NewExporter(opts)
			if err != nil {
				t.Fatal(err)
			}
			if err = e.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{original}); err != nil {
				t.Fatal(err)
			}
			if !seen || len(events()) != 1 {
				t.Fatalf("exporter fixture seen=%v events=%v", seen, events())
			}
			_ = ai.Version
		})
	}
}
