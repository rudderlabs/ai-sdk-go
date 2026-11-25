package rudderanthropic_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"runtime/debug"
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
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
	"github.com/rudderlabs/ai-sdk-go/rudderanthropic"
)

type maStream struct {
	Events    []json.RawMessage `json:"events"`
	Cassette  string            `json:"cassette"`
	StopAfter *int              `json:"stop_after"`
	// "pause_restore": after this stream the caller pauses its interaction and restores it from the JSON snapshot.
	Caller string `json:"caller"`
}

type maExpected struct {
	Event       string         `json:"event"`
	Interaction string         `json:"interaction"`
	Properties  map[string]any `json:"properties"`
}

type maScenario struct {
	Name string `json:"name"`
	// Warn-once keys the scenario must warn under, once each, with a fragment of the warning's text.
	Warnings []struct {
		Key  string `json:"key"`
		Text string `json:"text"`
	} `json:"warnings"`
	Options struct {
		ToolBreakdown  bool            `json:"tool_breakdown"`
		CaptureContent bool            `json:"capture_content"`
		Session        json.RawMessage `json:"session"`
		MaxMapEntries  int             `json:"max_map_entries"`
		// "interaction": every stream is read inside one caller interaction, ended without a status after the last.
		Caller string `json:"caller"`
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

// consumeCallerMA reads a scenario's streams inside one caller interaction, as Node's fixture runner does.
func consumeCallerMA(ctx context.Context, t *testing.T, f maFixture, sc maScenario, opts []rudderanthropic.SessionOption) {
	t.Helper()
	var session struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(f.Session, &session); err != nil {
		t.Fatal(err)
	}
	callerCtx, caller := rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "user_1", ConversationID: session.ID})
	for _, s := range sc.Streams {
		consumeMA(callerCtx, t, s, opts)
		if s.Caller != "pause_restore" {
			continue
		}
		snapshot, ok := caller.Pause(rudderstackai.PauseOptions{})
		if !ok {
			t.Fatal("the caller's interaction did not pause")
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var restored rudderstackai.PausedInteraction
		if err := json.Unmarshal(raw, &restored); err != nil {
			t.Fatal(err)
		}
		callerCtx, caller = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{Resume: &restored})
	}
	caller.End(rudderstackai.EndOptions{})
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
			// setup re-arms every warn-once key; the scenario's warnings go to a buffer.
			var logged bytes.Buffer
			if len(sc.Warnings) > 0 {
				prev := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
				t.Cleanup(func() { slog.SetDefault(prev) })
			}
			ctx := context.Background()
			if sc.Options.Caller == "interaction" {
				consumeCallerMA(ctx, t, f, sc, opts)
			} else {
				for _, s := range sc.Streams {
					consumeMA(ctx, t, s, opts)
				}
			}
			rudderstackai.Flush(ctx)
			for _, w := range sc.Warnings {
				if n := strings.Count(logged.String(), w.Text); n != 1 {
					t.Errorf("warning %s logged %d times, want once", w.Key, n)
				}
			}
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
	// The custom tool waits on the app: it counts in the segment that resumes the pause.
	got := rec.events(t)["ai_response_received"]
	if len(got) != 1 || got[0]["response_status"] != "paused" || got[0]["llm_call_count"] != float64(1) ||
		got[0]["tool_call_count"] != float64(0) || got[0]["total_input_tokens"] != float64(30) || got[0]["total_output_tokens"] != float64(8) {
		t.Fatalf("%v", got)
	}
	// The paused segment holds the call; the resumed one counts nothing twice.
	_ = events.Close()
	i.Resume()
	i.End(rudderstackai.EndOptions{Status: rudderstackai.StatusSuccess})
	got = rec.events(t)["ai_response_received"]
	if len(got) != 2 || got[1]["llm_call_count"] != float64(0) || got[1]["tool_call_count"] != float64(1) || got[1]["total_input_tokens"] != float64(0) {
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
	app := map[string]any{"build": "original"}
	events := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, sc.Streams[0].Events)},
		rudderanthropic.WithSession(withID(f.Session, "sesn_nested_options")),
		rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{
			ContentCapture: policy, CookieValues: cookies, AttachmentCount: rudderstackai.Ptr(1), AttachmentTypes: attachmentTypes,
			Context: map[string]any{"app": app},
		}))
	*policy.Default = true
	cookies["rl_anonymous_id"] = "%22later%22"
	attachmentTypes[0] = "audio"
	app["build"] = "later"
	for events.Next() {
	}
	_ = events.Close()
	got := rec.events(t)
	rec.mu.Lock()
	for _, m := range rec.msgs {
		if m.AnonymousId != "original" {
			t.Errorf("%s anonymousId %q", m.Event, m.AnonymousId)
		}
		if build := fmt.Sprint(m.Context.Extra["app"]); build != "map[build:original]" {
			t.Errorf("%s context app %s", m.Event, build)
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
		`{"id":"sevt_kid_s","type":"agent.thinking","session_thread_id":"sthr_kid",`+at+`}`,
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

// The tool is in the core before the pause; its result after the pause still settles it there.
func TestManagedAgentsCarriesAToolAlreadyRecordedAtTheCallCapWhenAPauseNamesIt(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithLimits(rudderstackai.Limits{MaxCallsPerInteraction: 2, MaxMapEntries: 1}))
	resetMA(t)
	first := maEvents(t,
		`{"id":"sevt_rec_u","type":"user.message","content":[],`+maAt(0)+`}`,
		// A request's tool uses follow its end, as the session stream sends them.
		`{"id":"sevt_rec_c","type":"span.model_request_end","is_error":false,"model_usage":{"input_tokens":1,"output_tokens":1},`+maAt(50)+`}`,
		`{"id":"sevt_rec_h","type":"agent.tool_use","name":"held","input":{},`+maAt(100)+`}`,
		`{"id":"sevt_rec_t","type":"agent.tool_use","name":"bash","input":{},`+maAt(150)+`}`,
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
	if calls != 1 || errors != 1 {
		t.Fatalf("bash call_count %v, error_count %v", calls, errors)
	}
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

func TestManagedAgentsKeepsServerLatencyUpToALastRunningOrErrorEvent(t *testing.T) {
	for _, last := range []struct{ typ, fields string }{
		{"session.status_running", ""},
		{"session.status_rescheduled", ""},
		{"session.error", `"error":{"type":"unknown_error","message":"x","retry_status":{"type":"terminal"}},`},
	} {
		t.Run(last.typ, func(t *testing.T) {
			_, rec := setup(t)
			resetMA(t)
			events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t,
				`{"id":"sevt_last_r","type":"session.status_running",`+maAt(0)+`}`,
				`{"id":"sevt_last_u","type":"user.message","content":[],`+maAt(0)+`}`,
				`{"id":"sevt_last_s","type":"span.model_request_start",`+maAt(1000)+`}`,
				`{"id":"sevt_last_x","type":"`+last.typ+`",`+last.fields+maAt(3000)+`}`,
			), rudderanthropic.WithSessionID("sesn_last_"+last.typ))
			maDrain(events)
			_ = events.Close()
			got := rec.events(t)["ai_response_received"]
			// Server time maps onto the local clock to the millisecond: allow rounding, not the 1000 ms bug.
			if len(got) != 1 {
				t.Fatalf("%v", got)
			}
			if ms, _ := got[0]["latency_ms"].(float64); ms <= 2900 || ms >= 3500 {
				t.Fatalf("latency_ms %v", got[0]["latency_ms"])
			}
		})
	}
}

func TestManagedAgentsKeepsTheOptionsRequestCookiesAsTheyWereWhenTheStreamWasWrapped(t *testing.T) {
	f, sc := maFixtureScenario(t, "text_turn_capture")
	_, rec := setup(t, rudderstackai.WithIdentityFromCookies(true))
	resetMA(t)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Cookie", "rl_anonymous_id=%22original%22")
	events := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, sc.Streams[0].Events)},
		rudderanthropic.WithSession(withID(f.Session, "sesn_nested_request")),
		rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{Request: req}))
	req.Header.Set("Cookie", "rl_anonymous_id=%22later%22")
	maDrain(events)
	_ = events.Close()
	rudderstackai.Flush(context.Background())
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.msgs) == 0 {
		t.Fatal("no events")
	}
	for _, m := range rec.msgs {
		if m.AnonymousId != "original" {
			t.Errorf("%s anonymousId %q", m.Event, m.AnonymousId)
		}
	}
}

func TestManagedAgentsCyclicContextSnapshot(t *testing.T) {
	// Stack overflow is fatal, so the regression runs in a subprocess with a small stack.
	if os.Getenv("MANAGED_AGENTS_CYCLE_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestManagedAgentsCyclicContextSnapshot$")
		cmd.Env = append(os.Environ(), "MANAGED_AGENTS_CYCLE_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			if len(output) > 4000 {
				output = output[:4000]
			}
			t.Fatalf("cyclic context subprocess: %v\n%s", err, output)
		}
		return
	}
	debug.SetMaxStack(1 << 20)
	setup(t)
	resetMA(t)
	m := map[string]any{}
	m["self"] = m
	s := make([]any, 1)
	s[0] = s
	m["slice"] = s
	events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t), rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{Context: m}))
	maDrain(events)
	_ = events.Close()
}

func TestManagedAgentsTypedContextSnapshot(t *testing.T) {
	_, rec := setup(t)
	resetMA(t)
	type namedMap map[string]string
	type namedSlice []string
	m := map[string]string{"build": "original"}
	s := []string{"original"}
	ms := []map[string]any{{"build": "original"}}
	b := []byte("original")
	im := map[int]string{1: "original"}
	nm, ns := namedMap{"build": "original"}, namedSlice{"original"}
	array := [1]map[string]any{{"build": "original"}}
	ctx := map[string]any{"map": m, "slice": s, "map_slice": ms, "bytes": b, "int_map": im, "named_map": nm, "named_slice": ns, "array": array}
	want := map[string]any{}
	for key, value := range ctx {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		want[key] = string(raw)
	}
	events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t,
		`{"id":"u","type":"user.message","content":[]}`,
		`{"id":"i","type":"session.status_idle","stop_reason":{"type":"end_turn"}}`,
	), rudderanthropic.WithSessionID("typed"), rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{Context: ctx}))
	m["build"], s[0], ms[0]["build"], im[1], nm["build"], ns[0], array[0]["build"] = "later", "later", "later", "later", "later", "later", "later"
	copy(b, "modified")
	maDrain(events)
	_ = events.Close()
	rudderstackai.Flush(context.Background())
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.msgs) != 2 {
		t.Fatalf("got %d events", len(rec.msgs))
	}
	for _, message := range rec.msgs {
		for key, expected := range want {
			raw, err := json.Marshal(message.Context.Extra[key])
			if err != nil || string(raw) != expected {
				t.Errorf("%s context[%s]=%s, want %s (error %v)", message.Event, key, raw, expected, err)
			}
		}
	}
}

func TestManagedAgentsPausesAfterRunningRequiresAction(t *testing.T) {
	for _, kind := range []string{"session.status_running", "session.status_rescheduled"} {
		t.Run(kind, func(t *testing.T) {
			_, rec := setup(t)
			resetMA(t)
			for segment := range 2 {
				events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t,
					`{"id":"r`+strconv.Itoa(segment)+`","type":"`+kind+`",`+maAt(segment*1000)+`}`,
					`{"id":"i`+strconv.Itoa(segment)+`","type":"session.status_idle","stop_reason":{"type":"requires_action","event_ids":["sevt_child_tool"]},`+maAt(segment*1000+200)+`}`,
				), rudderanthropic.WithSessionID("running_pause_"+kind))
				maDrain(events)
				_ = events.Close()
			}
			got := rec.events(t)["ai_response_received"]
			if len(got) != 2 || got[0]["response_status"] != "paused" || got[1]["response_status"] != "paused" || got[0]["interaction_id"] != got[1]["interaction_id"] {
				t.Fatalf("responses=%v", got)
			}
			if latency, _ := got[0]["latency_ms"].(float64); latency < 190 || latency > 250 {
				t.Fatalf("latency_ms=%v", latency)
			}
		})
	}
}

