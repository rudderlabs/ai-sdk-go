// Command managed-agents runs one Claude Managed Agents session turn: the agent runs `echo hi` with
// the built-in bash tool, calls a custom get_weather tool the app answers, then replies.
// rudderanthropic.TrackSessionEvents reports the turn as one answer with both model requests and
// both tools.
//
//	ANTHROPIC_API_KEY=... RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/managed-agents
//
// With --check it runs end to end against a mock data plane, with the session answered from
// spec/fixtures/cassettes/claude-managed-agents-tools.json (recorded live), and exits non-zero
// when an expected event is missing or wrong. It needs no network or credentials. --live runs the
// same check with the events sent to RUDDERSTACK_DATA_PLANE_URL, and is skipped without
// credentials.
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
	fs := flag.NewFlagSet("managed-agents", flag.ContinueOnError)
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
	model  = "claude-haiku-5-5"
	prompt = "Step 1: run `echo hi` with bash. Step 2: call get_weather for Paris. " +
		"Step 3: reply with one word describing the weather."
)

// result is what the app keeps: the answer and the session it ran in.
type result struct {
	Answer, SessionID string
}

// runSink runs the example with its events going to the sink and the given Anthropic client
// options.
func runSink(sink *harness.Sink, opts ...option.RequestOption) (result, error) {
	rs, err := sink.NewClient()
	if err != nil {
		return result{}, err
	}
	res, err := session(context.Background(), anthropic.NewClient(opts...), rs)
	// Close flushes queued events.
	return res, errors.Join(err, rs.Close())
}

// session creates an environment, an agent and a session, then runs one turn. Agents and
// environments are created once and reused; set MANAGED_ENVIRONMENT_ID and MANAGED_AGENT_ID to
// skip creating them.
func session(ctx context.Context, client anthropic.Client, rs analytics.Client) (result, error) {
	// Example rates in USD per million tokens: use your own.
	if err := rudderstackai.RegisterPrice(model, rudderstackai.ModelPrice{
		Provider: "anthropic",
		TokenPrice: rudderstackai.TokenPrice{
			InputPerMtok: 0.1, OutputPerMtok: 0.5,
			CachedInputPerMtok: rudderstackai.Ptr(0.01), CacheWritePerMtok: rudderstackai.Ptr(0.125),
		},
	}); err != nil {
		return result{}, err
	}
	environmentID, agentID, err := setup(ctx, client)
	if err != nil {
		return result{}, err
	}
	s, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(agentID)},
		EnvironmentID: environmentID,
		Title:         anthropic.String("rudderstack-ai example"),
	})
	if err != nil {
		return result{}, err
	}
	res := result{SessionID: s.ID}
	// Open the stream before sending, so no event is missed.
	stream := client.Beta.Sessions.Events.StreamEvents(ctx, s.ID, anthropic.BetaSessionEventStreamParams{})
	events := rudderanthropic.TrackSessionEvents(ctx, stream,
		rudderanthropic.WithSession(s),
		rudderanthropic.WithSessionAnalytics(rs),
		rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{UserID: "user_42", Feature: "managed_agents_example"}))
	defer func() { _ = events.Close() }()
	if _, err := client.Beta.Sessions.Events.Send(ctx, s.ID, anthropic.BetaSessionEventSendParams{
		Events: []anthropic.BetaManagedAgentsEventParamsUnion{{OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
			Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
			Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
				OfText: &anthropic.BetaManagedAgentsTextBlockParam{Text: prompt},
			}},
		}}},
	}); err != nil {
		return res, err
	}
	for events.Next() {
		switch event := events.Current().AsAny().(type) {
		case anthropic.BetaManagedAgentsAgentMessageEvent:
			for _, block := range event.Content {
				res.Answer += block.Text
			}
		case anthropic.BetaManagedAgentsAgentCustomToolUseEvent:
			if _, err := client.Beta.Sessions.Events.Send(ctx, s.ID, anthropic.BetaSessionEventSendParams{
				Events: []anthropic.BetaManagedAgentsEventParamsUnion{{OfUserCustomToolResult: &anthropic.BetaManagedAgentsUserCustomToolResultEventParams{
					Type:            anthropic.BetaManagedAgentsUserCustomToolResultEventParamsTypeUserCustomToolResult,
					CustomToolUseID: event.ID,
					Content: []anthropic.BetaManagedAgentsUserCustomToolResultEventParamsContentUnion{{
						OfText: &anthropic.BetaManagedAgentsTextBlockParam{Text: "Sunny, 21 C"},
					}},
				}}},
			}); err != nil {
				return res, err
			}
		case anthropic.BetaManagedAgentsSessionStatusIdleEvent:
			if event.StopReason.Type == "requires_action" {
				continue
			}
			if event.StopReason.Type != "end_turn" {
				return res, fmt.Errorf("session stopped: %s", event.StopReason.Type)
			}
			return res, finish(ctx, client, s.ID)
		case anthropic.BetaManagedAgentsSessionStatusTerminatedEvent:
			return res, errors.New("session terminated")
		}
	}
	if err := events.Err(); err != nil {
		return res, err
	}
	// No idle or termination was read: the turn did not finish.
	return res, errors.New("the stream ended before the session went idle")
}

