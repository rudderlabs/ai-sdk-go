package ruddergenai_test

import (
	"context"
	"testing"

	"google.golang.org/genai"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
	"github.com/rudderlabs/ai-sdk-go/ruddergenai"
)

// Gemini passes a function response's output as it is, so a JSON string is decoded for the check only.
func TestToolOutputFailureFixture(t *testing.T) {
	type tc struct {
		name      string
		response  map[string]any
		failed    bool
		errorType any
	}
	cases := []tc{{"whole response successful false", map[string]any{"successful": false}, true, "api_error"}}
	for _, c := range testkit.ToolOutputCases() {
		key, errorType := "output", any(nil)
		if c.FrameworkFailure {
			key = "error" // Google's failure convention
		}
		if c.Expected.ErrorType != "" {
			errorType = c.Expected.ErrorType
		}
		cases = append(cases, tc{c.Name, map[string]any{key: c.Output}, c.Expected.Error, errorType})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var seen []any
			rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithEventRules(rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{
				"lookup": {Event: "lookup_done", Properties: func(_ map[string]any, result any) map[string]any { seen = append(seen, result); return nil }},
			}}))
			llm.Script("google.generate", jsonReply(reply([]any{m{"text": "ok"}}, nil, "STOP")))
			client := newClient(t, genai.BackendGeminiAPI, ruddergenai.HTTPClient(nil))
			err := rudderstackai.RunInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u1"}, func(ctx context.Context, i *rudderstackai.Interaction) error {
				i.RecordToolCalls(rudderstackai.ToolCall{ID: "call-1", Name: "lookup"})
				contents := []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{
					{FunctionResponse: &genai.FunctionResponse{ID: "call-1", Name: "lookup", Response: c.response}},
				}}}
				_, err := client.Models.GenerateContent(ctx, model, contents, nil)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			status, errorType, breakdown := "success", any(nil), m{"tool_name": "lookup", "call_count": 1, "error_count": 0}
			if c.failed {
				status, errorType = "error", c.errorType
				breakdown = m{"tool_name": "lookup", "call_count": 1, "error_count": 1, "error_types": m{"api_error": 1}}
			}
			events := 0
			for _, e := range rec.events(t) {
				if e.Event == "lookup_done" {
					events++
					want(t, e.Properties, "tool_status", status)
					want(t, e.Properties, "error_type", errorType)
					want(t, e.Properties, "tool_call_id", "call-1")
				}
			}
			if events != 1 {
				t.Errorf("lookup_done sent %d times", events)
			}
			want(t, rec.response(t), "tool_breakdown", []any{breakdown})
			// Rules see the output as sent, not the convention's own decode.
			want(t, m{"seen": seen}, "seen", []any{first(c.response)})
		})
	}
}

func first(response map[string]any) any {
	for _, key := range []string{"error", "output"} {
		if v, ok := response[key]; ok {
			return v
		}
	}
	return response
}
