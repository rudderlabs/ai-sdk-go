package rudderstackai

import (
	"context"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

type round7BlockingClient struct {
	sink
	entered, release chan struct{}
	event            string
}

func (c *round7BlockingClient) Enqueue(m analytics.Message) error {
	if e, ok := m.(analytics.Track); ok && e.Event == first(c.event, "ai_otel_span") {
		close(c.entered)
		<-c.release
	}
	return c.sink.Enqueue(m)
}
func (c *round7BlockingClient) Flush(context.Context) error { return nil }

func TestPendingSpanEnqueueRespectsFlushDeadline(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(testBoolName(shutdown), func(t *testing.T) {
			c := &round7BlockingClient{entered: make(chan struct{}), release: make(chan struct{})}
			setupAI(t, WithEmitSpans(true), WithAnalytics(c))
			ctx, owner := StartInteraction(context.Background(), InteractionOptions{UserID: "alice"})
			_, span := beginWrapperSpan(ctx, owner, time.Now())
			result := make(chan bool, 1)
			deadline, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			go func() {
				if shutdown {
					result <- Shutdown(deadline)
				} else {
					result <- Flush(deadline)
				}
			}()
			<-c.entered
			select {
			case ok := <-result:
				close(c.release)
				if ok {
					t.Error("blocked enqueue reported success")
				}
			case <-time.After(250 * time.Millisecond):
				close(c.release)
				<-result
				t.Error("pending span enqueue exceeded deadline")
			}
			// Wait until the asynchronous handoff settles before resetting SDK state.
			span.End(nil)
			Flush(context.Background())
		})
	}
}

type round7ContentMarshaler struct{ Value string }

func (round7ContentMarshaler) MarshalJSON() ([]byte, error) { panic("content getter") }

func TestRecordCallContainsThrowingContentMarshalers(t *testing.T) {
	for _, emit := range []bool{false, true} {
		t.Run(testBoolName(emit), func(t *testing.T) {
			r := setupAI(t, WithEmitSpans(emit), WithCaptureContent(true), WithLLMCallEvents(true))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "alice"})
			if !i.RecordCall(LLMCall{Provider: "openai", Model: "m", InputMessages: []GenAIMessage{{Role: "user", Parts: []GenAIPart{{"type": "tool_call", "name": "tool", "arguments": round7ContentMarshaler{Value: "safe"}}}}}}) {
				t.Fatal("plain content rejected")
			}
			i.End(EndOptions{})
			if len(r.named("ai_llm_response_received")) != 1 {
				t.Fatal("call lost")
			}
		})
	}
}

func TestRecordCallContainsPanickingContentCallback(t *testing.T) {
	for _, emit := range []bool{false, true} {
		t.Run(testBoolName(emit), func(t *testing.T) {
			setupAI(t, WithEmitSpans(emit), WithCaptureContent(true), WithLLMCallEvents(true), WithRedactHook(func(string) (string, error) { panic("content callback") }))
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "alice"})
			i.RecordCall(LLMCall{Provider: "openai", Model: "m", InputMessages: []GenAIMessage{textMessage("user", "secret")}})
			i.End(EndOptions{})
		})
	}
}

func TestInvocationCallbackEndsOwnerBeforeSpanAllocation(t *testing.T) {
	r := setupAI(t, WithEmitSpans(true))
	ctx, owner := StartInteraction(context.Background(), InteractionOptions{UserID: "alice"})
	previous := hook.Tracing.ParentContext
	hook.Tracing.ParentContext = func(context.Context) (string, string, string) { owner.End(EndOptions{}); return "", "", "" }
	defer func() { hook.Tracing.ParentContext = previous }()
	_, span := beginWrapperSpan(ctx, owner, time.Now())
	if span == nil {
		t.Fatal("span disabled")
	}
	owner.recordWrapperCall(LLMCall{Provider: "openai", Model: "m", InputTokens: 3, OutputTokens: 2}, span, 200)
	Flush(context.Background())
	spans := r.named("ai_otel_span")
	if len(spans) != 1 || spans[0].Properties["status_code"] != "ok" || spans[0].Properties["interaction_id"] != owner.ID() || spans[0].UserId != "alice" {
		t.Fatalf("allocation callback lost owner or outcome: %v", spans)
	}
}

func TestShutdownDeadlineBoundsInteractionEnqueue(t *testing.T) {
	c := &round7BlockingClient{entered: make(chan struct{}), release: make(chan struct{}), event: "ai_response_received"}
	setupAI(t, WithEmitSpans(true), WithAnalytics(c))
	_, owner := StartInteraction(context.Background(), InteractionOptions{UserID: "alice"})
	deadline, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result := make(chan bool, 1)
	go func() { result <- Shutdown(deadline) }()
	<-c.entered
	select {
	case ok := <-result:
		close(c.release)
		if ok {
			t.Error("blocked shutdown reported success")
		}
	case <-time.After(250 * time.Millisecond):
		close(c.release)
		<-result
		t.Error("interaction enqueue exceeded shutdown deadline")
	}
	owner.End(EndOptions{})
	Flush(context.Background())
}
