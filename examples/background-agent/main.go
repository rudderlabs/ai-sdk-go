// Command background-agent runs a scheduled job with no user in the loop: rudderstackai.RunTask
// drafts a digest with a streamed Anthropic call (tracked by rudderanthropic), hands the draft to
// a review sub-task chained to it, and scores the result with TrackScore.
//
//	ANTHROPIC_API_KEY=... RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/background-agent
//
// With --check it runs end to end against a mock data plane, with each model call answered from
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
	fs := flag.NewFlagSet("background-agent", flag.ContinueOnError)
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
		fmt.Fprintf(stderr, "digest: %s\nreview: %s\n", res.Draft, res.Review)
		return err
	})
}

const model = "claude-haiku-4-5"

// result is what the job produced, and the IDs its events carry.
type result struct {
	Draft, Review, TaskID, ReviewID, ConversationID string
}

// runSink runs the job with its events going to the sink and the given Anthropic client options.
func runSink(sink *harness.Sink, opts ...option.RequestOption) (result, error) {
	rs, err := sink.NewClient()
	if err != nil {
		return result{}, err
	}
	res, err := nightlyDigest(context.Background(), anthropic.NewClient(append(opts, rudderanthropic.WithTracking())...), rs)
	// Close flushes queued events.
	return res, errors.Join(err, rs.Close())
}

// nightlyDigest is the scheduled job. Its actor is the system; the account and the person it
// works for are named separately. The review sub-task chains to it through the context.
func nightlyDigest(ctx context.Context, client anthropic.Client, rs analytics.Client) (result, error) {
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
	err := rudderstackai.RunTask(ctx, "nightly-digest", rudderstackai.TaskOptions{
		TriggerSource: "schedule", AccountID: "acct_acme", OnBehalfOf: "user_42", Feature: "ops_digest", Analytics: rs,
	}, func(ctx context.Context, task *rudderstackai.Interaction) error {
		res.TaskID, res.ConversationID = task.ID(), task.ConversationID()
		var err error
		if res.Draft, err = ask(ctx, client, "Summarize yesterday's support tickets for the account owner in one sentence."); err != nil {
			return err
		}
		return rudderstackai.RunTask(ctx, "digest-review", rudderstackai.TaskOptions{Analytics: rs},
			func(ctx context.Context, review *rudderstackai.Interaction) error {
				res.ReviewID = review.ID()
				res.Review, err = ask(ctx, client, "Check this digest for mistakes, in one sentence: "+res.Draft)
				return err
			})
	})
	if err != nil {
		return res, err
	}
	// A code check of the digest, sent as ai_interaction_scored for the task.
	rudderstackai.TrackScore(ctx, rudderstackai.TrackScoreOptions{
		Score:         rudderstackai.Score{EvaluationName: "digest_quality", ScoreValue: rudderstackai.Ptr(0.9), Evaluator: rudderstackai.EvaluatorCode},
		InteractionID: res.TaskID, ConversationID: res.ConversationID, Feature: "ops_digest",
		Trigger: rudderstackai.TriggerSystem, AccountID: "acct_acme", OnBehalfOf: "user_42", Analytics: rs,
	})
	return res, nil
}

// ask streams one short answer from the model.
func ask(ctx context.Context, client anthropic.Client, prompt string) (string, error) {
	stream := client.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: 64,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
	})
	defer stream.Close()
	var msg anthropic.Message
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return "", err
		}
	}
	if err := stream.Err(); err != nil {
		return "", err
	}
	text := ""
	for _, b := range msg.Content {
		text += b.Text
	}
	return text, nil
}
