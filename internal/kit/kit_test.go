package kit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"

	analytics "github.com/rudderlabs/analytics-go/v4"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

func TestMain(m *testing.M) {
	// Per-call events make each recorded call observable.
	if err := rudderstackai.Configure(rudderstackai.WithLLMCallEvents(true)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// recorder is an analytics client that keeps every track call and checks it against the spec schema.
type recorder struct {
	mu       sync.Mutex
	messages []analytics.Track
	invalid  []string
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	r := &recorder{}
	t.Cleanup(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, msg := range r.invalid {
			t.Errorf("event breaks the spec schema: %s", msg)
		}
	})
	return r
}

func (r *recorder) Enqueue(m analytics.Message) error {
	t, ok := m.(analytics.Track)
	if !ok {
		return fmt.Errorf("unexpected message %T", m)
	}
	err := testkit.ValidateEvent(t.Event, t.Properties)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.invalid = append(r.invalid, fmt.Sprintf("%s: %v", t.Event, err))
	}
	r.messages = append(r.messages, t)
	return nil
}

func (r *recorder) Close() error { return nil }

func (r *recorder) named(event string) []analytics.Track {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []analytics.Track
	for _, m := range r.messages {
		if m.Event == event {
			out = append(out, m)
		}
	}
	return out
}

func (r *recorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, m := range r.messages {
		out = append(out, m.Event)
	}
	return out
}

// props returns the properties of the only event with this name.
func (r *recorder) props(t *testing.T, event string) map[string]any {
	t.Helper()
	got := r.named(event)
	if len(got) != 1 {
		t.Fatalf("want 1 %s, got %d: %v", event, len(got), r.names())
	}
	return got[0].Properties
}

func (r *recorder) none(t *testing.T, event string) {
	t.Helper()
	if got := r.named(event); len(got) != 0 {
		t.Fatalf("want no %s, got %d: %v", event, len(got), r.names())
	}
}

