package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"
)

type emitSpanStep struct {
	Kind, Name, Operation, Provider, Model, Error string
	AdvanceMS                                     int64   `json:"advance_ms"`
	AttemptDurations                              []int64 `json:"attempt_durations_ms"`
	Attributes                                    map[string]any
	Steps                                         []emitSpanStep
}
type portableClass struct {
	Text  any `json:"text"`
	Bytes any `json:"bytes,omitempty"`
}
type unsupportedKey struct{}

func materializeSpanFixture(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = materializeSpanFixture(e)
		}
		return out
	case map[string]any:
		switch x["$container"] {
		case "bytes":
			return []byte{1, 2}
		case "bigint":
			return x["value"]
		case "symbol":
			return unsupportedKey{}
		case "set":
			return materializeSpanFixture(x["values"])
		case "class":
			f := materializeSpanFixture(x["fields"]).(map[string]any)
			return portableClass{f["text"], f["bytes"]}
		case "cycle":
			out := materializeSpanFixture(x["value"]).(map[string]any)
			out[x["self_key"].(string)] = out
			return out
		case "map":
			out := map[any]any{}
			for _, entry := range x["entries"].([]any) {
				e := entry.([]any)
				k := materializeSpanFixture(e[0])
				switch k.(type) {
				case map[string]any, []any:
					k = unsupportedKey{}
				}
				out[k] = materializeSpanFixture(e[1])
			}
			return out
		}
		out := map[string]any{}
		for k, e := range x {
			out[k] = materializeSpanFixture(e)
		}
		return out
	}
	return v
}
func decodeSpanFixture(v map[string]any) any {
	if x, ok := v["stringValue"]; ok {
		return x
	}
	if x, ok := v["boolValue"]; ok {
		return x
	}
	if x, ok := v["intValue"]; ok {
		return json.Number(x.(string))
	}
	if x, ok := v["doubleValue"]; ok {
		return x
	}
	if x, ok := v["arrayValue"].(map[string]any); ok {
		out := []any{}
		for _, e := range x["values"].([]map[string]any) {
			out = append(out, decodeSpanFixture(e))
		}
		return out
	}
	if x, ok := v["kvlistValue"].(map[string]any); ok {
		return decodeSpanAttributes(x["values"].([]map[string]any))
	}
	return nil
}
func decodeSpanAttributes(kvs []map[string]any) map[string]any {
	out := map[string]any{}
	for _, kv := range kvs {
		out[kv["key"].(string)] = decodeSpanFixture(kv["value"].(map[string]any))
	}
	return out
}
func spanAttributes(p map[string]any) map[string]any {
	rs := p["otlp"].(map[string]any)["resourceSpans"].([]map[string]any)[0]
	ss := rs["scopeSpans"].([]map[string]any)[0]
	s := ss["spans"].([]map[string]any)[0]
	return decodeSpanAttributes(s["attributes"].([]map[string]any))
}

