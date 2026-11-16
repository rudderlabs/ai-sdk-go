package rudderstackai

// SDK-6111: overflow call events behind a provisional call, dated-model twins, and writes during paused delivery.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

func sdk6111Window(call LLMCall, startMs int64) LLMCall {
	call.StartedAt, call.EndedAt = time.UnixMilli(startMs), time.UnixMilli(startMs+10)
	return call
}

func sdk6111Num(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case float64:
		return n
	}
	return 0
}

func TestSDK6111OverflowReportsBehindAProvisionalCallKeepTheFirstReportFlags(t *testing.T) {
	for _, field := range []string{"cost_usd", "credits", "server_tool_cost_usd"} {
		t.Run(field, func(t *testing.T) {
			model := "unpriced-model"
			opts := []ConfigOption{WithLLMCallEvents(true), WithLimits(Limits{MaxCallsPerInteraction: 1})}
			if field == "server_tool_cost_usd" {
				model = "fee-model"
				opts = append(opts, WithPrices(map[string]ModelPrice{model: {TokenPrice: TokenPrice{InputPerMtok: 1, OutputPerMtok: 1}, ServerToolPerCall: map[string]float64{"web_search": 0.01}}}))
			}
			r := setupAI(t, opts...)
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			// A timed span holds call events; priced and credited, so the interaction totals depend on the overflow group alone.
			span := sdk6111Window(LLMCall{Provider: "openai", Model: model, Integration: "tracing", InputTokens: 10, CostUSD: Ptr(0.001), Credits: Ptr(1.0)}, 100)
			i.RecordCall(span)
			// Unpriced, uncredited or without fees, then priced, credited or with fees, in one breakdown group past the cap.
			first := sdk6111Window(LLMCall{Provider: "openai", Model: model, Integration: "openai", InputTokens: 20}, 200)
			second := sdk6111Window(LLMCall{Provider: "openai", Model: model, Integration: "openai", InputTokens: 30}, 300)
			switch field {
			case "cost_usd":
				second.CostUSD = Ptr(0.004)
			case "credits":
				second.Credits = Ptr(7.0)
			default:
				// A kind without a fee leaves the first call's fees unknown.
				first.ServerToolCalls = map[string]int{"code_execution": 1}
				second.ServerToolCalls = map[string]int{"web_search": 1}
			}
			i.RecordCall(first)
			i.RecordCall(second)
			if len(r.named("ai_llm_response_received")) != 0 {
				t.Fatal("overflow call events were not held behind the span")
			}
			i.End(EndOptions{})
			found := 0
			for _, e := range r.named("ai_llm_response_received") {
				if e.Properties["input_tokens"] != 50 {
					continue
				}
				found++
				if v, ok := e.Properties[field]; ok {
					t.Errorf("overflow call event %s = %v, want absent", field, v)
				}
			}
			if found != 1 {
				t.Fatalf("overflow call events = %d, want 1", found)
			}
			resp := lastResponse(t, r)
			if field == "server_tool_cost_usd" {
				for _, group := range resp["usage_breakdown"].([]map[string]any) {
					if v, ok := group[field]; ok {
						t.Fatalf("usage_breakdown %s = %v, want absent", field, v)
					}
				}
				return
			}
			total := map[string]string{"cost_usd": "total_cost_usd", "credits": "total_credits"}[field]
			if v, ok := resp[total]; ok {
				t.Fatalf("response %s = %v, want absent", total, v)
			}
		})
	}
}

func TestSDK6111DatedModelSpanMatchesWrapper(t *testing.T) {
	wrapper := LLMCall{
		Provider: "openai", Model: "gpt-4o", Integration: IntegrationOpenAI, ResponseID: "resp_1",
		InputTokens: 10, OutputTokens: 2, StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110),
	}
	// The same call's span, without a response ID and with the dated model ID.
	dated := wrapper
	dated.Integration, dated.ResponseID, dated.Model = "tracing", "", "gpt-4o-2024-08-06"
	for _, spanFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("spanFirst=%t", spanFirst), func(t *testing.T) {
			r := setupAI(t)
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			reports := []LLMCall{wrapper, dated}
			if spanFirst {
				reports[0], reports[1] = reports[1], reports[0]
			}
			for _, c := range reports {
				i.RecordCall(c)
			}
			i.End(EndOptions{})
			resp := lastResponse(t, r)
			if sdk6111Num(resp["llm_call_count"]) != 1 || sdk6111Num(resp["total_input_tokens"]) != 10 {
				t.Fatalf("llm_call_count=%v total_input_tokens=%v, want 1 and 10", resp["llm_call_count"], resp["total_input_tokens"])
			}
		})
	}
}

