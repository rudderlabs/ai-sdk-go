package rudderstackai

// SDK-6945, SDK-7019: a JSON restore keeps the message context and the content of a call whose
// event is still pending, beyond what spec/fixtures/pause-segments-v0.1.json can express.

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"math"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

// sdk6945Cookie is cookies-v3 session_auto: rl_session id 1760342400123, expiring in 2100.
const sdk6945Cookie = "rl_session=RS_ENC_v3_eyJpZCI6MTc2MDM0MjQwMDEyMywiZXhwaXJlc0F0Ijo0MTAyNDQ0ODAwMDAwLCJ0aW1lb3V0IjoxODAwMDAwLCJhdXRvVHJhY2siOnRydWUsInNlc3Npb25TdGFydCI6dHJ1ZX0%3D"

// sdk6945Stored is s as a store returns it: JSON, optionally edited, decoded again.
func sdk6945Stored(t *testing.T, s PausedInteraction, edit func(map[string]any)) PausedInteraction {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // exact numbers, as the stored bytes
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(m)
	}
	var out PausedInteraction
	if err := json.Unmarshal([]byte(mustJSON(m)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// sdk6945Context is an event's message context as JSON, without the SDK's own wrapperLibrary.
func sdk6945Context(t *testing.T, e analytics.Track) map[string]any {
	t.Helper()
	extra := maps.Clone(e.Context.Extra)
	delete(extra, "wrapperLibrary")
	out, _ := pauseSegmentsJSON(t, extra).(map[string]any)
	return out
}

// sdk6945DuringDelivery runs fn once, inside delivery of the paused response.
func sdk6945DuringDelivery(r *recorder, fn func()) {
	var once sync.Once
	r.onEnqueue = func(m analytics.Message) {
		if e, ok := m.(analytics.Track); ok && e.Event == "ai_response_received" {
			once.Do(fn)
		}
	}
}

func sdk6945PendingCall(t *testing.T, s PausedInteraction) map[string]any {
	t.Helper()
	if len(s.PendingCalls) != 1 {
		t.Fatalf("pending calls = %d, want 1", len(s.PendingCalls))
	}
	out, _ := pauseSegmentsJSON(t, s.PendingCalls[0]).(map[string]any)
	return out
}

var sdk6945ContentKeys = []string{"input_messages", "input_truncated", "output_text", "output_tool_calls"}

func sdk6945Message(text string) []GenAIMessage {
	return []GenAIMessage{{Role: "user", Parts: []GenAIPart{{"type": "text", "content": text}}}}
}

func TestSDK6945SnapshotContextLeavesOutSDKKeys(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Context: map[string]any{
		"app_key": "a", "library": map[string]any{"name": "app"}, "wrapperLibrary": map[string]any{"name": "outer"},
	}})
	i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", InputTokens: 1})
	s, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("not paused")
	}
	written, _ := pauseSegmentsJSON(t, s).(map[string]any)
	// Only library is the SDK's; an outer layer's wrapperLibrary is app data.
	if got := mustJSON(written["context"]); got != `{"app_key":"a","wrapperLibrary":{"name":"outer"}}` {
		t.Fatalf("snapshot context = %s, want the app's keys without library", got)
	}
	// A snapshot from an older SDK version may hold its library and wrapperLibrary.
	stored := sdk6945Stored(t, s, func(m map[string]any) {
		c, _ := m["context"].(map[string]any)
		if c == nil {
			c = map[string]any{}
			m["context"] = c
		}
		c["library"] = map[string]any{"name": "old"}
		c["wrapperLibrary"] = map[string]any{"name": "old-sdk", "version": "0.0.1"}
	})
	r.reset()
	_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	restored.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", InputTokens: 2})
	restored.End(EndOptions{})
	events := r.all()
	if len(events) == 0 {
		t.Fatal("no events")
	}
	for _, e := range events {
		if _, has := e.Context.Extra["library"]; has {
			t.Errorf("%s: library restored from the snapshot", e.Event)
		}
		wrapper, _ := e.Context.Extra["wrapperLibrary"].(map[string]any)
		// The snapshot's old wrapperLibrary is the app's outer one: nested inside this SDK's.
		if outer, _ := wrapper["wrapperLibrary"].(map[string]any); wrapper["name"] != LibraryName || outer["name"] != "old-sdk" {
			t.Errorf("%s: wrapperLibrary = %v, want this SDK's around the snapshot's", e.Event, wrapper)
		}
		if e.Context.Extra["app_key"] != "a" {
			t.Errorf("%s: app_key = %v, want a", e.Event, e.Context.Extra["app_key"])
		}
	}
}

