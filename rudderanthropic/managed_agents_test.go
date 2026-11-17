package rudderanthropic_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	analytics "github.com/rudderlabs/analytics-go/v4"

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
		MaxMapEntries  int             `json:"max_map_entries"`
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
			cfg := []rudderstackai.ConfigOption{rudderstackai.WithToolBreakdown(sc.Options.ToolBreakdown), rudderstackai.WithCaptureContent(sc.Options.CaptureContent)}
			if n := sc.Options.MaxMapEntries; n > 0 {
				cfg = append(cfg, rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: n}))
			}
			_, rec := setup(t, cfg...)
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
	// The segment ran 610 ms on the server (01.810 -> 02.420); the rest of the 800 ms wait was paused.
	// Real time spent reading adds to either; without the backdate latency is near 0 and paused 800+.
	latency, paused := got[1]["latency_ms"].(float64), got[1]["total_paused_ms"].(float64)
	if latency < 600 || latency > 1600 || paused > 600 {
		t.Fatalf("latency %v, paused %v", latency, paused)
	}
}

// hookClient is an analytics client that runs a user hook as each event is enqueued.
type hookClient struct {
	*recorder
	hook func(analytics.Track)
}

func (h *hookClient) Enqueue(m analytics.Message) error {
	if tr, ok := m.(analytics.Track); ok && h.hook != nil {
		h.hook(tr)
	}
	return h.recorder.Enqueue(m)
}

// maFixtureScenario is the named scenario of the shared fixture.
func maFixtureScenario(t *testing.T, name string) (maFixture, maScenario) {
	t.Helper()
	f := loadMAFixture(t)
	for _, s := range f.Scenarios {
		if s.Name == name {
			return f, s
		}
	}
	t.Fatalf("no scenario %s", name)
	return f, maScenario{}
}

// maCappedTurn is a user.message and three calls, one second apart.
func maCappedTurn(t *testing.T, prefix string) *sliceStream {
	t.Helper()
	raw := []string{fmt.Sprintf(`{"id":"%s_u","type":"user.message","content":[],"processed_at":"2026-11-16T10:00:00.000Z"}`, prefix)}
	for n := 1; n <= 3; n++ {
		raw = append(raw, fmt.Sprintf(`{"id":"%s_%d","type":"span.model_request_end","is_error":false,"model_usage":{"input_tokens":1,"output_tokens":1},"processed_at":"2026-11-16T10:00:0%d.000Z"}`, prefix, n, n))
	}
	return maEvents(t, raw...)
}

func TestManagedAgentsWaitsForTheEventBeingReadWhenAHookClosesTheStream(t *testing.T) {
	rec := &recorder{}
	hooked := &hookClient{recorder: rec}
	setup(t, rudderstackai.WithAnalytics(hooked), rudderstackai.WithLimits(rudderstackai.Limits{MaxCallsPerInteraction: 2}))
	resetMA(t)
	events := rudderanthropic.TrackSessionEvents(context.Background(), maCappedTurn(t, "sevt_hook"), rudderanthropic.WithSessionID("sesn_hook_close"))
	// The spill at the second call opens the answer: its prompt event's hook closes the stream.
	hooked.hook = func(tr analytics.Track) {
		if tr.Event == "ai_user_prompt_created" {
			_ = events.Close()
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for events.Next() {
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: Close from a hook blocked on the event being read")
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 || got[0]["response_status"] != "cancelled" || got[0]["llm_call_count"] != float64(2) {
		t.Fatalf("%v", got)
	}
}

func TestManagedAgentsCloseFromAnotherGoroutineDoesNotWaitForAHook(t *testing.T) {
	rec := &recorder{}
	hooked := &hookClient{recorder: rec}
	setup(t, rudderstackai.WithAnalytics(hooked), rudderstackai.WithLimits(rudderstackai.Limits{MaxCallsPerInteraction: 2}))
	resetMA(t)
	events := rudderanthropic.TrackSessionEvents(context.Background(), maCappedTurn(t, "sevt_hookg"), rudderanthropic.WithSessionID("sesn_hook_goroutine"))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hooked.hook = func(tr analytics.Track) {
		if tr.Event == "ai_user_prompt_created" {
			once.Do(func() { close(entered); <-release })
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for events.Next() {
		}
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- events.Close() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("Close blocked while a hook ran")
	}
	close(release)
	<-done
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 || got[0]["response_status"] != "cancelled" {
		t.Fatalf("%v", got)
	}
}

func TestManagedAgentsLeavesTheEventsUntrackedWhenAnOptionPanics(t *testing.T) {
	_, sc := maFixtureScenario(t, "text_turn_capture")
	_, rec := setup(t)
	resetMA(t)
	events := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, sc.Streams[0].Events)},
		rudderanthropic.PanickingOption())
	n := 0
	for events.Next() {
		n++
	}
	if n != len(sc.Streams[0].Events) || len(rec.events(t)) != 0 {
		t.Fatalf("read %d, events %v", n, rec.events(t))
	}
}

func TestManagedAgentsKeepsAWaitingToolOutOfASpillSoTheResumedSegmentCountsIt(t *testing.T) {
	f, sc := maFixtureScenario(t, "custom_tool_error_after_resume")
	_, rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithLimits(rudderstackai.Limits{MaxCallsPerInteraction: 1}))
	resetMA(t)
	session := rudderanthropic.WithSession(withID(f.Session, "sesn_spill_pause"))
	for _, s := range sc.Streams {
		events := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, s.Events)}, session)
		for events.Next() {
		}
		_ = events.Close()
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 2 || got[0]["tool_call_count"] != float64(0) || got[1]["tool_call_count"] != float64(1) {
		t.Fatalf("%v", got)
	}
	breakdown := got[1]["tool_breakdown"].([]any)[0].(map[string]any)
	if breakdown["call_count"] != float64(1) || breakdown["error_count"] != float64(1) || breakdown["duration_ms"] != float64(1600) {
		t.Fatalf("%v", breakdown)
	}
}

func TestManagedAgentsKeepsAHistoryTurnsLatencyAfterASpill(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithLimits(rudderstackai.Limits{MaxCallsPerInteraction: 2}))
	resetMA(t)
	at := func(n int) string { return fmt.Sprintf(`"processed_at":"2026-11-16T10:00:0%d.000Z"`, n) }
	raw := []string{`{"id":"sevt_hist_u","type":"user.message","content":[],` + at(0) + `}`}
	for n := 1; n <= 5; n++ {
		raw = append(raw, fmt.Sprintf(`{"id":"sevt_hist_%d","type":"span.model_request_end","is_error":false,"model_usage":{"input_tokens":1,"output_tokens":1},%s}`, n, at(n)))
	}
	raw = append(raw, `{"id":"sevt_hist_i","type":"session.status_idle","stop_reason":{"type":"end_turn"},`+at(6)+`}`)
	events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t, raw...), rudderanthropic.WithSessionID("sesn_hist_spill"))
	for events.Next() {
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 {
		t.Fatalf("%v", got)
	}
	if latency := got[0]["latency_ms"].(float64); latency < 5990 || latency >= 6500 {
		t.Fatalf("latency %v", latency)
	}
}

