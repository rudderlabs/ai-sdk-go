package conformance

import (
	"context"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
	first "github.com/rudderlabs/ai-sdk-go/rudderopenai"  //nolint:staticcheck // Two aliases intentionally prove shared package identity.
	second "github.com/rudderlabs/ai-sdk-go/rudderopenai" //nolint:staticcheck // Two aliases intentionally prove shared package identity.
)

func TestGoImportAliasesShareWrapperRecognition(t *testing.T) {
	rec := &recorder{}
	configure(t, obj{"emit_spans": true, "emit_llm_calls": true}, rec)
	llm.Reset()
	llm.Script("openai.chat", testkit.Reply{JSON: []byte(`{"id":"alias-call","model":"gpt-4.1-mini","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)})
	client := openai.NewClient(option.WithBaseURL(llm.URL+"/v1"), option.WithAPIKey("test"), option.WithMaxRetries(0), option.WithUnsafeAllowHTTP(), first.WithTracking(), second.WithTracking())
	_, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{Model: "gpt-4.1-mini", Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	rudderstackai.Flush(context.Background())
	counts := map[string]int{}
	for _, event := range rec.all() {
		counts[event.Event]++
	}
	if counts["ai_otel_span"] != 1 || counts["ai_llm_response_received"] != 1 || len(llm.Requests("openai.chat")) != 1 {
		t.Fatalf("import aliases double-wrapped: %v", counts)
	}
}
