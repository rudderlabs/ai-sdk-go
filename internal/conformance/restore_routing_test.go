package conformance

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

// Exercise the actual provider clients, including their streaming paths, with two clients and
// identities whose paused objects deliberately reuse the same public interaction ID.
func TestRestoreProviderRoutingWithReusedID(t *testing.T) {
	selected := strings.Fields("openai_chat openai_chat_stream openai_responses openai_responses_stream anthropic_create anthropic_create_stream google_generate_plain google_generate_stream bedrock_converse_tool_use bedrock_converse_stream_tools")
	for _, sc := range loadFixture(t) {
		found := false
		for _, name := range selected {
			found = found || name == sc.Name
		}
		if !found {
			continue
		}
		call := restoreFirstCall(sc.Steps)
		if call == nil {
			t.Fatalf("%s has no call", sc.Name)
		}
		for _, order := range [][]int{{0}, {1}, {0, 1}, {1, 0}} {
			t.Run(fmt.Sprintf("%s/%v", sc.Name, order), func(t *testing.T) {
				clients := [2]*recorder{{}, {}}
				configure(t, obj{"capture_content": true, "emit_llm_calls": true, "emit_spans": true}, clients[0])
				llm.Reset()
				var saved [2]context.Context
				var original, target [2]*rudderstackai.Interaction
				var snapshots [2]rudderstackai.PausedInteraction
				for n, user := range []string{"tenant-a", "tenant-b"} {
					ctx := rudderstackai.WithValues(context.Background(), rudderstackai.ContextValues{Analytics: clients[n]})
					_, original[n] = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{InteractionID: "shared", UserID: user, ConversationID: user + "-conv", Classify: rudderstackai.Ptr(false)})
					saved[n] = rudderstackai.JoinInteraction(context.Background(), "shared", rudderstackai.JoinOptions{})
					target[n] = original[n]
				}
				for n := range original {
					snap, ok := original[n].Pause(rudderstackai.PauseOptions{})
					if !ok {
						t.Fatal("pause failed")
					}
					raw, err := json.Marshal(snap)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(raw, &snapshots[n]); err != nil {
						t.Fatal(err)
					}
				}
				for _, n := range order {
					ctx := rudderstackai.WithValues(context.Background(), rudderstackai.ContextValues{Analytics: clients[n]})
					_, target[n] = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{Resume: &snapshots[n], Classify: rudderstackai.Ptr(false)})
				}
				// Use both contexts after restoring either or both snapshots; a still-paused owner resumes
				// automatically. Unique response IDs keep provider dedup from hiding a routing error.
				for n := range saved {
					if rudderstackai.FromContext(saved[n]) != target[n] {
						t.Errorf("tenant %d context did not follow its own restore", n)
					}
					for route, replies := range sc.Replies {
						for _, rp := range replies {
							raw, err := json.Marshal(rp)
							if err != nil {
								t.Fatal(err)
							}
							raw = []byte(strings.ReplaceAll(string(raw), "Mock", fmt.Sprintf("customer-private-%d", n)))
							raw = []byte(strings.ReplaceAll(string(raw), "chatcmpl-1", fmt.Sprintf("chatcmpl-%d", n)))
							var reply testkit.Reply
							if err := json.Unmarshal(raw, &reply); err != nil {
								t.Fatal(err)
							}
							llm.Script(route, reply)
						}
					}
					if err := apis[str(call["api"])](saved[n], &run{t: t, llm: llm}, call); err != nil {
						t.Fatal(err)
					}
					_, span := rudderstackai.StartSpan(saved[n], rudderstackai.SpanOptions{Name: fmt.Sprintf("private-span-%d", n), Operation: "invoke_agent"})
					span.End(nil)
				}
				for n := range target {
					target[n].End(rudderstackai.EndOptions{})
					original[n].End(rudderstackai.EndOptions{})
				}
				rudderstackai.Flush(context.Background())
				for n, client := range clients {
					calls, spans, total := 0, 0, 0.0
					for _, event := range client.all() {
						if event.UserId != []string{"tenant-a", "tenant-b"}[n] || event.Properties["conversation_id"] != []string{"tenant-a-conv", "tenant-b-conv"}[n] {
							t.Errorf("client %d received foreign identity: %v", n, event)
						}
						if event.Event == "ai_llm_response_received" {
							calls++
						}
						if event.Event == "ai_otel_span" {
							spans++
						}
						if event.Event == "ai_response_received" {
							total += num(jsonRoundTrip(event.Properties).(obj)["llm_call_count"])
						}
						raw, _ := json.Marshal(event.Properties)
						if strings.Contains(string(raw), fmt.Sprintf("customer-private-%d", 1-n)) || strings.Contains(string(raw), fmt.Sprintf("private-span-%d", 1-n)) {
							t.Errorf("client %d received other tenant's content: %s", n, raw)
						}
						if err := testkit.ValidateEvent(event.Event, jsonRoundTrip(event.Properties).(obj)); err != nil {
							t.Error(err)
						}
					}
					if calls != 1 || total != 1 || spans != 2 {
						t.Errorf("client %d: calls=%d total=%v spans=%d; want 1,1,2", n, calls, total, spans)
					}
				}
			})
		}
	}
}

