package rudderotel

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestSpanErrorContentPolicy(t *testing.T) {
	for _, c := range []struct {
		name     string
		capture  bool
		explicit *bool
		withhold bool
		wantText bool
	}{
		{name: "capture off"},
		{name: "capture on", capture: true, wantText: true},
		{name: "explicit false", capture: true, explicit: rudderstackai.Ptr(false)},
		{name: "explicit true", explicit: rudderstackai.Ptr(true), wantText: true},
		{name: "withheld", capture: true, withhold: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			recorder := tracetest.NewSpanRecorder()
			provider, _, events := testProvider(t, Options{Simple: true, CaptureContent: c.explicit}, recorder)
			if err := rudderstackai.Configure(rudderstackai.WithCaptureContent(c.capture), rudderstackai.WithRedactHook(func(s string) (string, error) {
				if c.withhold {
					return "", errors.New("withheld")
				}
				return strings.ReplaceAll(s, "secret", "[masked]") + " hook", nil
			})); err != nil {
				t.Fatal(err)
			}
			_, span := provider.Tracer("test").Start(context.Background(), "chat", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
			span.RecordError(errors.New("secret prompt person@example.com"), trace.WithAttributes(attribute.String("exception.stacktrace", "secret stack person@example.com")))
			span.SetStatus(codes.Error, "secret status person@example.com")
			span.End()
			got := spanEvents(events())
			if len(got) != 1 {
				t.Fatalf("spans=%v", got)
			}
			p := got[0]["properties"].(object)
			rs := p["otlp"].(object)["resourceSpans"].([]any)[0].(object)
			ss := rs["scopeSpans"].([]any)[0].(object)
			s := ss["spans"].([]any)[0].(object)
			status := s["status"].(object)
			if p["status_code"] != "error" || status["code"] != float64(2) {
				t.Errorf("status code lost: summary=%v OTLP=%v", p["status_code"], status)
			}
			wantAttrs := []object{{"key": "exception.type", "value": object{"stringValue": "*errors.errorString"}}}
			if c.wantText {
				wantStatus := "[masked] status [email] hook"
				if p["status_message"] != wantStatus || status["message"] != wantStatus {
					t.Errorf("status redaction: summary=%v OTLP=%v", p["status_message"], status)
				}
				wantAttrs = append([]object{{"key": "exception.stacktrace", "value": object{"stringValue": "[masked] stack [email] hook"}}}, wantAttrs...)
				wantAttrs = append(wantAttrs, object{"key": "exception.message", "value": object{"stringValue": "[masked] prompt [email] hook"}})
			} else {
				if _, exists := p["status_message"]; exists {
					t.Error("summary retained error text")
				}
				if _, exists := status["message"]; exists {
					t.Error("OTLP status retained error text")
				}
			}
			exception := s["events"].([]any)[0].(object)
			wantJSON, _ := json.Marshal(wantAttrs)
			var want []any
			if err := json.Unmarshal(wantJSON, &want); err != nil {
				t.Fatal(err)
			}
			if exception["name"] != "exception" || !reflect.DeepEqual(exception["attributes"], want) {
				t.Errorf("exception=%v want attributes %v", exception, want)
			}
			b, err := json.Marshal(got[0])
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "secret") || strings.Contains(string(b), "person@example.com") {
				t.Errorf("unmasked error text: %s", b)
			}
			source := recorder.Ended()[0]
			if source.Status().Description != "secret status person@example.com" || len(source.Events()[0].Attributes) != 3 {
				t.Fatal("source span mutated")
			}
		})
	}
}
