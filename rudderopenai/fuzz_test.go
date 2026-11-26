//go:build unix

package rudderopenai_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
	"github.com/rudderlabs/ai-sdk-go/rudderopenai"
)

// Fuzz tests for usage and stream parsing, the Go twin of the Node.js and Python property tests.
// The seed inputs run with every go test; go test -fuzz <name> explores more.

// static answers every request with the same body, delivered in pieces.
type static struct {
	status      int
	contentType string
	pieces      [][]byte
}

func (s static) RoundTrip(req *http.Request) (*http.Response, error) {
	_, _ = io.Copy(io.Discard, req.Body)
	_ = req.Body.Close()
	return response(s.status, s.contentType, s.pieces), nil
}

// split cuts body at offsets taken from cuts: each byte picks a point along the body.
func split(body, cuts []byte) [][]byte {
	points := []int{0}
	for _, c := range cuts {
		points = append(points, int(c)*len(body)/256)
	}
	points = append(points, len(body))
	slices.Sort(points)
	var out [][]byte
	for i := 1; i < len(points); i++ {
		if points[i] > points[i-1] {
			out = append(out, body[points[i-1]:points[i]])
		}
	}
	return out
}

// sseBody is the SSE body of a recorded streamed response in spec/fixtures/cassettes.
func sseBody(t testing.TB, cassette string) []byte {
	t.Helper()
	c, err := testkit.LoadCassette(filepath.Join(testkit.RepoRoot(), "spec", "fixtures", "cassettes", cassette))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	for _, e := range c.Interactions[0].Response.Events {
		if e.Event != "" {
			b.WriteString("event: " + e.Event + "\n")
		}
		var s string
		if json.Unmarshal(e.Payload, &s) != nil {
			var compact bytes.Buffer
			_ = json.Compact(&compact, e.Payload)
			s = compact.String()
		}
		b.WriteString("data: " + s + "\n\n")
	}
	return b.Bytes()
}

// outcome is what the app sees: the response's JSON, or the error.
var outcome = testkit.Outcome

func track(t *testing.T) *recorder {
	t.Helper()
	hook.Reset()
	rec := &recorder{}
	if err := rudderstackai.Configure(rudderstackai.WithAnalytics(rec), rudderstackai.WithLLMCallEvents(true),
		rudderstackai.WithCaptureContent(false), rudderstackai.WithEmitSpans(false)); err != nil {
		t.Fatal(err)
	}
	return rec
}

var tokenTotals = []string{"total_input_tokens", "total_output_tokens", "total_tokens", "total_cached_input_tokens", "total_cache_write_tokens"}

// checkAnswer: one conforming answer whose token totals are non-negative integers.
func checkAnswer(t *testing.T, rec *recorder) []event {
	t.Helper()
	events := rec.events(t) // schema-checks every event
	answer := one(t, events, "ai_response_received")
	for _, k := range tokenTotals {
		if v, ok := answer[k]; ok {
			if f, isNum := v.(float64); !isNum || f < 0 || f != math.Trunc(f) {
				t.Errorf("%s = %v", k, v)
			}
		}
	}
	return events
}

// plausible reports whether every number in the JSON is below 1e15 in magnitude: no provider
// reports token counts beyond that, and sums of larger ones may overflow.
var plausible = testkit.PlausibleJSONNumbers

func FuzzChatUsage(f *testing.F) {
	for _, s := range []string{
		`{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17}`, `null`, `"x"`, `[]`, `{"prompt_tokens":-1}`,
		`{"prompt_tokens":1.5}`, `{"prompt_tokens":9007199254740993}`, `{"prompt_tokens":true}`,
		`{"prompt_tokens_details":0,"completion_tokens_details":{"reasoning_tokens":"x"}}`,
		`{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":40}}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, usage []byte) {
		if !json.Valid(usage) || !plausible(usage) {
			return
		}
		body := []byte(`{"id":"chatcmpl-p","object":"chat.completion","created":1760400000,"model":"gpt-4.1-mini-2025-04-14",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":` + string(usage) + `}`)
		rt := static{200, "application/json", [][]byte{body}}
		ctx := context.Background()
		plain, wrapped := fakeClient(rt), fakeClient(rt, rudderopenai.WithTracking())
		raw := outcome(rawJSON(plain.Chat.Completions.New(ctx, chatParams("hi"))))
		if strings.HasPrefix(raw, "error: ") { // openai-go does not validate usage: the call must succeed
			t.Fatalf("the call failed without the SDK: %s", raw)
		}
		rec := track(t)
		var tracked string
		_ = interaction(ctx, benchUser, func(ctx context.Context) error {
			c, err := wrapped.Chat.Completions.New(ctx, chatParams("hi"))
			tracked = outcome(rawJSON(c, err))
			return err
		})
		if tracked != raw {
			t.Fatalf("the app saw %q, without the SDK %q", tracked, raw)
		}
		checkAnswer(t, rec)
	})
}

