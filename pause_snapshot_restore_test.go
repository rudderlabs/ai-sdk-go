package rudderstackai

// SDK-6945, SDK-7019: SDK keys, fitted content, content shape, resumed scope, copies on
// restore, the serializer at pause, the live restoring context.

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

func sdk6945Wrapper(e analytics.Track) any {
	return pauseSegmentsJSON2(e.Context.Extra["wrapperLibrary"])
}

func pauseSegmentsJSON2(v any) any {
	var out any
	b, _ := json.Marshal(v)
	_ = json.Unmarshal(b, &out)
	return out
}

func TestSDK6945OuterWrapperLibrarySurvivesRestore(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Context: map[string]any{
		"k": "v", "library": map[string]any{"name": "app"}, "wrapperLibrary": map[string]any{"name": "outer-sdk"},
	}})
	i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", InputTokens: 1})
	s, _ := i.Pause(PauseOptions{})
	want := sdk6945Wrapper(r.named("ai_response_received")[0])
	if w, _ := want.(map[string]any); w["name"] != LibraryName || mustJSON(w["wrapperLibrary"]) != `{"name":"outer-sdk"}` {
		t.Fatalf("in-process wrapperLibrary = %v", want)
	}
	written, _ := pauseSegmentsJSON(t, s).(map[string]any)
	if got := mustJSON(written["context"]); got != `{"k":"v","wrapperLibrary":{"name":"outer-sdk"}}` {
		t.Fatalf("snapshot context = %s", got)
	}
	stored := sdk6945Stored(t, s, func(m map[string]any) {
		m["context"].(map[string]any)["library"] = map[string]any{"name": "old"}
	})
	r.reset()
	_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	restored.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", InputTokens: 2})
	restored.End(EndOptions{})
	if len(r.all()) == 0 {
		t.Fatal("no events")
	}
	for _, e := range r.all() {
		if _, has := e.Context.Extra["library"]; has {
			t.Errorf("%s: library restored", e.Event)
		}
		if got := sdk6945Wrapper(e); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: wrapperLibrary = %v, want %v", e.Event, got, want)
		}
		if e.Context.Extra["k"] != "v" {
			t.Errorf("%s: k lost", e.Event)
		}
	}
}

func sdk6945Big() []GenAIMessage {
	var m []GenAIMessage
	for n := 0; n < 40; n++ {
		m = append(m, GenAIMessage{Role: "user", Parts: []GenAIPart{{"type": "text", "content": strings.Repeat("x", 4000)}}})
	}
	return m
}

func sdk6945ContentOf(p map[string]any) string {
	out := map[string]any{}
	for _, k := range sdk6945ContentKeys {
		if v, ok := p[k]; ok {
			out[k] = v
		}
	}
	return mustJSON(out)
}

func TestSDK6945SnapshotContentIsFittedToTheEvent(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	sdk6945DuringDelivery(r, func() {
		i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", InputTokens: 10,
			StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110), InputMessages: sdk6945Big(), OutputText: "answer"})
	})
	s, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("not paused")
	}
	record := sdk6945PendingCall(t, s)
	stored := sdk6945Stored(t, s, nil)
	snapshotContent := sdk6945ContentOf(record)
	r.reset()
	i.End(EndOptions{})
	inProcess := sdk6945ContentOf(r.props(t, "ai_llm_response_received"))
	if !strings.Contains(inProcess, "input_messages") {
		t.Fatal("no in-process content")
	}
	if snapshotContent != inProcess {
		t.Fatalf("snapshot content is %d bytes, the event's %d", len(snapshotContent), len(inProcess))
	}
	r.reset()
	_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	restored.End(EndOptions{})
	if got := sdk6945ContentOf(r.props(t, "ai_llm_response_received")); got != inProcess {
		t.Fatalf("restored event content is %d bytes, in process %d", len(got), len(inProcess))
	}
}

func TestSDK6945ContentShapeIsChecked(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	sdk6945DuringDelivery(r, func() {
		i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", InputTokens: 10,
			StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110), InputMessages: sdk6945Message("hi")})
	})
	s, _ := i.Pause(PauseOptions{})
	for name, edit := range map[string]func(rec map[string]any){
		"no role":  func(rec map[string]any) { rec["input_messages"] = []any{map[string]any{"parts": []any{}}} },
		"no parts": func(rec map[string]any) { rec["input_messages"] = []any{map[string]any{"role": "user"}} },
		"part no type": func(rec map[string]any) {
			rec["input_messages"] = []any{map[string]any{"role": "user", "parts": []any{map[string]any{}}}}
		},
		"tool no name": func(rec map[string]any) { rec["output_tool_calls"] = []any{map[string]any{"id": "x"}} },
	} {
		stored := sdk6945Stored(t, s, func(m map[string]any) { edit(m["pending_calls"].([]any)[0].(map[string]any)) })
		if validPendingSnapshot(&stored) {
			t.Errorf("%s: snapshot is usable", name)
		}
	}
}

