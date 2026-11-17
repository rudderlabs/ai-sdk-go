package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/rudderlabs/ai-sdk-go/internal/spanmodel"
)

type sdk6942Fields struct {
	Keep   any    `json:"keep"`
	Raw    []byte `json:"raw,omitempty"`
	Nested any    `json:"nested"`
}
type sdk6942Embedded struct{ sdk6942Fields }

func sdk6942Build(v any) any {
	if a, ok := v.([]any); ok {
		for n, x := range a {
			a[n] = sdk6942Build(x)
		}
		return a
	}
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if m["binary"] != nil {
		return []byte("PRIVATE_BINARY_CANARY")
	}
	if entries, ok := m["entries"].([]any); ok {
		out := map[int]any{}
		for _, e := range entries {
			pair := e.([]any)
			out[int(pair[0].(float64))] = sdk6942Build(pair[1])
		}
		return out
	}
	if set, ok := m["set"].([]any); ok {
		return sdk6942Build(set)
	}
	if shapes, ok := m["construct_as"].(map[string]any); ok {
		f := m["fields"].(map[string]any)
		raw, _ := sdk6942Build(f["raw"]).([]byte)
		p := &sdk6942Fields{Keep: sdk6942Build(f["keep"]), Raw: raw, Nested: sdk6942Build(f["nested"])}
		if m["cycle"] == true {
			p.Nested = p
		}
		switch shapes["go"] {
		case "embedded":
			return sdk6942Embedded{*p}
		case "struct":
			return *p
		}
		return p
	}
	out := map[string]any{}
	for k, x := range m {
		out[k] = sdk6942Build(x)
	}
	return out
}
func TestSDK6942NativeContentContainers(t *testing.T) {
	raw, err := os.ReadFile("spec/fixtures/conformance-v0.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string
			Input    any
			Expected any
		} `json:"native_content_cases"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		for _, spans := range []bool{false, true} {
			t.Run(c.Name+"/spans="+map[bool]string{false: "off", true: "on"}[spans], func(t *testing.T) {
				r := setupAI(t, WithCaptureContent(true), WithLLMCallEvents(true), WithEmitSpans(spans))
				// Build fresh containers for each path without changing the fixture tree.
				b, _ := json.Marshal(c.Input)
				var input any
				_ = json.Unmarshal(b, &input)
				value := sdk6942Build(input)
				if got := stripBinary(value); !reflect.DeepEqual(got, c.Expected) {
					t.Fatalf("normalizer: %#v != %#v", got, c.Expected)
				}
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
				i.RecordCall(LLMCall{Provider: "openai", Model: "m", InputMessages: []GenAIMessage{{Role: "tool", Parts: []GenAIPart{{"type": "tool_call_response", "response": value}}}}, OutputToolCalls: []GenAIToolCall{{Name: "lookup", Arguments: value}}})
				i.End(EndOptions{})
				p := r.named("ai_llm_response_received")[0].Properties
				messages := p["input_messages"].([]any)
				got := messages[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["response"]
				if !reflect.DeepEqual(got, c.Expected) {
					t.Fatalf("messages: %#v", got)
				}
				got = p["output_tool_calls"].([]any)[0].(map[string]any)["arguments"]
				if !reflect.DeepEqual(got, c.Expected) {
					t.Fatalf("tool calls: %#v", got)
				}
				span := spanmodel.Span{Attributes: map[string]any{"gen_ai.input.messages": value}, Events: []spanmodel.Event{{Name: "gen_ai.content", Attributes: map[string]any{"content": value}}}}
				encoded, _ := spanmodel.ToOTLP(span, true, false)
				span.Attributes["gen_ai.input.messages"] = c.Expected
				span.Events[0].Attributes["content"] = c.Expected
				want, _ := spanmodel.ToOTLP(span, true, false)
				if !reflect.DeepEqual(encoded, want) {
					t.Fatalf("span: %#v != %#v", encoded, want)
				}
				wire, _ := json.Marshal([]any{p, encoded})
				if strings.Contains(string(wire), "PRIVATE_BINARY_CANARY") {
					t.Fatal(string(wire))
				}
			})
		}
	}
}

type sdk6942Hidden struct {
	Visible string
	Secret  string
}

// The app's MarshalJSON leaves Secret out on purpose.
func (h sdk6942Hidden) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"visible": h.Visible})
}

type sdk6942Failing struct{ Value string }

func (sdk6942Failing) MarshalJSON() ([]byte, error) { return nil, errors.New("no JSON form") }

// Values with their own JSON form keep it, as on main; plain values are walked so bytes drop.
func TestSDK6942CustomMarshalersKeepTheirRepresentation(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want any
	}{
		{"map of byte slices drops its bytes", map[string][]byte{"raw": []byte("PRIVATE")}, map[string]any{}},
		{"marshaler hiding a field keeps it hidden", sdk6942Hidden{Visible: "ok", Secret: "PRIVATE"}, map[string]any{"visible": "ok"}},
		{"pointer to a hiding marshaler", &sdk6942Hidden{Visible: "ok", Secret: "PRIVATE"}, map[string]any{"visible": "ok"}},
		{"failing marshaler drops the value", map[string]any{"keep": "ok", "bad": sdk6942Failing{Value: "PRIVATE"}}, map[string]any{"keep": "ok"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripBinary(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("stripBinary() = %#v, want %#v", got, tc.want)
			}
		})
	}
}
