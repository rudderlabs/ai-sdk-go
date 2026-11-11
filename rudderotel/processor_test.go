package rudderotel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/ruddertest"
)

func testProvider(t *testing.T, options Options, more ...sdktrace.SpanProcessor) (*sdktrace.TracerProvider, *SpanProcessor, func() []object) {
	t.Helper()
	hook.Reset()
	t.Cleanup(hook.Reset)
	r, events := recorder(t)
	if options.Analytics == nil {
		options.Analytics = r
	}
	if err := rudderstackai.Configure(rudderstackai.WithAnalytics(r)); err != nil {
		t.Fatal(err)
	}
	p, err := NewSpanProcessor(options)
	if err != nil {
		t.Fatal(err)
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithSpanProcessor(p), sdktrace.WithResource(resource.Empty())}
	for _, processor := range more {
		opts = append(opts, sdktrace.WithSpanProcessor(processor))
	}
	provider := sdktrace.NewTracerProvider(opts...)
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return provider, p, events
}

func spanEvents(events []object) []object {
	out := []object{}
	for _, e := range events {
		if e["event"] == DefaultEventName {
			out = append(out, e)
		}
	}
	return out
}

func TestIdentityAndLateAttributes(t *testing.T) {
	for _, c := range []struct {
		name                      string
		propagation               *bool
		late, endedParent, system bool
		explicit                  string
		wantUser                  string
	}{
		{name: "context through non-AI parent", wantUser: "context-user"},
		{name: "instrumentation wins", explicit: "instrumentation-user", wantUser: "instrumentation-user"},
		{name: "late AI attributes", late: true, wantUser: "context-user"},
		{name: "ended parent", endedParent: true, wantUser: "context-user"},
		{name: "propagation disabled", propagation: rudderstackai.Ptr(false)},
		{name: "system run", system: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			capture := tracetest.NewSpanRecorder()
			provider, _, events := testProvider(t, Options{Simple: true, PropagateIdentity: c.propagation}, capture)
			tracer := provider.Tracer("test")
			ctx := rudderstackai.WithValues(context.Background(), rudderstackai.ContextValues{UserID: "context-user", AnonymousID: "anon", ConversationID: "conv"})
			parentCtx, parent := tracer.Start(ctx, "HTTP request")
			if c.endedParent {
				parent.End()
			}
			// Keep the parent span, dropping AI context values, to test held parent identity.
			childCtx := trace.ContextWithSpan(context.Background(), trace.SpanFromContext(parentCtx))
			run := func(ctx context.Context) {
				attrs := []attribute.KeyValue{}
				if !c.late {
					attrs = append(attrs, attribute.String("gen_ai.operation.name", "chat"))
				}
				if c.explicit != "" {
					attrs = append(attrs, attribute.String("user.id", c.explicit))
				}
				_, s := tracer.Start(ctx, "chat", trace.WithAttributes(attrs...))
				if c.late {
					s.SetAttributes(attribute.String("gen_ai.operation.name", "chat"))
				}
				s.End()
			}
			if c.system {
				if err := rudderstackai.RunTask(childCtx, "job", rudderstackai.TaskOptions{}, func(ctx context.Context, _ *rudderstackai.Interaction) error { run(ctx); return nil }); err != nil {
					t.Fatal(err)
				}
			} else {
				run(childCtx)
			}
			parent.End()
			for _, s := range capture.Ended() {
				if s.Name() == "HTTP request" && len(s.Attributes()) != 0 {
					t.Fatalf("identity leaked onto non-AI parent: %v", s.Attributes())
				}
			}
			got := spanEvents(events())
			if len(got) != 1 {
				t.Fatalf("spans=%v", got)
			}
			if user, _ := got[0]["userId"].(string); user != c.wantUser {
				t.Fatalf("user=%q want %q", user, c.wantUser)
			}
			if c.late {
				for _, s := range capture.Ended() {
					if s.Name() == "chat" && firstString(attributes(s.Attributes()), "user.id") != "" {
						t.Fatal("late tagging mutated another exporter snapshot")
					}
				}
			}
		})
	}
}

