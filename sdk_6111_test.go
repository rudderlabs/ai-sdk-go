package rudderstackai

// SDK-6111: overflow call events behind a provisional call, dated-model twins, and writes during paused delivery.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

func TestSDK6111CreditsReentrySharesRetryBudget(t *testing.T) {
	var i *Interaction
	armed := false
	attempts := 0
	span := sdk6942Call()
	r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
		if armed {
			attempts++
			if attempts < 6 {
				e := span
				e.BYOK = Ptr(c.BYOK == nil || !*c.BYOK)
				i.RecordCall(e)
			}
		}
		factor := 1.0
		if c.BYOK != nil && *c.BYOK {
			factor = 2
		}
		return Ptr(float64(c.InputTokens) * factor), nil
	}))
	_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(span)
	armed = true
	w := span
	w.Integration = "openai"
	w.InputTokens = 100
	i.RecordCall(w)
	i.End(EndOptions{})
	if attempts != 3 {
		t.Fatal(attempts)
	}
	if _, ok := lastResponse(t, r)["total_credits"]; ok {
		t.Fatal(lastResponse(t, r))
	}
}

func sdk6111Window(call LLMCall, startMs int64) LLMCall {
	call.StartedAt, call.EndedAt = time.UnixMilli(startMs), time.UnixMilli(startMs+10)
	return call
}

