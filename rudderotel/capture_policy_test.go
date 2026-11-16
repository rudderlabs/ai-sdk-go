package rudderotel

import (
	"encoding/json"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// SDK-6888: the exporter's ContentCapture keeps span content per kind over its CaptureContent.
func TestExporterContentCapturePerKind(t *testing.T) {
	source := tracetest.SpanStub{Attributes: []attribute.KeyValue{
		attribute.String("gen_ai.input.messages", `[{"role":"user","content":"hi"}]`),
		attribute.String("gen_ai.tool.call.arguments", `{"order":"A-1"}`),
		attribute.String("url.full", "https://api.example.com/v1/chat?key=secret"),
	}}
	for _, tc := range []struct {
		name string
		opts Options
		want []string
	}{
		{"bool on keeps messages and tool calls", Options{CaptureContent: rudderstackai.Ptr(true)}, []string{"gen_ai.input.messages", "gen_ai.tool.call.arguments"}},
		{"tool calls only", Options{CaptureContent: rudderstackai.Ptr(true), ContentCapture: &rudderstackai.ContentCapture{Spans: &rudderstackai.SpanCapture{Messages: rudderstackai.Ptr(false)}}}, []string{"gen_ai.tool.call.arguments"}},
		{"http opt-in", Options{ContentCapture: &rudderstackai.ContentCapture{Spans: &rudderstackai.SpanCapture{HTTP: rudderstackai.Ptr(true)}}}, []string{"url.full"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			track, err := SpanToTrack(source.Snapshot(), tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			otlp := track.Properties["otlp"].(object)
			span := otlp["resourceSpans"].([]object)[0]["scopeSpans"].([]object)[0]["spans"].([]object)[0]
			var keys []string
			for _, kv := range span["attributes"].([]object) {
				keys = append(keys, kv["key"].(string))
			}
			if strings.Join(keys, ",") != strings.Join(tc.want, ",") {
				b, _ := json.Marshal(span["attributes"])
				t.Fatalf("attributes %s, want keys %v", b, tc.want)
			}
			if b, _ := json.Marshal(span["attributes"]); strings.Contains(string(b), "secret") {
				t.Fatalf("query kept: %s", b)
			}
		})
	}
}
