package harness

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
)

func ctx() map[string]any {
	return map[string]any{
		"library":        map[string]any{"name": "analytics-go"},
		"wrapperLibrary": map[string]any{"name": "ai-sdk-go", "integration": "mcp", "specVersion": "0.4.0"},
	}
}

func TestValidate(t *testing.T) {
	ok := dataplane.Message{Type: "track", Event: "audience_created", Context: ctx(), Properties: map[string]any{"server_name": "s", "tool_name": "t", "tool_status": "success"}}
	if err := Validate([]dataplane.Message{ok}); err != nil {
		t.Errorf("valid event: %v", err)
	}
	cases := map[string][]dataplane.Message{
		"no events":   nil,
		"context":     {{Type: "track", Event: "mcp_client_seen", Context: map[string]any{}}},
		"schema":      {{Type: "track", Event: "mcp_client_seen", Context: ctx(), Properties: map[string]any{"transport": "pigeon"}}},
		"unknown mcp": {{Type: "track", Event: "mcp_nope", Context: ctx(), Properties: map[string]any{}}},
	}
	for name, msgs := range cases {
		if err := Validate(msgs); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	t.Chdir(t.TempDir())
	if err := Validate([]dataplane.Message{ok}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("outside the repository: %v", err)
	}
}

func TestFieldsAndCounts(t *testing.T) {
	props := map[string]any{"a": 1.0, "b": []any{"x"}}
	if err := Fields("e", props, map[string]any{"a": 1, "b": []string{"x"}, "c": Absent, "a ": Absent}); err != nil {
		t.Error(err)
	}
	for _, want := range []map[string]any{{"a": 2}, {"a": Absent}, {"c": Present}, {"c": "x"}} {
		if err := Fields("e", props, want); err == nil {
			t.Errorf("%v: no error", want)
		}
	}
	if err := Fields("e", props, map[string]any{"a": Present}); err != nil {
		t.Error(err)
	}
	found := []dataplane.Message{{Properties: map[string]any{"i": 1.0}}, {Properties: map[string]any{"i": 0.0}}}
	if Find(found, "i", 0)["i"] != 0.0 || len(Find(found, "i", 2)) != 0 {
		t.Error("find")
	}
	by := ByEvent([]dataplane.Message{{Event: "x"}, {Event: "x"}})
	if Counts(by, map[string]int{"x": 2}) != nil || Counts(by, map[string]int{"y": 1}) == nil {
		t.Error("counts")
	}
}

func TestRun(t *testing.T) {
	var out bytes.Buffer
	sent := func(s *Sink) error {
		c, err := s.NewClient()
		if err != nil {
			return err
		}
		_ = c.Enqueue(analytics.Track{Event: "mcp_tool_called", UserId: "u"})
		return c.Close()
	}
	if code := Run(&out, false, sent); code != 1 || !strings.Contains(out.String(), "check failed") {
		t.Errorf("event without the SDK context: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run(&out, false, func(*Sink) error { return errors.New("boom") }); code != 1 || !strings.Contains(out.String(), "boom") {
		t.Errorf("failing check: %d %s", code, out.String())
	}
	t.Setenv("RUDDERSTACK_WRITE_KEY", "")
	out.Reset()
	if code := Run(&out, true, sent); code != 0 || !strings.Contains(out.String(), Skipped) {
		t.Errorf("live without a key: %d %s", code, out.String())
	}
}

func TestEnvClientAndServe(t *testing.T) {
	t.Setenv("RUDDERSTACK_WRITE_KEY", "")
	if _, err := EnvClient(); err == nil {
		t.Error("EnvClient without env")
	}
	t.Setenv("RUDDERSTACK_WRITE_KEY", "k")
	t.Setenv("RUDDERSTACK_DATA_PLANE_URL", "http://127.0.0.1:1")
	c, err := EnvClient()
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := serveUntil(ctx, "127.0.0.1:0", http.NotFoundHandler()); err != nil {
		t.Errorf("stopped server: %v", err)
	}
	if err := Serve("bad address", http.NotFoundHandler()); err == nil {
		t.Error("bad address served")
	}
}

func TestMockLLM(t *testing.T) {
	llm, err := MockLLM("examples/node-openai.json", "examples/node-openai.json")
	if err != nil {
		t.Fatal(err)
	}
	defer llm.Close()
	if llm.Pending() != 2 {
		t.Errorf("pending = %d, want 2", llm.Pending())
	}
	if _, err := MockLLM("nope.json"); err == nil {
		t.Error("missing cassette")
	}
	if Integration(dataplane.Message{Context: ctx()}) != "mcp" || Integration(dataplane.Message{}) != "" {
		t.Error("integration")
	}
}

func TestOnce(t *testing.T) {
	var out bytes.Buffer
	t.Setenv("RUDDERSTACK_WRITE_KEY", "")
	if code := Once(&out, func(*Sink) error { return errors.New("boom") }); code != 1 || !strings.Contains(out.String(), "local data plane") {
		t.Errorf("failing run: %d %s", code, out.String())
	}
	upstream := dataplane.Start()
	defer upstream.Close()
	t.Setenv("RUDDERSTACK_WRITE_KEY", "k")
	t.Setenv("RUDDERSTACK_DATA_PLANE_URL", upstream.URL)
	out.Reset()
	sent := func(s *Sink) error {
		c, err := s.NewClient()
		if err != nil {
			return err
		}
		_ = c.Enqueue(analytics.Track{Event: "audience_created", UserId: "u", Context: &analytics.Context{Extra: ctx()},
			Properties: analytics.Properties{"server_name": "s", "tool_name": "t", "tool_status": "success"}})
		return c.Close()
	}
	if code := Once(&out, sent); code != 0 || !strings.Contains(out.String(), "audience_created (mcp)") || len(upstream.Messages()) != 1 {
		t.Errorf("run: %d %s", code, out.String())
	}
}
