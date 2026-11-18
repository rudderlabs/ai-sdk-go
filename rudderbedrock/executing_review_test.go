package rudderbedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go/middleware"

	ai "github.com/rudderlabs/ai-sdk-go"
)

func TestExecutingBedrockRequestHandoff(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			r := setup(t)
			if err := ai.Configure(ai.WithEmitSpans(true), ai.WithCaptureContent(true), ai.WithLLMCallEvents(true)); err != nil {
				t.Fatal(err)
			}
			ctx, owner := ai.StartInteraction(context.Background(), ai.InteractionOptions{UserID: "u", Classify: ai.Ptr(false)})
			text := &types.ContentBlockMemberText{Value: "SENT"}
			messages := []types.Message{{Role: types.ConversationRoleUser, Content: []types.ContentBlock{text}}}
			var parameters any = &bedrockruntime.ConverseInput{ModelId: aws.String(model), Messages: messages}
			if stream {
				parameters = &bedrockruntime.ConverseStreamInput{ModelId: aws.String(model), Messages: messages}
			}
			tracker := &tracker{cfg: &config{}}
			_, _, err := tracker.handle(ctx, middleware.InitializeInput{Parameters: parameters}, func(context.Context) (middleware.InitializeOutput, middleware.Metadata, error) {
				if text.Value != "SENT" {
					t.Fatal("incorrect provider input")
				}
				text.Value = "PRIVATE_ADDED"
				return middleware.InitializeOutput{}, middleware.Metadata{}, fmt.Errorf("provider error")
			})
			if err == nil {
				t.Fatal("provider error lost")
			}
			owner.End(ai.EndOptions{})
			calls := r.named(t, "ai_llm_response_received")
			if len(calls) != 1 {
				t.Fatalf("calls=%d", len(calls))
			}
			wire, _ := json.Marshal(calls)
			if !strings.Contains(string(wire), "SENT") || strings.Contains(string(wire), "PRIVATE_ADDED") {
				t.Fatalf("captured %s", wire)
			}
		})
	}
}
