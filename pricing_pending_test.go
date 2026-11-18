package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"
)

func pricingPendingRoundTrip(t *testing.T, snapshot PausedInteraction) PausedInteraction {
	t.Helper()
	wire, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var restored PausedInteraction
	if err := json.Unmarshal(wire, &restored); err != nil {
		t.Fatal(err)
	}
	return restored
}

func TestPauseCallbackRefusal(t *testing.T) {
	for _, kind := range []string{"credits", "retry", "redact", "capture_marshaler", "analytics", "event_when", "event_properties"} {
		t.Run(kind, func(t *testing.T) {
			var i *Interaction
			var armed bool
			var attempts int
			var errors []ErrorInfo
			invoke := func() {
				if !armed {
					return
				}
				armed = false
				if _, ok := i.Pause(PauseOptions{}); ok || i.Paused() {
					t.Error("callback pause was accepted")
				}
			}
			opts := []ConfigOption{WithLLMCallEvents(true), WithCaptureContent(true),
				WithOnError(func(_ error, info ErrorInfo) { errors = append(errors, info) }),
				WithCredits(func(c CreditCall) (*float64, error) {
					attempts++
					if kind == "credits" {
						invoke()
					}
					if kind == "retry" && armed {
						if attempts == 2 {
							call := pauseCall
							call.ResponseID = "r"
							call.Integration = "tracing"
							call.BYOK = Ptr(true)
							i.RecordCall(call)
						} else {
							invoke()
						}
					}
					multiplier := 1.0
					if c.BYOK != nil && *c.BYOK {
						multiplier = 2
					}
					return Ptr(float64(c.InputTokens) * multiplier), nil
				}),
			}
			if kind == "redact" {
				opts = append(opts, WithRedactHook(func(s string) (string, error) { invoke(); return s, nil }))
			}
			rule := ToolEventRule{Event: "looked_up"}
			if kind == "event_when" {
				rule.When = func(map[string]any, any) bool { invoke(); return true }
			}
			if kind == "event_properties" {
				rule.Properties = func(map[string]any, any) map[string]any { invoke(); return nil }
			}
			opts = append(opts, WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": rule}}))
			r := setupAI(t, opts...)
			_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			if kind == "analytics" {
				client := &review117Client{recorder: r}
				client.setCallback(func(analytics.Track) { invoke() })
				i.adapterAnalytics = client
			}
			call := pauseCall
			call.ResponseID = "r"
			call.OutputText = "answer"
			if kind == "retry" {
				call.Integration = "tracing"
				call.BYOK = Ptr(false)
				i.RecordCall(call)
				call.AgentName = "enriched"
			}
			armed = true
			if kind == "capture_marshaler" {
				call.InputMessages = []GenAIMessage{{Role: "user", Parts: []GenAIPart{{"type": "text", "content": callbackContent{invoke}}}}}
			}
			i.RecordCall(call)
			if kind == "event_when" || kind == "event_properties" {
				i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup"})
				i.RecordToolResult("t", ToolResult{Result: "ok"})
			}
			i.End(EndOptions{})
			if len(errors) != 1 || errors[0].Kind != KindPauseRefused || errors[0].Reason != "in_callback" {
				t.Fatalf("refusal errors: %+v", errors)
			}
			p := lastResponse(t, r)
			credits := 10.0
			if kind == "retry" {
				credits = 20
			}
			if len(r.named("ai_response_received")) != 1 || p["llm_call_count"] != 1 || p["total_credits"] != credits {
				t.Fatal("refusal lost segment or credits", p)
			}
		})
	}
}

func TestPausePricingWait(t *testing.T) {
	for _, stage := range []string{"credits", "capture"} {
		for _, releaseBeforeBound := range []bool{true, false} {
			for _, repeat := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/release=%t/repeat=%t", stage, releaseBeforeBound, repeat), func(t *testing.T) {
					entered, release := make(chan struct{}), make(chan struct{})
					var armed atomic.Bool
					var captureParked atomic.Bool
					errors := make(chan ErrorInfo, 1)
					r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true), WithRedactHook(func(text string) (string, error) {
						if stage == "capture" && armed.Load() && text == "answer" && captureParked.CompareAndSwap(false, true) {
							close(entered)
							<-release
						}
						return text, nil
					}), WithOnError(func(_ error, info ErrorInfo) { errors <- info }), WithCredits(func(CreditCall) (*float64, error) {
						if armed.Load() {
							if stage == "credits" {
								close(entered)
								<-release
							}
							return Ptr(20.0), nil
						}
						return Ptr(10.0), nil
					}))
					_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
					i.pausePricingWait = 50 * time.Millisecond
					call := pauseCall
					call.ResponseID = "r"
					call.Integration = "tracing"
					call.BYOK = Ptr(false)
					if repeat {
						i.RecordCall(call)
					}
					armed.Store(true)
					call.OutputText = "answer"
					call.BYOK = Ptr(true)
					recorded := make(chan struct{})
					go func() { defer close(recorded); i.RecordCall(call) }()
					<-entered
					type result struct {
						snapshot PausedInteraction
						ok       bool
					}
					paused := make(chan result, 1)
					go func() { snapshot, ok := i.Pause(PauseOptions{}); paused <- result{snapshot, ok} }()
					select {
					case <-paused:
						t.Fatal("pause did not wait")
					case <-time.After(5 * time.Millisecond):
					}
					if releaseBeforeBound {
						close(release)
					}
					var got result
					select {
					case got = <-paused:
					case <-time.After(time.Second):
						t.Fatal("pause exceeded bound")
					}
					if releaseBeforeBound {
						if !got.ok {
							t.Fatal("settled pricing was refused")
						}
						restored := pricingPendingRoundTrip(t, got.snapshot)
						_, resumed := StartInteraction(context.Background(), InteractionOptions{Resume: &restored})
						resumed.End(EndOptions{})
						select {
						case e := <-errors:
							t.Fatal(e)
						default:
						}
					} else {
						if got.ok || i.Paused() {
							t.Fatal("bound paused interaction")
						}
						if e := <-errors; e.Kind != KindPauseRefused || e.Reason != "pricing_in_flight" {
							t.Fatal(e)
						}
						close(release)
					}
					<-recorded
					if !releaseBeforeBound {
						i.End(EndOptions{})
					}
					count, credits := 0, 0.0
					for _, e := range r.named("ai_response_received") {
						count += e.Properties["llm_call_count"].(int)
						if v, ok := e.Properties["total_credits"].(float64); ok {
							credits += v
						}
					}
					if count != 1 || credits != 20 {
						t.Fatal(count, credits)
					}
				})
			}
		}
	}
}

