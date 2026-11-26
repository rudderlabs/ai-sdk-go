package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type round1Left struct {
	Conflict string `json:"conflict"`
	Plain    string
}
type round1Right struct {
	Conflict string `json:"conflict"`
	Tagged   string `json:"Plain"`
}
type round1Deep struct{ round1Left }
type round1DiamondA struct{ round1Deep }
type round1DiamondB struct{ round1Deep }
type round1Zero int

func (v *round1Zero) IsZero() bool { return *v == 7 }

type round1Fields struct {
	round1Left
	*round1Right
	EmptySlice    []int          `json:"empty_slice,omitempty"`
	EmptyMap      map[string]int `json:"empty_map,omitempty"`
	EmptyArray    [0]int         `json:"empty_array,omitempty"`
	NonemptyArray [1]int         `json:"nonempty_array,omitempty"`
	ZeroStruct    struct{}       `json:"zero_struct,omitempty"`
	Number        int            `json:"number,string"`
	Text          string         `json:"text,string"`
	Flag          bool           `json:"flag,string"`
	Pointer       *int           `json:"pointer,string"`
	Zero          round1Zero     `json:"zero,omitzero"`
	Skip          string         `json:"-"`
	Dash          string         `json:"-,omitempty"`   //nolint:staticcheck // Intentionally exercises encoding/json tag parsing.
	Invalid       string         `json:"bad\\tag"`      //nolint:staticcheck // Intentionally exercises encoding/json tag parsing.
	NotAnOption   string         `json:",notomitempty"` //nolint:staticcheck // Intentionally exercises encoding/json tag parsing.
}

func round1StructCases() map[string]any {
	n := 3
	v := round1Fields{round1Left: round1Left{"PRIVATE_CONFLICT_CANARY", "hidden"}, round1Right: &round1Right{"PRIVATE_CONFLICT_CANARY", "public"}, EmptySlice: []int{}, EmptyMap: map[string]int{}, Number: 3, Text: "quoted", Flag: true, Pointer: &n, Zero: 7, Skip: "PRIVATE_SKIP_CANARY", Dash: "dash", Invalid: "invalid", NotAnOption: ""}
	return map[string]any{"tags": &v, "float32": struct {
		Value float32 `json:"value"`
	}{0.1}, "nil_embedded": round1Fields{}, "diamond": struct {
		round1DiamondA
		round1DiamondB
	}{}, "nested": map[string]any{"value": &v}, "embedded_tag": struct {
		round1Left `json:"nested"`
	}{round1Left{"public", "plain"}}}
}

func TestRound1StructEncodingParity(t *testing.T) {
	for name, value := range round1StructCases() {
		for _, spans := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/spans=%v", name, spans), func(t *testing.T) {
				setupAI(t, WithEmitSpans(spans))
				want, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				got := mustJSON(captureValue(value).value)
				var decoded any
				_ = json.Unmarshal(want, &decoded)
				if got != mustJSON(decoded) {
					t.Fatalf("got %s; encoding/json %s", got, mustJSON(decoded))
				}
			})
		}
	}
}

