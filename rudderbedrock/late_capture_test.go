package rudderbedrock

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go/middleware"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func TestLateBedrockResponseCapturesToolContent(t *testing.T) {
	rec := setup(t)
	if err := rudderstackai.Configure(rudderstackai.WithEmitSpans(true), rudderstackai.WithCaptureContent(true)); err != nil {
		t.Fatal(err)
	}
	ctx, owner := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "alice"})
	tr := &tracker{cfg: &config{}}
	result := &bedrockruntime.ConverseOutput{Output: &types.ConverseOutputMemberMessage{Value: types.Message{Content: []types.ContentBlock{&types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{Name: aws.String("late_tool"), ToolUseId: aws.String("id"), Input: document.NewLazyDocument(map[string]any{"value": "late argument"})}}}}}}
	_, _, err := tr.HandleInitialize(ctx, middleware.InitializeInput{Parameters: &bedrockruntime.ConverseInput{ModelId: aws.String(model)}}, middleware.InitializeHandlerFunc(func(context.Context, middleware.InitializeInput) (middleware.InitializeOutput, middleware.Metadata, error) {
		owner.End(rudderstackai.EndOptions{})
		return middleware.InitializeOutput{Result: result}, middleware.Metadata{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	spans := rec.named(t, "ai_otel_span")
	if len(spans) != 1 {
		t.Fatalf("spans=%d", len(spans))
	}
	raw, _ := json.Marshal(spans[0])
	if !strings.Contains(string(raw), "late_tool") || !strings.Contains(string(raw), "late argument") {
		t.Fatalf("late content lost: %s", raw)
	}
}