func sdk6111Num(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
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
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", entry.WrittenBy, reverse), func(t *testing.T) {
				r := setupAI(t, WithLLMCallEvents(true))
				var stored PausedInteraction
				if err := json.Unmarshal(entry.Snapshot, &stored); err != nil {
					t.Fatal(err)
				}
				_, i := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
				reports := slices.Clone(fixture.Reports)
				if reverse {
					slices.Reverse(reports)
				}
				for _, report := range reports {
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
		i.mu.Lock()
		i.primary = &primarySelection{model: "selected-model", provider: "aws.bedrock", calls: i.callCount, persistent: true}
		i.mu.Unlock()
		i.hintOutcome(StatusCancelled, "cancelled")
	}
	snapshot, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("pause")
	}
	full := pauseSegmentsJSON(t, snapshot).(map[string]any)
	for _, entry := range fixture.Snapshots {
		if entry.WrittenBy != "go" {
			continue
		}
		var own map[string]any
		if err := json.Unmarshal(entry.Snapshot, &own); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"primary", "outcome_hint"} {
			if !reflect.DeepEqual(full[key], own[key]) {
				t.Fatalf("%s = %v, want %v", key, full[key], own[key])
			}
		}
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
	// Every key Node.js writes has the same value here; report_sequence is this SDK's own.
	for n, node := range records["node"] {
		record := records["go"][n].(map[string]any)
		for key, want := range node.(map[string]any) {
			if !reflect.DeepEqual(record[key], want) {
				t.Errorf("record %d %s = %v, want %v as Node.js writes it", n, key, record[key], want)
			}
		}
		for key := range record {
			if _, shared := node.(map[string]any)[key]; !shared && key != "report_sequence" {
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

func TestSDK6111GBPendingRawModelSurvivesRestore(t *testing.T) {
	for _, restore := range []bool{false, true} {
		for _, rawFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("json=%t/rawFirst=%t", restore, rawFirst), func(t *testing.T) {
				var models []string
				r := setupAI(t, WithCredits(func(c CreditCall) (*float64, error) {
					models = append(models, c.Model)
					if c.Model == "claude-sonnet-4-5-20250929" {
						return Ptr(7.0), nil
					}
					return Ptr(1.0), nil
				}))
				client := &sdk6111ReenterClient{recorder: r, event: "ai_response_received"}
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
				first := "claude-sonnet-4-5"
				if rawFirst {
					first = "claude-sonnet-4-5-20250929"
				}
				call := sdk6111Window(LLMCall{Provider: "anthropic", Model: first, Integration: "openai", ResponseID: "resp_1", InputTokens: 10, OutputTokens: 2}, 100)
				client.reenter = func() { i.RecordCall(call) }
				snapshot, ok := i.Pause(PauseOptions{})
				if !ok {
					t.Fatal("pause")
				}
				if snapshot.PendingCalls[0]["model"] != "claude-sonnet-4-5" || snapshot.PendingCalls[0]["raw_model"] != first {
					t.Fatalf("pending = %v", snapshot.PendingCalls)
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
				call.Model = "claude-sonnet-4-5-20250929"
				if rawFirst {
					call.Model = "claude-sonnet-4-5"
				}
				call.CallPurpose = "answer"
				i.RecordCall(call)
				i.End(EndOptions{})
				if !reflect.DeepEqual(models, []string{first, first}) {
					t.Fatalf("models = %v", models)
				}
				want := 1.0
				if rawFirst {
					want = 7
				}
				if resp := lastResponse(t, r); sdk6111Num(resp["total_credits"]) != want || resp["primary_model"] != "claude-sonnet-4-5" {
					t.Fatalf("response = %v", resp)
				}
			})
		}
	}
}

func TestSDK6111GCPrimaryAndOutcomeHintsPreserveCallOrdering(t *testing.T) {
	for _, restore := range []bool{false, true} {
		for _, hintFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("json=%t/hintFirst=%t", restore, hintFirst), func(t *testing.T) {
				r := setupAI(t)
				client := &sdk6111ReenterClient{recorder: r, event: "ai_response_received"}
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
				client.reenter = func() {
					// Seed a portable selection before the call, exactly as another SDK's snapshot does.
					var selection PausedInteraction
					if err := json.Unmarshal([]byte(`{"primary":{"model":"selected-model","provider":"aws.bedrock","calls":0,"persistent":true}}`), &selection); err != nil {
						t.Fatal(err)
					}
					i.restorePending(&selection)
					if hintFirst {
						i.hintOutcome(StatusBlocked, "content_filter")
					}
					i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4o", Integration: "openai", ResponseID: "resp_1", InputTokens: 10}, 100))
					if !hintFirst {
						i.hintOutcome(StatusBlocked, "content_filter")
					}
				}
				snapshot, ok := i.Pause(PauseOptions{})
				if !ok {
					t.Fatal("pause")
				}
				written := pauseSegmentsJSON(t, snapshot).(map[string]any)
				wantPrimary := map[string]any{"model": "selected-model", "provider": "aws.bedrock", "calls": float64(0), "persistent": true}
				if !reflect.DeepEqual(written["primary"], wantPrimary) {
					t.Fatalf("primary = %v", written["primary"])
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
				i.End(EndOptions{})
				resp := lastResponse(t, r)
				wantStatus := "blocked"
				if hintFirst {
					wantStatus = "success"
				}
				if resp["primary_model"] != "selected-model" || resp["primary_provider"] != "aws.bedrock" || resp["response_status"] != wantStatus {
					t.Fatalf("response = %v", resp)
				}
			})
		}
	}
}

func TestSDK6111GCRestoresPauseReasonFromSelectionMetadata(t *testing.T) {
	for _, form := range []string{"object", "snake_case", "list"} {
		t.Run(form, func(t *testing.T) {
			r := setupAI(t)
			s := map[string]any{"interaction_id": "selection-run", "conversation_id": "selection-thread", "trigger": "user", "user_id": "u", "pause_count": 1, "total_paused_ms": 0, "paused_at": 1, "primary": map[string]any{"model": "selected-model", "provider": "aws.bedrock", "calls": 0}, "outcome_hint": map[string]any{"status": "paused", "calls": 0, "pauseReason": "approval"}}
			if form == "list" {
				s["primary"] = []any{"aws.bedrock", "selected-model", 0}
				s["outcome_hint"] = []any{"paused", nil, 0, "approval"}
			}
			if form == "snake_case" {
				s["outcome_hint"] = map[string]any{"status": "paused", "calls": 0, "pause_reason": "approval"}
			}
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			var stored PausedInteraction
			if err := json.Unmarshal(raw, &stored); err != nil {
				t.Fatal(err)
			}
			_, i := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
			i.End(EndOptions{})
			resp := lastResponse(t, r)
			if resp["response_status"] != "paused" || resp["pause_reason"] != "approval" || resp["primary_model"] != "selected-model" || resp["primary_provider"] != "aws.bedrock" {
				t.Fatalf("response = %v", resp)
			}
		})
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

func TestSDK6111R1CancelledCallsNeverGainCredits(t *testing.T) {
	for _, field := range []string{"agent", "purpose", "fallback", "byok"} {
		for _, mode := range []string{"segment", "in-process", "json"} {
			for _, source := range []string{"openai", "tracing"} {
				t.Run(field+"/"+mode+"/"+source, func(t *testing.T) {
					r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(CreditCall) (*float64, error) { t.Error("cancelled call priced"); return Ptr(1.0), nil }))
					_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
					first := sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4o", InputTokens: 10, OutputTokens: 2, ResponseID: "r1", Integration: source, ErrorType: "cancelled"}, 100)
					if mode == "segment" {
						i.RecordCall(first)
					} else {
						client := &sdk6111ReenterClient{recorder: r, event: "ai_response_received", reenter: func() { i.RecordCall(first) }}
						i.explicitAnalytics = client
						snapshot, ok := i.Pause(PauseOptions{})
						if !ok {
							t.Fatal("pause failed")
						}
						if mode == "json" {
							var stored PausedInteraction
							if err := json.Unmarshal([]byte(mustJSON(snapshot)), &stored); err != nil {
								t.Fatal(err)
							}
							_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
						} else {
							i.Resume()
						}
					}
					later := first
					later.ErrorType = ""
					later.Credits = Ptr(7.0)
					switch field {
					case "agent":
						later.AgentName = "answer"
					case "purpose":
						later.CallPurpose = "answer"
					case "fallback":
						later.FallbackUsed = Ptr(true)
					case "byok":
						later.BYOK = Ptr(true)
					}
					i.RecordCall(later)
					i.End(EndOptions{})
					var calls, tokens float64
					for _, e := range r.named("ai_response_received") {
						calls += sdk6111Num(e.Properties["llm_call_count"])
						tokens += sdk6111Num(e.Properties["total_input_tokens"])
						if _, ok := e.Properties["total_credits"]; ok {
							t.Error("cancelled credits", e.Properties)
						}
					}
					if calls != 1 || tokens != 10 {
						t.Fatalf("calls=%g tokens=%g", calls, tokens)
					}
					if lastResponse(t, r)["response_status"] != "cancelled" {
						t.Fatal(lastResponse(t, r))
					}
					for _, e := range r.named("ai_llm_response_received") {
						if _, ok := e.Properties["credits"]; ok {
							t.Error("call credits", e.Properties)
						}
					}
				})
			}
		}
	}
}

func TestSDK6111R2LegacyFailedPendingCallsKeepCredits(t *testing.T) {
	raw, err := os.ReadFile("spec/fixtures/pause-pending-usage-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Snapshot json.RawMessage `json:"snapshot"`
		Errors   []string        `json:"error_types"`
		Expected map[string]any  `json:"expected"`
	}
	if err = json.Unmarshal(fixtures["legacy_failed_credits"], &fixture); err != nil {
		t.Fatal(err)
	}
	for _, errType := range fixture.Errors {
		for _, field := range []string{"agent", "purpose", "fallback", "byok"} {
			for _, explicit := range []string{"legacy", "true", "false"} {
				t.Run(errType+"/"+field+"/"+explicit, func(t *testing.T) {
					r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(CreditCall) (*float64, error) { t.Error("failed call priced"); return nil, nil }))
					var snapshot PausedInteraction
					if err := json.Unmarshal(fixture.Snapshot, &snapshot); err != nil {
						t.Fatal(err)
					}
					snapshot.PendingCalls[0]["error_type"] = errType
					if explicit != "legacy" {
						snapshot.PendingCalls[0]["credits_explicit"] = explicit == "true"
					}
					_, i := StartInteraction(context.Background(), InteractionOptions{Resume: &snapshot})
					call := sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4o", Integration: "openai", ResponseID: "legacy", InputTokens: 10, OutputTokens: 2}, 100)
					switch field {
					case "agent":
						call.AgentName = "answer"
					case "purpose":
						call.CallPurpose = "answer"
					case "fallback":
						call.FallbackUsed = Ptr(true)
					case "byok":
						call.BYOK = Ptr(true)
					}
					i.RecordCall(call)
					i.End(EndOptions{})
					response := lastResponse(t, r)
					for k, v := range fixture.Expected {
						if n, ok := v.(float64); ok {
							if sdk6111Num(response[k]) != n {
								t.Errorf("%s=%v want %v", k, response[k], v)
							}
						} else if response[k] != v {
							t.Errorf("%s=%v want %v", k, response[k], v)
						}
					}
					if sdk6111Num(r.named("ai_llm_response_received")[0].Properties["credits"]) != 7 {
						t.Fatal("lost call credits")
					}
				})
			}
		}
	}
}