func TestManagedAgentsIgnoresResume(t *testing.T) {
	_, rec := setup(t)
	resetMA(t)
	_, original := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u", NoPromptEvent: true})
	snapshot, ok := original.Pause(rudderstackai.PauseOptions{})
	if !ok {
		t.Fatal("failed to pause")
	}
	for _, session := range []string{"resume_a", "resume_b"} {
		events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t,
			`{"id":"u","type":"user.message","content":[]}`,
			`{"id":"i","type":"session.status_idle","stop_reason":{"type":"end_turn"}}`,
		), rudderanthropic.WithSessionID(session), rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{Resume: &snapshot}))
		maDrain(events)
		_ = events.Close()
	}
	var ids []any
	for _, response := range rec.events(t)["ai_response_received"] {
		if response["response_status"] == "success" {
			ids = append(ids, response["interaction_id"])
		}
	}
	if len(ids) != 2 || ids[0] == ids[1] || ids[0] == snapshot.InteractionID || ids[1] == snapshot.InteractionID {
		t.Fatalf("interaction IDs=%v, snapshot=%s", ids, snapshot.InteractionID)
	}
}

func TestManagedAgentsCarriesEveryToolAPauseNamesWhateverItsOrder(t *testing.T) {
	for _, order := range []string{`["sevt_mix_approval","sevt_mix_bash"]`, `["sevt_mix_bash","sevt_mix_approval"]`} {
		t.Run(order, func(t *testing.T) {
			_, rec := setup(t, rudderstackai.WithToolBreakdown(true))
			resetMA(t)
			at := func(n int) string { return fmt.Sprintf(`"processed_at":"2026-11-16T10:00:0%d.000Z"`, n) }
			segments := [][]string{{
				`{"id":"sevt_mix_prompt","type":"user.message","content":[],` + at(0) + `}`,
				`{"id":"sevt_mix_approval","type":"agent.tool_use","name":"ask_tool","evaluated_permission":"ask","input":{},` + at(1) + `}`,
				`{"id":"sevt_mix_bash","type":"agent.tool_use","name":"bash","evaluated_permission":"allow","input":{},` + at(2) + `}`,
				`{"id":"sevt_mix_pause","type":"session.status_idle","stop_reason":{"type":"requires_action","event_ids":` + order + `},` + at(3) + `}`,
			}, {
				`{"id":"sevt_mix_deny","type":"user.tool_confirmation","tool_use_id":"sevt_mix_approval","result":"deny",` + at(4) + `}`,
				`{"id":"sevt_mix_result","type":"user.tool_result","tool_use_id":"sevt_mix_bash","is_error":true,"content":[{"type":"text","text":"failed"}],` + at(5) + `}`,
				`{"id":"sevt_mix_idle","type":"session.status_idle","stop_reason":{"type":"end_turn"},` + at(6) + `}`,
			}}
			for _, raw := range segments {
				events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t, raw...), rudderanthropic.WithSessionID("sesn_mix_"+strconv.Itoa(len(order))+order[2:6]))
				for events.Next() {
				}
				_ = events.Close()
			}
			got := rec.events(t)["ai_response_received"]
			if len(got) != 2 || got[0]["tool_call_count"] != float64(0) || got[1]["tool_call_count"] != float64(2) {
				t.Fatalf("%v", got)
			}
			for _, e := range got[1]["tool_breakdown"].([]any) {
				b := e.(map[string]any)
				if b["tool_name"] == "bash" && (b["error_count"] != float64(1) || b["duration_ms"] == nil || b["duration_ms"] == float64(0)) {
					t.Fatalf("%v", b)
				}
			}
		})
	}
}

func TestManagedAgentsKeepsARequestsOutputAcrossAReconnectWhenItsStartWasNotRead(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithLLMCallEvents(true), rudderstackai.WithCaptureContent(true))
	resetMA(t)
	ctx, i := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	segments := [][]string{{
		`{"id":"sevt_rc_u","type":"user.message","content":[],"processed_at":"2026-11-16T10:00:00.000Z"}`,
		`{"id":"sevt_rc_a","type":"agent.message","content":[{"type":"text","text":"Hi"}],"processed_at":"2026-11-16T10:00:01.000Z"}`,
	}, {
		`{"id":"sevt_rc_e","type":"span.model_request_end","model_request_start_id":"sevt_not_read","is_error":false,"model_usage":{"input_tokens":10,"output_tokens":2},"processed_at":"2026-11-16T10:00:02.000Z"}`,
		`{"id":"sevt_rc_i","type":"session.status_idle","stop_reason":{"type":"end_turn"},"processed_at":"2026-11-16T10:00:03.000Z"}`,
	}}
	for _, raw := range segments {
		events := rudderanthropic.TrackSessionEvents(ctx, maEvents(t, raw...), rudderanthropic.WithSessionID("sesn_rc_no_start"))
		for events.Next() {
		}
		_ = events.Close()
	}
	i.End(rudderstackai.EndOptions{Status: rudderstackai.StatusSuccess})
	got := rec.events(t)["ai_llm_response_received"]
	if len(got) != 1 || got[0]["output_text"] != "Hi" {
		t.Fatalf("%v", got)
	}
}

// maAcrossWholeState compares an uninterrupted control with a split turn. Reconnect reports
// into the caller's interaction; pause resumes an answer owned by the adapter.
func maAcrossWholeState(t *testing.T, path string, first, second, controlFirst []string, changed []rudderanthropic.SessionOption, assert func(*testing.T, map[string][]map[string]any)) {
	t.Helper()
	for _, split := range []bool{false, true} {
		t.Run(map[bool]string{false: "control", true: "split"}[split], func(t *testing.T) {
			_, rec := setup(t, rudderstackai.WithLLMCallEvents(true), rudderstackai.WithCaptureContent(true), rudderstackai.WithToolBreakdown(true))
			resetMA(t)
			for model, price := range map[string]rudderstackai.TokenPrice{
				"claude-haiku-5-5": {InputPerMtok: 1, OutputPerMtok: 2},
				"different-model":  {InputPerMtok: 100, OutputPerMtok: 200},
			} {
				if err := rudderstackai.RegisterPrice(model, rudderstackai.ModelPrice{Provider: "anthropic", TokenPrice: price}); err != nil {
					t.Fatal(err)
				}
			}
			options := []rudderanthropic.SessionOption{rudderanthropic.WithSessionID("whole-state"), rudderanthropic.WithSessionModel("claude-haiku-5-5"), rudderanthropic.WithAgentName("original")}
			ctx := context.Background()
			var caller *rudderstackai.Interaction
			if path == "reconnect" {
				ctx, caller = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "u"})
			}
			consume := func(raw []string, opts []rudderanthropic.SessionOption) {
				events := rudderanthropic.TrackSessionEvents(ctx, maEvents(t, raw...), opts...)
				maDrain(events)
				if err := events.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if split {
				segment := slices.Clone(first)
				if path == "pause" {
					segment = append(segment, maWholeIdle("requires_action"))
				}
				consume(segment, options)
				consume(second, append(slices.Clone(options), changed...))
			} else {
				if controlFirst == nil {
					controlFirst = first
				}
				consume(append(slices.Clone(controlFirst), second...), options)
			}
			if caller != nil {
				caller.End(rudderstackai.EndOptions{})
			}
			assert(t, rec.events(t))
		})
	}
}

func maWholeEvent(kind string) string { return `{"type":` + strconv.Quote(kind) + `}` }
func maWholeCall(extra string) string {
	return `{"type":"span.model_request_end","model_usage":{"input_tokens":10,"output_tokens":2}` + extra + `}`
}
func maWholeIdle(kind string, ids ...string) string {
	if ids == nil {
		ids = []string{}
	}
	raw, _ := json.Marshal(ids)
	return `{"type":"session.status_idle","stop_reason":{"type":` + strconv.Quote(kind) + `,"event_ids":` + string(raw) + `}}`
}

func TestManagedAgentsCarriedEvents(t *testing.T) {
	for _, ending := range []struct {
		name, event, status, errorType string
	}{
		{"end_turn", maWholeIdle("end_turn"), "success", ""},
		{"terminated", maWholeEvent("session.status_terminated"), "success", ""},
		{"terminal error", `{"type":"session.error","error":{"type":"mcp_authentication_failed_error","retry_status":{"type":"terminal"}}}`, "error", "auth_error"},
		{"refusal", maWholeIdle("refusal"), "error", "content_filter"},
		{"budget_reached", maWholeIdle("budget_reached"), "incomplete", "max_budget_usd"},
		{"retries_exhausted", maWholeIdle("retries_exhausted"), "error", "api_error"},
	} {
		t.Run("carried event: borrowed ending-only "+ending.name, func(t *testing.T) {
			maAcrossWholeState(t, "reconnect",
				[]string{maWholeEvent("user.message"), maWholeCall("")}, []string{ending.event}, nil, nil,
				func(t *testing.T, events map[string][]map[string]any) {
					responses := events["ai_response_received"]
					if len(responses) != 1 || responses[0]["response_status"] != ending.status || responses[0]["llm_call_count"] != float64(1) {
						t.Fatalf("%v", responses)
					}
					if ending.errorType != "" && responses[0]["error_type"] != ending.errorType {
						t.Fatalf("%v", responses)
					}
				})
		})
	}
	for _, path := range []string{"reconnect", "pause"} {
		t.Run("carried event: requires_action-only preserves approval across "+path, func(t *testing.T) {
			maAcrossWholeState(t, path,
				[]string{maWholeEvent("user.message"), maWholeCall(""), `{"type":"agent.tool_use","id":"approval","name":"tool","evaluated_permission":"ask"}`},
				[]string{maWholeIdle("requires_action", "approval")}, nil, nil,
				func(t *testing.T, events map[string][]map[string]any) {
					maAssertLastResponse(t, events, "paused", "approval")
				})
		})
		for _, kind := range []string{"agent.tool_use", "agent.custom_tool_use"} {
			for _, running := range []bool{false, true} {
				t.Run(fmt.Sprintf("carried event: child blocking before primary events across %s (%s, running %t)", path, kind, running), func(t *testing.T) {
					var second []string
					if running {
						second = append(second, maWholeEvent("session.status_running"))
					}
					second = append(second,
						`{"type":`+strconv.Quote(kind)+`,"id":"child-blocking","session_thread_id":"child","evaluated_permission":"ask","name":"child tool"}`,
						maWholeEvent("agent.thinking"), maWholeIdle("requires_action", "child-blocking"))
					maAcrossWholeState(t, path, []string{maWholeEvent("user.message"), maWholeCall("")}, second, nil, nil,
						func(t *testing.T, events map[string][]map[string]any) {
							reason := "approval"
							if kind == "agent.custom_tool_use" {
								reason = "deferred_tool"
							}
							maAssertLastResponse(t, events, "paused", reason)
						})
				})
			}
		}
		for _, start := range []string{"user.message", "session.status_running", "session.status_rescheduled"} {
			t.Run(fmt.Sprintf("carried event: retry before request start or output across %s (%s)", path, start), func(t *testing.T) {
				errEvent := `{"type":"session.error","error":{"type":"model_rate_limited_error","retry_status":{"type":"retrying"}}}`
				maAcrossWholeState(t, path,
					[]string{maWholeEvent(start), errEvent}, []string{maWholeCall(`,"is_error":true`), maWholeIdle("end_turn")},
					[]string{maWholeEvent(start), `{"type":"span.model_request_start","id":"control-start"}`, errEvent}, nil,
					maAssertLastRateLimit)
			})
		}
	}
	t.Run("carried event: retry-only opening restores borrowed state", func(t *testing.T) {
		maAcrossWholeState(t, "reconnect",
			[]string{maWholeEvent("user.message"), `{"type":"span.model_request_start","id":"request"}`},
			[]string{`{"type":"session.error","error":{"type":"model_rate_limited_error","retry_status":{"type":"retrying"}}}`,
				maWholeCall(`,"is_error":true,"model_request_start_id":"request"`), maWholeIdle("end_turn")}, nil, nil,
			maAssertLastRateLimit)
	})
}

func maAssertLastResponse(t *testing.T, events map[string][]map[string]any, status, reason string) {
	t.Helper()
	responses := events["ai_response_received"]
	if len(responses) == 0 || responses[len(responses)-1]["response_status"] != status || responses[len(responses)-1]["pause_reason"] != reason {
		t.Fatalf("%v", responses)
	}
}

func maAssertLastRateLimit(t *testing.T, events map[string][]map[string]any) {
	t.Helper()
	calls := events["ai_llm_response_received"]
	if len(calls) == 0 || calls[len(calls)-1]["error_type"] != "rate_limit" {
		t.Fatalf("%v", calls)
	}
}

