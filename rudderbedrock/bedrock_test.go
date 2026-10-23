package rudderbedrock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

type recorder struct {
	mu   sync.Mutex
	msgs []analytics.Track
}

func (r *recorder) Enqueue(m analytics.Message) error {
	t, ok := m.(analytics.Track)
	if !ok {
		return fmt.Errorf("unexpected %T", m)
	}
	r.mu.Lock()
	r.msgs = append(r.msgs, t)
	r.mu.Unlock()
	return nil
}

func (r *recorder) Close() error { return nil }

// events flushes and returns every event, each checked against the spec schema.
func (r *recorder) events(t *testing.T) []analytics.Track {
	t.Helper()
	rudderstackai.Flush(context.Background())
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.msgs {
		raw, _ := json.Marshal(m.Properties)
		var props map[string]any
		_ = json.Unmarshal(raw, &props)
		if err := testkit.ValidateEvent(m.Event, props); err != nil {
			t.Errorf("schema: %v", err)
		}
	}
	return append([]analytics.Track(nil), r.msgs...)
}

func (r *recorder) named(t *testing.T, event string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range r.events(t) {
		if m.Event == event {
			raw, _ := json.Marshal(m.Properties)
			var props map[string]any
			_ = json.Unmarshal(raw, &props)
			out = append(out, props)
		}
	}
	return out
}

var llm *testkit.LLM

func TestMain(m *testing.M) {
	llm = testkit.StartLLM()
	code := m.Run()
	llm.Close()
	os.Exit(code)
}

func setup(t *testing.T) *recorder {
	t.Helper()
	hook.Reset()
	llm.Reset()
	rec := &recorder{}
	if err := rudderstackai.Configure(rudderstackai.WithAnalytics(rec), rudderstackai.WithLLMCallEvents(true), rudderstackai.WithToolBreakdown(true)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hook.Reset)
	return rec
}

func client(opts ...func(*bedrockruntime.Options)) *bedrockruntime.Client {
	if len(opts) == 0 {
		opts = []func(*bedrockruntime.Options){WithTracking()}
	}
	return bedrockruntime.New(bedrockruntime.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(llm.URL),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, nil
		}),
		Retryer: aws.NopRetryer{},
	}, opts...)
}

const model = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"

func userText(s string) types.Message {
	return types.Message{Role: types.ConversationRoleUser, Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: s}}}
}

func interaction(t *testing.T, fn func(ctx context.Context) error) error {
	t.Helper()
	return rudderstackai.RunInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "user_1", ConversationID: "conv_1"},
		func(ctx context.Context, _ *rudderstackai.Interaction) error { return fn(ctx) })
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func stream(events ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(events))
	for i, e := range events {
		out[i] = raw(e)
	}
	return out
}

func check(t *testing.T, props map[string]any, want map[string]any) {
	t.Helper()
	for k, v := range want {
		got, _ := json.Marshal(props[k])
		exp, _ := json.Marshal(v)
		if string(got) != string(exp) {
			t.Errorf("%s: got %s, want %s", k, got, exp)
		}
	}
}

