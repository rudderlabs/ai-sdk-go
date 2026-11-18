package rudderopenai_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	ai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/serializerfixture"
	"github.com/rudderlabs/ai-sdk-go/rudderopenai"
)

// handoffValue is app content whose serializer reads its current text, so a later app write
// would show up in any telemetry that serialized it again.
type handoffValue struct {
	mu    sync.Mutex
	text  string
	calls int
}

func (v *handoffValue) MarshalJSON() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	return json.Marshal(v.text)
}

func (v *handoffValue) set(text string) {
	v.mu.Lock()
	v.text = text
	v.mu.Unlock()
}

// userTexts finds every user message text in decoded telemetry, wherever the sink nests it.
func userTexts(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		if x["type"] == "text" {
			if s, ok := x["content"].(string); ok {
				*out = append(*out, s)
			}
		}
		for _, child := range x {
			userTexts(child, out)
		}
	case []any:
		for _, child := range x {
			userTexts(child, out)
		}
	case string:
		var decoded any
		if strings.HasPrefix(x, "[") || strings.HasPrefix(x, "{") {
			if json.Unmarshal([]byte(x), &decoded) == nil {
				userTexts(decoded, out)
			}
		}
	}
}

// runRequestHandoff sends the serializer-bearing request through the real client and middleware,
// mutates the app value once the provider has the request, and checks what was captured.
func runRequestHandoff(t *testing.T, c serializerfixture.Case) {
	rec, llm := setup(t, ai.WithEmitSpans(c.Config.EmitSpans), ai.WithCaptureContent(c.Config.CaptureContent))
	descriptor := c.Setup["value"].(map[string]any)
	v := &handoffValue{text: descriptor["fields"].(map[string]any)["text"].(string)}
	stream, _ := c.Setup["stream"].(bool)
	async := c.Setup["mode"] == "async"
	raw, err := json.Marshal(map[string]any{"model": "gpt-4.1-mini", "stream": stream, "messages": []any{map[string]any{"role": "user", "content": v}}})
	if err != nil {
		t.Fatal(err)
	}
	if stream {
		llm.Script("openai.chat", reply(t, chatSSE))
	} else {
		llm.Script("openai.chat", reply(t, `{"json":{"id":"c1","model":"gpt-4.1-mini","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}}`))
	}
	sent, mutated := make(chan struct{}), make(chan struct{})
	var providerInput string
	// Innermost middleware: the provider boundary, after the tracking middleware ran.
	provider := func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(b))
		var sample struct {
			Messages []struct{ Content string }
		}
		if json.Unmarshal(b, &sample) == nil && len(sample.Messages) == 1 {
			providerInput = sample.Messages[0].Content
		}
		res, err := next(req)
		if async {
			close(sent)
			<-mutated
		} else {
			v.set(c.Setup["mutation"].(string))
		}
		return res, err
	}
	client := openai.NewClient(option.WithBaseURL(llm.URL+"/v1"), option.WithAPIKey("sk-test"), option.WithMaxRetries(0),
		option.WithUnsafeAllowHTTP(), rudderopenai.WithTracking(), option.WithMiddleware(provider))
	errs := make(chan error, 1)
	call := func(ctx context.Context) {
		if stream {
			s := client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{}, option.WithRequestBody("application/json", raw))
			errs <- consume(s, func() {})
			return
		}
		_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, option.WithRequestBody("application/json", raw))
		errs <- err
	}
	err = inInteraction(t, func(ctx context.Context) error {
		go call(ctx)
		if async {
			<-sent
			v.set(c.Setup["mutation"].(string))
			close(mutated)
		}
		return <-errs
	})
	if err != nil {
		t.Fatal(err)
	}
	events := rec.events(t)
	call1 := one(t, events, "ai_llm_response_received")
	var texts []string
	userTexts(call1["input_messages"], &texts)
	want := c.Expect["decoded"].(map[string]any)
	if len(texts) != 1 || texts[0] != want["call_input"] {
		t.Errorf("call input=%v want %v", texts, want["call_input"])
	}
	if c.Config.EmitSpans {
		span := one(t, events, "ai_otel_span")
		texts = otlpInputTexts(span)
		for _, key := range []string{"wrapper_input", "otlp_input"} {
			if !containsText(texts, want[key].(string)) {
				t.Errorf("%s: %v lacks %v", key, texts, want[key])
			}
		}
	}
	if providerInput != c.Expect["provider_input"] {
		t.Errorf("provider input=%q want %v", providerInput, c.Expect["provider_input"])
	}
	if got := v.calls; got != int(c.Expect["serializer_calls"].(map[string]any)["value"].(float64)) {
		t.Errorf("serializer calls=%d", got)
	}
	// event's fields are unexported: marshal the payloads themselves.
	payloads := make([]obj, len(events))
	for n, e := range events {
		payloads[n] = obj{"event": e.name, "properties": e.props}
	}
	all, _ := json.Marshal(payloads)
	for _, word := range c.Expect["never_emitted"].([]any) {
		if strings.Contains(string(all), word.(string)) {
			t.Errorf("emitted %v", word)
		}
	}
	if n := len(llm.Requests("openai.chat")); n != 1 {
		t.Errorf("provider requests=%d", n)
	}
}

// otlpInputTexts reads the text parts of the span's gen_ai.input.messages attribute.
func otlpInputTexts(span map[string]any) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if x["key"] == "content" {
				if s, ok := x["value"].(map[string]any)["stringValue"].(string); ok {
					out = append(out, s)
				}
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	root, _ := span["otlp"].(map[string]any)
	b, _ := json.Marshal(root)
	var decoded any
	_ = json.Unmarshal(b, &decoded)
	attrs := map[string]any{}
	var find func(v any)
	find = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if x["key"] == "gen_ai.input.messages" {
				attrs["input"] = x["value"]
			}
			for _, child := range x {
				find(child)
			}
		case []any:
			for _, child := range x {
				find(child)
			}
		}
	}
	find(decoded)
	walk(attrs["input"])
	return out
}

func containsText(texts []string, want string) bool {
	for _, text := range texts {
		if text == want {
			return true
		}
	}
	return false
}