func TestManagedAgentsKeepsAResumedHistorySegmentsLatencyAfterASpill(t *testing.T) {
	f, sc := maFixtureScenario(t, "custom_tool_pause_resume")
	_, rec := setup(t, rudderstackai.WithLimits(rudderstackai.Limits{MaxCallsPerInteraction: 1}))
	resetMA(t)
	session := rudderanthropic.WithSession(withID(f.Session, "sesn_hist_resume_spill"))
	// A fake clock: the 60 s between the streams cost no real time.
	synctest.Test(t, func(t *testing.T) {
		for n, s := range sc.Streams {
			if n > 0 {
				time.Sleep(60 * time.Second) // the app reads the rest from history later
			}
			events := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, s.Events)}, session)
			for events.Next() {
			}
			_ = events.Close()
		}
	})
	got := rec.events(t)["ai_response_received"]
	if len(got) != 2 {
		t.Fatalf("%v", got)
	}
	// The segment ran 610 ms on the server; the rest of the 60 s wait stays paused.
	latency, paused := got[1]["latency_ms"].(float64), got[1]["total_paused_ms"].(float64)
	if latency < 600 || latency > 1100 || paused < 59390 || paused > 59800 {
		t.Fatalf("latency %v, paused %v", latency, paused)
	}
}

func TestManagedAgentsEndsATurnTheAppsOwnExceptionStoppedAsAppError(t *testing.T) {
	_, sc := maFixtureScenario(t, "text_turn_capture")
	_, rec := setup(t)
	resetMA(t)
	src := &sliceStream{events: decodeEvents(t, sc.Streams[0].Events)[:3], err: errors.New("boom")}
	events := rudderanthropic.TrackSessionEvents(context.Background(), src, rudderanthropic.WithSessionID("sesn_app_error"))
	for events.Next() {
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 || got[0]["response_status"] != "error" || got[0]["error_type"] != "app_error" {
		t.Fatalf("%v", got)
	}
}

func TestManagedAgentsRecordsABorrowedAnswersWorkAsItArrivesSoTheCallersPauseHoldsIt(t *testing.T) {
	f, sc := maFixtureScenario(t, "custom_tool_pause_resume")
	_, rec := setup(t, rudderstackai.WithToolBreakdown(true))
	resetMA(t)
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	// running, user, request start, request end, tool use: the idle is never read.
	events := rudderanthropic.TrackSessionEvents(ctx, &sliceStream{events: decodeEvents(t, sc.Streams[0].Events[:5])}, rudderanthropic.WithSession(withID(f.Session, "sesn_borrowed_pause")))
	// Read exactly five: reaching the stream's end would settle the turn into the answer anyway.
	for range 5 {
		if !events.Next() {
			t.Fatal("stream ended early")
		}
	}
	if _, ok := i.Pause(rudderstackai.PauseOptions{}); !ok {
		t.Fatal("not paused")
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 || got[0]["response_status"] != "paused" || got[0]["llm_call_count"] != float64(1) ||
		got[0]["tool_call_count"] != float64(1) || got[0]["total_input_tokens"] != float64(30) || got[0]["total_output_tokens"] != float64(8) {
		t.Fatalf("%v", got)
	}
	// The paused segment holds the work; the resumed one counts nothing twice.
	_ = events.Close()
	i.Resume()
	i.End(rudderstackai.EndOptions{Status: rudderstackai.StatusSuccess})
	got = rec.events(t)["ai_response_received"]
	if len(got) != 2 || got[1]["llm_call_count"] != float64(0) || got[1]["tool_call_count"] != float64(0) || got[1]["total_input_tokens"] != float64(0) {
		t.Fatalf("%v", got)
	}
}

func TestManagedAgentsResumesAPausedAnswerReadInsideAnotherCallersAmbientInteractionAsItself(t *testing.T) {
	f, sc := maFixtureScenario(t, "custom_tool_pause_resume")
	_, rec := setup(t, rudderstackai.WithToolBreakdown(true))
	resetMA(t)
	session := rudderanthropic.WithSession(withID(f.Session, "sesn_resume_owner"))
	owner := rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{UserID: "owner"})
	read := func(ctx context.Context, n int, opts ...rudderanthropic.SessionOption) {
		events := rudderanthropic.TrackSessionEvents(ctx, &sliceStream{events: decodeEvents(t, sc.Streams[n].Events)}, append([]rudderanthropic.SessionOption{session}, opts...)...)
		for events.Next() {
		}
		_ = events.Close()
	}
	read(context.Background(), 0, owner)
	ctx, caller := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "other", NoPromptEvent: true})
	read(ctx, 1)
	caller.End(rudderstackai.EndOptions{Status: rudderstackai.StatusSuccess})
	rec.events(t) // flush
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var status, ids, tools, otherTools []any
	for _, m := range rec.msgs {
		if m.Event != "ai_response_received" {
			continue
		}
		switch m.UserId {
		case "owner":
			status, ids, tools = append(status, m.Properties["response_status"]), append(ids, m.Properties["interaction_id"]), append(tools, m.Properties["tool_call_count"])
		case "other":
			otherTools = append(otherTools, m.Properties["tool_call_count"])
		}
	}
	if fmt.Sprint(status) != "[paused success]" || len(ids) != 2 || ids[0] != ids[1] || fmt.Sprint(tools) != "[0 1]" || fmt.Sprint(otherTools) != "[0]" {
		t.Fatalf("status %v ids %v tools %v other %v", status, ids, tools, otherTools)
	}
}

func TestManagedAgentsKeepsTheOptionsNestedObjectsAsTheyWereWhenTheStreamWasWrapped(t *testing.T) {
	f, sc := maFixtureScenario(t, "text_turn_capture")
	_, rec := setup(t, rudderstackai.WithIdentityFromCookies(true))
	resetMA(t)
	policy := &rudderstackai.ContentCapture{Default: rudderstackai.Ptr(false)}
	cookies := map[string]string{"rl_anonymous_id": "%22original%22"}
	attachmentTypes := []string{"image"}
	events := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, sc.Streams[0].Events)},
		rudderanthropic.WithSession(withID(f.Session, "sesn_nested_options")),
		rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{
			ContentCapture: policy, CookieValues: cookies, AttachmentCount: rudderstackai.Ptr(1), AttachmentTypes: attachmentTypes,
		}))
	*policy.Default = true
	cookies["rl_anonymous_id"] = "%22later%22"
	attachmentTypes[0] = "audio"
	for events.Next() {
	}
	_ = events.Close()
	got := rec.events(t)
	rec.mu.Lock()
	for _, m := range rec.msgs {
		if m.AnonymousId != "original" {
			t.Errorf("%s anonymousId %q", m.Event, m.AnonymousId)
		}
	}
	rec.mu.Unlock()
	if types := fmt.Sprint(got["ai_user_prompt_created"][0]["attachment_types"]); types != "[image]" {
		t.Errorf("attachment_types %s", types)
	}
	if r := got["ai_response_received"][0]; r["prompt_text"] != nil || r["response_text"] != nil {
		t.Errorf("captured text: %v", r)
	}
}

