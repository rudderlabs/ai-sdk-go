package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"weak"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
	"github.com/rudderlabs/ai-sdk-go/internal/serializerfixture"
	"github.com/rudderlabs/ai-sdk-go/internal/spanmodel"
)

type serializerFixture = serializerfixture.Case

// This explicit inventory makes a newly added applicable ID fail until it has a driver.
// A list rather than a map literal: gofmt aligns long map literals differently across Go releases.
var serializerCaseIDs = set(
	"P1-request-message-spans-false",
	"P1-request-message-spans-true",
	"P1-hidden-tool-result-spans-false",
	"P1-hidden-tool-result-spans-true",
	"P1-response-spans-false",
	"P1-response-spans-true",
	"P1-tool-arguments-spans-false",
	"P1-tool-arguments-spans-true",
	"P1-tool-event-result-spans-false",
	"P1-tool-event-result-spans-true",
	"P1-responses-custom-tool-input-spans-false",
	"P1-responses-custom-tool-input-spans-true",
	"P1-manual-report-spans-false",
	"P1-manual-report-spans-true",
	"P1-mcp-result-spans-false",
	"P1-mcp-result-spans-true",
	"P1-manual-span-attribute",
	"P1-manual-span-event",
	"P1-manual-span-context-capture-off",
	"P1-inherited-span-context-capture-off",
	"P1-wrapper-span-context-capture-off",
	"P2-throwing-serializer-placeholder",
	"P2-record-call-tool-arguments-spans-false",
	"P2-record-call-tool-arguments-spans-true",
	"P2-record-tool-result-spans-false",
	"P2-record-tool-result-spans-true",
	"P2-responses-custom-tool-input-spans-false",
	"P2-responses-custom-tool-input-spans-true",
	"P2-go-error",
	"P2-go-panic",
	"P2-go-invalid-json",
	"P2-go-text-error",
	"P2-go-text-panic",
	"P2-serializer-reentry",
	"P3-raw-bytes-spans-false",
	"P3-serializer-text-spans-false",
	"P3-raw-bytes-spans-true",
	"P3-serializer-text-spans-true",
	"P3-serializer-text-masked",
	"P3-serializer-redaction-hook",
	"P3-capture-off-no-content",
	"P1-request-message-spans-false-sync-stream",
	"P1-request-message-spans-false-async-nonstream",
	"P1-request-message-spans-false-async-stream",
	"P1-request-message-spans-true-sync-stream",
	"P1-request-message-spans-true-async-nonstream",
	"P1-request-message-spans-true-async-stream",
	"P1-response-spans-false-sync-stream",
	"P1-response-spans-false-async-nonstream",
	"P1-response-spans-false-async-stream",
	"P1-response-spans-true-sync-stream",
	"P1-response-spans-true-async-nonstream",
	"P1-response-spans-true-async-stream",
	"P1-request-anthropic-spans-false",
	"P1-request-anthropic-spans-true",
	"P1-request-google-spans-false",
	"P1-request-google-spans-true",
	"P1-request-bedrock-spans-false",
	"P1-request-bedrock-spans-true",
	"P1-tracing-message-spans-false",
	"P1-tracing-message-spans-true",
	"S2-precedence-pointer-struct-field-spans-false",
	"S2-precedence-pointer-struct-field-spans-true",
	"S2-precedence-slice-element-spans-false",
	"S2-precedence-slice-element-spans-true",
	"S2-precedence-pointer-array-element-spans-false",
	"S2-precedence-pointer-array-element-spans-true",
	"S2-precedence-unaddressable-struct-field-spans-false",
	"S2-precedence-unaddressable-struct-field-spans-true",
	"S2-precedence-unaddressable-array-element-spans-false",
	"S2-precedence-unaddressable-array-element-spans-true",
	"S2-time-time",
	"N1-provider-call-spans-false",
	"N1-provider-call-spans-true",
	"N1-tool-call-spans-false",
	"N1-tool-call-spans-true",
	"N1-tool-result-spans-false",
	"N1-tool-result-spans-true",
	"N1-manual-span-spans-true",
	"N1-tool-result-size-capture-false-spans-false",
	"N1-tool-result-size-capture-true-spans-false",
	"N1-tool-result-size-capture-false-spans-true",
	"N1-tool-result-size-capture-true-spans-true",
	"N1-separate-tool-call-result-provider-reports-spans-false",
	"N1-separate-tool-call-result-provider-reports-spans-true",
	"N2-shared-object-identity",
	"N2-shared-map-identity",
	"N2-shared-slice-identity",
	"N2-go-by-value-slice-field",
	"N2-go-by-value-map-field",
	"N2-go-by-value-function-field",
	"N2-go-by-value-interface-slice-field",
	"N2-cycle-in-return",
	"N3-twins-wrapper-first-spans-false",
	"N3-twins-wrapper-first-spans-true",
	"N3-twins-span-first-spans-false",
	"N3-twins-span-first-spans-true",
	"N3-same-source-repeat",
	"N4-mcp-success-write-success-spans-false",
	"N4-mcp-success-write-success-spans-true",
	"N4-mcp-success-write-failure-spans-false",
	"N4-mcp-success-write-failure-spans-true",
	"N4-mcp-error-content-write-success-spans-false",
	"N4-mcp-error-content-write-success-spans-true",
	"N4-mcp-error-content-write-failure-spans-false",
	"N4-mcp-error-content-write-failure-spans-true",
	"N4-mcp-error-structured-content-write-success-spans-false",
	"N4-mcp-error-structured-content-write-success-spans-true",
	"N4-mcp-error-structured-content-write-failure-spans-false",
	"N4-mcp-error-structured-content-write-failure-spans-true",
	"N4-adapter-go-mcp-sdk",
	"N4-adapter-go-mcp-go",
	"M1-mutate-after-return-spans-false",
	"M2-mutate-between-reports-spans-false",
	"M1-mutate-after-return-spans-true",
	"M2-mutate-between-reports-spans-true",
	"I1-anthropic-sync",
	"I1-anthropic-async",
	"I1-google-sync",
	"I1-google-async",
	"I2-mcp-when",
	"I2-mcp-properties",
	"I2-mcp-classify-tool-error",
	"I3-app-result-sync",
	"I3-app-result-async",
	"I4-exporter-filter",
	"I4-exporter-identity",
	"I4-exporter-extra-properties",
	"C2-prototype-context-limit",
	"C2-prototype-timeout",
	"C2-prototype-connection-error",
	"C2-non-enumerable-fields-context-limit",
	"C2-non-enumerable-fields-timeout",
	"C2-non-enumerable-fields-connection-error",
	"C3-mcp-is-error",
	"C3-mcp-meta-status",
	"C3-mcp-structured-status",
	"C3-tool-successful-false",
	"C4-span-error-type-original",
	"C4-classifier-evaluator-capture-text",
	"C4-classifier-evaluator-capture-off",
	"B1-document-arguments-spans-false",
	"B1-document-arguments-spans-true",
	"B1-document-result-spans-false",
	"B1-document-result-spans-true",
	"B1-document-nested-result-spans-false",
	"B1-document-nested-result-spans-true",
	"B1-document-pointer-result-spans-false",
	"B1-document-pointer-result-spans-true",
	"B1-document-multi-block-result-spans-false",
	"B1-document-multi-block-result-spans-true",
	"B1-document-stream-history-spans-false",
	"B1-document-stream-history-spans-true",
	"B2-callback-map-when",
	"B2-callback-map-properties",
	"B2-callback-struct-when",
	"B2-callback-struct-properties",
	"B2-callback-pointer-when",
	"B2-callback-pointer-properties",
	"B2-callback-typed_container-when",
	"B2-callback-typed_container-properties",
	"H1-input-pointer-spans-false",
	"H1-input-pointer-spans-true",
	"H1-arguments-pointer-spans-false",
	"H1-arguments-pointer-spans-true",
	"H1-arguments-map-spans-false",
	"H1-arguments-map-spans-true",
	"H1-arguments-slice-spans-false",
	"H1-arguments-slice-spans-true",
	"H1-manual-pointer-spans-true",
	"H2-serializer-output-cap-4-spans-false",
	"H2-serializer-output-cap-1000-spans-false",
	"H3-receiver-weak-refs-spans-false",
	"H2-serializer-output-cap-4-spans-true",
	"H2-serializer-output-cap-1000-spans-true",
	"H3-receiver-weak-refs-spans-true",
	"L1-late-google-typed-config-sync",
	"L1-late-google-typed-config-async",
	"L1-late-openai-chat-typed-tool-choice-sync",
	"L1-late-openai-chat-typed-tool-choice-async",
	"L1-late-openai-chat-dict-tool-choice-sync",
	"L1-late-openai-chat-dict-tool-choice-async",
	"L2-manual-span-after-end",
	"L3-pause-in-process",
	"L3-pause-json-restore",
	"L4-pause-from-provider-report",
	"L4-pause-from-tool-result",
	"L4-pause-from-manual-span",
	"L4-pause-from-span-context",
	"L4-pause-from-mcp-result",
	"L4-end-from-serializer",
	"L5-cancelled-openai-chat-spans-false",
	"L5-cancelled-openai-chat-spans-true",
	"L5-cancelled-openai-responses-spans-false",
	"L5-cancelled-openai-responses-spans-true",
	"L5-cancelled-anthropic-spans-false",
	"L5-cancelled-anthropic-spans-true",
	"L5-cancelled-google-spans-false",
	"L5-cancelled-google-spans-true",
	"L5-cancelled-bedrock-spans-false",
	"L5-cancelled-bedrock-spans-true",
	"L4-request-controls-not-captured-early",
	"D1-serializer",
	"D1-failure-null-no-field-walk",
	"D1-raw-bytes-and-serializer-text",
	"D1-per-report-counter",
	"D1-post-write-mcp",
	"D1-base-callback-inputs",
	"D1-known-counter-limits",
	"D1-no-stale-field-walk-claim",
	"M3-request-handoff-sync-nonstream-spans-false",
	"M3-request-handoff-async-nonstream-spans-false",
	"M3-request-handoff-sync-stream-spans-false",
	"M3-request-handoff-async-stream-spans-false",
	"M3-request-handoff-sync-nonstream-spans-true",
	"M3-request-handoff-async-nonstream-spans-true",
	"M3-request-handoff-sync-stream-spans-true",
	"M3-request-handoff-async-stream-spans-true",
	"M4-mcp-delivered-go_sdk-result",
	"M4-mcp-delivered-go_sdk-error",
	"M4-mcp-delivered-go_mcp_go-result",
	"M4-mcp-delivered-go_mcp_go-error",
	"L5-mcp-handler-duration-go_sdk-success",
	"L5-mcp-handler-duration-go_sdk-error",
	"L5-mcp-handler-duration-go_sdk-slow_serializer",
	"L5-mcp-handler-duration-go_mcp_go-success",
	"L5-mcp-handler-duration-go_mcp_go-error",
	"L5-mcp-handler-duration-go_mcp_go-slow_serializer",
)

