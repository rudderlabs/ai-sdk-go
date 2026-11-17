package rudderstackai

// Shared fixture: which segment counts usage recorded while pause or end delivers events.

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	analytics "github.com/rudderlabs/analytics-go/v4"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
)

type pauseSegmentsStep struct {
	AgentName            string              `json:"agent_name"`
	FallbackUsed         *bool               `json:"fallback_used"`
	PendingCallOverrides map[string]any      `json:"pending_call_overrides"`
	PendingCallOmit      []string            `json:"pending_call_omit"`
	ErrorType            string              `json:"error_type"`
	CallPurpose          string              `json:"call_purpose"`
	Credits              *float64            `json:"credits"`
	Refused              bool                `json:"refused"`
	ResponseID           string              `json:"response_id"`
	BYOK                 *bool               `json:"byok"`
	Text                 string              `json:"text"`
	Op                   string              `json:"op"`
	Source               string              `json:"source"`
	Provider             string              `json:"provider"`
	Model                string              `json:"model"`
	InputTokens          *int                `json:"input_tokens"`
	StartedMS            *int64              `json:"started_ms"`
	EndedMS              *int64              `json:"ended_ms"`
	OutputText           string              `json:"output_text"`
	SpanID               string              `json:"span_id"`
	ParentSpan           string              `json:"parent_span_id"`
	ID                   string              `json:"id"`
	Result               any                 `json:"result"`
	Error                *bool               `json:"error"`
	Event                string              `json:"event"`
	Steps                []pauseSegmentsStep `json:"steps"`
	JSON                 bool                `json:"json"`
	Status               string              `json:"status"`
	Calls                []struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		SDKs      []string `json:"sdks"`
		Result    any      `json:"result"`
		Error     *bool    `json:"error"`
		AgentName string   `json:"agent_name"`
		ErrorType string   `json:"error_type"`
	} `json:"calls"`
}

type pauseSegmentsHook struct {
	event string
	steps []pauseSegmentsStep
}

// pauseSegmentsClient runs each on_event hook once, synchronously, inside delivery of the first
// matching track event, as an app's transport callback can.
type pauseSegmentsClient struct {
	*recorder
	mu    sync.Mutex
	hooks []pauseSegmentsHook
	run   func([]pauseSegmentsStep)
}

func (c *pauseSegmentsClient) Enqueue(m analytics.Message) error {
	err := c.recorder.Enqueue(m)
	if e, ok := m.(analytics.Track); ok {
		c.mu.Lock()
		var steps []pauseSegmentsStep
		for n, h := range c.hooks {
			if h.event == e.Event {
				steps = h.steps
				c.hooks = append(c.hooks[:n], c.hooks[n+1:]...)
				break
			}
		}
		c.mu.Unlock()
		if steps != nil {
			c.run(steps)
		}
	}
	return err
}