func sdk6111ReviewPending(t *testing.T, r *recorder, mode string, record func(*Interaction)) *Interaction {
	t.Helper()
	client := &sdk6111ReenterClient{recorder: r, event: "ai_response_received"}
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
	if mode == "segment" {
		record(i)
		return i
	}
	client.reenter = func() { record(i) }
	snapshot, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("pause")
	}
	if mode == "json" {
		var stored PausedInteraction
		if err := json.Unmarshal([]byte(mustJSON(snapshot)), &stored); err != nil {
			t.Fatal(err)
		}
		_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	} else {
		i.Resume()
	}
	return i
}
func TestSDK6111R3RepeatHooksRetainNewerEnrichmentAndPricing(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, source := range []string{"openai", "tracing"} {
			for _, threaded := range []bool{false, true} {
				for field := 0; field < 4; field++ {
					t.Run(fmt.Sprintf("%s/%s/thread=%t/field=%d", mode, source, threaded, field), func(t *testing.T) {
						var i *Interaction
						inject := false
						inner := (field + 1) % 4
						call := sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4o", InputTokens: 10, OutputTokens: 2, ResponseID: "r3", Integration: source}, 100)
						enrich := func(c *LLMCall, f int) {
							switch f {
							case 0:
								c.AgentName = "answer"
							case 1:
								c.CallPurpose = "answer"
							case 2:
								c.FallbackUsed = Ptr(true)
							case 3:
								c.BYOK = Ptr(true)
							}
						}
						r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
							if inject {
								inject = false
								record := func() { later := call; enrich(&later, inner); i.RecordCall(later) }
								if threaded {
									done := make(chan struct{})
									go func() { defer close(done); record() }()
									select {
									case <-done:
									case <-time.After(2 * time.Second):
										t.Fatal("credits hook deadlocked")
									}
								} else {
									record()
								}
							}
							present := []bool{c.AgentName != "", c.CallPurpose != "", c.FallbackUsed != nil, c.BYOK != nil}
							if present[field] && present[inner] {
								return Ptr(7.0), nil
							}
							return Ptr(1.0), nil
						}))
						i = sdk6111ReviewPending(t, r, mode, func(i *Interaction) { i.RecordCall(call) })
						inject = true
						later := call
						enrich(&later, field)
						i.RecordCall(later)
						i.End(EndOptions{})
						resp := lastResponse(t, r)
						if sdk6111Num(resp["llm_call_count"]) != 1 || sdk6111Num(resp["total_input_tokens"]) != 10 || sdk6111Num(resp["total_credits"]) != 7 {
							t.Fatal(resp)
						}
						var calls float64
						for _, e := range r.named("ai_response_received") {
							calls += sdk6111Num(e.Properties["llm_call_count"])
						}
						if calls != 1 {
							t.Fatal(calls)
						}
						if source == "tracing" {
							event := r.named("ai_llm_response_received")[0].Properties
							names := []string{"agent_name", "call_purpose", "fallback_used", "byok"}
							for _, f := range []int{field, inner} {
								if _, ok := event[names[f]]; !ok {
									t.Fatal(event)
								}
							}
							if sdk6111Num(event["credits"]) != 7 {
								t.Fatal(event)
							}
						}
					})
				}
			}
		}
	}
}

func TestSDK6111R4LateTwinsAfterAggregateReportsPreservePrimaryOrder(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, count := range []int{3, 10} {
			for _, late := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/count=%d/late=%t", mode, count, late), func(t *testing.T) {
					r := setupAI(t, WithLLMCallEvents(true))
					older := sdk6111Window(LLMCall{Provider: "openai", Model: "older-model", Integration: "openai", ResponseID: "older", InputTokens: 10, OutputTokens: 2, CostUSD: Ptr(0.01)}, 200)
					i := sdk6111ReviewPending(t, r, mode, func(i *Interaction) {
						i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "aggregate-model", Integration: "openai", ResponseID: "aggregate", CallCount: count, InputTokens: 30, CostUSD: Ptr(0.03)}, 100))
						span := older
						span.Integration = "tracing"
						span.ErrorType = "timeout"
						i.RecordCall(span)
					})
					if !late {
						i.RecordCall(older)
					}
					i.RecordCall(sdk6111Window(LLMCall{Provider: "openai", Model: "latest-model", Integration: "openai", ResponseID: "latest", InputTokens: 20, CostUSD: Ptr(0.02)}, 300))
					if late {
						i.RecordCall(older)
					}
					i.End(EndOptions{})
					resp := lastResponse(t, r)
					if resp["primary_model"] != "latest-model" || sdk6111Num(resp["llm_call_count"]) != float64(count+2) || sdk6111Num(resp["total_input_tokens"]) != 60 || sdk6111Num(resp["total_cost_usd"]) != 0.06 {
						t.Fatal(resp)
					}
					var calls float64
					for _, e := range r.named("ai_response_received") {
						calls += sdk6111Num(e.Properties["llm_call_count"])
					}
					if calls != float64(count+2) {
						t.Fatal(calls)
					}
				})
			}
		}
	}
}

