package rudderstackai

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestTerminalTwinDuringCreditsAndRetries(t *testing.T) {
	for _, terminal := range []string{"api_error", "cancelled", "timeout"} {
		for _, repeat := range []bool{false, true} {
			for _, blockedAttempt := range []int64{2, 3} {
				t.Run(fmt.Sprintf("%s/repeat=%t/attempt=%d", terminal, repeat, blockedAttempt), func(t *testing.T) {
					entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
					var attempts atomic.Int64
					var run *Interaction
					basis := LLMCall{Provider: "openai", Model: "gpt-4o", ResponseID: "terminal", Integration: "tracing", InputTokens: 10, StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110)}
					r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(c CreditCall) (*float64, error) {
						attempt := attempts.Add(1)
						if attempt == 1 {
							return nil, nil
						}
						if attempt == 2 && blockedAttempt == 3 {
							enrichment := basis
							enrichment.CallPurpose = "answer"
							run.RecordCall(enrichment)
						}
						if attempt == blockedAttempt {
							close(entered)
							<-release
						}
						return Ptr(float64(c.InputTokens)), nil
					}))
					_, run = StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
					run.RecordCall(basis)
					success := basis
					if repeat {
						success.AgentName = "agent"
					} else {
						success.Integration = IntegrationOpenAI
						success.InputTokens = 100
					}
					go func() { defer close(done); run.RecordCall(success) }()
					select {
					case <-entered:
					case <-time.After(time.Second):
						close(release)
						<-done
						t.Fatal("pricing did not start")
					}
					failure := basis
					failure.Integration = IntegrationOpenAI
					failure.ErrorType = terminal
					run.RecordCall(failure)
					close(release)
					<-done
					run.End(EndOptions{})
					if attempts.Load() != blockedAttempt {
						t.Errorf("terminal call was retried: %d attempts", attempts.Load())
					}
					for _, event := range r.named("ai_llm_response_received") {
						if _, ok := event.Properties["credits"]; ok {
							t.Errorf("failed call billed: %v", event.Properties)
						}
						if event.Properties["error_type"] != terminal {
							t.Errorf("wrong terminal outcome: %v", event.Properties)
						}
					}
					if _, ok := r.response(t)["total_credits"]; ok {
						t.Fatalf("failed response billed: %v", r.response(t))
					}
				})
			}
		}
	}
}

func TestFailedTwinFirstReportedDuringCredits(t *testing.T) {
	for _, terminal := range []string{"api_error", "cancelled", "timeout"} {
		t.Run(terminal, func(t *testing.T) {
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			r := setupAI(t, WithLLMCallEvents(true), WithCredits(func(CreditCall) (*float64, error) { close(entered); <-release; return Ptr(10.0), nil }))
			_, run := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			call := LLMCall{Provider: "openai", Model: "gpt-4o", ResponseID: "new-twin", Integration: IntegrationOpenAI, InputTokens: 10, StartedAt: time.UnixMilli(100), EndedAt: time.UnixMilli(110)}
			go func() { defer close(done); run.RecordCall(call) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				close(release)
				<-done
				t.Fatal("pricing did not start")
			}
			call.Integration = "tracing"
			call.ErrorType = terminal
			run.RecordCall(call)
			close(release)
			<-done
			run.End(EndOptions{})
			events := r.named("ai_llm_response_received")
			if len(events) != 1 {
				t.Fatalf("expected one merged call, got %d", len(events))
			}
			if _, ok := events[0].Properties["credits"]; ok {
				t.Errorf("late failed twin billed: %v", events[0].Properties)
			}
			if _, ok := r.response(t)["total_credits"]; ok {
				t.Fatalf("late failed twin billed in total: %v", r.response(t))
			}
		})
	}
}
