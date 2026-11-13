package rudderbedrock

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go/middleware"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestBedrockMiddlewareManualSpanIsWrapperChild(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run("explicit="+fmt.Sprint(explicit), func(t *testing.T) {
			rec := setup(t)
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if explicit {
				var i *rudderstackai.Interaction
				ctx, i = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "alice"})
				defer i.End(rudderstackai.EndOptions{})
			}
			ctx, parent := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: "outer"})
			defer parent.End(nil)
			tracker := &tracker{cfg: &config{}}
			_, _, err := tracker.HandleInitialize(ctx, middleware.InitializeInput{Parameters: &bedrockruntime.ConverseInput{ModelId: aws.String("m")}}, middleware.InitializeHandlerFunc(func(ctx context.Context, _ middleware.InitializeInput) (middleware.InitializeOutput, middleware.Metadata, error) {
				_, child := rudderstackai.StartSpan(ctx, rudderstackai.SpanOptions{Name: "middleware"})
				child.End(nil)
				return middleware.InitializeOutput{Result: &bedrockruntime.ConverseOutput{StopReason: types.StopReasonEndTurn}}, middleware.Metadata{}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			spans := rec.named(t, "ai_otel_span")
			if len(spans) != 2 {
				t.Fatalf("spans=%d", len(spans))
			}
			child, wrapper := spans[0], spans[1]
			if child["parent_span_id"] != wrapper["span_id"] || child["trace_id"] != wrapper["trace_id"] || child["interaction_id"] != wrapper["interaction_id"] {
				t.Fatalf("middleware child=%v wrapper=%v", child, wrapper)
			}
			if explicit && wrapper["parent_span_id"] != parent.SpanID {
				t.Fatal("same-interaction outer parent lost")
			}
			if !explicit && (wrapper["parent_span_id"] != nil || wrapper["trace_id"] == parent.TraceID) {
				t.Fatal("implicit interaction inherited foreign outer span")
			}
		})
	}
}

func TestBedrockFailureUsageUnknown(t *testing.T) {
	rec := setup(t)
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
		t.Fatal(err)
	}
	tracker := &tracker{cfg: &config{}}
	_, _, _ = tracker.HandleInitialize(context.Background(), middleware.InitializeInput{Parameters: &bedrockruntime.ConverseInput{ModelId: aws.String("m")}}, middleware.InitializeHandlerFunc(func(context.Context, middleware.InitializeInput) (middleware.InitializeOutput, middleware.Metadata, error) {
		return middleware.InitializeOutput{}, middleware.Metadata{}, errors.New("fail")
	}))
	spans := rec.named(t, "ai_otel_span")
	if len(spans) != 1 {
		t.Fatalf("spans=%d", len(spans))
	}
	attrs := spans[0]["otlp"].(map[string]any)["resourceSpans"].([]any)[0].(map[string]any)["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)[0].(map[string]any)["attributes"].([]any)
	for _, a := range attrs {
		k := a.(map[string]any)["key"]
		if k == "gen_ai.usage.input_tokens" || k == "gen_ai.usage.output_tokens" {
			t.Errorf("invented %v", k)
		}
	}
}
func TestBedrockPanicFinishesSpanAndRepanics(t *testing.T) {
	for _, value := range []any{errors.New("sentinel"), "panic", &struct{ N int }{3}} {
		t.Run("panic", func(t *testing.T) {
			rec := setup(t)
			if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true)); err != nil {
				t.Fatal(err)
			}
			tracker := &tracker{cfg: &config{}}
			var got any
			func() {
				defer func() { got = recover() }()
				_, _, _ = tracker.HandleInitialize(context.Background(), middleware.InitializeInput{Parameters: &bedrockruntime.ConverseInput{ModelId: aws.String("m")}}, middleware.InitializeHandlerFunc(func(context.Context, middleware.InitializeInput) (middleware.InitializeOutput, middleware.Metadata, error) {
					panic(value)
				}))
			}()
			if got != value {
				t.Fatal("panic changed")
			}
			spans := rec.named(t, "ai_otel_span")
			if len(spans) != 1 {
				t.Fatalf("spans=%d", len(spans))
			}
			if spans[0]["status_code"] != "error" {
				t.Fatal("span not failed")
			}
		})
	}
}
