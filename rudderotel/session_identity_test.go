package rudderotel

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestInteractionSessionTagged(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(strconvBool(late), func(t *testing.T) {
			provider, _, events := testProvider(t, Options{Simple: true})
			ctx := rudderstackai.WithValues(context.Background(), rudderstackai.ContextValues{CookieValues: map[string]string{"rl_session": `{"id":12345}`}})
			ctx, i := rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "u"})
			defer i.End(rudderstackai.EndOptions{})
			attrs := []attribute.KeyValue{}
			if !late {
				attrs = append(attrs, attribute.String("gen_ai.operation.name", "chat"))
			}
			_, span := provider.Tracer("test").Start(ctx, "chat", trace.WithAttributes(attrs...))
			span.SetAttributes(attribute.String("gen_ai.operation.name", "chat"))
			span.End()
			got := spanEvents(events())
			if len(got) != 1 || got[0]["properties"].(object)["session_id"] != "12345" {
				t.Fatalf("missing interaction session: %v", got)
			}
			rs := got[0]["properties"].(object)["otlp"].(object)["resourceSpans"].([]any)[0].(object)
			ss := rs["scopeSpans"].([]any)[0].(object)
			spanJSON := ss["spans"].([]any)[0].(object)
			found := false
			for _, kv := range spanJSON["attributes"].([]any) {
				attr := kv.(object)
				if attr["key"] == "session.id" && attr["value"].(object)["stringValue"] == "12345" {
					found = true
				}
			}
			if !found {
				t.Fatal("missing session.id attribute")
			}
		})
	}
}
