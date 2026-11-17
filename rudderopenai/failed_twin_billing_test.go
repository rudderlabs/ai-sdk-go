package rudderopenai_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"

	ai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/rudderopenai"
)

type failedUsageTransport struct {
	interaction *ai.Interaction
	status      string
}

func (rt failedUsageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	rt.interaction.RecordCall(ai.LLMCall{Provider: "openai", Model: "gpt-4o", Integration: "tracing", InputTokens: 10, OutputTokens: 2, StartedAt: start, EndedAt: start.Add(time.Millisecond)})
	payload := fmt.Sprintf(`{"id":"resp-failed","object":"response","created_at":1,"model":"gpt-4o","status":%q,"output":[],"error":{"code":"server_error","message":"failed"},"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}`, rt.status)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(payload)), Request: req}, nil
}

func TestWrappedHTTP200ResponsesWithStatuslessUsageTwin(t *testing.T) {
	for _, emit := range []bool{false, true} {
		for _, status := range []string{"failed", "completed"} {
			t.Run(fmt.Sprintf("%s/spans=%t", status, emit), func(t *testing.T) {
				rec, _ := setup(t, ai.WithLLMCallEvents(true), ai.WithEmitSpans(emit), ai.WithCredits(func(ai.CreditCall) (*float64, error) { return ai.Ptr(7.0), nil }))
				if err := ai.RegisterPrice("gpt-4o", ai.ModelPrice{TokenPrice: ai.TokenPrice{InputPerMtok: 1000000}}); err != nil {
					t.Fatal(err)
				}
				ctx, i := ai.StartInteraction(context.Background(), ai.InteractionOptions{UserID: "u", NoPromptEvent: true})
				client := openai.NewClient(option.WithAPIKey("test"), option.WithMaxRetries(0), option.WithHTTPClient(&http.Client{Transport: failedUsageTransport{i, status}}), rudderopenai.WithTracking())
				result, err := client.Responses.New(ctx, responses.ResponseNewParams{}, body(`{"model":"gpt-4o","input":"hello"}`))
				if err != nil {
					t.Fatal(err)
				}
				if string(result.Status) != status {
					t.Fatal(result.Status)
				}
				i.End(ai.EndOptions{})
				events := rec.events(t)
				response, call := one(t, events, "ai_response_received"), one(t, events, "ai_llm_response_received")
				if num(response["llm_call_count"]) != 1 {
					t.Fatal(response)
				}
				if status == "failed" {
					if response["error_type"] != "api_error" || call["error_type"] != "api_error" || response["total_cost_usd"] != nil || response["total_credits"] != nil || call["cost_usd"] != nil || call["credits"] != nil {
						t.Fatal(response, call)
					}
				} else if num(response["total_cost_usd"]) != 10 || num(response["total_credits"]) != 7 || num(call["cost_usd"]) != 10 || num(call["credits"]) != 7 {
					t.Fatal(response, call)
				}
			})
		}
	}
}
