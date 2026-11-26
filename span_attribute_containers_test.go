package rudderstackai

import (
	"context"
	"strings"
	"testing"
)

// Span attributes are a container: an app attribute set shaped like a Node Buffer never drops
// every attribute of the span, the SDK's own included.
func TestRound2SpanAttributesBufferShaped(t *testing.T) {
	r := setupAI(t, WithEmitSpans(true), WithCaptureContent(true))
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	_, s := StartSpan(ctx, SpanOptions{Name: "lookup", Operation: "execute_tool", Attributes: map[string]any{
		"type": "Buffer", "data": []int{1, 2}, "custom.attr": "keep",
	}})
	s.End(nil)
	i.End(EndOptions{})
	spans := r.named("ai_otel_span")
	if len(spans) == 0 {
		t.Fatal("span lost")
	}
	otlp := mustJSON(spans[0].Properties["otlp"])
	for _, key := range []string{`"custom.attr"`, `"type"`, `"data"`, `"gen_ai.operation.name"`} {
		if !strings.Contains(otlp, key) {
			t.Fatalf("attribute %s lost: %s", key, otlp)
		}
	}
}