func TestLinkTraceID(t *testing.T) {
	for _, c := range []struct {
		name     string
		link     bool
		explicit string
	}{{name: "off"}, {name: "on", link: true}, {name: "explicit wins", link: true, explicit: "app-trace"}} {
		t.Run(c.name, func(t *testing.T) {
			provider, _, events := testProvider(t, Options{Simple: true, LinkTraceID: c.link})
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "user", TraceID: c.explicit})
			_, s := provider.Tracer("test").Start(ctx, "HTTP request")
			s.End()
			i.End(rudderstackai.EndOptions{})
			want := c.explicit
			if c.link && want == "" {
				want = s.SpanContext().TraceID().String()
			}
			for _, e := range events() {
				if e["event"] == "ai_response_received" {
					got, _ := e["properties"].(object)["trace_id"].(string)
					if got != want {
						t.Fatalf("trace=%q want %q", got, want)
					}
				}
			}
		})
	}
}

func TestProductEventsOptionsAndLateOwner(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(string(rune('0'+boolInt(on))), func(t *testing.T) {
			provider, _, events := testProvider(t, Options{Simple: true, ProductEvents: on})
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			_, s := provider.Tracer("test").Start(ctx, "late AI kind")
			s.SetAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.Int("gen_ai.usage.input_tokens", 10))
			// A paused run remains the owner of its in-flight span (Python parity).
			if _, ok := i.Pause(rudderstackai.PauseOptions{Reason: "approval"}); !ok {
				t.Fatal("pause failed")
			}
			s.End()
			i.End(rudderstackai.EndOptions{})
			lastCount := float64(0)
			for _, e := range events() {
				if e["event"] == "ai_response_received" {
					lastCount = e["properties"].(object)["llm_call_count"].(float64)
				}
			}
			if lastCount != float64(boolInt(on)) {
				t.Fatalf("call count=%v", lastCount)
			}
		})
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TestToolSpanSettlesWrapperWithoutID(t *testing.T) {
	provider, _, events := testProvider(t, Options{Simple: true, ProductEvents: true})
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", ToolBreakdown: rudderstackai.Ptr(true)})
	i.RecordToolCalls(rudderstackai.ToolCall{ID: "wrapper-id", Name: "lookup"})
	_, s := provider.Tracer("test").Start(ctx, "lookup", trace.WithAttributes(attribute.String("gen_ai.operation.name", "execute_tool"), attribute.String("gen_ai.tool.name", "lookup")))
	s.End()
	i.End(rudderstackai.EndOptions{})
	for _, e := range events() {
		if e["event"] == "ai_response_received" {
			if e["properties"].(object)["tool_call_count"] != float64(1) {
				t.Fatalf("double counted tool: %v", e)
			}
		}
	}
}

