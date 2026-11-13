package rudderopenai_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/openai/openai-go/v3"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

func TestRawResponseConsumedUnderBobKeepsAliceIdentity(t *testing.T) {
	rec, llm := setup(t, rudderstackai.WithEmitSpans(true))
	llm.Script("openai.chat", reply(t, retriedAnswer))
	client := newClient(llm)
	ctx, alice := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{AnonymousID: "alice", ConversationID: "alice-conversation"})
	var raw *http.Response
	if err := client.Post(ctx, "chat/completions", []byte(chatBody), &raw); err != nil {
		t.Fatal(err)
	}
	alice.End(rudderstackai.EndOptions{})
	err := rudderstackai.RunInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "bob"}, func(context.Context, *rudderstackai.Interaction) error {
		defer func() { _ = raw.Body.Close() }()
		b, err := io.ReadAll(raw.Body)
		if len(b) == 0 {
			t.Fatal("raw response was consumed before Bob read it")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	span := one(t, rec.events(t), "ai_otel_span")
	if span["interaction_id"] != alice.ID() || span["conversation_id"] != "alice-conversation" {
		t.Fatalf("span owner lost: %v", span)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, m := range rec.msgs {
		if m.Event == "ai_otel_span" && (m.UserId != "" || m.AnonymousId != "alice") {
			t.Fatalf("mixed identity: user=%q anonymous=%q", m.UserId, m.AnonymousId)
		}
	}
}

func TestOpenAIInvalidJSONKeepsProviderErrorAndProductEvents(t *testing.T) {
	for _, emit := range []bool{false, true} {
		t.Run(fmt.Sprint(emit), func(t *testing.T) {
			rec, llm := setup(t, rudderstackai.WithEmitSpans(emit))
			llm.Script("openai.chat", testkit.Reply{JSON: []byte(`{"broken":`)})
			client := newClient(llm)
			ctx, owner := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "alice"})
			_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, body(chatBody))
			if err == nil {
				t.Fatal("invalid JSON did not reject")
			}
			owner.End(rudderstackai.EndOptions{})
			events := rec.events(t)
			if p := one(t, events, "ai_response_received"); num(p["llm_call_count"]) != 0 {
				t.Fatalf("parse failure changed product calls: %v", p)
			}
			spans := 0
			for _, event := range events {
				if event.name == "ai_llm_response_received" {
					t.Fatal("parse failure invented product call")
				}
				if event.name == "ai_otel_span" {
					spans++
					if event.props["status_code"] != "error" {
						t.Fatal("parse failure span succeeded")
					}
				}
			}
			want := 0
			if emit {
				want = 1
			}
			if spans != want {
				t.Fatalf("spans=%d want %d", spans, want)
			}
		})
	}
}
