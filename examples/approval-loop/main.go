// Command approval-loop runs a background refund agent that pauses twice for approval: each
// pause sends ai_response_received (response_status paused) and stores the run's snapshot, and
// a later worker, as after a pod restart, resumes the same interaction from it. The final answer
// carries pause_count and total_paused_ms; ai_task_started (switched on here) marks the start.
//
//	ANTHROPIC_API_KEY=... RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/approval-loop
//
// With --check it runs end to end against a mock data plane, with each model call answered from
// spec/fixtures/cassettes/examples/node-anthropic.json, and exits non-zero when an expected event
// is missing or wrong. It needs no network or credentials. --live runs the same check with the
// events sent to RUDDERSTACK_DATA_PLANE_URL, and is skipped without credentials.
package main

import (
	"context"
	"encoding/json"
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
	fs := flag.NewFlagSet("approval-loop", flag.ContinueOnError)
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
		fmt.Fprintf(stderr, "refund: %s\n", res.Answer)
		return err
	})
}

const model = "claude-haiku-4-5"

// result is the agent's last answer and the IDs its events carry.
type result struct {
	Answer, InteractionID, ConversationID string
}

// runSink runs the agent with its events going to the sink and the given Anthropic client options.
func runSink(sink *harness.Sink, opts ...option.RequestOption) (result, error) {
	rs, err := sink.NewClient()
	if err != nil {
		return result{}, err
	}
	res, err := refund(context.Background(), anthropic.NewClient(append(opts, rudderanthropic.WithTracking())...), rs)
	// Close flushes queued events.
	return res, errors.Join(err, rs.Close())
}

// store stands in for the job's database: it keeps the paused run's snapshot between workers.
type store map[string][]byte

// refund handles one refund request in three worker runs: it drafts the refund and waits for a
// manager, then for finance, then issues it. Each wait is a pause; the next run resumes it.
func refund(ctx context.Context, client anthropic.Client, rs analytics.Client) (result, error) {
	if err := rudderstackai.Configure(rudderstackai.WithTaskStartedEvents(true)); err != nil {
		return result{}, err
	}
	db := store{}
	steps := []string{
		"Draft a refund of $120 for order 1042 in one sentence, for a manager to approve.",
		"The manager approved. Summarize the refund for finance in one sentence.",
		"Finance approved. Confirm the refund to the customer in one sentence.",
	}
	var res result
	for n, prompt := range steps {
		last := n == len(steps)-1
		if err := worker(ctx, client, rs, db, prompt, last, &res); err != nil {
			return res, err
		}
	}
	return res, nil
}

// worker is one run of the job, as a fresh process would start it: it resumes the stored run, if
// any, asks the model, then pauses for the next approval unless this is the last step.
func worker(ctx context.Context, client anthropic.Client, rs analytics.Client, db store, prompt string, last bool, res *result) error {
	o := rudderstackai.TaskOptions{TriggerSource: "queue", AccountID: "acct_acme", OnBehalfOf: "user_42", Feature: "refunds", Analytics: rs}
	if saved, ok := db["refund-1042"]; ok {
		o.Resume = &rudderstackai.PausedInteraction{}
		if err := json.Unmarshal(saved, o.Resume); err != nil {
			return err
		}
	}
	return rudderstackai.RunTask(ctx, "refund-approval", o, func(ctx context.Context, task *rudderstackai.Interaction) error {
		res.InteractionID, res.ConversationID = task.ID(), task.ConversationID()
		var err error
		if res.Answer, err = ask(ctx, client, prompt); err != nil {
			return err
		}
		if last {
			return nil
		}
		// Waits for a person: RunTask leaves the run paused, and the snapshot resumes it later.
		paused, _ := task.Pause(rudderstackai.PauseOptions{Reason: rudderstackai.PauseApproval})
		db["refund-1042"], err = json.Marshal(paused)
		return err
	})
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