func TestManagedAgentsTimesASpilledDenialFromTheToolUse(t *testing.T) {
	f, sc := maFixtureScenario(t, "denied_after_wait")
	_, rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithLimits(rudderstackai.Limits{MaxCallsPerInteraction: 1}))
	resetMA(t)
	session := rudderanthropic.WithSession(withID(f.Session, "sesn_denied_spill"))
	for _, s := range sc.Streams {
		events := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, s.Events)}, session)
		for events.Next() {
		}
		_ = events.Close()
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 2 {
		t.Fatalf("%v", got)
	}
	breakdown := got[1]["tool_breakdown"].([]any)
	entry := breakdown[0].(map[string]any)
	if len(breakdown) != 1 || entry["duration_ms"] != float64(10_000) || entry["error_types"].(map[string]any)["denied"] != float64(1) {
		t.Fatalf("%v", breakdown)
	}
}

func TestManagedAgentsGivesAToolRefusedUpFrontNoDuration(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithToolBreakdown(true))
	resetMA(t)
	events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t,
		`{"id":"sevt_refused_1","type":"session.status_running","processed_at":"2026-11-16T10:00:00.000Z"}`,
		`{"id":"sevt_refused_2","type":"user.message","content":[],"processed_at":"2026-11-16T10:00:00.100Z"}`,
		`{"id":"sevt_refused_3","type":"agent.tool_use","name":"bash","evaluated_permission":"deny","processed_at":"2026-11-16T10:00:00.200Z"}`,
		`{"id":"sevt_refused_4","type":"session.status_idle","stop_reason":{"type":"end_turn"},"processed_at":"2026-11-16T10:00:00.900Z"}`,
	), rudderanthropic.WithSessionID("sesn_refused"))
	for events.Next() {
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 {
		t.Fatalf("%v", got)
	}
	entry := got[0]["tool_breakdown"].([]any)[0].(map[string]any)
	if _, has := entry["duration_ms"]; has || entry["error_types"].(map[string]any)["denied"] != float64(1) {
		t.Fatalf("%v", entry)
	}
}

func TestManagedAgentsReadsAToolResultAsTheAnthropicWrapperDoes(t *testing.T) {
	var seen any
	rules := rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{"weather": {
		Event: "weather_read",
		When:  func(_ map[string]any, result any) bool { seen = result; return true },
	}}}
	setup(t, rudderstackai.WithEventRules(rules))
	resetMA(t)
	events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t,
		`{"id":"sevt_decode_1","type":"session.status_running","processed_at":"2026-11-16T10:00:00.000Z"}`,
		`{"id":"sevt_decode_2","type":"user.message","content":[],"processed_at":"2026-11-16T10:00:00.100Z"}`,
		`{"id":"sevt_decode_3","type":"agent.tool_use","name":"weather","input":{},"processed_at":"2026-11-16T10:00:00.200Z"}`,
		`{"id":"sevt_decode_4","type":"agent.tool_result","tool_use_id":"sevt_decode_3","is_error":false,"content":[{"type":"text","text":"{\"temperature\":21}"}],"processed_at":"2026-11-16T10:00:00.300Z"}`,
		`{"id":"sevt_decode_5","type":"session.status_idle","stop_reason":{"type":"end_turn"},"processed_at":"2026-11-16T10:00:00.900Z"}`,
	), rudderanthropic.WithSessionID("sesn_decode"))
	for events.Next() {
	}
	if got, ok := seen.(map[string]any); !ok || got["temperature"] != float64(21) {
		t.Fatalf("rule saw %#v, want the parsed object", seen)
	}
}

// enteringStream is an SDK stream that reports when a Next starts waiting for its second event.
type enteringStream struct {
	*ssestream.Stream[anthropic.BetaManagedAgentsStreamSessionEventsUnion]
	reads   int
	waiting chan struct{}
}

func (e *enteringStream) Next() bool {
	if e.reads++; e.reads == 2 {
		close(e.waiting)
	}
	return e.Stream.Next()
}