func TestSerializerCapture(t *testing.T) {
	b, err := os.ReadFile("spec/fixtures/serializer-capture-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []serializerFixture `json:"cases"`
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		if !containsSDK(c.AppliesTo, "go") {
			continue
		}
		t.Run(c.ID, func(t *testing.T) {
			if !serializerCaseIDs[c.ID] {
				t.Fatalf("unknown applicable serializer case %q", c.ID)
			}
			c, err := c.ResolveGo()
			if err != nil {
				t.Fatal(err)
			}
			setupAI(t, WithEmitSpans(c.Config.EmitSpans), WithCaptureContent(c.Config.CaptureContent), WithLLMCallEvents(true), WithRedaction(c.Config.Redact))
			runSerializerFixture(t, c)
		})
	}
}
func containsSDK(values []string, sdk string) bool {
	for _, value := range values {
		if value == sdk {
			return true
		}
	}
	return false
}

type fixtureSerializer struct {
	Fields  any
	Output  any
	Failure string
	Calls   int
	Action  func()
}

func (v *fixtureSerializer) MarshalJSON() ([]byte, error) {
	v.Calls++
	if v.Action != nil {
		v.Action()
	}
	switch v.Failure {
	case "panic":
		panic("serializer")
	case "invalid_json":
		return []byte("{"), nil
	case "error":
		return nil, fmt.Errorf("serializer")
	}
	b, err := json.Marshal(v.Output)
	return []byte(strings.ReplaceAll(string(b), "{n}", fmt.Sprint(v.Calls))), err
}

type fixtureTextSerializer struct {
	Calls int
	Panic bool
}

func (v *fixtureTextSerializer) MarshalText() ([]byte, error) {
	v.Calls++
	if v.Panic {
		panic("serializer")
	}
	return nil, fmt.Errorf("serializer")
}

func fixtureValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		kind, _ := x["$container"].(string)
		switch kind {
		case "hiding_serializer", "counting_serializer", "throwing_serializer":
			failure, _ := x["failure"].(string)
			if kind == "throwing_serializer" && failure == "" {
				failure = "error"
			}
			if strings.HasPrefix(failure, "text_") {
				return &fixtureTextSerializer{Panic: failure == "text_panic"}
			}
			return &fixtureSerializer{Fields: x["fields"], Output: fixtureValue(x["serialized"]), Failure: failure}
		case "bytes":
			var b []byte
			for _, n := range x["values"].([]any) {
				b = append(b, byte(n.(float64)))
			}
			return b
		case "cycle":
			m := fixtureValue(x["value"]).(map[string]any)
			key, _ := x["self_key"].(string)
			m[key] = m
			return m
		}
		if x["binary"] == "bytes" {
			return []byte("HIDDEN")
		}
		m := map[string]any{}
		for k, value := range x {
			m[k] = fixtureValue(value)
		}
		return m
	case []any:
		list := make([]any, len(x))
		for n, value := range x {
			list[n] = fixtureValue(value)
		}
		return list
	}
	return v
}