func TestSDK6111R5AggregateTwinPricingRetainsMetadataWithUsage(t *testing.T) {
	raw, err := os.ReadFile("spec/fixtures/pause-pending-usage-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Count    int            `json:"call_count"`
		Input    int            `json:"input_tokens"`
		Cost     float64        `json:"cost_usd"`
		Credits  float64        `json:"credits_per_call"`
		Expected map[string]any `json:"expected"`
	}
	if err = json.Unmarshal(fixtures["aggregate_twin_metadata"], &fixture); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, spanFirst := range []bool{false, true} {
			for field := 0; field < 4; field++ {
				t.Run(fmt.Sprintf("%s/spanFirst=%t/field=%d", mode, spanFirst, field), func(t *testing.T) {
					r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
						present := []bool{c.AgentName == "answer", c.CallPurpose == "answer", c.FallbackUsed != nil && *c.FallbackUsed, c.BYOK != nil && *c.BYOK}
						rate := 1.0
						if present[field] {
							rate = fixture.Credits
						}
						return Ptr(float64(c.CallCount) * rate), nil
					}))
					w := sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4o", Integration: "openai", ResponseID: "r5", InputTokens: fixture.Input, OutputTokens: 2, CostUSD: Ptr(fixture.Cost)}, 100)
					switch field {
					case 0:
						w.AgentName = "answer"
					case 1:
						w.CallPurpose = "answer"
					case 2:
						w.FallbackUsed = Ptr(true)
					case 3:
						w.BYOK = Ptr(true)
					}
					s := sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4o", Integration: "tracing", ResponseID: "r5", UsageUnknown: true, CallCount: fixture.Count}, 100)
					first, second := w, s
					if spanFirst {
						first, second = s, w
					}
					i := sdk6111ReviewPending(t, r, mode, func(i *Interaction) { i.RecordCall(first) })
					i.RecordCall(second)
					i.End(EndOptions{})
					resp := lastResponse(t, r)
					for k, v := range fixture.Expected {
						if sdk6111Num(resp[k]) != v.(float64) {
							t.Errorf("%s=%v want %v", k, resp[k], v)
						}
					}
					var credits float64
					for _, e := range r.named("ai_response_received") {
						credits += sdk6111Num(e.Properties["total_credits"])
					}
					if credits != 21 {
						t.Fatal(credits)
					}
				})
			}
		}
	}
}

func TestSDK6111R6TypedNestedBytesStayOutOfTelemetry(t *testing.T) {
	type Nested struct {
		Body []byte `json:"body"`
		Keep string `json:"keep"`
	}
	type Payload struct {
		Body   []byte `json:"body"`
		Nested Nested `json:"nested"`
	}
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, capture := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/capture=%t", mode, capture), func(t *testing.T) {
				r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(capture))
				value := Payload{[]byte("customer-binary-S3cr3t"), Nested{[]byte("secret"), "ok"}}
				i := sdk6111ReviewPending(t, r, mode, func(i *Interaction) {
					i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o", OutputToolCalls: []GenAIToolCall{{Name: "lookup", Arguments: value}}, InputMessages: []GenAIMessage{{Role: "user", Parts: []GenAIPart{{"type": "tool_call", "id": "t", "name": "lookup", "arguments": value}}}}})
				})
				i.End(EndOptions{})
				for _, e := range r.named("ai_llm_response_received") {
					text := mustJSON(e.Properties)
					if strings.Contains(text, "customer-binary-S3cr3t") || strings.Contains(text, "\"body\"") {
						t.Fatal(text)
					}
					if capture && !strings.Contains(text, "ok") {
						t.Fatal(text)
					}
				}
			})
		}
	}
}

func TestSDK6111R8RestoredFractionalTimingRoundsHalfUp(t *testing.T) {
	raw, err := os.ReadFile("spec/fixtures/pause-pending-usage-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Snapshots []struct {
			Writer   string          `json:"written_by"`
			Snapshot json.RawMessage `json:"snapshot"`
		} `json:"snapshots"`
		Expected struct {
			Response map[string]any   `json:"response"`
			Events   []map[string]any `json:"call_events"`
		} `json:"expected"`
	}
	if err = json.Unmarshal(fixtures["fractional_timing"], &fixture); err != nil {
		t.Fatal(err)
	}
	for _, entry := range fixture.Snapshots {
		t.Run(entry.Writer, func(t *testing.T) {
			r := setupAI(t, WithLLMCallEvents(true))
			var stored PausedInteraction
			if err := json.Unmarshal(entry.Snapshot, &stored); err != nil {
				t.Fatal(err)
			}
			_, i := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
			i.End(EndOptions{})
			resp := lastResponse(t, r)
			for k, v := range fixture.Expected.Response {
				if sdk6111Num(resp[k]) != v.(float64) {
					t.Errorf("%s=%v want %v", k, resp[k], v)
				}
			}
			events := r.named("ai_llm_response_received")
			if len(events) != 3 {
				t.Fatal(len(events))
			}
			for n, e := range events {
				for k, v := range fixture.Expected.Events[n] {
					if sdk6111Num(e.Properties[k]) != v.(float64) {
						t.Errorf("%s=%v want %v", k, e.Properties[k], v)
					}
				}
			}
		})
	}
}
func TestSDK6111R8FractionalCallWindowsPreserveTimingAcrossResume(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		t.Run(mode, func(t *testing.T) {
			r := setupAI(t, WithLLMCallEvents(true))
			i := sdk6111ReviewPending(t, r, mode, func(i *Interaction) {
				i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o", Integration: "tracing", InputTokens: 10, StartedAt: time.Unix(0, 100250000), EndedAt: time.Unix(0, 110750000), FirstTokenAt: time.Unix(0, 110750000)})
			})
			i.End(EndOptions{})
			events := r.named("ai_llm_response_received")
			if len(events) != 1 {
				t.Fatal(len(events))
			}
			if sdk6111Num(events[0].Properties["latency_ms"]) != 11 || sdk6111Num(events[0].Properties["time_to_first_token_ms"]) != 11 {
				t.Fatal(events[0].Properties)
			}
		})
	}
}