func TestSDK6945RestoredScopeHoldsTheAppContext(t *testing.T) {
	r := setupAI(t, WithTaskStartedEvents(true))
	opts := InteractionOptions{UserID: "u", Context: map[string]any{"app_key": "a"}, Cookies: sdk6945Cookie, IdentityFromCookies: Ptr(true)}
	run := func(o InteractionOptions) (ctxs map[string]string) {
		ctxs = map[string]string{}
		_ = RunInteraction(context.Background(), o, func(ctx context.Context, i *Interaction) error {
			_ = RunTask(ctx, "nested", TaskOptions{}, func(ctx context.Context, tk *Interaction) error { return nil })
			TrackAction(ctx, TrackActionOptions{ActionType: ActionCopiedResponse, InteractionID: i.ID(), ConversationID: i.ConversationID()})
			return nil
		})
		for _, e := range r.all() {
			if e.Event == "ai_task_started" || e.Event == "ai_user_action" {
				ctxs[e.Event] = mustJSON(sdk6945Context(t, e))
			}
		}
		return ctxs
	}
	var s PausedInteraction
	_ = RunInteraction(context.Background(), opts, func(ctx context.Context, i *Interaction) error {
		s, _ = i.Pause(PauseOptions{})
		return nil
	})
	want := map[string]string{}
	r.reset()
	// In process: the same scope, resumed by the app.
	_ = RunInteraction(context.Background(), opts, func(ctx context.Context, i *Interaction) error {
		_ = RunTask(ctx, "nested", TaskOptions{}, func(ctx context.Context, tk *Interaction) error { return nil })
		TrackAction(ctx, TrackActionOptions{ActionType: ActionCopiedResponse, InteractionID: i.ID(), ConversationID: i.ConversationID()})
		return nil
	})
	for _, e := range r.all() {
		if e.Event == "ai_task_started" || e.Event == "ai_user_action" {
			want[e.Event] = mustJSON(sdk6945Context(t, e))
		}
	}
	if want["ai_task_started"] != `{"app_key":"a"}` {
		t.Fatalf("in-process nested task context = %v", want)
	}
	stored := sdk6945Stored(t, s, nil)
	r.reset()
	got := run(InteractionOptions{Resume: &stored})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restored contexts = %v, in process %v", got, want)
	}
}

func TestSDK6945RestoreFromStructSharesNothing(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Context: map[string]any{"nested": map[string]any{"n": "1"}}})
	sdk6945DuringDelivery(r, func() {
		i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", InputTokens: 10,
			StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110), InputMessages: sdk6945Message("hello")})
	})
	s, _ := i.Pause(PauseOptions{})
	r.reset()
	_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &s})
	// The app edits its snapshot object after the restore.
	s.Context["nested"].(map[string]any)["n"] = "changed"
	s.Context["added"] = true
	s.PendingCalls[0]["input_messages"].([]any)[0].(map[string]any)["role"] = "changed"
	restored.End(EndOptions{})
	for _, e := range r.all() {
		if mustJSON(e.Context.Extra["nested"]) != `{"n":"1"}` || e.Context.Extra["added"] != nil {
			t.Errorf("%s context = %v", e.Event, e.Context.Extra)
		}
	}
	if got := mustJSON(r.props(t, "ai_llm_response_received")["input_messages"]); !strings.Contains(got, `"role":"user"`) {
		t.Fatalf("restored content = %s", got)
	}
}

type sdk6945Counter struct {
	n    *atomic.Int32
	hook func()
}

func (c sdk6945Counter) MarshalJSON() ([]byte, error) {
	c.n.Add(1)
	if c.hook != nil {
		c.hook()
	}
	return []byte(`"c"`), nil
}

