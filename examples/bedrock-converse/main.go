// Command bedrock-converse answers a weather question with Amazon Bedrock Runtime, tracked by
// rudderbedrock, inside a RudderStack interaction: Converse calls a get_weather tool, the app runs
// it, and ConverseStream streams the answer to the tool result.
//
//	AWS_REGION=us-east-1 AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \
//	RUDDERSTACK_WRITE_KEY=... RUDDERSTACK_DATA_PLANE_URL=... go run ./examples/bedrock-converse
//
// With --check it runs end to end against a mock data plane, with the model calls answered from
// spec/fixtures/cassettes/bedrock-converse.json, and exits non-zero when an expected event is
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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/rudderbedrock"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("bedrock-converse", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "run end to end against a mock data plane and exit")
	live := fs.Bool("live", false, "run the check with events sent to RUDDERSTACK_DATA_PLANE_URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *check || *live {
		return harness.Run(stderr, *live, runCheck)
	}
	cfg, err := envConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return harness.Once(stderr, func(sink *harness.Sink) error {
		res, err := runSink(sink, cfg)
		fmt.Fprintf(stderr, "answer: %s\n", res.Answer)
		return err
	})
}

// envConfig is the AWS configuration from AWS_REGION and the AWS_ACCESS_KEY_ID,
// AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN credentials. In an app, use config.LoadDefaultConfig
// (github.com/aws/aws-sdk-go-v2/config) for profiles, SSO and instance roles.
func envConfig() (aws.Config, error) {
	creds := aws.Credentials{
		AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken: os.Getenv("AWS_SESSION_TOKEN"),
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return aws.Config{}, errors.New("set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY (and AWS_REGION)")
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	return aws.Config{Region: region, Credentials: aws.NewCredentialsCache(aws.CredentialsProviderFunc(
		func(context.Context) (aws.Credentials, error) { return creds, nil }))}, nil
}

const (
	// model is a cross-region inference profile.
	model    = "us.anthropic.claude-haiku-4-5-20251001-v1:0"
	question = "What's the weather in Paris?"
)

// result is what the app returns to its client: the answer and the IDs feedback refers to.
type result struct {
	Answer, InteractionID, ConversationID string
}

// runSink runs the example with its events going to the sink and a Bedrock Runtime client from
// cfg.
func runSink(sink *harness.Sink, cfg aws.Config) (result, error) {
	rs, err := sink.NewClient()
	if err != nil {
		return result{}, err
	}
	res, err := weather(context.Background(), bedrockruntime.NewFromConfig(cfg, rudderbedrock.WithTracking()), rs)
	// Close flushes queued events.
	return res, errors.Join(err, rs.Close())
}

var toolConfig = &types.ToolConfiguration{Tools: []types.Tool{&types.ToolMemberToolSpec{Value: types.ToolSpecification{
	Name:        aws.String("get_weather"),
	Description: aws.String("Returns the current weather in a city."),
	InputSchema: &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(map[string]any{
		"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"},
	})},
}}}}

// getWeather is the app's tool.
func getWeather(city any) map[string]any {
	return map[string]any{"city": city, "conditions": "cloudy", "temperature_c": 18}
}

// weather prices the model and answers one question in an interaction, running the tool the
// model asks for.
func weather(ctx context.Context, client *bedrockruntime.Client, rs analytics.Client) (result, error) {
	// Example rates in USD per million tokens: use your own. Inference profile and vendor
	// prefixes are matched, so the base model name prices every region.
	if err := rudderstackai.RegisterPrice("claude-haiku-4-5", rudderstackai.ModelPrice{
		Provider: "aws.bedrock",
		TokenPrice: rudderstackai.TokenPrice{
			InputPerMtok: 1, OutputPerMtok: 5,
			CachedInputPerMtok: rudderstackai.Ptr(0.10), CacheWritePerMtok: rudderstackai.Ptr(1.25),
		},
	}); err != nil {
		return result{}, err
	}
	var res result
	err := rudderstackai.RunInteraction(ctx, rudderstackai.InteractionOptions{
		UserID: "user_42", ConversationID: "conv_weather_2", Feature: "travel_assistant", Surface: rudderstackai.SurfaceWeb,
		Analytics: rs,
	}, func(ctx context.Context, in *rudderstackai.Interaction) error {
		res.InteractionID, res.ConversationID = in.ID(), in.ConversationID()
		messages := []types.Message{{Role: types.ConversationRoleUser, Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: question}}}}
		inference := &types.InferenceConfiguration{MaxTokens: aws.Int32(200)}
		first, err := client.Converse(ctx, &bedrockruntime.ConverseInput{
			ModelId: aws.String(model), Messages: messages, ToolConfig: toolConfig, InferenceConfig: inference,
		})
		if err != nil {
			return err
		}
		out, _ := first.Output.(*types.ConverseOutputMemberMessage)
		if out == nil {
			return errors.New("no message in the Converse output")
		}
		messages = append(messages, out.Value)
		var results []types.ContentBlock
		for _, b := range out.Value.Content {
			use, ok := b.(*types.ContentBlockMemberToolUse)
			if !ok {
				continue
			}
			var args map[string]any
			if err := use.Value.Input.UnmarshalSmithyDocument(&args); err != nil {
				return err
			}
			results = append(results, &types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{
				ToolUseId: use.Value.ToolUseId,
				Content:   []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberJson{Value: document.NewLazyDocument(getWeather(args["city"]))}},
			}})
		}
		if len(results) == 0 {
			return nil
		}
		messages = append(messages, types.Message{Role: types.ConversationRoleUser, Content: results})
		stream, err := client.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
			ModelId: aws.String(model), Messages: messages, ToolConfig: toolConfig, InferenceConfig: inference,
		})
		if err != nil {
			return err
		}
		events := stream.GetStream()
		defer events.Close()
		for e := range events.Events() {
			if d, ok := e.(*types.ConverseStreamOutputMemberContentBlockDelta); ok {
				if t, ok := d.Value.Delta.(*types.ContentBlockDeltaMemberText); ok {
					res.Answer += t.Value
				}
			}
		}
		return events.Err()
	})
	return res, err
}
