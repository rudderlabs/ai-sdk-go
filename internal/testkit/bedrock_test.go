package testkit

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

const conversePath = "/model/us.anthropic.claude-sonnet-4%3A0/converse"

// decoded is one event-stream message as the tests compare it.
type decoded struct {
	headers map[string]string
	payload string
}

func decodeAll(t *testing.T, r io.Reader) []decoded {
	t.Helper()
	dec := eventstream.NewDecoder()
	var out []decoded
	for {
		m, err := dec.Decode(r, nil)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		h := map[string]string{}
		for _, hd := range m.Headers {
			h[hd.Name] = hd.Value.String()
		}
		out = append(out, decoded{h, string(m.Payload)})
	}
}

func TestBedrockConverse(t *testing.T) {
	l := startLLM(t)
	l.ScriptBedrock("bedrock.converse",
		BedrockReply{Response: json.RawMessage(`{"stopReason":"end_turn"}`), RequestID: "req-1"},
		BedrockReply{},
	)
	resp := post(t, l.URL+conversePath, `{"messages":[]}`)
	if resp.StatusCode != 200 || resp.Header.Get("X-Amzn-Requestid") != "req-1" ||
		resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %v", resp.StatusCode, resp.Header)
	}
	if got := readAll(t, resp.Body); got != `{"stopReason":"end_turn"}` {
		t.Fatalf("body = %s", got)
	}
	resp = post(t, l.URL+conversePath, `{}`)
	if got := readAll(t, resp.Body); got != `{}` || resp.Header.Get("X-Amzn-Requestid") != "" {
		t.Fatalf("empty reply: %s %v", got, resp.Header)
	}
	if got := l.Requests("bedrock.converse"); len(got) != 2 || string(got[0]) != `{"messages":[]}` {
		t.Fatalf("requests = %q", got)
	}
}

func TestBedrockError(t *testing.T) {
	l := startLLM(t)
	l.ScriptBedrock("bedrock.converse",
		BedrockReply{Error: &BedrockError{Code: "ThrottlingException", Status: 429, Message: "slow down"}},
		BedrockReply{Error: &BedrockError{Code: "ValidationException", Message: "bad model"}},
	)
	resp := post(t, l.URL+conversePath, `{}`)
	if resp.StatusCode != 429 || resp.Header.Get("X-Amzn-Errortype") != "ThrottlingException" {
		t.Fatalf("got %d %v", resp.StatusCode, resp.Header)
	}
	if got := readAll(t, resp.Body); got != `{"message":"slow down"}` {
		t.Fatalf("body = %s", got)
	}
	resp = post(t, l.URL+conversePath, `{}`)
	if resp.StatusCode != 400 || resp.Header.Get("X-Amzn-Errortype") != "ValidationException" {
		t.Fatalf("got %d %v", resp.StatusCode, resp.Header)
	}
}

func TestBedrockStream(t *testing.T) {
	l := startLLM(t)
	l.ScriptBedrock("bedrock.converse_stream", BedrockReply{
		RequestID: "req-s",
		Stream: []json.RawMessage{
			json.RawMessage(`{"messageStart": {"role": "assistant"}}`),
			json.RawMessage(`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"Hi"}}}`),
		},
		StreamError: &BedrockError{Code: "ThrottlingException", Message: "slow down"},
	})
	resp := post(t, l.URL+"/model/m/converse-stream", `{}`)
	if resp.Header.Get("Content-Type") != "application/vnd.amazon.eventstream" ||
		resp.Header.Get("X-Amzn-Requestid") != "req-s" {
		t.Fatalf("headers %v", resp.Header)
	}
	got := decodeAll(t, resp.Body)
	want := []decoded{
		{map[string]string{":message-type": "event", ":event-type": "messageStart", ":content-type": "application/json"}, `{"role":"assistant"}`},
		{map[string]string{":message-type": "event", ":event-type": "contentBlockDelta", ":content-type": "application/json"}, `{"contentBlockIndex":0,"delta":{"text":"Hi"}}`},
		{map[string]string{":message-type": "exception", ":exception-type": "ThrottlingException", ":content-type": "application/json"}, `{"message":"slow down"}`},
	}
	assertMessages(t, got, want)
}

func assertMessages(t *testing.T, got, want []decoded) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].payload != want[i].payload || len(got[i].headers) != len(want[i].headers) {
			t.Fatalf("message %d: got %v, want %v", i, got[i], want[i])
		}
		for k, v := range want[i].headers {
			if got[i].headers[k] != v {
				t.Fatalf("message %d header %s: got %q, want %q", i, k, got[i].headers[k], v)
			}
		}
	}
}

func TestBedrockStreamOnlyError(t *testing.T) {
	l := startLLM(t)
	l.ScriptBedrock("bedrock.converse_stream", BedrockReply{StreamError: &BedrockError{Code: "ModelStreamErrorException", Message: "x"}})
	got := decodeAll(t, post(t, l.URL+"/model/m/converse-stream", `{}`).Body)
	if len(got) != 1 || got[0].headers[":exception-type"] != "ModelStreamErrorException" {
		t.Fatalf("got %v", got)
	}
}

func TestBedrockStreamBadEvent(t *testing.T) {
	l := startLLM(t)
	l.ScriptBedrock("bedrock.converse_stream", BedrockReply{Stream: []json.RawMessage{json.RawMessage(`{"a":1,"b":2}`)}})
	resp := post(t, l.URL+"/model/m/converse-stream", `{}`)
	if body := readAll(t, resp.Body); resp.StatusCode != 500 || !strings.Contains(body, "one key") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
}

func TestCompactInvalid(t *testing.T) {
	if got := string(compact([]byte("{bad"))); got != "{bad" {
		t.Fatalf("got %q", got)
	}
}