func fixtureEqual(t *testing.T, got, want any) {
	t.Helper()
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(expected) {
		t.Fatalf("captured=%s want=%s", b, expected)
	}
}

func runSerializerFixture(t *testing.T, c serializerFixture) {
	switch c.Setup["scenario"] {
	case "go_marshaler_precedence":
		v := review3258Dual{Secret: "PRIVATE_CUSTOMER_VALUE"}
		var value any
		switch c.Setup["position"] {
		case "pointer_struct_field":
			value = &struct{ Child review3258Dual }{v}
		case "slice_element":
			value = []review3258Dual{v}
		case "pointer_array_element":
			value = &[1]review3258Dual{v}
		case "unaddressable_struct_field":
			value = struct{ Child review3258Dual }{v}
		case "unaddressable_array_element":
			value = [1]review3258Dual{v}
		default:
			t.Fatal("unknown precedence position")
		}
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var expected any
		if err = json.Unmarshal(b, &expected); err != nil {
			t.Fatal(err)
		}
		fixtureEqual(t, captureValue(value).value, expected)
	case "go_time":
		v, err := time.Parse(time.RFC3339Nano, c.Setup["rfc3339"].(string))
		if err != nil {
			t.Fatal(err)
		}
		fixtureEqual(t, captureValue(v).value, c.Expect["decoded"].(map[string]any)["captured_value"])
	case "shared_value":
		v := fixtureValue(c.Setup["value"])
		counter := v.(*fixtureSerializer)
		if c.Setup["receiver"] == "by_value" {
			v = fixtureByValue{counter, []string{"hidden"}, map[string]bool{"hidden": true}, func() {}, []any{"hidden"}}
		} else if c.Setup["shape"] == "map" {
			v = fixtureMap{"value": counter}
		} else if c.Setup["shape"] == "slice" {
			v = fixtureSlice{counter}
		}
		occurrences := int(c.Setup["occurrences"].(float64))
		values := make([]any, occurrences)
		for n := range values {
			values[n] = v
		}
		fixtureEqual(t, captureValue(values).value, c.Expect["decoded"].(map[string]any)["occurrences"])
	default:
		runSerializerIntegration(t, c)
	}
}

type fixtureByValue struct {
	Serializer *fixtureSerializer
	Slice      []string
	Map        map[string]bool
	Function   func()
	Interface  any
}

func (v fixtureByValue) MarshalJSON() ([]byte, error) { return v.Serializer.MarshalJSON() }

type fixtureMap map[string]*fixtureSerializer

func (v fixtureMap) MarshalJSON() ([]byte, error) { return v["value"].MarshalJSON() }

type fixtureSlice []*fixtureSerializer

func (v fixtureSlice) MarshalJSON() ([]byte, error) { return v[0].MarshalJSON() }

