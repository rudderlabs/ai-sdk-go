package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

type review117Client struct {
	*recorder
	callbackMu sync.Mutex
	callback   func(analytics.Track)
}

func (r *review117Client) setCallback(callback func(analytics.Track)) {
	r.callbackMu.Lock()
	defer r.callbackMu.Unlock()
	r.callback = callback
}

func (r *review117Client) Enqueue(message analytics.Message) error {
	err := r.recorder.Enqueue(message)
	r.callbackMu.Lock()
	callback := r.callback
	r.callbackMu.Unlock()
	if event, ok := message.(analytics.Track); ok && callback != nil {
		callback(event)
	}
	return err
}

func review117Pending(t *testing.T, client *review117Client, mode string, initial func(*Interaction)) *Interaction {
	t.Helper()
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
	if mode == "segment" {
		initial(i)
		return i
	}
	client.setCallback(func(event analytics.Track) {
		if event.Event == "ai_response_received" && event.Properties["response_status"] == "paused" {
			client.setCallback(nil)
			initial(i)
		}
	})
	snapshot, ok := i.Pause(PauseOptions{})
	client.setCallback(nil)
	if !ok {
		t.Fatal("pause failed")
	}
	if mode == "json" {
		wire, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var stored PausedInteraction
		if err := json.Unmarshal(wire, &stored); err != nil {
			t.Fatal(err)
		}
		_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &stored, Analytics: client})
	} else {
		i.Resume()
	}
	return i
}

func review117Invoke(callback func(), threaded bool) bool {
	if !threaded {
		callback()
		return true
	}
	done := make(chan struct{})
	go func() { defer close(done); callback() }()
	select {
	case <-done:
		return true
	case <-time.After(time.Second):
		return false
	}
}

func TestSDK6111Review117DeliveryOnce(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, threaded := range []bool{false, true} {
			for _, scenario := range []struct {
				drain    string
				overflow bool
			}{{"pause", false}, {"ready", false}, {"pause", true}, {"ready", true}} {
				drain, overflow := scenario.drain, scenario.overflow
				t.Run(fmt.Sprintf("%s/threaded=%t/%s/overflow=%t", mode, threaded, drain, overflow), func(t *testing.T) {
					limit := 500
					if overflow {
						limit = 1
					}
					r := setupAI(t, WithLLMCallEvents(true), WithLimits(Limits{MaxCallsPerInteraction: limit}))
					client := &review117Client{recorder: r}
					barrier := sdk6111TwinWindow(LLMCall{Provider: "openai", Model: "gpt-4o", ResponseID: "barrier", InputTokens: 10, Integration: "tracing"})
					i := review117Pending(t, client, mode, func(run *Interaction) {
						run.RecordCall(barrier)
						run.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o", ResponseID: "second", InputTokens: 20})
					})
					var progress []bool
					client.setCallback(func(event analytics.Track) {
						if event.Event == "ai_llm_response_received" && event.Properties["response_id"] == "barrier" {
							client.setCallback(nil)
							progress = append(progress, review117Invoke(func() {
								i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o", ResponseID: "third", InputTokens: 30})
							}, threaded))
						}
					})
					if drain == "ready" {
						wrapper := barrier
						wrapper.Integration = "openai"
						i.RecordCall(wrapper)
					}
					i.Pause(PauseOptions{})
					i.Resume()
					i.End(EndOptions{})
					var ids []string
					callTokens, responseTokens, count := 0, 0, 0
					for _, call := range r.named("ai_llm_response_received") {
						if id, ok := call.Properties["response_id"].(string); ok {
							ids = append(ids, id)
						}
						callTokens += call.Properties["input_tokens"].(int)
					}
					for _, response := range r.named("ai_response_received") {
						responseTokens += response.Properties["total_input_tokens"].(int)
						count += response.Properties["llm_call_count"].(int)
					}
					if !slices.Equal(progress, []bool{true}) || (!overflow && !slices.Equal(ids, []string{"barrier", "second", "third"})) || callTokens != 60 || responseTokens != 60 || count != 3 {
						t.Fatalf("progress=%v ids=%v call tokens=%d response tokens=%d count=%d", progress, ids, callTokens, responseTokens, count)
					}
				})
			}
		}
	}
}