func TestManagedAgentsCloseReleasesANextWaitingOnTheSDKStream(t *testing.T) {
	_, rec := setup(t)
	resetMA(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: user.message\ndata: {\"id\":\"sevt_wait_u\",\"type\":\"user.message\",\"content\":[]}\n\n")
		w.(http.Flusher).Flush()
		// No second event: the client's read waits until it closes the stream.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	client := anthropic.NewClient(option.WithAPIKey("test"), option.WithMaxRetries(0), option.WithBaseURL(srv.URL))
	stream := &enteringStream{
		Stream:  client.Beta.Sessions.Events.StreamEvents(context.Background(), "sesn_wait", anthropic.BetaSessionEventStreamParams{}),
		waiting: make(chan struct{}),
	}
	events := rudderanthropic.TrackSessionEvents(context.Background(), stream, rudderanthropic.WithSessionID("sesn_wait"))
	done := make(chan int, 1)
	go func() {
		n := 0
		for events.Next() {
			n++
		}
		done <- n
	}()
	select {
	case <-stream.waiting:
	case n := <-done:
		t.Fatalf("stream ended after %d events: %v", n, events.Err())
	}
	time.Sleep(50 * time.Millisecond) // the read blocks on the response body
	closed := make(chan error, 1)
	go func() { closed <- events.Close() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung while a Next waited")
	}
	select {
	case n := <-done:
		if n != 1 {
			t.Fatalf("read %d events", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not release the waiting Next")
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 || got[0]["response_status"] != "cancelled" {
		t.Fatalf("%v", got)
	}
}

// panickingStream panics in Current at its second event, or in Err at its end.
type panickingStream struct {
	*sliceStream
	in string
}

func (p *panickingStream) Current() anthropic.BetaManagedAgentsStreamSessionEventsUnion {
	if p.in == "Current" && p.n == 2 {
		panic("decode failed")
	}
	return p.sliceStream.Current()
}

func (p *panickingStream) Err() error {
	if p.in == "Err" {
		panic("err failed")
	}
	return p.sliceStream.Err()
}

func TestManagedAgentsReleasesItsLockWhenTheSourcePanics(t *testing.T) {
	for _, in := range []string{"Current", "Err"} {
		t.Run(in, func(t *testing.T) {
			_, rec := setup(t)
			resetMA(t)
			src := &panickingStream{in: in, sliceStream: maEvents(t,
				`{"id":"sevt_panic_u","type":"user.message","content":[]}`,
				`{"id":"sevt_panic_c","type":"span.model_request_end","is_error":false,"model_usage":{"input_tokens":1,"output_tokens":1}}`,
			)}
			events := rudderanthropic.TrackSessionEvents(context.Background(), src, rudderanthropic.WithSessionID("sesn_panic_"+in))
			panicked := false
			func() {
				defer func() { panicked = recover() != nil }()
				for events.Next() {
				}
			}()
			if !panicked {
				t.Fatal("the source's panic did not reach the caller")
			}
			_ = events.Close()
			next := make(chan bool, 1)
			go func() { next <- events.Next() }()
			select {
			case more := <-next:
				if more {
					t.Fatal("Next after Close")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Next blocked on the lock the panic left held")
			}
			got := rec.events(t)["ai_response_received"]
			if len(got) != 1 || got[0]["response_status"] != "cancelled" {
				t.Fatalf("%v", got)
			}
		})
	}
}

func TestManagedAgentsSettlesToolsAcrossAReconnectInTheCallersInteraction(t *testing.T) {
	f, sc := maFixtureScenario(t, "custom_tool_error_after_resume")
	_, rec := setup(t, rudderstackai.WithToolBreakdown(true))
	resetMA(t)
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	session := rudderanthropic.WithSession(withID(f.Session, "sesn_borrowed_reconnect"))
	for _, s := range sc.Streams {
		events := rudderanthropic.TrackSessionEvents(ctx, &sliceStream{events: decodeEvents(t, s.Events)}, session)
		for events.Next() {
		}
		_ = events.Close()
	}
	i.End(rudderstackai.EndOptions{Status: rudderstackai.StatusSuccess})
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 {
		t.Fatalf("%v", got)
	}
	breakdown := got[0]["tool_breakdown"].([]any)
	entry := breakdown[0].(map[string]any)
	if len(breakdown) != 1 || entry["call_count"] != float64(1) || entry["error_count"] != float64(1) || entry["duration_ms"] != float64(1600) {
		t.Fatalf("%v", breakdown)
	}
}

// gatedStream holds its second event until gate closes; waiting closes when a read waits for it.
type gatedStream struct {
	*sliceStream
	gate, waiting chan struct{}
}

func (g *gatedStream) Next() bool {
	if g.n == 1 {
		close(g.waiting)
		<-g.gate
	}
	return g.sliceStream.Next()
}

func TestManagedAgentsOpensNoAnswerForAReadThatResolvesAfterClose(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithLimits(rudderstackai.Limits{MaxCallsPerInteraction: 1}))
	resetMA(t)
	src := &gatedStream{sliceStream: maEvents(t,
		`{"id":"sevt_late_u","type":"user.message","content":[]}`,
		`{"id":"sevt_late_c","type":"span.model_request_end","is_error":false,"model_usage":{"input_tokens":1,"output_tokens":1}}`,
	), gate: make(chan struct{}), waiting: make(chan struct{})}
	events := rudderanthropic.TrackSessionEvents(context.Background(), src, rudderanthropic.WithSessionID("sesn_late_read"))
	if !events.Next() {
		t.Fatal("no events")
	}
	pending := make(chan bool, 1)
	go func() { pending <- events.Next() }()
	<-src.waiting
	_ = events.Close()
	close(src.gate)
	if <-pending {
		t.Fatal("Next passed an event read after Close")
	}
	rudderstackai.Flush(context.Background())
	rec.mu.Lock()
	var names []string
	for _, m := range rec.msgs {
		names = append(names, m.Event)
	}
	rec.mu.Unlock()
	if !reflect.DeepEqual(names, []string{"ai_user_prompt_created", "ai_response_received"}) {
		t.Fatalf("%v", names)
	}
}

func TestManagedAgentsEndsATurnAStreamTimeoutStoppedWithTheTimeoutStatus(t *testing.T) {
	_, sc := maFixtureScenario(t, "text_turn_capture")
	_, rec := setup(t)
	resetMA(t)
	src := &sliceStream{events: decodeEvents(t, sc.Streams[0].Events)[:3], err: context.DeadlineExceeded}
	events := rudderanthropic.TrackSessionEvents(context.Background(), src, rudderanthropic.WithSessionID("sesn_timeout"))
	for events.Next() {
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 || got[0]["response_status"] != "timeout" || got[0]["error_type"] != "timeout" {
		t.Fatalf("%v", got)
	}
}

func TestManagedAgentsKeepsAtMostMaxMapEntriesEventIDsForTheWholeProcess(t *testing.T) {
	f := loadMAFixture(t)
	var sc maScenario
	for _, s := range f.Scenarios {
		if s.Name == "text_turn_capture" {
			sc = s
		}
	}
	events := sc.Streams[0].Events
	// One session's events fit; a second session's push the first one's oldest out.
	_, rec := setup(t, rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: len(events) + 2}))
	resetMA(t)
	for _, id := range []string{"sesn_ids_a", "sesn_ids_b", "sesn_ids_a"} {
		tracked := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, events)}, rudderanthropic.WithSessionID(id))
		for tracked.Next() {
		}
	}
	if got := rec.events(t)["ai_response_received"]; len(got) != 3 {
		t.Fatalf("%d answers, want 3", len(got))
	}
}

func TestManagedAgentsEndsATurnWhoseStreamEndedAfterATerminalSessionErrorAsThatError(t *testing.T) {
	_, sc := maFixtureScenario(t, "paused_then_error")
	_, rec := setup(t)
	resetMA(t)
	first := sc.Streams[0].Events
	var terminal []json.RawMessage
	for _, raw := range sc.Streams[1].Events {
		var e struct{ Type string }
		_ = json.Unmarshal(raw, &e)
		if e.Type == "session.error" {
			terminal = append(terminal, raw)
		}
	}
	read := func(session string, raw []json.RawMessage) {
		events := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, raw)}, rudderanthropic.WithSessionID(session))
		for events.Next() {
		}
	}
	// Fresh: the error ends the stream mid-turn. Resumed: a paused session's later stream.
	read("sesn_eof_fresh", append(slicesClone(first[:2]), terminal...))
	read("sesn_eof_resumed", first)
	read("sesn_eof_resumed", terminal)
	var ends [][2]any
	for _, p := range rec.events(t)["ai_response_received"] {
		ends = append(ends, [2]any{p["response_status"], p["error_type"]})
	}
	if want := [][2]any{{"error", "connection_error"}, {"paused", nil}, {"error", "connection_error"}}; !reflect.DeepEqual(ends, want) {
		t.Fatalf("%v", ends)
	}
}

func TestManagedAgentsKeepsAHistoryTurnsLatencyWhenTheReaderFallsBehindAfterASpill(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithLimits(rudderstackai.Limits{MaxCallsPerInteraction: 1}))
	resetMA(t)
	at := func(n int) string { return fmt.Sprintf(`"processed_at":"2026-11-16T10:00:0%d.000Z"`, n) }
	// A fake clock: the minute the app takes to read the idle costs no real time.
	synctest.Test(t, func(t *testing.T) {
		events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t,
			`{"id":"sevt_stall_u","type":"user.message","content":[],`+at(0)+`}`,
			`{"id":"sevt_stall_c","type":"span.model_request_end","is_error":false,"model_usage":{"input_tokens":1,"output_tokens":1},`+at(1)+`}`,
			`{"id":"sevt_stall_i","type":"session.status_idle","stop_reason":{"type":"end_turn"},`+at(2)+`}`,
		), rudderanthropic.WithSessionID("sesn_stall"))
		for n := 1; events.Next(); n++ {
			if n == 2 {
				time.Sleep(60 * time.Second) // the app reads the idle a minute after the spill
			}
		}
	})
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 || got[0]["latency_ms"] != float64(2000) {
		t.Fatalf("%v", got)
	}
}

func TestManagedAgentsKnowsASubagentThreadFromItsCrossPostedEventsAlone(t *testing.T) {
	f := loadMAFixture(t)
	_, rec := setup(t)
	resetMA(t)
	session := rudderanthropic.WithSession(withID(f.Session, "sesn_kid"))
	const at = `"processed_at":"2026-11-16T10:00:01.000Z"`
	track := func(events *sliceStream) {
		stream := rudderanthropic.TrackSessionEvents(context.Background(), events, session)
		for stream.Next() {
		}
		_ = stream.Close()
	}
	track(maEvents(t,
		`{"id":"sevt_kid_u","type":"user.message","content":[],`+at+`}`,
		`{"id":"sevt_kid_s","type":"session.thread_status_running","session_thread_id":"sthr_kid",`+at+`}`,
		`{"id":"sevt_kid_x","type":"user.interrupt","session_thread_id":"sthr_kid",`+at+`}`,
		`{"id":"sevt_kid_i","type":"session.status_idle","stop_reason":{"type":"end_turn"},`+at+`}`,
	))
	// A subagent's custom tool, answered without naming its thread, after the turn ended.
	track(maEvents(t,
		`{"id":"ctu_kid","type":"agent.custom_tool_use","name":"lookup","input":{},"session_thread_id":"sthr_kid",`+at+`}`,
		`{"id":"sevt_kid_a","type":"user.custom_tool_result","custom_tool_use_id":"ctu_kid","content":[],`+at+`}`,
	))
	responses := rec.events(t)["ai_response_received"]
	if len(responses) != 1 || responses[0]["response_status"] != "success" {
		t.Fatalf("%v", responses)
	}
}