// sdk6945Marshaler records whether its MarshalJSON ran while the interaction lock was held.
type sdk6945Marshaler struct {
	in     *atomic.Pointer[Interaction]
	locked *atomic.Bool
}

func (m sdk6945Marshaler) MarshalJSON() ([]byte, error) {
	if i := m.in.Load(); i != nil {
		if i.mu.TryLock() {
			i.mu.Unlock()
		} else {
			m.locked.Store(true)
		}
	}
	return []byte(`{"b":1,"a":[2]}`), nil
}

type sdk6945Node struct{ Next *sdk6945Node }

func TestSDK6945SnapshotContextIsJSONSafe(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithOnError(func(error, ErrorInfo) {}))
	var current atomic.Pointer[Interaction]
	var locked atomic.Bool
	cycle := &sdk6945Node{}
	cycle.Next = cycle
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Trigger: TriggerSystem, Context: map[string]any{
		"ok": "x", "big": int64(1<<60 + 1), "nested": map[string]any{"n": 1.5},
		"marshaler": sdk6945Marshaler{&current, &locked},
		"ch":        make(chan int), "fn": func() {}, "nan": math.NaN(), "cycle": cycle,
	}})
	current.Store(i)
	i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", InputTokens: 1})
	s, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("not paused")
	}
	if _, err := json.Marshal(s); err != nil {
		t.Fatalf("snapshot does not encode: %v", err)
	}
	want := sdk6945Context(t, r.named("ai_response_received")[0])
	if _, has := want["marshaler"]; !has || len(want) != 4 {
		t.Fatalf("in-process context = %v", want)
	}
	stored := sdk6945Stored(t, s, nil)
	r.reset()
	_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	current.Store(restored)
	restored.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", InputTokens: 2})
	restored.End(EndOptions{})
	for _, e := range r.all() {
		if got := sdk6945Context(t, e); !reflect.DeepEqual(got, want) {
			t.Errorf("%s context = %v, want %v", e.Event, got, want)
		}
		// Lossless: a float64 decode would round 2^60+1.
		if b, _ := json.Marshal(e.Context.Extra["big"]); string(b) != "1152921504606846977" {
			t.Errorf("%s big = %s", e.Event, b)
		}
	}
	if locked.Load() {
		t.Fatal("app MarshalJSON ran under the interaction lock")
	}
}

func TestSDK6945RestoredSessionTagsSpans(t *testing.T) {
	setupAI(t)
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Cookies: sdk6945Cookie, IdentityFromCookies: Ptr(true)})
	tags, _, _, _ := tracingIdentity(ctx, "")
	if tags["session.id"] != "1760342400123" {
		t.Fatalf("in-process session.id = %q", tags["session.id"])
	}
	s, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("not paused")
	}
	stored := sdk6945Stored(t, s, nil)
	restoredCtx, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	defer restored.End(EndOptions{})
	if tags, _, _, _ := tracingIdentity(restoredCtx, ""); tags["session.id"] != "1760342400123" {
		t.Fatalf("restored session.id = %q, want the in-process one", tags["session.id"])
	}
}