// Claude IDs stay exact: a different Claude model is a different call.
func TestSDK6111SameModel(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"gpt-4o", "gpt-4o-2024-08-06", true},
		{"GPT-4o-2024-08-06", "gpt-4o", true},
		{"gpt-4o", "", true},
		{"gpt-4o", "gpt-4.1", false},
		{"gpt-4o", "gpt-4o-mini", false},
		{"claude-sonnet-4-5", "claude-sonnet-4-5-20250929", true},
		{"claude-sonnet-4", "claude-sonnet-4-5", false},
	} {
		if got := sameModel(tc.a, tc.b); got != tc.want {
			t.Errorf("sameModel(%q, %q) = %t, want %t", tc.a, tc.b, got, tc.want)
		}
	}
}

// As in Node and Python, a folded group reads as successful once any of its calls succeeded.
func TestSDK6111OverflowGroupWithASuccessfulCallSendsNoErrorType(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithLimits(Limits{MaxCallsPerInteraction: 1}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	// A timed span holds call events, so both overflow calls fold into one event.
	i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", InputTokens: 10}, 100))
	i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "openai", InputTokens: 20, ErrorType: "timeout"}, 200))
	i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "openai", InputTokens: 30}, 300))
	i.End(EndOptions{})
	found := 0
	for _, e := range r.named("ai_llm_response_received") {
		if e.Properties["input_tokens"] != 50 {
			continue
		}
		found++
		if v, ok := e.Properties["error_type"]; ok {
			t.Errorf("overflow call event error_type = %v, want absent", v)
		}
	}
	if found != 1 {
		t.Fatalf("overflow call events = %d, want 1", found)
	}
}

// sdk6111ReenterClient records once from inside delivery of the first matching event, as an app's
// synchronous transport callback can.
type sdk6111ReenterClient struct {
	*recorder
	event   string
	done    bool
	reenter func()
}

func (c *sdk6111ReenterClient) Enqueue(m analytics.Message) error {
	err := c.recorder.Enqueue(m)
	if e, ok := m.(analytics.Track); ok && !c.done && e.Event == c.event {
		c.done = true
		c.reenter()
	}
	return err
}

func TestSDK6111ToolRecordedDuringPausedCallDeliveryIsCountedOnceWithItsEvent(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "Looked Up"}}}))
	client := &sdk6111ReenterClient{recorder: r, event: "ai_llm_response_received"}
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
	// A timed span's call event waits until pause delivers it.
	i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", InputTokens: 10}, 100))
	client.reenter = func() { i.RecordToolCalls(ToolCall{ID: "call_1", Name: "lookup"}) }
	if _, ok := i.Pause(PauseOptions{}); !ok {
		t.Fatal("pause")
	}
	if !client.done {
		t.Fatal("call event was not delivered during pause")
	}
	i.Resume()
	i.End(EndOptions{})
	segments := r.named("ai_response_received")
	if len(segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(segments))
	}
	// The paused segment counts it, as in Node and Python.
	if paused, final := sdk6111Num(segments[0].Properties["tool_call_count"]), sdk6111Num(segments[1].Properties["tool_call_count"]); paused != 1 || final != 0 {
		t.Fatalf("tool_call_count paused=%g final=%g, want 1 and 0", paused, final)
	}
	if n := len(r.named("Looked Up")); n != 1 {
		t.Fatalf("declared tool events = %d, want 1", n)
	}
}