func TestConverseToolUse(t *testing.T) {
	rec := setup(t)
	llm.ScriptBedrock("bedrock.converse", testkit.BedrockReply{RequestID: "req-1", Response: raw(`{
		"output": {"message": {"role": "assistant", "content": [{"text": "Checking."}, {"toolUse": {"toolUseId": "tu_1", "name": "get_weather", "input": {"city": "Paris"}}}]}},
		"stopReason": "tool_use",
		"usage": {"inputTokens": 30, "outputTokens": 8, "totalTokens": 53, "cacheReadInputTokens": 10, "cacheWriteInputTokens": 5}}`)})
	err := interaction(t, func(ctx context.Context) error {
		_, err := client().Converse(ctx, &bedrockruntime.ConverseInput{ModelId: aws.String(model), Messages: []types.Message{userText("Weather in Paris?")}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := rec.named(t, "ai_llm_response_received")
	if len(calls) != 1 {
		t.Fatalf("got %d calls", len(calls))
	}
	check(t, calls[0], map[string]any{
		"provider": "aws.bedrock", "model": model, "input_tokens": 45, "output_tokens": 8, "cached_input_tokens": 10,
		"cache_write_tokens": 5, "finish_reason": "tool_calls", "response_id": "req-1",
	})
	answer := rec.named(t, "ai_response_received")[0]
	check(t, answer, map[string]any{"tool_call_count": 1, "response_status": "success"})
}

func TestConverseToolResultLoop(t *testing.T) {
	rec := setup(t)
	llm.ScriptBedrock("bedrock.converse",
		testkit.BedrockReply{RequestID: "req-a", Response: raw(`{"output":{"message":{"role":"assistant","content":[{"toolUse":{"toolUseId":"tu_a","name":"lookup","input":{}}},{"toolUse":{"toolUseId":"tu_b","name":"lookup","input":{}}}]}},"stopReason":"tool_use","usage":{"inputTokens":1,"outputTokens":1}}`)},
		testkit.BedrockReply{RequestID: "req-b", Response: raw(`{"output":{"message":{"role":"assistant","content":[{"text":"done"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1}}`)},
	)
	err := interaction(t, func(ctx context.Context) error {
		c := client()
		if _, err := c.Converse(ctx, &bedrockruntime.ConverseInput{ModelId: aws.String(model), Messages: []types.Message{userText("hi")}}); err != nil {
			return err
		}
		results := types.Message{Role: types.ConversationRoleUser, Content: []types.ContentBlock{
			&types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{ToolUseId: aws.String("tu_a"), Status: types.ToolResultStatusError,
				Content: []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberText{Value: "down"}}}},
			&types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{ToolUseId: aws.String("tu_b"),
				Content: []types.ToolResultContentBlock{&types.ToolResultContentBlockMemberJson{Value: document.NewLazyDocument(map[string]any{"ok": true})}}}},
		}}
		_, err := c.Converse(ctx, &bedrockruntime.ConverseInput{ModelId: aws.String(model), Messages: []types.Message{userText("hi"), results}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	answer := rec.named(t, "ai_response_received")[0]
	check(t, answer, map[string]any{"finish_reason": "stop", "tool_call_count": 2})
	breakdown, _ := json.Marshal(answer["tool_breakdown"])
	if want := `"error_count":1`; !strings.Contains(string(breakdown), want) {
		t.Errorf("tool_breakdown %s lacks %s", breakdown, want)
	}
}

func TestToolResultValue(t *testing.T) {
	if v := toolResultValue([]types.ToolResultContentBlock{&types.ToolResultContentBlockMemberText{Value: `{"a":1}`}}); fmt.Sprint(v) != "map[a:1]" {
		t.Errorf("text JSON: %v", v)
	}
	if v := toolResultValue([]types.ToolResultContentBlock{&types.ToolResultContentBlockMemberJson{Value: document.NewLazyDocument([]any{1})}}); fmt.Sprint(v) != "[1]" {
		t.Errorf("json: %v", v)
	}
	two := toolResultValue([]types.ToolResultContentBlock{
		&types.ToolResultContentBlockMemberText{Value: "a"},
		&types.ToolResultContentBlockMemberJson{Value: document.NewLazyDocument(map[string]any{"b": 2})},
		&types.ToolResultContentBlockMemberImage{},
	})
	if b, _ := json.Marshal(two); string(b) != `[{"text":"a"},{"json":{"b":2}}]` {
		t.Errorf("list: %s", b)
	}
	if docValue(nil) != nil {
		t.Error("nil document")
	}
}

func TestFinishReasons(t *testing.T) {
	for in, want := range map[types.StopReason]string{
		"end_turn": "stop", "stop_sequence": "stop", "tool_use": "tool_calls", "max_tokens": "length",
		"guardrail_intervened": "content_filter", "content_filtered": "content_filter", "model_context_window_exceeded": "model_context_window_exceeded", "": "",
	} {
		if got := finishReason(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestMaxTokensIsIncomplete(t *testing.T) {
	rec := setup(t)
	llm.ScriptBedrock("bedrock.converse", testkit.BedrockReply{Response: raw(`{"output":{"message":{"role":"assistant","content":[{"text":"Once"}]}},"stopReason":"max_tokens","usage":{"inputTokens":5,"outputTokens":1,"totalTokens":6}}`)})
	_ = interaction(t, func(ctx context.Context) error {
		_, err := client().Converse(ctx, &bedrockruntime.ConverseInput{ModelId: aws.String("anthropic.claude-sonnet-4-5-20250929-v1:0"), Messages: []types.Message{userText("story")}})
		return err
	})
	check(t, rec.named(t, "ai_response_received")[0], map[string]any{
		"response_status": "incomplete", "error_type": "length", "finish_reason": "length", "total_input_tokens": 5, "total_output_tokens": 1,
	})
}

func TestConverseErrors(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
		want   string
	}{
		{"ThrottlingException", 429, "rate_limit"},
		{"ValidationException", 400, "invalid_request"},
		{"AccessDeniedException", 403, "auth_error"},
		{"InternalServerException", 500, "api_error"},
		{"ServiceUnavailableException", 503, "api_error"},
		{"ModelTimeoutException", 408, "timeout"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			rec := setup(t)
			llm.ScriptBedrock("bedrock.converse", testkit.BedrockReply{Error: &testkit.BedrockError{Code: tc.code, Status: tc.status, Message: "no"}})
			err := interaction(t, func(ctx context.Context) error {
				_, err := client().Converse(ctx, &bedrockruntime.ConverseInput{ModelId: aws.String(model), Messages: []types.Message{userText("hi")}})
				return err
			})
			if err == nil {
				t.Fatal("expected an error")
			}
			check(t, rec.named(t, "ai_llm_response_received")[0], map[string]any{"error_type": tc.want, "model": model})
		})
	}
}

func TestClassifyError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
		ok   bool
	}{
		{&smithy.GenericAPIError{Code: "throttlingException"}, "rate_limit", true},
		{&smithy.GenericAPIError{Code: "AccessDeniedException"}, "auth_error", true},
		{&smithy.GenericAPIError{Code: "ValidationException"}, "invalid_request", true},
		{&smithy.GenericAPIError{Code: "Other"}, "", false},
		{&smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 429}}, Err: errors.New("x")}, "rate_limit", true},
		{&smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 503}}, Err: errors.New("x")}, "", false},
		{errors.New("plain"), "", false},
	} {
		got, ok := classifyError(tc.err)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%v: got %q %v", tc.err, got, ok)
		}
	}
}