func TestSDK6111R1CreditsFollowRetainedStatusAndTwinReplacement(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, source := range []string{"openai", "tracing"} {
			for _, errType := range []string{"", "timeout", "cancelled"} {
				t.Run(mode+"/"+source+"/"+errType, func(t *testing.T) {
					r := setupAI(t, WithCredits(func(CreditCall) (*float64, error) { return Ptr(1.0), nil }))
					call := sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4o", Integration: source, ResponseID: "status", InputTokens: 10, ErrorType: errType}, 100)
					i := sdk6111ReviewPending(t, r, mode, func(i *Interaction) { i.RecordCall(call) })
					call.ErrorType = "cancelled"
					call.AgentName = "agent"
					call.Credits = Ptr(7.0)
					i.RecordCall(call)
					i.End(EndOptions{})
					resp := lastResponse(t, r)
					if errType == "cancelled" {
						if _, ok := resp["total_credits"]; ok {
							t.Fatal(resp)
						}
					} else if sdk6111Num(resp["total_credits"]) != 7 {
						t.Fatal(resp)
					}
				})
			}
		}
	}
	for _, mode := range []string{"segment", "in-process", "json"} {
		t.Run(mode+"/replacement", func(t *testing.T) {
			r := setupAI(t, WithCredits(func(CreditCall) (*float64, error) { return Ptr(1.0), nil }))
			call := sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4o", Integration: "tracing", ResponseID: "status-twin", InputTokens: 10, ErrorType: "cancelled"}, 100)
			i := sdk6111ReviewPending(t, r, mode, func(i *Interaction) { i.RecordCall(call) })
			call.Integration = "openai"
			call.ErrorType = ""
			call.AgentName = "agent"
			call.Credits = Ptr(7.0)
			i.RecordCall(call)
			i.End(EndOptions{})
			resp := lastResponse(t, r)
			if resp["response_status"] != "success" || sdk6111Num(resp["total_credits"]) != 7 {
				t.Fatal(resp)
			}
		})
	}
}

func TestSDK6111Review137FilteredReportsKeepPrimaryOrder(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, removed := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s/%d", mode, removed), func(t *testing.T) {
				r := setupAI(t, WithLLMCallEvents(true))
				older := taggedRuleCall("older", "", true, "older", "")
				older.Model = "older-model"
				older.spanSource.ResponseModel = "older-model"
				older.StartedAt = time.UnixMilli(200)
				older.EndedAt = time.UnixMilli(210)
				older.spanSource.Start = 200
				older.spanSource.End = 210
				older.InputTokens = 10
				i := sdk6111ReviewPending(t, r, mode, func(i *Interaction) {
					for n := 0; n < removed; n++ {
						prep := taggedRuleCall(fmt.Sprintf("prep-%d", n), "", false, "", "")
						prep.spanSource.LLM = true
						prep.Model = "prep"
						prep.spanSource.ResponseModel = "prep"
						prep.InputTokens = 0
						prep.UsageUnknown = true
						prep.StartedAt = time.UnixMilli(100)
						prep.EndedAt = time.UnixMilli(110)
						prep.spanSource.Start = 100
						prep.spanSource.End = 110
						i.RecordCall(prep)
					}
					i.RecordCall(older)
					i.RecordCall(LLMCall{Provider: "openai", Model: "latest-model", ResponseID: "latest", InputTokens: 10, Integration: "openai", StartedAt: time.UnixMilli(300), EndedAt: time.UnixMilli(310)})
				})
				client := &sdk6111ReenterClient{recorder: r, event: "ai_llm_response_received", reenter: func() { w := older; w.Integration = "openai"; w.spanSource = nil; i.RecordCall(w) }}
				i.explicitAnalytics = client
				i.Pause(PauseOptions{})
				p := lastResponse(t, r)
				if !client.done || p["primary_model"] != "latest-model" || sdk6111Num(p["llm_call_count"]) != 2 || sdk6111Num(p["total_input_tokens"]) != 20 {
					t.Fatal(p)
				}
				var total float64
				for _, e := range r.named("ai_response_received") {
					total += sdk6111Num(e.Properties["llm_call_count"])
				}
				if total != 2 {
					t.Fatal(total)
				}
			})
		}
	}
}