func TestRound1StructPropertySiblings(t *testing.T) {
	for name, value := range round1StructCases() {
		for _, site := range []string{"tool", "mcp"} {
			t.Run(name+"/"+site, func(t *testing.T) {
				want, _ := json.Marshal(value)
				var decoded any
				_ = json.Unmarshal(want, &decoded)
				rule := ToolEventRule{Event: "lookup_done", Properties: func(map[string]any, any) map[string]any { return map[string]any{"custom": value} }}
				var got any
				if site == "tool" {
					r := setupAI(t, WithCaptureContent(true), WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": rule}}))
					_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
					i.RecordToolCalls(ToolCall{ID: "t", Name: "lookup", Result: "ok"})
					i.End(EndOptions{})
					got = r.props(t, "lookup_done")["custom"]
				} else {
					got = round1MCPProperties(t, rule)
				}
				if mustJSON(got) != mustJSON(decoded) {
					t.Fatalf("got %s; want %s", mustJSON(got), mustJSON(decoded))
				}
			})
		}
	}
}

func TestRound1StructBytesStripped(t *testing.T) {
	setupAI(t, WithCaptureContent(true))
	value := struct {
		Public string `json:"public"`
		Bytes  []byte `json:"bytes"`
		Nested *struct {
			Bytes []byte `json:"bytes"`
		} `json:"nested"`
	}{Public: "visible", Bytes: []byte("private"), Nested: &struct {
		Bytes []byte `json:"bytes"`
	}{[]byte("private")}}
	if got := mustJSON(captureValue(value).value); got != `{"nested":{},"public":"visible"}` {
		t.Fatal(got)
	}
}

func TestRound1StructCallContentBaseParity(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprint(spans), func(t *testing.T) {
			setupAI(t, WithEmitSpans(spans), WithCaptureContent(true))
			v := struct {
				Public string `json:"public"`
				Nested *struct {
					Value int `json:"value"`
				} `json:"nested"`
				Bytes []byte `json:"bytes"`
				Skip  string `json:"-"`
			}{Public: "visible", Nested: &struct {
				Value int `json:"value"`
			}{3}, Bytes: []byte("private"), Skip: "private"}
			c := captureCallContent([]GenAIMessage{{Role: "user", Parts: []GenAIPart{{"type": "tool_call_response", "response": v}}}}, "", []GenAIToolCall{{Name: "lookup", Arguments: v}})
			want := `{"nested":{"value":3},"public":"visible"}`
			if got := mustJSON(c.outputToolCalls[0].(map[string]any)["arguments"]); got != want {
				t.Fatal(got)
			}
			if got := mustJSON(c.inputMessages[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["response"]); got != want {
				t.Fatal(got)
			}
		})
	}
}

func TestRound1RootFailureSiblings(t *testing.T) {
	for _, kind := range []string{"json_error", "json_panic", "invalid_json", "text_error", "text_panic", "explicit_null", "absent"} {
		for _, wrapper := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wrapper=%v", kind, wrapper), func(t *testing.T) {
				r := setupAI(t, WithCaptureContent(true), WithEmitSpans(true), WithLLMCallEvents(true))
				var v any
				var calls *int
				switch kind {
				case "json_error", "json_panic", "invalid_json", "explicit_null":
					s := &fixtureSerializer{}
					calls = &s.Calls
					v = s
					if kind == "json_error" {
						s.Failure = "error"
					}
					if kind == "json_panic" {
						s.Failure = "panic"
					}
					if kind == "invalid_json" {
						s.Failure = "invalid_json"
					}
				case "text_error", "text_panic":
					s := &fixtureTextSerializer{Panic: kind == "text_panic"}
					v = s
					calls = &s.Calls
				}
				ctx, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
				call := LLMCall{Provider: "openai", Model: "m", OutputToolCalls: []GenAIToolCall{{Name: "lookup", Arguments: v}}}
				if wrapper {
					_, call.wrapperSpan = beginWrapperSpan(ctx, i, time.Now())
				}
				i.RecordCall(call)
				i.End(EndOptions{})
				args := r.props(t, "ai_llm_response_received")["output_tool_calls"].([]any)[0].(map[string]any)
				a, present := args["arguments"]
				if present != (kind != "absent") || a != nil {
					t.Fatalf("arguments=%v present=%v", a, present)
				}
				if calls != nil && *calls != 1 {
					t.Fatalf("serializer calls=%d", *calls)
				}
				if wrapper && kind != "absent" {
					found := false
					for _, e := range r.named("ai_otel_span") {
						s := mustJSON(e.Properties)
						if strings.Contains(s, `"key":"arguments","value":{}`) {
							found = true
						}
					}
					if !found {
						t.Fatalf("wrapper span lost null argument: %s", mustJSON(r.named("ai_otel_span")))
					}
				}
			})
		}
	}
}