func fixtureCall(value any) LLMCall {
	return LLMCall{Provider: "openai", Model: "m", OutputToolCalls: []GenAIToolCall{{Name: "lookup", Arguments: value}}}
}
func fixtureCalls(value any) int {
	switch v := value.(type) {
	case *fixtureSerializer:
		return v.Calls
	case *fixtureTextSerializer:
		return v.Calls
	}
	return 0
}
func fixtureWire(value any) any {
	b, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var v any
	_ = json.Unmarshal(b, &v)
	return v
}
func fixtureNever(t *testing.T, c serializerFixture, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if forbidden, ok := c.Expect["never_emitted"].([]any); ok {
		for _, word := range forbidden {
			if strings.Contains(string(b), word.(string)) {
				t.Fatalf("emitted %q: %s", word, b)
			}
		}
	}
}
func runSerializerIntegration(t *testing.T, c serializerFixture) {
	value := fixtureValue(c.Setup["value"])
	if c.Setup["scenario"] == "manual_span" {
		value = fixtureValue(c.Setup["attributes"].(map[string]any)["input.value"])
	}
	scenario := c.Setup["scenario"].(string)
	switch scenario {
	case "capture_site", "manual_span", "late_manual_span", "redaction_hook":
		runSerializerSite(t, c, value)
	case "serializer_reentry":
		v := value.(*fixtureSerializer)
		var nested any
		v.Action = func() { nested = captureValue(map[string]any{"value": v}).value.(map[string]any)["value"] }
		got := captureValue(v).value
		fixtureEqual(t, got, c.Expect["decoded"].(map[string]any)["outer"])
		fixtureEqual(t, nested, nil)
		if v.Calls != 1 {
			t.Fatal("recursive serializer ran again")
		}
	case "tool_result_size", "separate_tool_reports":
		runSerializerTool(t, c, value)
	case "report_sinks":
		switch c.Setup["report"] {
		case "tool_call", "tool_result":
			runSerializerTool(t, c, value)
		case "manual_span":
			attrs := map[string]any{"input.value": value}
			captured := captureValue(map[string]any{"attributes": attrs, "event": attrs}).value.(map[string]any)
			track, err := spanmodel.ToTrack(spanmodel.Span{Name: "lookup", Start: time.Now(), End: time.Now(), Attributes: captured["attributes"].(map[string]any), Events: []spanmodel.Event{{Name: "gen_ai.content", Time: time.Now(), Attributes: captured["event"].(map[string]any)}}}, spanmodel.Options{})
			if err != nil || track == nil {
				t.Fatal(err)
			}
			fixtureNever(t, c, track.Properties)
			if fixtureCalls(value) != 1 {
				t.Fatal("repeated span serialization")
			}
		default:
			rec := cfg().analytics.(*recorder)
			ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			ctx, s := beginWrapperSpan(ctx, i, time.Now())
			_ = ctx
			call := fixtureCall(value)
			call.wrapperSpan = s
			i.RecordCall(call)
			snapshot, ok := i.Pause(PauseOptions{})
			if !ok {
				t.Fatal("pause refused")
			}
			fixtureNever(t, c, snapshot)
			i.Resume()
			i.End(EndOptions{})
			fixtureNever(t, c, rec.named("ai_llm_response_received"))
			fixtureNever(t, c, rec.named("ai_otel_span"))
			if fixtureCalls(value) != 1 {
				t.Fatal("report sinks ran serializer again")
			}
		}
	case "twin_reports", "repeat_reports":
		rec := cfg().analytics.(*recorder)
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
		call := fixtureCall(value)
		call.ResponseID = "same"
		call.Integration = "openai"
		if c.Setup["order"] == "span_first" {
			call.Integration = "tracing"
		}
		i.RecordCall(call)
		if scenario == "twin_reports" {
			if call.Integration == "tracing" {
				call.Integration = "openai"
			} else {
				call.Integration = "tracing"
			}
		}
		i.RecordCall(call)
		i.End(EndOptions{})
		if fixtureCalls(value) != 2 || len(rec.named("ai_llm_response_received")) != 1 {
			t.Fatal("twin/repeat count mismatch")
		}
		fixtureNever(t, c, rec.named("ai_llm_response_received"))
	case "mutation":
		runSerializerMutation(t, c, value)
	case "mcp_write", "mcp_callback_inputs", "mcp_classification":
		runSerializerMCP(t, c, value)
	case "mcp_delivered", "mcp_handler_duration":
		if c.Setup["adapter"] == "go_mcp_go" {
			t.Logf("case executed by ruddermcpgo/TestSerializerCapture/%s", c.ID)
		} else if scenario == "mcp_delivered" {
			runSerializerDelivered(t, c)
		} else {
			runSerializerHandlerDuration(t, c)
		}
	case "request_handoff":
		t.Logf("case executed at the provider boundary by rudderopenai/TestSerializerCapture/%s", c.ID)
	case "serializer_pause", "serializer_end":
		runSerializerAction(t, c, value)
	case "receiver_heap", "serializer_output_heap":
		runSerializerHeap(t, c)
	case "pause_restore":
		runSerializerPause(t, c, value)
	case "native_error_classification":
		errorClassifiers.mu.Lock()
		previous := errorClassifiers.fns
		errorClassifiers.mu.Unlock()
		t.Cleanup(func() { errorClassifiers.mu.Lock(); errorClassifiers.fns = previous; errorClassifiers.mu.Unlock() })
		err := &fixtureNativeError{category: c.Setup["category"].(string)}
		hook.RegisterErrorClassifier(func(original error) (string, bool) {
			if original == err {
				return err.category, true
			}
			return "", false
		})
		if got, _ := classifyError(err); got != err.category {
			t.Fatalf("classification=%q", got)
		}
		if err.Calls != 0 {
			t.Fatal("classification invoked serializer")
		}
	case "function_tool_classification":
		// Core tool accounting takes its failure signal from the original report, not capture.
		rec := cfg().analytics.(*recorder)
		_ = Configure(WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "looked_up"}}}))
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
		i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup"})
		i.RecordToolResult("t", ToolResult{Result: value, Error: Ptr(true), ErrorType: "api_error"})
		i.End(EndOptions{})
		if rec.props(t, "looked_up")["tool_status"] != "error" {
			t.Fatal("serializer altered failure signal")
		}
	case "turn_consumers":
		// Provider inputs in Go are wire-decoded. Turn text uses the same wire projection.
		v := fixtureWire(value).(map[string]any)
		text := v["text"].(string)
		classified := make(chan string, 1)
		evaluated := make(chan string, 1)
		_ = Configure(WithClassifier(&ClassifierOptions{Classify: func(_ context.Context, turn ClassifyTurn) (ClassifyResult, error) {
			classified <- turn.Answer
			return ClassifyResult{}, nil
		}}), WithEvaluator(func(_ context.Context, turn EvaluationTurn) ([]Score, error) {
			evaluated <- turn.Answer
			if turn.Answer != text {
				t.Errorf("evaluator answer=%q", turn.Answer)
			}
			return nil, nil
		}))
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(true)})
		call := fixtureCall(nil)
		call.OutputText = text
		i.RecordCall(call)
		i.mu.Lock()
		answer := i.answer
		i.mu.Unlock()
		if answer != text {
			t.Fatalf("turn text=%q", answer)
		}
		i.End(EndOptions{})
		if !FlushTimeout(5 * time.Second) {
			t.Fatal("turn work did not finish")
		}
		for _, ch := range []chan string{classified, evaluated} {
			select {
			case got := <-ch:
				fixtureEqual(t, got, text)
			default:
				t.Fatal("consumer was not called")
			}
		}
	case "app_result_identity":
		t.Logf("case executed at the provider boundary by rudderopenai/TestSerializerCapture/%s", c.ID)
	case "provider_tool_rules":
		rec := cfg().analytics.(*recorder)
		args := c.Setup["arguments"].(map[string]any)
		result := fixtureWire(fixtureValue(c.Setup["result"])).(map[string]any)
		seen := false
		_ = Configure(WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "lookup_done", When: func(a map[string]any, r any) bool {
			seen = reflect.ValueOf(a).Pointer() == reflect.ValueOf(args).Pointer() && reflect.ValueOf(r).Pointer() == reflect.ValueOf(result).Pointer()
			return seen
		}, Properties: func(_ map[string]any, r any) map[string]any { return r.(map[string]any) }}}}))
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
		i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup", Args: args})
		i.RecordToolResult("t", ToolResult{Result: result, Error: Ptr(false)})
		i.End(EndOptions{})
		if !seen || len(rec.named("lookup_done")) != 1 {
			t.Fatal("base wire rule inputs changed")
		}
		fixtureEqual(t, rec.props(t, "lookup_done")["order"], "order-1")
	case "bedrock_document", "bedrock_callback_shapes":
		t.Logf("case executed by rudderbedrock/TestSerializerCapture/%s", c.ID)
	case "exporter_hook":
		t.Logf("case executed by rudderotel/TestSerializerCapture/%s", c.ID)
	case "late_provider_response", "cancelled_stream", "request_control_timing":
		runSerializerLifecycle(t, c)
	case "documentation_contract":
		runSerializerDocs(t, c)
	default:
		t.Fatalf("unknown scenario %q", scenario)
	}
}

type fixtureNativeError struct {
	category string
	Calls    int
}

func (e *fixtureNativeError) Error() string                { return "original error" }
func (e *fixtureNativeError) ErrorCode() string            { return e.category }
func (e *fixtureNativeError) MarshalJSON() ([]byte, error) { e.Calls++; return []byte("{}"), nil }

