# RudderStack AI SDK for Go

Standard AI product events for Go services, sent through the RudderStack Go client you already use and following the [AI events spec v0.3](spec/ai-events-v0.3.schema.json):

- AI interactions in chat apps and background agents: each answer becomes a small set of track events (prompt, answer with models, tokens, cost, latency and status, feedback, scores), however many model calls it took.
- MCP servers: each request an AI client makes becomes a track event that says which client called, which tool, whether it worked, and for which customer.

## How it fits

```
your app ─ provider client + integration ─ RunInteraction ─ analytics-go ─ RudderStack ─ warehouse, destinations
AI client ─ MCP ─ your MCP server (go-sdk or mcp-go) ─ wrapped ─ analytics-go ─ RudderStack
```

This is product analytics, not observability: it puts AI usage next to the rest of your customer data and complements your tracing tool. It is private by default: no prompt, answer, tool argument or result text is sent unless you turn content capture on, and captured text is masked.

The Node.js ([`@rudderstack/ai`](https://github.com/rudderlabs/ai-sdk-node)) and Python ([`rudderstack-ai`](https://github.com/rudderlabs/ai-sdk-python)) SDKs send the same events with the same properties.

## Install

```sh
go get github.com/rudderlabs/ai-sdk-go github.com/rudderlabs/analytics-go/v4
go get github.com/rudderlabs/ai-sdk-go/rudderopenai # plus the integration module for each provider SDK you use
```

The root module (Go 1.26 or later) holds the interaction API and MCP support and depends only on analytics-go and the MCP libraries. Each provider integration is its own module, so an MCP-only service does not pull provider SDKs.

Supported MCP libraries:

| MCP library | Import | Wrap with | Tested | Protocol revisions |
| --- | --- | --- | --- | --- |
| [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) (official) | `github.com/rudderlabs/ai-sdk-go` | `rudderstackai.WrapMCPServer` | v1.8.0 | 2025-06-18, 2025-11-25, 2026-07-28 |
| [`github.com/mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go) | `github.com/rudderlabs/ai-sdk-go/ruddermcpgo` | `ruddermcpgo.WrapServer` | v0.55.1 (the minimum) to v1.1.1 | v0.55.1 to v0.x: 2025-06-18, 2025-11-25; v1.x: also 2026-07-28 |

Both send the same events with the same options, defaults and properties. `ruddermcpgo` requires mcp-go v0.55.1, the oldest version it supports, so adding it never upgrades yours; CI tests v0.55.1 and v1.1.1. mcp-go v0.x does not serve 2026-07-28: latest clients fall back to `initialize`.

## Quick start

```go
import (
	"context"

	"github.com/openai/openai-go/v3"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/rudderopenai"
)

client, err := analytics.NewWithConfig(writeKey, analytics.Config{DataPlaneUrl: dataPlaneURL})
if err != nil {
	return err
}
defer client.Close() // sends queued events

if err := rudderstackai.Configure(rudderstackai.WithAnalytics(client)); err != nil {
	return err
}
llm := openai.NewClient(rudderopenai.WithTracking())

// One interaction per user turn: every model call inside it adds up into one answer.
var answer, interactionID string
err = rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{
	UserID: userID, ConversationID: chatID, Feature: "support_bot",
}, func(ctx context.Context, in *rudderstackai.Interaction) error {
	completion, err := llm.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    "gpt-4.1-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(question)},
	})
	if err != nil {
		return err // ends the answer as failed; returned unchanged
	}
	answer, interactionID = completion.Choices[0].Message.Content, in.ID() // send the ID to your client
	return nil
})
```

Unlike the Node.js and Python SDKs, `trace_id` is not read from the active OpenTelemetry span unless you opt in: `rudderstackai.Configure(rudderstackai.WithTraceIDFrom(rudderotel.TraceID))` with the `github.com/rudderlabs/ai-sdk-go/rudderotel` module.

This sends `ai_user_prompt_created` and `ai_response_received` (models, tokens, latency, status, and cost once the model is [priced](docs/configuration.md#pricing)). Pass the same context to every model call: a call made with a context that carries no interaction is reported as its own answer.

## Integrations

| Integration                                          | Package (`go get`)                                | Go SDK, version tested                                       | Tracks                                                                                 |
| ---------------------------------------------------- | ------------------------------------------------- | ------------------------------------------------------------ | -------------------------------------------------------------------------------------- |
| [`openai`](docs/integrations.md#openai)              | `github.com/rudderlabs/ai-sdk-go/rudderopenai`    | `github.com/openai/openai-go/v3` v3.70.0                     | Chat Completions and Responses API, streamed or not, through `option.WithMiddleware`; OpenAI-compatible gateways |
| [`anthropic`](docs/integrations.md#anthropic)        | `github.com/rudderlabs/ai-sdk-go/rudderanthropic` | `github.com/anthropics/anthropic-sdk-go` v1.78.0             | Messages API, streamed or not, also through its Bedrock and Vertex AI clients          |
| [`google-genai`](docs/integrations.md#google-gen-ai) | `github.com/rudderlabs/ai-sdk-go/ruddergenai`     | `google.golang.org/genai` v1.72.0                            | `GenerateContent`, `GenerateContentStream` and chats on the Gemini API (provider `google`) and Vertex AI (`google.vertex`), through the HTTP client |
| [`bedrock`](docs/integrations.md#amazon-bedrock)     | `github.com/rudderlabs/ai-sdk-go/rudderbedrock`   | `github.com/aws/aws-sdk-go-v2/service/bedrockruntime` v1.63.1 | `Converse` and `ConverseStream`, through the client's `APIOptions`                     |
| [`manual`](docs/integrations.md#manual)              | `github.com/rudderlabs/ai-sdk-go`                 | any                                                          | Calls and tool calls you report with `RecordCall`, `RecordToolCalls`                   |
| [`mcp`](docs/mcp.md)                                 | `github.com/rudderlabs/ai-sdk-go`                 | `github.com/modelcontextprotocol/go-sdk` v1.8.0              | MCP servers: clients, tool calls, resources, prompts, elicitations                    |

Every integration records the model, tokens (input always includes cached and cache-write tokens), cost inputs, finish reason, errors and tool calls; streamed calls are recorded when the stream ends. Rules that apply to all of them:

- One interaction per user turn, not per conversation or process. Pass your chat or thread ID as `ConversationID` so turns group into a conversation. Resending the history each turn is fine: each turn reports only its own calls.
- Read streams inside the `RunInteraction` function: a stream still open when the interaction ends is recorded as a cancelled call.
- The last call decides the status: a failed final call fails the answer even if your code handled the error; a failed call followed by a successful retry is a success.

Setup, what each one records, streaming, retries, gateways and limits: [docs/integrations.md](docs/integrations.md).

## Manual recording

For any other model client, report calls into the interaction:

```go
err := rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{UserID: userID, ConversationID: chatID},
	func(ctx context.Context, in *rudderstackai.Interaction) error {
		res, err := myLLM.Complete(ctx, prompt)
		if err != nil {
			return err
		}
		in.RecordCall(rudderstackai.LLMCall{
			Provider: "mistral_ai", Model: res.Model,
			InputTokens: res.Usage.Input, OutputTokens: res.Usage.Output, // input includes cached tokens
		})
		return nil
	})
