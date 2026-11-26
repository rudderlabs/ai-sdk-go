# RudderStack AI SDK for Go

## What it is

AI product events for Go services, sent through your RudderStack Go client using the
[AI events spec v0.4](spec/ai-events-v0.4.schema.json):

- Chat apps and background agents: each answer produces track events for the prompt, answer (models, tokens, cost, latency, status), feedback and scores, across all its model calls.
- MCP servers: each AI client request produces a track event with the client, tool, outcome and customer.

```
your app ─ provider client + integration ─ RunInteraction ─ analytics-go ─ RudderStack ─ warehouse, destinations
AI client ─ MCP ─ your MCP server (go-sdk or mcp-go) ─ wrapped ─ analytics-go ─ RudderStack
```

It puts AI usage next to the rest of your customer data and complements your tracing tool.
Content capture is off by default; see [privacy](docs/privacy.md).

Other language SDKs: [Node.js](https://github.com/rudderlabs/ai-sdk-node) and [Python](https://github.com/rudderlabs/ai-sdk-python).

## Install

```sh
go get github.com/rudderlabs/ai-sdk-go@v0.15.0 github.com/rudderlabs/analytics-go/v4@v4.5.1
go get github.com/rudderlabs/ai-sdk-go/rudderopenai@v0.6.0 # plus the integration module for each provider SDK you use
```

See [module dependencies and tested versions](docs/integrations.md#compatibility).

## Quickstart

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
	UserID:         userID,
	ConversationID: chatID,
	Feature:        "support_bot",
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

// Later, from your feedback endpoint:
rudderstackai.TrackAction(ctx, rudderstackai.TrackActionOptions{
	ActionType:     rudderstackai.ActionFeedbackGiven,
	InteractionID:  interactionID,
	ConversationID: chatID,
	UserID:         userID,
	ActionDetails: map[string]any{"feedback_type": "thumbs", "feedback_value": 1},
})
```

This sends `ai_user_prompt_created`, `ai_response_received` (models, tokens, latency, status, and cost once the model is [priced](docs/configuration.md#pricing)) and `ai_user_action`.
Pass the same context to every model call: a call made with a context that carries no interaction is reported as its own answer.
See [event fields](docs/events.md) and [feedback](docs/events.md#ai_user_action).

## Integrations

| Integration | Entry point | Page |
| --- | --- | --- |
| OpenAI | `openai.NewClient(rudderopenai.WithTracking())` | [OpenAI](docs/integrations.md#openai) |
| Anthropic | `anthropic.NewClient(rudderanthropic.WithTracking())` | [Anthropic](docs/integrations.md#anthropic) |
| Google Gen AI | `genai.ClientConfig{HTTPClient: ruddergenai.HTTPClient(nil)}` | [Google Gen AI](docs/integrations.md#google-gen-ai) |
| Amazon Bedrock | `bedrockruntime.NewFromConfig(cfg, rudderbedrock.WithTracking())` | [Amazon Bedrock](docs/integrations.md#amazon-bedrock) |
| Manual | `in.RecordCall(...)` | [Manual recording](docs/integrations.md#manual) |
| MCP (go-sdk) | `rudderstackai.WrapMCPServer` | [MCP setup](docs/mcp.md#setup) |
| MCP (mcp-go) | `ruddermcpgo.WrapServer` | [MCP setup](docs/mcp.md#setup) |
| OpenTelemetry | `rudderotel.NewSpanProcessor` | [Tracing](docs/tracing.md) |

## Documentation

| Page | What it covers |
| --- | --- |
| [Configuration](docs/configuration.md) | Options, interactions, tasks, identity, pricing, classification, flushing and troubleshooting |
| [Events](docs/events.md) | Event properties, status, totals, cost, tool events, feedback and scores |
| [Integrations](docs/integrations.md) | Provider setup, streaming, retries, manual recording, versions, limits and examples |
| [MCP](docs/mcp.md) | Server tracking, identity, request context, feedback, protocols and limits |
| [Tracing](docs/tracing.md) | Wrapper and manual spans, OpenTelemetry input, joins and delivery |
| [Privacy](docs/privacy.md) | Capture policy, serialization, custom properties, masking and limits |
| [Migration](docs/migration.md) | Upgrade notes and recording-based migration checks |
| [Client events](docs/client-events.md) | Browser and mobile event joins |
| [Performance](docs/performance.md) | Overhead, load and fuzz tests |

## Compatibility

Go 1.26.11 or later.
See [module dependencies, minimums and CI-tested versions](docs/integrations.md#compatibility), [provider limits](docs/integrations.md#go-platform-limits) and [MCP limits](docs/mcp.md#limits).

## Contributing, security and license

See [CONTRIBUTING.md](CONTRIBUTING.md) and [RELEASING.md](RELEASING.md). Report security issues as described in [SECURITY.md](SECURITY.md).

[MIT](LICENSE.md)