// Another goroutine records in each window where pause runs unlocked: the call or tool is counted
// once and its events are delivered, or carried by the snapshot.
func TestSDK6111WriteFromAnotherGoroutineDuringPauseIsDeliveredOrCarried(t *testing.T) {
	for _, emit := range []bool{true, false} {
		for _, window := range []string{"ai_llm_response_received", "Looked Up", "ai_response_received"} {
			for _, kind := range []string{"span", "wrapper and tool"} {
				for _, restore := range []bool{false, true} {
					if !emit && window == "ai_llm_response_received" {
						continue
					}
					t.Run(fmt.Sprintf("emit=%v/during %s/%s/json=%v", emit, window, kind, restore), func(t *testing.T) {
						r := setupAI(t, WithLLMCallEvents(emit), WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "Looked Up"}}}))
						client := &sdk6111ReenterClient{recorder: r, event: window}
						_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
						i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", InputTokens: 10}, 100))
						i.RecordToolCalls(ToolCall{ID: "first", Name: "lookup"})
						wantTools := 1.0
						client.reenter = func() {
							done := make(chan struct{})
							go func() {
								defer close(done)
								call := sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "openai", InputTokens: 20}, 300)
								if kind == "span" {
									call.Integration = "tracing"
								}
								i.RecordCall(call)
								if kind != "span" {
									i.RecordToolCalls(ToolCall{ID: "late", Name: "lookup", Result: "ok"})
								}
							}()
							<-done
						}
						if kind != "span" {
							wantTools = 2
						}
						snapshot, ok := i.Pause(PauseOptions{})
						if !ok || !client.done {
							t.Fatalf("pause ok=%v, raced=%v", ok, client.done)
						}
						if restore {
							var stored PausedInteraction
							if err := json.Unmarshal([]byte(mustJSON(snapshot)), &stored); err != nil {
								t.Fatal(err)
							}
							_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &stored, Analytics: client})
						} else {
							i.Resume()
						}
						i.End(EndOptions{})
						var calls, tokens, tools float64
						for _, e := range r.named("ai_response_received") {
							calls += sdk6111Num(e.Properties["llm_call_count"])
							tokens += sdk6111Num(e.Properties["total_input_tokens"])
							tools += sdk6111Num(e.Properties["tool_call_count"])
						}
						if calls != 2 || tokens != 30 || tools != wantTools {
							t.Errorf("calls=%g tokens=%g tools=%g, want 2, 30 and %g", calls, tokens, tools, wantTools)
						}
						if n := len(r.named("ai_llm_response_received")); emit && n != 2 {
							t.Errorf("call events = %d, want 2", n)
						}
						if n := len(r.named("Looked Up")); float64(n) != wantTools {
							t.Errorf("declared tool events = %d, want %g", n, wantTools)
						}
					})
				}
			}
		}
	}
}

// A credits hook priced with its twin's call count or usage, when a pause during the hook closes
// the twin's segment: the report is then a new call and is priced alone.
func TestSDK6111CreditsHookThatPausesPricesTheReportAlone(t *testing.T) {
	for _, order := range []string{"span first", "wrapper first"} {
		for _, pauser := range []string{"hook", "another goroutine"} {
			t.Run(order+"/"+pauser, func(t *testing.T) {
				var i *Interaction
				var hookCalls []int
				pauseOn := 1
				if order == "wrapper first" {
					pauseOn = 2
				}
				r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
					hookCalls = append(hookCalls, c.CallCount)
					// Only the report that follows its twin pauses.
					if len(hookCalls) == pauseOn {
						if pauser == "hook" {
							i.Pause(PauseOptions{})
						} else {
							done := make(chan struct{})
							go func() { defer close(done); i.Pause(PauseOptions{}) }()
							<-done
						}
					}
					return Ptr(float64(c.CallCount)), nil
				}))
				_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
				// One aggregate of three calls with unknown usage, and its twin for one call with usage.
				aggregate := sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4o-2024-08-06", Integration: "tracing", UsageUnknown: true, CallCount: 3}, 100)
				wrapper := sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4o", Integration: "openai", InputTokens: 10}, 100)
				reports, want := []LLMCall{aggregate, wrapper}, [][3]any{{3.0, nil, 0.0}, {1.0, 1.0, 10.0}}
				if order == "wrapper first" {
					reports, want = []LLMCall{wrapper, aggregate}, [][3]any{{1.0, 1.0, 10.0}, {3.0, nil, 0.0}}
				}
				for _, call := range reports {
					if !i.RecordCall(call) {
						t.Fatal("report was not recorded")
					}
				}
				i.End(EndOptions{})
				segments, calls := r.named("ai_response_received"), r.named("ai_llm_response_received")
				if len(segments) != 2 || len(calls) != 2 {
					t.Fatalf("segments = %d, call events = %d, want 2 and 2", len(segments), len(calls))
				}
				for n, w := range want {
					// The pause closed the twin's segment, so the later report is a call of its own count.
					segment := pauseSegmentsJSON(t, segments[n].Properties).(map[string]any)
					if segment["llm_call_count"] != w[0] || segment["total_credits"] != w[1] {
						t.Errorf("segment %d: llm_call_count=%v total_credits=%v, want %v and %v", n, segment["llm_call_count"], segment["total_credits"], w[0], w[1])
					}
					event := pauseSegmentsJSON(t, calls[n].Properties).(map[string]any)
					if event["input_tokens"] != w[2] || event["credits"] != w[1] {
						t.Errorf("call event %d: input_tokens=%v credits=%v, want %v and %v", n, event["input_tokens"], event["credits"], w[2], w[1])
					}
				}
				wantHook := []int{3, 1}
				if order == "wrapper first" {
					wantHook = []int{1, 3}
				}
				if fmt.Sprint(hookCalls) != fmt.Sprint(wantHook) {
					t.Errorf("hook call counts = %v, want %v", hookCalls, wantHook)
				}
			})
		}
	}
}