// want checks the listed properties of props; nil means absent.
func want(t *testing.T, props map[string]any, fields map[string]any) {
	t.Helper()
	for k, v := range fields {
		got, ok := props[k]
		if v == nil {
			if ok {
				t.Errorf("%s: want absent, got %v", k, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, v) {
			t.Errorf("%s: want %v (%T), got %v (%T)", k, v, v, got, got)
		}
	}
}

// interaction starts an interaction reporting into rec.
func interaction(rec *recorder) (context.Context, *rudderstackai.Interaction) {
	return rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u1", Feature: "kit_test", Analytics: rec})
}

func TestParseToolValue(t *testing.T) {
	cases := []struct {
		in, want any
	}{
		{`{"city":"Paris"}`, map[string]any{"city": "Paris"}},
		{`[1,2]`, []any{float64(1), float64(2)}},
		{`"quoted"`, "quoted"},
		{"not json", "not json"},
		{42, 42},
		{nil, nil},
	}
	for _, c := range cases {
		if got := ParseToolValue(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseToolValue(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestToolResultValue(t *testing.T) {
	blocks := []any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "text", "text": "b"}}
	image := []any{map[string]any{"type": "image", "source": "x"}}
	noText := []any{map[string]any{"type": "text", "text": 3}}
	notMap := []any{"raw"}
	cases := []struct {
		name     string
		in, want any
	}{
		{"json string", `{"ok":true}`, map[string]any{"ok": true}},
		{"plain string", "done", "done"},
		{"single text block", []any{map[string]any{"type": "text", "text": `{"n":1}`}}, map[string]any{"n": float64(1)}},
		{"several blocks", blocks, blocks},
		{"image block", image, image},
		{"text not a string", noText, noText},
		{"block not an object", notMap, notMap},
		{"other", map[string]any{"k": "v"}, map[string]any{"k": "v"}},
	}
	for _, c := range cases {
		if got := ToolResultValue(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestJSONHelpers(t *testing.T) {
	ints := []struct {
		in   any
		want int
	}{
		{float64(12.9), 12}, {float64(-3), 0}, {7, 7}, {-7, 0},
		{json.Number("5"), 5}, {json.Number("-5"), 0}, {json.Number("x"), 0}, {"9", 0}, {nil, 0},
	}
	for _, c := range ints {
		if got := Int(c.in); got != c.want {
			t.Errorf("Int(%v) = %d, want %d", c.in, got, c.want)
		}
	}
	if p := IntPtr(float64(3)); p == nil || *p != 3 {
		t.Errorf("IntPtr(3) = %v", p)
	}
	if p := IntPtr(0); p == nil || *p != 0 {
		t.Errorf("IntPtr(0) = %v, want pointer to 0", p)
	}
	if p := IntPtr(json.Number("4")); p == nil || *p != 4 {
		t.Errorf("IntPtr(json 4) = %v", p)
	}
	if p := IntPtr("3"); p != nil {
		t.Errorf("IntPtr(string) = %v, want nil", *p)
	}
	if p := IntPtr(nil); p != nil {
		t.Errorf("IntPtr(nil) = %v, want nil", *p)
	}
	if Str("a") != "a" || Str(1) != "" || Str(nil) != "" {
		t.Error("Str")
	}
	if m := Map(map[string]any{"a": 1}); m["a"] != 1 {
		t.Error("Map of an object")
	}
	if Map([]any{}) != nil || Map(nil) != nil {
		t.Error("Map of a non-object")
	}
	if l := List([]any{1}); len(l) != 1 {
		t.Error("List of an array")
	}
	if List(map[string]any{}) != nil || List(nil) != nil {
		t.Error("List of a non-array")
	}
}

func TestHTTPErrorType(t *testing.T) {
	quota := map[string]any{"error": map[string]any{"code": "insufficient_quota"}}
	limited := map[string]any{"error": map[string]any{"type": "rate_limit_exceeded"}}
	other := map[string]any{"error": map[string]any{"code": "server_error"}}
	cases := []struct {
		status int
		body   map[string]any
		want   string
	}{
		{429, nil, "rate_limit"},
		{401, nil, "auth_error"},
		{403, nil, "auth_error"},
		{400, nil, "invalid_request"},
		{422, nil, "invalid_request"},
		{408, nil, "timeout"},
		{504, nil, "timeout"},
		{500, quota, "rate_limit"},
		{503, limited, "rate_limit"},
		{500, other, "api_error"},
		{500, map[string]any{"error": "text"}, "api_error"},
		{502, nil, "api_error"},
	}
	for _, c := range cases {
		if got := HTTPErrorType(c.status, c.body); got != c.want {
			t.Errorf("HTTPErrorType(%d, %v) = %q, want %q", c.status, c.body, got, c.want)
		}
	}
}

var errClassified = errors.New("provider says no")

func TestErrorTypeOfTransport(t *testing.T) {
	RegisterErrorClassifier(func(err error) (string, bool) {
		if errors.Is(err, errClassified) {
			return "auth_error", true
		}
		return "", false
	})
	cases := []struct {
		err  error
		want string
	}{
		{context.Canceled, "cancelled"},
		{fmt.Errorf("wrapped: %w", context.Canceled), "cancelled"},
		{context.DeadlineExceeded, "timeout"},
		{errors.New("boom"), "api_error"},
		{fmt.Errorf("call: %w", errClassified), "auth_error"},
	}
	for _, c := range cases {
		if got := ErrorTypeOfTransport(c.err); got != c.want {
			t.Errorf("ErrorTypeOfTransport(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestEndEphemeral(t *testing.T) {
	t.Run("derived from calls", func(t *testing.T) {
		rec := newRecorder(t)
		i, ephemeral := ForCall(context.Background(), "kit-test", rec)
		if i == nil || !ephemeral {
			t.Fatalf("ForCall without an interaction: got %v, ephemeral %v", i, ephemeral)
		}
		i.RecordCall(rudderstackai.LLMCall{Provider: "openai", Model: "gpt-4o-mini", InputTokens: 3, OutputTokens: 2})
		EndEphemeral(i, "")
		want(t, rec.props(t, "ai_response_received"), map[string]any{"response_status": "success", "trigger": "system", "error_type": nil})
	})
	t.Run("with an error type", func(t *testing.T) {
		rec := newRecorder(t)
		i, _ := ForCall(context.Background(), "kit-test", rec)
		EndEphemeral(i, "timeout")
		want(t, rec.props(t, "ai_response_received"), map[string]any{"response_status": "timeout", "error_type": "timeout"})
	})
}

func TestForCallActive(t *testing.T) {
	rec := newRecorder(t)
	ctx, active := interaction(rec)
	i, ephemeral := ForCall(ctx, "kit-test", rec)
	if i != active || ephemeral {
		t.Fatalf("ForCall in an interaction: got %p ephemeral %v, want %p", i, ephemeral, active)
	}
	if Untracked(ctx) {
		t.Error("an interaction's context is not untracked")
	}
	if KeepsText(i) {
		t.Error("text is kept only for a classifier or evaluator")
	}
	active.End(rudderstackai.EndOptions{})
}

func TestHintOutcome(t *testing.T) {
	rec := newRecorder(t)
	_, i := interaction(rec)
	HintOutcome(i, "cancelled")
	i.End(rudderstackai.EndOptions{})
	want(t, rec.props(t, "ai_response_received"), map[string]any{"response_status": "cancelled", "error_type": "cancelled"})
}
