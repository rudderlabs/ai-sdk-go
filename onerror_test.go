package rudderstackai

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

type seenErrors struct {
	mu    sync.Mutex
	infos []ErrorInfo
	errs  []error
}

func (s *seenErrors) hook(err error, info ErrorInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.infos = append(s.infos, info)
	s.errs = append(s.errs, err)
}

func (s *seenErrors) all() []ErrorInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ErrorInfo(nil), s.infos...)
}

func TestOnErrorRateLimitedPerKind(t *testing.T) {
	seen := &seenErrors{}
	setupAI(t, WithOnError(seen.hook))
	for range 3 {
		report(KindInternal, "a", errors.New("x"))
		report(KindLimitReached, "b", errors.New("y"))
	}
	got := seen.all()
	if len(got) != 2 || got[0] != (ErrorInfo{Kind: KindInternal, Where: "a"}) || got[1] != (ErrorInfo{Kind: KindLimitReached, Where: "b"}) {
		t.Fatalf("hook saw %+v", got)
	}
}

func TestOnErrorHookPanicIsSwallowed(t *testing.T) {
	setupAI(t, WithOnError(func(error, ErrorInfo) { panic("hook") }))
	report(KindInternal, "a", errors.New("x")) // must not panic
}

func TestDefaultReportLogsOncePerKindWithoutErrorText(t *testing.T) {
	setupAI(t)
	logs := captureLogs(t)
	for range 3 {
		report(KindUserCallback, "WithRedactHook", errors.New("sk-secret-token"))
	}
	if logs.count("user_callback error in WithRedactHook") != 1 || logs.count("sk-secret") != 0 {
		t.Fatalf("logs: %s", logs)
	}
}

func TestSafelyReportsWhere(t *testing.T) {
	seen := &seenErrors{}
	setupAI(t, WithOnError(seen.hook))
	safely(func() { panic("boom") })
	got := seen.all()
	if len(got) != 1 || got[0].Kind != KindInternal || got[0].Where != "TestSafelyReportsWhere" {
		t.Fatalf("hook saw %+v", got)
	}
	if !strings.Contains(seen.errs[0].Error(), "boom") {
		t.Fatalf("err = %v", seen.errs[0])
	}
}

// A failing SDK step never replaces the error the caller's code returned.
func TestUserErrorWinsOverFailingCallback(t *testing.T) {
	seen := &seenErrors{}
	setupAI(t, WithOnError(seen.hook), WithCaptureContent(true),
		WithRedactHook(func(string) (string, error) { panic("redact") }),
		WithTraceIDFrom(func(context.Context) string { panic("trace") }))
	want := errors.New("provider down")
	err := RunInteraction(context.Background(), InteractionOptions{UserID: "u", PromptText: "hi"}, func(context.Context, *Interaction) error {
		return want
	})
	if err != want {
		t.Fatalf("err = %v; want the caller's", err)
	}
	got := seen.all()
	if len(got) != 1 || got[0].Kind != KindUserCallback {
		t.Fatalf("hook saw %+v", got)
	}
}

func TestToolRuleCallbackFailureReported(t *testing.T) {
	seen := &seenErrors{}
	r := setupAI(t, WithOnError(seen.hook), WithEventRules(EventRules{Tools: map[string]ToolEventRule{
		"search": {Event: "Search Ran", When: func(map[string]any, any) bool { panic("when") }},
	}}))
	_ = RunInteraction(context.Background(), InteractionOptions{UserID: "u"}, func(_ context.Context, i *Interaction) error {
		i.RecordToolCalls(ToolCall{ID: "1", Name: "search", Error: Ptr(false)})
		return nil
	})
	if len(r.named("Search Ran")) != 0 {
		t.Fatal("event sent though When panicked")
	}
	if got := seen.all(); len(got) != 1 || got[0] != (ErrorInfo{Kind: KindUserCallback, Where: "ToolEventRule.When"}) {
		t.Fatalf("hook saw %+v", got)
	}
}

type explodingJSON struct{}

func (explodingJSON) MarshalJSON() ([]byte, error) { panic("MarshalJSON") }

type tagged struct {
	Name string `json:"name"`
}

// analytics-go encodes events on its own goroutine: a panicking MarshalJSON there would end the
// process. Values are encoded before the client sees them; the bad one is dropped.
func TestPanickingMarshalJSONNeverReachesTheClient(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			body, _ = gzip.NewReader(r.Body)
		}
		b, _ := io.ReadAll(body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	defer srv.Close()
	client, err := analytics.NewWithConfig("key", analytics.Config{DataPlaneUrl: srv.URL, Interval: time.Hour, Logger: analytics.StdLogger(log.New(io.Discard, "", 0))})
	if err != nil {
		t.Fatal(err)
	}
	seen := &seenErrors{}
	setupAI(t, WithAnalytics(client), WithOnError(seen.hook))
	extra := map[string]any{"bad": explodingJSON{}, "nested": map[string]any{"bad": explodingJSON{}}, "plan": tagged{Name: "pro"}}
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true, Context: map[string]any{"bad": explodingJSON{}, "app": "x"}})
	i.End(EndOptions{ExtraProperties: extra})
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("bodies: %d", len(bodies))
	}
	var batch struct {
		Batch []struct {
			Properties map[string]any `json:"properties"`
			Context    map[string]any `json:"context"`
		} `json:"batch"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &batch); err != nil || len(batch.Batch) != 1 {
		t.Fatalf("batch %s: %v", bodies[0], err)
	}
	p, c := batch.Batch[0].Properties, batch.Batch[0].Context
	if _, ok := p["bad"]; ok || p["nested"] != nil || p["plan"].(map[string]any)["name"] != "pro" || c["app"] != "x" || c["bad"] != nil {
		t.Fatalf("properties %v context %v", p, c)
	}
	if got := seen.all(); len(got) != 1 || got[0] != (ErrorInfo{Kind: KindUserCallback, Where: "MarshalJSON"}) {
		t.Fatalf("hook saw %+v", got)
	}
}

// A map the caller keeps changing after End is not the map the client encodes.
func TestPropertiesAreCopiedBeforeEnqueue(t *testing.T) {
	r := setupAI(t)
	extra := map[string]any{"tags": map[string]any{"a": 1}}
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", NoPromptEvent: true})
	i.End(EndOptions{ExtraProperties: extra})
	extra["tags"].(map[string]any)["a"] = 2
	if got := r.response(t)["tags"].(map[string]any)["a"]; got != 1 {
		t.Fatalf("tags.a = %v; the sent copy changed", got)
	}
}