// A repeat of a counted response from the same source stays a repeat when its credits hook
// pauses: only the other source's twin is admitted as a new call, as in Node and Python.
func TestSDK6111SameSourceRepeatWhoseCreditsHookPausesIsDropped(t *testing.T) {
	for _, source := range []string{"openai", "tracing"} {
		for _, pauser := range []string{"hook", "another goroutine"} {
			t.Run(source+"/"+pauser, func(t *testing.T) {
				var i *Interaction
				hookCalls := 0
				r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(CreditCall) (*float64, error) {
					hookCalls++
					if hookCalls == 2 {
						if pauser == "hook" {
							i.Pause(PauseOptions{})
						} else {
							done := make(chan struct{})
							go func() { defer close(done); i.Pause(PauseOptions{}) }()
							<-done
						}
					}
					// New agent metadata makes the repeat eligible for repricing.
					return nil, nil
				}))
				_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
				call := LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: source, ResponseID: "resp_1", InputTokens: 10}
				if !i.RecordCall(call) {
					t.Fatal("first report was not recorded")
				}
				call.AgentName = "planner"
				if i.RecordCall(call) {
					t.Error("the repeat was recorded as a new call")
				}
				if hookCalls != 2 {
					t.Fatalf("hook calls = %d, want 2", hookCalls)
				}
				i.End(EndOptions{})
				var calls, tokens float64
				for _, e := range r.named("ai_response_received") {
					calls += sdk6111Num(e.Properties["llm_call_count"])
					tokens += sdk6111Num(e.Properties["total_input_tokens"])
				}
				if calls != 1 || tokens != 10 {
					t.Errorf("llm_call_count=%g total_input_tokens=%g across segments, want 1 and 10", calls, tokens)
				}
				if n := len(r.named("ai_llm_response_received")); n != 1 {
					t.Errorf("call events = %d, want 1", n)
				}
			})
		}
	}
}

// A span twin for the same single call never prices over a wrapper's own credits, as in Node.
func TestSDK6111SpanTwinOfAWrapperWithExplicitCreditsDoesNotRunTheCreditsHook(t *testing.T) {
	for _, mode := range []string{"same segment", "pending in-process", "pending json"} {
		for _, responseID := range []string{"", "resp_1"} {
			t.Run(mode+"/responseID="+responseID, func(t *testing.T) {
				var hookCalls []int
				r := setupAI(t, WithCredits(func(c CreditCall) (*float64, error) {
					hookCalls = append(hookCalls, c.InputTokens)
					return Ptr(1.0), nil
				}))
				client := &sdk6111ReenterClient{recorder: r, event: "ai_response_received", done: mode == "same segment"}
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
				wrapper := func() {
					if !i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "openai", InputTokens: 10, ResponseID: responseID, Credits: Ptr(7.0)}, 100)) {
						t.Error("wrapper was not recorded")
					}
				}
				if mode == "same segment" {
					wrapper()
				} else {
					// Accepted during paused delivery: pending for the next segment.
					client.reenter = wrapper
					snapshot, ok := i.Pause(PauseOptions{})
					if !ok || !client.done {
						t.Fatalf("pause ok=%v, delivered=%v", ok, client.done)
					}
					if mode == "pending json" {
						var stored PausedInteraction
						if err := json.Unmarshal([]byte(mustJSON(snapshot)), &stored); err != nil {
							t.Fatal(err)
						}
						_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &stored, Analytics: client})
					} else {
						i.Resume()
					}
				}
				// The span stands for the same single call: the wrapper's own credits already cover it.
				i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4.1", Integration: "tracing", InputTokens: 3, ResponseID: responseID}, 100))
				i.End(EndOptions{})
				segments := r.named("ai_response_received")
				last := segments[len(segments)-1].Properties
				if calls, tokens, credits := sdk6111Num(last["llm_call_count"]), sdk6111Num(last["total_input_tokens"]), sdk6111Num(last["total_credits"]); calls != 1 || tokens != 10 || credits != 7 {
					t.Errorf("llm_call_count=%g total_input_tokens=%g total_credits=%g, want 1, 10 and 7", calls, tokens, credits)
				}
				if len(hookCalls) != 0 {
					t.Errorf("credits hook ran for input tokens %v, want no call", hookCalls)
				}
			})
		}
	}
}

