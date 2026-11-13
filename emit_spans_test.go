package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

func TestEmitSpansAdmissionAndDuplicate(t *testing.T) {
	freshState(t)
	r := &recorder{}
	if err := Configure(WithAnalytics(r), WithEmitSpans(true), WithLLMCallEvents(true)); err != nil {
		t.Fatal(err)
	}

	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "user", Classify: Ptr(false)})
	for range 2 {
		_, s := beginWrapperSpan(ctx, i, time.Now())
		i.recordWrapperCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "same", InputTokens: 1, OutputTokens: 2}, s, 200)
	}
	i.End(EndOptions{})
	var spans, calls []analytics.Track
	for _, e := range r.all() {
		if e.Event == "ai_otel_span" {
			spans = append(spans, e)
		}
		if e.Event == "ai_llm_response_received" {
			calls = append(calls, e)
		}
	}
	if len(spans) != 2 || len(calls) != 1 {
		t.Fatalf("spans=%d calls=%d", len(spans), len(calls))
	}
	if calls[0].Properties["span_id"] != spans[0].Properties["span_id"] {
		t.Fatal("deduplicated event must keep first invocation's ID")
	}
}

func TestManualSpansNestingErrorsAndDisabled(t *testing.T) {
	freshState(t)
	r := &recorder{}
	if err := Configure(WithAnalytics(r), WithEmitSpans(true), WithCaptureContent(true)); err != nil {
		t.Fatal(err)
	}
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "user", Classify: Ptr(false)})
	ctx, parent := StartSpan(ctx, SpanOptions{Name: "agent", Operation: "invoke_agent"})
	_, child := StartSpan(ctx, SpanOptions{Name: "search", Operation: "retrieval", Attributes: map[string]any{"input.value": "a@example.com"}})
	child.SetAttributes(map[string]any{"custom": true})
	err := errors.New("failed a@example.com")
	child.End(err)
	child.End(nil)
	parent.End(nil)
	i.End(EndOptions{})
	spans := []analytics.Track{}
	for _, e := range r.all() {
		if e.Event == "ai_otel_span" {
			spans = append(spans, e)
		}
	}
	if len(spans) != 2 {
		t.Fatalf("spans=%d", len(spans))
	}
	if spans[0].Properties["parent_span_id"] != parent.SpanID || child.TraceID != parent.TraceID {
		t.Fatal("nesting lost")
	}
	if spans[0].Properties["status_message"] != "failed [email]" {
		t.Fatal(spans[0].Properties)
	}
	if err := Configure(WithEmitSpans(false)); err != nil {
		t.Fatal(err)
	}
	same, s := StartSpan(ctx, SpanOptions{Name: "off", Operation: "retrieval"})
	s.End(err)
	if same != ctx || s.SpanID != "" {
		t.Fatal("disabled span is not a no-op")
	}
}

func TestPortableContentContainers(t *testing.T) {
	type payload struct {
		Text  string `json:"text"`
		Bytes []byte `json:"bytes"`
	}
	cycle := map[string]any{"text": "keep"}
	cycle["self"] = cycle
	input := map[any]any{1: "one", true: []any{"keep", []byte{1}}, 2.5: payload{"keep", []byte{2}}, struct{}{}: "drop", "cycle": cycle, "numeric": map[int]int{1: 7, 2: 8}, "list": []any{func() {}, "last"}}
	got := stripBinary(input)
	want := map[string]any{"1": "one", "true": []any{"keep"}, "2.5": map[string]any{"text": "keep"}, "cycle": map[string]any{"text": "keep"}, "numeric": map[string]any{"1": int64(7), "2": int64(8)}, "list": []any{nil, "last"}}
	if !reflect.DeepEqual(got, want) {
		b, _ := json.Marshal(got)
		t.Fatalf("got %s", b)
	}
}

func TestLateAndPausedWrapperSpans(t *testing.T) {
	r := setupAI(t, WithEmitSpans(true), WithLLMCallEvents(true))
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false), TraceID: "application-trace"})
	ctx, parent := StartSpan(ctx, SpanOptions{Name: "agent", Operation: "invoke_agent"})
	_, s := beginWrapperSpan(ctx, i, time.Now())
	i.End(EndOptions{})
	if i.recordWrapperCall(LLMCall{Provider: "openai", Model: "m", InputTokens: 1, OutputTokens: 1}, s, 200) {
		t.Fatal("late product call accepted")
	}
	parent.End(nil)
	if len(r.named("ai_llm_response_received")) != 0 || len(r.named("ai_otel_span")) != 2 {
		t.Fatal(r.names())
	}
	p := r.named("ai_otel_span")[0].Properties
	if p["interaction_id"] != i.id || p["parent_span_id"] != parent.SpanID || p["trace_id"] != parent.TraceID {
		t.Fatal(p)
	}
	if i.traceID != "application-trace" {
		t.Fatal("application trace semantics changed")
	}
	var f struct{ Snapshot PausedInteraction }
	loadFixture(t, "spec/fixtures/pause-pending-usage-v0.1.json", &f)
	_, resumed := StartInteraction(context.Background(), InteractionOptions{Resume: &f.Snapshot, Classify: Ptr(false)})
	if resumed.sdkTraceID != f.Snapshot.SDKTraceID {
		t.Fatal("SDK trace ID lost during restore")
	}
	if len(resumed.calls) == 0 || resumed.calls[0].spanID != "0000000000000001" {
		t.Fatal("pending span ID lost")
	}
	n := len(r.named("ai_otel_span"))
	resumed.End(EndOptions{})
	if len(r.named("ai_otel_span")) != n {
		t.Fatal("restoration replayed a span")
	}
}

