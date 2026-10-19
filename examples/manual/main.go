// Command manual tracks an agent turn without a provider integration: the app reports each model
// call and tool call itself (StartInteraction, RecordCall, RecordToolCalls, RecordToolResult,
// End). A declared tool event, per-call events, a tool breakdown and a custom classifier are on.
// It makes no model call, so it needs no provider key.
//
//	RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/manual
//
// With --check it runs end to end against a mock data plane and exits non-zero when an expected
// event is missing or wrong. It needs no network or credentials. --live runs the same check with
// the events sent to RUDDERSTACK_DATA_PLANE_URL, and is skipped without credentials.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"slices"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("manual", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "run end to end against a mock data plane and exit")
	live := fs.Bool("live", false, "run the check with events sent to RUDDERSTACK_DATA_PLANE_URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *check || *live {
		return harness.Run(stderr, *live, runCheck)
	}
	return harness.Once(stderr, func(sink *harness.Sink) error {
		_, err := runSink(sink)
		return err
	})
}

const (
	model  = "gpt-4.1-mini"
	prompt = "My invoice export keeps failing, please open a ticket."
	answer = "I opened ticket T-1042 with high priority; support will reply within a day."
)

// result is what the app returns to its client: the IDs feedback refers to.
type result struct {
	InteractionID, ConversationID string
}

// runSink runs the example with its events going to the sink.
func runSink(sink *harness.Sink) (result, error) {
	rs, err := sink.NewClient()
	if err != nil {
		return result{}, err
	}
	if err := setup(); err != nil {
		return result{}, errors.Join(err, rs.Close())
	}
	res := helpdesk(context.Background(), rs)
	// Flush waits for the classification; Close then sends the queue.
	rudderstackai.Flush(0)
	return res, rs.Close()
}

// setup configures the process once: a declared tool event, per-call events, the tool breakdown,
// a classifier of your own and a price.
func setup() error {
	return rudderstackai.Configure(
		rudderstackai.WithEventRules(rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{
			"create_ticket": {
				Event: "ticket_created",
				Properties: func(args map[string]any, result any) map[string]any {
					ticket, _ := result.(map[string]any)
					return map[string]any{"priority": args["priority"], "ticket_id": ticket["id"]}
				},
			},
		}}),
		rudderstackai.WithLLMCallEvents(true),
		rudderstackai.WithToolBreakdown(true),
		rudderstackai.WithClassifier(&rudderstackai.ClassifierOptions{
			Classify: classify,
			Intents:  []string{"open_ticket", "billing_question"},
		}),
		// Example rates in USD per million tokens: use your own.
		rudderstackai.WithPrices(map[string]rudderstackai.ModelPrice{
			model: {Provider: "openai", TokenPrice: rudderstackai.TokenPrice{InputPerMtok: 0.40, OutputPerMtok: 1.60}},
		}),
	)
}

// classify is a rule-based classifier; it could equally call your own model. It runs after the
// answer is out, and sees tool names, never their arguments.
func classify(_ context.Context, turn rudderstackai.ClassifyTurn) (rudderstackai.ClassifyResult, error) {
	if slices.Contains(turn.ToolNames, "create_ticket") {
		return rudderstackai.ClassifyResult{Intent: "open_ticket", Outcome: "resolved", IntentSummary: "The user asked for a support ticket."}, nil
	}
	return rudderstackai.ClassifyResult{Intent: "other", Outcome: "unclear"}, nil
}

// createTicket is the app's tool.
func createTicket(args map[string]any) map[string]any {
	return map[string]any{"id": "T-1042", "priority": args["priority"]}
}

// helpdesk is one agent turn, reported by hand: a model call that asks for the create_ticket tool,
// the tool run, then the model call that writes the answer.
func helpdesk(ctx context.Context, rs analytics.Client) result {
	_, in := rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{
		UserID: "user_42", ConversationID: "conv_helpdesk_1", Feature: "helpdesk", Surface: rudderstackai.SurfaceSlack,
		PromptText: prompt, Analytics: rs,
	})
	// Pass the context StartInteraction returns to code that reports into the interaction.
	defer in.End(rudderstackai.EndOptions{ResponseText: answer})

	// ... call your model here ...
	in.RecordCall(rudderstackai.LLMCall{
		Provider: "openai", Model: model, InputTokens: 120, OutputTokens: 18,
		ResponseID: "resp_tool", FinishReason: "tool_calls",
	})
	args := map[string]any{"priority": "high", "summary": "Invoice export fails"}
	in.RecordToolCalls(rudderstackai.ToolCall{ID: "call_1", Name: "create_ticket", Args: args})
	toolStarted := time.Now()
	ticket := createTicket(args)
	took := time.Since(toolStarted)
	in.RecordToolResult("call_1", rudderstackai.ToolResult{Error: rudderstackai.Ptr(false), Duration: &took, Result: ticket})

	in.RecordCall(rudderstackai.LLMCall{
		Provider: "openai", Model: model, InputTokens: 160, OutputTokens: 25,
		ResponseID: "resp_answer", FinishReason: "stop", OutputText: answer,
	})
	return result{InteractionID: in.ID(), ConversationID: in.ConversationID()}
}