// The call event of calls folded past the cap behind a held span is the same after a JSON restore.
func TestSDK6111FoldedCallEventRestoredFromJSONEqualsTheOneSentInProcess(t *testing.T) {
	run := func(t *testing.T, restore bool) ([]any, map[string]any) {
		r := setupAI(t, WithLLMCallEvents(true), WithLimits(Limits{MaxCallsPerInteraction: 1}))
		client := &sdk6111ReenterClient{recorder: r, event: "ai_response_received"}
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", InteractionID: "run", ConversationID: "thread", Analytics: client})
		wrapper := LLMCall{Provider: "openai", Model: "unpriced-model", Integration: "openai"}
		client.reenter = func() {
			// A held span, then two calls past the cap in one group: only the first is priced and credited.
			i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "unpriced-model", Integration: "tracing", InputTokens: 10}, 100))
			first, second := wrapper, wrapper
			first.InputTokens, first.CostUSD, first.Credits = 20, Ptr(0.002), Ptr(2.0)
			second.InputTokens = 30
			i.RecordCall(first)
			i.RecordCall(second)
		}
		snapshot, ok := i.Pause(PauseOptions{})
		if !ok || !client.done {
			t.Fatalf("pause ok=%v, delivered=%v", ok, client.done)
		}
		if restore {
			var stored PausedInteraction
			if err := json.Unmarshal([]byte(mustJSON(snapshot)), &stored); err != nil {
				t.Fatal(err)
			}
			_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &stored, Analytics: client})
		}
		// Folded after the restore into the same unsent event.
		late := wrapper
		late.InputTokens, late.CostUSD, late.Credits = 40, Ptr(0.001), Ptr(1.0)
		i.RecordCall(late)
		i.End(EndOptions{})
		calls := []any{}
		for _, e := range r.named("ai_llm_response_received") {
			calls = append(calls, pauseSegmentsJSON(t, e.Properties))
		}
		segments := r.named("ai_response_received")
		return calls, pauseSegmentsJSON(t, segments[len(segments)-1].Properties).(map[string]any)
	}
	inProcess, last := run(t, false)
	if len(inProcess) != 2 {
		t.Fatalf("call events = %v, want the span's and the folded group's", inProcess)
	}
	folded := inProcess[1].(map[string]any)
	if folded["input_tokens"] != 90.0 || folded["call_index"] != 2.0 || folded["cost_usd"] != nil || folded["credits"] != nil {
		t.Errorf("folded call event = %v, want 90 input tokens at call_index 2 without cost_usd or credits", folded)
	}
	if last["llm_call_count"] != 4.0 || last["total_input_tokens"] != 100.0 {
		t.Errorf("in process: llm_call_count=%v total_input_tokens=%v, want 4 and 100", last["llm_call_count"], last["total_input_tokens"])
	}
	restored, last := run(t, true)
	if !reflect.DeepEqual(restored, inProcess) {
		t.Errorf("restored call events = %v, want %v", restored, inProcess)
	}
	if last["llm_call_count"] != 4.0 || last["total_input_tokens"] != 100.0 {
		t.Errorf("restored: llm_call_count=%v total_input_tokens=%v, want 4 and 100", last["llm_call_count"], last["total_input_tokens"])
	}
}

