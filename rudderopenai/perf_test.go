//go:build unix

package rudderopenai_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/rudderopenai"
)

// Per-interaction overhead of the OpenAI wrapper: go test -run '^$' -bench . -benchmem ./rudderopenai
// The same scenario IDs run in the Node.js and Python SDKs; make bench reports all of them.

// nullAnalytics drops every event, so only the SDK's own work is measured.
type nullAnalytics struct{}

func (nullAnalytics) Enqueue(analytics.Message) error { return nil }
func (nullAnalytics) Close() error                    { return nil }

const benchModel = "gpt-4.1-mini-2025-04-14"

// longPrompt is a 4 KB user prompt with an email address and a phone number for masking to find.
var (
	longPrompt = strings.Repeat("Where is my order? ", 100) + "Reach me at jane.doe@example.com or +1 415 555 0100. " +
		strings.Repeat("Thanks. ", 260)
	longAnswer = strings.Repeat("Your order ships tomorrow. ", 150)
)

func completion(content string) []byte {
	b, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-bench", "object": "chat.completion", "created": 1760400000, "model": benchModel,
		"choices": []any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "content": content, "refusal": nil},
			"finish_reason": "stop", "logprobs": nil,
		}},
		"usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 5, "total_tokens": 17},
	})
	return b
}

var toolCall = []byte(`{"id":"chatcmpl-bench-tool","object":"chat.completion","created":1760400000,"model":"` + benchModel +
	`","choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":null,"tool_calls":[{"id":"call_bench",` +
	`"type":"function","function":{"name":"lookup_order","arguments":"{\"order\":\"A-1\"}"}}]},"finish_reason":"tool_calls",` +
	`"logprobs":null}],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28}}`)

// chatStreamEvents is one SSE event per element: a role chunk, n content chunks, the finish and
// usage chunks, [DONE].
func chatStreamEvents(n int) [][]byte {
	chunk := func(rest string) []byte {
		return []byte(`data: {"id":"chatcmpl-bench","object":"chat.completion.chunk","created":1760400000,"model":"` +
			benchModel + `",` + rest + "}\n\n")
	}
	events := [][]byte{chunk(`"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]`)}
	for range n {
		events = append(events, chunk(`"choices":[{"index":0,"delta":{"content":"token "},"finish_reason":null}]`))
	}
	return append(events,
		chunk(`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`),
		chunk(fmt.Sprintf(`"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":%d,"total_tokens":%d}`, n, 12+n)),
		[]byte("data: [DONE]\n\n"))
}

// chunks is a response body that hands out one piece per Read, as a network stream would.
type chunks struct{ pieces [][]byte }

func (c *chunks) Read(p []byte) (int, error) {
	for len(c.pieces) > 0 && len(c.pieces[0]) == 0 {
		c.pieces = c.pieces[1:]
	}
	if len(c.pieces) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.pieces[0])
	c.pieces[0] = c.pieces[0][n:]
	return n, nil
}

func (*chunks) Close() error { return nil }

// fakeOpenAI answers in process: a tool call while tools are offered and no tool result is in the
// conversation yet, else text; streamed when the request asks for it; HTTP 429 every failEvery-th
// request.
type fakeOpenAI struct {
	text      []byte
	stream    [][]byte
	failEvery int64
	requests  atomic.Int64
}

func (f *fakeOpenAI) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if n := f.requests.Add(1); f.failEvery > 0 && n%f.failEvery == 0 {
		return response(429, "application/json", [][]byte{[]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`)}), nil
	}
	var p struct {
		Stream   bool  `json:"stream"`
		Tools    []any `json:"tools"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &p)
	if p.Stream {
		return response(200, "text/event-stream", f.stream), nil
	}
	if len(p.Tools) > 0 && !slices.ContainsFunc(p.Messages, func(m struct {
		Role string `json:"role"`
	}) bool {
		return m.Role == "tool"
	}) {
		return response(200, "application/json", [][]byte{toolCall}), nil
	}
	return response(200, "application/json", [][]byte{f.text}), nil
}

func response(status int, contentType string, body [][]byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       &chunks{pieces: slices.Clone(body)},
	}
}

func newFakeOpenAI(streamChunks int, long bool, failEvery int64) *fakeOpenAI {
	text := completion("Your order ships tomorrow.")
	if long {
		text = completion(longAnswer)
	}
	return &fakeOpenAI{text: text, stream: chatStreamEvents(streamChunks), failEvery: failEvery}
}

func fakeClient(rt http.RoundTripper, opts ...option.RequestOption) openai.Client {
	return openai.NewClient(append([]option.RequestOption{
		option.WithBaseURL("https://bench.invalid/v1"), option.WithAPIKey("bench"), option.WithMaxRetries(0),
		option.WithHTTPClient(&http.Client{Transport: rt}),
	}, opts...)...)
}