func runSerializerSite(t *testing.T, c serializerFixture, value any) {
	if strings.HasPrefix(c.ID, "P1-") && (c.Setup["site"] == "provider_request_message" || c.Setup["site"] == "provider_response_message") {
		t.Logf("case executed at HTTP/SSE boundary by internal/kit/TestSerializerCapture/%s", c.ID)
		return
	}
	rec := cfg().analytics.(*recorder)
	site, _ := c.Setup["site"].(string)
	if site == "" {
		site = "manual_span_attribute"
	}
	var got any
	if c.Setup["scenario"] == "redaction_hook" {
		var inputs []string
		_ = Configure(WithRedactHook(func(s string) (string, error) {
			inputs = append(inputs, s)
			return c.Setup["hook_return"].(string), nil
		}))
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
		i.RecordCall(fixtureCall(value))
		i.End(EndOptions{})
		got = rec.props(t, "ai_llm_response_received")["output_tool_calls"].([]any)[0].(map[string]any)["arguments"]
		fixtureEqual(t, inputs, c.Expect["hook_inputs"])
	} else if strings.Contains(site, "span_context") {
		ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false), Context: map[string]any{"custom": value}})
		var span *Span
		if site == "wrapper_span_context" {
			_, span = beginWrapperSpan(ctx, i, time.Now())
			call := fixtureCall(nil)
			call.wrapperSpan = span
			i.RecordCall(call)
		} else {
			_, span = StartSpan(ctx, SpanOptions{Name: "lookup", Operation: "execute_tool"})
			span.End(nil)
		}
		got = span.msgContext["custom"]
		i.End(EndOptions{})
	} else if strings.Contains(site, "span") || c.Setup["scenario"] == "late_manual_span" {
		if site == "manual_span_event" {
			attrs := captureValue(map[string]any{"input.value": value}).value.(map[string]any)
			track, err := spanmodel.ToTrack(spanmodel.Span{Name: "lookup", Start: time.Now(), End: time.Now(), Events: []spanmodel.Event{{Name: "gen_ai.content", Time: time.Now(), Attributes: attrs}}}, spanmodel.Options{})
			if err != nil || track == nil {
				t.Fatal(err)
			}
			got = attrs["input.value"]
			fixtureNever(t, c, track.Properties)
		} else {
			ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			_, span := StartSpan(ctx, SpanOptions{Name: "lookup", Operation: "execute_tool", Attributes: map[string]any{"input.value": value}})
			if c.Setup["scenario"] == "late_manual_span" {
				i.End(EndOptions{})
			}
			span.End(nil)
			i.End(EndOptions{})
			events := rec.named("ai_otel_span")
			if len(events) != 1 {
				t.Fatalf("spans=%d", len(events))
			}
			got = spanAttributes(events[0].Properties)["input.value"]
		}
	} else if site == "record_tool_result" || site == "record_tool_arguments" {
		_ = Configure(WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "looked_up", Properties: func(args map[string]any, result any) map[string]any {
			if site == "record_tool_arguments" {
				return map[string]any{"input": args["value"]}
			}
			return map[string]any{"output": result}
		}}}}))
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
		args := map[string]any{}
		if site == "record_tool_arguments" {
			args["value"] = value
		}
		i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup", Args: args})
		var result any = "done"
		if site == "record_tool_result" {
			result = value
		}
		i.RecordToolResult("t", ToolResult{Result: result, Error: Ptr(false)})
		i.End(EndOptions{})
		key := "output"
		if site == "record_tool_arguments" {
			key = "input"
		}
		got = rec.props(t, "looked_up")[key]
	} else if site == "mcp_result" {
		runSerializerMCP(t, c, value)
		return
	} else {
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
		call := fixtureCall(value)
		switch site {
		case "provider_request_message", "provider_response_message":
			// Go providers pass wire-decoded maps to projections. Serializer invocation here models
			// the application's provider request encoding, outside SDK telemetry capture.
			wire, _ := fixtureWire(value).(map[string]any)
			role, _ := wire["role"].(string)
			text, _ := wire["content"].(string)
			call = fixtureCall(nil)
			if site == "provider_response_message" {
				call.OutputText = text
			} else {
				call.InputMessages = []GenAIMessage{{Role: role, Parts: []GenAIPart{{"type": "text", "content": text}}}}
			}
		case "provider_history_tool_result":
			call.OutputToolCalls = nil
			call.InputMessages = []GenAIMessage{{Role: "tool", Parts: []GenAIPart{{"type": "tool_call_response", "response": value}}}}
		}
		i.RecordCall(call)
		i.End(EndOptions{})
		events := rec.named("ai_llm_response_received")
		if len(events) != 1 {
			t.Fatal("missing call")
		}
		p := events[0].Properties
		switch site {
		case "provider_request_message":
			if messages, ok := p["input_messages"].([]any); ok && len(messages) > 0 {
				got = messages[0]
			}
		case "provider_response_message":
			got = map[string]any{"role": "assistant", "parts": []any{map[string]any{"type": "text", "content": p["output_text"]}}}
		case "provider_history_tool_result":
			got = p["input_messages"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["response"]
		default:
			if calls, ok := p["output_tool_calls"].([]any); ok && len(calls) > 0 {
				got = calls[0].(map[string]any)["arguments"]
			}
		}
	}
	if decoded, ok := c.Expect["decoded"].(map[string]any); ok {
		want, has := decoded["captured_value"]
		if !has {
			want, has = decoded["context.custom"]
		}
		if !has {
			want, has = decoded["attributes"].(map[string]any)["input.value"]
		}
		if has {
			fixtureEqual(t, got, want)
		}
	}
	fixtureNever(t, c, rec.named("ai_llm_response_received"))
	fixtureNever(t, c, rec.named("ai_otel_span"))
	fixtureNever(t, c, rec.named("looked_up"))
	if calls, ok := c.Expect["serializer_calls"].(map[string]any); ok {
		if want, ok := calls["value"].(float64); ok && fixtureCalls(value) != int(want) {
			t.Fatalf("serializer calls=%d want=%v", fixtureCalls(value), want)
		}
	}
}

func runSerializerTool(t *testing.T, c serializerFixture, value any) {
	rec := cfg().analytics.(*recorder)
	_ = Configure(WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "looked_up", Properties: func(args map[string]any, result any) map[string]any {
		return map[string]any{"input": args, "output": result}
	}}}}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	args := map[string]any{}
	result := value
	report, _ := c.Setup["report"].(string)
	if report == "tool_call" || c.Setup["scenario"] == "separate_tool_reports" {
		args["value"] = value
	}
	if report == "tool_call" {
		result = "done"
	}
	i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup", Args: args})
	i.RecordToolResult("t", ToolResult{Result: result, Error: Ptr(false)})
	if c.Setup["scenario"] == "separate_tool_reports" {
		i.RecordCall(fixtureCall(value))
	}
	snapshot, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("pause refused")
	}
	i.Resume()
	i.End(EndOptions{})
	event := rec.props(t, "looked_up")
	if want, ok := c.Expect["result_size"].(float64); ok && event["result_size_bytes"] != int(want) {
		t.Fatalf("size=%v want=%v", event["result_size_bytes"], want)
	}
	if calls, ok := c.Expect["serializer_calls"].(map[string]any); ok {
		if want, ok := calls["value"].(float64); ok && fixtureCalls(value) != int(want) {
			t.Fatalf("calls=%d want=%v", fixtureCalls(value), want)
		}
	}
	if c.Setup["scenario"] == "separate_tool_reports" {
		fixtureEqual(t, event["input"].(map[string]any)["value"], map[string]any{"public": "capture-1"})
		fixtureEqual(t, event["output"], map[string]any{"public": "capture-2"})
		fixtureEqual(t, rec.props(t, "ai_llm_response_received")["output_tool_calls"].([]any)[0].(map[string]any)["arguments"], map[string]any{"public": "capture-3"})
	}
	fixtureNever(t, c, event)
	fixtureNever(t, c, snapshot)
}

