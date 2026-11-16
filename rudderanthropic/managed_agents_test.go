package rudderanthropic_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
	"github.com/rudderlabs/ai-sdk-go/rudderanthropic"
)

type maStream struct {
	Events    []json.RawMessage `json:"events"`
	Cassette  string            `json:"cassette"`
	StopAfter *int              `json:"stop_after"`
}

type maExpected struct {
	Event       string         `json:"event"`
	Interaction string         `json:"interaction"`
	Properties  map[string]any `json:"properties"`
}

type maScenario struct {
	Name    string `json:"name"`
	Options struct {
		ToolBreakdown  bool            `json:"tool_breakdown"`
		CaptureContent bool            `json:"capture_content"`
		Session        json.RawMessage `json:"session"`
	} `json:"options"`
	Streams  []maStream   `json:"streams"`
	Expected []maExpected `json:"expected"`
}

type maFixture struct {
	Session json.RawMessage `json:"session"`
	Price   struct {
		Model, Provider    string
		InputPerMtok       float64 `json:"input_per_mtok"`
		OutputPerMtok      float64 `json:"output_per_mtok"`
		CachedInputPerMtok float64 `json:"cached_input_per_mtok"`
		CacheWritePerMtok  float64 `json:"cache_write_per_mtok"`
	} `json:"price"`
	Scenarios []maScenario `json:"scenarios"`
}

// rawJSON is a session (or event) given as its JSON.
type rawJSON string

func (r rawJSON) RawJSON() string { return string(r) }

// sliceStream is a session event stream over fixture events.
type sliceStream struct {
	events []anthropic.BetaManagedAgentsStreamSessionEventsUnion
	n      int
	err    error
	closed bool
}

func (s *sliceStream) Next() bool {
	if s.n >= len(s.events) {
		return false
	}
	s.n++
	return true
}

func (s *sliceStream) Current() anthropic.BetaManagedAgentsStreamSessionEventsUnion {
	return s.events[s.n-1]
}
func (s *sliceStream) Err() error   { return s.err }
func (s *sliceStream) Close() error { s.closed = true; return nil }

func decodeEvents(t *testing.T, raw []json.RawMessage) []anthropic.BetaManagedAgentsStreamSessionEventsUnion {
	t.Helper()
	out := make([]anthropic.BetaManagedAgentsStreamSessionEventsUnion, len(raw))
	for n, r := range raw {
		if err := json.Unmarshal(r, &out[n]); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// cassetteTransport serves a cassette's interactions by method and path, in order: only the
// network is replaced.
type cassetteTransport struct {
	mu    sync.Mutex
	queue []struct {
		Request  struct{ Method, Path string } `json:"request"`
		Response struct {
			Status  int               `json:"status"`
			Headers map[string]string `json:"headers"`
			Body    json.RawMessage   `json:"body"`
			Events  []struct {
				Event   string          `json:"event"`
				Payload json.RawMessage `json:"payload"`
			} `json:"events"`
		} `json:"response"`
	}
}

func (c *cassetteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for n, item := range c.queue {
		if item.Request.Method != req.Method || item.Request.Path != req.URL.Path {
			continue
		}
		c.queue = append(c.queue[:n], c.queue[n+1:]...)
		var body bytes.Buffer
		if item.Response.Events != nil {
			for _, e := range item.Response.Events {
				if e.Event != "" {
					fmt.Fprintf(&body, "event: %s\n", e.Event)
				}
				var data bytes.Buffer
				_ = json.Compact(&data, e.Payload) // the cassette file is indented; SSE data is one line
				fmt.Fprintf(&body, "data: %s\n\n", data.Bytes())
			}
		} else {
			body.Write(item.Response.Body)
		}
		header := http.Header{}
		for k, v := range item.Response.Headers {
			header.Set(k, v)
		}
		return &http.Response{StatusCode: item.Response.Status, Header: header, Body: io.NopCloser(&body), Request: req}, nil
	}
	return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("not recorded")), Request: req}, nil
}

func cassetteClient(t *testing.T, name string) anthropic.Client {
	t.Helper()
	raw, err := os.ReadFile("../spec/fixtures/cassettes/" + name)
	if err != nil {
		t.Fatal(err)
	}
	transport := &cassetteTransport{}
	var cassette struct {
		Interactions json.RawMessage `json:"interactions"`
	}
	if err := json.Unmarshal(raw, &cassette); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(cassette.Interactions, &transport.queue); err != nil {
		t.Fatal(err)
	}
	return anthropic.NewClient(option.WithAPIKey("test"), option.WithMaxRetries(0), option.WithHTTPClient(&http.Client{Transport: transport}))
}

