package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"
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

func TestPricingPendingPause(t *testing.T) {
	for _, origin := range []string{"segment", "in-process", "json"} {
		for _, threaded := range []bool{false, true} {
			for _, restore := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/threaded=%t/restore=%t", origin, threaded, restore), func(t *testing.T) {
					var i *Interaction
					var snapshot PausedInteraction
					var armed atomic.Bool
					entered, release := make(chan struct{}), make(chan struct{})
					r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(basis CreditCall) (*float64, error) {
						if armed.Swap(false) {
							if threaded {
								close(entered)
								select {
								case <-release:
								case <-time.After(5 * time.Second):
									t.Error("hook remained blocked")
								}
							} else {
								snapshot, _ = i.Pause(PauseOptions{})
							}
						}
						if basis.BYOK != nil && *basis.BYOK {
							return Ptr(20.0), nil
						}
						return Ptr(10.0), nil
					}))
					call := sdk6111TwinWindow(LLMCall{Provider: "openai", Model: "gpt-4o", InputTokens: 10, ResponseID: "pricing-pending", Integration: "tracing", BYOK: Ptr(false)})
					i = sdk6111TwinPending(t, r, origin, call)
					armed.Store(true)
					call.BYOK = Ptr(true)
					if threaded {
						done := make(chan struct{})
						go func() { defer close(done); i.RecordCall(call) }()
						select {
						case <-entered:
						case <-time.After(5 * time.Second):
							t.Fatal("hook never entered")
						}
						snapshot, _ = i.Pause(PauseOptions{})
						if !restore {
							i.Resume()
						}
						close(release)
						select {
						case <-done:
						case <-time.After(5 * time.Second):
							t.Fatal("report remained blocked")
						}
					} else {
						i.RecordCall(call)
					}
					if len(snapshot.PendingCalls) != 1 || snapshot.PendingCalls[0]["pricing_pending"] != true || snapshot.PendingCalls[0]["byok"] != true || snapshot.PendingCalls[0]["credits"] != nil {
						t.Fatalf("bad pending snapshot: %+v", snapshot)
					}
					responses := r.named("ai_response_received")
					if responses[len(responses)-1].Properties["llm_call_count"] != 0 || len(r.named("ai_llm_response_received")) != 0 {
						t.Fatalf("premature publication: %+v", r.all())
					}
					if restore {
						stored := pricingPendingRoundTrip(t, snapshot)
						_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
					} else {
						i.Resume()
					}
					i.End(EndOptions{})
					credits, calls := 0.0, 0
					for _, response := range r.named("ai_response_received") {
						if v, ok := response.Properties["total_credits"].(float64); ok {
							credits += v
						}
						calls += response.Properties["llm_call_count"].(int)
					}
					events := r.named("ai_llm_response_received")
					if credits != 20 || calls != 1 || len(events) != 1 || events[0].Properties["byok"] != true || events[0].Properties["credits"] != 20.0 {
						t.Fatalf("wrong final publication: %+v", r.all())
					}
				})
			}
		}
	}
}

func TestPricingPendingSecondPauseAfterRestore(t *testing.T) {
	var i *Interaction
	var snapshot PausedInteraction
	armed := false
	r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(basis CreditCall) (*float64, error) {
		if armed {
			armed = false
			snapshot, _ = i.Pause(PauseOptions{})
		}
		if basis.BYOK != nil && *basis.BYOK {
			return Ptr(20.0), nil
		}
		return Ptr(10.0), nil
	}))
	call := sdk6111TwinWindow(LLMCall{Provider: "openai", Model: "gpt-4o", InputTokens: 10, ResponseID: "pricing-pending", Integration: "tracing", BYOK: Ptr(false)})
	i = sdk6111TwinPending(t, r, "segment", call)
	armed = true
	call.BYOK = Ptr(true)
	i.RecordCall(call)
	if err := Configure(WithCredits(func(basis CreditCall) (*float64, error) {
		open.mu.Lock()
		var restored *Interaction
		for e := open.list.Front(); e != nil; e = e.Next() {
			candidate := e.Value.(*Interaction)
			if candidate.id == snapshot.InteractionID {
				restored = candidate
			}
		}
		open.mu.Unlock()
		if restored == nil {
			t.Fatal("restored run is not registered before pricing")
		}
		snapshot, _ = restored.Pause(PauseOptions{})
		return Ptr(20.0), nil
	})); err != nil {
		t.Fatal(err)
	}
	stored := pricingPendingRoundTrip(t, snapshot)
	StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	if len(snapshot.PendingCalls) != 1 || snapshot.PendingCalls[0]["pricing_pending"] != true {
		t.Fatalf("lost pending marker: %+v", snapshot)
	}
	if err := Configure(WithCredits(func(CreditCall) (*float64, error) { return Ptr(20.0), nil })); err != nil {
		t.Fatal(err)
	}
	stored = pricingPendingRoundTrip(t, snapshot)
	_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	i.End(EndOptions{})
	credits := 0.0
	for _, response := range r.named("ai_response_received") {
		if v, ok := response.Properties["total_credits"].(float64); ok {
			credits += v
		}
	}
	if credits != 20 || len(r.named("ai_llm_response_received")) != 1 {
		t.Fatalf("wrong restored totals: %+v", r.all())
	}
}