func TestSDK6945RestoredRunHelpersKeepContext(t *testing.T) {
	r := setupAI(t)
	opts := InteractionOptions{UserID: "u", Context: map[string]any{"app_key": "a"}, Cookies: sdk6945Cookie, IdentityFromCookies: Ptr(true)}
	var s PausedInteraction
	_ = RunInteraction(context.Background(), opts, func(ctx context.Context, i *Interaction) error {
		TrackAction(ctx, TrackActionOptions{ActionType: ActionCopiedResponse, InteractionID: i.ID(), ConversationID: i.ConversationID()})
		s, _ = i.Pause(PauseOptions{})
		return nil
	})
	actions := r.named("ai_user_action")
	if len(actions) != 1 {
		t.Fatal("no in-process action")
	}
	want := sdk6945Context(t, actions[0])
	if mustJSON(want) != `{"app_key":"a","sessionId":1760342400123}` {
		t.Fatalf("in-process action context = %v", want)
	}
	stored := sdk6945Stored(t, s, nil)
	r.reset()
	_ = RunInteraction(context.Background(), InteractionOptions{Resume: &stored}, func(ctx context.Context, i *Interaction) error {
		TrackAction(ctx, TrackActionOptions{ActionType: ActionCopiedResponse, InteractionID: i.ID(), ConversationID: i.ConversationID()})
		return nil
	})
	actions = r.named("ai_user_action")
	if len(actions) != 1 {
		t.Fatal("no restored action")
	}
	if got := sdk6945Context(t, actions[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored action context = %v, want %v", got, want)
	}
}

func TestSDK6945InputTruncatedRoundTrip(t *testing.T) {
	var redactions atomic.Int32
	r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true), WithRedactHook(func(text string) (string, error) {
		redactions.Add(1)
		return text, nil
	}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	long := strings.Repeat("a", maxMaskedCodePoints+10)
	sdk6945DuringDelivery(r, func() {
		i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", InputTokens: 10,
			StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110), InputMessages: sdk6945Message(long)})
	})
	s, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("not paused")
	}
	record := sdk6945PendingCall(t, s)
	if record["call_event_pending"] != true || record["input_truncated"] != true || record["input_messages"] == nil {
		t.Fatalf("pending call = %v, want its truncated input", record)
	}
	// The stored flag is restored with the stored messages, however short.
	stored := sdk6945Stored(t, s, func(m map[string]any) {
		m["pending_calls"].([]any)[0].(map[string]any)["input_messages"] = pauseSegmentsJSON(t, genAIMessagesValue(sdk6945Message("hi")))
	})
	r.reset()
	before := redactions.Load()
	_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	restored.End(EndOptions{})
	p := r.props(t, "ai_llm_response_received")
	if p["input_truncated"] != true || mustJSON(p["input_messages"]) != `[{"parts":[{"content":"hi","type":"text"}],"role":"user"}]` {
		t.Fatalf("restored call event = %v", p)
	}
	if redactions.Load() != before {
		t.Fatal("restored content was masked again")
	}
}

func TestSDK6945NoContentForSentOrSpanOnlyCalls(t *testing.T) {
	for _, tc := range []struct {
		name      string
		opts      []ConfigOption
		call      LLMCall
		wrapper   bool
		wantEvent bool
	}{
		// A wrapper call's event goes out at once: nothing is left to restore.
		{"event already sent", []ConfigOption{WithLLMCallEvents(true), WithCaptureContent(true)}, LLMCall{}, false, false},
		// Without call events the wrapper span keeps the content for the span only.
		{"no call events", []ConfigOption{WithEmitSpans(true), WithCaptureContent(true)}, LLMCall{}, true, false},
		// Span content capture alone: the waiting call event has no content of its own.
		{"span-only content", []ConfigOption{WithLLMCallEvents(true), WithEmitSpans(true), WithContentCapture(ContentCapture{LLMCalls: Ptr(false), Spans: &SpanCapture{Default: Ptr(true)}})},
			LLMCall{UsageUnknown: true, StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110)}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := setupAI(t, tc.opts...)
			ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			sdk6945DuringDelivery(r, func() {
				call := tc.call
				call.Provider, call.Model, call.InputTokens = "openai", "gpt-4.1", 10
				call.InputMessages, call.OutputText = sdk6945Message("hello"), "answer"
				if tc.wrapper {
					_, call.wrapperSpan = beginWrapperSpan(ctx, i, time.UnixMilli(100))
					if call.wrapperSpan == nil {
						t.Error("no wrapper span")
					}
				}
				i.RecordCall(call)
			})
			s, ok := i.Pause(PauseOptions{})
			if !ok {
				t.Fatal("not paused")
			}
			record := sdk6945PendingCall(t, s)
			if record["call_event_pending"] != tc.wantEvent {
				t.Fatalf("call_event_pending = %v, want %v", record["call_event_pending"], tc.wantEvent)
			}
			for _, key := range sdk6945ContentKeys {
				if _, has := record[key]; has {
					t.Errorf("pending call holds %s", key)
				}
			}
			i.End(EndOptions{})
		})
	}
}

