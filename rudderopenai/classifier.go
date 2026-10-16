package rudderopenai

import (
	"context"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

type classifier struct {
	client openai.Client
	model  string
}

// ClassifierClient returns a [rudderstackai.ClassifierClient] that labels interactions with model
// through your own client (a Chat Completions call in JSON mode, without retries). Its calls are
// never tracked, also when the client has [WithTracking].
func ClassifierClient(client openai.Client, model string) rudderstackai.ClassifierClient {
	return &classifier{client: client, model: model}
}

// Classify sends the classifier prompt.
func (c *classifier) Classify(ctx context.Context, system, user string) (rudderstackai.ClassifierReply, error) {
	r, err := c.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: c.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(system),
			openai.UserMessage(user),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{OfJSONObject: &shared.ResponseFormatJSONObjectParam{}},
		// JSON mode can emit long whitespace runs: cap the reply like the Anthropic call.
		MaxCompletionTokens: openai.Int(300),
	}, option.WithMaxRetries(0))
	if err != nil {
		return rudderstackai.ClassifierReply{}, err
	}
	reply := rudderstackai.ClassifierReply{Model: firstOf(r.Model, c.model), Provider: "openai"}
	if len(r.Choices) > 0 {
		reply.Text = r.Choices[0].Message.Content
	}
	if r.JSON.Usage.Valid() {
		reply.UsageKnown = true
		reply.InputTokens, reply.OutputTokens = int(r.Usage.PromptTokens), int(r.Usage.CompletionTokens)
		reply.CachedInputTokens = int(r.Usage.PromptTokensDetails.CachedTokens)
	}
	return reply, nil
}