func TestManagedAgentsCarriedEventGuards(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		t.Run(fmt.Sprintf("metadata leaves carried state unopened (borrowed %t)", borrowed), func(t *testing.T) {
			setup(t)
			resetMA(t)
			ctx := context.Background()
			var owner *rudderstackai.Interaction
			if borrowed {
				ctx, owner = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{NoPromptEvent: true})
				defer owner.End(rudderstackai.EndOptions{})
			}
			first := rudderanthropic.TrackSessionEvents(ctx, maEvents(t, maWholeEvent("user.message"), maWholeIdle("requires_action")), rudderanthropic.WithSessionID("metadata"))
			maDrain(first)
			_ = first.Close()
			var raw []string
			for _, kind := range []string{"session.updated", "session.usage", "session.thread_created"} {
				raw = append(raw, `{"type":`+strconv.Quote(kind)+`,"session_thread_id":"child"}`)
			}
			second := rudderanthropic.TrackSessionEvents(ctx, maEvents(t, raw...), rudderanthropic.WithSessionID("metadata"))
			for second.Next() {
				if open, _, _ := rudderanthropic.InspectManagedAgents(second, "metadata"); open {
					t.Fatal("metadata opened a turn")
				}
			}
			_ = second.Close()
			_, paused, carriedOwner := rudderanthropic.InspectManagedAgents(second, "metadata")
			if (borrowed && carriedOwner != owner) || (!borrowed && !paused) {
				t.Fatal("metadata lost carried state")
			}
		})
	}
	t.Run("an ending in another interaction cannot adopt borrowed state", func(t *testing.T) {
		setup(t)
		resetMA(t)
		ctx, owner := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{NoPromptEvent: true})
		defer owner.End(rudderstackai.EndOptions{})
		first := rudderanthropic.TrackSessionEvents(ctx, maEvents(t, maWholeEvent("user.message"), maWholeCall("")), rudderanthropic.WithSessionID("different-owner"))
		maDrain(first)
		_ = first.Close()
		otherCtx, other := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{NoPromptEvent: true})
		defer other.End(rudderstackai.EndOptions{})
		second := rudderanthropic.TrackSessionEvents(otherCtx, maEvents(t, maWholeIdle("end_turn")), rudderanthropic.WithSessionID("different-owner"))
		if !second.Next() {
			t.Fatal("missing ending event")
		}
		open, _, carriedOwner := rudderanthropic.InspectManagedAgents(second, "different-owner")
		if open || carriedOwner != owner {
			t.Fatal("another interaction adopted borrowed state")
		}
		_ = second.Close()
	})
}

func TestManagedAgentsWholeTurnState(t *testing.T) {
	for _, path := range []string{"reconnect", "pause"} {
		t.Run("request keeps starting model and agent across "+path, func(t *testing.T) {
			maAcrossWholeState(t, path,
				[]string{maWholeEvent("user.message"), `{"type":"span.model_request_start","id":"request"}`},
				[]string{maWholeCall(`,"model_request_start_id":"request"`), maWholeIdle("end_turn")}, nil,
				[]rudderanthropic.SessionOption{rudderanthropic.WithSessionModel("different-model"), rudderanthropic.WithAgentName("replacement")},
				func(t *testing.T, events map[string][]map[string]any) {
					calls := events["ai_llm_response_received"]
					if len(calls) != 1 || calls[0]["model"] != "claude-haiku-5-5" || calls[0]["agent_name"] != "original" || calls[0]["cost_usd"] != 0.000014 {
						t.Fatalf("%v", calls)
					}
				})
		})
		t.Run("interrupt survives "+path, func(t *testing.T) {
			maAcrossWholeState(t, path, []string{maWholeEvent("user.message"), maWholeEvent("user.interrupt")},
				[]string{maWholeCall(""), maWholeIdle("end_turn")}, nil, nil,
				func(t *testing.T, events map[string][]map[string]any) {
					responses := events["ai_response_received"]
					if len(responses) == 0 || responses[len(responses)-1]["response_status"] != "cancelled" {
						t.Fatalf("%v", responses)
					}
				})
		})
		t.Run("grader keeps primary model across "+path, func(t *testing.T) {
			maAcrossWholeState(t, path,
				[]string{maWholeEvent("user.message"), maWholeCall(""), maWholeEvent("agent.thread_context_compacted"), `{"type":"span.outcome_evaluation_start","id":"grader"}`},
				[]string{`{"type":"span.outcome_evaluation_end","outcome_evaluation_start_id":"grader","usage":{"input_tokens":100}}`, maWholeIdle("end_turn")}, nil, nil,
				func(t *testing.T, events map[string][]map[string]any) {
					responses := events["ai_response_received"]
					var compactions, calls float64
					for _, response := range responses {
						if n, ok := response["compaction_count"].(float64); ok {
							compactions += n
						}
						if n, ok := response["llm_call_count"].(float64); ok {
							calls += n
						}
					}
					if len(responses) == 0 || responses[len(responses)-1]["primary_model"] != "claude-haiku-5-5" || compactions != 1 || calls != 2 {
						t.Fatalf("%v", responses)
					}
				})
		})
		t.Run("retry error without request start survives "+path, func(t *testing.T) {
			first := []string{maWholeEvent("user.message"), `{"type":"agent.message","content":[{"type":"text","text":"partial"}]}`, `{"type":"session.error","error":{"type":"model_rate_limited_error","retry_status":{"type":"retrying"}}}`}
			control := append([]string{first[0], `{"type":"span.model_request_start","id":"uninterrupted-start"}`}, first[1:]...)
			maAcrossWholeState(t, path, first, []string{maWholeCall(`,"is_error":true`), maWholeIdle("end_turn")}, control, nil,
				func(t *testing.T, events map[string][]map[string]any) {
					calls := events["ai_llm_response_received"]
					if len(calls) != 1 || calls[0]["error_type"] != "rate_limit" {
						t.Fatalf("%v", calls)
					}
				})
		})
		t.Run("subagent approval alone survives "+path, func(t *testing.T) {
			maAcrossWholeState(t, path,
				[]string{maWholeEvent("user.message"), `{"type":"agent.tool_use","id":"child-tool","session_thread_id":"child","evaluated_permission":"ask","name":"approval"}`},
				[]string{maWholeEvent("agent.thinking"), maWholeIdle("requires_action", "child-tool")}, nil, nil,
				func(t *testing.T, events map[string][]map[string]any) {
					responses := events["ai_response_received"]
					if len(responses) == 0 || responses[len(responses)-1]["pause_reason"] != "approval" {
						t.Fatalf("%v", responses)
					}
				})
		})
	}
	t.Run("tool keeps starting agent across pause", func(t *testing.T) {
		first := []string{maWholeEvent("user.message"), `{"type":"agent.custom_tool_use","id":"tool","name":"lookup"}`}
		// The first pause names the buffered custom tool, so it remains an adapter-held draft.
		first = append(first, maWholeIdle("requires_action", "tool"))
		maAcrossWholeState(t, "pause", first,
			[]string{`{"type":"user.custom_tool_result","custom_tool_use_id":"tool","content":[]}`, maWholeIdle("end_turn")}, nil,
			[]rudderanthropic.SessionOption{rudderanthropic.WithAgentName("replacement")},
			func(t *testing.T, events map[string][]map[string]any) {
				responses := events["ai_response_received"]
				var tools []any
				for _, response := range responses {
					if entries, ok := response["tool_breakdown"].([]any); ok {
						tools = append(tools, entries...)
					}
				}
				if len(tools) != 1 || tools[0].(map[string]any)["agent_name"] != "original" {
					t.Fatalf("%v", responses)
				}
			})
	})
}

func TestManagedAgentsWholeTurnStateStructure(t *testing.T) {
	t.Run("covers every real opened turn field with state or the explicit stream-local list", func(t *testing.T) {
		setup(t)
		resetMA(t)
		local := []string{"startAt", "lastAt", "borrowed", "backdated", "resumed", "interaction", "abandoned"}
		typ := rudderanthropic.ManagedAgentsTurnType()
		var carried []string
		for n := 0; n < typ.NumField(); n++ {
			field := typ.Field(n).Name
			if !slices.Contains(local, field) {
				carried = append(carried, field)
			}
		}
		if !slices.Equal(carried, []string{"state"}) {
			t.Fatalf("turn fields outside stream-local set: %v", carried)
		}
		for _, field := range local {
			if _, ok := typ.FieldByName(field); !ok {
				t.Errorf("stale stream-local field: %s", field)
			}
		}
	})
	t.Run("bounds carried entries and every TurnState map and evicts the oldest", func(t *testing.T) {
		setup(t, rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: 2}))
		resetMA(t)
		rudderanthropic.CheckManagedAgentsBounds(t)
		maCheckOutstandingBounds(t)
	})
	t.Run("flushes a full tool buffer before any unrecorded work is lost", func(t *testing.T) {
		_, rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: 2}))
		resetMA(t)
		var raw = []string{maWholeEvent("user.message")}
		for _, id := range []string{"oldest", "middle", "newest"} {
			raw = append(raw, `{"type":"agent.tool_use","id":`+strconv.Quote(id)+`,"name":`+strconv.Quote(id)+`}`)
		}
		raw = append(raw, maWholeIdle("end_turn"))
		events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t, raw...), rudderanthropic.WithSessionID("full-buffer"))
		maDrain(events)
		responses := rec.events(t)["ai_response_received"]
		if len(responses) != 1 || responses[0]["tool_call_count"] != float64(3) {
			t.Fatalf("%v", responses)
		}
	})
}

func maPreviewStart(id, kind string) string {
	return `{"type":"event_start","event":{"type":` + strconv.Quote(kind) + `,"id":` + strconv.Quote(id) + `}}`
}

func maPreviewDelta(text, id string) string {
	return `{"type":"event_delta","event_id":` + strconv.Quote(id) + `,"delta":{"type":"content_delta","index":0,"content":{"type":"text","text":` + strconv.Quote(text) + `}}}`
}

func maPreviewMessage(text, id string) string {
	return `{"type":"agent.message","id":` + strconv.Quote(id) + `,"content":[{"type":"text","text":` + strconv.Quote(text) + `}]}`
}

// maPreviewStream exercises the installed client's SSE parser and event_deltas option.
func maPreviewStream(t *testing.T, raw []string, each func(*rudderanthropic.SessionEvents[anthropic.BetaManagedAgentsStreamSessionEventsUnion]), stopAfter int) {
	maPreviewStreamContext(context.Background(), t, raw, each, stopAfter)
}

func maPreviewStreamContext(ctx context.Context, t *testing.T, raw []string, each func(*rudderanthropic.SessionEvents[anthropic.BetaManagedAgentsStreamSessionEventsUnion]), stopAfter int) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query()["event_deltas[]"]; !slices.Equal(got, []string{"agent.message", "agent.thinking"}) {
			t.Errorf("event_deltas: %v", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range raw {
			var e struct{ Type string }
			if err := json.Unmarshal([]byte(event), &e); err != nil {
				t.Error(err)
				return
			}
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, event)
		}
	}))
	defer server.Close()
	client := anthropic.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL))
	source := client.Beta.Sessions.Events.StreamEvents(ctx, "previews", anthropic.BetaSessionEventStreamParams{
		EventDeltas: []anthropic.BetaManagedAgentsDeltaType{anthropic.BetaManagedAgentsDeltaTypeAgentMessage, anthropic.BetaManagedAgentsDeltaTypeAgentThinking},
	})
	events := rudderanthropic.TrackSessionEvents(ctx, source,
		rudderanthropic.WithSessionID("previews"), rudderanthropic.WithSessionModel("claude-haiku-5-5"),
		rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{UserID: "u"}))
	defer events.Close()
	read := 0
	for events.Next() {
		if each != nil {
			each(events)
		}
		read++
		if read == stopAfter {
			break
		}
	}
	if err := events.Err(); err != nil {
		t.Fatal(err)
	}
}

// Reflection keeps the tests compilable when the old adapter source is restored.
func maPreviewState(t *testing.T, events *rudderanthropic.SessionEvents[anthropic.BetaManagedAgentsStreamSessionEventsUnion]) reflect.Value {
	t.Helper()
	tracker := reflect.ValueOf(events).Elem().FieldByName("tracker")
	turn := tracker.Elem().FieldByName("turn")
	if turn.IsNil() {
		t.Fatal("preview did not open a turn")
	}
	return turn.Elem().FieldByName("state").Elem()
}