func loadMAFixture(t *testing.T) maFixture {
	t.Helper()
	raw, err := os.ReadFile("../spec/fixtures/claude-managed-agents-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var f maFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// consumeMA feeds one stream to the adapter, the way an app reads it.
func consumeMA(ctx context.Context, t *testing.T, s maStream, opts []rudderanthropic.SessionOption) {
	t.Helper()
	read := 0
	stop := func() bool { read++; return s.StopAfter != nil && read >= *s.StopAfter }
	if s.Cassette != "" {
		client := cassetteClient(t, s.Cassette)
		session, err := client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
			Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String("agent_x")},
			EnvironmentID: "env_x",
		})
		if err != nil {
			t.Fatalf("sessions.New: %v", err)
		}
		stream := client.Beta.Sessions.Events.StreamEvents(ctx, session.ID, anthropic.BetaSessionEventStreamParams{})
		events := rudderanthropic.TrackSessionEvents(ctx, stream, append(opts, rudderanthropic.WithSession(session))...)
		for events.Next() {
			if stop() {
				break
			}
		}
		if err := events.Err(); err != nil {
			t.Fatalf("stream: %v", err)
		}
		_ = events.Close()
		return
	}
	events := rudderanthropic.TrackSessionEvents(ctx, &sliceStream{events: decodeEvents(t, s.Events)}, opts...)
	for events.Next() {
		if stop() {
			break
		}
	}
	_ = events.Close()
}

// stableMA drops the values that differ between runs.
func stableMA(sc maScenario, props map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range props {
		switch k {
		case "interaction_id", "latency_ms", "total_paused_ms":
			continue
		case "conversation_id":
			if string(sc.Options.Session) == "null" {
				continue
			}
		}
		out[k] = v
	}
	return out
}

func TestManagedAgentsFixture(t *testing.T) {
	f := loadMAFixture(t)
	for _, sc := range f.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			_, rec := setup(t, rudderstackai.WithToolBreakdown(sc.Options.ToolBreakdown), rudderstackai.WithCaptureContent(sc.Options.CaptureContent))
			resetMA(t)
			t.Cleanup(rudderstackai.ClearPrices)
			p := f.Price
			if err := rudderstackai.RegisterPrice(p.Model, rudderstackai.ModelPrice{Provider: p.Provider, TokenPrice: rudderstackai.TokenPrice{
				InputPerMtok: p.InputPerMtok, OutputPerMtok: p.OutputPerMtok,
				CachedInputPerMtok: rudderstackai.Ptr(p.CachedInputPerMtok), CacheWritePerMtok: rudderstackai.Ptr(p.CacheWritePerMtok),
			}}); err != nil {
				t.Fatal(err)
			}
			opts := []rudderanthropic.SessionOption{rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{UserID: "user_1"})}
			if string(sc.Options.Session) != "null" {
				opts = append(opts, rudderanthropic.WithSession(rawJSON(f.Session)))
			}
			ctx := context.Background()
			for _, s := range sc.Streams {
				consumeMA(ctx, t, s, opts)
			}
			rudderstackai.Flush(ctx)
			rec.mu.Lock()
			msgs := slicesClone(rec.msgs)
			rec.mu.Unlock()
			if len(msgs) != len(sc.Expected) {
				names := make([]string, len(msgs))
				for n, m := range msgs {
					names[n] = m.Event
				}
				t.Fatalf("events %v, want %d", names, len(sc.Expected))
			}
			ids := map[string]string{}
			for n, want := range sc.Expected {
				m := msgs[n]
				raw, _ := json.Marshal(m.Properties)
				var props map[string]any
				_ = json.Unmarshal(raw, &props)
				if err := testkit.ValidateEvent(m.Event, props); err != nil {
					t.Errorf("schema: %v", err)
				}
				if m.Event != want.Event {
					t.Fatalf("event %d: %s, want %s", n, m.Event, want.Event)
				}
				if ctxJSON, _ := json.Marshal(m.Context); !strings.Contains(string(ctxJSON), `"integration":"claude-managed-agents"`) {
					t.Errorf("%s %d: context %s", want.Event, n, ctxJSON)
				}
				if got := stableMA(sc, props); !reflect.DeepEqual(got, want.Properties) {
					gotJSON, _ := json.Marshal(got)
					wantJSON, _ := json.Marshal(want.Properties)
					t.Errorf("%s %d:\n got %s\nwant %s", want.Event, n, gotJSON, wantJSON)
				}
				id, _ := props["interaction_id"].(string)
				if seen, ok := ids[want.Interaction]; ok && seen != id {
					t.Errorf("%s %d: interaction %s, want %s", want.Event, n, id, seen)
				} else if !ok {
					for _, other := range ids {
						if other == id {
							t.Errorf("%s %d: interaction %s reused", want.Event, n, id)
						}
					}
					ids[want.Interaction] = id
				}
			}
		})
	}
}