func restoreFirstCall(steps []obj) obj {
	for _, step := range steps {
		if step["op"] == "call" {
			return step
		}
		if nested := restoreFirstCall(objs0(step["steps"])); nested != nil {
			return nested
		}
	}
	return nil
}

// A real analytics client sends the private provider output to separate local data planes.
func TestRestoreRealClientRoutingWithReusedID(t *testing.T) {
	hook.Reset()
	t.Cleanup(hook.Reset)
	clients := [2]analytics.Client{}
	planes := [2]*recorder{{}, {}}
	for n := range clients {
		plane := planes[n]
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var batch struct {
				Batch []analytics.Track `json:"batch"`
			}
			var body io.Reader = r.Body
			if r.Header.Get("Content-Encoding") == "gzip" {
				compressed, err := gzip.NewReader(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				defer func() {
					if err := compressed.Close(); err != nil {
						t.Error(err)
					}
				}()
				body = compressed
			}
			if err := json.NewDecoder(body).Decode(&batch); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, event := range batch.Batch {
				if err := plane.Enqueue(event); err != nil {
					t.Error(err)
				}
			}
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(server.Close)
		client, err := analytics.NewWithConfig("test-key", analytics.Config{DataPlaneUrl: server.URL, BatchSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		clients[n] = client
		t.Cleanup(func() {
			if err := client.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err := rudderstackai.Configure(rudderstackai.WithAnalytics(clients[0]), rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true)); err != nil {
		t.Fatal(err)
	}
	var saved [2]context.Context
	var original [2]*rudderstackai.Interaction
	var snapshot rudderstackai.PausedInteraction
	for n, user := range []string{"tenant-a", "tenant-b"} {
		ctx := rudderstackai.WithValues(context.Background(), rudderstackai.ContextValues{Analytics: clients[n]})
		saved[n], original[n] = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{InteractionID: "job-42", UserID: user, ConversationID: user + "-conv", Classify: rudderstackai.Ptr(false)})
		snap, ok := original[n].Pause(rudderstackai.PauseOptions{})
		if !ok {
			t.Fatal("pause failed")
		}
		if n == 0 {
			raw, err := json.Marshal(snap)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &snapshot); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx := rudderstackai.WithValues(context.Background(), rudderstackai.ContextValues{Analytics: clients[0]})
	_, restored := rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{Resume: &snapshot, Classify: rudderstackai.Ptr(false)})
	llm.Reset()
	llm.Script("openai.chat", testkit.Reply{JSON: json.RawMessage(`{"id":"private-b","object":"chat.completion","model":"gpt-4.1-mini","choices":[{"index":0,"message":{"role":"assistant","content":"customer-private-B"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)})
	client, opts, ctx, cancel := openaiCall(saved[1], obj{"params": obj{"model": "gpt-4.1-mini", "messages": []any{obj{"role": "user", "content": "hello"}}}})
	defer cancel()
	response, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if response.Choices[0].Message.Content != "customer-private-B" {
		t.Fatal("provider output changed")
	}
	restored.End(rudderstackai.EndOptions{})
	for _, i := range original {
		i.End(rudderstackai.EndOptions{})
	}
	rudderstackai.Flush(context.Background())
	for n, plane := range planes {
		count := 0
		for _, event := range plane.all() {
			if event.Event != "ai_llm_response_received" {
				continue
			}
			count++
			if event.UserId != "tenant-b" || event.Properties["conversation_id"] != "tenant-b-conv" || event.Properties["output_text"] != "customer-private-B" {
				t.Errorf("wrong private call routing: %v", event)
			}
		}
		if count != n {
			t.Errorf("data plane %d received %d private calls, want %d", n, count, n)
		}
	}
}
