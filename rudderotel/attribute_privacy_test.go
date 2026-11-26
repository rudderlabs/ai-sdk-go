package rudderotel

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

func TestOTLPPrivacyAllAttributeLocations(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(strconvBool(capture), func(t *testing.T) {
			hook.Reset()
			t.Cleanup(hook.Reset)
			if err := rudderstackai.Configure(rudderstackai.WithRedactHook(func(s string) (string, error) {
				if strings.Contains(s, "withhold") {
					return "", errors.New("withheld")
				}
				return s + " masked", nil
			})); err != nil {
				t.Fatal(err)
			}
			attrs := []attribute.KeyValue{
				attribute.String("service.name", "safe"), attribute.String("gen_ai.input.messages", "a@b.com"),
				attribute.StringSlice("gen_ai.tool.call.arguments", []string{"a@b.com", "hello"}),
				attribute.String("input.value", "withhold"), attribute.StringSlice("output.value", []string{"hello", "withhold"}),
				{Key: "gen_ai.tool.call.result", Value: attribute.MapValue(attribute.KeyValue{
					Key: "nested", Value: attribute.SliceValue(attribute.StringValue("hello"), attribute.StringValue("withhold")),
				})},
				{Key: "gen_ai.retrieval.documents", Value: attribute.SliceValue(attribute.MapValue(attribute.String("text", "withhold")))},
				attribute.String("url.full", "secret"), attribute.String("http.request.header.authorization", "secret"),
			}
			recorder := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder), sdktrace.WithResource(resource.NewSchemaless(attrs...)))
			t.Cleanup(func() {
				if err := provider.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
			})
			_, span := provider.Tracer("test", trace.WithInstrumentationAttributes(attrs...)).Start(context.Background(), "chat", trace.WithAttributes(attrs...), trace.WithLinks(trace.Link{SpanContext: trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}}), Attributes: attrs}))
			span.AddEvent("metadata", trace.WithAttributes(attrs...))
			span.AddEvent("gen_ai.user.message", trace.WithAttributes(attribute.String("body", "a@b.com"), attribute.String("url.full", "secret")))
			span.End()
			source := recorder.Ended()[0]
			otlp, _ := spanToOTLP(source, capture)
			rs := otlp["resourceSpans"].([]object)[0]
			ss := rs["scopeSpans"].([]object)[0]
			s := ss["spans"].([]object)[0]
			locations := map[string][]object{
				"resource": rs["resource"].(object)["attributes"].([]object),
				"scope":    ss["scope"].(object)["attributes"].([]object),
				"span":     s["attributes"].([]object), "link": s["links"].([]object)[0]["attributes"].([]object),
				"event": s["events"].([]object)[0]["attributes"].([]object),
			}
			want := []object{{"key": "service.name", "value": object{"stringValue": "safe"}}}
			if capture {
				want = append(want, object{"key": "gen_ai.input.messages", "value": object{"stringValue": "[email] masked"}}, object{"key": "gen_ai.tool.call.arguments", "value": object{"arrayValue": object{"values": []object{{"stringValue": "[email] masked"}, {"stringValue": "hello masked"}}}}})
			}
			sort.Slice(want, func(i, j int) bool { return want[i]["key"].(string) < want[j]["key"].(string) })
			for name, got := range locations {
				sort.Slice(got, func(i, j int) bool { return got[i]["key"].(string) < got[j]["key"].(string) })
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s=%v want %v", name, got, want)
				}
			}
			events := s["events"].([]object)
			if capture {
				if len(events) != 2 || !reflect.DeepEqual(events[1]["attributes"], []object{{"key": "body", "value": object{"stringValue": "[email] masked"}}}) {
					t.Errorf("content event=%v", events)
				}
			} else if len(events) != 1 {
				t.Errorf("content events retained: %v", events)
			}
			if len(source.Attributes()) != len(attrs) || source.Resource().Set().Len() != len(attrs) {
				t.Fatal("source mutated")
			}
		})
	}
}