func rawJSON(c *openai.ChatCompletion, err error) (string, error) {
	if err != nil {
		return "", err
	}
	return c.RawJSON(), nil
}

type streamCase struct {
	cassette string
	consume  func(ctx context.Context, c openai.Client) ([]string, error)
}

var withBody = body

var streamCases = map[string]streamCase{
	// The recorded stream ends with a usage chunk; without asking for it, the wrapper (which asks
	// on the app's behalf) would rightly hide that chunk from the app.
	"chat": {"examples/node-openai.json", func(ctx context.Context, c openai.Client) ([]string, error) {
		s := c.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{}, withBody(
			`{"model":"gpt-4.1-mini","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`))
		var out []string
		for s.Next() {
			out = append(out, s.Current().RawJSON())
		}
		return out, s.Err()
	}},
	"responses": {"examples/node-openai-responses.json", func(ctx context.Context, c openai.Client) ([]string, error) {
		s := c.Responses.NewStreaming(ctx, responses.ResponseNewParams{}, withBody(`{"model":"gpt-4.1-mini","input":"hi","stream":true}`))
		var out []string
		for s.Next() {
			out = append(out, s.Current().RawJSON())
		}
		return out, s.Err()
	}},
}

func streamed(ctx context.Context, sc streamCase, body []byte, cuts []byte, wrap bool) string {
	var opts []option.RequestOption
	if wrap {
		opts = append(opts, rudderopenai.WithTracking())
	}
	chunks, err := sc.consume(ctx, fakeClient(static{200, "text/event-stream", split(body, cuts)}, opts...))
	return delivered(chunks, err)
}

// delivered is what the app saw of a stream: every chunk, then the error that ended it, if any.
func delivered(chunks []string, err error) string {
	out := strings.Join(chunks, "\n")
	if err != nil {
		out += "\nerror: " + err.Error()
	}
	return out
}

// stable drops the event fields that differ between two runs of the same call.
func stable(events []event) []event {
	out := make([]event, 0, len(events))
	for _, e := range events {
		props := map[string]any{}
		for k, v := range e.props {
			if k != "interaction_id" && !strings.HasSuffix(k, "_ms") && k != "span_id" {
				props[k] = v
			}
		}
		out = append(out, event{e.name, props})
	}
	return out
}

func fuzzStreamSplit(f *testing.F, name string) {
	sc := streamCases[name]
	body := sseBody(f, sc.cassette)
	var once sync.Once
	var whole string
	var wholeEvents []event
	f.Add([]byte{})
	f.Add([]byte{1, 7, 64, 128, 200, 255})
	f.Fuzz(func(t *testing.T, cuts []byte) {
		ctx := context.Background()
		once.Do(func() {
			rec := track(t)
			_ = interaction(ctx, benchUser, func(ctx context.Context) error {
				whole = streamed(ctx, sc, body, nil, true)
				return nil
			})
			wholeEvents = stable(checkAnswer(t, rec))
			if strings.Contains(whole, "error: ") {
				t.Fatalf("the recorded stream failed: %s", whole)
			}
		})
		rec := track(t)
		var got string
		_ = interaction(ctx, benchUser, func(ctx context.Context) error {
			got = streamed(ctx, sc, body, cuts, true)
			return nil
		})
		if got != whole {
			t.Fatalf("split at %v the app saw %q, want %q", cuts, got, whole)
		}
		if events := stable(checkAnswer(t, rec)); !reflect.DeepEqual(events, wholeEvents) {
			t.Fatalf("split at %v the events differ:\n%v\nwant\n%v", cuts, events, wholeEvents)
		}
	})
}

func fuzzStreamCut(f *testing.F, name string) {
	sc := streamCases[name]
	body := sseBody(f, sc.cassette)
	f.Add(uint16(len(body)), []byte{}, []byte{})
	f.Add(uint16(len(body)/2), []byte("data: {not json\n\n"), []byte{9, 99})
	f.Add(uint16(10), []byte{0xff, 0xfe}, []byte{})
	f.Fuzz(func(t *testing.T, keep uint16, junk, cuts []byte) {
		cut := append(append([]byte{}, body[:int(keep)%(len(body)+1)]...), junk...)
		ctx := context.Background()
		raw := streamed(ctx, sc, cut, cuts, false)
		rec := track(t)
		var tracked string
		_ = interaction(ctx, benchUser, func(ctx context.Context) error {
			tracked = streamed(ctx, sc, cut, cuts, true)
			return nil
		})
		if tracked != raw {
			t.Fatalf("the app saw %q, without the SDK %q", tracked, raw)
		}
		checkAnswer(t, rec)
	})
}

func FuzzChatStreamSplit(f *testing.F)      { fuzzStreamSplit(f, "chat") }
func FuzzChatStreamCut(f *testing.F)        { fuzzStreamCut(f, "chat") }
func FuzzResponsesStreamSplit(f *testing.F) { fuzzStreamSplit(f, "responses") }
func FuzzResponsesStreamCut(f *testing.F)   { fuzzStreamCut(f, "responses") }