func maNoPreviewText(t *testing.T, state reflect.Value) {
	t.Helper()
	if state.FieldByName("answer").String() != "" || state.FieldByName("requestText").String() != "" {
		t.Fatal("preview text stored in turn state")
	}
	if state.FieldByName("provisional").IsValid() {
		t.Fatal("turn state still has a provisional map")
	}
}

func maBufferedText(preview bool, text string) any {
	if preview {
		return nil
	}
	return text
}

func maPreviewResponse(t *testing.T, events map[string][]map[string]any, status string, text any) {
	t.Helper()
	responses := events["ai_response_received"]
	if len(responses) == 0 {
		t.Fatal("missing response")
	}
	response := responses[len(responses)-1]
	if response["response_status"] != status || response["response_text"] != text {
		t.Fatalf("response: %v", response)
	}
}

func maPreviewCallText(t *testing.T, events map[string][]map[string]any, text any) {
	t.Helper()
	calls := events["ai_llm_response_received"]
	if len(calls) != 1 || calls[0]["output_text"] != text {
		t.Fatalf("calls: %v", calls)
	}
}

func TestManagedAgentsPreviewEvents(t *testing.T) {
	start := maPreviewStart("message", "agent.message")
	delta := func(text string) string { return maPreviewDelta(text, "message") }
	message := func(text string) string { return maPreviewMessage(text, "message") }
	call := maWholeCall("")
	// Each Node case retains its exact name and runs the buffered control before previews.
	controls := func(t *testing.T, options []rudderstackai.ConfigOption, run func(*testing.T, bool, func() map[string][]map[string]any)) {
		t.Helper()
		for _, preview := range []bool{false, true} {
			t.Run(map[bool]string{false: "buffered", true: "preview"}[preview], func(t *testing.T) {
				_, rec := setup(t, options...)
				resetMA(t)
				run(t, preview, func() map[string][]map[string]any { return rec.events(t) })
			})
		}
	}
	output := func(preview bool, text string) []string {
		if preview {
			return []string{start, delta(text)}
		}
		return []string{message(text)}
	}
	for _, capture := range []bool{false, true} {
		t.Run(fmt.Sprintf("preview-only fresh turn (capture %t), buffered control", capture), func(t *testing.T) {
			controls(t, []rudderstackai.ConfigOption{rudderstackai.WithCaptureContent(capture)}, func(t *testing.T, preview bool, recorded func() map[string][]map[string]any) {
				raw := []string{message("partial answer")}
				if preview {
					raw = []string{start, delta("partial"), delta(" answer")}
				}
				maPreviewStream(t, raw, nil, 0)
				events := recorded()
				if len(events["ai_user_prompt_created"]) != 1 || len(events["ai_response_received"]) != 1 {
					t.Fatalf("events: %v", events)
				}
				var text any
				if capture && !preview {
					text = "partial answer"
				}
				maPreviewResponse(t, events, "cancelled", text)
			})
		})
	}
	capture := []rudderstackai.ConfigOption{rudderstackai.WithCaptureContent(true)}
	calls := append(slices.Clone(capture), rudderstackai.WithLLMCallEvents(true))
	t.Run("previews after a pause resume the answer, buffered control", func(t *testing.T) {
		controls(t, capture, func(t *testing.T, preview bool, recorded func() map[string][]map[string]any) {
			maPreviewStream(t, []string{maWholeEvent("user.message"), maWholeIdle("requires_action")}, nil, 0)
			raw := output(preview, "resumed")
			maPreviewStream(t, raw, nil, len(raw))
			events := recorded()
			answers := events["ai_response_received"]
			if len(answers) != 2 || answers[0]["response_status"] != "paused" || answers[1]["response_status"] != "cancelled" || answers[1]["interaction_id"] != answers[0]["interaction_id"] || len(events["ai_user_prompt_created"]) != 1 {
				t.Fatalf("events: %v", events)
			}
			maPreviewResponse(t, events, "cancelled", maBufferedText(preview, "resumed"))
		})
	})
	t.Run("interrupt before the buffered message has no answer or call text", func(t *testing.T) {
		controls(t, calls, func(t *testing.T, preview bool, recorded func() map[string][]map[string]any) {
			raw := []string{maWholeEvent("user.message"), `{"type":"span.model_request_start","id":"request"}`}
			raw = append(raw, output(preview, "partial")...)
			raw = append(raw, maWholeEvent("user.interrupt"), maWholeCall(`,"is_error":true,"model_request_start_id":"request"`), maWholeIdle("end_turn"))
			maPreviewStream(t, raw, nil, 0)
			maPreviewResponse(t, recorded(), "cancelled", maBufferedText(preview, "partial"))
			maPreviewCallText(t, recorded(), maBufferedText(preview, "partial"))
		})
	})
	t.Run("previews then buffered message record only buffered text without double counting", func(t *testing.T) {
		controls(t, calls, func(t *testing.T, preview bool, recorded func() map[string][]map[string]any) {
			raw := []string{message("complete")}
			var carried reflect.Value
			if preview {
				raw = []string{start, delta("preview-only text")}
			}
			maPreviewStream(t, append(raw, maWholeIdle("requires_action")), func(events *rudderanthropic.SessionEvents[anthropic.BetaManagedAgentsStreamSessionEventsUnion]) {
				if preview {
					carried = maPreviewState(t, events)
				}
			}, 0)
			if len(recorded()["ai_response_received"]) != 1 {
				t.Fatal("missing paused response")
			}
			if preview {
				maNoPreviewText(t, carried)
			}
			maPreviewStream(t, []string{message("complete"), call, maWholeIdle("end_turn")}, nil, 0)
			maPreviewResponse(t, recorded(), "success", "complete")
			maPreviewCallText(t, recorded(), "complete")
		})
	})
	t.Run("previews and buffered message in one stream record only buffered output", func(t *testing.T) {
		controls(t, calls, func(t *testing.T, preview bool, recorded func() map[string][]map[string]any) {
			var raw []string
			if preview {
				raw = []string{start, delta("preview-only text")}
			}
			raw = append(raw, message("complete"), call, maWholeIdle("end_turn"))
			read := 0
			maPreviewStream(t, raw, func(events *rudderanthropic.SessionEvents[anthropic.BetaManagedAgentsStreamSessionEventsUnion]) {
				read++
				if preview && read == 2 {
					maNoPreviewText(t, maPreviewState(t, events))
				}
			}, 0)
			maPreviewResponse(t, recorded(), "success", "complete")
			maPreviewCallText(t, recorded(), "complete")
		})
	})
	for _, ending := range []string{"end", "error", "close"} {
		t.Run("preview-only output is absent on "+ending+", buffered control", func(t *testing.T) {
			controls(t, capture, func(t *testing.T, preview bool, recorded func() map[string][]map[string]any) {
				raw := output(preview, "partial")
				status := "cancelled"
				switch ending {
				case "end":
					raw = append(raw, maWholeIdle("end_turn"))
					status = "success"
				case "error":
					raw = append(raw, `{"type":"session.error","error":{"type":"mcp_authentication_failed_error","retry_status":{"type":"terminal"}}}`)
					status = "error"
				}
				maPreviewStream(t, raw, nil, 0)
				maPreviewResponse(t, recorded(), status, maBufferedText(preview, "partial"))
			})
		})
	}
	t.Run("thinking previews open a turn without answer text, buffered control", func(t *testing.T) {
		controls(t, capture, func(t *testing.T, preview bool, recorded func() map[string][]map[string]any) {
			raw := `{"type":"agent.thinking","id":"thinking"}`
			if preview {
				raw = maPreviewStart("thinking", "agent.thinking")
			}
			maPreviewStream(t, []string{raw}, nil, 0)
			events := recorded()
			if len(events["ai_user_prompt_created"]) != 1 || len(events["ai_response_received"]) != 1 {
				t.Fatalf("events: %v", events)
			}
			maPreviewResponse(t, events, "cancelled", nil)
		})
	})
	t.Run("preview IDs do not mark buffered events as read, buffered control", func(t *testing.T) {
		controls(t, calls, func(t *testing.T, preview bool, recorded func() map[string][]map[string]any) {
			var raw []string
			if preview {
				raw = []string{strings.Replace(start, `{`, `{"id":"message",`, 1), strings.Replace(delta("partial"), `{`, `{"id":"message",`, 1)}
			}
			raw = append(raw, message("complete"), message("complete"), call, maWholeIdle("end_turn"))
			maPreviewStream(t, raw, nil, 0)
			maPreviewResponse(t, recorded(), "success", "complete")
			maPreviewCallText(t, recorded(), "complete")
		})
	})
	t.Run("carries preview-opened state through pause without storing preview text", func(t *testing.T) {
		_, rec := setup(t, rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true), rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: 2}))
		resetMA(t)
		var carried reflect.Value
		maPreviewStream(t, []string{start, delta("partial"), maWholeIdle("requires_action")}, func(events *rudderanthropic.SessionEvents[anthropic.BetaManagedAgentsStreamSessionEventsUnion]) {
			carried = maPreviewState(t, events)
		}, 0)
		maNoPreviewText(t, carried)
		maPreviewStream(t, []string{message("complete"), call, maWholeIdle("end_turn")}, nil, 0)
		maPreviewResponse(t, rec.events(t), "success", "complete")
		maPreviewCallText(t, rec.events(t), "complete")
		var raw []string
		for _, id := range []string{"oldest", "middle", "newest"} {
			raw = append(raw, maPreviewStart(id, "agent.message"), maPreviewDelta(strings.Repeat("x", 40_000), id))
		}
		var state reflect.Value
		maPreviewStream(t, raw, func(events *rudderanthropic.SessionEvents[anthropic.BetaManagedAgentsStreamSessionEventsUnion]) {
			state = maPreviewState(t, events)
		}, 0)
		maNoPreviewText(t, state)
	})
	t.Run("only buffered text uses capture and redaction rules", func(t *testing.T) {
		options := append(slices.Clone(calls), rudderstackai.WithRedactHook(func(string) (string, error) { return "masked", nil }))
		controls(t, options, func(t *testing.T, preview bool, recorded func() map[string][]map[string]any) {
			raw := append(output(preview, "private"), call, maWholeIdle("end_turn"))
			maPreviewStream(t, raw, nil, 0)
			maPreviewResponse(t, recorded(), "success", maBufferedText(preview, "masked"))
			maPreviewCallText(t, recorded(), maBufferedText(preview, "masked"))
			encoded, err := json.Marshal(recorded())
			if err != nil || strings.Contains(string(encoded), "private") {
				t.Fatal("unredacted preview text in events")
			}
		})
	})
}

// Go streams expose errors through Err rather than throwing named JavaScript errors.
func maStreamEndings() []struct {
	name string
	err  error
} {
	return []struct {
		name string
		err  error
	}{
		{"clean EOF", nil},
		{"context.Canceled", context.Canceled},
		{"context.DeadlineExceeded", context.DeadlineExceeded},
		{"net.OpError", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("stream failed")}},
		{"url.Error", &url.Error{Op: "Get", URL: "https://example.invalid", Err: errors.New("stream failed")}},
		{"json.SyntaxError", &json.SyntaxError{Offset: 1}},
		{"json.UnmarshalTypeError", &json.UnmarshalTypeError{Value: "number", Type: reflect.TypeFor[string]()}},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF},
		{"anthropic.Error", &anthropic.Error{StatusCode: http.StatusInternalServerError}},
		{"Error", errors.New("stream failed")},
	}
}

