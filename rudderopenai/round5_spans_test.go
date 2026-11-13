package rudderopenai_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/rudderopenai"
)

func TestDoubleWrappedResponseBodyIntoEmitsOneSpan(t *testing.T) {
	for _, inside := range []bool{false, true} {
		for _, raw := range []bool{false, true} {
			t.Run(fmt.Sprintf("inside=%v/raw=%v", inside, raw), func(t *testing.T) {
				rec, llm := setup(t, rudderstackai.WithEmitSpans(true))
				llm.Script("openai.chat", reply(t, retriedAnswer))
				client := openai.NewClient(option.WithBaseURL(llm.URL+"/v1"), option.WithAPIKey("sk-test"), option.WithMaxRetries(0), option.WithUnsafeAllowHTTP(), rudderopenai.WithTracking(), rudderopenai.WithTracking())
				ctx := context.Background()
				var owner *rudderstackai.Interaction
				if inside {
					ctx, owner = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "u"})
					defer owner.End(rudderstackai.EndOptions{})
				}
				var response *http.Response
				if raw {
					if err := client.Post(ctx, "chat/completions", []byte(chatBody), &response); err != nil {
						t.Fatal(err)
					}
				} else {
					_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body(chatBody), option.WithResponseBodyInto(&response))
					if err != nil {
						t.Fatal(err)
					}
				}
				if response == nil {
					t.Fatal("response manager lost response")
				}
				if raw {
					if b, err := io.ReadAll(response.Body); err != nil || len(b) == 0 {
						t.Fatal("raw bytes lost")
					}
				}
				_ = response.Body.Close()
				if owner != nil {
					owner.End(rudderstackai.EndOptions{})
				}
				one(t, rec.events(t), "ai_otel_span")
				if len(llm.Requests("openai.chat")) != 1 {
					t.Fatal("double wrapping duplicated request")
				}
			})
		}
	}
}