func TestManagedAgentsOpensNoSecondAnswerWhenTheCallerEndsItsInteractionMidTurn(t *testing.T) {
	f := loadMAFixture(t)
	_, rec := setup(t)
	resetMA(t)
	const at = `"processed_at":"2026-11-16T10:00:01.000Z"`
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	events := rudderanthropic.TrackSessionEvents(ctx, maEvents(t,
		`{"id":"sevt_end_u","type":"user.message","content":[],`+at+`}`,
		`{"id":"sevt_end_c","type":"span.model_request_end","is_error":false,"model_usage":{"input_tokens":1,"output_tokens":1},`+at+`}`,
		`{"id":"sevt_end_i","type":"session.status_idle","stop_reason":{"type":"end_turn"},`+at+`}`,
	), rudderanthropic.WithSession(withID(f.Session, "sesn_caller_end")))
	if !events.Next() {
		t.Fatal("no events")
	}
	i.End(rudderstackai.EndOptions{Status: rudderstackai.StatusSuccess})
	for events.Next() {
	}
	_ = events.Close()
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 || got[0]["llm_call_count"] != float64(0) {
		t.Fatalf("%v", got)
	}
}

// closeCounter is a stream that counts its Close calls.
type closeCounter struct {
	sliceStream
	closes int
}

func (c *closeCounter) Close() error { c.closes++; return nil }

func TestManagedAgentsClosesTheSourceAtMostOnce(t *testing.T) {
	_, _ = setup(t)
	resetMA(t)
	src := &closeCounter{sliceStream: *maEvents(t, `{"id":"sevt_once_u","type":"user.message","content":[]}`)}
	events := rudderanthropic.TrackSessionEvents(context.Background(), src, rudderanthropic.WithSessionID("sesn_once"))
	for events.Next() {
	}
	_ = events.Close()
	_ = events.Close()
	if src.closes != 1 {
		t.Fatalf("closed %d times", src.closes)
	}
}

func TestManagedAgentsEndsTheTurnOfARunningSessionWhoseStreamStopsBeforeAnyOtherEvent(t *testing.T) {
	_, rec := setup(t)
	resetMA(t)
	running := func(id string) []json.RawMessage {
		return []json.RawMessage{json.RawMessage(`{"id":"` + id + `","type":"session.status_running","processed_at":"2026-11-16T10:00:01.000Z"}`)}
	}
	eof := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, running("sevt_only_r1"))}, rudderanthropic.WithSessionID("sesn_only_eof"))
	for eof.Next() {
	}
	failed := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, running("sevt_only_r2")), err: errors.New("stop")}, rudderanthropic.WithSessionID("sesn_only_err"))
	for failed.Next() {
	}
	var ends [][2]any
	for _, p := range rec.events(t)["ai_response_received"] {
		ends = append(ends, [2]any{p["response_status"], p["error_type"]})
	}
	if want := [][2]any{{"cancelled", "cancelled"}, {"error", "app_error"}}; !reflect.DeepEqual(ends, want) {
		t.Fatalf("%v", ends)
	}
}

func TestManagedAgentsKeepsPrimaryModelWhenTheCallerPausesOrEndsRightAfterAGraderCycle(t *testing.T) {
	f, sc := maFixtureScenario(t, "grader_then_failed_call")
	var beforeIdle []json.RawMessage
	for _, raw := range sc.Streams[0].Events {
		var e struct{ Type string }
		_ = json.Unmarshal(raw, &e)
		if e.Type != "session.status_idle" {
			beforeIdle = append(beforeIdle, raw)
		}
	}
	for _, name := range []string{"pause", "end"} {
		_, rec := setup(t)
		resetMA(t)
		ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
		events := rudderanthropic.TrackSessionEvents(ctx, &sliceStream{events: decodeEvents(t, beforeIdle)}, rudderanthropic.WithSession(withID(f.Session, "sesn_grader_"+name)))
		// Read every event but not the stream's end, which would settle the turn into the answer.
		for range beforeIdle {
			if !events.Next() {
				t.Fatal("stream ended early")
			}
		}
		if name == "pause" {
			if _, ok := i.Pause(rudderstackai.PauseOptions{}); !ok {
				t.Fatal("not paused")
			}
		} else {
			i.End(rudderstackai.EndOptions{})
		}
		got := rec.events(t)["ai_response_received"]
		if len(got) != 1 || got[0]["primary_model"] != "claude-haiku-5-5" || got[0]["llm_call_count"] != float64(3) {
			t.Fatalf("%s: %v", name, got)
		}
	}
}

// countingStream numbers an event each time Current is read, like a source whose Current has side effects.
type countingStream struct {
	*sliceStream
	reads int
}

func (c *countingStream) Current() anthropic.BetaManagedAgentsStreamSessionEventsUnion {
	c.reads++
	return c.events[c.reads-1]
}

func TestManagedAgentsReadsTheSourcesCurrentOncePerEvent(t *testing.T) {
	for _, tracked := range []bool{true, false} {
		t.Run(fmt.Sprintf("tracked=%v", tracked), func(t *testing.T) {
			_, rec := setup(t)
			resetMA(t)
			src := &countingStream{sliceStream: maEvents(t,
				`{"id":"sevt_1","type":"user.message","content":[]}`,
				`{"id":"sevt_2","type":"span.model_request_end","is_error":false,"model_usage":{"input_tokens":1,"output_tokens":1}}`,
			)}
			opt := rudderanthropic.WithSessionID("sesn_once")
			if !tracked {
				opt = rudderanthropic.PanickingOption() // leaves the events untracked
			}
			events := rudderanthropic.TrackSessionEvents(context.Background(), src, opt)
			if !events.Next() {
				t.Fatal("no first event")
			}
			var got struct{ ID string }
			_ = json.Unmarshal([]byte(events.Current().RawJSON()), &got)
			if got.ID != "sevt_1" || src.reads != 1 {
				t.Fatalf("app got %q after %d source reads, want sevt_1 after 1", got.ID, src.reads)
			}
			_ = events.Close()
			if !tracked {
				return
			}
			// Tracking saw the user message only: a second read would have recorded a model call.
			res := rec.events(t)["ai_response_received"]
			if len(res) != 1 || res[0]["llm_call_count"] == float64(1) {
				t.Fatalf("tracked a different event than the app got: %v", res)
			}
		})
	}
}

