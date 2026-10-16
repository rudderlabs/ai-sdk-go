package rudderanthropic

import (
	"context"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

// ClassifierClient returns a [rudderstackai.ClassifierClient] that sends the classifier prompt to
// model through client's Messages API. Its calls are never tracked, even through a tracked client.
func ClassifierClient(client anthropic.Client, model string) rudderstackai.ClassifierClient {
	return &classifier{client: client, model: model}
}

type classifier struct {
	client anthropic.Client
	model  string
}

func (c *classifier) Classify(ctx context.Context, system, user string) (rudderstackai.ClassifierReply, error) {
	msg, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: 300,
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
	}, option.WithMaxRetries(0))
	if err != nil {
		return rudderstackai.ClassifierReply{}, err
	}
	reply := rudderstackai.ClassifierReply{Model: string(msg.Model), Provider: "anthropic"}
	if reply.Model == "" {
		reply.Model = c.model
	}
	for _, b := range msg.Content {
		if b.Type == "text" {
			reply.Text += b.Text
		}
	}
	if msg.JSON.Usage.Valid() {
		u := msg.Usage
		reply.UsageKnown = true
		reply.CachedInputTokens, reply.CacheWriteTokens = int(u.CacheReadInputTokens), int(u.CacheCreationInputTokens)
		reply.InputTokens = int(u.InputTokens) + reply.CachedInputTokens + reply.CacheWriteTokens
		reply.OutputTokens = int(u.OutputTokens)
	}
	return reply, nil
}