func runSerializerMutation(t *testing.T, c serializerFixture, value any) {
	rec := cfg().analytics.(*recorder)
	v := value.(*fixtureSerializer)
	v.Output = map[string]any{"text": "before"}
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	call := fixtureCall(v)
	call.ResponseID = "first"
	_, call.wrapperSpan = beginWrapperSpan(ctx, i, time.Now())
	i.RecordCall(call)
	v.Fields = map[string]any{"text": "after"}
	v.Output = map[string]any{"text": "after"}
	if strings.HasPrefix(c.ID, "M2") {
		call.ResponseID = "second"
		_, call.wrapperSpan = beginWrapperSpan(ctx, i, time.Now())
		i.RecordCall(call)
	}
	i.End(EndOptions{})
	calls := rec.named("ai_llm_response_received")
	decoded := c.Expect["decoded"].(map[string]any)
	expected := []any{decoded["call_input"]}
	if inputs, ok := decoded["call_inputs"].([]any); ok {
		expected = inputs
	}
	if len(calls) != len(expected) {
		t.Fatalf("call events=%d want=%d", len(calls), len(expected))
	}
	for n, event := range calls {
		fixtureEqual(t, event.Properties["output_tool_calls"].([]any)[0].(map[string]any)["arguments"], expected[n])
	}
	if counts, ok := c.Expect["event_counts"].(map[string]any); ok {
		for name, want := range counts {
			fixtureEqual(t, len(rec.named(name)), want)
		}
	}
	fixtureEqual(t, fixtureCalls(value), c.Expect["serializer_calls"].(map[string]any)["value"])
	fixtureNever(t, c, calls)
	fixtureNever(t, c, rec.named("ai_otel_span"))
}

func runSerializerMCP(t *testing.T, c serializerFixture, value any) {
	if c.Setup["adapter"] == "go_mcp_go" {
		t.Logf("case executed by ruddermcpgo/TestSerializerCapture/%s", c.ID)
		return
	}
	setupAI(t, WithEmitSpans(c.Config.EmitSpans))
	s := workspaceServer("fixture-mcp", nil)
	rec := wrap(t, s, Options{CaptureContent: c.Config.CaptureContent, EventRules: EventRules{Tools: map[string]ToolEventRule{"list_destinations": {Event: "Lookup", Properties: func(_ map[string]any, result any) map[string]any { return map[string]any{"output": result} }}}}})
	w, _ := instances.get(s)
	fail := c.Setup["write"] == "failure"
	out := io.Writer(io.Discard)
	if fail {
		out = fixtureFailWriter{}
	}
	responses := mcpbridge.NewResponses()
	var writer io.Writer = mcpbridge.NewWriter(out, responses)
	if c.Setup["adapter"] == "go_mcp_sdk" {
		ctx, transport := ObserveMCPTransport(context.Background(), &mcp.IOTransport{Reader: io.NopCloser(strings.NewReader("")), Writer: nopCloser{out}})
		responses = mcpbridge.ResponsesFrom(ctx)
		writer = transport.(*mcp.IOTransport).Writer
	}
	defer responses.Close()
	rc := w.begin(mcpbridge.Request{Ctx: context.Background(), RequestID: 1, Responses: responses, RegisteredTools: []mcpbridge.Tool{{Name: "list_destinations", Schema: map[string]any{"type": "object"}}}})
	rc.ToolCall("list_destinations", nil, false, nil, "")
	var result any = map[string]any{"structuredContent": value, "isError": strings.HasPrefix(fmt.Sprint(c.Setup["status"]), "error")}
	if c.Setup["scenario"] == "mcp_classification" {
		signal := c.Setup["signal"]
		result = map[string]any{"isError": true}
		if signal == "meta_status" {
			result.(map[string]any)["_meta"] = map[string]any{"com.rudderstack/http_status": 400}
		}
		if signal == "structured_status" {
			result.(map[string]any)["structuredContent"] = &fixtureSerializer{Fields: map[string]any{"com.rudderstack/http_status": 400}, Output: map[string]any{}}
		}
	}
	rc.ToolDone(mcpbridge.Outcome{Result: result})
	if value != nil && fixtureCalls(value) != 0 {
		t.Fatal("MCP serializer before write")
	}
	wireResult := result
	if v, ok := value.(*fixtureSerializer); ok && fail {
		// The library encodes the result before the write fails; that run is not the SDK's.
		twin := *v
		wireResult = map[string]any{"structuredContent": &twin, "isError": result.(map[string]any)["isError"]}
	}
	wire, err := json.Marshal(map[string]any{"id": 1, "result": wireResult})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(append(wire, '\n')); (err != nil) != fail {
		t.Fatal(err)
	}
	if value != nil {
		want := 1
		if fail {
			want = 0
		}
		if fixtureCalls(value) != want {
			t.Fatalf("MCP calls=%d want=%d", fixtureCalls(value), want)
		}
	}
	events := rec.named("mcp_tool_called")
	if len(events) != 1 {
		t.Fatal("missing MCP event")
	}
	fixtureNever(t, c, events)
	fixtureNever(t, c, rec.named("Lookup"))
	if c.Setup["scenario"] == "mcp_classification" {
		fixtureEqual(t, events[0].Properties["error_type"], c.Expect["error_type"])
	}
	if !fail && value != nil && c.Config.CaptureContent {
		if decoded, ok := c.Expect["decoded"].(map[string]any); ok {
			want, has := decoded["captured_value"]
			if !has {
				want, has = decoded["mcp_result"]
			}
			if has {
				fixtureEqual(t, rec.props(t, "Lookup")["output"], want)
			}
		}
	}
}

type fixtureFailWriter struct{}

func (fixtureFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func runSerializerAction(t *testing.T, c serializerFixture, value any) {
	rec := cfg().analytics.(*recorder)
	_ = rec
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	v := value.(*fixtureSerializer)
	accepted := false
	v.Action = func() {
		if c.Setup["scenario"] == "serializer_end" {
			i.End(EndOptions{})
			accepted = i.Ended()
		} else {
			_, accepted = i.Pause(PauseOptions{})
		}
	}
	switch c.Setup["site"] {
	case "tool_result":
		_ = Configure(WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "looked_up"}}}))
		i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup"})
		i.RecordToolResult("t", ToolResult{Result: v, Error: Ptr(false)})
	case "manual_span":
		_, s := StartSpan(ctx, SpanOptions{Name: "lookup", Operation: "execute_tool", Attributes: map[string]any{"input.value": v}})
		s.End(nil)
	case "span_context":
		i.msgContext = map[string]any{"custom": v}
		_, s := StartSpan(ctx, SpanOptions{Name: "lookup", Operation: "execute_tool"})
		s.End(nil)
	case "mcp_result":
		s := workspaceServer("action-mcp", nil)
		wrap(t, s, Options{})
		w, _ := instances.get(s)
		rc := w.begin(mcpbridge.Request{Ctx: ctx, RequestID: 1})
		rc.ToolCall("list_destinations", nil, false, nil, "")
		rc.ToolDone(mcpbridge.Outcome{Result: map[string]any{"structuredContent": v}})
	default:
		i.RecordCall(fixtureCall(v))
	}
	if accepted || v.Calls != 1 {
		t.Fatalf("accepted=%v calls=%d", accepted, v.Calls)
	}
	i.End(EndOptions{})
}

