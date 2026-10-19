// Command openai answers a support question with a streamed OpenAI Chat Completions call tracked
// by rudderopenai, inside a RudderStack interaction, then records the user's thumbs-up.
//
//	OPENAI_API_KEY=... RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/openai
//
// With --check it runs end to end against a mock data plane, with the model call answered from
// spec/fixtures/cassettes/examples/node-openai.json, and exits non-zero when an expected event is
// missing or wrong. It needs no network or credentials. --live runs the same check with the
// events sent to RUDDERSTACK_DATA_PLANE_URL, and is skipped without credentials.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/rudderopenai"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("openai", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "run end to end against a mock data plane and exit")
	live := fs.Bool("live", false, "run the check with events sent to RUDDERSTACK_DATA_PLANE_URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *check || *live {
		return harness.Run(stderr, *live, runCheck)
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		fmt.Fprintln(stderr, "set OPENAI_API_KEY")
		return 1
	}
	return harness.Once(stderr, func(sink *harness.Sink) error {
		_, err := runSink(sink, option.WithAPIKey(key))
		return err
	})
}

const (
	model    = "gpt-4.1-mini"
	question = "How long does standard shipping take?"
)

// result is what the app returns to its client: the answer and the IDs feedback refers to.
type result struct {
	Answer, InteractionID, ConversationID string
}

// runSink runs the example with its events going to the sink and the given OpenAI client options.
func runSink(sink *harness.Sink, opts ...option.RequestOption) (result, error) {
	rs, err := sink.NewClient()
	if err != nil {
		return result{}, err
	}
	res, err := support(context.Background(), openai.NewClient(append(opts, rudderopenai.WithTracking())...), rs)
	// Close flushes queued events.
	return res, errors.Join(err, rs.Close())
}

// support prices the model, answers one question in an interaction, then sends the user's
// thumbs-up for that answer, as a feedback endpoint would.
func support(ctx context.Context, client openai.Client, rs analytics.Client) (result, error) {
	// Example rates in USD per million tokens: use your own.
	if err := rudderstackai.RegisterPrice(model, rudderstackai.ModelPrice{
		Provider:   "openai",
		TokenPrice: rudderstackai.TokenPrice{InputPerMtok: 0.40, OutputPerMtok: 1.60, CachedInputPerMtok: rudderstackai.Ptr(0.10)},
	}); err != nil {
		return result{}, err
	}
	var res result
	err := rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{
		UserID: "user_42", ConversationID: "conv_shipping_1", Feature: "support_bot", Surface: rudderstackai.SurfaceWeb,
		InputMethod: "text", Analytics: rs,
	}, func(ctx context.Context, in *rudderstackai.Interaction) error {
		res.InteractionID, res.ConversationID = in.ID(), in.ConversationID()
		stream := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
			Model:               model,
			MaxCompletionTokens: openai.Int(100),
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage("You are a concise support agent for an online store."),
				openai.UserMessage(question),
			},
		})
		defer stream.Close()
		for stream.Next() {
			for _, c := range stream.Current().Choices {
				res.Answer += c.Delta.Content
			}
		}
		return stream.Err()
	})
	if err != nil {
		return res, err
	}
	rudderstackai.TrackAction(ctx, rudderstackai.TrackActionOptions{
		ActionType: rudderstackai.ActionFeedbackGiven, InteractionID: res.InteractionID, ConversationID: res.ConversationID,
		UserID: "user_42", Surface: rudderstackai.SurfaceWeb, Analytics: rs,
		ActionDetails: map[string]any{"feedback_type": "thumbs", "feedback_value": 1},
	})
	return res, nil
}