func TestSDK6945SerializerRunsOnlyForThePausingPause(t *testing.T) {
	var n atomic.Int32
	r := setupAI(t, WithLLMCallEvents(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Context: map[string]any{"c": sdk6945Counter{n: &n}}})
	i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", InputTokens: 1})
	n.Store(0)
	r.reset()
	if _, ok := i.Pause(PauseOptions{}); !ok {
		t.Fatal("not paused")
	}
	first := n.Load()
	// Once for each event the pause sent, and once for the snapshot.
	if sent := int32(len(r.all())); first != sent+1 {
		t.Fatalf("first pause ran the serializer %d times for %d events, want %d", first, sent, sent+1)
	}
	s2, ok := i.Pause(PauseOptions{})
	if !ok || n.Load() != first {
		t.Fatalf("repeated pause: ok=%v, serializer runs %d, want %d", ok, n.Load(), first)
	}
	if _, has := pauseSegmentsJSON(t, s2).(map[string]any)["context"]; !has {
		t.Fatal("repeated snapshot lost its context")
	}
	i.End(EndOptions{})
	after := n.Load()
	if _, ok := i.Pause(PauseOptions{}); ok {
		t.Fatal("paused after end")
	}
	if n.Load() != after {
		t.Fatal("pause after End ran the serializer")
	}
	// A new pause after a resume takes a fresh form.
	_, j := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Context: map[string]any{"c": sdk6945Counter{n: &n}}})
	n.Store(0)
	j.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", InputTokens: 1})
	j.Pause(PauseOptions{})
	j.Resume()
	before := n.Load()
	j.Pause(PauseOptions{})
	if n.Load() <= before {
		t.Fatal("a pause after a resume reused the old form")
	}
	_ = r
}

func TestSDK6945CallRecordedBySerializerIsPending(t *testing.T) {
	setupAI(t, WithLLMCallEvents(true))
	var n atomic.Int32
	var current atomic.Pointer[Interaction]
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Context: map[string]any{"c": sdk6945Counter{n: &n, hook: func() {
		if n.Load() == 1 { // the paused response's run, as on base
			current.Load().RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", InputTokens: 7, StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110)})
		}
	}}}})
	current.Store(i)
	s, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("not paused")
	}
	if len(s.PendingCalls) != 1 || s.PendingCalls[0]["call_event_pending"] != true {
		t.Fatalf("pending calls = %v, want the recorded call pending", s.PendingCalls)
	}
}

func TestSDK6945LiveRestoringContext(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Cookies: sdk6945Cookie, IdentityFromCookies: Ptr(true),
		Context: map[string]any{"snap": "s", "both": "snapshot"}})
	i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", InputTokens: 1})
	s, _ := i.Pause(PauseOptions{})
	stored := sdk6945Stored(t, s, nil)
	r.reset()
	live := map[string]any{"live": 1, "both": "live"}
	_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &stored, Context: live})
	restored.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", InputTokens: 2})
	live["live"] = 2
	live["added"] = "x"
	live["snap"] = "now-live"
	restored.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", InputTokens: 3, ResponseID: "r2"})
	restored.End(EndOptions{})
	events := r.all()
	last := sdk6945Context(t, events[len(events)-1])
	if mustJSON(last) != `{"added":"x","both":"live","live":2,"sessionId":1760342400123,"snap":"now-live"}` {
		t.Fatalf("last event context = %s", mustJSON(last))
	}
	delete(live, "snap")
	delete(live, "both")
	restored.mu.Lock()
	got := restored.messageContext()
	restored.mu.Unlock()
	if mustJSON(got) != `{"added":"x","both":"snapshot","live":2,"sessionId":1760342400123,"snap":"s"}` {
		t.Fatalf("message context = %s", mustJSON(got))
	}
}

func TestSDK6945SnapshotIsStrictJSON(t *testing.T) {
	setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Context: map[string]any{"nan": math.NaN(), "inf": math.Inf(1), "ok": 1.5}})
	c := &recordedCall{callEventPending: true, content: &callContent{
		inputMessages:   []any{map[string]any{"role": "user", "parts": []any{map[string]any{"type": "text", "n": math.NaN()}}}},
		outputToolCalls: []any{map[string]any{"name": "t", "args": math.Inf(-1)}},
	}}
	i.mu.Lock()
	p := i.pendingCallLocked(c)
	i.mu.Unlock()
	if _, err := json.Marshal(p); err != nil {
		t.Fatalf("pending call does not encode: %v", err)
	}
	i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4.1", InputTokens: 1})
	s, _ := i.Pause(PauseOptions{})
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "nan") || strings.Contains(string(b), "inf") {
		t.Fatalf("snapshot = %s", b)
	}
}