func TestSDK6111Review137TwinPricingRetainsNewerEnrichment(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, spanFirst := range []bool{true, false} {
			for field := 0; field < 4; field++ {
				for _, threaded := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/spanFirst=%t/field=%d/thread=%t", mode, spanFirst, field, threaded), func(t *testing.T) {
						var i *Interaction
						inject := false
						first := sdk6111Window(LLMCall{Provider: "openai", Model: "gpt-4o", ResponseID: "review137", InputTokens: 10, Integration: "tracing"}, 100)
						if !spanFirst {
							first.Integration = "openai"
							first.InputTokens = 100
						}
						later := first
						later.Integration = "openai"
						later.InputTokens = 100
						if !spanFirst {
							later.Integration = "tracing"
							later.InputTokens = 10
							later.CallCount = 2
						}
						enriched := first
						switch field {
						case 0:
							enriched.AgentName = "answer"
						case 1:
							enriched.CallPurpose = "answer"
						case 2:
							enriched.FallbackUsed = Ptr(true)
						case 3:
							enriched.BYOK = Ptr(true)
						}
						r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
							if inject {
								inject = false
								if threaded {
									done := make(chan struct{})
									go func() { i.RecordCall(enriched); close(done) }()
									select {
									case <-done:
									case <-time.After(5 * time.Second):
										t.Error("repeat blocked")
									}
								} else {
									i.RecordCall(enriched)
								}
							}
							newer := (field == 0 && c.AgentName == "answer") || (field == 1 && c.CallPurpose == "answer") || (field == 2 && c.FallbackUsed != nil && *c.FallbackUsed) || (field == 3 && c.BYOK != nil && *c.BYOK)
							if newer {
								return Ptr(20.0), nil
							}
							return Ptr(10.0), nil
						}))
						i = sdk6111ReviewPending(t, r, mode, func(i *Interaction) { i.RecordCall(first) })
						inject = true
						i.RecordCall(later)
						if inject {
							t.Fatal("twin hook did not run")
						}
						i.End(EndOptions{})
						p := lastResponse(t, r)
						count := 1
						if !spanFirst {
							count = 2
						}
						if sdk6111Num(p["total_input_tokens"]) != 100 || sdk6111Num(p["llm_call_count"]) != float64(count) || sdk6111Num(p["total_credits"]) != 20 {
							t.Fatal(p)
						}
						events := r.named("ai_llm_response_received")
						e := events[len(events)-1].Properties
						key := []string{"agent_name", "call_purpose", "fallback_used", "byok"}[field]
						want := any("answer")
						if field >= 2 {
							want = true
						}
						if spanFirst && (sdk6111Num(e["credits"]) != 20 || e[key] != want) {
							t.Fatal(e)
						}
						var total float64
						for _, e := range r.named("ai_response_received") {
							total += sdk6111Num(e.Properties["total_credits"])
						}
						if total != 20 {
							t.Fatal(total)
						}
					})
				}
			}
		}
	}
}

// SDK-6948: marshaler output is plain data with spans off, even when it encodes native bytes.
type sdk6111Review137JSONPayload struct {
	Body []byte `json:"body"`
	Keep string `json:"keep"`
}

func (p sdk6111Review137JSONPayload) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"body": p.Body, "keep": p.Keep})
}

type sdk6111Review137TextPayload sdk6111Review137JSONPayload

func (p sdk6111Review137TextPayload) MarshalText() ([]byte, error) {
	return []byte("Y3VzdG9tZXItYmluYXJ5LVMzY3IzdA=="), nil
}
func TestSDK6111Review137CustomMarshalerOutputException(t *testing.T) {
	for _, spans := range []bool{false, true} {
		for _, kind := range []string{"json", "text"} {
			t.Run(fmt.Sprintf("spans=%t/%s", spans, kind), func(t *testing.T) {
				r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true), WithEmitSpans(spans))
				payload := sdk6111Review137JSONPayload{Body: []byte("customer-binary-S3cr3t"), Keep: "ok"}
				var value any = payload
				if kind == "text" {
					value = sdk6111Review137TextPayload(payload)
				}
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
				i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o", InputTokens: 10,
					OutputToolCalls: []GenAIToolCall{{Name: "lookup", Arguments: map[string]any{"nested": []any{value}}}},
					InputMessages: []GenAIMessage{{Role: "user", Parts: []GenAIPart{
						{"type": "tool_call", "id": "t", "name": "lookup", "arguments": value},
						{"type": "tool_call_response", "id": "t", "response": value},
					}}},
				})
				i.End(EndOptions{})
				calls := r.named("ai_llm_response_received")
				if len(calls) != 1 {
					t.Fatal(len(calls))
				}
				content := pauseSegmentsJSON(t, calls[0].Properties).(map[string]any)
				out := content["output_tool_calls"].([]any)[0].(map[string]any)["arguments"].(map[string]any)["nested"].([]any)[0]
				parts := content["input_messages"].([]any)[0].(map[string]any)["parts"].([]any)
				arguments := parts[0].(map[string]any)["arguments"]
				response := parts[1].(map[string]any)["response"]
				for _, got := range []any{out, arguments, response} {
					encoded := "Y3VzdG9tZXItYmluYXJ5LVMzY3IzdA=="
					if !spans && kind == "text" {
						if got != encoded {
							t.Fatal(got)
						}
					} else {
						m, ok := got.(map[string]any)
						if !ok || m["keep"] != "ok" {
							t.Fatal(got)
						}
						if spans {
							if _, exists := m["body"]; exists {
								t.Fatal(got)
							}
						} else if m["body"] != encoded {
							t.Fatal(got)
						}
					}
				}
			})
		}
	}
}

func TestSDK6111FinalReentrantAuthoritativePricing(t *testing.T) {
	var i *Interaction
	inject := false
	priced := [][2]any{}
	span := sdk6942Call()
	r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
		priced = append(priced, [2]any{c.InputTokens, c.AgentName})
		if inject {
			inject = false
			enriched := span
			enriched.AgentName = "planner"
			i.RecordCall(enriched)
		}
		factor := 1.0
		if c.AgentName == "planner" {
			factor = 2
		}
		return Ptr(float64(c.InputTokens) * factor), nil
	}))
	_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(span)
	inject = true
	wrapper := span
	wrapper.Integration = "openai"
	wrapper.InputTokens = 100
	i.RecordCall(wrapper)
	i.End(EndOptions{})
	p := lastResponse(t, r)
	t.Logf("PRICING %v RESPONSE %v", priced, p)
	if sdk6111Num(p["total_credits"]) != 200 {
		t.Fatalf("wanted 200 credits, got %v for %v tokens", p["total_credits"], p["total_input_tokens"])
	}
}