func TestClassifierSuppressionAndFlush(t *testing.T) {
	for _, simple := range []bool{false, true} {
		for _, shutdown := range []bool{false, true} {
			t.Run(strings.Join([]string{strconvBool(simple), strconvBool(shutdown)}, "/"), func(t *testing.T) {
				provider, p, events := testProvider(t, Options{Simple: simple, ProductEvents: true})
				tracer := provider.Tracer("test")
				started, release := make(chan struct{}), make(chan struct{})
				if err := rudderstackai.Configure(rudderstackai.WithClassifier(&rudderstackai.ClassifierOptions{Intents: []string{"question"}, Classify: func(ctx context.Context, _ rudderstackai.ClassifyTurn) (rudderstackai.ClassifyResult, error) {
					_, s := tracer.Start(ctx, "classifier call", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String(InteractionIDAttribute, "owner")))
					close(started)
					<-release
					s.End()
					return rudderstackai.ClassifyResult{Intent: "question", Outcome: "resolved"}, nil
				}})); err != nil {
					t.Fatal(err)
				}
				ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", InteractionID: "owner", PromptText: "hello"})
				_, s := tracer.Start(ctx, "app call", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.Int("gen_ai.usage.input_tokens", 1)))
				s.End()
				i.End(rudderstackai.EndOptions{ResponseText: "hello"})
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("classifier did not start")
				}
				finished := make(chan error, 1)
				go func() {
					if shutdown {
						finished <- p.Shutdown(context.Background())
					} else {
						finished <- p.ForceFlush(context.Background())
					}
				}()
				select {
				case err := <-finished:
					t.Fatalf("flush returned before classification: %v", err)
				case <-time.After(20 * time.Millisecond):
				}
				close(release)
				select {
				case err := <-finished:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("flush blocked")
				}
				spans := spanEvents(events())
				if len(spans) != 1 || spans[0]["properties"].(object)["span_name"] != "app call" {
					t.Fatalf("classifier exported: %v", spans)
				}
				classified := false
				for _, e := range events() {
					classified = classified || e["event"] == "ai_interaction_classified"
				}
				if !classified {
					t.Fatal("classification not flushed")
				}
			})
		}
	}
}