// pauseSegmentsJSON is v as JSON values, so fixture numbers compare with event numbers.
func pauseSegmentsJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPauseSegmentsFixture(t *testing.T) {
	raw, err := os.ReadFile("spec/fixtures/pause-segments-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name   string `json:"name"`
			Config struct {
				Credits *struct {
					PerInputToken  float64               `json:"per_input_token"`
					BYOKMultiplier float64               `json:"byok_multiplier"`
					OnHook         [][]pauseSegmentsStep `json:"on_hook"`
				} `json:"credits"`
				EmitSpans      bool `json:"emit_spans"`
				EmitLLMCalls   bool `json:"emit_llm_calls"`
				CaptureContent bool `json:"capture_content"`
				MaxCalls       int  `json:"max_calls_per_interaction"`
				EventRules     *struct {
					Tools map[string]string `json:"tools"`
				} `json:"event_rules"`
				PricingWaitMS int `json:"pricing_wait_ms"`
			} `json:"config"`
			Steps    []pauseSegmentsStep `json:"steps"`
			Expected struct {
				Segments     []map[string]any `json:"segments"`
				Errors       []string         `json:"errors"`
				ErrorReasons []string         `json:"error_reasons"`
				CallCredits  []*float64       `json:"call_credits"`
				CallEvents   []float64        `json:"call_events"`
				ToolEvents   []map[string]any `json:"tool_events"`
			} `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			opts := []ConfigOption{WithLLMCallEvents(tc.Config.EmitLLMCalls), WithEmitSpans(tc.Config.EmitSpans), WithCaptureContent(tc.Config.CaptureContent)}
			if tc.Config.MaxCalls > 0 {
				opts = append(opts, WithLimits(Limits{MaxCallsPerInteraction: tc.Config.MaxCalls}))
			}
			if tc.Config.EventRules != nil {
				rules := map[string]ToolEventRule{}
				for name, event := range tc.Config.EventRules.Tools {
					rules[name] = ToolEventRule{Event: event}
				}
				opts = append(opts, WithEventRules(EventRules{Tools: rules}))
			}
			var redactors []pauseSegmentsStep
			var run func([]pauseSegmentsStep)
			errors := []string{}
			errorReasons := []string{}
			entered, release := make(chan struct{}), make(chan struct{})
			var pricingBlock atomic.Bool
			opts = append(opts, WithOnError(func(_ error, info ErrorInfo) {
				errors = append(errors, info.Kind)
				errorReasons = append(errorReasons, info.Reason)
			}))
			if pricing := tc.Config.Credits; pricing != nil {
				attempts := 0
				opts = append(opts, WithCredits(func(call CreditCall) (*float64, error) {
					if pricingBlock.Load() {
						close(entered)
						<-release
					}
					invocation := attempts
					attempts++
					if invocation < len(pricing.OnHook) {
						run(pricing.OnHook[invocation])
					}
					price := float64(call.InputTokens) * pricing.PerInputToken
					if call.BYOK != nil && *call.BYOK {
						price *= pricing.BYOKMultiplier
					}
					return Ptr(price), nil
				}))
			}
			opts = append(opts, WithRedactHook(func(text string) (string, error) {
				for n, h := range redactors {
					if h.Text == text {
						redactors = append(redactors[:n], redactors[n+1:]...)
						run(h.Steps)
						break
					}
				}
				return text, nil
			}))
			r := setupAI(t, opts...)
			client := &pauseSegmentsClient{recorder: r}
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
			run = func(steps []pauseSegmentsStep) {
				for _, step := range steps {
					switch step.Op {
					case "record_call":
						call := LLMCall{Provider: step.Provider, Model: step.Model, Integration: "openai", InputTokens: derefInt(step.InputTokens), OutputText: step.OutputText, ResponseID: step.ResponseID, BYOK: step.BYOK, ErrorType: step.ErrorType, CallPurpose: step.CallPurpose, Credits: step.Credits, AgentName: step.AgentName, FallbackUsed: step.FallbackUsed}
						if step.Source == "span" {
							call.Integration = "tracing"
						}
						if step.StartedMS != nil {
							call.StartedAt = time.UnixMilli(*step.StartedMS)
						}
						if step.EndedMS != nil {
							call.EndedAt = time.UnixMilli(*step.EndedMS)
						}
						if step.SpanID != "" {
							call.spanSource = &hook.SpanSource{
								TraceID: "trace", SpanID: step.SpanID, ParentSpanID: step.ParentSpan, RequestModel: step.Model,
								Start: float64(call.StartedAt.UnixMilli()), End: float64(call.EndedAt.UnixMilli()),
								Usage: step.InputTokens != nil, Output: step.OutputText != "", LLM: true, Ancestors: []string{},
							}
						}
						i.RecordCall(call)
					case "pricing_wait":
						i.pausePricingWait = time.Duration(tc.Config.PricingWaitMS) * time.Millisecond
						pricingBlock.Store(true)
						done := make(chan struct{})
						go func() {
							defer close(done)
							i.RecordCall(LLMCall{Provider: step.Provider, Model: step.Model, InputTokens: derefInt(step.InputTokens)})
						}()
						<-entered
						if _, ok := i.Pause(PauseOptions{}); ok || i.Paused() {
							t.Fatal("pricing bound paused")
						}
						close(release)
						<-done
						pricingBlock.Store(false)
					case "record_tool_call":
						calls := make([]ToolCall, 0, len(step.Calls))
						for _, c := range step.Calls {
							calls = append(calls, ToolCall{ID: c.ID, Name: c.Name, AgentName: c.AgentName, Result: c.Result, Error: c.Error, ErrorType: c.ErrorType})
						}
						i.RecordToolCalls(calls...)
					case "record_tool_result":
						i.RecordToolResult(step.ID, ToolResult{Result: step.Result, Error: step.Error, ErrorType: step.ErrorType})
					case "on_event":
						client.mu.Lock()
						client.hooks = append(client.hooks, pauseSegmentsHook{step.Event, step.Steps})
						client.mu.Unlock()
					case "on_redact":
						redactors = append(redactors, step)
					case "pause":
						snapshot, ok := i.Pause(PauseOptions{})
						if ok == step.Refused {
							t.Fatal("unexpected pause result")
						}
						if step.JSON && ok {
							var stored PausedInteraction
							if err := json.Unmarshal([]byte(mustJSON(snapshot)), &stored); err != nil {
								t.Fatal(err)
							}
							if step.PendingCallOverrides != nil || step.PendingCallOmit != nil {
								if len(stored.PendingCalls) != 1 {
									t.Fatal("expected one pending call")
								}
								for key, value := range step.PendingCallOverrides {
									stored.PendingCalls[0][key] = value
								}
								for _, key := range step.PendingCallOmit {
									delete(stored.PendingCalls[0], key)
								}
							}
							_, i = StartInteraction(context.Background(), InteractionOptions{Resume: &stored, Analytics: client})
						}
					case "resume":
						i.Resume()
					case "end":
						i.End(EndOptions{Status: ResponseStatus(step.Status)})
					default:
						t.Fatalf("unknown op %q", step.Op)
					}
				}
			}
			client.run = run
			client.run(tc.Steps)
			if tc.Expected.ErrorReasons != nil && !reflect.DeepEqual(errorReasons, tc.Expected.ErrorReasons) {
				t.Fatal("error reasons", errorReasons)
			}

			segments := r.named("ai_response_received")
			if len(segments) != len(tc.Expected.Segments) {
				t.Fatalf("ai_response_received = %d, want %d", len(segments), len(tc.Expected.Segments))
			}
			for n, want := range tc.Expected.Segments {
				got := pauseSegmentsJSON(t, segments[n].Properties).(map[string]any)
				for k, v := range want {
					if !reflect.DeepEqual(got[k], v) {
						t.Errorf("segment %d %s = %v, want %v", n, k, got[k], v)
					}
				}
			}
			calls := []float64{}
			for _, e := range r.named("ai_llm_response_received") {
				calls = append(calls, pauseSegmentsJSON(t, e.Properties["input_tokens"]).(float64))
			}
			if !reflect.DeepEqual(calls, tc.Expected.CallEvents) {
				t.Errorf("call event input_tokens = %v, want %v", calls, tc.Expected.CallEvents)
			}
			if !reflect.DeepEqual(errors, append([]string{}, tc.Expected.Errors...)) {
				t.Errorf("errors = %v, want %v", errors, tc.Expected.Errors)
			}
			if tc.Expected.CallCredits != nil {
				got := []*float64{}
				for _, e := range r.named("ai_llm_response_received") {
					var amount *float64
					if value, ok := e.Properties["credits"]; ok {
						amount = Ptr(pauseSegmentsJSON(t, value).(float64))
					}
					got = append(got, amount)
				}
				if !reflect.DeepEqual(got, tc.Expected.CallCredits) {
					t.Errorf("credits = %v, want %v", pauseSegmentsJSON(t, got), pauseSegmentsJSON(t, tc.Expected.CallCredits))
				}
				for _, amount := range tc.Expected.CallCredits {
					if amount == nil {
						for _, segment := range segments {
							if _, ok := segment.Properties["total_credits"]; ok {
								t.Error("uncredited call has total_credits")
							}
						}
					}
				}
			}
			tools := []map[string]any{}
			for _, e := range r.all() {
				if !strings.HasPrefix(e.Event, "ai_") {
					tools = append(tools, map[string]any{"event": e.Event, "tool_call_id": e.Properties["tool_call_id"], "tool_status": e.Properties["tool_status"]})
				}
			}
			if !reflect.DeepEqual(tools, tc.Expected.ToolEvents) {
				t.Errorf("tool events = %v, want %v", tools, tc.Expected.ToolEvents)
			}
		})
	}
}