func TestManagedAgentsPausesAWaitingAnswerAcrossStreamEndings(t *testing.T) {
	for _, ending := range maStreamEndings() {
		for _, borrowed := range []bool{false, true} {
			t.Run(fmt.Sprintf("pauses a waiting answer across %s (borrowed %t)", ending.name, borrowed), func(t *testing.T) {
				_, rec := setup(t, rudderstackai.WithToolBreakdown(true))
				resetMA(t)
				ctx := context.Background()
				var caller *rudderstackai.Interaction
				if borrowed {
					ctx, caller = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "u"})
				}
				sessionID := "sesn_wait_" + ending.name + strconv.FormatBool(borrowed)
				var hintedStatus, hintedError, hintedReason string
				original := hook.HintOutcome
				hook.HintOutcome = func(i any, status, errorType, reason string) {
					if i == caller {
						hintedStatus, hintedError, hintedReason = status, errorType, reason
					}
					original(i, status, errorType, reason)
				}
				defer func() { hook.HintOutcome = original }()
				source := maEvents(t,
					`{"id":"wait_running","type":"session.status_running"}`,
					`{"id":"wait_prompt","type":"user.message","content":[]}`,
					`{"id":"wait_call","type":"span.model_request_end","is_error":false,"model_usage":{"input_tokens":10,"output_tokens":2}}`,
					`{"id":"wait_tool","type":"agent.custom_tool_use","name":"lookup","input":{}}`,
					`{"id":"wait_idle","type":"session.status_idle","stop_reason":{"type":"requires_action","event_ids":["wait_tool"]}}`)
				source.err = ending.err
				events := rudderanthropic.TrackSessionEvents(ctx, source, rudderanthropic.WithSessionID(sessionID))
				maDrain(events)
				if events.Err() != ending.err {
					t.Fatal("stream error changed")
				}
				_ = events.Close()
				if borrowed {
					if hintedStatus != "paused" || hintedError != "" || hintedReason != "deferred_tool" {
						t.Fatalf("hint: %s %s %s", hintedStatus, hintedError, hintedReason)
					}
					if len(rec.events(t)["ai_response_received"]) != 0 {
						t.Fatal("ended caller interaction")
					}
				} else {
					first := rec.events(t)["ai_response_received"]
					if len(first) != 1 || first[0]["response_status"] != "paused" || first[0]["tool_call_count"] != float64(0) {
						t.Fatalf("first: %v", first)
					}
				}
				resumed := rudderanthropic.TrackSessionEvents(ctx, maEvents(t,
					`{"id":"wait_result","type":"user.custom_tool_result","custom_tool_use_id":"wait_tool","is_error":true}`,
					`{"id":"wait_end","type":"session.status_idle","stop_reason":{"type":"end_turn"}}`), rudderanthropic.WithSessionID(sessionID))
				maDrain(resumed)
				if caller != nil {
					caller.End(rudderstackai.EndOptions{})
				}
				responses := rec.events(t)["ai_response_received"]
				count := 2
				if borrowed {
					count = 1
				}
				if len(responses) != count {
					t.Fatalf("responses: %v", responses)
				}
				var tools float64
				for _, response := range responses {
					if response["interaction_id"] != responses[0]["interaction_id"] {
						t.Fatal("interaction changed")
					}
					tools += response["tool_call_count"].(float64)
				}
				last := responses[len(responses)-1]
				if last["response_status"] != "success" || tools != 1 {
					t.Fatalf("responses: %v", responses)
				}
				breakdown := last["tool_breakdown"].([]any)
				if len(breakdown) != 1 || breakdown[0].(map[string]any)["tool_name"] != "lookup" || breakdown[0].(map[string]any)["error_count"] != float64(1) {
					t.Fatalf("tools: %v", breakdown)
				}
			})
		}
	}
}

func TestManagedAgentsCarriesAnUnfinishedCallerTurnAcrossStreamEndingsAndReplay(t *testing.T) {
	for _, ending := range maStreamEndings() {
		t.Run("carries an unfinished caller turn across "+ending.name+" and replay", func(t *testing.T) {
			_, rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true))
			resetMA(t)
			ctx, caller := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
			first := []string{
				`{"id":"carry_prompt","type":"user.message","content":[]}`,
				`{"id":"carry_call","type":"span.model_request_end","is_error":false,"model_usage":{"input_tokens":10,"output_tokens":2}}`,
				`{"id":"carry_tool","type":"agent.tool_use","name":"bash","input":{}}`,
				`{"id":"carry_start","type":"span.model_request_start"}`,
				`{"id":"carry_text","type":"agent.message","content":[{"type":"text","text":"carried output"}]}`,
			}
			source := maEvents(t, first...)
			source.err = ending.err
			events := rudderanthropic.TrackSessionEvents(ctx, source, rudderanthropic.WithSessionID("sesn_carry"))
			maDrain(events)
			if events.Err() != ending.err {
				t.Fatal("stream error changed")
			}
			_, _, carried := rudderanthropic.InspectManagedAgents(events, "sesn_carry")
			if carried != caller {
				t.Fatal("unfinished caller state was discarded")
			}
			second := append(slices.Clone(first),
				`{"id":"carry_result","type":"agent.tool_result","tool_use_id":"carry_tool","is_error":true}`,
				`{"id":"carry_call_end","type":"span.model_request_end","is_error":false,"model_request_start_id":"carry_start","model_usage":{"input_tokens":5,"output_tokens":1}}`,
				`{"id":"carry_end","type":"session.status_idle","stop_reason":{"type":"end_turn"}}`)
			resumed := rudderanthropic.TrackSessionEvents(ctx, maEvents(t, second...), rudderanthropic.WithSessionID("sesn_carry"))
			maDrain(resumed)
			_, _, carried = rudderanthropic.InspectManagedAgents(resumed, "sesn_carry")
			if carried != nil {
				t.Fatal("finished caller state retained")
			}
			caller.End(rudderstackai.EndOptions{})
			responses := rec.events(t)["ai_response_received"]
			if len(responses) != 1 || responses[0]["response_status"] != "success" || responses[0]["llm_call_count"] != float64(2) || responses[0]["tool_call_count"] != float64(1) {
				t.Fatalf("responses: %v", responses)
			}
			breakdown := responses[0]["tool_breakdown"].([]any)
			if len(breakdown) != 1 || breakdown[0].(map[string]any)["tool_name"] != "bash" || breakdown[0].(map[string]any)["error_count"] != float64(1) {
				t.Fatalf("tools: %v", breakdown)
			}
			calls := rec.events(t)["ai_llm_response_received"]
			if len(calls) != 2 || calls[1]["output_text"] != "carried output" {
				t.Fatalf("calls: %v", calls)
			}
		})
	}
}

func TestManagedAgentsCancelsAPrimaryThreadInterruptAfterTheCassetteThreadStatuses(t *testing.T) {
	t.Run("cancels a primary-thread interrupt after the cassette thread statuses", func(t *testing.T) {
		_, rec := setup(t)
		resetMA(t)
		raw, err := os.ReadFile("../spec/fixtures/cassettes/claude-managed-agents-tools.json")
		if err != nil {
			t.Fatal(err)
		}
		var cassette cassetteTransport
		var data struct{ Interactions json.RawMessage }
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data.Interactions, &cassette.queue); err != nil {
			t.Fatal(err)
		}
		var events []json.RawMessage
		for _, interaction := range cassette.queue {
			for _, event := range interaction.Response.Events {
				events = append(events, event.Payload)
			}
		}
		idle := -1
		primary := ""
		for n, raw := range events {
			var event struct {
				Type   string
				Thread string `json:"session_thread_id"`
			}
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			if event.Type == "session.thread_status_idle" {
				idle, primary = n, event.Thread
				break
			}
		}
		if idle < 0 || primary == "" {
			t.Fatal("cassette has no primary thread idle")
		}
		var second struct {
			Type   string
			Thread string `json:"session_thread_id"`
		}
		if err := json.Unmarshal(events[1], &second); err != nil {
			t.Fatal(err)
		}
		if second.Type != "session.thread_status_running" || second.Thread != primary {
			t.Fatal("cassette event order changed")
		}
		events = append(events[:idle+1], json.RawMessage(`{"id":"primary_interrupt","type":"user.interrupt","session_thread_id":`+strconv.Quote(primary)+`}`), json.RawMessage(`{"id":"primary_end","type":"session.status_idle","stop_reason":{"type":"end_turn"}}`))
		tracked := rudderanthropic.TrackSessionEvents(context.Background(), &sliceStream{events: decodeEvents(t, events)}, rudderanthropic.WithSessionID("sesn_fixture"))
		maDrain(tracked)
		responses := rec.events(t)["ai_response_received"]
		if len(responses) != 1 || responses[0]["response_status"] != "cancelled" || responses[0]["error_type"] != "cancelled" {
			t.Fatalf("responses: %v", responses)
		}
	})
}

func TestManagedAgentsResolvesConversationIDsInOrder(t *testing.T) {
	for _, row := range []struct{ option, context, session, expected string }{
		{"option", "context", "session", "option"}, {"", "context", "session", "context"}, {"", "", "session", "session"}, {},
	} {
		t.Run(fmt.Sprintf("resolves conversation IDs in order (%s, %s, %s)", row.option, row.context, row.session), func(t *testing.T) {
			_, rec := setup(t)
			resetMA(t)
			_, scenario := maFixtureScenario(t, "two_turns")
			ctx := rudderstackai.WithValues(context.Background(), rudderstackai.ContextValues{ConversationID: row.context})
			read := func(ctx context.Context) {
				consumeMA(ctx, t, scenario.Streams[0], []rudderanthropic.SessionOption{rudderanthropic.WithSessionID(row.session), rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{ConversationID: row.option}), rudderanthropic.WithSessionModel("claude-haiku-5-5")})
			}
			read(ctx)
			responses := rec.events(t)["ai_response_received"]
			if len(responses) != 2 || responses[0]["conversation_id"] == "" || responses[1]["conversation_id"] != responses[0]["conversation_id"] || (row.expected != "" && responses[0]["conversation_id"] != row.expected) {
				t.Fatalf("responses: %v", responses)
			}
			if row.expected == "" {
				read(context.Background())
				next := rec.events(t)["ai_response_received"][2:]
				if len(next) != 2 || next[0]["conversation_id"] == responses[0]["conversation_id"] || next[1]["conversation_id"] != next[0]["conversation_id"] {
					t.Fatalf("next: %v", next)
				}
			}
		})
	}
}

func TestManagedAgentsMapsSessionErrors(t *testing.T) {
	for _, row := range []struct{ kind, errorType string }{{"billing_error", "rate_limit"}, {"credential_host_unreachable_error", "connection_error"}, {"repository_forbidden_error", "auth_error"}} {
		t.Run("maps session "+row.kind+" to "+row.errorType, func(t *testing.T) {
			_, rec := setup(t)
			resetMA(t)
			events := rudderanthropic.TrackSessionEvents(context.Background(), maEvents(t,
				`{"id":"mapping_prompt","type":"user.message","content":[]}`,
				`{"id":"mapping_error","type":"session.error","error":{"type":`+strconv.Quote(row.kind)+`}}`,
				`{"id":"mapping_idle","type":"session.status_idle","stop_reason":{"type":"retries_exhausted"}}`), rudderanthropic.WithSessionID("sesn_fixture"))
			maDrain(events)
			responses := rec.events(t)["ai_response_received"]
			if len(responses) != 1 || responses[0]["response_status"] != "error" || responses[0]["error_type"] != row.errorType {
				t.Fatalf("responses: %v", responses)
			}
		})
	}
}

func TestManagedAgentsKeepsConversationIDWhenMaxOpenInteractionsIsReached(t *testing.T) {
	_, rec := setup(t, rudderstackai.WithLimits(rudderstackai.Limits{MaxOpenInteractions: 1}))
	resetMA(t)
	_, caller := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	source := maEvents(t, maWholeEvent("user.message"), maWholeIdle("end_turn"), maWholeEvent("user.message"), maWholeIdle("end_turn"))
	events := rudderanthropic.TrackSessionEvents(context.Background(), source, rudderanthropic.WithSessionID("sesn_limit"), rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{UserID: "u"}))
	for events.Next() {
		if source.n == 2 {
			caller.End(rudderstackai.EndOptions{})
		}
	}
	responses := rec.events(t)["ai_response_received"]
	if len(responses) != 2 || responses[1]["conversation_id"] != "sesn_limit" {
		t.Fatalf("responses: %v", responses)
	}
}

func TestManagedAgentsEventsStayUntrackedInsideTheSDKsOwnUntrackedContext(t *testing.T) {
	_, rec := setup(t)
	resetMA(t)
	done := make(chan error, 1)
	source := maEvents(t, maWholeEvent("user.message"), maWholeCall(""), maWholeIdle("end_turn"))
	if err := rudderstackai.Configure(rudderstackai.WithEvaluator(func(ctx context.Context, _ rudderstackai.EvaluationTurn) ([]rudderstackai.Score, error) {
		if !kit.Untracked(ctx) {
			done <- errors.New("evaluator context is tracked")
			return nil, nil
		}
		events := rudderanthropic.TrackSessionEvents(ctx, source, rudderanthropic.WithSessionID("untracked"))
		n := 0
		for events.Next() {
			n++
			if events.Current().RawJSON() != source.Current().RawJSON() {
				done <- errors.New("event changed")
				return nil, nil
			}
		}
		_ = events.Close()
		if n != 3 || events.Err() != nil || !source.closed {
			done <- errors.New("untracked events not passed through")
			return nil, nil
		}
		done <- nil
		return nil, nil
	})); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rudderstackai.Configure(rudderstackai.WithEvaluator(nil)) })
	_, caller := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
	caller.End(rudderstackai.EndOptions{})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("evaluator did not run")
	}
	events := rec.events(t)
	if len(events["ai_user_prompt_created"]) != 1 || len(events["ai_response_received"]) != 1 || len(events["ai_llm_response_received"]) != 0 {
		t.Fatalf("events: %v", events)
	}
}