func strconvBool(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func TestRegisterTracing(t *testing.T) {
	if mode := os.Getenv("RUDDEROTEL_SETUP_TEST"); mode != "" {
		hook.Reset()
		r, events := recorder(t)
		previous := otel.GetTracerProvider()
		switch mode {
		case "noop":
			otel.SetTracerProvider(noop.NewTracerProvider())
		case "sdk":
			otel.SetTracerProvider(sdktrace.NewTracerProvider())
		case "default":
			_ = otel.Tracer("before-registration")
		}
		current := otel.GetTracerProvider()
		provider, err := RegisterTracing(Options{Analytics: r, Simple: true})
		if mode != "default" {
			if err == nil || otel.GetTracerProvider() != current {
				t.Fatal("replaced an existing provider")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if provider == nil || otel.GetTracerProvider() == previous {
			t.Fatal("provider not registered")
		}
		_, s := otel.Tracer("test").Start(context.Background(), "chat", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
		s.End()
		if _, err := RegisterTracing(Options{Analytics: r}); err == nil {
			t.Fatal("second registration allowed")
		}
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(spanEvents(events())) != 1 {
			t.Fatal("global provider exported nothing")
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"default", "noop", "sdk"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(executable, "-test.run=^TestRegisterTracing$")
			cmd.Env = append(os.Environ(), "RUDDEROTEL_SETUP_TEST="+mode)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", mode, err, out)
			}
		})
	}
}

func TestOTLPCompleteAndImmutable(t *testing.T) {
	capture := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(capture), sdktrace.WithResource(resource.NewWithAttributes("resource-schema", attribute.String("service.name", "test"))), sdktrace.WithRawSpanLimits(sdktrace.SpanLimits{AttributeValueLengthLimit: -1, AttributeCountLimit: 2, EventCountLimit: 1, LinkCountLimit: 1, AttributePerEventCountLimit: 1, AttributePerLinkCountLimit: 1}))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	parentCtx, parent := provider.Tracer("test").Start(context.Background(), "parent")
	state, err := trace.ParseTraceState("vendor=value")
	if err != nil {
		t.Fatal(err)
	}
	linked := parent.SpanContext().WithTraceState(state)
	_, s := provider.Tracer("scope", trace.WithInstrumentationVersion("1.2.3"), trace.WithSchemaURL("scope-schema"), trace.WithInstrumentationAttributes(attribute.String("scope.attr", "yes"))).Start(parentCtx, "chat", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.input.messages", "secret"), attribute.Bool("dropped", true)), trace.WithLinks(trace.Link{SpanContext: linked}, trace.Link{SpanContext: linked, Attributes: []attribute.KeyValue{attribute.Bool("link.bool", true), attribute.String("link.drop", "x")}}))
	s.AddEvent("first")
	s.AddEvent("last", trace.WithAttributes(attribute.Float64("number", math.Inf(1)), attribute.Bool("event.drop", true)))
	s.SetStatus(codes.Error, "failure")
	s.End()
	parent.End()
	span := capture.Ended()[0]
	m, err := SpanToTrack(span, Options{})
	if err != nil {
		t.Fatal(err)
	}
	otlp := m.Properties["otlp"].(object)
	rs := otlp["resourceSpans"].([]object)[0]
	ss := rs["scopeSpans"].([]object)[0]
	encoded := ss["spans"].([]object)[0]
	if rs["schemaUrl"] != "resource-schema" || ss["schemaUrl"] != "scope-schema" || ss["scope"].(object)["version"] != "1.2.3" {
		t.Fatalf("scope/resource lost: %v", otlp)
	}
	for _, k := range []string{"droppedAttributesCount", "droppedEventsCount", "droppedLinksCount"} {
		if encoded[k] != 1 {
			t.Fatalf("missing %s: %v", k, encoded)
		}
	}
	if encoded["kind"] != int(trace.SpanKindClient) || encoded["status"].(object)["code"] != 2 {
		t.Fatalf("wrong OTLP enum: %v", encoded)
	}
	if encoded["parentSpanId"] != parent.SpanContext().SpanID().String() {
		t.Fatal("parent lost")
	}
	link := encoded["links"].([]object)[0]
	if link["traceState"] != "vendor=value" || link["droppedAttributesCount"] != 1 {
		t.Fatalf("link lost: %v", link)
	}
	event := encoded["events"].([]object)[0]
	if event["name"] != "last" || event["droppedAttributesCount"] != 1 {
		t.Fatalf("event lost: %v", event)
	}
	if _, err := json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if firstString(attributes(span.Attributes()), "gen_ai.input.messages") != "secret" {
		t.Fatal("source span mutated")
	}
}

type countingClient struct {
	*ruddertest.Recorder
	mu      sync.Mutex
	flushes int
	fail    bool
}

func (c *countingClient) Flush(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushes++
	if c.fail {
		return errors.New("flush failed")
	}
	return nil
}

func TestExporterErrorsAndFlush(t *testing.T) {
	hook.Reset()
	t.Cleanup(hook.Reset)
	if _, err := NewExporter(Options{}); err == nil {
		t.Fatal("accepted missing client")
	}
	r, events := recorder(t)
	client := &countingClient{Recorder: r}
	if _, err := NewExporter(Options{Analytics: client, MaxEventBytes: -1}); err == nil {
		t.Fatal("accepted negative budget")
	}
	e, err := NewExporter(Options{Analytics: client})
	if err != nil {
		t.Fatal(err)
	}
	// The exporter must still flush its own client after the core's bounded client cache evicts it.
	if err := rudderstackai.Configure(rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: 1})); err != nil {
		t.Fatal(err)
	}
	other, _ := recorder(t)
	hook.Tracing.RememberClient(other)
	if err := e.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	if client.flushes == 0 {
		t.Fatal("client not flushed")
	}
	client.fail = true
	client.mu.Unlock()
	if err := e.ForceFlush(context.Background()); err == nil {
		t.Fatal("lost client flush failure")
	}
	client.mu.Lock()
	client.fail = false
	client.mu.Unlock()
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.ExportSpans(context.Background(), nil); err == nil {
		t.Fatal("export after shutdown succeeded")
	}
	if len(events()) != 0 {
		t.Fatal("unexpected events")
	}
}