func TestSpanContentMaskOnceAndOverflow(t *testing.T) {
	masked := 0
	r := setupAI(t, WithEmitSpans(true), WithLLMCallEvents(true), WithCaptureContent(true), WithLimits(Limits{MaxCallsPerInteraction: 1}), WithRedactHook(func(s string) (string, error) {
		if s == "answer" {
			masked++
		}
		return s + " masked", nil
	}))
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	for n := range 3 {
		_, s := beginWrapperSpan(ctx, i, time.Now())
		i.recordWrapperCall(LLMCall{Provider: "openai", Model: "m", ResponseID: fmt.Sprint(n), InputTokens: 1, OutputTokens: 1, OutputText: "answer"}, s, 200)
	}
	i.End(EndOptions{})
	if masked != 3 {
		t.Fatalf("content masked %d times", masked)
	}
	if len(r.named("ai_otel_span")) != 3 {
		t.Fatal("overflow suppressed invocation spans")
	}
	calls := r.named("ai_llm_response_received")
	if len(calls) < 2 {
		t.Fatal("missing overflow event")
	}
	for _, c := range calls[1:] {
		if c.Properties["span_id"] != nil {
			t.Fatal("aggregate references one span")
		}
	}
}

func TestPrimitiveMapKeyStringParity(t *testing.T) {
	got := stripBinary(map[any]any{1e21: "large", 1e-7: "small", math.Inf(1): "infinite", math.Copysign(0, -1): "zero", true: "bool"})
	want := `{"0":"zero","1e+21":"large","1e-7":"small","Infinity":"infinite","true":"bool"}`
	if mustJSON(got) != want {
		t.Fatalf("got %s want %s", mustJSON(got), want)
	}
}

type failingSpanClient struct {
	r       *recorder
	inspect func()
}

func (c *failingSpanClient) Enqueue(m analytics.Message) error {
	track := m.(analytics.Track)
	if track.Event == "ai_otel_span" {
		c.inspect()
		return errors.New("transport failed")
	}
	return c.r.Enqueue(m)
}
func (*failingSpanClient) Close() error { return nil }
func TestSpanFailureAfterAdmission(t *testing.T) {
	freshState(t)
	r := &recorder{}
	client := &failingSpanClient{r: r}
	var i *Interaction
	client.inspect = func() {
		i.mu.Lock()
		defer i.mu.Unlock()
		if len(i.calls) != 1 || i.callCount != 1 {
			t.Error("transport started before admission completed")
		}
	}
	if err := Configure(WithAnalytics(client), WithEmitSpans(true), WithLLMCallEvents(true), WithOnError(func(error, ErrorInfo) { panic("onError failed") })); err != nil {
		t.Fatal(err)
	}
	ctx, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	i = in
	_, s := beginWrapperSpan(ctx, i, time.Now())
	if !i.recordWrapperCall(LLMCall{Provider: "openai", Model: "m", InputTokens: 2, OutputTokens: 1}, s, 200) {
		t.Fatal("transport failure changed admission")
	}
	i.End(EndOptions{})
	if len(r.named("ai_llm_response_received")) != 1 || r.named("ai_response_received")[0].Properties["total_tokens"] != 3 {
		t.Fatal(r.names())
	}
	if mustJSON(r.names()) != `["ai_user_prompt_created","ai_llm_response_received","ai_response_received"]` {
		t.Fatal("product order changed")
	}
}

type marshaledBinaryContainer struct {
	Text  string `json:"text"`
	Bytes []byte `json:"bytes"`
}

func (v marshaledBinaryContainer) MarshalJSON() ([]byte, error) {
	type plain marshaledBinaryContainer
	return json.Marshal(plain(v))
}
func TestBinaryInMarshaledStruct(t *testing.T) {
	got := stripBinary(marshaledBinaryContainer{Text: "keep", Bytes: []byte("PRIVATE")})
	if mustJSON(got) != `{"text":"keep"}` {
		t.Fatalf("typed binary escaped into JSON: %s", mustJSON(got))
	}
}

func TestConcurrentSpanContextsAndEndedParent(t *testing.T) {
	r := setupAI(t, WithEmitSpans(true))
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	ctx, parent := StartSpan(ctx, SpanOptions{Name: "agent", Operation: "invoke_agent"})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			callCtx, wrapper := beginWrapperSpan(ctx, i, time.Now())
			nested, child := StartSpan(callCtx, SpanOptions{Name: "tool", Operation: "execute_tool"})
			child.End(nil)
			_, sibling := StartSpan(nested, SpanOptions{Name: "retrieval", Operation: "retrieval"})
			sibling.End(nil)
			i.recordWrapperCall(LLMCall{Provider: "openai", Model: "m", ResponseID: wrapper.SpanID, InputTokens: 1, OutputTokens: 1}, wrapper, 200)
		})
	}
	wg.Wait()
	parent.End(nil)
	i.End(EndOptions{})
	wrappers := map[any]bool{}
	for _, m := range r.named("ai_otel_span") {
		if m.Properties["span_name"] == "chat m" {
			wrappers[m.Properties["span_id"]] = true
			if m.Properties["parent_span_id"] != parent.SpanID {
				t.Fatal("wrapper parent leaked")
			}
		}
	}
	if len(wrappers) != 8 {
		t.Fatal("concurrent wrappers merged")
	}
	for _, m := range r.named("ai_otel_span") {
		name := m.Properties["span_name"]
		if name == "tool" || name == "retrieval" {
			if !wrappers[m.Properties["parent_span_id"]] {
				t.Fatal("nested span escaped its provider context")
			}
		}
	}
	cycle := map[string]any{"text": "a@example.com"}
	cycle["self"] = cycle
	masked, ok := maskValue(cycle)
	if !ok || mustJSON(masked) != `{"text":"[email]"}` {
		t.Fatal("masker failed to drop self-reference")
	}
}