func TestManagedAgentsNeverRecordsPreviewText(t *testing.T) {
	for _, path := range []string{"fresh", "interrupted", "ended", "errored", "closed", "paused-then-resumed", "caller-interaction", "spill"} {
		t.Run("never records preview text ("+path+")", func(t *testing.T) {
			cfg := []rudderstackai.ConfigOption{rudderstackai.WithCaptureContent(true), rudderstackai.WithLLMCallEvents(true)}
			if path == "spill" {
				cfg = append(cfg, rudderstackai.WithLimits(rudderstackai.Limits{MaxCallsPerInteraction: 1}))
			}
			_, rec := setup(t, cfg...)
			resetMA(t)
			previews := func(suffix string) []string {
				id := "message-" + suffix
				return []string{
					`{"type":"event_start","event":{"type":"agent.message","id":` + strconv.Quote(id) + `,"content":[{"type":"text","text":` + strconv.Quote("preview-start-"+suffix) + `}]}}`,
					maPreviewDelta("preview-message-"+suffix, id),
					`{"type":"event_delta","event_id":` + strconv.Quote(id) + `,"delta":{"type":"content_delta","content":{"type":"text","text":` + strconv.Quote("preview-unindexed-"+suffix) + `}}}`,
					maPreviewStart("thinking-"+suffix, "agent.thinking"),
					maPreviewDelta("preview-thinking-"+suffix, "thinking-"+suffix),
				}
			}
			ctx := context.Background()
			var caller *rudderstackai.Interaction
			if path == "caller-interaction" {
				ctx, caller = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "u"})
			}
			if path == "paused-then-resumed" {
				maPreviewStreamContext(ctx, t, append(previews("paused"), maWholeCall(""), maWholeIdle("requires_action")), nil, 0)
				maPreviewStreamContext(ctx, t, append(previews("resumed"), maWholeCall(""), maWholeIdle("end_turn")), nil, 0)
				responses := rec.events(t)["ai_response_received"]
				if len(responses) != 2 || responses[0]["response_status"] != "paused" || responses[1]["response_status"] != "success" || responses[1]["interaction_id"] != responses[0]["interaction_id"] {
					t.Fatalf("responses: %v", responses)
				}
			} else {
				raw := previews(path)
				extra := ""
				if path == "interrupted" {
					raw = append(raw, maWholeEvent("user.interrupt"))
					extra = `,"is_error":true`
				}
				raw = append(raw, maWholeCall(extra))
				if path == "ended" || path == "interrupted" || path == "spill" {
					raw = append(raw, maWholeIdle("end_turn"))
				}
				if path == "errored" {
					raw = append(raw, `{"type":"session.error","error":{"type":"model_request_failed_error","retry_status":{"type":"terminal"}}}`)
				}
				stopAfter := 0
				if path == "closed" {
					stopAfter = len(raw)
				}
				maPreviewStreamContext(ctx, t, raw, nil, stopAfter)
			}
			if caller != nil {
				caller.End(rudderstackai.EndOptions{})
			}
			events := rec.events(t)
			if len(events["ai_user_prompt_created"]) != 1 || len(events["ai_response_received"]) == 0 || len(events["ai_llm_response_received"]) == 0 {
				t.Fatalf("events: %v", events)
			}
			encoded, err := json.Marshal(events)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "preview-") {
				t.Fatal("preview text captured")
			}
			for _, named := range events {
				for _, event := range named {
					if _, ok := event["response_text"]; ok {
						t.Fatal("preview response_text captured")
					}
					if _, ok := event["output_text"]; ok {
						t.Fatal("preview output_text captured")
					}
				}
			}
		})
	}
}

// Blocking regression cases mirror the Node reply kinds, owners, orders and stream endings.
var maReplyKinds = []string{"confirmation", "custom", "tool"}

func maBlockingReply(kind, id string, eventIDs ...string) string {
	eventID := "reply-" + id
	if len(eventIDs) != 0 {
		eventID = eventIDs[0]
	}
	if kind == "confirmation" {
		return fmt.Sprintf(`{"type":"user.tool_confirmation","id":%q,"tool_use_id":%q,"result":"allow"}`, eventID, id)
	}
	eventType, key := "user.custom_tool_result", "custom_tool_use_id"
	if kind == "tool" {
		eventType, key = "user.tool_result", "tool_use_id"
	}
	return fmt.Sprintf(`{"type":%q,"id":%q,%q:%q,"content":[{"type":"text","text":"done"}]}`, eventType, eventID, key, id)
}

func maBlockingOpening(kinds ...string) []string {
	raw := []string{`{"type":"user.message","id":"prompt"}`, `{"type":"span.model_request_end","id":"model","model_usage":{"input_tokens":10,"output_tokens":2}}`}
	var ids []string
	for n, kind := range kinds {
		typ, permission := "agent.tool_use", "allow"
		if kind == "custom" {
			typ = "agent.custom_tool_use"
		}
		if kind == "confirmation" {
			permission = "ask"
		}
		id := fmt.Sprintf("tool-%d", n)
		ids = append(ids, id)
		raw = append(raw, fmt.Sprintf(`{"type":%q,"id":%q,"name":"tool_%d","evaluated_permission":%q}`, typ, id, n, permission))
	}
	return append(raw, maWholeIdle("requires_action", ids...))
}

func maBlockingReason(kind string) string {
	if kind == "confirmation" {
		return "approval"
	}
	return "deferred_tool"
}

func maBlockingStream(ctx context.Context, t *testing.T, raw []string) *rudderanthropic.SessionEvents[anthropic.BetaManagedAgentsStreamSessionEventsUnion] {
	t.Helper()
	events := rudderanthropic.TrackSessionEvents(ctx, maEvents(t, raw...),
		rudderanthropic.WithSessionID("blocking-regression"), rudderanthropic.WithSessionModel("claude-haiku-5-5"),
		rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{UserID: "u"}))
	t.Cleanup(func() { _ = events.Close() })
	return events
}

func maBlockingRead(ctx context.Context, t *testing.T, raw []string, end string) (*rudderanthropic.SessionEvents[anthropic.BetaManagedAgentsStreamSessionEventsUnion], reflect.Value) {
	t.Helper()
	src := maEvents(t, raw...)
	failure := errors.New("stream disconnected")
	if end == "failed" {
		src.err = failure
	}
	events := rudderanthropic.TrackSessionEvents(ctx, src,
		rudderanthropic.WithSessionID("blocking-regression"), rudderanthropic.WithSessionModel("claude-haiku-5-5"),
		rudderanthropic.WithInteractionOptions(rudderstackai.InteractionOptions{UserID: "u"}))
	t.Cleanup(func() { _ = events.Close() })
	for range raw {
		if !events.Next() {
			t.Fatal("stream ended before its events")
		}
	}
	state := maPreviewState(t, events)
	if end == "closed" {
		_ = events.Close()
	} else if events.Next() {
		t.Fatal("stream did not end")
	}
	if end == "failed" && events.Err() != failure {
		t.Fatal("stream error changed")
	}
	return events, state
}

func maBlockingWait(t *testing.T, state reflect.Value, reason string, keys ...string) {
	t.Helper()
	waiting, outstanding := state.FieldByName("waiting"), state.FieldByName("outstanding")
	if !waiting.IsValid() || !outstanding.IsValid() {
		t.Fatal("turn state must carry waiting and outstanding calls")
	}
	if waiting.String() != reason {
		t.Errorf("waiting: got %q, want %q", waiting.String(), reason)
	}
	order := outstanding.Elem().FieldByName("order")
	var got []string
	for n := 0; n < order.Len(); n++ {
		got = append(got, order.Index(n).String())
	}
	if !slices.Equal(got, keys) || outstanding.Elem().FieldByName("m").Len() != len(keys) {
		t.Errorf("outstanding: got %v, want %v", got, keys)
	}
}

func maBlockingCarried(t *testing.T, events *rudderanthropic.SessionEvents[anthropic.BetaManagedAgentsStreamSessionEventsUnion], owner string) {
	t.Helper()
	_, paused, borrowed := rudderanthropic.InspectManagedAgents(events, "blocking-regression")
	if (owner == "adapter" && !paused) || (owner == "interaction" && borrowed == nil) {
		t.Fatal("remaining call did not keep the same turn carried")
	}
}

func maBlockingOwned(t *testing.T, owner string, run func(context.Context)) {
	t.Helper()
	ctx := context.Background()
	if owner == "interaction" {
		var caller *rudderstackai.Interaction
		ctx, caller = rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "u"})
		defer caller.End(rudderstackai.EndOptions{})
	}
	run(ctx)
}

func maBlockingCountedOnce(t *testing.T, rec *recorder, count int) {
	t.Helper()
	all := rec.events(t)
	responses := all["ai_response_received"]
	ids := map[any]bool{}
	for _, response := range responses {
		ids[response["interaction_id"]] = true
		if status := response["response_status"]; status != "paused" && status != "success" {
			t.Errorf("response status: %v", status)
		}
	}
	if len(ids) != 1 || len(responses) == 0 || responses[len(responses)-1]["response_status"] != "success" {
		t.Fatalf("responses: %v", responses)
	}
	for key, expected := range map[string]float64{"tool_call_count": float64(count), "llm_call_count": 1, "total_input_tokens": 10, "total_output_tokens": 2} {
		var total float64
		for _, response := range responses {
			value, _ := response[key].(float64)
			total += value
		}
		if total != expected {
			t.Errorf("%s: got %v, want %v", key, total, expected)
		}
	}
	if len(all["ai_user_prompt_created"]) != 1 {
		t.Errorf("prompts: %v", all["ai_user_prompt_created"])
	}
}

func TestManagedAgentsBlockingSplitReplies(t *testing.T) {
	for _, owner := range []string{"adapter", "interaction"} {
		for _, a := range maReplyKinds {
			for _, b := range maReplyKinds {
				for _, order := range []string{"forward", "reverse"} {
					for _, end := range []string{"clean", "closed", "failed"} {
						t.Run(strings.Join([]string{owner, a + "+" + b, order, end}, "/"), func(t *testing.T) {
							_, rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithLimits(rudderstackai.Limits{}))
							resetMA(t)
							kinds, first, second := []string{a, b}, 0, 1
							if order == "reverse" {
								first, second = 1, 0
							}
							maBlockingOwned(t, owner, func(ctx context.Context) {
								initial := append(maBlockingOpening(kinds...), maBlockingReply(kinds[first], fmt.Sprintf("tool-%d", first)))
								events, state := maBlockingRead(ctx, t, initial, end)
								maBlockingCarried(t, events, owner)
								if owner == "adapter" && rec.events(t)["ai_response_received"][0]["response_status"] != "paused" {
									t.Fatal("first answer must pause")
								}
								maBlockingWait(t, state, maBlockingReason(kinds[second]), fmt.Sprintf("tool-%d", second))
								maDrain(maBlockingStream(ctx, t, append(initial, maBlockingReply(kinds[second], fmt.Sprintf("tool-%d", second)), maWholeEvent("session.status_running"), maWholeIdle("end_turn"))))
							})
							maBlockingCountedOnce(t, rec, 2)
						})
					}
				}
			}
		}
	}
}

func maTimed(raw string, ms int) string { return strings.TrimSuffix(raw, "}") + "," + maAt(ms) + "}" }

