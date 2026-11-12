package rudderotel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/ruddertest"
)

type fixtureSpan struct {
	TraceID              string `json:"traceId"`
	SpanID               string `json:"spanId"`
	ParentSpanID         string `json:"parentSpanId"`
	Name, Kind           string
	Start, End           string
	Attributes, Resource map[string]any
	Scope                struct {
		Name, Version string
		Attributes    map[string]any
	}
	Status struct{ Code, Message string }
	Links  []struct {
		TraceID    string `json:"traceId"`
		SpanID     string `json:"spanId"`
		Attributes map[string]any
	}
	Events []struct {
		Name       string
		Time       string `json:"timeUnixNano"`
		Attributes map[string]any
	}
}

// UnmarshalJSON preserves the reference's nanosecond timestamp field names.
func (f *fixtureSpan) UnmarshalJSON(b []byte) error {
	type plain fixtureSpan
	var v struct {
		plain
		Start string `json:"startTimeUnixNano"`
		End   string `json:"endTimeUnixNano"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*f = fixtureSpan(v.plain)
	f.Start, f.End = v.Start, v.End
	return nil
}

type fixedIDs struct {
	tid trace.TraceID
	sid trace.SpanID
}

func (i fixedIDs) NewIDs(context.Context) (trace.TraceID, trace.SpanID)  { return i.tid, i.sid }
func (i fixedIDs) NewSpanID(context.Context, trace.TraceID) trace.SpanID { return i.sid }

func nanos(s string) time.Time { n, _ := strconv.ParseInt(s, 10, 64); return time.Unix(0, n) }

func fixtureAttributes(m map[string]any) []attribute.KeyValue {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	attrs := []attribute.KeyValue{}
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			attrs = append(attrs, attribute.String(k, v))
		case bool:
			attrs = append(attrs, attribute.Bool(k, v))
		case float64:
			if math.Trunc(v) == v {
				attrs = append(attrs, attribute.Int64(k, int64(v)))
			} else {
				attrs = append(attrs, attribute.Float64(k, v))
			}
		case []any:
			if len(v) == 0 {
				attrs = append(attrs, attribute.StringSlice(k, nil))
				continue
			}
			switch v[0].(type) {
			case string:
				values := []string{}
				for _, x := range v {
					values = append(values, x.(string))
				}
				attrs = append(attrs, attribute.StringSlice(k, values))
			case bool:
				values := []bool{}
				for _, x := range v {
					values = append(values, x.(bool))
				}
				attrs = append(attrs, attribute.BoolSlice(k, values))
			case float64:
				ints, floats := []int64{}, []float64{}
				integral := true
				for _, x := range v {
					f := x.(float64)
					floats = append(floats, f)
					ints = append(ints, int64(f))
					integral = integral && math.Trunc(f) == f
				}
				if integral {
					attrs = append(attrs, attribute.Int64Slice(k, ints))
				} else {
					attrs = append(attrs, attribute.Float64Slice(k, floats))
				}
			}
		}
	}
	return attrs
}

func startFixture(ctx context.Context, t *testing.T, f fixtureSpan, processors ...sdktrace.SpanProcessor) (trace.Span, *sdktrace.TracerProvider) {
	t.Helper()
	tid, err := trace.TraceIDFromHex(f.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := trace.SpanIDFromHex(f.SpanID)
	if err != nil {
		t.Fatal(err)
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithIDGenerator(fixedIDs{tid, sid}), sdktrace.WithResource(resource.NewSchemaless(fixtureAttributes(f.Resource)...)), sdktrace.WithRawSpanLimits(sdktrace.SpanLimits{AttributeValueLengthLimit: -1, AttributeCountLimit: 10000, EventCountLimit: 10000, LinkCountLimit: 10000, AttributePerEventCountLimit: 10000, AttributePerLinkCountLimit: 10000})}
	for _, p := range processors {
		opts = append(opts, sdktrace.WithSpanProcessor(p))
	}
	provider := sdktrace.NewTracerProvider(opts...)
	if f.ParentSpanID != "" {
		parent, err := trace.SpanIDFromHex(f.ParentSpanID)
		if err != nil {
			t.Fatal(err)
		}
		ctx = trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: parent, TraceFlags: trace.FlagsSampled}))
	}
	kind := map[string]trace.SpanKind{"internal": trace.SpanKindInternal, "server": trace.SpanKindServer, "client": trace.SpanKindClient, "producer": trace.SpanKindProducer, "consumer": trace.SpanKindConsumer}[f.Kind]
	links := make([]trace.Link, 0, len(f.Links))
	for _, l := range f.Links {
		linkTraceID, err := trace.TraceIDFromHex(l.TraceID)
		if err != nil {
			t.Fatal(err)
		}
		linkSpanID, err := trace.SpanIDFromHex(l.SpanID)
		if err != nil {
			t.Fatal(err)
		}
		links = append(links, trace.Link{
			SpanContext: trace.NewSpanContext(trace.SpanContextConfig{TraceID: linkTraceID, SpanID: linkSpanID}),
			Attributes:  fixtureAttributes(l.Attributes),
		})
	}
	_, s := provider.Tracer(f.Scope.Name, trace.WithInstrumentationVersion(f.Scope.Version), trace.WithInstrumentationAttributes(fixtureAttributes(f.Scope.Attributes)...)).Start(ctx, f.Name, trace.WithTimestamp(nanos(f.Start)), trace.WithSpanKind(kind), trace.WithAttributes(fixtureAttributes(f.Attributes)...), trace.WithLinks(links...))
	status := map[string]codes.Code{"ok": codes.Ok, "error": codes.Error}[f.Status.Code]
	s.SetStatus(status, f.Status.Message)
	for _, e := range f.Events {
		s.AddEvent(e.Name, trace.WithTimestamp(nanos(e.Time)), trace.WithAttributes(fixtureAttributes(e.Attributes)...))
	}
	return s, provider
}

func recorder(t *testing.T) (*ruddertest.Recorder, func() []object) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	r, err := ruddertest.NewRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r, func() []object {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		}()
		out := []object{}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 4096), 1000000)
		for scanner.Scan() {
			var v object
			if err := json.Unmarshal(scanner.Bytes(), &v); err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
}

func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, y := range x {
			x[k] = normalize(y)
		}
		return x
	case []any:
		for i, y := range x {
			x[i] = normalize(y)
		}
		if len(x) > 0 {
			if kv, ok := x[0].(map[string]any); ok && kv["key"] != nil {
				sort.Slice(x, func(i, j int) bool { return x[i].(object)["key"].(string) < x[j].(object)["key"].(string) })
			}
		}
		return x
	default:
		return v
	}
}

func loadCases(t *testing.T, path string, out any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
}

func TestTracingConformance(t *testing.T) {
	var fixtures struct {
		Cases []struct {
			Name, Compare string
			Options       struct {
				CaptureContent  *bool
				ExportAllSpans  bool
				EventName       string
				MaxEventBytes   int
				ExtraProperties map[string]any
			}
			AIContext struct {
				UserID         string `json:"userId"`
				AnonymousID    string `json:"anonymousId"`
				ConversationID string `json:"conversationId"`
			}
			Interaction *struct {
				InteractionID  string `json:"interactionId"`
				ConversationID string `json:"conversationId"`
				TraceID        string `json:"traceId"`
			}
			LinkTraceID                bool
			ExpectedInteractionTraceID *string
			ExpectedExportedByDefault  *bool
			Span                       fixtureSpan
			Spans                      []fixtureSpan
			Client                     struct {
				Throw string `json:"throw_on_span_id"`
			}
			Expected json.RawMessage
		}
	}
	loadCases(t, "../spec/fixtures/tracing-conformance-v0.1.json", &fixtures)
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			hook.Reset()
			t.Cleanup(hook.Reset)
			r, events := recorder(t)
			opts := Options{Analytics: r, CaptureContent: c.Options.CaptureContent, ExportAllSpans: c.Options.ExportAllSpans, EventName: c.Options.EventName, MaxEventBytes: c.Options.MaxEventBytes, ExtraProperties: c.Options.ExtraProperties, Simple: true, LinkTraceID: c.LinkTraceID}
			p, err := NewSpanProcessor(opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := p.Shutdown(context.Background()); err != nil {
					t.Error(err)
				}
			})
			ctx := rudderstackai.WithValues(context.Background(), rudderstackai.ContextValues{UserID: c.AIContext.UserID, AnonymousID: c.AIContext.AnonymousID, ConversationID: c.AIContext.ConversationID})
			var interaction *rudderstackai.Interaction
			if c.Interaction != nil {
				ctx, interaction = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{InteractionID: c.Interaction.InteractionID, ConversationID: c.Interaction.ConversationID, TraceID: c.Interaction.TraceID, Classify: rudderstackai.Ptr(false)})
				t.Cleanup(func() { interaction.End(rudderstackai.EndOptions{}) })
			}
			if len(c.Spans) > 0 {
				// Real SDK snapshots exported together exercise partial transport failure.
				capture := tracetest.NewSpanRecorder()
				spans := []sdktrace.ReadOnlySpan{}
				for _, f := range c.Spans {
					s, provider := startFixture(ctx, t, f, capture)
					s.End(trace.WithTimestamp(nanos(f.End)))
					spans = append(spans, capture.Ended()[len(capture.Ended())-1])
					if err := provider.Shutdown(ctx); err != nil {
						t.Fatal(err)
					}
				}
				failing := &failingRecorder{Recorder: r, span: c.Client.Throw}
				opts.Analytics = failing
				e, err := NewExporter(opts)
				if err != nil {
					t.Fatal(err)
				}
				if err := e.ExportSpans(ctx, spans); err == nil {
					t.Fatal("expected export failure")
				}
				var expected []object
				if err := json.Unmarshal(c.Expected, &expected); err != nil {
					t.Fatal(err)
				}
				actual := events()
				if len(actual) != len(expected) {
					t.Fatalf("got %d messages want %d", len(actual), len(expected))
				}
				for i := range expected {
					delete(actual[i], "context")
					delete(actual[i], "type")
					delete(actual[i], "sentAt")
					actual[i]["timestamp"] = actual[i]["originalTimestamp"]
					delete(actual[i], "originalTimestamp")
					actual[i]["timestamp"] = iso(nanos(c.Spans[i].End))
					if !reflect.DeepEqual(normalize(actual[i]), normalize(expected[i])) {
						t.Fatalf("batch message mismatch: %v", actual[i])
					}
				}
				return
			}
			capture := tracetest.NewSpanRecorder()
			s, provider := startFixture(ctx, t, c.Span, p, capture)
			s.End(trace.WithTimestamp(nanos(c.Span.End)))
			ended := capture.Ended()[0]
			if c.ExpectedExportedByDefault != nil && IsAISpan(ended) != *c.ExpectedExportedByDefault {
				t.Fatal("wrong default filter result")
			}
			if interaction != nil {
				snapshot, _ := interaction.Pause(rudderstackai.PauseOptions{Reason: "approval"})
				want := ""
				if c.ExpectedInteractionTraceID != nil {
					want = *c.ExpectedInteractionTraceID
				}
				if snapshot.TraceID != want {
					t.Fatalf("trace_id=%s want %s", snapshot.TraceID, want)
				}
			}
			// Map the tagged snapshot used by this processor's exporter.
			actualEvents := events()
			var actual object
			for _, event := range actualEvents {
				if event["event"] == DefaultEventName || event["event"] == opts.EventName {
					actual = event
					break
				}
			}
			if actual == nil {
				t.Fatal("no exported span")
			}
			delete(actual, "context")
			delete(actual, "type")
			delete(actual, "sentAt")
			delete(actual, "originalTimestamp")
			actual["timestamp"] = iso(nanos(c.Span.End))
			var expected object
			if err := json.Unmarshal(c.Expected, &expected); err != nil {
				t.Fatal(err)
			}
			if c.Compare == "truncation" {
				if actual["properties"].(object)["truncated"] != true {
					t.Fatal("not truncated")
				}
				b, _ := json.Marshal(actual)
				if len(b) > opts.MaxEventBytes {
					t.Fatalf("event %d > budget", len(b))
				}
			} else if !reflect.DeepEqual(normalize(actual), normalize(expected)) {
				got, _ := json.MarshalIndent(actual, "", "  ")
				want, _ := json.MarshalIndent(expected, "", "  ")
				t.Fatalf("got %s\nwant %s", got, want)
			}
			if err := provider.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type failingRecorder struct {
	*ruddertest.Recorder
	span string
}

func (r *failingRecorder) Enqueue(m analytics.Message) error {
	if track, ok := m.(analytics.Track); ok && track.Properties["span_id"] == r.span {
		return errors.New("enqueue failed")
	}
	return r.Recorder.Enqueue(m)
}

// matchSubset checks every fixture field while allowing existing Go product-event extensions.
func matchSubset(t *testing.T, actual, expected any, path string) {
	t.Helper()
	switch want := expected.(type) {
	case map[string]any:
		got, ok := actual.(map[string]any)
		if !ok {
			t.Fatalf("%s: got %T want object", path, actual)
		}
		for k, v := range want {
			matchSubset(t, got[k], v, path+"."+k)
		}
	case []any:
		got, ok := actual.([]any)
		if !ok || len(got) != len(want) {
			t.Fatalf("%s: got %v want %v", path, actual, expected)
		}
		for i, v := range want {
			matchSubset(t, got[i], v, fmt.Sprintf("%s[%d]", path, i))
		}
	case string:
		if want == "<number>" {
			if _, ok := actual.(float64); !ok {
				t.Fatalf("%s: got %v want number", path, actual)
			}
			return
		}
		if actual != expected {
			t.Fatalf("%s: got %v want %v", path, actual, expected)
		}
	default:
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("%s: got %v want %v", path, actual, expected)
		}
	}
}