type fixtureOutputBuffer struct{ Output weak.Pointer[byte] }

func (v *fixtureOutputBuffer) MarshalJSON() ([]byte, error) {
	b := make([]byte, 1<<20)
	for n := range b {
		b[n] = ' '
	}
	copy(b, `{"visible":true}`)
	v.Output = weak.Make(&b[0])
	return b, nil
}
func fixtureGC() {
	for n := 0; n < 8; n++ {
		runtime.GC()
	}
}
func runSerializerHeap(t *testing.T, c serializerFixture) {
	_ = Configure(WithLimits(Limits{MaxCallsPerInteraction: 1}))
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	if c.Setup["scenario"] == "serializer_output_heap" {
		var values []*fixtureOutputBuffer
		for n := 0; n < 16; n++ {
			v := &fixtureOutputBuffer{}
			values = append(values, v)
			call := fixtureCall(v)
			call.ResponseID = fmt.Sprint(n)
			i.RecordCall(call)
		}
		fixtureGC()
		for _, v := range values {
			if v.Output.Value() != nil {
				t.Fatal("serializer output retained before End")
			}
		}
		i.End(EndOptions{})
		fixtureGC()
		for _, v := range values {
			if v.Output.Value() != nil {
				t.Fatal("serializer output retained after End")
			}
		}
		runtime.KeepAlive(values)
		return
	}
	keep := c.Setup["keep_app_values"] == true
	var values []*fixtureSerializer
	var refs []weak.Pointer[fixtureSerializer]
	reports := int(c.Setup["reports"].(float64))
	// Allocate app-owned buffers before the baseline when the app keeps them.
	if keep {
		for n := 0; n < reports; n++ {
			values = append(values, &fixtureSerializer{Fields: make([]byte, 4<<20), Output: map[string]any{"visible": true}})
		}
	}
	fixtureGC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for n := 0; n < reports; n++ {
		func() {
			var v *fixtureSerializer
			if keep {
				v = values[n]
			} else {
				v = &fixtureSerializer{Fields: make([]byte, 4<<20), Output: map[string]any{"visible": "small"}}
			}
			refs = append(refs, weak.Make(v))
			var value any = v
			site, _ := c.Setup["site"].(string)
			if site == "arguments_map" {
				value = fixtureMap{"value": v}
			}
			if site == "arguments_slice" {
				value = fixtureSlice{v}
			}
			call := fixtureCall(value)
			call.ResponseID = fmt.Sprint(n)
			if site == "input_pointer" {
				call.OutputToolCalls = nil
				call.InputMessages = []GenAIMessage{{Role: "tool", Parts: []GenAIPart{{"type": "tool_call_response", "response": v}}}}
			}
			if site == "manual_pointer" {
				_, s := StartSpan(ctx, SpanOptions{Name: "lookup", Operation: "execute_tool", Attributes: map[string]any{"input.value": v}})
				s.End(nil)
			} else {
				i.RecordCall(call)
			}
		}()
	}
	for _, ended := range []bool{false, true} {
		if ended {
			i.End(EndOptions{})
		}
		fixtureGC()
		runtime.ReadMemStats(&after)
		growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
		limit := int64(16 << 20)
		if !keep {
			limit = 2 << 20
			for _, ref := range refs {
				if ref.Value() != nil {
					t.Fatal("receiver retained before/after End")
				}
			}
		}
		t.Logf("ended=%v heap growth %.2f MiB", ended, float64(growth)/(1<<20))
		if growth >= limit {
			t.Fatalf("growth=%d limit=%d", growth, limit)
		}
	}
	runtime.KeepAlive(i)
	runtime.KeepAlive(values)
}

func runSerializerPause(t *testing.T, c serializerFixture, value any) {
	rec := cfg().analytics.(*recorder)
	ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
	_ = ctx
	pending := fixtureValue(c.Setup["pending_tool_value"])
	_ = Configure(WithEventRules(EventRules{Tools: map[string]ToolEventRule{"pending": {Event: "restored_tool", Properties: func(_ map[string]any, v any) map[string]any { return map[string]any{"custom": v} }}}}))
	i.RecordCall(fixtureCall(value))
	i.RecordToolCalls(ToolCall{ID: "pending", Name: "pending"})
	_, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("pause refused")
	}
	i.RecordToolResult("pending", ToolResult{Result: pending, Error: Ptr(false)})
	snapshot, ok := i.Pause(PauseOptions{})
	if !ok {
		t.Fatal("snapshot refresh refused")
	}
	if len(rec.named("restored_tool")) != 0 {
		t.Fatal("pending tool sent before resume")
	}
	if v, ok := pending.(*fixtureSerializer); ok {
		v.Output = map[string]any{"public": "mutated"}
	}
	before := fixtureCalls(value)
	if c.Setup["handoff"] == "json_restore" {
		b, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var restored PausedInteraction
		if err = json.Unmarshal(b, &restored); err != nil {
			t.Fatal(err)
		}
		_, i = StartInteraction(context.Background(), InteractionOptions{UserID: "u", Resume: &restored, Classify: Ptr(false)})
	} else {
		i.Resume()
	}
	i.End(EndOptions{})
	events := rec.named("ai_llm_response_received")
	if len(events) != 1 {
		t.Fatalf("call events=%d want=1", len(events))
	}
	fixtureEqual(t, events[0].Properties["output_tool_calls"].([]any)[0].(map[string]any)["arguments"], c.Expect["decoded"].(map[string]any)["before_pause"])
	tools := rec.named("restored_tool")
	if len(tools) != 1 {
		t.Fatalf("restored tools=%d want=1", len(tools))
	}
	fixtureEqual(t, tools[0].Properties["custom"], c.Expect["decoded"].(map[string]any)["after_resume"])
	if fixtureCalls(pending) != 1 {
		t.Fatalf("pending serializer calls=%d", fixtureCalls(pending))
	}
	if fixtureCalls(value) != before {
		t.Fatal("restore serialized app content")
	}
	fixtureNever(t, c, rec.named("ai_llm_response_received"))
	fixtureNever(t, c, snapshot)
}

