package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"

	"github.com/rudderlabs/ai-sdk-go/rudderopenai"
)

func init() {
	apis["openai.chat"] = func(ctx context.Context, r *run, step obj) error {
		client, opts, ctx, cancel := openaiCall(ctx, step)
		defer cancel()
		if obj0(step["params"])["stream"] == true {
			return consumeStream(ctx, r, step, client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{}, opts...))
		}
		_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, opts...)
		return err
	}
	apis["openai.responses"] = func(ctx context.Context, r *run, step obj) error {
		client, opts, ctx, cancel := openaiCall(ctx, step)
		defer cancel()
		if obj0(step["params"])["stream"] == true {
			return consumeStream(ctx, r, step, client.Responses.NewStreaming(ctx, responses.ResponseNewParams{}, opts...))
		}
		_, err := client.Responses.New(ctx, responses.ResponseNewParams{}, opts...)
		return err
	}
}

// openaiCall builds the tracked client of one call step and the options sending params as is.
func openaiCall(ctx context.Context, step obj) (openai.Client, []option.RequestOption, context.Context, context.CancelFunc) {
	var tracking []rudderopenai.Option
	if p := str(step["provider"]); p != "" {
		tracking = append(tracking, rudderopenai.WithProvider(p))
	}
	clientOpts := []option.RequestOption{
		option.WithBaseURL(llm.URL + "/v1"), option.WithAPIKey("sk-test"), option.WithMaxRetries(0),
		option.WithUnsafeAllowHTTP(), // the mock listens on loopback HTTP
		rudderopenai.WithTracking(tracking...),
	}
	if step["wrap_twice"] == true {
		clientOpts = append(clientOpts, rudderopenai.WithTracking(tracking...))
	}
	client := openai.NewClient(clientOpts...)
	raw, _ := json.Marshal(step["params"])
	opts := []option.RequestOption{option.WithRequestBody("application/json", raw)}
	if ms := num(step["timeout_ms"]); ms > 0 {
		opts = append(opts, option.WithRequestTimeout(time.Duration(ms)*time.Millisecond))
	}
	ctx, cancel := context.WithCancel(ctx)
	if ms := num(step["abort_after_ms"]); ms > 0 {
		time.AfterFunc(time.Duration(ms)*time.Millisecond, cancel)
	}
	return client, opts, ctx, cancel
}

// consumeStream iterates a stream to the end, or stops after the first chunk; `during` steps run
// after the first chunk. An abort while consuming is swallowed unless the step expects an error.
func consumeStream[T any](ctx context.Context, r *run, step obj, stream *ssestream.Stream[T]) error {
	defer func() { _ = stream.Close() }()
	first := true
	for stream.Next() {
		if !first {
			continue
		}
		first = false
		if during, ok := step["during"].([]any); ok {
			if err := r.steps(ctx, objs(during)); err != nil {
				return err
			}
		}
		if step["consume"] == "first_chunk" {
			return nil
		}
	}
	err := stream.Err()
	if err != nil && step["expect_error"] != true && (errors.Is(err, context.Canceled) || ctx.Err() != nil) {
		return nil
	}
	return err
}