func TestMappingOptionsAndContentConfiguration(t *testing.T) {
	for _, c := range []struct {
		name     string
		capture  bool
		explicit *bool
		filter   func(sdktrace.ReadOnlySpan) bool
		all      bool
		want     bool
	}{
		{name: "config capture", capture: true, want: true},
		{name: "explicit false", capture: true, explicit: rudderstackai.Ptr(false), want: true},
		{name: "filter overrides all", all: true, filter: func(sdktrace.ReadOnlySpan) bool { return false }},
		{name: "custom filter", filter: func(sdktrace.ReadOnlySpan) bool { return true }, want: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			provider, p, events := testProvider(t, Options{Simple: true, ExportAllSpans: c.all, Filter: c.filter, CaptureContent: c.explicit,
				ResolveIdentity:     func(sdktrace.ReadOnlySpan) Identity { panic("resolver") },
				ExtraPropertiesFrom: func(sdktrace.ReadOnlySpan) map[string]any { panic("extra") },
			})
			_, s := provider.Tracer("test").Start(context.Background(), "chat", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.String("gen_ai.input.messages", "person@example.com")))
			// Configure after construction: capture follows the core at export time.
			if err := rudderstackai.Configure(rudderstackai.WithCaptureContent(c.capture)); err != nil {
				t.Fatal(err)
			}
			s.End()
			if err := p.ForceFlush(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := spanEvents(events())
			if (len(got) > 0) != c.want {
				t.Fatalf("filter mismatch: %v", got)
			}
			if !c.want {
				return
			}
			b, _ := json.Marshal(got[0])
			kept := strings.Contains(string(b), "[email]")
			wantContent := c.capture
			if c.explicit != nil {
				wantContent = *c.explicit
			}
			if kept != wantContent {
				t.Fatalf("capture mismatch: %s", b)
			}
			if strings.Contains(string(b), "person@example.com") {
				t.Fatal("unmasked content")
			}
			if got[0]["anonymousId"] != s.SpanContext().TraceID().String() {
				t.Fatal("identity fallback failed")
			}
		})
	}
}

func TestTracingSchema(t *testing.T) {
	provider, _, events := testProvider(t, Options{Simple: true})
	_, s := provider.Tracer("test").Start(context.Background(), "chat", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	s.End()
	b, err := os.ReadFile("../spec/ai-otel-span-v0.1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	url := "https://rudderstack.com/schemas/ai-otel-span-v0.1.schema.json"
	if err := c.AddResource(url, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile(url)
	if err != nil {
		t.Fatal(err)
	}
	got := spanEvents(events())
	if len(got) != 1 {
		t.Fatal("missing span")
	}
	if err := schema.Validate(got[0]); err != nil {
		t.Fatal(err)
	}
}

func TestRoleAndCountConformance(t *testing.T) {
	for _, c := range []struct {
		name  string
		attrs map[string]any
		want  string
	}{
		{name: "GenAI wins", attrs: map[string]any{"gen_ai.operation.name": "invoke_agent", "openinference.span.kind": "LLM", "llm.request.type": "chat"}},
		{name: "OpenInference wins", attrs: map[string]any{"openinference.span.kind": "CHAIN", "llm.request.type": "chat"}},
		{name: "OpenInference tool", attrs: map[string]any{"openinference.span.kind": "tool"}, want: "tool"},
		{name: "OpenLLMetry completion", attrs: map[string]any{"llm.request.type": "completion"}, want: "llm"},
		{name: "numeric strings unknown", attrs: map[string]any{"gen_ai.operation.name": "chat", "gen_ai.usage.input_tokens": "10", "gen_ai.usage.output_tokens": float64(-1)}, want: "llm"},
	} {
		t.Run(c.name, func(t *testing.T) {
			capture := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(capture))
			_, s := provider.Tracer("test").Start(context.Background(), "span", trace.WithAttributes(fixtureAttributes(c.attrs)...))
			s.End()
			span := capture.Ended()[0]
			if got := spanRole(span); got != c.want {
				t.Fatalf("role=%s want %s", got, c.want)
			}
			if c.name == "numeric strings unknown" && !spanCall(span).UsageUnknown {
				t.Fatal("invalid counts treated as known")
			}
			if err := provider.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