func runSerializerLifecycle(t *testing.T, c serializerFixture) {
	switch c.Setup["scenario"] {
	case "request_control_timing":
		t.Logf("case executed at the provider boundary by rudderopenai/TestSerializerCapture/%s", c.ID)
	case "late_provider_response":
		rec := cfg().analytics.(*recorder)
		ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
		_, s := beginWrapperSpan(ctx, i, time.Now())
		i.End(EndOptions{})
		call := LLMCall{Provider: "openai", Model: "m", ResponseID: c.Setup["response_id"].(string), OutputText: c.Setup["answer"].(string), InputTokens: 3, OutputTokens: 2, FinishReason: "stop", wrapperSpan: s}
		i.RecordCall(call)
		spans := rec.named("ai_otel_span")
		if len(spans) != 1 {
			t.Fatal("late wrapper dropped")
		}
		decoded := c.Expect["decoded"].(map[string]any)
		attrs := spanAttributes(spans[0].Properties)
		fixtureEqual(t, attrs["gen_ai.output.messages"], []any{map[string]any{"role": "assistant", "parts": []any{map[string]any{"type": "text", "content": decoded["span_output_text"]}}}})
		fixtureEqual(t, attrs["gen_ai.response.id"], decoded["response_id"])
		fixtureEqual(t, attrs["gen_ai.response.finish_reasons"], decoded["finish_reasons"])
		fixtureEqual(t, attrs["gen_ai.usage.input_tokens"], decoded["input_tokens"])
		fixtureEqual(t, attrs["gen_ai.usage.output_tokens"], decoded["output_tokens"])
		for name, want := range c.Expect["event_counts"].(map[string]any) {
			fixtureEqual(t, len(rec.named(name)), want)
		}
	case "cancelled_stream":
		rec := cfg().analytics.(*recorder)
		_ = Configure(WithCredits(func(CreditCall) (*float64, error) { return Ptr(7.0), nil }))
		ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
		_, s := beginWrapperSpan(ctx, i, time.Now())
		call := LLMCall{Provider: "openai", Model: "m", ErrorType: "cancelled", UsageUnknown: true, OutputText: "partial", wrapperSpan: s}
		i.RecordCall(call)
		i.End(EndOptions{})
		events := rec.named("ai_llm_response_received")
		if len(events) != 1 || events[0].Properties["credits"] != nil || rec.props(t, "ai_response_received")["total_credits"] != nil {
			t.Fatal("cancelled call billed or duplicated")
		}
	}
}

// perGo returns the Go entry of a per-SDK setup map, else the shared default.
func perGo(c serializerFixture, by, shared string) []any {
	if m, ok := c.Setup[by].(map[string]any); ok {
		if list, ok := m["go"].([]any); ok {
			return list
		}
	}
	list, _ := c.Setup[shared].([]any)
	return list
}

func readFixtureDocs(t *testing.T, name string, into map[string]string) {
	t.Helper()
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		into[name] = string(b)
		return
	}
	entries, err := os.ReadDir(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		readFixtureDocs(t, strings.TrimSuffix(name, "/")+"/"+entry.Name(), into)
	}
}

func runSerializerDocs(t *testing.T, c serializerFixture) {
	for _, key := range []string{"unbacked_behavior_lines", "stale_claim_count", "missing_required_contracts"} {
		fixtureEqual(t, c.Expect[key], 0)
	}
	documents := map[string]string{}
	for _, value := range perGo(c, "documents_by_sdk", "documents") {
		readFixtureDocs(t, value.(string), documents)
	}
	fixtureData, err := os.ReadFile("spec/fixtures/serializer-capture-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		Cases []serializerFixture `json:"cases"`
	}
	if err = json.Unmarshal(fixtureData, &inventory); err != nil {
		t.Fatal(err)
	}
	for _, id := range perGo(c, "case_ids_by_sdk", "case_ids") {
		found := false
		for _, referenced := range inventory.Cases {
			if referenced.ID == id {
				found = true
				if referenced.Setup["scenario"] == "documentation_contract" || referenced.Expect == nil {
					t.Fatalf("backing case %s is not executable", id)
				}
				if !containsSDK(referenced.AppliesTo, "go") {
					t.Fatalf("backing case %s does not apply to go", id)
				}
				if !serializerCaseIDs[referenced.ID] {
					t.Fatalf("undriven case %s", referenced.ID)
				}
				break
			}
		}
		if !found {
			t.Fatalf("unknown backing case %v", id)
		}
	}

	var all strings.Builder
	for _, name := range slices.Sorted(maps.Keys(documents)) {
		all.WriteString(documents[name])
		all.WriteString("\n")
	}
	text := all.String()
	flat := strings.Join(strings.Fields(text), " ")
	// The shared contract requires claims somewhere in the declared document set.
	if claims, ok := c.Setup["claims_by_sdk"].(map[string]any); ok {
		for _, claim := range claims["go"].([]any) {
			if !regexp.MustCompile(claim.(string)).MatchString(flat) {
				t.Fatalf("missing documented claim %q", claim)
			}
		}
	}
	required := map[string][]string{"serializer": {"MarshalJSON", "MarshalText"}, "failure_null_no_field_walk": {"null", "never walked"}, "raw_bytes_and_serializer_text": {"raw bytes", "serializer"}, "per_report_counter": {"per report"}, "post_write_mcp": {"successful write"}, "base_callback_inputs": {"callbacks", "rules"}, "known_counter_limits": {"by-value", "itself"}, "no_stale_field_walk_claim": {"per report"}}
	words, ok := required[c.Setup["contract"].(string)]
	if !ok {
		t.Fatalf("unknown documentation contract %v", c.Setup["contract"])
	}
	for _, word := range words {
		if !strings.Contains(text, word) {
			t.Fatalf("missing documented contract %q", word)
		}
	}
	staleClaim := regexp.MustCompile(`(?i)(?:spans.on.*field.walk|interaction.wide.*once.only cache|pre.write MCP capture)`)
	fallback := regexp.MustCompile(`(?i)(?:MarshalJSON|MarshalText|serializer).*fails.*(?:fall back|fallback).*field`)
	skip := regexp.MustCompile(`Reject claims|must not claim|never|not dispatch`)
	about := regexp.MustCompile(`(?i)serializer|MarshalJSON|MarshalText|captured content`)
	for name, doc := range documents {
		for _, paragraph := range regexp.MustCompile(`\n\s*\n`).Split(doc, -1) {
			paragraph = strings.Join(strings.Fields(paragraph), " ")
			if !about.MatchString(paragraph) || skip.MatchString(paragraph) {
				continue
			}
			if fallback.MatchString(paragraph) || staleClaim.MatchString(paragraph) {
				t.Fatalf("%s: stale serializer claim: %s", name, paragraph)
			}
		}
	}
}