// measure runs op b.N times and reports, beside ns/op, B/op and allocs/op: the p50 and p99 of one
// op, process CPU per op and GC runs per 10k ops.
func measure(b *testing.B, op func(context.Context) error) {
	ctx := context.Background()
	b.ReportAllocs()
	samples := make([]time.Duration, b.N)
	var ru0, ru1 syscall.Rusage
	var ms0, ms1 runtime.MemStats
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
	runtime.ReadMemStats(&ms0)
	b.ResetTimer()
	for i := range b.N {
		start := time.Now()
		if err := op(ctx); err != nil {
			b.Fatal(err)
		}
		samples[i] = time.Since(start)
	}
	b.StopTimer()
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
	runtime.ReadMemStats(&ms1)
	slices.Sort(samples)
	cpu := func(ru syscall.Rusage) time.Duration {
		return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	}
	b.ReportMetric(float64(samples[len(samples)/2]), "p50-ns")
	b.ReportMetric(float64(samples[min(len(samples)-1, len(samples)*99/100)]), "p99-ns")
	b.ReportMetric(float64(cpu(ru1)-cpu(ru0))/float64(b.N), "cpu-ns/op")
	b.ReportMetric(float64(ms1.NumGC-ms0.NumGC)*10_000/float64(b.N), "gc/10k-op")
}

func settings(b *testing.B, opts ...rudderstackai.ConfigOption) {
	b.Helper()
	base := []rudderstackai.ConfigOption{
		rudderstackai.WithAnalytics(nullAnalytics{}), rudderstackai.WithCaptureContent(false),
		rudderstackai.WithLLMCallEvents(false), rudderstackai.WithEmitSpans(false),
		// A warning printed mid-run would break the benchmark output.
		rudderstackai.WithOnError(func(error, rudderstackai.ErrorInfo) {}),
	}
	if err := rudderstackai.Configure(append(base, opts...)...); err != nil {
		b.Fatal(err)
	}
}

const streamChunks = 100

var benchUser = rudderstackai.InteractionOptions{UserID: "bench-user", ConversationID: "bench-conversation"}

func chatParams(prompt string) openai.ChatCompletionNewParams {
	return openai.ChatCompletionNewParams{
		Model:    "gpt-4.1-mini",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
	}
}

func interaction(ctx context.Context, o rudderstackai.InteractionOptions, fn func(context.Context) error) error {
	return rudderstackai.RunInteraction(ctx, o, func(ctx context.Context, _ *rudderstackai.Interaction) error {
		return fn(ctx)
	})
}

// pair benchmarks the same call without the SDK (baseline) and with it (sdk).
func pair(b *testing.B, id string, call func(context.Context, openai.Client) error, long bool, opts ...rudderstackai.ConfigOption) {
	start := benchUser
	if long {
		start.PromptText = longPrompt
	}
	raw := fakeClient(newFakeOpenAI(streamChunks, long, 0))
	wrapped := fakeClient(newFakeOpenAI(streamChunks, long, 0), rudderopenai.WithTracking())
	b.Run(id+"/baseline", func(b *testing.B) {
		settings(b, opts...)
		measure(b, func(ctx context.Context) error { return call(ctx, raw) })
	})
	b.Run(id+"/sdk", func(b *testing.B) {
		settings(b, opts...)
		measure(b, func(ctx context.Context) error {
			return interaction(ctx, start, func(ctx context.Context) error { return call(ctx, wrapped) })
		})
	})
}

func create(prompt string) func(context.Context, openai.Client) error {
	params := chatParams(prompt)
	return func(ctx context.Context, c openai.Client) error {
		_, err := c.Chat.Completions.New(ctx, params)
		return err
	}
}

func consumeStream(ctx context.Context, c openai.Client) error {
	stream := c.Chat.Completions.NewStreaming(ctx, chatParams("Where is my order?"))
	for stream.Next() {
		_ = stream.Current()
	}
	return stream.Err()
}

func BenchmarkOverhead(b *testing.B) {
	short := create("Where is my order?")
	pair(b, "interaction/openai", short, false)
	pair(b, "interaction/openai-stream", consumeStream, false)
	pair(b, "interaction/openai+content", create(longPrompt), true,
		rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true))
	pair(b, "interaction/openai+spans", short, false, rudderstackai.WithEmitSpans(true))
	pair(b, "interaction/openai-stream+spans", consumeStream, false, rudderstackai.WithEmitSpans(true))
}

// TestFakeOpenAIAnswers keeps the benchmark honest: the fake answers as the scenarios expect.
func TestFakeOpenAIAnswers(t *testing.T) {
	c := fakeClient(newFakeOpenAI(3, false, 0))
	got, err := c.Chat.Completions.New(context.Background(), chatParams("hi"))
	if err != nil || got.Choices[0].Message.Content != "Your order ships tomorrow." {
		t.Fatalf("chat: %v %v", got, err)
	}
	var text bytes.Buffer
	stream := c.Chat.Completions.NewStreaming(context.Background(), chatParams("hi"))
	for stream.Next() {
		if cs := stream.Current().Choices; len(cs) > 0 {
			text.WriteString(cs[0].Delta.Content)
		}
	}
	if stream.Err() != nil || text.String() != "token token token " {
		t.Fatalf("stream: %q %v", text.String(), stream.Err())
	}
}
