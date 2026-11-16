package rudderanthropic_test

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

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
	"github.com/rudderlabs/ai-sdk-go/rudderanthropic"
)

// Fuzz tests for usage and stream parsing, the Go twin of the Node.js and Python property tests.
// The seed inputs run with every go test; go test -fuzz <name> explores more.

// pieces is a response body that hands out one piece per Read, as a network stream would.
type pieces [][]byte

func (p *pieces) Read(b []byte) (int, error) {
	for len(*p) > 0 && len((*p)[0]) == 0 {
		*p = (*p)[1:]
	}
	if len(*p) == 0 {
		return 0, io.EOF
	}
	n := copy(b, (*p)[0])
	(*p)[0] = (*p)[0][n:]
	return n, nil
}

func (*pieces) Close() error { return nil }

// static answers every request with the same body, split at offsets taken from cuts.
type static struct {
	contentType string
	body, cuts  []byte
}

func (s static) RoundTrip(req *http.Request) (*http.Response, error) {
	_, _ = io.Copy(io.Discard, req.Body)
	_ = req.Body.Close()
	points := []int{0}
	for _, c := range s.cuts {
		points = append(points, int(c)*len(s.body)/256)
	}
	points = append(points, len(s.body))
	slices.Sort(points)
	var p pieces
	for i := 1; i < len(points); i++ {
		if points[i] > points[i-1] {
			p = append(p, s.body[points[i-1]:points[i]])
		}
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {s.contentType}}, Body: &p}, nil
}

func client(rt http.RoundTripper, wrap bool) anthropic.Client {
	opts := []option.RequestOption{
		option.WithBaseURL("http://fuzz.invalid"), option.WithAPIKey("fuzz"), option.WithMaxRetries(0),
		option.WithHTTPClient(&http.Client{Transport: rt}),
	}
	if wrap {
		opts = append(opts, rudderanthropic.WithTracking())
	}
	return anthropic.NewClient(opts...)
}

var params = anthropic.MessageNewParams{
	Model: "claude-haiku-4-5-20251001", MaxTokens: 64,
	Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
}

func outcome(raw string, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	return raw
}

// tracked runs fn in an interaction with a fresh recorder and returns what fn returned.
func tracked(t *testing.T, fn func(context.Context) string) (string, *recorder) {
	t.Helper()
	hook.Reset()
	rec := &recorder{}
	if err := rudderstackai.Configure(rudderstackai.WithAnalytics(rec), rudderstackai.WithLLMCallEvents(true),
		rudderstackai.WithCaptureContent(false), rudderstackai.WithEmitSpans(false)); err != nil {
		t.Fatal(err)
	}
	var got string
	_ = rudderstackai.RunInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", ConversationID: "c"},
		func(ctx context.Context, _ *rudderstackai.Interaction) error {
			got = fn(ctx)
			return nil
		})
	return got, rec
}

var tokenTotals = []string{"total_input_tokens", "total_output_tokens", "total_tokens", "total_cached_input_tokens", "total_cache_write_tokens"}

// checkAnswer: one conforming answer whose token totals are non-negative integers.
func checkAnswer(t *testing.T, rec *recorder) map[string][]map[string]any {
	t.Helper()
	events := rec.events(t) // schema-checks every event
	answers := events["ai_response_received"]
	if len(answers) != 1 {
		t.Fatalf("want one ai_response_received, got %d", len(answers))
	}
	for _, k := range tokenTotals {
		if v, ok := answers[0][k]; ok {
			if f, isNum := v.(float64); !isNum || f < 0 || f != math.Trunc(f) {
				t.Errorf("%s = %v", k, v)
			}
		}
	}
	return events
}

// plausible reports whether every number in the JSON is below 1e15 in magnitude: no provider
// reports token counts beyond that, and sums of larger ones may overflow.
func plausible(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	for {
		tok, err := d.Token()
		if err != nil {
			return err == io.EOF
		}
		if n, ok := tok.(json.Number); ok {
			if f, err := n.Float64(); err != nil || math.Abs(f) >= 1e15 {
				return false
			}
		}
	}
}

