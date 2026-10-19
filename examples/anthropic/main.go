// Command anthropic answers a support question with a streamed Anthropic Messages call tracked by
// rudderanthropic, inside a RudderStack interaction, with the store policy as a cached system
// prompt and a registered price that covers cache reads and writes.
//
//	ANTHROPIC_API_KEY=... RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/anthropic
//
// With --check it runs end to end against a mock data plane, with the model call answered from
// spec/fixtures/cassettes/examples/node-anthropic.json, and exits non-zero when an expected event
// is missing or wrong. It needs no network or credentials. --live runs the same check with the
// events sent to RUDDERSTACK_DATA_PLANE_URL, and is skipped without credentials.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/rudderanthropic"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("anthropic", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "run end to end against a mock data plane and exit")
	live := fs.Bool("live", false, "run the check with events sent to RUDDERSTACK_DATA_PLANE_URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *check || *live {
		return harness.Run(stderr, *live, runCheck)
	}
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		fmt.Fprintln(stderr, "set ANTHROPIC_API_KEY")
		return 1
	}
	return harness.Once(stderr, func(sink *harness.Sink) error {
		res, err := runSink(sink, option.WithAPIKey(key))
		fmt.Fprintf(stderr, "answer: %s\n", res.Answer)
		return err
	})
}

const (
	model    = "claude-haiku-4-5"
	question = "Can I change my delivery address after ordering?"
	// policy is the long, stable part of the prompt: cached, so later turns read it at the cache
	// rate (once it reaches the model's minimum cacheable length).
	policy = "You are a concise support agent for an online store. Orders can be edited until they ship. " +
		"Shipping takes 3 to 5 business days. Returns are accepted within 30 days. Answer in one sentence."
)

// result is what the app returns to its client: the answer and the IDs feedback refers to.
type result struct {
	Answer, InteractionID, ConversationID string
}

// runSink runs the example with its events going to the sink and the given Anthropic client
// options.
func runSink(sink *harness.Sink, opts ...option.RequestOption) (result, error) {
	rs, err := sink.NewClient()
	if err != nil {
		return result{}, err
	}
	res, err := support(context.Background(), anthropic.NewClient(append(opts, rudderanthropic.WithTracking())...), rs)
	// Close flushes queued events.
	return res, errors.Join(err, rs.Close())
}

// support prices the model, cache rates included, and answers one question in an interaction.
func support(ctx context.Context, client anthropic.Client, rs analytics.Client) (result, error) {
	// Example rates in USD per million tokens: use your own.
	if err := rudderstackai.RegisterPrice(model, rudderstackai.ModelPrice{
		Provider: "anthropic",
		TokenPrice: rudderstackai.TokenPrice{
			InputPerMtok: 1, OutputPerMtok: 5,
			CachedInputPerMtok: rudderstackai.Ptr(0.10), CacheWritePerMtok: rudderstackai.Ptr(1.25),
		},
	}); err != nil {
		return result{}, err
	}
	var res result
	err := rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{
		UserID: "user_42", ConversationID: "conv_address_1", Feature: "support_bot", Surface: rudderstackai.SurfaceWeb,
		Analytics: rs,
	}, func(ctx context.Context, in *rudderstackai.Interaction) error {
		res.InteractionID, res.ConversationID = in.ID(), in.ConversationID()
		stream := client.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
			Model:     model,
			MaxTokens: 64,
			System:    []anthropic.TextBlockParam{{Text: policy, CacheControl: anthropic.NewCacheControlEphemeralParam()}},
			Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(question))},
		})
		defer stream.Close()
		var msg anthropic.Message
		for stream.Next() {
			if err := msg.Accumulate(stream.Current()); err != nil {
				return err
			}
		}
		if err := stream.Err(); err != nil {
			return err
		}
		for _, b := range msg.Content {
			res.Answer += b.Text
		}
		return nil
	})
	return res, err
}