func TestSDK6111FinalBlockedAuthoritativePricing(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		t.Run(mode, func(t *testing.T) {
			var i *Interaction
			entered, release := make(chan struct{}), make(chan struct{})
			var gate atomic.Bool
			span := sdk6942Call()
			r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
				if c.InputTokens == 100 && c.AgentName == "" && gate.CompareAndSwap(false, true) {
					close(entered)
					<-release
				}
				factor := 1.0
				if c.AgentName == "planner" {
					factor = 2
				}
				return Ptr(float64(c.InputTokens) * factor), nil
			}))
			i = sdk6111ReviewPending(t, r, mode, func(target *Interaction) { target.RecordCall(span) })
			wrapper := span
			wrapper.Integration = "openai"
			wrapper.InputTokens = 100
			done := make(chan struct{})
			go func() { i.RecordCall(wrapper); close(done) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("hook did not run")
			}
			enriched := span
			enriched.AgentName = "planner"
			i.RecordCall(enriched)
			close(release)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("hook deadlocked")
			}
			i.End(EndOptions{})
			p := lastResponse(t, r)
			t.Logf("RESPONSE %v", p)
			if sdk6111Num(p["total_credits"]) != 200 {
				t.Fatalf("wanted 200 credits, got %v", p["total_credits"])
			}
		})
	}
}

func TestSDK6111FinalPricingBasis(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, spanFirst := range []bool{true, false} {
			for _, shared := range []bool{true, false} {
				for _, field := range []string{"agent", "purpose", "fallback", "byok"} {
					t.Run(fmt.Sprintf("%s/%t/%t/%s", mode, spanFirst, shared, field), func(t *testing.T) {
						var i *Interaction
						inject := false
						first := sdk6942Call()
						if !spanFirst {
							first.Integration = "openai"
							first.InputTokens = 100
						}
						r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
							if inject {
								inject = false
								enriched := first
								switch field {
								case "agent":
									enriched.AgentName = "planner"
								case "purpose":
									enriched.CallPurpose = "planner"
								case "fallback":
									enriched.FallbackUsed = Ptr(true)
								case "byok":
									enriched.BYOK = Ptr(true)
								}
								i.RecordCall(enriched)
							}
							factor := 1.0
							if (field == "agent" && c.AgentName == "planner") || (field == "purpose" && c.CallPurpose == "planner") || (field == "fallback" && c.FallbackUsed != nil && *c.FallbackUsed) || (field == "byok" && c.BYOK != nil && *c.BYOK) {
								factor = 2
							}
							return Ptr(float64(c.InputTokens*c.CallCount) * factor), nil
						}))
						i = sdk6111ReviewPending(t, r, mode, func(target *Interaction) { target.RecordCall(first) })
						inject = true
						second := first
						second.CallCount = 2
						if spanFirst {
							second.Integration = "openai"
							second.InputTokens = 100
						} else {
							second.Integration = "tracing"
							second.InputTokens = 10
						}
						if !shared {
							second.ResponseID = ""
						}
						i.RecordCall(second)
						i.End(EndOptions{})
						p := lastResponse(t, r)
						if sdk6111Num(p["total_credits"]) != 400 || sdk6111Num(p["total_input_tokens"]) != 100 || sdk6111Num(p["llm_call_count"]) != 2 {
							t.Fatal(p)
						}
					})
				}
			}
		}
	}
}

func TestSDK6111FinalPricingRetriesBounded(t *testing.T) {
	var i *Interaction
	armed := false
	attempts := 0
	span := sdk6942Call()
	r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
		if armed && c.InputTokens == 100 {
			attempts++
			e := span
			e.BYOK = Ptr(c.BYOK == nil || !*c.BYOK)
			i.RecordCall(e)
		}
		factor := 1.0
		if c.BYOK != nil && *c.BYOK {
			factor = 2
		}
		return Ptr(float64(c.InputTokens) * factor), nil
	}))
	_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(span)
	armed = true
	w := span
	w.Integration = "openai"
	w.InputTokens = 100
	i.RecordCall(w)
	i.End(EndOptions{})
	if attempts != 3 {
		t.Fatal(attempts)
	}
	if _, ok := lastResponse(t, r)["total_credits"]; ok {
		t.Fatal(lastResponse(t, r))
	}
	events := r.named("ai_llm_response_received")
	if _, ok := events[len(events)-1].Properties["credits"]; ok {
		t.Fatal(events)
	}
}
func TestSDK6111RepeatPricingAfterConcurrentReplacement(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, field := range []string{"agent", "purpose", "fallback", "byok"} {
			t.Run(mode+"/"+field, func(t *testing.T) {
				var i *Interaction
				entered, release := make(chan struct{}), make(chan struct{})
				var gate atomic.Bool
				span := sdk6942Call()
				r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
					enriched := (field == "agent" && c.AgentName == "planner") || (field == "purpose" && c.CallPurpose == "planner") || (field == "fallback" && c.FallbackUsed != nil && *c.FallbackUsed) || (field == "byok" && c.BYOK != nil && *c.BYOK)
					if c.InputTokens == 10 && enriched && gate.CompareAndSwap(false, true) {
						close(entered)
						<-release
					}
					factor := 1.0
					if enriched {
						factor = 2
					}
					return Ptr(float64(c.InputTokens*c.CallCount) * factor), nil
				}))
				i = sdk6111ReviewPending(t, r, mode, func(target *Interaction) { target.RecordCall(span) })
				e := span
				switch field {
				case "agent":
					e.AgentName = "planner"
				case "purpose":
					e.CallPurpose = "planner"
				case "fallback":
					e.FallbackUsed = Ptr(true)
				case "byok":
					e.BYOK = Ptr(true)
				}
				done := make(chan struct{})
				go func() { i.RecordCall(e); close(done) }()
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("hook did not run")
				}
				w := span
				w.Integration = "openai"
				w.InputTokens = 100
				w.Model = "gpt-4o-2024-08-06"
				w.CallCount = 2
				i.RecordCall(w)
				close(release)
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("hook deadlocked")
				}
				i.End(EndOptions{})
				p := lastResponse(t, r)
				if sdk6111Num(p["total_credits"]) != 400 || sdk6111Num(p["total_input_tokens"]) != 100 || sdk6111Num(p["llm_call_count"]) != 2 {
					t.Fatal(p)
				}
			})
		}
	}
}