func FuzzMessagesUsage(f *testing.F) {
	for _, s := range []string{
		`{"input_tokens":12,"output_tokens":5}`, `null`, `"x"`, `[]`, `{"input_tokens":-1}`, `{"input_tokens":1.5}`,
		`{"cache_read_input_tokens":4,"cache_creation":{"ephemeral_1h_input_tokens":"x"}}`,
		`{"server_tool_use":{"web_search_requests":2},"service_tier":7}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, usage []byte) {
		if !json.Valid(usage) || !plausible(usage) {
			return
		}
		rt := static{contentType: "application/json", body: []byte(`{"id":"msg_f","type":"message","role":"assistant",` +
			`"model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
			`"stop_sequence":null,"usage":` + string(usage) + `}`)}
		call := func(ctx context.Context, wrap bool) string {
			c := client(rt, wrap)
			m, err := c.Messages.New(ctx, params)
			if err != nil {
				return outcome("", err)
			}
			return m.RawJSON()
		}
		raw := call(context.Background(), false)
		got, rec := tracked(t, func(ctx context.Context) string { return call(ctx, true) })
		if got != raw {
			t.Fatalf("the app saw %q, without the SDK %q", got, raw)
		}
		checkAnswer(t, rec)
	})
}

func sseBody(t testing.TB) []byte {
	t.Helper()
	c, err := testkit.LoadCassette(filepath.Join(testkit.RepoRoot(), "spec", "fixtures", "cassettes", "examples", "node-anthropic.json"))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	for _, e := range c.Interactions[0].Response.Events {
		if e.Event != "" {
			b.WriteString("event: " + e.Event + "\n")
		}
		var compact bytes.Buffer
		_ = json.Compact(&compact, e.Payload)
		b.WriteString("data: " + compact.String() + "\n\n")
	}
	return b.Bytes()
}

func stream(ctx context.Context, body, cuts []byte, wrap bool) string {
	c := client(static{contentType: "text/event-stream", body: body, cuts: cuts}, wrap)
	s := c.Messages.NewStreaming(ctx, params)
	var out []string
	for s.Next() {
		out = append(out, s.Current().RawJSON())
	}
	return outcome(strings.Join(out, "\n"), s.Err())
}

// stable drops the event fields that differ between two runs of the same call.
func stable(events map[string][]map[string]any) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	for name, list := range events {
		for _, e := range list {
			props := map[string]any{}
			for k, v := range e {
				if k != "interaction_id" && !strings.HasSuffix(k, "_ms") {
					props[k] = v
				}
			}
			out[name] = append(out[name], props)
		}
	}
	return out
}

func FuzzMessagesStreamSplit(f *testing.F) {
	body := sseBody(f)
	var once sync.Once
	var whole string
	var wholeEvents map[string][]map[string]any
	f.Add([]byte{})
	f.Add([]byte{1, 7, 64, 128, 200, 255})
	f.Fuzz(func(t *testing.T, cuts []byte) {
		once.Do(func() {
			var rec *recorder
			whole, rec = tracked(t, func(ctx context.Context) string { return stream(ctx, body, nil, true) })
			wholeEvents = stable(checkAnswer(t, rec))
			if strings.HasPrefix(whole, "error: ") {
				t.Fatalf("the recorded stream failed: %s", whole)
			}
		})
		got, rec := tracked(t, func(ctx context.Context) string { return stream(ctx, body, cuts, true) })
		if got != whole {
			t.Fatalf("split at %v the app saw %q, want %q", cuts, got, whole)
		}
		if events := stable(checkAnswer(t, rec)); !reflect.DeepEqual(events, wholeEvents) {
			t.Fatalf("split at %v the events differ:\n%v\nwant\n%v", cuts, events, wholeEvents)
		}
	})
}

func FuzzMessagesStreamCut(f *testing.F) {
	body := sseBody(f)
	f.Add(uint16(len(body)), []byte{}, []byte{})
	f.Add(uint16(len(body)/2), []byte("event: message_delta\ndata: {not json\n\n"), []byte{9, 99})
	f.Add(uint16(10), []byte{0xff, 0xfe}, []byte{})
	f.Fuzz(func(t *testing.T, keep uint16, junk, cuts []byte) {
		cut := append(append([]byte{}, body[:int(keep)%(len(body)+1)]...), junk...)
		raw := stream(context.Background(), cut, cuts, false)
		got, rec := tracked(t, func(ctx context.Context) string { return stream(ctx, cut, cuts, true) })
		if got != raw {
			t.Fatalf("the app saw %q, without the SDK %q", got, raw)
		}
		checkAnswer(t, rec)
	})
}
