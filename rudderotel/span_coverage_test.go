package rudderotel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/ruddertest"
)

// spanNames lists the exported ai_otel_span names.
func spanNames(events []object) []string {
	out := []string{}
	for _, e := range spanEvents(events) {
		out = append(out, e["properties"].(object)["span_name"].(string))
	}
	return out
}

func modelSpan(ctx context.Context, tracer trace.Tracer, name string) trace.Span {
	_, s := tracer.Start(ctx, name, trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	return s
}

func countName(names []string, want string) int {
	n := 0
	for _, name := range names {
		if name == want {
			n++
		}
	}
	return n
}

// endOnly is a processor that never classifies a span at start but still ends it: one whose
// recognition differs from another RudderStack processor's.
type endOnly struct{ sdktrace.SpanProcessor }

func (endOnly) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

// Coverage is shared: when any RudderStack processor covers a span, every one drops it.
func TestSpanCoverageIsSharedAcrossProcessors(t *testing.T) {
	for _, reversed := range []bool{false, true} {
		t.Run("reversed="+strconvBool(reversed), func(t *testing.T) {
			hook.Reset()
			t.Cleanup(hook.Reset)
			r, events := recorder(t)
			if err := rudderstackai.Configure(rudderstackai.WithAnalytics(r)); err != nil {
				t.Fatal(err)
			}
			full, err := NewSpanProcessor(Options{Analytics: r, Simple: true})
			if err != nil {
				t.Fatal(err)
			}
			blind, err := NewSpanProcessor(Options{Analytics: r, Simple: true})
			if err != nil {
				t.Fatal(err)
			}
			processors := []sdktrace.SpanProcessor{full, endOnly{blind}}
			if reversed {
				processors = []sdktrace.SpanProcessor{endOnly{blind}, full}
			}
			opts := []sdktrace.TracerProviderOption{}
			for _, p := range processors {
				opts = append(opts, sdktrace.WithSpanProcessor(p))
			}
			provider := sdktrace.NewTracerProvider(opts...)
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			configureSpans(t)
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			started := time.Now()
			wrapped, s := hook.Tracing.BeginWrapper(ctx, i, started)
			modelSpan(wrapped, provider.Tracer("app"), "provider").End()
			hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m", StartedAt: started, EndedAt: time.Now()}, s, 200)
			i.End(rudderstackai.EndOptions{})
			if names := spanNames(events()); countName(names, "provider") != 0 || countName(names, "chat m") != 1 {
				t.Errorf("spans %v, want the SDK span alone", names)
			}
			if n := coveredCalls.n.Load(); n > coveredRecent {
				t.Errorf("%d covered entries retained", n)
			}
		})
	}
}

// A manual span between the wrapper call and the provider's OTel model span does not hide the
// call: the OTel span is still dropped, whatever the middleware's operation or depth.
func TestSpanCoverageManualMiddlewareDoesNotHideTheCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		ops  []string
	}{
		{"tool", []string{"execute_tool"}},
		{"retrieval", []string{"retrieval"}},
		{"agent", []string{"invoke_agent"}},
		{"nested", []string{"invoke_agent", "execute_tool"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, _, events := testProvider(t, Options{Simple: true})
			configureSpans(t)
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			started := time.Now()
			wrapped, s := hook.Tracing.BeginWrapper(ctx, i, started)
			inner := wrapped
			var manual []*rudderstackai.Span
			for _, op := range tc.ops {
				var m *rudderstackai.Span
				inner, m = rudderstackai.StartSpan(inner, rudderstackai.SpanOptions{Name: "mw-" + op, Operation: op})
				manual = append(manual, m)
			}
			modelSpan(inner, provider.Tracer("app"), "provider").End()
			for j := len(manual) - 1; j >= 0; j-- {
				manual[j].End(nil)
			}
			hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m", InputTokens: 1, OutputTokens: 1, StartedAt: started, EndedAt: time.Now()}, s, 200)
			i.End(rudderstackai.EndOptions{})
			names := spanNames(events())
			if countName(names, "provider") != 0 || countName(names, "chat m") != 1 || len(names) != 1+len(tc.ops) {
				t.Errorf("spans %v, want the wrapper's chat span and the manual spans only", names)
			}
		})
	}
}