func TestSharedEmitSpansFixture(t *testing.T) {
	var f struct {
		Scenarios []struct {
			Name   string
			Config struct {
				EmitSpans bool `json:"emit_spans"`
				EmitCalls bool `json:"emit_llm_calls"`
				Capture   bool `json:"capture_content"`
			}
			Clock struct {
				Start int64 `json:"start_ms"`
			}
			IDs struct {
				Traces []string `json:"trace_ids"`
				Spans  []string `json:"span_ids"`
			}
			Steps    []emitSpanStep
			Expected []map[string]any `json:"expected_spans"`
			CallIDs  []any            `json:"expected_call_span_ids"`
		}
	}
	loadFixture(t, "spec/fixtures/emit-spans-v0.1.json", &f)
	if len(f.Scenarios) != 14 {
		t.Fatalf("expected all 14 scenarios, got %d", len(f.Scenarios))
	}
	for _, sc := range f.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			r := setupAI(t, WithEmitSpans(sc.Config.EmitSpans), WithLLMCallEvents(sc.Config.EmitCalls), WithCaptureContent(sc.Config.Capture))
			ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", InteractionID: "fixture-interaction", ConversationID: "fixture-conversation", Classify: Ptr(false)})
			if len(sc.IDs.Traces) > 0 {
				i.sdkTraceID = sc.IDs.Traces[0]
			}
			now := time.UnixMilli(sc.Clock.Start)
			sequence := 0
			responseSequence := 0
			responseIDs := map[string]string{}
			assign := func(s *Span) {
				if s != nil && s.SpanID != "" {
					if sequence >= len(sc.IDs.Spans) {
						t.Fatal("fixture exhausted IDs")
					}
					s.SpanID = sc.IDs.Spans[sequence]
					sequence++
					s.started = now
				}
			}
			var run func(context.Context, emitSpanStep)
			run = func(ctx context.Context, step emitSpanStep) {
				start := now
				if step.Kind == "span" {
					child, s := StartSpan(ctx, SpanOptions{Name: step.Name, Operation: step.Operation, Attributes: func() map[string]any {
						if step.Attributes == nil {
							return nil
						}
						return materializeSpanFixture(step.Attributes).(map[string]any)
					}()})
					assign(s)
					for _, nested := range step.Steps {
						run(child, nested)
					}
					now = now.Add(time.Duration(step.AdvanceMS) * time.Millisecond)
					var err error
					if step.Error != "" {
						err = errors.New(step.Error)
					}
					s.finish(step.Name, step.Operation, "manual", nil, err, false, now)
					s.End(nil)
				} else {
					_, s := beginWrapperSpan(ctx, i, start)
					assign(s)
					ms := step.AdvanceMS
					if len(step.AttemptDurations) > 0 {
						ms = 0
						for _, n := range step.AttemptDurations {
							ms += n
						}
					}
					now = now.Add(time.Duration(ms) * time.Millisecond)
					responseSequence++
					if s != nil {
						responseIDs[s.SpanID] = fmt.Sprintf("response-%d", responseSequence)
					}
					call := LLMCall{Provider: step.Provider, Model: step.Model, InputTokens: 2, OutputTokens: 1, StartedAt: start, EndedAt: now, ResponseID: fmt.Sprintf("response-%d", responseSequence), FinishReason: "stop"}
					if step.Error != "" {
						call = LLMCall{Provider: step.Provider, Model: step.Model, StartedAt: start, EndedAt: now, ErrorType: "api_error", UsageUnknown: true}
					}
					if step.Error == "" {
						call.InputMessages = []GenAIMessage{{Role: "user", Parts: []GenAIPart{{"type": "text", "content": "hello"}}}}
						call.OutputText = "answer"
					}
					i.recordWrapperCall(call, s, 200)
				}
			}
			for _, step := range sc.Steps {
				run(ctx, step)
			}
			i.End(EndOptions{})
			spans := r.named("ai_otel_span")
			if len(spans) != len(sc.Expected) {
				t.Fatalf("spans=%d want=%d", len(spans), len(sc.Expected))
			}
			for n, m := range spans {
				for k, v := range sc.Expected[n] {
					if k == "attributes" {
						got := spanAttributes(m.Properties)
						want := maps.Clone(v.(map[string]any))
						want["user.id"], want["rudderstack.interaction_id"], want["gen_ai.conversation.id"] = "u", "fixture-interaction", "fixture-conversation"
						if want["rudderstack.span_source"] == "wrapper" && sc.Expected[n]["status_code"] != "error" {
							want["gen_ai.response.id"] = responseIDs[m.Properties["span_id"].(string)]
							want["gen_ai.usage.input_tokens"], want["gen_ai.usage.output_tokens"] = 2, 1
							want["gen_ai.response.finish_reasons"] = []string{"stop"}
							if sc.Config.Capture {
								want["gen_ai.input.messages"] = []any{map[string]any{"role": "user", "parts": []any{map[string]any{"type": "text", "content": "hello"}}}}
								want["gen_ai.output.messages"] = []any{map[string]any{"role": "assistant", "parts": []any{map[string]any{"type": "text", "content": "answer"}}}}
							}
						}
						if mustJSON(got) != mustJSON(want) {
							t.Errorf("full attributes: %s want %s", mustJSON(got), mustJSON(want))
						}

					} else if mustJSON(m.Properties[k]) != mustJSON(v) {
						t.Errorf("%s: %s want %s", k, mustJSON(m.Properties[k]), mustJSON(v))
					}
				}
			}
			calls := r.named("ai_llm_response_received")
			ids := []any{}
			for _, c := range calls {
				ids = append(ids, c.Properties["span_id"])
			}
			if mustJSON(ids) != mustJSON(sc.CallIDs) {
				t.Errorf("call span IDs %s want %s", mustJSON(ids), mustJSON(sc.CallIDs))
			}
			if sequence != len(sc.IDs.Spans) {
				t.Errorf("used %d of %d span IDs", sequence, len(sc.IDs.Spans))
			}
		})
	}
}

func TestSharedSDKSpanSnapshotFixture(t *testing.T) {
	var f struct {
		SDKSpanSnapshot struct {
			TraceID  string           `json:"sdk_trace_id"`
			Calls    []map[string]any `json:"pending_calls"`
			Replayed int              `json:"expected_replayed_spans"`
		} `json:"sdk_span_snapshot"`
	}
	loadFixture(t, "spec/fixtures/pause-snapshot-v0.1.json", &f)
	var pending struct{ Snapshot PausedInteraction }
	loadFixture(t, "spec/fixtures/pause-pending-usage-v0.1.json", &pending)
	pending.Snapshot.SDKTraceID = f.SDKSpanSnapshot.TraceID
	for k, v := range f.SDKSpanSnapshot.Calls[0] {
		pending.Snapshot.PendingCalls[0][k] = v
	}
	r := setupAI(t, WithEmitSpans(true), WithLLMCallEvents(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{Resume: &pending.Snapshot, Classify: Ptr(false)})
	i.End(EndOptions{})
	if len(r.named("ai_otel_span")) != f.SDKSpanSnapshot.Replayed {
		t.Fatal("restored spans replayed")
	}
	calls := r.named("ai_llm_response_received")
	if len(calls) == 0 || calls[0].Properties["span_id"] != f.SDKSpanSnapshot.Calls[0]["span_id"] {
		t.Fatal("restored call lost its span ID")
	}
}