func TestManagedAgentsKeepsTheCapturePolicyATurnOpenedWithWhenTheCallerChangesIt(t *testing.T) {
	_, sc := maFixtureScenario(t, "text_turn_capture")
	_, rec := setup(t)
	resetMA(t)
	off := false
	policy := &rudderstackai.ContentCapture{Default: &off, Spans: &rudderstackai.SpanCapture{Default: rudderstackai.Ptr(false)}}
	on := rudderstackai.Ptr(false)
	events := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, sc.Streams[0].Events)},
		rudderanthropic.WithSessionID("sesn_policy_frozen"),
		rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{CaptureContent: on, ContentCapture: policy}))
	for range 2 { // the user.message opens the turn
		if !events.Next() {
			t.Fatal("stream ended early")
		}
	}
	// Everything the caller passed by pointer, changed to capture all.
	off, *on, *policy.Spans.Default = true, true, true
	for events.Next() {
	}
	got := rec.events(t)
	raw, _ := json.Marshal(got)
	if len(got["ai_response_received"]) != 1 || strings.Contains(string(raw), "prompt_text") || strings.Contains(string(raw), "response_text") {
		t.Fatalf("%s", raw)
	}
}

func TestManagedAgentsCarriesAToolTheSessionWaitsOnAcrossThePauseBuiltInToolsToo(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithToolBreakdown(true))
	resetMA(t)
	at := func(ms int) string {
		return fmt.Sprintf(`"processed_at":%q`, time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC).Add(time.Duration(ms)*time.Millisecond).Format("2006-01-02T15:04:05.000Z"))
	}
	first := maEvents(t,
		`{"id":"sevt_wait_u","type":"user.message","content":[],`+at(0)+`}`,
		`{"id":"sevt_wait_t","type":"agent.tool_use","name":"bash","input":{},"evaluated_permission":"allow",`+at(100)+`}`,
		`{"id":"sevt_wait_i","type":"session.status_idle","stop_reason":{"type":"requires_action","event_ids":["sevt_wait_t"]},`+at(200)+`}`,
	)
	second := maEvents(t,
		`{"id":"sevt_wait_r","type":"user.tool_result","tool_use_id":"sevt_wait_t","is_error":true,`+at(2100)+`}`,
		`{"id":"sevt_wait_e","type":"session.status_idle","stop_reason":{"type":"end_turn"},`+at(2200)+`}`,
	)
	for _, src := range []*sliceStream{first, second} {
		events := rudderanthropic.TrackSessionEvents(context.Background(), src, rudderanthropic.WithSessionID("sesn_wait_builtin"))
		for events.Next() {
		}
		_ = events.Close()
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 2 || got[0]["tool_call_count"] != float64(0) || got[1]["tool_call_count"] != float64(1) {
		t.Fatalf("%v", got)
	}
	breakdown := got[1]["tool_breakdown"].([]any)
	entry := breakdown[0].(map[string]any)
	if len(breakdown) != 1 || entry["tool_name"] != "bash" || entry["call_count"] != float64(1) || entry["error_count"] != float64(1) || entry["duration_ms"] != float64(2000) {
		t.Fatalf("%v", breakdown)
	}
}

func TestManagedAgentsKeepsAResumedSegmentsLatencyWhenHistoryIsReadBackToBack(t *testing.T) {
	f, sc := maFixtureScenario(t, "custom_tool_pause_resume")
	_, rec := setup(t)
	resetMA(t)
	session := rudderanthropic.WithSession(withID(f.Session, "sesn_back_to_back"))
	for _, s := range sc.Streams {
		events := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, s.Events)}, session)
		for events.Next() {
		}
		_ = events.Close()
	}
	got := rec.events(t)["ai_response_received"]
	if len(got) != 2 {
		t.Fatalf("%v", got)
	}
	// The resumed segment ran 610 ms on the server; the pause between the reads was about zero.
	latency, paused := got[1]["latency_ms"].(float64), got[1]["total_paused_ms"].(float64)
	if latency < 605 || latency >= 1000 || paused < 0 {
		t.Fatalf("latency %v, paused %v", latency, paused)
	}
}

func TestManagedAgentsKeepsAToolsOutcomeAcrossTheCallersOwnPauseAndResume(t *testing.T) {
	t.Skip("depends on the edge-cases D2 core change (pending tools across a caller's pause); remove this skip when D2 lands")
	_, rec := setup(t, rudderstackai.WithToolBreakdown(true))
	resetMA(t)
	at := func(ms int) string {
		return fmt.Sprintf(`"processed_at":%q`, time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC).Add(time.Duration(ms)*time.Millisecond).Format("2006-01-02T15:04:05.000Z"))
	}
	first := maEvents(t,
		`{"id":"sevt_cp_u","type":"user.message","content":[],`+at(0)+`}`,
		`{"id":"sevt_cp_t","type":"agent.custom_tool_use","name":"lookup","input":{},`+at(100)+`}`,
		`{"id":"sevt_cp_i","type":"session.status_idle","stop_reason":{"type":"requires_action","event_ids":["sevt_cp_t"]},`+at(200)+`}`,
	)
	second := maEvents(t,
		`{"id":"sevt_cp_r","type":"user.custom_tool_result","custom_tool_use_id":"sevt_cp_t","is_error":true,"content":[],`+at(2100)+`}`,
		`{"id":"sevt_cp_e","type":"session.status_idle","stop_reason":{"type":"end_turn"},`+at(2200)+`}`,
	)
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	session := rudderanthropic.WithSessionID("sesn_caller_pause")
	events := rudderanthropic.TrackSessionEvents(ctx, first, session)
	for events.Next() {
	}
	_ = events.Close()
	i.Pause(rudderstackai.PauseOptions{})
	i.Resume()
	events = rudderanthropic.TrackSessionEvents(ctx, second, session)
	for events.Next() {
	}
	_ = events.Close()
	i.End(rudderstackai.EndOptions{Status: rudderstackai.StatusSuccess})
	got := rec.events(t)["ai_response_received"]
	final := got[len(got)-1]
	breakdown, _ := final["tool_breakdown"].([]any)
	if len(breakdown) != 1 {
		t.Fatalf("%v", final)
	}
	entry := breakdown[0].(map[string]any)
	if entry["tool_name"] != "lookup" || entry["call_count"] != float64(1) || entry["error_count"] != float64(1) || entry["duration_ms"] != float64(2000) {
		t.Fatalf("%v", breakdown)
	}
}

func TestManagedAgentsKeepsTheCapturePolicyInForceWhenTheTurnStartedAlsoGlobalDefaults(t *testing.T) {
	turn := func(prefix string) []string {
		return []string{
			`{"id":"` + prefix + `_r","type":"session.status_running","processed_at":"2026-11-16T10:00:00.000Z"}`,
			`{"id":"` + prefix + `_u","type":"user.message","content":[{"type":"text","text":"customer-secret"}],"processed_at":"2026-11-16T10:00:01.000Z"}`,
			`{"id":"` + prefix + `_a","type":"agent.message","content":[{"type":"text","text":"answer-secret"}],"processed_at":"2026-11-16T10:00:02.000Z"}`,
			`{"id":"` + prefix + `_i","type":"session.status_idle","stop_reason":{"type":"end_turn"},"processed_at":"2026-11-16T10:00:03.000Z"}`,
		}
	}
	drain := func(events interface{ Next() bool }) {
		for events.Next() {
		}
	}
	_, rec := setup(t)
	resetMA(t)
	// The caller's policy changes after the running event started the turn, before the prompt.
	on := rudderstackai.Ptr(false)
	byOption := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t, turn("sevt_pol")...),
		rudderanthropic.WithSessionID("sesn_policy_running"),
		rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{CaptureContent: on}))
	byOption.Next()
	*on = true
	drain(byOption)
	// The global setting changes after the prompt, before the answer is created.
	byConfig := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t, turn("sevt_cfg")...),
		rudderanthropic.WithSessionID("sesn_policy_global"))
	byConfig.Next()
	byConfig.Next()
	if err := rudderstackai.Configure(rudderstackai.WithAnalytics(rec), rudderstackai.WithCaptureContent(true)); err != nil {
		t.Fatal(err)
	}
	drain(byConfig)
	got := rec.events(t)
	raw, _ := json.Marshal(got)
	if len(got["ai_response_received"]) != 2 || strings.Contains(string(raw), "customer-secret") || strings.Contains(string(raw), "answer-secret") {
		t.Fatalf("%s", raw)
	}
}