func TestPricingPendingEndWhileBlocked(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%t", resume), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(basis CreditCall) (*float64, error) {
				if basis.BYOK != nil && *basis.BYOK {
					close(entered)
					select {
					case <-release:
					case <-time.After(5 * time.Second):
						t.Error("hook remained blocked")
					}
					return Ptr(20.0), nil
				}
				return Ptr(10.0), nil
			}))
			call := sdk6111TwinWindow(LLMCall{Provider: "openai", Model: "gpt-4o", InputTokens: 10, ResponseID: "pricing-pending", Integration: "tracing", BYOK: Ptr(false)})
			i := sdk6111TwinPending(t, r, "segment", call)
			call.BYOK = Ptr(true)
			done := make(chan struct{})
			go func() { defer close(done); i.RecordCall(call) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("hook never entered")
			}
			i.Pause(PauseOptions{})
			if resume {
				i.Resume()
			}
			i.End(EndOptions{})
			close(release)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("report remained blocked")
			}
			responses := r.named("ai_response_received")
			events := r.named("ai_llm_response_received")
			if len(responses) != 2 || responses[0].Properties["llm_call_count"] != 0 || responses[1].Properties["llm_call_count"] != 1 || responses[1].Properties["total_credits"] != nil || len(events) != 1 || events[0].Properties["credits"] != nil || events[0].Properties["byok"] != true {
				t.Fatalf("wrong End publication: %+v", r.all())
			}
		})
	}
}

func TestPricingPendingRestoredRevisionRules(t *testing.T) {
	for _, mode := range []string{"retry", "unstable", "throw"} {
		t.Run(mode, func(t *testing.T) {
			var i *Interaction
			var snapshot PausedInteraction
			armed := false
			r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(basis CreditCall) (*float64, error) {
				if armed {
					armed = false
					snapshot, _ = i.Pause(PauseOptions{})
				}
				if basis.BYOK != nil && *basis.BYOK {
					return Ptr(20.0), nil
				}
				return Ptr(10.0), nil
			}))
			call := sdk6111TwinWindow(LLMCall{Provider: "openai", Model: "gpt-4o", InputTokens: 10, ResponseID: "pricing-pending", Integration: "tracing", BYOK: Ptr(false)})
			i = sdk6111TwinPending(t, r, "segment", call)
			armed = true
			call.BYOK = Ptr(true)
			i.RecordCall(call)
			attempts := 0
			var errors []string
			if err := Configure(WithOnError(func(_ error, info ErrorInfo) { errors = append(errors, string(info.Kind)) }), WithCredits(func(basis CreditCall) (*float64, error) {
				attempts++
				if mode != "retry" || attempts == 1 {
					open.mu.Lock()
					var restored *Interaction
					for e := open.list.Front(); e != nil; e = e.Next() {
						candidate := e.Value.(*Interaction)
						if candidate.id == snapshot.InteractionID {
							restored = candidate
						}
					}
					open.mu.Unlock()
					if restored == nil {
						t.Fatal("restored run missing")
					}
					call.BYOK = Ptr(!*basis.BYOK)
					restored.RecordCall(call)
				}
				if mode == "throw" {
					return nil, fmt.Errorf("credits callback failed")
				}
				if *basis.BYOK {
					return Ptr(20.0), nil
				}
				return Ptr(10.0), nil
			})); err != nil {
				t.Fatal(err)
			}
			stored := pricingPendingRoundTrip(t, snapshot)
			_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
			i.End(EndOptions{})
			wantAttempts := map[string]int{"retry": 2, "unstable": 3, "throw": 1}[mode]
			if attempts != wantAttempts {
				t.Fatalf("attempts=%d", attempts)
			}
			event := r.props(t, "ai_llm_response_received")
			if event["byok"] != false {
				t.Fatalf("stale metadata: %+v", event)
			}
			if mode == "retry" {
				if len(errors) != 0 || event["credits"] != 10.0 {
					t.Fatalf("bad retry result: errors=%v,event=%v", errors, event)
				}
			} else {
				want := "credits_unstable"
				if mode == "throw" {
					want = "user_callback"
				}
				if len(errors) != 1 || errors[0] != want || event["credits"] != nil {
					t.Fatalf("bad failure result: errors=%v,event=%v", errors, event)
				}
			}
		})
	}
}