// An SDK span that cannot be sent neither covers an inner OTel span nor replaces an outer one:
// the processor keeps it, as on main.
func TestSpanCoverageNeedsASendableSDKSpan(t *testing.T) {
	big := map[string]any{"traits": map[string]any{"blob": strings.Repeat("a", 40_000)}}
	for _, tc := range []struct {
		name    string
		options []rudderstackai.ConfigOption
		context map[string]any
	}{
		{"no SDK client", []rudderstackai.ConfigOption{rudderstackai.WithAnalytics(nil)}, nil},
		{"oversized message context", nil, big},
	} {
		for _, outer := range []bool{false, true} {
			t.Run(tc.name+"/outer="+strconvBool(outer), func(t *testing.T) {
				provider, _, events := testProvider(t, Options{Simple: true})
				configureSpans(t, tc.options...)
				ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", Context: tc.context})
				started := time.Now()
				if outer {
					octx, o := provider.Tracer("app").Start(ctx, "provider", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
					_, s := hook.Tracing.BeginWrapper(octx, i, started)
					hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m", StartedAt: started, EndedAt: time.Now()}, s, 200)
					o.End()
				} else {
					wrapped, s := hook.Tracing.BeginWrapper(ctx, i, started)
					modelSpan(wrapped, provider.Tracer("app"), "provider").End()
					hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m", StartedAt: started, EndedAt: time.Now()}, s, 200)
				}
				i.End(rudderstackai.EndOptions{})
				if names := spanNames(events()); len(names) != 1 || names[0] != "provider" {
					t.Errorf("spans %v, want the OTel span exported once", names)
				}
			})
		}
	}
}

type failingClient struct {
	*ruddertest.Recorder
	panics bool
	calls  int
}

func (c *failingClient) Enqueue(analytics.Message) error {
	c.calls++
	if c.panics {
		panic("delivery failed")
	}
	return errors.New("delivery failed")
}

// Accepted limit: when the processor replaces an OTel model span, the SDK span is the one kept;
// if the SDK's own client fails to deliver, the call has no span.
func TestSpanCoverageSDKDeliveryFailureLosesTheSpan(t *testing.T) {
	for _, panics := range []bool{false, true} {
		for _, outer := range []bool{false, true} {
			t.Run("panics="+strconvBool(panics)+"/outer="+strconvBool(outer), func(t *testing.T) {
				provider, _, events := testProvider(t, Options{Simple: true})
				r, _ := recorder(t)
				sdk := &failingClient{Recorder: r, panics: panics}
				configureSpans(t, rudderstackai.WithAnalytics(sdk), rudderstackai.WithOnError(func(error, rudderstackai.ErrorInfo) {}))
				ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
				started := time.Now()
				if outer {
					octx, o := provider.Tracer("app").Start(ctx, "provider", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
					_, s := hook.Tracing.BeginWrapper(octx, i, started)
					hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m", StartedAt: started, EndedAt: time.Now()}, s, 200)
					o.End()
				} else {
					wrapped, s := hook.Tracing.BeginWrapper(ctx, i, started)
					modelSpan(wrapped, provider.Tracer("app"), "provider").End()
					hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m", StartedAt: started, EndedAt: time.Now()}, s, 200)
				}
				i.End(rudderstackai.EndOptions{})
				if sdk.calls == 0 {
					t.Fatal("the SDK client was never asked to deliver")
				}
				if names := spanNames(events()); len(names) != 0 {
					t.Errorf("spans %v, want none for the call", names)
				}
			})
		}
	}
}