```

`StartInteraction` returns the context and the `*Interaction` for code that cannot use a callback; call `End(rudderstackai.EndOptions{})` when the answer is out. Every `LLMCall` and `ToolCall` field: [docs/integrations.md](docs/integrations.md#manual).

## Background tasks

For LLM work with no user in front of it (a schedule, webhook, Slack mention, queue message or another agent), use `RunTask`:

```go
ctx = rudderstackai.WithValues(ctx, rudderstackai.ContextValues{AccountID: job.WorkspaceID, OnBehalfOf: job.CreatedBy})
err := rudderstackai.RunTask(ctx, "alert-triage", rudderstackai.TaskOptions{TriggerSource: "schedule", ConversationID: "alert-triage:" + job.ID},
	func(ctx context.Context, task *rudderstackai.Interaction) error {
		if _, err := claude.Messages.New(ctx, params); err != nil {
			return err
		}
		return rudderstackai.RunTask(ctx, "draft-status-update", rudderstackai.TaskOptions{}, draftUpdate) // a sub-agent, chained
	})
```

- The system is the actor: task events have no user (`anonymousId: "ai_system"`) and never inherit the person of the context or cookies. Put the customer in `AccountID` and the human the work is for in `OnBehalfOf`, and set a stable, scoped `ConversationID`.
- A task inside another interaction or task chains to it: `parent_interaction_id`, `conversation_id` and `trigger_source: agent` default from it. Each task sends its own `ai_response_received`.
- No prompt event, and no classification unless `Classify` is set. Details: [docs/configuration.md](docs/configuration.md#taskoptions).

## Feedback

```go
rudderstackai.TrackAction(ctx, rudderstackai.TrackActionOptions{
	ActionType:     rudderstackai.ActionFeedbackGiven,
	InteractionID:  interactionID,
	ConversationID: chatID,
	UserID:         userID,
	ActionDetails:  map[string]any{"feedback_type": "thumbs", "feedback_value": 1},
})
```

`TrackAction` sends `ai_user_action`, typically from your feedback endpoint. Action types: `feedback_given`, `feedback_retracted`, `copied_response`, `shared`, `reported`, `regenerated`, `accepted`, `rejected`, `edited`, `clicked_through`. `InteractionID` and `ConversationID` are required; `feedback_text` in `ActionDetails` is sent only with content capture on. When the browser sends its own copy, `WithServerFeedback(false)` turns the server copy off.

## Scores

```go
rudderstackai.TrackScore(ctx, rudderstackai.TrackScoreOptions{
	Score:          rudderstackai.Score{EvaluationName: "helpfulness", ScoreLabel: "pass", Evaluator: rudderstackai.EvaluatorHuman},
	InteractionID:  interactionID,
	ConversationID: chatID,
	UserID:         userID,
})
```

`TrackScore` sends `ai_interaction_scored` for a score from elsewhere (a human review, an offline judge). To score every successful answer as it happens, configure an evaluator with `WithEvaluator`; to label each answer with an intent and outcome, a classifier with `WithClassifier`. Both run after the answer is out and never block it: [docs/configuration.md](docs/configuration.md#classifier-and-evaluator).

## Events

| Event                                                   | When                                                                         |
| ------------------------------------------------------- | ---------------------------------------------------------------------------- |
| `ai_user_prompt_created`                                | A user-triggered interaction starts (sent when its first model call starts)  |
| `ai_response_received`                                  | The interaction ends: status, models, tokens, cost, latency, tool counts     |
| `ai_llm_response_received`                              | One per model call, only with `WithLLMCallEvents(true)`                      |
| `ai_interaction_classified`                             | After a classified answer, with `intent` and `outcome`                       |
| `ai_interaction_scored`                                 | One per score: from your evaluator, or `TrackScore`                          |
| `ai_user_action`                                        | `TrackAction`: feedback, copy, share, report, regenerate                     |
| Declared tool events, e.g. `ticket_created`             | A tool call of a tool listed in `WithEventRules` (or MCP `Options.EventRules`) |
| `mcp_client_seen`                                       | The first request of a (client, OAuth client, user) in a 24-hour window      |
| `mcp_tool_called`                                       | A `tools/call` completes, fails or is cancelled                              |
| `mcp_elicitation_completed`                             | The user answered (or did not answer) a question the server asked            |
| `mcp_resource_read`, `mcp_prompt_used`                  | A `resources/read` or `prompts/get` completes or fails                       |
| `mcp_capability_requested`                              | Opt-in: the agent called the injected `request_capability` tool              |
| `mcp_event_subscription_changed`, `mcp_event_delivered` | Experimental, opt-in: the MCP Events draft extension                         |

Every event carries `context.wrapperLibrary` (`name: "ai-sdk-go"`, `version`, `specVersion: "0.3.0"`, `integration`) beside analytics-go's own `context.library`. Every property and how it is derived: [docs/events.md](docs/events.md) and [docs/mcp.md](docs/mcp.md).

## MCP servers

```go
server := mcp.NewServer(&mcp.Implementation{Name: "workspace-mcp", Version: "1.4.0"}, nil)
mcp.AddTool(server, &mcp.Tool{Name: "create_audience"}, createAudience)

