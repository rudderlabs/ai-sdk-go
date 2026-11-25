package rudderopenai_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

// Chat and Responses outputs carry no error flag: a vendor failure is an error, anything else unknown.
func TestToolOutputFailureFixture(t *testing.T) {
	for _, c := range testkit.ToolOutputCases() {
		for _, api := range []string{"chat", "responses"} {
			t.Run(api+"/"+c.Name, func(t *testing.T) {
				var seen []any
				rec, llm := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithEventRules(rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{
					"lookup": {Event: "lookup_done", Properties: func(_ map[string]any, result any) map[string]any { seen = append(seen, result); return nil }},
				}}))
				llm.Script("openai.chat", reply(t, `{"json":{"id":"c1","model":"gpt-4.1-mini","choices":[{"index":0,"message":{"content":"done"},"finish_reason":"stop"}],"usage":{}}}`))
				llm.Script("openai.responses", reply(t, `{"json":{"id":"r1","model":"gpt-4.1-mini","status":"completed","output":[],"usage":{}}}`))
				output, _ := json.Marshal(c.Output)
				client := newClient(llm)
				err := rudderstackai.RunInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u1"}, func(ctx context.Context, i *rudderstackai.Interaction) error {
					i.RecordToolCalls(rudderstackai.ToolCall{ID: "call-1", Name: "lookup"})
					if api == "chat" {
						_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body(`{"model":"gpt-4.1-mini","messages":[{"role":"tool","tool_call_id":"call-1","content":`+string(output)+`}]}`))
						return err
					}
					_, err := client.Responses.New(ctx, responses.ResponseNewParams{}, body(`{"model":"gpt-4.1-mini","input":[{"type":"function_call_output","call_id":"call-1","output":`+string(output)+`}]}`))
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				events := rec.events(t)
				event, status := one(t, events, "lookup_done"), "unknown"
				if c.Expected.Error {
					status = "error"
				}
				errorType, _ := event["error_type"].(string)
				if event["tool_status"] != status || errorType != c.Expected.ErrorType {
					t.Errorf("lookup_done %v", event)
				}
				breakdown := one(t, events, "ai_response_received")["tool_breakdown"].([]any)
				if len(breakdown) != 1 || (num(breakdown[0].(obj)["error_count"]) == 1) != c.Expected.Error {
					t.Errorf("tool_breakdown %v", breakdown)
				}
				// Rules see the decoded output they saw before the convention.
				got, _ := json.Marshal(seen)
				want, _ := json.Marshal([]any{kit.ParseToolValue(c.Output)})
				if string(got) != string(want) {
					t.Errorf("rule saw %s, want %s", got, want)
				}
			})
		}
	}
}
