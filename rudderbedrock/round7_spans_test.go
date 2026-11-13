package rudderbedrock

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	smithyhttp "github.com/aws/smithy-go/transport/http"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
)

func TestBedrockTerminalWinsOverCancellation(t *testing.T) {
	rec := setup(t)
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "alice"})
	defer i.End(rudderstackai.EndOptions{})
	ctx, span := hook.Tracing.BeginWrapper(ctx, i, time.Now())
	sc := &streamCall{ctx: ctx, call: &kit.Call{Span: span, Interaction: i, Provider: provider, Integration: integration, Model: model, StartedAt: time.Now()}}
	sc.start(&smithyhttp.Response{Response: &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}})
	sc.mu.Lock()
	in, out := 3, 2
	sc.obs.Done, sc.obs.Incomplete = true, false
	sc.obs.InputTokens, sc.obs.OutputTokens = &in, &out
	sc.mu.Unlock()
	sc.end(kit.Errored, "cancelled")
	spans := rec.named(t, "ai_otel_span")
	if len(spans) != 1 || spans[0]["status_code"] != "ok" {
		t.Fatalf("terminal cancelled: %v", spans)
	}
	calls := rec.named(t, "ai_llm_response_received")
	if len(calls) != 1 || calls[0]["error_type"] != nil || calls[0]["input_tokens"] != float64(3) {
		t.Fatalf("usage lost: %v", calls)
	}
}