func TestSDK6111Review117ThrowingRetry(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, threaded := range []bool{false, true} {
			for _, field := range []string{"agent_name", "call_purpose", "fallback_used", "byok", "usage"} {
				t.Run(fmt.Sprintf("%s/threaded=%t/%s", mode, threaded, field), func(t *testing.T) {
					var i *Interaction
					armed := false
					var attempts []CreditCall
					var notices []ErrorInfo
					var progress []bool
					span := sdk6111TwinWindow(LLMCall{Provider: "openai", Model: "gpt-4o", ResponseID: "retry", InputTokens: 10, Integration: "tracing"})
					r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(call CreditCall) (*float64, error) {
						if !armed {
							return Ptr(1.0), nil
						}
						attempts = append(attempts, call)
						if len(attempts) == 1 {
							update := sdk6111TwinEnrich(span, field)
							if field == "usage" {
								update = span
								update.Integration = "openai"
								update.InputTokens = 100
							}
							progress = append(progress, review117Invoke(func() { i.RecordCall(update) }, threaded))
							return Ptr(10.0), nil
						}
						return nil, errors.New("retry failed")
					}), WithOnError(func(_ error, info ErrorInfo) { notices = append(notices, info) }))
					i = review117Pending(t, &review117Client{recorder: r}, mode, func(run *Interaction) { run.RecordCall(span) })
					armed = true
					seed := span
					if field == "agent_name" {
						seed.CallPurpose = "seed"
					} else {
						seed.AgentName = "seed"
					}
					i.RecordCall(seed)
					i.End(EndOptions{})
					if !slices.Equal(progress, []bool{true}) || len(attempts) != 2 {
						t.Fatalf("progress=%v attempts=%+v", progress, attempts)
					}
					if field == "usage" {
						if attempts[0].InputTokens != 10 || attempts[1].InputTokens != 100 {
							t.Fatalf("pricing usage=%+v", attempts)
						}
					} else if sdk6111TwinFlag(attempts[0], field) || !sdk6111TwinFlag(attempts[1], field) {
						t.Fatalf("pricing metadata=%+v", attempts)
					}
					if len(notices) != 1 || notices[0].Kind != "user_callback" || notices[0].Where != "credits" {
						t.Fatalf("notices=%+v", notices)
					}
					calls := r.named("ai_llm_response_received")
					if _, ok := calls[len(calls)-1].Properties["credits"]; ok {
						t.Fatal("invalidated credits reached call event")
					}
					response := lastResponse(t, r)
					if _, ok := response["total_credits"]; ok {
						t.Fatal("invalidated credits reached response total")
					}
					rows, _ := response["usage_breakdown"].([]map[string]any)
					for _, row := range rows {
						if _, ok := row["credits"]; ok {
							t.Fatal("invalidated credits reached usage breakdown")
						}
					}
				})
			}
		}
	}
}

func TestSDK6111Review117UnstableNoticeUnlocked(t *testing.T) {
	for _, mode := range []string{"segment", "in-process", "json"} {
		for _, path := range []string{"repeat", "twin"} {
			for _, action := range []string{"pause", "end", "record", "read"} {
				t.Run(mode+"/"+path+"/"+action, func(t *testing.T) {
					var i *Interaction
					armed, attempts := false, 0
					var progress []bool
					var notices []ErrorInfo
					span := sdk6111TwinWindow(LLMCall{Provider: "openai", Model: "gpt-4o", ResponseID: "unstable", InputTokens: 10, Integration: "tracing"})
					r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(call CreditCall) (*float64, error) {
						if armed {
							attempts++
							update := span
							update.BYOK = Ptr(call.BYOK == nil || !*call.BYOK)
							i.RecordCall(update)
						}
						return Ptr(float64(call.InputTokens)), nil
					}), WithOnError(func(_ error, info ErrorInfo) {
						notices = append(notices, info)
						if info.Kind != "credits_unstable" {
							return
						}
						armed = false
						progress = append(progress, review117Invoke(func() {
							switch action {
							case "pause":
								i.Pause(PauseOptions{})
							case "end":
								i.End(EndOptions{})
							case "record":
								i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o", ResponseID: "worker", InputTokens: 20})
							case "read":
								i.Ended()
							}
						}, true))
					}))
					i = review117Pending(t, &review117Client{recorder: r}, mode, func(run *Interaction) { run.RecordCall(span) })
					armed = true
					update := span
					if path == "twin" {
						update.Integration = "openai"
						update.InputTokens = 100
					} else {
						update.AgentName = "planner"
					}
					i.RecordCall(update)
					i.End(EndOptions{})
					if attempts != 3 || !slices.Equal(progress, []bool{true}) {
						t.Fatalf("attempts=%d worker progress=%v; error callback must release mutex", attempts, progress)
					}
					if len(notices) != 1 || notices[0].Kind != "credits_unstable" || notices[0].Where != "credits" {
						t.Fatalf("notices=%+v", notices)
					}
				})
			}
		}
	}
}