type sdk6111CrossSDK struct {
	Snapshots []struct {
		WrittenBy string          `json:"written_by"`
		Snapshot  json.RawMessage `json:"snapshot"`
	} `json:"snapshots"`
	Reports []struct {
		Source       string `json:"source"`
		Provider     string `json:"provider"`
		Model        string `json:"model"`
		InputTokens  int    `json:"input_tokens"`
		OutputTokens int    `json:"output_tokens"`
		StartedMS    int64  `json:"started_ms"`
		EndedMS      int64  `json:"ended_ms"`
	} `json:"reports"`
	Expected struct {
		Response   map[string]any   `json:"response"`
		CallEvents []map[string]any `json:"call_events"`
	} `json:"expected"`
}

func sdk6111CrossSDKFixture(t *testing.T) sdk6111CrossSDK {
	t.Helper()
	raw, err := os.ReadFile("spec/fixtures/pause-pending-usage-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		CrossSDK sdk6111CrossSDK `json:"cross_sdk"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture.CrossSDK
}

// Shared fixture: pending calls as each SDK writes them restore here and merge their twins.
func TestSDK6111RestoresThePendingCallsEachSDKWroteAndMergesTheirTwins(t *testing.T) {
	fixture := sdk6111CrossSDKFixture(t)
	for _, entry := range fixture.Snapshots {
		t.Run(entry.WrittenBy, func(t *testing.T) {
			r := setupAI(t, WithLLMCallEvents(true))
			var stored PausedInteraction
			if err := json.Unmarshal(entry.Snapshot, &stored); err != nil {
				t.Fatal(err)
			}
			_, i := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
			for _, report := range fixture.Reports {
				call := LLMCall{
					Provider: report.Provider, Model: report.Model, Integration: "openai", InputTokens: report.InputTokens, OutputTokens: report.OutputTokens,
					StartedAt: time.UnixMilli(report.StartedMS), EndedAt: time.UnixMilli(report.EndedMS),
				}
				if report.Source == "span" {
					call.Integration = "tracing"
				}
				i.RecordCall(call)
			}
			i.End(EndOptions{})
			segments := r.named("ai_response_received")
			last := pauseSegmentsJSON(t, segments[len(segments)-1].Properties).(map[string]any)
			for key, want := range fixture.Expected.Response {
				if !reflect.DeepEqual(last[key], want) {
					t.Errorf("response %s = %v, want %v", key, last[key], want)
				}
			}
			calls := r.named("ai_llm_response_received")
			if len(calls) != len(fixture.Expected.CallEvents) {
				t.Fatalf("call events = %d, want %d", len(calls), len(fixture.Expected.CallEvents))
			}
			for n, want := range fixture.Expected.CallEvents {
				got := pauseSegmentsJSON(t, calls[n].Properties).(map[string]any)
				for key, value := range want {
					if !reflect.DeepEqual(got[key], value) {
						t.Errorf("call event %d %s = %v, want %v", n, key, got[key], value)
					}
				}
			}
		})
	}
}

func TestSDK6111WritesTheSharedPendingCallKeys(t *testing.T) {
	fixture := sdk6111CrossSDKFixture(t)
	r := setupAI(t, WithLLMCallEvents(true))
	client := &sdk6111ReenterClient{recorder: r, event: "ai_response_received"}
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
	client.reenter = func() {
		// Accepted during paused delivery: a timed span, and a timed wrapper call with unknown usage.
		i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "example-model", Integration: "tracing", InputTokens: 3}, 100))
		i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "example-model", Integration: "openai", UsageUnknown: true}, 200))
	}
	snapshot, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("pause")
	}
	written := pauseSegmentsJSON(t, snapshot.PendingCalls)
	records := map[string][]any{}
	for _, entry := range fixture.Snapshots {
		var s struct {
			PendingCalls []any `json:"pending_calls"`
		}
		if err := json.Unmarshal(entry.Snapshot, &s); err != nil {
			t.Fatal(err)
		}
		records[entry.WrittenBy] = s.PendingCalls
	}
	if !reflect.DeepEqual(written, any(records["go"])) {
		t.Fatalf("pending_calls = %v, want the fixture's go entry %v", written, records["go"])
	}
	// Every key Node.js writes has the same value here; raw_model and report_sequence are this SDK's own.
	for n, node := range records["node"] {
		record := records["go"][n].(map[string]any)
		for key, want := range node.(map[string]any) {
			if !reflect.DeepEqual(record[key], want) {
				t.Errorf("record %d %s = %v, want %v as Node.js writes it", n, key, record[key], want)
			}
		}
		for key := range record {
			if _, shared := node.(map[string]any)[key]; !shared && key != "raw_model" && key != "report_sequence" {
				t.Errorf("record %d has %s, which Node.js does not write", n, key)
			}
		}
	}
}