func TestOutsideInteraction(t *testing.T) {
	setup(t)
	own := &recorder{}
	if err := rudderstackai.Configure(rudderstackai.WithAnalytics(nil)); err != nil {
		t.Fatal(err)
	}
	llm.ScriptBedrock("bedrock.converse",
		testkit.BedrockReply{RequestID: "r1", Response: raw(`{"output":{"message":{"role":"assistant","content":[{"text":"hi"}]}},"stopReason":"end_turn"}`)},
		testkit.BedrockReply{Error: &testkit.BedrockError{Code: "ThrottlingException", Status: 429, Message: "slow"}},
	)
	c := client(WithTracking(WithAnalytics(own)))
	if _, err := c.Converse(context.Background(), &bedrockruntime.ConverseInput{ModelId: aws.String(model), Messages: []types.Message{userText("hi")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Converse(context.Background(), &bedrockruntime.ConverseInput{ModelId: aws.String(model), Messages: []types.Message{userText("hi")}}); err == nil {
		t.Fatal("expected an error")
	}
	answers := own.named(t, "ai_response_received")
	if len(answers) != 2 {
		t.Fatalf("got %d answers", len(answers))
	}
	// No usage reported: the call is unpriced and its tokens unknown.
	check(t, answers[0], map[string]any{"response_status": "success", "llm_call_count": 1})
	check(t, answers[1], map[string]any{"response_status": "error", "error_type": "rate_limit"})
}

func TestWrapTwiceAndUntracked(t *testing.T) {
	rec := setup(t)
	llm.ScriptBedrock("bedrock.converse", testkit.BedrockReply{RequestID: "r1", Response: raw(`{"output":{"message":{"role":"assistant","content":[{"text":"hi"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1}}`)})
	c := client(WithTracking(), WithTracking())
	err := interaction(t, func(ctx context.Context) error {
		if _, err := c.Converse(ctx, &bedrockruntime.ConverseInput{ModelId: aws.String(model), Messages: []types.Message{userText("hi")}}); err != nil {
			return err
		}
		// Other operations pass through (the mock has no route for them).
		_, _ = c.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{ModelId: aws.String(model), Body: []byte(`{}`)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rec.named(t, "ai_llm_response_received")); n != 1 {
		t.Fatalf("got %d calls", n)
	}
}

func TestValidationFailureRecorded(t *testing.T) {
	rec := setup(t)
	_ = interaction(t, func(ctx context.Context) error {
		_, err := client().Converse(ctx, &bedrockruntime.ConverseInput{})
		return err
	})
	if n := len(rec.named(t, "ai_llm_response_received")); n != 1 {
		t.Fatalf("got %d calls", n)
	}
}

func converseStream(ctx context.Context, c *bedrockruntime.Client) (*bedrockruntime.ConverseStreamEventStream, error) {
	out, err := c.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{ModelId: aws.String(model), Messages: []types.Message{userText("hi")}})
	if err != nil {
		return nil, err
	}
	return out.GetStream(), nil
}

func drain(es *bedrockruntime.ConverseStreamEventStream) error {
	defer es.Close()
	for range es.Events() {
	}
	return es.Err()
}

var fullStream = stream(
	`{"messageStart":{"role":"assistant"}}`,
	`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"Checking."}}}`,
	`{"contentBlockStop":{"contentBlockIndex":0}}`,
	`{"contentBlockStart":{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"tu_s","name":"get_weather"}}}}`,
	`{"contentBlockDelta":{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"city\":"}}}}`,
	`{"contentBlockDelta":{"contentBlockIndex":1,"delta":{"toolUse":{"input":"\"Paris\"}"}}}}`,
	`{"contentBlockStop":{"contentBlockIndex":1}}`,
	`{"messageStop":{"stopReason":"tool_use"}}`,
	`{"metadata":{"usage":{"inputTokens":30,"outputTokens":8,"totalTokens":53,"cacheReadInputTokens":10,"cacheWriteInputTokens":5},"metrics":{"latencyMs":400}}}`,
)

func TestStreamCompleted(t *testing.T) {
	rec := setup(t)
	llm.ScriptBedrock("bedrock.converse_stream", testkit.BedrockReply{RequestID: "req-s", Stream: fullStream})
	err := interaction(t, func(ctx context.Context) error {
		es, err := converseStream(ctx, client())
		if err != nil {
			return err
		}
		return drain(es)
	})
	if err != nil {
		t.Fatal(err)
	}
	check(t, rec.named(t, "ai_llm_response_received")[0], map[string]any{
		"input_tokens": 45, "output_tokens": 8, "cached_input_tokens": 10, "cache_write_tokens": 5,
		"finish_reason": "tool_calls", "response_id": "req-s",
	})
	answer := rec.named(t, "ai_response_received")[0]
	check(t, answer, map[string]any{"tool_call_count": 1, "response_status": "success"})
	if _, ok := answer["time_to_first_token_ms"]; !ok {
		t.Error("no time_to_first_token_ms")
	}
}

func TestStreamOutsideInteraction(t *testing.T) {
	setup(t)
	own := &recorder{}
	_ = rudderstackai.Configure(rudderstackai.WithAnalytics(nil))
	llm.ScriptBedrock("bedrock.converse_stream", testkit.BedrockReply{RequestID: "req-s", Stream: fullStream})
	es, err := converseStream(context.Background(), client(WithTracking(WithAnalytics(own))))
	if err != nil {
		t.Fatal(err)
	}
	if err := drain(es); err != nil {
		t.Fatal(err)
	}
	check(t, own.named(t, "ai_response_received")[0], map[string]any{"response_status": "success", "total_input_tokens": 45})
}

func TestStreamBreak(t *testing.T) {
	rec := setup(t)
	llm.ScriptBedrock("bedrock.converse_stream", testkit.BedrockReply{RequestID: "req-b", Stream: fullStream})
	_ = interaction(t, func(ctx context.Context) error {
		es, err := converseStream(ctx, client())
		if err != nil {
			return err
		}
		<-es.Events()
		return es.Close()
	})
	check(t, rec.named(t, "ai_llm_response_received")[0], map[string]any{"error_type": "cancelled", "response_id": "req-b"})
	check(t, rec.named(t, "ai_response_received")[0], map[string]any{"response_status": "cancelled"})
}

func TestStreamCloseAfterMessageStop(t *testing.T) {
	rec := setup(t)
	llm.ScriptBedrock("bedrock.converse_stream", testkit.BedrockReply{Stream: fullStream[:8]})
	_ = interaction(t, func(ctx context.Context) error {
		es, err := converseStream(ctx, client())
		if err != nil {
			return err
		}
		for ev := range es.Events() {
			if _, ok := ev.(*types.ConverseStreamOutputMemberMessageStop); ok {
				break
			}
		}
		return es.Close()
	})
	// As in Node.js a stream the app stops reading is cancelled, but the SDK decodes ahead of the
	// app: when it already reached the end of the stream, the call completed.
	got := rec.named(t, "ai_response_received")
	if len(got) != 1 {
		t.Fatalf("got %d ai_response_received", len(got))
	}
	if status := got[0]["response_status"]; status != "cancelled" && status != "success" {
		t.Errorf("response_status = %v", status)
	}
}

func TestStreamNoMessageStop(t *testing.T) {
	rec := setup(t)
	llm.ScriptBedrock("bedrock.converse_stream", testkit.BedrockReply{RequestID: "req-n", Stream: fullStream[:2]})
	_ = interaction(t, func(ctx context.Context) error {
		es, err := converseStream(ctx, client())
		if err != nil {
			return err
		}
		return drain(es)
	})
	check(t, rec.named(t, "ai_llm_response_received")[0], map[string]any{"error_type": "api_error", "response_id": "req-n"})
	check(t, rec.named(t, "ai_response_received")[0], map[string]any{"response_status": "error", "error_type": "api_error"})
}

func TestStreamException(t *testing.T) {
	rec := setup(t)
	llm.ScriptBedrock("bedrock.converse_stream", testkit.BedrockReply{RequestID: "req-e", Stream: fullStream[:5],
		StreamError: &testkit.BedrockError{Code: "ThrottlingException", Message: "slow"}})
	err := interaction(t, func(ctx context.Context) error {
		es, err := converseStream(ctx, client())
		if err != nil {
			return err
		}
		return drain(es)
	})
	var throttled *types.ThrottlingException
	if !errors.As(err, &throttled) {
		t.Fatalf("got %T %v", err, err)
	}
	check(t, rec.named(t, "ai_llm_response_received")[0], map[string]any{"error_type": "rate_limit", "response_id": "req-e"})
	check(t, rec.named(t, "ai_response_received")[0], map[string]any{"tool_call_count": 1, "response_status": "error"})
}

func TestStreamHTTPError(t *testing.T) {
	rec := setup(t)
	llm.ScriptBedrock("bedrock.converse_stream", testkit.BedrockReply{Error: &testkit.BedrockError{Code: "ValidationException", Status: 400, Message: "bad"}})
	_ = interaction(t, func(ctx context.Context) error {
		_, err := converseStream(ctx, client())
		return err
	})
	check(t, rec.named(t, "ai_llm_response_received")[0], map[string]any{"error_type": "invalid_request"})
}

func TestStreamDecodeErrors(t *testing.T) {
	for _, tc := range []struct {
		cancel bool
		want   string
	}{{false, "connection_error"}, {true, "cancelled"}} {
		rec := setup(t)
		ctx, cancel := context.WithCancel(context.Background())
		ctx, i := rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "user_1"})
		s := &streamCall{ctx: ctx, call: &kit.Call{Interaction: i, Provider: provider, Integration: integration, Model: model, StartedAt: time.Now()}}
		resp := &smithyhttp.Response{Response: &http.Response{Header: http.Header{"X-Amzn-Requestid": {"req-x"}}, Body: io.NopCloser(strings.NewReader(""))}}
		s.start(resp)
		if tc.cancel {
			cancel()
		}
		s.observe(nil, &net.OpError{Op: "read", Err: errors.New("connection reset")})
		// Events after the end are ignored.
		s.observe(&types.MessageStopEvent{StopReason: "end_turn"}, nil)
		_ = resp.Body.Close()
		cancel()
		i.End(rudderstackai.EndOptions{})
		check(t, rec.named(t, "ai_llm_response_received")[0], map[string]any{"error_type": tc.want, "response_id": "req-x"})
	}
}

func TestPlainMessages(t *testing.T) {
	got := plainMessages([]types.Message{{Role: types.ConversationRoleUser, Content: []types.ContentBlock{
		&types.ContentBlockMemberText{Value: "a"}, &types.ContentBlockMemberImage{},
	}}})
	if b, _ := json.Marshal(got); string(b) != `[{"content":[{"text":"a"}],"role":"user"}]` {
		t.Errorf("got %s", b)
	}
	if blockKey(nil) != "" || blockKey(aws.Int32(3)) != "3" {
		t.Error("blockKey")
	}
}