func TestSDK6945LegacySnapshotRestoresAsBefore(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Context: map[string]any{"app_key": "a"}, Cookies: sdk6945Cookie, IdentityFromCookies: Ptr(true)})
	sdk6945DuringDelivery(r, func() {
		i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", InputTokens: 10,
			StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110), InputMessages: sdk6945Message("hello"), OutputText: "answer"})
	})
	s, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("not paused")
	}
	stored := sdk6945Stored(t, s, func(m map[string]any) {
		delete(m, "context")
		delete(m, "session_id")
		for _, key := range sdk6945ContentKeys {
			delete(m["pending_calls"].([]any)[0].(map[string]any), key)
		}
	})
	r.reset()
	restoredCtx, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	if restored.ID() != s.InteractionID {
		t.Fatal("legacy snapshot not restored")
	}
	if tags, _, _, _ := tracingIdentity(restoredCtx, ""); tags["session.id"] != "" {
		t.Fatalf("session.id = %q, want none", tags["session.id"])
	}
	restored.End(EndOptions{})
	for _, e := range r.all() {
		if got := sdk6945Context(t, e); len(got) != 0 {
			t.Errorf("%s context = %v, want none", e.Event, got)
		}
	}
	p := r.props(t, "ai_llm_response_received")
	for _, key := range sdk6945ContentKeys {
		if _, has := p[key]; has {
			t.Errorf("call event sends %s", key)
		}
	}
}

func TestSDK6945SnapshotContextSizeBound(t *testing.T) {
	// {"k":"…"} is 8 bytes plus the value.
	for _, tc := range []struct {
		size    int
		written bool
	}{{maxMessageBytes - 1, true}, {maxMessageBytes, false}} {
		setupAI(t, WithOnError(func(error, ErrorInfo) {}))
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Context: map[string]any{"k": strings.Repeat("a", tc.size-8)}})
		s, ok := i.Pause(PauseOptions{})
		if !ok {
			t.Fatal("not paused")
		}
		written, _ := pauseSegmentsJSON(t, s).(map[string]any)
		if got := written["context"] != nil; got != tc.written {
			t.Errorf("%d bytes: context written = %v, want %v", tc.size, got, tc.written)
		}
		i.End(EndOptions{})
	}
}

func TestSDK6945HiddenAppSessionIDStaysHiddenAfterRestore(t *testing.T) {
	r := setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Cookies: sdk6945Cookie, IdentityFromCookies: Ptr(true),
		Context: map[string]any{"sessionId": make(chan int), "k": "v"}})
	s, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("not paused")
	}
	if got := sdk6945Context(t, r.named("ai_response_received")[0]); got["sessionId"] != nil {
		t.Fatalf("in-process sessionId = %v", got["sessionId"])
	}
	if written, _ := pauseSegmentsJSON(t, s).(map[string]any); written["session_id"] != nil {
		t.Fatalf("session_id written = %v", written["session_id"])
	}
	stored := sdk6945Stored(t, s, nil)
	r.reset()
	_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	restored.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", InputTokens: 1})
	restored.End(EndOptions{})
	if len(r.all()) == 0 {
		t.Fatal("no events")
	}
	for _, e := range r.all() {
		if v, has := sdk6945Context(t, e)["sessionId"]; has {
			t.Errorf("%s: restored sessionId = %v", e.Event, v)
		}
	}
}
