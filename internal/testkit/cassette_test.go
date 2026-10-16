package testkit

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func cassettePath(name string) string {
	return filepath.Join(RepoRoot(), "spec", "fixtures", "cassettes", name)
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("%s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

// parseSSE splits a server-sent event body into events (data kept raw).
func parseSSE(body string) []StreamEvent {
	var out []StreamEvent
	for _, block := range strings.Split(body, "\n\n") {
		var e StreamEvent
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				e.Event = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				e.Payload = json.RawMessage(v)
			}
		}
		if e.Payload != nil {
			out = append(out, e)
		}
	}
	return out
}

// sameEvent compares a received event with the recorded one; a string payload arrives raw.
func sameEvent(t *testing.T, got, want StreamEvent) bool {
	t.Helper()
	if got.Event != want.Event {
		return false
	}
	var s string
	if json.Unmarshal(want.Payload, &s) == nil {
		return string(got.Payload) == s
	}
	return jsonEqual(t, got.Payload, want.Payload)
}

// requestPaths stand in for what a real client sends for each recorded path.
var requestPaths = map[string]string{
	"/model/{modelId}/converse":        "/model/us.anthropic.claude-sonnet-4%3A0/converse",
	"/model/{modelId}/converse-stream": "/model/us.anthropic.claude-sonnet-4%3A0/converse-stream",
}

func TestReplayCassettes(t *testing.T) {
	for _, name := range []string{
		"examples/node-openai.json",
		"examples/node-anthropic.json",
		"google-genai.json",
		"bedrock-converse.json",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := LoadCassette(cassettePath(name))
			if err != nil {
				t.Fatal(err)
			}
			l := startLLM(t)
			l.Replay(c)
			if l.Pending() != len(c.Interactions) {
				t.Fatalf("pending = %d, want %d", l.Pending(), len(c.Interactions))
			}
			for i, it := range c.Interactions {
				path := it.Request.Path
				if p, ok := requestPaths[path]; ok {
					path = p
				}
				resp := post(t, l.URL+path, "{}")
				want := it.Response
				if resp.StatusCode != want.Status || resp.Header.Get("Content-Type") != want.Headers["content-type"] {
					t.Fatalf("%d: got %d %q", i, resp.StatusCode, resp.Header.Get("Content-Type"))
				}
				for k, v := range want.Headers {
					if resp.Header.Get(k) != v {
						t.Fatalf("%d: header %s = %q, want %q", i, k, resp.Header.Get(k), v)
					}
				}
				checkBody(t, resp.Body, want)
			}
			if l.Pending() != 0 {
				t.Fatalf("pending = %d", l.Pending())
			}
		})
	}
}

func checkBody(t *testing.T, body io.Reader, want CassetteResponse) {
	t.Helper()
	switch {
	case strings.Contains(want.Headers["content-type"], "eventstream"):
		got := decodeAll(t, body)
		if len(got) != len(want.Events) {
			t.Fatalf("got %d messages, want %d", len(got), len(want.Events))
		}
		for i, e := range want.Events {
			if got[i].headers[":event-type"] != e.Event || got[i].headers[":message-type"] != "event" ||
				!jsonEqual(t, []byte(got[i].payload), e.Payload) {
				t.Fatalf("message %d: got %v, want %s %s", i, got[i], e.Event, e.Payload)
			}
		}
	case want.Events != nil:
		got := parseSSE(readAll(t, body))
		if len(got) != len(want.Events) {
			t.Fatalf("got %d events, want %d", len(got), len(want.Events))
		}
		for i := range want.Events {
			if !sameEvent(t, got[i], want.Events[i]) {
				t.Fatalf("event %d: got %+v, want %+v", i, got[i], want.Events[i])
			}
		}
	default:
		if got := readAll(t, body); !jsonEqual(t, []byte(got), want.Body) {
			t.Fatalf("body = %s", got)
		}
	}
}

// The route, not the exact path, picks the reply: other API versions and models match.
func TestReplayMatchesAnyGooglePath(t *testing.T) {
	c, err := LoadCassette(cassettePath("google-genai.json"))
	if err != nil {
		t.Fatal(err)
	}
	l := startLLM(t)
	l.Replay(c)
	resp := post(t, l.URL+"/v1beta1/publishers/google/models/gemini-other:generateContent", "{}")
	checkBody(t, resp.Body, c.Interactions[0].Response)
	resp = post(t, l.URL+"/v1beta1/publishers/google/models/gemini-other:streamGenerateContent?alt=sse", "{}")
	checkBody(t, resp.Body, c.Interactions[1].Response)
}

func TestReplaySkipsOtherInteractionsAndSendsStringBodies(t *testing.T) {
	c := &Cassette{Interactions: []Interaction{
		{Request: CassetteRequest{Method: "GET", Path: "/v1/models"}},
		{Request: CassetteRequest{Method: "POST", Path: "/v1/embeddings"}},
		{
			Request:  CassetteRequest{Method: "post", Path: "/v1/chat/completions"},
			Response: CassetteResponse{Status: 502, Headers: map[string]string{"content-type": "text/plain"}, Body: json.RawMessage(`"upstream down"`)},
		},
	}}
	l := startLLM(t)
	l.Replay(c)
	if l.Pending() != 1 {
		t.Fatalf("pending = %d", l.Pending())
	}
	resp := post(t, l.URL+"/v1/chat/completions", "{}")
	if got := readAll(t, resp.Body); resp.StatusCode != 502 || got != "upstream down" ||
		resp.Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("got %d %q %v", resp.StatusCode, got, resp.Header)
	}
}

func TestLoadCassetteErrors(t *testing.T) {
	if _, err := LoadCassette(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file loaded")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCassette(bad); err == nil || !strings.Contains(err.Error(), "bad.json") {
		t.Fatalf("err = %v", err)
	}
}

func TestReplayedSSEIsCompact(t *testing.T) {
	c := &Cassette{Interactions: []Interaction{{
		Request: CassetteRequest{Method: "POST", Path: "/v1/responses"},
		Response: CassetteResponse{Status: 200, Headers: map[string]string{"content-type": "text/event-stream"},
			Events: []StreamEvent{{Event: "response.completed", Payload: json.RawMessage("{\n \"a\": 1\n}")}}},
	}}}
	l := startLLM(t)
	l.Replay(c)
	got := readAll(t, post(t, l.URL+"/v1/responses", "{}").Body)
	if !bytes.Equal([]byte(got), []byte("event: response.completed\ndata: {\"a\":1}\n\n")) {
		t.Fatalf("got %q", got)
	}
}