func slicesClone[T any](s []T) []T { return append([]T(nil), s...) }

func TestManagedAgentsIntegrationTag(t *testing.T) {
	f := loadMAFixture(t)
	_, rec := setup(t)
	resetMA(t)
	sc := f.Scenarios[1] // text_turn_capture
	events := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, sc.Streams[0].Events)}, rudderanthropic.WithSession(withID(f.Session, "sesn_tag")))
	for events.Next() {
	}
	rudderstackai.Flush(context.Background())
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.msgs) != 2 {
		t.Fatalf("%d events", len(rec.msgs))
	}
	for _, m := range rec.msgs {
		raw, _ := json.Marshal(m.Context)
		if !strings.Contains(string(raw), `"integration":"claude-managed-agents"`) {
			t.Errorf("%s: %s", m.Event, raw)
		}
	}
}

func TestManagedAgentsReportsIntoTheContextInteraction(t *testing.T) {
	f := loadMAFixture(t)
	_, rec := setup(t)
	resetMA(t)
	var two maScenario
	for _, sc := range f.Scenarios {
		if sc.Name == "two_turns" {
			two = sc
		}
	}
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	events := rudderanthropic.TrackSessionEvents(ctx, &sliceStream{events: decodeEvents(t, two.Streams[0].Events)}, rudderanthropic.WithSession(withID(f.Session, "sesn_borrowed")))
	for events.Next() {
	}
	if got := rec.events(t); len(got["ai_response_received"]) != 0 {
		t.Fatalf("ended the caller's interaction: %v", got)
	}
	i.End(rudderstackai.EndOptions{})
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 || got[0]["llm_call_count"] != float64(2) || got[0]["response_status"] != "success" {
		t.Fatalf("%v", got)
	}
}

func TestManagedAgentsFailingStreamEndsTheTurnAsAnError(t *testing.T) {
	f := loadMAFixture(t)
	_, rec := setup(t)
	resetMA(t)
	sc := f.Scenarios[1] // text_turn_capture
	failure := errors.New("connection reset")
	src := &sliceStream{events: decodeEvents(t, sc.Streams[0].Events)[:3], err: failure}
	events := rudderanthropic.TrackSessionEvents(context.Background(), src, rudderanthropic.WithSession(withID(f.Session, "sesn_failing")))
	n := 0
	for events.Next() {
		n++
	}
	if n != 3 || !errors.Is(events.Err(), failure) {
		t.Fatalf("read %d, err %v", n, events.Err())
	}
	if err := events.Close(); err != nil || !src.closed {
		t.Fatalf("close: %v %v", err, src.closed)
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 || got[0]["response_status"] != "error" {
		t.Fatalf("%v", got)
	}
}

func TestManagedAgentsOptionsAndMapCap(t *testing.T) {
	f := loadMAFixture(t)
	_, rec := setup(t, rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: 2}))
	resetMA(t)
	var two maScenario
	for _, sc := range f.Scenarios {
		if sc.Name == "two_turns" {
			two = sc
		}
	}
	client := &recorder{}
	events := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, two.Streams[0].Events)},
		rudderanthropic.WithSessionID("sesn_other"), rudderanthropic.WithSessionModel("claude-haiku-5-5"),
		rudderanthropic.WithAgentName("support"), rudderanthropic.WithSessionAnalytics(client))
	var seen []string
	for events.Next() {
		var e struct{ ID string }
		_ = json.Unmarshal([]byte(events.Current().RawJSON()), &e)
		seen = append(seen, e.ID)
	}
	if len(seen) != len(two.Streams[0].Events) {
		t.Fatalf("passed %d of %d events", len(seen), len(two.Streams[0].Events))
	}
	rudderstackai.Flush(context.Background())
	if len(rec.msgs) != 0 {
		t.Fatalf("the global client got %d events; want the session client", len(rec.msgs))
	}
	responses := client.events(t)["ai_response_received"]
	if len(responses) != 2 {
		t.Fatalf("%d answers", len(responses))
	}
	for _, p := range responses {
		usage := p["usage_breakdown"].([]any)[0].(map[string]any)
		if p["conversation_id"] != "sesn_other" || usage["agent_name"] != "support" || usage["model"] != "claude-haiku-5-5" {
			t.Errorf("%v", p)
		}
	}
}