// An approved call runs only after the reconnect: its result there keeps its outcome and time,
// as an app reply does.
func TestManagedAgentsBlockingSplitResultsKeepOutcome(t *testing.T) {
	for _, owner := range []string{"adapter", "interaction"} {
		for _, a := range maReplyKinds {
			for _, b := range maReplyKinds {
				for _, order := range []string{"forward", "reverse"} {
					for _, end := range []string{"clean", "closed", "failed"} {
						t.Run(strings.Join([]string{owner, a + "+" + b, order, end}, "/"), func(t *testing.T) {
							_, rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithLimits(rudderstackai.Limits{}))
							resetMA(t)
							kinds, first, second := []string{a, b}, 0, 1
							if order == "reverse" {
								first, second = 1, 0
							}
							// An app reply is the call's error result; a confirmation lets the agent run it later.
							answer := func(n, ms int) string {
								reply := maBlockingReply(kinds[n], fmt.Sprintf("tool-%d", n))
								if kinds[n] != "confirmation" {
									reply = strings.TrimSuffix(reply, "}") + `,"is_error":true}`
								}
								return maTimed(reply, ms)
							}
							var ran []string
							for n, kind := range kinds {
								if kind == "confirmation" {
									ran = append(ran, maTimed(fmt.Sprintf(`{"type":"agent.tool_result","id":"ran-%d","tool_use_id":"tool-%d","is_error":true}`, n, n), 1500))
								}
							}
							maBlockingOwned(t, owner, func(ctx context.Context) {
								var initial []string
								for _, raw := range maBlockingOpening(kinds...) {
									ms := 0
									if strings.Contains(raw, `tool_use"`) {
										ms = 100
									}
									initial = append(initial, maTimed(raw, ms))
								}
								maBlockingRead(ctx, t, append(initial, answer(first, 600)), end)
								second := append([]string{answer(second, 1000), maTimed(`{"type":"session.status_running","id":"running"}`, 1100)}, ran...)
								maDrain(maBlockingStream(ctx, t, append(second, maTimed(`{"type":"session.status_idle","id":"done","stop_reason":{"type":"end_turn"}}`, 1600))))
							})
							maBlockingCountedOnce(t, rec, 2)
							for n := range kinds {
								var got []map[string]any
								for _, response := range rec.events(t)["ai_response_received"] {
									breakdown, _ := response["tool_breakdown"].([]any)
									for _, e := range breakdown {
										if entry := e.(map[string]any); entry["tool_name"] == fmt.Sprintf("tool_%d", n) {
											got = append(got, entry)
										}
									}
								}
								want := 1000
								if n == first {
									want = 600
								}
								if kinds[n] == "confirmation" {
									want = 1500
								}
								if len(got) != 1 || got[0]["call_count"] != float64(1) || got[0]["error_count"] != float64(1) || got[0]["duration_ms"] != float64(want-100) {
									t.Errorf("tool_%d: %v", n, got)
								}
							}
						})
					}
				}
			}
		}
	}
}

// A reconnect that missed the session-wide transition still reads the threads' own statuses.
func TestManagedAgentsBlockingThreadStatusResumes(t *testing.T) {
	for _, owner := range []string{"adapter", "interaction"} {
		for _, kind := range []string{"session.thread_status_running", "session.thread_status_rescheduled"} {
			for _, thread := range []string{"child", "primary"} {
				t.Run(owner+"/"+kind+"/"+thread, func(t *testing.T) {
					_, rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithLimits(rudderstackai.Limits{}))
					resetMA(t)
					threadID := "sthr_main"
					if thread == "child" {
						threadID = "child"
					}
					var events *rudderanthropic.SessionEvents[anthropic.BetaManagedAgentsStreamSessionEventsUnion]
					maBlockingOwned(t, owner, func(ctx context.Context) {
						maBlockingRead(ctx, t, append(maBlockingChild(maBlockingOpening("custom", "custom")), maBlockingReply("custom", "tool-0")), "clean")
						// tool-1 was answered while disconnected; the session went on running.
						events = maBlockingStream(ctx, t, []string{fmt.Sprintf(`{"type":%q,"id":"thread-status","session_thread_id":%q}`, kind, threadID)})
						maDrain(events)
					})
					if _, paused, _ := rudderanthropic.InspectManagedAgents(events, "blocking-regression"); paused {
						t.Fatal("still paused")
					}
					if owner == "adapter" {
						maBlockingStatuses(t, rec, "paused", "cancelled")
					} else {
						maBlockingStatuses(t, rec, "cancelled")
					}
				})
			}
		}
	}
}

// maRecorded is a turn's start in the recorded order: the session's running status, the primary
// thread's, then the prompt.
func maRecorded(kind, p string) []string {
	return []string{
		fmt.Sprintf(`{"type":"session.status_running","id":"%s-running"}`, p),
		fmt.Sprintf(`{"type":%q,"id":"%s-thread","session_thread_id":"sthr_main"}`, kind, p),
		fmt.Sprintf(`{"type":"user.message","id":"%s-prompt","content":[{"type":"text","text":"%s prompt"}]}`, p, p),
		fmt.Sprintf(`{"type":"span.model_request_end","id":"%s-model","model_usage":{"input_tokens":10,"output_tokens":2}}`, p),
	}
}

func maEndTurn(p string) string {
	return fmt.Sprintf(`{"type":"session.status_idle","id":"%s-done","stop_reason":{"type":"end_turn"}}`, p)
}

func TestManagedAgentsBlockingThreadStatusLeavesThePromptToTheEvaluator(t *testing.T) {
	for _, kind := range []string{"session.thread_status_running", "session.thread_status_rescheduled"} {
		t.Run(kind, func(t *testing.T) {
			prompts := make(chan string, 4)
			setup(t, rudderstackai.WithLimits(rudderstackai.Limits{}), rudderstackai.WithEvaluator(func(_ context.Context, turn rudderstackai.EvaluationTurn) ([]rudderstackai.Score, error) {
				prompts <- turn.Prompt
				return nil, nil
			}))
			t.Cleanup(func() { _ = rudderstackai.Configure(rudderstackai.WithEvaluator(nil)) })
			resetMA(t)
			maDrain(maBlockingStream(context.Background(), t, append(maRecorded(kind, "one"), maEndTurn("one"))))
			select {
			case got := <-prompts:
				if got != "one prompt" {
					t.Fatalf("prompt: %q", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("evaluator never ran")
			}
		})
	}
}

func TestManagedAgentsBlockingThreadStatusLeavesACancelledCallerTurnBehind(t *testing.T) {
	for _, kind := range []string{"session.thread_status_running", "session.thread_status_rescheduled"} {
		t.Run(kind, func(t *testing.T) {
			_, rec := setup(t, rudderstackai.WithLimits(rudderstackai.Limits{}))
			resetMA(t)
			maBlockingOwned(t, "interaction", func(ctx context.Context) {
				maDrain(maBlockingStream(ctx, t, append(maRecorded(kind, "one"), `{"type":"user.interrupt","id":"one-interrupt"}`, maEndTurn("one"))))
				maDrain(maBlockingStream(ctx, t, append(maRecorded(kind, "two"), maEndTurn("two"))))
			})
			responses := rec.events(t)["ai_response_received"]
			if len(responses) != 1 || responses[0]["response_status"] != "success" || responses[0]["llm_call_count"] != float64(2) {
				t.Fatalf("responses: %v", responses)
			}
		})
	}
}

// A turn that finished cancelled is over: the next turn starts fresh, also when its stream opened after
// its prompt was sent, so it has no prompt echo.
func TestManagedAgentsBlockingFinishedCancelledCallerTurnLeavesTheNextAlone(t *testing.T) {
	for _, statuses := range []bool{true, false} {
		t.Run(fmt.Sprintf("statuses=%v", statuses), func(t *testing.T) {
			_, rec := setup(t, rudderstackai.WithLimits(rudderstackai.Limits{}))
			resetMA(t)
			maBlockingOwned(t, "interaction", func(ctx context.Context) {
				maDrain(maBlockingStream(ctx, t, append(maRecorded("session.thread_status_running", "one"), `{"type":"user.interrupt","id":"one-interrupt"}`, maEndTurn("one"))))
				var second []string
				if statuses {
					second = append(second, maRecorded("session.thread_status_running", "two")[:2]...)
				}
				second = append(second, `{"type":"span.model_request_end","id":"two-model","model_usage":{"input_tokens":10,"output_tokens":2}}`, maEndTurn("two"))
				maDrain(maBlockingStream(ctx, t, second))
			})
			responses := rec.events(t)["ai_response_received"]
			if len(responses) != 1 || responses[0]["response_status"] != "success" || responses[0]["llm_call_count"] != float64(2) {
				t.Fatalf("responses: %v", responses)
			}
		})
	}
}

// A caller that restores its paused interaction from JSON (in this process or another) has none of the
// adapter's own record of its tools; their results still settle them in its interaction.
func TestManagedAgentsBlockingResultsAfterAJSONRestore(t *testing.T) {
	for _, where := range []string{"same process", "another process"} {
		for _, kind := range []string{"custom", "tool", "confirmation", "deny"} {
			t.Run(where+"/"+kind, func(t *testing.T) {
				_, rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithLimits(rudderstackai.Limits{}))
				resetMA(t)
				opening := kind
				if kind == "deny" {
					opening = "confirmation"
				}
				ctx, caller := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{UserID: "u"})
				maDrain(maBlockingStream(ctx, t, maBlockingOpening(opening)))
				snapshot, ok := caller.Pause(rudderstackai.PauseOptions{})
				if !ok {
					t.Fatal("the caller's interaction did not pause")
				}
				raw, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				var restored rudderstackai.PausedInteraction
				if err := json.Unmarshal(raw, &restored); err != nil {
					t.Fatal(err)
				}
				if where == "another process" {
					rudderanthropic.ResetManagedAgents()
				}
				ctx, resumed := rudderstackai.StartInteraction(context.Background(), rudderstackai.InteractionOptions{Resume: &restored})
				answer := strings.TrimSuffix(maBlockingReply(kind, "tool-0"), "}") + `,"is_error":true}`
				switch kind {
				case "deny":
					answer = `{"type":"user.tool_confirmation","id":"deny-0","tool_use_id":"tool-0","result":"deny"}`
				case "confirmation":
					answer = maBlockingReply("confirmation", "tool-0")
				}
				// A result for a tool the interaction never had changes nothing.
				rest := []string{answer, strings.TrimSuffix(maBlockingReply("custom", "tool-9"), "}") + `,"is_error":true}`, `{"type":"session.status_running","id":"running"}`}
				if kind == "confirmation" {
					rest = append(rest, `{"type":"agent.tool_result","id":"ran-0","tool_use_id":"tool-0","is_error":true}`)
				}
				maDrain(maBlockingStream(ctx, t, append(rest, maEndTurn("restored"))))
				resumed.End(rudderstackai.EndOptions{})
				var statuses []string
				var tools []map[string]any
				for _, response := range rec.events(t)["ai_response_received"] {
					statuses = append(statuses, fmt.Sprint(response["response_status"]))
					breakdown, _ := response["tool_breakdown"].([]any)
					for _, e := range breakdown {
						tools = append(tools, e.(map[string]any))
					}
				}
				if !slices.Equal(statuses, []string{"paused", "success"}) || len(tools) != 1 || tools[0]["tool_name"] != "tool_0" ||
					tools[0]["call_count"] != float64(1) || tools[0]["error_count"] != float64(1) {
					t.Fatalf("statuses %v, tools %v", statuses, tools)
				}
			})
		}
	}
}

func TestManagedAgentsBlockingOutstandingReplies(t *testing.T) {
	for _, owner := range []string{"adapter", "interaction"} {
		for _, a := range maReplyKinds {
			for _, b := range maReplyKinds {
				for _, extra := range []string{"duplicate", "unrelated", "second idle", "empty reconnect"} {
					t.Run(owner+"/"+a+"+"+b+"/"+extra, func(t *testing.T) {
						_, rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithLimits(rudderstackai.Limits{}))
						resetMA(t)
						maBlockingOwned(t, owner, func(ctx context.Context) {
							initial := append(maBlockingOpening(a, b), maBlockingReply(a, "tool-0"))
							switch extra {
							case "duplicate":
								initial = append(initial, maBlockingReply(a, "tool-0", "duplicate"))
							case "unrelated":
								initial = append(initial, maBlockingReply(a, "unknown"))
							case "second idle":
								initial = append(initial, maWholeIdle("requires_action", "tool-2"))
							}
							events, state := maBlockingRead(ctx, t, initial, "clean")
							if extra == "empty reconnect" {
								events, state = maBlockingRead(ctx, t, []string{maBlockingReply(a, "unknown")}, "clean")
							}
							maBlockingCarried(t, events, owner)
							keys := []string{"tool-1"}
							if extra == "second idle" {
								keys = append(keys, "tool-2")
							}
							maBlockingWait(t, state, maBlockingReason(b), keys...)
							last := "tool-1"
							if extra == "second idle" {
								events, state = maBlockingRead(ctx, t, []string{maBlockingReply(b, "tool-1")}, "clean")
								maBlockingCarried(t, events, owner)
								maBlockingWait(t, state, "other", "tool-2")
								last = "tool-2"
							}
							maDrain(maBlockingStream(ctx, t, []string{maBlockingReply(b, last), maWholeIdle("end_turn")}))
						})
						maBlockingCountedOnce(t, rec, 2)
					})
				}
			}
		}
	}
}

