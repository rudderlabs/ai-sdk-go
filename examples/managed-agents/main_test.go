package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rudderlabs/ai-sdk-go/examples/internal/harness"
	"github.com/rudderlabs/ai-sdk-go/internal/dataplane"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

func TestCheck(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"--check"}, &out); code != 0 {
		t.Fatalf("check exited %d:\n%s", code, out.String())
	}
}

// TestLive runs --live against a mock standing in for the real data plane.
func TestLive(t *testing.T) {
	upstream := dataplane.Start()
	defer upstream.Close()
	t.Setenv("RUDDERSTACK_WRITE_KEY", "live-key")
	t.Setenv("RUDDERSTACK_DATA_PLANE_URL", upstream.URL)
	var out bytes.Buffer
	if code := run([]string{"--live"}, &out); code != 0 {
		t.Fatalf("live check exited %d:\n%s", code, out.String())
	}
	if len(upstream.Messages()) != 2 {
		t.Errorf("upstream got %d events", len(upstream.Messages()))
	}
	t.Setenv("RUDDERSTACK_WRITE_KEY", "")
	out.Reset()
	if code := run([]string{"--live"}, &out); code != 0 || !strings.Contains(out.String(), harness.Skipped) {
		t.Errorf("live without a key exited %d: %s", code, out.String())
	}
}

func TestExitsWhenTheStreamEndsBeforeTheSessionGoesIdle(t *testing.T) {
	path, err := harness.RepoFile("spec/fixtures/cassettes/claude-managed-agents-tools.json")
	if err != nil {
		t.Fatal(err)
	}
	cassette, err := testkit.LoadCassette(path)
	if err != nil {
		t.Fatal(err)
	}
	freshSession(cassette)
	body := func(suffix string) json.RawMessage {
		for _, it := range cassette.Interactions {
			if strings.HasSuffix(it.Request.Path, suffix) && it.Response.Body != nil {
				return it.Response.Body
			}
		}
		t.Fatalf("no %s reply", suffix)
		return nil
	}
	at := `"processed_at":"2026-11-16T10:00:00.000000Z"`
	llm := testkit.StartLLM()
	defer llm.Close()
	llm.Script("anthropic.managed_agents",
		testkit.Reply{JSON: body("/v1/environments")},
		testkit.Reply{JSON: body("/v1/agents")},
		testkit.Reply{JSON: body("/v1/sessions")},
		testkit.Reply{SSE: []testkit.SSEEvent{
			{Event: "message", Data: json.RawMessage(`{"id":"sevt_early_1","type":"user.message","content":[{"type":"text","text":"Hi"}],` + at + `}`)},
			{Event: "message", Data: json.RawMessage(`{"id":"sevt_early_2","type":"span.model_request_start",` + at + `}`)},
		}},
		testkit.Reply{JSON: json.RawMessage(`{"data":[]}`)},
	)
	upstream := dataplane.Start()
	defer upstream.Close()
	t.Setenv("ANTHROPIC_API_KEY", "test")
	t.Setenv("ANTHROPIC_BASE_URL", llm.URL)
	t.Setenv("MANAGED_ENVIRONMENT_ID", "")
	t.Setenv("MANAGED_AGENT_ID", "")
	t.Setenv("RUDDERSTACK_WRITE_KEY", "live-key")
	t.Setenv("RUDDERSTACK_DATA_PLANE_URL", upstream.URL)
	var out bytes.Buffer
	if code := run(nil, &out); code != 1 || !strings.Contains(out.String(), "the stream ended before the session went idle") {
		t.Fatalf("exited %d:\n%s", code, out.String())
	}
	answers := harness.ByEvent(upstream.Messages())["ai_response_received"]
	if len(answers) != 1 || answers[0].Properties["response_status"] != "cancelled" {
		t.Fatalf("%+v", answers)
	}
}

func TestRunErrors(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"--nope"}, &out); code != 2 {
		t.Errorf("bad flag exit = %d", code)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	if code := run(nil, &out); code != 1 || !strings.Contains(out.String(), "ANTHROPIC_API_KEY") {
		t.Errorf("missing key exit = %d: %s", code, out.String())
	}
}