func TestPauseErrorHandlerAndOtherInteraction(t *testing.T) {
	var i, other *Interaction
	r := setupAI(t, WithCredits(func(CreditCall) (*float64, error) {
		if _, ok := other.Pause(PauseOptions{}); !ok {
			t.Error("different interaction refused")
		}
		if _, ok := i.Pause(PauseOptions{}); ok {
			t.Error("same callback accepted")
		}
		return Ptr(10.0), nil
	}), WithOnError(func(_ error, info ErrorInfo) {
		if _, ok := i.Pause(PauseOptions{}); !ok {
			t.Error("on_error refused", info)
		}
	}))
	_, other = StartInteraction(context.Background(), InteractionOptions{UserID: "other"})
	_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(pauseCall)
	i.End(EndOptions{})
	other.End(EndOptions{})
	if len(r.named("ai_response_received")) < 2 {
		t.Fatal("missing pauses")
	}
}

func TestEndWhilePricingDoesNotWait(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(CreditCall) (*float64, error) {
		if armed.Load() {
			close(entered)
			<-release
		}
		return Ptr(10.0), nil
	}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	call := pauseCall
	call.ResponseID = "r"
	call.Integration = "tracing"
	call.BYOK = Ptr(false)
	i.RecordCall(call)
	armed.Store(true)
	call.BYOK = Ptr(true)
	done := make(chan struct{})
	go func() { defer close(done); i.RecordCall(call) }()
	<-entered
	i.End(EndOptions{})
	p := lastResponse(t, r)
	if p["llm_call_count"] != 1 || p["total_credits"] != nil {
		t.Fatal(p)
	}
	close(release)
	<-done
	if len(r.named("ai_llm_response_received")) != 1 {
		t.Fatal("duplicate call event")
	}
}

func TestCallbackCallerParsing(t *testing.T) {
	for _, tc := range []struct {
		stack string
		want  uint64
	}{
		{"goroutine 123 [running]:\nignored 456", 123},
		{"goroutine 18446744073709551616 [running]:", 0},
		{"thread 123 [running]:", 0}, {"goroutine invalid [running]:", 0}, {"", 0},
	} {
		if got := parseCallbackCaller(tc.stack); got != tc.want {
			t.Fatalf("parse %q=%d want %d", tc.stack, got, tc.want)
		}
	}
}

