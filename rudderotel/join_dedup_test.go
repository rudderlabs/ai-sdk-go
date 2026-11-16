package rudderotel

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

// A join's parent is an SDK span started at join time: it is newer than an OTel owner already
// active in the context, so a wrapper call under it gets its own span instead of joining.
func TestJoinParentWinsOverAnOlderOTelOwner(t *testing.T) {
	provider, _, events := testProvider(t, Options{Simple: true})
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	_, tool := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: "tool", Operation: "execute_tool"})
	tool.End(nil)
	ctx, o := provider.Tracer("app").Start(ctx, "chat", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	joined := rudderstackai.JoinInteraction(ctx, i.ID(), rudderstackai.JoinOptions{ParentSpanID: tool.SpanID})
	_, s := hook.Tracing.BeginWrapper(joined, i, time.Now())
	hook.Tracing.RecordWrapper(i, rudderstackai.LLMCall{Provider: "openai", Model: "w", InputTokens: 1, OutputTokens: 1}, s, 200)
	o.End()
	i.End(rudderstackai.EndOptions{})
	found := false
	for _, e := range spanEvents(events()) {
		p := e["properties"].(object)
		if p["span_name"] == "chat w" {
			found = true
			if p["parent_span_id"] != tool.SpanID {
				t.Errorf("wrapper parent %v, want the join parent %s", p["parent_span_id"], tool.SpanID)
			}
		}
	}
	if !found {
		t.Error("the wrapper joined the older OTel span instead of nesting under the join parent")
	}
}