func maBlockingChild(raw []string) []string {
	raw[2] = strings.TrimSuffix(raw[2], "}") + `,"session_thread_id":"child"}`
	return raw
}

func TestManagedAgentsBlockingChildReplies(t *testing.T) {
	for _, owner := range []string{"adapter", "interaction"} {
		for _, a := range maReplyKinds {
			for _, b := range maReplyKinds {
				t.Run(owner+"/"+a+"+"+b, func(t *testing.T) {
					_, rec := setup(t, rudderstackai.WithToolBreakdown(true), rudderstackai.WithLimits(rudderstackai.Limits{}))
					resetMA(t)
					maBlockingOwned(t, owner, func(ctx context.Context) {
						events, state := maBlockingRead(ctx, t, append(maBlockingChild(maBlockingOpening(a, b)), maBlockingReply(a, "tool-0")), "clean")
						maBlockingCarried(t, events, owner)
						maBlockingWait(t, state, maBlockingReason(b), "tool-1")
						maDrain(maBlockingStream(ctx, t, []string{maBlockingReply(b, "tool-1"), maWholeIdle("end_turn")}))
					})
					maBlockingCountedOnce(t, rec, 1)
				})
			}
		}
	}
}

func TestManagedAgentsBlockingAllRepliesClearWaiting(t *testing.T) {
	for _, a := range maReplyKinds {
		for _, b := range maReplyKinds {
			t.Run(a+"+"+b, func(t *testing.T) {
				_, rec := setup(t, rudderstackai.WithLimits(rudderstackai.Limits{}))
				resetMA(t)
				raw := append(maBlockingOpening(a, b), maBlockingReply(b, "tool-1"), maBlockingReply(a, "tool-0"))
				events := maBlockingStream(context.Background(), t, raw)
				for range len(raw) - 1 {
					if !events.Next() {
						t.Fatal("early EOF")
					}
				}
				maBlockingWait(t, maPreviewState(t, events), maBlockingReason(a), "tool-0")
				if !events.Next() {
					t.Fatal("missing final reply")
				}
				maBlockingWait(t, maPreviewState(t, events), "")
				_ = events.Close()
				if responses := rec.events(t)["ai_response_received"]; len(responses) != 1 || responses[0]["response_status"] != "cancelled" {
					t.Fatalf("responses: %v", responses)
				}
				_, paused, _ := rudderanthropic.InspectManagedAgents(events, "blocking-regression")
				if paused {
					t.Fatal("fully answered turn paused")
				}
			})
		}
	}
}

func maBlockingStatuses(t *testing.T, rec *recorder, want ...string) {
	t.Helper()
	responses := rec.events(t)["ai_response_received"]
	var got []string
	ids := map[any]bool{}
	for _, response := range responses {
		got = append(got, fmt.Sprint(response["response_status"]))
		ids[response["interaction_id"]] = true
	}
	if !slices.Equal(got, want) || len(ids) != 1 {
		t.Fatalf("responses: %v", responses)
	}
}

func TestManagedAgentsBlockingResumptionOnReconnect(t *testing.T) {
	for _, kind := range []string{"session.status_running", "session.status_rescheduled", "session.thread_status_running", "session.thread_status_rescheduled", "agent.thinking", "span.model_request_start", "event_start"} {
		t.Run(kind, func(t *testing.T) {
			_, rec := setup(t, rudderstackai.WithLimits(rudderstackai.Limits{}))
			resetMA(t)
			maBlockingRead(context.Background(), t, append(maBlockingOpening("custom", "custom"), maBlockingReply("custom", "tool-0")), "clean")
			responses := rec.events(t)["ai_response_received"]
			if len(responses) != 1 || responses[0]["response_status"] != "paused" {
				t.Fatalf("responses: %v", responses)
			}
			raw := []string{maWholeEvent(kind)}
			if kind == "event_start" {
				raw[0] = `{"type":"event_start","event":{"id":"preview","type":"agent.message"}}`
			}
			if strings.HasPrefix(kind, "session.thread_") {
				raw[0] = fmt.Sprintf(`{"type":%q,"session_thread_id":"sthr_main"}`, kind)
			}
			// Running may defer opening to the next event.
			raw = append(raw, maBlockingReply("custom", "unknown"))
			events := maBlockingStream(context.Background(), t, raw)
			for range raw {
				if !events.Next() {
					t.Fatal("early EOF")
				}
			}
			maBlockingWait(t, maPreviewState(t, events), "")
			_ = events.Close()
			maBlockingStatuses(t, rec, "paused", "cancelled")
		})
	}
}

func TestManagedAgentsBlockingChildReplyClock(t *testing.T) {
	for _, kind := range maReplyKinds {
		t.Run(kind, func(t *testing.T) {
			_, rec := setup(t, rudderstackai.WithLimits(rudderstackai.Limits{}))
			resetMA(t)
			raw := maBlockingChild(maBlockingOpening(kind))
			for n := range raw {
				raw[n] = strings.TrimSuffix(raw[n], "}") + "," + maAt(0) + "}"
			}
			raw = append(raw, strings.TrimSuffix(maBlockingReply("custom", "unknown"), "}")+","+maAt(500)+"}", strings.TrimSuffix(maBlockingReply(kind, "tool-0"), "}")+","+maAt(2500)+"}")
			events := maBlockingStream(context.Background(), t, raw)
			for range raw {
				if !events.Next() {
					t.Fatal("early EOF")
				}
			}
			tracker := reflect.ValueOf(events).Elem().FieldByName("tracker").Elem()
			lastAt := tracker.FieldByName("turn").Elem().FieldByName("lastAt")
			if !lastAt.FieldByName("ok").Bool() || lastAt.FieldByName("v").Int() != time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC).UnixMilli()+2500 {
				t.Fatal("child reply did not advance turn clock")
			}
			_ = events.Close()
			response := rec.events(t)["ai_response_received"][0]
			if latency, _ := response["latency_ms"].(float64); latency < 2490 || latency > 2510 || response["tool_call_count"] != float64(0) {
				t.Fatalf("response: %v", response)
			}
		})
	}
}

func TestManagedAgentsBlockingNonResumptionPreservesCalls(t *testing.T) {
	for _, kind := range []string{"user.interrupt", "system.message", "span.model_request_end", "span.outcome_evaluation_start", "span.outcome_evaluation_end", "span.outcome_evaluation_ongoing"} {
		t.Run(kind, func(t *testing.T) {
			setup(t, rudderstackai.WithLimits(rudderstackai.Limits{}))
			resetMA(t)
			raw := append(maBlockingOpening("custom", "custom"), maWholeEvent(kind))
			events := maBlockingStream(context.Background(), t, raw)
			for range raw {
				if !events.Next() {
					t.Fatal("early EOF")
				}
			}
			maBlockingWait(t, maPreviewState(t, events), "deferred_tool", "tool-0", "tool-1")
			_ = events.Close()
		})
	}
}

func TestManagedAgentsBlockingChildResumption(t *testing.T) {
	for _, kind := range maReplyKinds {
		for _, evidence := range []string{"agent.tool_use", "agent.thinking", "span.model_request_start", "event_start", "session.thread_status_running", "session.thread_status_rescheduled"} {
			t.Run(kind+"/"+evidence, func(t *testing.T) {
				_, rec := setup(t, rudderstackai.WithLimits(rudderstackai.Limits{}))
				resetMA(t)
				maBlockingRead(context.Background(), t, maBlockingOpening("custom", "custom"), "clean")
				childType, permission := "agent.tool_use", "allow"
				if kind == "custom" {
					childType = "agent.custom_tool_use"
				}
				if kind == "confirmation" {
					permission = "ask"
				}
				resumption := fmt.Sprintf(`{"type":%q,"id":"child-evidence","session_thread_id":"child","name":"evidence_tool"}`, evidence)
				if evidence == "event_start" {
					resumption = `{"type":"event_start","event":{"type":"agent.message","id":"preview-child","session_thread_id":"child"}}`
				}
				raw := []string{maBlockingReply("custom", "tool-0"), `{"type":"session.thread_created","session_thread_id":"child"}`, resumption,
					fmt.Sprintf(`{"type":%q,"id":"child-call","session_thread_id":"child","name":"child_tool","evaluated_permission":%q}`, childType, permission),
					maWholeIdle("requires_action", "child-call"), maBlockingReply(kind, "child-call")}
				events := maBlockingStream(context.Background(), t, raw)
				for range 3 {
					if !events.Next() {
						t.Fatal("early EOF")
					}
				}
				maBlockingWait(t, maPreviewState(t, events), "")
				for range 3 {
					if !events.Next() {
						t.Fatal("early EOF")
					}
				}
				maBlockingWait(t, maPreviewState(t, events), "")
				_ = events.Close()
				maBlockingStatuses(t, rec, "paused", "cancelled")
				var tools float64
				for _, response := range rec.events(t)["ai_response_received"] {
					value, _ := response["tool_call_count"].(float64)
					tools += value
				}
				if tools != 2 {
					t.Fatalf("tool count: %v", tools)
				}
			})
		}
	}
}

func TestManagedAgentsBlockingOutstandingBounds(t *testing.T) {
	setup(t, rudderstackai.WithLimits(rudderstackai.Limits{MaxMapEntries: 2}))
	resetMA(t)
	raw := append(maBlockingOpening("custom", "custom", "custom"), maBlockingReply("custom", "tool-0"), maBlockingReply("custom", "tool-1"), maBlockingReply("custom", "tool-2"))
	events := maBlockingStream(context.Background(), t, raw)
	for range len(raw) - 3 {
		if !events.Next() {
			t.Fatal("early EOF")
		}
	}
	maBlockingWait(t, maPreviewState(t, events), "deferred_tool", "tool-1", "tool-2")
	if !events.Next() {
		t.Fatal("missing evicted reply")
	}
	maBlockingWait(t, maPreviewState(t, events), "deferred_tool", "tool-1", "tool-2")
	for range 2 {
		if !events.Next() {
			t.Fatal("missing reply")
		}
	}
	maBlockingWait(t, maPreviewState(t, events), "")
	_ = events.Close()
}

func TestManagedAgentsBlockingFinalEndClearsCalls(t *testing.T) {
	for _, kind := range []string{"session.deleted", "session.status_terminated", "session.status_idle"} {
		t.Run(kind, func(t *testing.T) {
			setup(t, rudderstackai.WithLimits(rudderstackai.Limits{}))
			resetMA(t)
			raw := append(maBlockingOpening("custom"), fmt.Sprintf(`{"type":%q,"stop_reason":{"type":"end_turn"}}`, kind))
			events := maBlockingStream(context.Background(), t, raw)
			for range len(raw) - 1 {
				if !events.Next() {
					t.Fatal("early EOF")
				}
			}
			state := maPreviewState(t, events)
			if !events.Next() {
				t.Fatal("missing ending")
			}
			maBlockingWait(t, state, "")
		})
	}
}

// Extend the whole-state map test with the outstanding map's insertion, update and deletion order.
func maCheckOutstandingBounds(t *testing.T) {
	t.Helper()
	raw := []string{maWholeEvent("user.message"), maWholeIdle("requires_action", "oldest", "middle", "newest"), maWholeIdle("requires_action", "middle"), maBlockingReply("custom", "middle"), maWholeIdle("requires_action", "middle"), maWholeIdle("requires_action", "last"), maWholeEvent("session.status_running")}
	events := maBlockingStream(context.Background(), t, raw)
	for range 2 {
		if !events.Next() {
			t.Fatal("early EOF")
		}
	}
	maBlockingWait(t, maPreviewState(t, events), "other", "middle", "newest")
	for range 4 {
		if !events.Next() {
			t.Fatal("early EOF")
		}
	}
	maBlockingWait(t, maPreviewState(t, events), "other", "middle", "last")
	if !events.Next() {
		t.Fatal("missing resumption")
	}
	maBlockingWait(t, maPreviewState(t, events), "")
	_ = events.Close()
}