func TestPricingPendingWritesUntimedSnapshot(t *testing.T) {
	var snapshot PausedInteraction
	r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(CreditCall) (*float64, error) {
		open.mu.Lock()
		restored := open.list.Back().Value.(*Interaction)
		open.mu.Unlock()
		snapshot, _ = restored.Pause(PauseOptions{})
		return Ptr(20.0), nil
	}))
	// A legacy untimed held record: restoration must keep it held while repricing.
	stored := PausedInteraction{InteractionID: "pricing-pending-untimed", ConversationID: "pricing-pending-conversation", Trigger: TriggerUser, UserID: "u", PauseCount: 1, PausedAt: time.Now().UnixMilli(), PendingCalls: []map[string]any{{"provider": "openai", "model": "gpt-4o", "raw_model": "gpt-4o", "input_tokens": 10, "call_count": 1, "byok": true, "pricing_pending": true, "call_event_pending": true, "call_index": 1, "response_id": "pricing-pending", "usage_known": true, "from_span": true, "matched": false}}}
	_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
	if len(snapshot.PendingCalls) != 1 || snapshot.PendingCalls[0]["pricing_pending"] != true || snapshot.PendingCalls[0]["credits"] != nil || snapshot.PendingCalls[0]["started_at"] != nil {
		t.Fatalf("wrong untimed snapshot: %+v", snapshot)
	}
	if path := os.Getenv("PRICING_PENDING_UNTIMED_SNAPSHOT"); path != "" {
		data, err := json.MarshalIndent(snapshot, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	restored.Resume()
	restored.End(EndOptions{})
	events := r.named("ai_llm_response_received")
	if len(events) != 1 || events[0].Properties["credits"] != 20.0 {
		t.Fatalf("wrong untimed publication: %+v", r.all())
	}
}

func TestPricingPendingAnotherGoroutinePausesRestore(t *testing.T) {
	var i *Interaction
	var snapshot PausedInteraction
	armed := false
	r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(basis CreditCall) (*float64, error) {
		if armed {
			armed = false
			snapshot, _ = i.Pause(PauseOptions{})
		}
		if basis.BYOK != nil && *basis.BYOK {
			return Ptr(20.0), nil
		}
		return Ptr(10.0), nil
	}))
	call := sdk6111TwinWindow(LLMCall{Provider: "openai", Model: "gpt-4o", InputTokens: 10, ResponseID: "pricing-pending", Integration: "tracing", BYOK: Ptr(false)})
	i = sdk6111TwinPending(t, r, "segment", call)
	armed = true
	call.BYOK = Ptr(true)
	i.RecordCall(call)
	entered, release := make(chan struct{}), make(chan struct{})
	if err := Configure(WithCredits(func(CreditCall) (*float64, error) {
		close(entered)
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			t.Error("hook remained blocked")
		}
		return Ptr(20.0), nil
	})); err != nil {
		t.Fatal(err)
	}
	stored := pricingPendingRoundTrip(t, snapshot)
	done := make(chan *Interaction, 1)
	go func() {
		_, restored := StartInteraction(context.Background(), InteractionOptions{Resume: &stored})
		done <- restored
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("restore pricing never entered")
	}
	open.mu.Lock()
	restored := open.list.Back().Value.(*Interaction)
	open.mu.Unlock()
	pending, ok := restored.Pause(PauseOptions{})
	if !ok || len(pending.PendingCalls) != 1 || pending.PendingCalls[0]["pricing_pending"] != true {
		t.Errorf("bad restored pause: %+v", pending)
	}
	if len(r.named("ai_llm_response_received")) != 0 {
		t.Error("call emitted while pricing was blocked")
	}
	close(release)
	select {
	case restored = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("restore remained blocked")
	}
	restored.Resume()
	restored.End(EndOptions{})
	credits := 0.0
	for _, event := range r.named("ai_response_received") {
		if value, ok := event.Properties["total_credits"].(float64); ok {
			credits += value
		}
	}
	if credits != 20 || r.props(t, "ai_llm_response_received")["credits"] != 20.0 {
		t.Fatalf("wrong restored credits: %+v", r.all())
	}
}