func TestSDK6111GAEnrichingSameSourceRepeatRepricesRetainedUsage(t *testing.T) {
	for _, restore := range []bool{false, true} {
		for _, enrichedFirst := range []bool{false, true} {
			for _, explicit := range []bool{false, true} {
				t.Run(fmt.Sprintf("json=%t/enrichedFirst=%t/explicit=%t", restore, enrichedFirst, explicit), func(t *testing.T) {
					var priced []string
					r := setupAI(t, WithCredits(func(c CreditCall) (*float64, error) {
						priced = append(priced, fmt.Sprintf("%d/%s", c.InputTokens, c.CallPurpose))
						if c.CallPurpose == "answer" {
							return Ptr(7.0), nil
						}
						return Ptr(1.0), nil
					}))
					client := &sdk6111ReenterClient{recorder: r, event: "ai_response_received"}
					_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
					call := sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4o", Integration: "openai", ResponseID: "resp_1", InputTokens: 10, OutputTokens: 2}, 100)
					if enrichedFirst {
						call.CallPurpose = "answer"
					}
					if explicit {
						call.Credits = Ptr(9.0)
					}
					client.reenter = func() { i.RecordCall(call) }
					snapshot, ok := i.Pause(PauseOptions{})
					if !ok {
						t.Fatal("pause")
					}
					if restore {
						raw, err := json.Marshal(snapshot)
						if err != nil {
							t.Fatal(err)
						}
						var stored PausedInteraction
						if err := json.Unmarshal(raw, &stored); err != nil {
							t.Fatal(err)
						}
						_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
					} else {
						i.Resume()
					}
					call.Credits, call.InputTokens = nil, 999
					if enrichedFirst {
						call.CallPurpose = ""
					} else {
						call.CallPurpose = "answer"
					}
					if i.RecordCall(call) {
						t.Fatal("repeat accepted")
					}
					i.End(EndOptions{})
					resp := lastResponse(t, r)
					want := 7.0
					if explicit {
						want = 9
					}
					if sdk6111Num(resp["llm_call_count"]) != 1 || sdk6111Num(resp["total_input_tokens"]) != 10 || sdk6111Num(resp["total_credits"]) != want {
						t.Fatalf("response = %v", resp)
					}
					var wantPriced []string
					if !explicit {
						if enrichedFirst {
							wantPriced = []string{"10/answer"}
						} else {
							wantPriced = []string{"10/", "10/answer"}
						}
					}
					if !reflect.DeepEqual(priced, wantPriced) {
						t.Fatalf("priced = %v, want %v", priced, wantPriced)
					}
				})
			}
		}
	}
}

func TestSDK6111GAEnrichingRepeatPricesLargerReportedCountWithoutReplacingUsage(t *testing.T) {
	var priced [][2]int
	r := setupAI(t, WithCredits(func(call CreditCall) (*float64, error) {
		priced = append(priced, [2]int{call.InputTokens, call.CallCount})
		return Ptr(float64(call.CallCount)), nil
	}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	call := LLMCall{Provider: "openai", Model: "gpt-4o", Integration: "openai", ResponseID: "resp_1", InputTokens: 10}
	i.RecordCall(call)
	call.InputTokens, call.CallCount, call.BYOK = 999, 3, Ptr(false)
	i.RecordCall(call)
	i.End(EndOptions{})
	if !reflect.DeepEqual(priced, [][2]int{{10, 1}, {10, 3}}) {
		t.Fatalf("priced = %v", priced)
	}
	resp := lastResponse(t, r)
	if sdk6111Num(resp["llm_call_count"]) != 1 || sdk6111Num(resp["total_input_tokens"]) != 10 || sdk6111Num(resp["total_credits"]) != 3 {
		t.Fatalf("response = %v", resp)
	}
}