// withID is the fixture session under another ID: event IDs dedupe per session.
func withID(session json.RawMessage, id string) rawJSON {
	return rawJSON(strings.Replace(string(session), `"sesn_fixture"`, strconv.Quote(id), 1))
}

func TestManagedAgentsCloseEndsIterationAndTypedNilSession(t *testing.T) {
	f := loadMAFixture(t)
	_, rec := setup(t)
	resetMA(t)
	var nilSession *anthropic.BetaManagedAgentsSession
	src := &sliceStream{events: decodeEvents(t, f.Scenarios[1].Streams[0].Events)}
	events := rudderanthropic.TrackSessionEvents(context.Background(), src,
		rudderanthropic.WithSession(nilSession), rudderanthropic.WithSessionID("sesn_close"))
	for range 2 {
		if !events.Next() {
			t.Fatal("no events")
		}
	}
	if err := events.Close(); err != nil {
		t.Fatal(err)
	}
	if events.Next() {
		t.Fatal("Next after Close")
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 || got[0]["response_status"] != "cancelled" {
		t.Fatalf("%v", got)
	}
}

func TestManagedAgentsModelOptionWinsOverTheSession(t *testing.T) {
	f := loadMAFixture(t)
	_, rec := setup(t)
	resetMA(t)
	events := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, f.Scenarios[1].Streams[0].Events)},
		rudderanthropic.WithSessionModel("claude-opus-5-5"), rudderanthropic.WithSession(withID(f.Session, "sesn_model")))
	for events.Next() {
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 || got[0]["primary_model"] != "claude-opus-5-5" {
		t.Fatalf("%v", got)
	}
}

func TestManagedAgentsSpillsCallsAndToolsPastTheCap(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithLimits(rudderstackai.Limits{MaxCallsPerInteraction: 2}))
	resetMA(t)
	at := `"processed_at":"2026-11-16T10:00:01.000Z"`
	raw := []json.RawMessage{json.RawMessage(`{"id":"sevt_fold_u","type":"user.message","content":[],` + at + `}`)}
	for n := range 5 {
		raw = append(raw,
			json.RawMessage(fmt.Sprintf(`{"id":"sevt_fold_c%d","type":"span.model_request_end","is_error":false,"model_usage":{"input_tokens":10,"output_tokens":1},%s}`, n, at)),
			json.RawMessage(fmt.Sprintf(`{"id":"sevt_fold_t%d","type":"agent.tool_use","name":"bash","input":{},%s}`, n, at)))
	}
	raw = append(raw, json.RawMessage(`{"id":"sevt_fold_i","type":"session.status_idle","stop_reason":{"type":"end_turn"},`+at+`}`))
	events := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, raw)}, rudderanthropic.WithSessionID("sesn_fold"))
	for events.Next() {
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 || got[0]["llm_call_count"] != float64(5) || got[0]["tool_call_count"] != float64(5) || got[0]["total_input_tokens"] != float64(50) {
		t.Fatalf("%v", got)
	}
}

// resetMA forgets every session's state, so a test reads its sessions afresh under -count.
func resetMA(t *testing.T) {
	t.Helper()
	rudderanthropic.ResetManagedAgents()
	t.Cleanup(rudderanthropic.ResetManagedAgents)
}

func maEvents(t *testing.T, raw ...string) *sliceStream {
	t.Helper()
	list := make([]json.RawMessage, len(raw))
	for n, r := range raw {
		list[n] = json.RawMessage(r)
	}
	return &sliceStream{events: decodeEvents(t, list)}
}