func TestSDK6111AggregateTwinRetainsWrapperFlags(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, field := range []string{"byok", "fallback"} {
			for _, shared := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/%t", mode, field, shared), func(t *testing.T) {
					r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
						flag := c.BYOK
						if field == "fallback" {
							flag = c.FallbackUsed
						}
						factor := 1.0
						if flag != nil && *flag {
							factor = 2
						}
						return Ptr(float64(c.InputTokens*c.CallCount) * factor), nil
					}))
					call := sdk6942Call()
					if !shared {
						call.ResponseID = ""
					}
					call.Integration = "openai"
					call.InputTokens = 100
					if field == "byok" {
						call.BYOK = Ptr(true)
					} else {
						call.FallbackUsed = Ptr(true)
					}
					i := sdk6111ReviewPending(t, r, mode, func(target *Interaction) { target.RecordCall(call) })
					call.Integration = "tracing"
					call.InputTokens = 10
					call.CallCount = 2
					key := "byok"
					if field == "byok" {
						call.BYOK = Ptr(false)
					} else {
						call.FallbackUsed = Ptr(false)
						key = "fallback_used"
					}
					i.RecordCall(call)
					i.End(EndOptions{})
					if sdk6111Num(lastResponse(t, r)["total_credits"]) != 400 {
						t.Fatal(lastResponse(t, r))
					}
					events := r.named("ai_llm_response_received")
					if events[len(events)-1].Properties[key] != true {
						t.Fatal(events)
					}
				})
			}
		}
	}
}

func TestSDK6111DetachedCallPublishesOwnMetadata(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, shared := range []bool{true, false} {
			for _, ownFlags := range []*bool{nil, Ptr(false)} {
				t.Run(fmt.Sprintf("%s/%t/%v", mode, shared, ownFlags), func(t *testing.T) {
					var i *Interaction
					armed := false
					r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
						if armed {
							armed = false
							i.Pause(PauseOptions{})
						}
						same := func(v *bool) bool {
							return (v == nil && ownFlags == nil) || (v != nil && ownFlags != nil && *v == *ownFlags)
						}
						if c.CallPurpose == "new-purpose" && same(c.BYOK) && same(c.FallbackUsed) {
							return Ptr(7.0), nil
						}
						return Ptr(2.0), nil
					}))
					call := sdk6942Call()
					if !shared {
						call.ResponseID = ""
					}
					call.UsageUnknown = true
					call.InputTokens = 0
					call.CallPurpose = "old-purpose"
					call.BYOK = Ptr(true)
					call.FallbackUsed = Ptr(true)
					i = sdk6111ReviewPending(t, r, mode, func(target *Interaction) { target.RecordCall(call) })
					armed = true
					call.Integration = "openai"
					call.UsageUnknown = false
					call.InputTokens = 100
					call.CallPurpose = "new-purpose"
					call.BYOK = ownFlags
					call.FallbackUsed = ownFlags
					i.RecordCall(call)
					i.End(EndOptions{})
					events := r.named("ai_llm_response_received")
					p := events[len(events)-1].Properties
					if sdk6111Num(p["input_tokens"]) != 100 || sdk6111Num(p["credits"]) != 7 || p["call_purpose"] != "new-purpose" {
						t.Fatal(p)
					}
					for _, key := range []string{"byok", "fallback_used"} {
						if ownFlags == nil {
							if _, ok := p[key]; ok {
								t.Fatal(p)
							}
						} else if p[key] != *ownFlags {
							t.Fatal(p)
						}
					}
					var total float64
					for _, e := range r.named("ai_response_received") {
						total += sdk6111Num(e.Properties["total_input_tokens"])
					}
					if total != 100 {
						t.Fatal(total)
					}
				})
			}
		}
	}
}

func TestSDK6111CanonicalFieldsSupersedeLegacyAliases(t *testing.T) {
	for _, alias := range []string{"twin_matched", "usage_unknown", "started_ns", "ended_ns"} {
		t.Run(alias, func(t *testing.T) {
			r := setupAI(t, WithLLMCallEvents(true))
			client := &sdk6111ReenterClient{recorder: r, event: "ai_response_received"}
			_, original := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
			span := sdk6942Call()
			client.reenter = func() { original.RecordCall(span) }
			snapshot, ok := original.Pause(PauseOptions{})
			if !ok || len(snapshot.PendingCalls) != 1 {
				t.Fatal(snapshot)
			}
			p := snapshot.PendingCalls[0]
			switch alias {
			case "twin_matched", "usage_unknown":
				p[alias] = true
			case "started_ns", "ended_ns":
				p[alias] = int64(999_000_000)
			}
			_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &snapshot})
			if restored.id != original.id {
				t.Fatal("identity changed")
			}
			w := span
			w.Integration = "openai"
			w.InputTokens = 100
			restored.RecordCall(w)
			restored.End(EndOptions{})
			props := lastResponse(t, r)
			if sdk6111Num(props["total_input_tokens"]) != 100 || sdk6111Num(props["llm_call_count"]) != 1 {
				t.Fatal(props)
			}
		})
	}
}