func TestPauseWaitsThroughEveryPricingRetry(t *testing.T) {
	first, second, releaseFirst, releaseSecond := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	attempts := 0
	errors := make(chan ErrorInfo, 1)
	r := setupAI(t, WithOnError(func(_ error, info ErrorInfo) { errors <- info }), WithCredits(func(basis CreditCall) (*float64, error) {
		if armed.Load() {
			attempts++
			if attempts == 1 {
				close(first)
				<-releaseFirst
			} else {
				close(second)
				<-releaseSecond
			}
		}
		amount := 10.0
		if basis.BYOK != nil && *basis.BYOK {
			amount = 20
		}
		return Ptr(amount), nil
	}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.pausePricingWait = 200 * time.Millisecond
	call := pauseCall
	call.ResponseID = "retry"
	call.Integration = "tracing"
	call.BYOK = Ptr(false)
	i.RecordCall(call)
	armed.Store(true)
	call.BYOK = Ptr(true)
	recorded := make(chan struct{})
	go func() { defer close(recorded); i.RecordCall(call) }()
	<-first
	paused := make(chan bool, 1)
	go func() { _, ok := i.Pause(PauseOptions{}); paused <- ok }()
	select {
	case <-paused:
		t.Fatal("did not wait for first attempt")
	case <-time.After(5 * time.Millisecond):
	}
	update := call
	update.BYOK = Ptr(false)
	i.RecordCall(update)
	close(releaseFirst)
	<-second
	select {
	case <-paused:
		t.Fatal("did not wait for retry")
	case <-time.After(5 * time.Millisecond):
	}
	close(releaseSecond)
	if !<-paused {
		t.Fatal("settled retry refused")
	}
	<-recorded
	if attempts != 2 {
		t.Fatal(attempts)
	}
	select {
	case err := <-errors:
		t.Fatal(err)
	default:
	}
	p := lastResponse(t, r)
	if p["llm_call_count"] != 1 || p["total_credits"] != 10.0 {
		t.Fatal(p)
	}
	i.End(EndOptions{})
}

func TestLegacyPricingExtensionIsIgnored(t *testing.T) {
	prices := 0
	r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(CreditCall) (*float64, error) { prices++; return Ptr(10.0), nil }))
	client := &review117Client{recorder: r}
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
	client.setCallback(func(event analytics.Track) {
		if event.Event == "ai_response_received" {
			call := pauseCall
			call.Integration = "tracing"
			i.RecordCall(call)
		}
	})
	snapshot, ok := i.Pause(PauseOptions{})
	client.setCallback(nil)
	if !ok {
		t.Fatal("pause")
	}
	snapshot.PendingCalls[0]["pricing_pending"] = "ignored extension"
	before := prices
	snapshot = pricingPendingRoundTrip(t, snapshot)
	_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &snapshot, Analytics: client})
	if restored.id != i.id || prices != before {
		t.Fatal("extension rejected or repriced")
	}
	restored.End(EndOptions{})
	p := lastResponse(t, r)
	if p["llm_call_count"] != 1 || p["total_credits"] != 10.0 {
		t.Fatal(p)
	}
}

func TestErrorHandlerExemptionDoesNotCoverNewSDKCallbacks(t *testing.T) {
	var i *Interaction
	var reported []ErrorInfo
	r := setupAI(t, WithCredits(func(basis CreditCall) (*float64, error) {
		if basis.Model == "outer" {
			return nil, errors.New("credits failed")
		}
		if _, ok := i.Pause(PauseOptions{}); ok {
			t.Error("new SDK callback inherited on_error exemption")
		}
		return Ptr(10.0), nil
	}), WithOnError(func(_ error, info ErrorInfo) {
		reported = append(reported, info)
		if info.Kind == KindUserCallback {
			if _, ok := i.Pause(PauseOptions{}); !ok {
				t.Error("on_error cannot pause")
			}
			i.RecordCall(LLMCall{Provider: "openai", Model: "nested", ResponseID: "nested", InputTokens: 10})
		}
	}))
	_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Provider: "openai", Model: "outer", ResponseID: "outer", InputTokens: 10})
	i.End(EndOptions{})
	if len(reported) != 2 || reported[0].Kind != KindUserCallback || reported[1].Kind != KindPauseRefused || reported[1].Reason != "in_callback" {
		t.Fatal(reported)
	}
	p := lastResponse(t, r)
	if p["llm_call_count"] != 2 || p["total_credits"] != nil {
		t.Fatal(p)
	}
}
