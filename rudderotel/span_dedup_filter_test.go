package rudderotel

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// Filter, ResolveIdentity and ExtraPropertiesFrom get the app's span, with its own trace, span
// and parent IDs, for every descendant of a covered model span; the IDs RudderStack exports
// under are resolved after them. A filter that accepts a tool only under the app's model span
// keeps accepting it.
func TestSpanDedupCallbacksSeeTheAppSpan(t *testing.T) {
	for _, simple := range []bool{true, false} {
		t.Run("simple="+strconvBool(simple), func(t *testing.T) {
			var mu sync.Mutex
			var model, tool, retrieval trace.SpanContext
			seen := map[string]map[string][2]trace.SpanContext{"filter": {}, "identity": {}, "extra": {}}
			record := func(callback string, s sdktrace.ReadOnlySpan) {
				seen[callback][s.Name()] = [2]trace.SpanContext{s.SpanContext(), s.Parent()}
			}
			filter := func(s sdktrace.ReadOnlySpan) bool {
				mu.Lock()
				defer mu.Unlock()
				record("filter", s)
				switch s.Name() {
				case "tool":
					return s.SpanContext().TraceID() == model.TraceID() && s.Parent().SpanID() == model.SpanID()
				case "retrieval":
					return s.SpanContext().TraceID() == model.TraceID() && s.Parent().SpanID() == tool.SpanID()
				case "agent":
					return s.SpanContext().TraceID() == model.TraceID() && s.Parent().SpanID() == retrieval.SpanID()
				}
				return IsAISpan(s)
			}
			apm := tracetest.NewSpanRecorder()
			options := Options{Simple: simple, Filter: filter,
				ResolveIdentity: func(s sdktrace.ReadOnlySpan) Identity {
					mu.Lock()
					defer mu.Unlock()
					record("identity", s)
					return Identity{}
				},
				ExtraPropertiesFrom: func(s sdktrace.ReadOnlySpan) map[string]any {
					mu.Lock()
					defer mu.Unlock()
					record("extra", s)
					return nil
				},
			}
			provider, _, events := testProvider(t, options, apm)
			configureSpans(t)
			tracer := provider.Tracer("app")
			ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			started := time.Now()
			wctx, s := hook.Tracing.BeginWrapper(ctx, i, started)
			mctx, m := tracer.Start(wctx, "model", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
			tctx, tl := tracer.Start(mctx, "tool", trace.WithAttributes(attribute.String("gen_ai.operation.name", "execute_tool")))
			rctx, r := tracer.Start(tctx, "retrieval", trace.WithAttributes(attribute.String("gen_ai.operation.name", "retrieval")))
			_, a := tracer.Start(rctx, "agent", trace.WithAttributes(attribute.String("gen_ai.operation.name", "invoke_agent")))
			mu.Lock()
			model, tool, retrieval = m.SpanContext(), tl.SpanContext(), r.SpanContext()
			mu.Unlock()
			a.End()
			r.End()
			tl.End()
			m.End()
			hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "m", StartedAt: started, EndedAt: time.Now()}, s, 200)
			i.End(rudderstackai.EndOptions{})
			if err := provider.ForceFlush(context.Background()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, callback := range []string{"filter", "identity", "extra"} {
				for _, o := range apm.Ended() {
					got, ok := seen[callback][o.Name()]
					if o.Name() == "model" {
						if ok {
							t.Errorf("%s was called for the covered model span", callback)
						}
						continue
					}
					if !ok || !got[0].Equal(o.SpanContext()) || !got[1].Equal(o.Parent()) {
						t.Errorf("%s got %s as %s/%s under %s, want the app's %s/%s under %s", callback, o.Name(),
							got[0].TraceID(), got[0].SpanID(), got[1].SpanID(), o.SpanContext().TraceID(), o.SpanContext().SpanID(), o.Parent().SpanID())
					}
				}
			}
			spans := spanEvents(events())
			requireIntactTrees(t, spans)
			names := spanNames(spans)
			for _, name := range []string{"chat m", "tool", "retrieval", "agent"} {
				if countName(names, name) != 1 {
					t.Errorf("%s exported %d times: %v", name, countName(names, name), names)
				}
			}
		})
	}
}