// setup returns the environment and agent to run in, creating them unless their IDs are set.
func setup(ctx context.Context, client anthropic.Client) (environmentID, agentID string, err error) {
	environmentID, agentID = os.Getenv("MANAGED_ENVIRONMENT_ID"), os.Getenv("MANAGED_AGENT_ID")
	if environmentID == "" {
		env, err := client.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
			Name: "rudderstack-ai-example",
			Config: anthropic.BetaEnvironmentNewParamsConfigUnion{OfCloud: &anthropic.BetaCloudConfigParams{
				Networking: anthropic.BetaCloudConfigParamsNetworkingUnion{OfLimited: &anthropic.BetaLimitedNetworkParams{
					AllowPackageManagers: anthropic.Bool(false), AllowMCPServers: anthropic.Bool(false),
				}},
			}},
		})
		if err != nil {
			return "", "", err
		}
		environmentID = env.ID
	}
	if agentID == "" {
		agent, err := client.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
			Name:   "rudderstack-ai-example",
			Model:  anthropic.BetaManagedAgentsModelConfigParams{ID: model},
			System: anthropic.String("Be brief. Follow the steps exactly."),
			Tools: []anthropic.BetaAgentNewParamsToolUnion{
				{OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
					DefaultConfig: anthropic.BetaManagedAgentsAgentToolsetDefaultConfigParams{
						Enabled: anthropic.Bool(false),
						PermissionPolicy: anthropic.BetaManagedAgentsAgentToolsetDefaultConfigParamsPermissionPolicyUnion{
							OfAlwaysAllow: &anthropic.BetaManagedAgentsAlwaysAllowPolicyParam{},
						},
					},
					Configs: []anthropic.BetaManagedAgentsAgentToolConfigParamsUnion{
						{OfBash: &anthropic.BetaManagedAgentsBashToolConfigParams{Enabled: anthropic.Bool(true)}},
					},
				}},
				{OfCustom: &anthropic.BetaManagedAgentsCustomToolParams{
					Type:        anthropic.BetaManagedAgentsCustomToolParamsTypeCustom,
					Name:        "get_weather",
					Description: "Current weather for a city.",
					InputSchema: anthropic.BetaManagedAgentsCustomToolInputSchemaParam{
						Properties: map[string]any{"city": map[string]any{"type": "string"}},
						Required:   []string{"city"},
					},
				}},
			},
		})
		if err != nil {
			return "", "", err
		}
		agentID = agent.ID
	}
	return environmentID, agentID, nil
}

// finish reads the session's usage; list_cost includes runtime, so the events price tokens instead.
func finish(ctx context.Context, client anthropic.Client, sessionID string) error {
	s, err := client.Beta.Sessions.Get(ctx, sessionID, anthropic.BetaSessionGetParams{})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "session list cost: %s cents\n", s.Usage.ListCost.Amount)
	return nil
}