func TestManagedAgentsPricesEachCallPastTheCapOnItsOwn(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithLimits(rudderstackai.Limits{MaxCallsPerInteraction: 2}))
	resetMA(t)
	t.Cleanup(rudderstackai.ClearPrices)
	if err := rudderstackai.RegisterPrice("claude-long", rudderstackai.ModelPrice{
		Provider: "anthropic", TokenPrice: rudderstackai.TokenPrice{InputPerMtok: 1, OutputPerMtok: 1},
		LongContext: &rudderstackai.LongContextPrice{ThresholdTokens: 200_000, TokenPrice: rudderstackai.TokenPrice{InputPerMtok: 2, OutputPerMtok: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	at := `"processed_at":"2026-11-16T10:00:01.000Z"`
	raw := []string{`{"id":"sevt_long_u","type":"user.message","content":[],` + at + `}`}
	for n := range 5 {
		raw = append(raw, fmt.Sprintf(`{"id":"sevt_long_%d","type":"span.model_request_end","is_error":false,"model_usage":{"input_tokens":150000,"output_tokens":0},%s}`, n, at))
	}
	raw = append(raw, `{"id":"sevt_long_i","type":"session.status_idle","stop_reason":{"type":"end_turn"},`+at+`}`)
	events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t, raw...),
		rudderanthropic.WithSessionID("sesn_long"), rudderanthropic.WithSessionModel("claude-long"))
	for events.Next() {
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 || got[0]["llm_call_count"] != float64(5) || got[0]["total_cost_usd"] != 0.75 {
		t.Fatalf("%v", got)
	}
}

func TestManagedAgentsSettlesToolsRecordedPastTheCap(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithLimits(rudderstackai.Limits{MaxCallsPerInteraction: 2}))
	resetMA(t)
	at := func(n int) string { return fmt.Sprintf(`"processed_at":"2026-11-16T10:00:0%d.000Z"`, n) }
	raw := []string{`{"id":"sevt_spill_u","type":"user.message","content":[],` + at(1) + `}`}
	for n := range 3 {
		raw = append(raw, fmt.Sprintf(`{"id":"sevt_spill_t%d","type":"agent.tool_use","name":"bash","input":{},%s}`, n, at(1)))
	}
	for n := range 3 {
		raw = append(raw, fmt.Sprintf(`{"id":"sevt_spill_r%d","type":"agent.tool_result","tool_use_id":"sevt_spill_t%d","is_error":%t,%s}`, n, n, n == 0, at(2)))
	}
	raw = append(raw, `{"id":"sevt_spill_i","type":"session.status_idle","stop_reason":{"type":"end_turn"},`+at(3)+`}`)
	events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t, raw...), rudderanthropic.WithSessionID("sesn_spill"))
	for events.Next() {
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 || got[0]["tool_call_count"] != float64(3) {
		t.Fatalf("%v", got)
	}
	breakdown := got[0]["tool_breakdown"].([]any)[0].(map[string]any)
	if breakdown["call_count"] != float64(3) || breakdown["error_count"] != float64(1) {
		t.Fatalf("%v", breakdown)
	}
}

func TestManagedAgentsIDLessToolsExposeNoLocalKey(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithCaptureContent(true))
	resetMA(t)
	at := `"processed_at":"2026-11-16T10:00:01.000Z"`
	events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t,
		`{"id":"sevt_noid_u","type":"user.message","content":[],`+at+`}`,
		`{"type":"agent.tool_use","name":"bash","input":{},`+at+`}`,
		`{"id":"sevt_noid_i","type":"session.status_idle","stop_reason":{"type":"end_turn"},`+at+`}`,
	), rudderanthropic.WithSessionID("sesn_noid"))
	for events.Next() {
	}
	got := rec.events(t)
	raw, _ := json.Marshal(got)
	if len(got["ai_response_received"]) != 1 || got["ai_response_received"][0]["tool_call_count"] != float64(1) || strings.Contains(string(raw), "local:") {
		t.Fatalf("%s", raw)
	}
}

func TestManagedAgentsKeepsAResumedSegmentReadFromHistoryToItsOwnLatency(t *testing.T) {
	f := loadMAFixture(t)
	_, rec := setup(t)
	resetMA(t)
	var sc maScenario
	for _, s := range f.Scenarios {
		if s.Name == "custom_tool_pause_resume" {
			sc = s
		}
	}
	session := rudderanthropic.WithSession(withID(f.Session, "sesn_history"))
	for n, s := range sc.Streams {
		if n > 0 {
			time.Sleep(800 * time.Millisecond) // the app reads the rest from history later
		}
		events := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, s.Events)}, session)
		for events.Next() {
		}
		_ = events.Close()
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 2 {
		t.Fatalf("%v", got)
	}
	// The segment ran 610 ms on the server (01.810 -> 02.420); the rest of the wait was paused.
	latency, paused := got[1]["latency_ms"].(float64), got[1]["total_paused_ms"].(float64)
	if latency < 600 || latency > 700 || paused > 300 {
		t.Fatalf("latency %v, paused %v", latency, paused)
	}
}