err := rudderstackai.WrapMCPServer(server, rudderstackai.Options{
	Analytics: client,
	Identify: func(ctx context.Context, tok *auth.TokenInfo) (rudderstackai.Identity, error) {
		if tok == nil {
			return rudderstackai.Identity{}, nil
		}
		workspace, _ := tok.Extra["workspace_id"].(string)
		return rudderstackai.Identity{AccountID: workspace}, nil
	},
	EventRules: rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{
		"create_audience": {Event: "audience_created"},
	}},
	RequestContext: rudderstackai.RequestContextOptions{Intents: []string{"debug_delivery", "build_audience"}},
})
// Serve as usual: server.Run(ctx, &mcp.StdioTransport{}) or mcp.NewStreamableHTTPHandler(...).
```

On mcp-go, `ruddermcpgo.WrapServer(s, ruddermcpgo.Options{...})` takes the same options, with `Identify(ctx)` reading your own authentication from the context; see [docs/mcp.md](docs/mcp.md#mcp-go).

Call `WrapMCPServer` once, before serving. It takes its own options (`Configure` does not apply) and adds a receiving and a sending middleware; requests and responses pass through unchanged except for the optional `request_context` argument (added to each tool's input schema and removed before your handler runs). Protocol revisions 2025-06-18 (initialize, sessions) and 2026-07-28 (stateless, client details in each request's `_meta`) are supported, on mcp-go from v1.0. Options, every property, identity, joining the calling agent through `_meta`, and limits: [docs/mcp.md](docs/mcp.md).

## Privacy

Sent by default: IDs, models, token counts, latency, cost, status, tool names. Never sent by default: prompt, answer and feedback text, score explanations, tool arguments and results, MCP resource contents and elicitation answers.

`WithCaptureContent(true)` (or `CaptureContent` per interaction) adds `prompt_text` (from `PromptText`), `response_text` (from `EndOptions.ResponseText`), `action_details.feedback_text` and score `explanation`. Captured text and the classifier's `intent_summary` are masked before they leave the process: emails, phone numbers, payment cards, IP addresses, US SSNs, IBANs, and API keys, tokens and passwords become `[email]`, `[phone]`, `[card]`, `[ip]`, `[ssn]`, `[iban]` and `[secret]`. `WithRedactHook` adds your own masking after the built-ins; a hook that fails drops the field. Names and postal addresses are not caught. MCP servers have their own `Options.CaptureContent`. Details: [docs/configuration.md](docs/configuration.md#privacy-and-masking).

## Reliability

- Tracking never breaks your code: the SDK recovers from its own panics, returns provider errors unchanged, and never changes requests or responses, except that streamed Chat Completions ask for usage (the extra usage chunk is hidden, and a host that rejects the parameter gets the request again as written).
- Without a RudderStack client, or when `Enqueue` fails, events are dropped with one warning per process (`log/slog`); set the slog level to debug to log everything tracking drops.
- Before a script, worker or serverless handler exits, call `rudderstackai.Flush(0)` (waits for pending classifications and scores) and then close the analytics-go client, which sends its queue. See [docs/configuration.md](docs/configuration.md#flush-and-shutdown).
- State is in memory, per process: classifications and scores still pending at exit are lost unless flushed, and MCP state is kept per server name: every server of one name in a process shares its client windows and counters, so give each distinct server a unique name; each replica keeps its own ([docs/mcp.md](docs/mcp.md#limits)).

## Examples

| Example                                               | Shows                                                                                          |
| ----------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| [`openai`](examples/openai)                           | An openai-go client with `rudderopenai.WithTracking` inside `RunInteraction`                   |
| [`anthropic`](examples/anthropic)                     | An anthropic-sdk-go client with `rudderanthropic.WithTracking`                                 |
| [`google-genai`](examples/google-genai)               | A genai client with `ruddergenai.HTTPClient`                                                   |
| [`bedrock-converse`](examples/bedrock-converse)       | A Bedrock Runtime client with `rudderbedrock.WithTracking` and `Converse`                      |
| [`background-agent`](examples/background-agent)       | `RunTask`: system-triggered work with a chained sub-task                                       |
| [`manual`](examples/manual)                           | `RecordCall` and `RecordToolCalls` for a model client without an integration                   |
| [`mcp-server`](examples/mcp-server)                   | stdio and streamable HTTP, `request_context`, a declared event, `request_capability`           |
| [`mcp-server-mcpgo`](examples/mcp-server-mcpgo)       | The same server on `github.com/mark3labs/mcp-go` with `ruddermcpgo.WrapServer`, 2025-06-18 and 2026-07-28 |
| [`mcp-stateless-http`](examples/mcp-stateless-http)   | Stateless streamable HTTP on 2026-07-28, confirmation through an `input_required` elicitation  |
| [`mcp-auth`](examples/mcp-auth)                       | A bearer token check and `Identify`: `userId`, `account_id`, `oauth_client_id`, `token_scopes` |

How to run them, with a local mock data plane or against your own: [examples/README.md](examples/README.md).

## Documentation

- [docs/events.md](docs/events.md): every AI interaction event and property, status, totals, `usage_breakdown`, `tool_breakdown`, cost.
- [docs/configuration.md](docs/configuration.md): `Configure` options, interaction and task options, context values, cookies, pricing, `trace_id`, classifier and evaluator, flushing.
- [docs/integrations.md](docs/integrations.md): per-integration setup and behaviour, streaming, retries, gateways, manual recording, limits.
- [docs/mcp.md](docs/mcp.md): MCP server tracking.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE.md)