func TestManagedAgentsCarriesABuiltInToolAPauseNamesAfterTheCallCap(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(fmt.Sprintf("cap=%d", limit), func(t *testing.T) {
			_, rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithLimits(rudderstackai.Limits{MaxCallsPerInteraction: limit}))
			resetMA(t)
			at := func(ms int) string {
				return fmt.Sprintf(`"processed_at":%q`, time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC).Add(time.Duration(ms)*time.Millisecond).Format("2006-01-02T15:04:05.000Z"))
			}
			first := maEvents(t,
				`{"id":"sevt_cap_u","type":"user.message","content":[],`+at(0)+`}`,
				`{"id":"sevt_cap_c","type":"span.model_request_end","is_error":false,"model_usage":{"input_tokens":1,"output_tokens":1},`+at(50)+`}`,
				`{"id":"sevt_cap_t","type":"agent.tool_use","name":"bash","input":{},"evaluated_permission":"allow",`+at(100)+`}`,
				`{"id":"sevt_cap_i","type":"session.status_idle","stop_reason":{"type":"requires_action","event_ids":["sevt_cap_t"]},`+at(200)+`}`,
			)
			second := maEvents(t,
				`{"id":"sevt_cap_r","type":"user.tool_result","tool_use_id":"sevt_cap_t","is_error":true,`+at(2100)+`}`,
				`{"id":"sevt_cap_e","type":"session.status_idle","stop_reason":{"type":"end_turn"},`+at(2200)+`}`,
			)
			for _, src := range []*sliceStream{first, second} {
				events := rudderanthropic.TrackSessionEvents(context.Background(), src, rudderanthropic.WithSessionID(fmt.Sprintf("sesn_cap_builtin_%d", limit)))
				for events.Next() {
				}
				_ = events.Close()
			}
			got := rec.events(t)["ai_response_received"]
			if len(got) != 2 || got[0]["tool_call_count"] != float64(0) || got[1]["tool_call_count"] != float64(1) {
				t.Fatalf("%v", got)
			}
			breakdown := got[1]["tool_breakdown"].([]any)
			entry := breakdown[0].(map[string]any)
			if len(breakdown) != 1 || entry["tool_name"] != "bash" || entry["call_count"] != float64(1) || entry["error_count"] != float64(1) || entry["duration_ms"] != float64(2000) {
				t.Fatalf("%v", breakdown)
			}
		})
	}
}

// maAt is a server time ms after a fixed start.
func maAt(ms int) string {
	return fmt.Sprintf(`"processed_at":%q`, time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC).Add(time.Duration(ms)*time.Millisecond).Format("2006-01-02T15:04:05.000Z"))
}

func maDrain(events interface{ Next() bool }) {
	for events.Next() {
	}
}

func TestManagedAgentsSendsTheSameEventsAsAnInteractionRun(t *testing.T) {
	shape := func(rec *recorder) [][2]string {
		rudderstackai.Flush(context.Background())
		rec.mu.Lock()
		defer rec.mu.Unlock()
		var out [][2]string
		for _, m := range rec.msgs {
			keys := make([]string, 0, len(m.Properties))
			for k := range m.Properties {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			out = append(out, [2]string{m.Event, strings.Join(keys, ",")})
		}
		return out
	}
	_, rec := setup(t, rudderstackai.WithCaptureContent(true))
	resetMA(t)
	events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t,
		`{"id":"sevt_par_r","type":"session.status_running",`+maAt(0)+`}`,
		`{"id":"sevt_par_u","type":"user.message","content":[{"type":"text","text":"Hi"}],`+maAt(1000)+`}`,
		`{"id":"sevt_par_s","type":"span.model_request_start",`+maAt(2000)+`}`,
		`{"id":"sevt_par_a","type":"agent.message","content":[{"type":"text","text":"Hello"}],`+maAt(3000)+`}`,
		`{"id":"sevt_par_e","type":"span.model_request_end","is_error":false,"model_request_start_id":"sevt_par_s","model_usage":{"input_tokens":10,"output_tokens":2},`+maAt(4000)+`}`,
		`{"id":"sevt_par_i","type":"session.status_idle","stop_reason":{"type":"end_turn"},`+maAt(5000)+`}`,
	), rudderanthropic.WithSession(rawJSON(`{"id":"sesn_parity","agent":{"model":"claude-haiku-5-5"}}`)),
		rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{UserID: "u"}))
	maDrain(events)
	viaAdapter := shape(rec)
	_, rec = setup(t, rudderstackai.WithCaptureContent(true))
	_, in := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", ConversationID: "sesn_parity", PromptText: "Hi"})
	in.RecordCall(rudderstackai.LLMCall{Provider: "anthropic", Model: "claude-haiku-5-5", InputTokens: 10, OutputTokens: 2, OutputText: "Hello"})
	in.End(rudderstackai.EndOptions{Status: rudderstackai.StatusSuccess, ResponseText: "Hello"})
	if viaInteraction := shape(rec); !reflect.DeepEqual(viaAdapter, viaInteraction) || len(viaAdapter) == 0 {
		t.Fatalf("adapter     %v\ninteraction %v", viaAdapter, viaInteraction)
	}
}

func TestManagedAgentsResolvesEverySettingWhenTheTurnStartsAtItsRunningEvent(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithIdentityFromCookies(false))
	resetMA(t)
	turn := func(prefix, first string) []string {
		return []string{
			`{"id":"` + prefix + `_f","type":"` + first + `",` + maAt(0) + `}`,
			`{"id":"` + prefix + `_u","type":"user.message","content":[{"type":"text","text":"customer-secret"}],` + maAt(1000) + `}`,
			`{"id":"` + prefix + `_a","type":"agent.message","content":[{"type":"text","text":"answer-secret"}],` + maAt(2000) + `}`,
			`{"id":"` + prefix + `_i","type":"session.status_idle","stop_reason":{"type":"end_turn"},` + maAt(3000) + `}`,
		}
	}
	configure := func(on bool) {
		if err := rudderstackai.Configure(rudderstackai.WithAnalytics(rec), rudderstackai.WithCaptureContent(on), rudderstackai.WithIdentityFromCookies(on)); err != nil {
			t.Fatal(err)
		}
	}
	// Capture and cookie identity turned on after the turn's first event: neither applies to it.
	for _, first := range []string{"session.status_running", "agent.thinking"} {
		tracked := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t, turn("sevt_start_"+first, first)...),
			rudderanthropic.WithSessionID("sesn_start_"+first),
			rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{Cookies: "rl_anonymous_id=%22cookie-secret%22"}))
		tracked.Next()
		configure(true)
		maDrain(tracked)
		configure(false)
	}
	// The options as given: what the caller changes through a pointer after the call has no effect.
	capture := rudderstackai.Ptr(false)
	tracked := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t, turn("sevt_start_emit", "session.status_running")...),
		rudderanthropic.WithSessionID("sesn_start_emit"),
		rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{NoPromptEvent: true, CaptureContent: capture}))
	*capture = true
	maDrain(tracked)
	got := rec.events(t)
	// The messages too: identity is on the message, not in the properties.
	raw, _ := json.Marshal([]any{got, rec.msgs})
	if len(got["ai_response_received"]) != 3 || len(got["ai_user_prompt_created"]) != 2 ||
		strings.Contains(string(raw), "customer-secret") || strings.Contains(string(raw), "answer-secret") || strings.Contains(string(raw), "cookie-secret") {
		t.Fatalf("%d answers, %d prompts: %s", len(got["ai_response_received"]), len(got["ai_user_prompt_created"]), raw)
	}
}

