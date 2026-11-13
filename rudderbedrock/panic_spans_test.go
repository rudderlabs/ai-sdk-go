package rudderbedrock

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/smithy-go/middleware"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

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