func TestManagedAgentsKeepsARequestsStateAcrossAReconnectInTheCallersInteraction(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithLLMCallEvents(true))
	resetMA(t)
	first := maEvents(t,
		`{"id":"sevt_rq_u","type":"user.message","content":[],`+maAt(0)+`}`,
		`{"id":"sevt_rq_s","type":"span.model_request_start",`+maAt(100)+`}`,
		`{"id":"sevt_rq_x","type":"session.error","error":{"type":"model_rate_limited_error","message":"slow down","retry_status":{"type":"retrying"}},`+maAt(200)+`}`,
	)
	second := maEvents(t,
		`{"id":"sevt_rq_e","type":"span.model_request_end","is_error":true,"model_request_start_id":"sevt_rq_s","model_usage":{"input_tokens":0,"output_tokens":0},`+maAt(2100)+`}`,
		`{"id":"sevt_rq_i","type":"session.status_idle","stop_reason":{"type":"end_turn"},`+maAt(2200)+`}`,
	)
	ctx, caller := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	for _, src := range []*sliceStream{first, second} {
		events := rudderanthropic.TrackSessionEvents(ctx, src, rudderanthropic.WithSessionID("sesn_request_state"), rudderanthropic.WithSessionModel("claude-haiku-5-5"))
		maDrain(events)
		_ = events.Close()
	}
	caller.End(rudderstackai.EndOptions{Status: rudderstackai.StatusSuccess})
	got := rec.events(t)["ai_llm_response_received"]
	if len(got) != 1 || got[0]["error_type"] != "rate_limit" || got[0]["latency_ms"] != float64(2000) {
		t.Fatalf("%v", got)
	}
}

func TestManagedAgentsTimesAnOutcomeGraderCycleFromItsStartEvent(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithLLMCallEvents(true))
	resetMA(t)
	events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t,
		`{"id":"sevt_gr_u","type":"user.message","content":[],`+maAt(0)+`}`,
		`{"id":"sevt_gr_s","type":"span.outcome_evaluation_start","outcome_id":"o1","iteration":0,`+maAt(100)+`}`,
		`{"id":"sevt_gr_e","type":"span.outcome_evaluation_end","outcome_evaluation_start_id":"sevt_gr_s","outcome_id":"o1","iteration":0,"result":"satisfied","usage":{"input_tokens":5,"output_tokens":1},`+maAt(2100)+`}`,
		`{"id":"sevt_gr_i","type":"session.status_idle","stop_reason":{"type":"end_turn"},`+maAt(2200)+`}`,
	), rudderanthropic.WithSessionID("sesn_grader_time"))
	maDrain(events)
	got := rec.events(t)["ai_llm_response_received"]
	if len(got) != 1 || got[0]["latency_ms"] != float64(2000) {
		t.Fatalf("%v", got)
	}
}

// The tool is in the core before the pause: its outcome after the pause needs the edge-cases D2
// core change (pending tools across a pause). It passes, and this marker goes, when D2 lands.
func TestManagedAgentsCarriesAToolAlreadyRecordedAtTheCallCapWhenAPauseNamesIt(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithLimits(rudderstackai.Limits{MaxCallsPerInteraction: 2, MaxMapEntries: 1}))
	resetMA(t)
	first := maEvents(t,
		`{"id":"sevt_rec_u","type":"user.message","content":[],`+maAt(0)+`}`,
		`{"id":"sevt_rec_h","type":"agent.tool_use","name":"held","input":{},`+maAt(50)+`}`,
		`{"id":"sevt_rec_t","type":"agent.tool_use","name":"bash","input":{},`+maAt(100)+`}`,
		`{"id":"sevt_rec_c","type":"span.model_request_end","is_error":false,"model_usage":{"input_tokens":1,"output_tokens":1},`+maAt(150)+`}`,
		`{"id":"sevt_rec_i","type":"session.status_idle","stop_reason":{"type":"requires_action","event_ids":["sevt_rec_t"]},`+maAt(200)+`}`,
	)
	second := maEvents(t,
		`{"id":"sevt_rec_r","type":"user.tool_result","tool_use_id":"sevt_rec_t","is_error":true,`+maAt(2000)+`}`,
		`{"id":"sevt_rec_e","type":"session.status_idle","stop_reason":{"type":"end_turn"},`+maAt(2100)+`}`,
	)
	for _, src := range []*sliceStream{first, second} {
		events := rudderanthropic.TrackSessionEvents(context.Background(), src, rudderanthropic.WithSessionID("sesn_recorded_pause"))
		maDrain(events)
		_ = events.Close()
	}
	var calls, errors float64
	for _, response := range rec.events(t)["ai_response_received"] {
		breakdown, _ := response["tool_breakdown"].([]any)
		for _, e := range breakdown {
			if entry := e.(map[string]any); entry["tool_name"] == "bash" {
				c, _ := entry["call_count"].(float64)
				n, _ := entry["error_count"].(float64)
				calls, errors = calls+c, errors+n
			}
		}
	}
	// Counted once across the segments, with its outcome.
	if calls == 1 && errors == 1 {
		t.Fatal("passes now: remove the expected-failure marker")
	}
	t.Logf("expected to fail until D2: bash call_count %v, error_count %v", calls, errors)
}

func TestManagedAgentsCountsTheDenialOfAToolWithoutAnID(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithToolBreakdown(true))
	resetMA(t)
	events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t,
		`{"id":"sevt_idless_u","type":"user.message","content":[],`+maAt(0)+`}`,
		`{"type":"agent.tool_use","name":"bash","input":{},"evaluated_permission":"deny",`+maAt(100)+`}`,
		`{"id":"sevt_idless_i","type":"session.status_idle","stop_reason":{"type":"end_turn"},`+maAt(200)+`}`,
	), rudderanthropic.WithSessionID("sesn_idless_denied"))
	maDrain(events)
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 {
		t.Fatalf("%v", got)
	}
	entry := got[0]["tool_breakdown"].([]any)[0].(map[string]any)
	if entry["call_count"] != float64(1) || entry["error_count"] != float64(1) || entry["error_types"].(map[string]any)["denied"] != float64(1) {
		t.Fatalf("%v", entry)
	}
}
