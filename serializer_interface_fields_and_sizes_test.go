package rudderstackai

import (
	"context"
	"encoding"
	"encoding/json"
	"fmt"
	"testing"
)

type round2Both struct {
	calls  *int
	secret string
}

func (v round2Both) MarshalJSON() ([]byte, error) {
	*v.calls++
	return json.Marshal(map[string]string{"secret": v.secret})
}

func (v round2Both) MarshalText() ([]byte, error) {
	*v.calls++
	return []byte("public"), nil
}

type round2BothPointer struct{ round2Both }

func (v *round2BothPointer) MarshalJSON() ([]byte, error) { return v.round2Both.MarshalJSON() }
func (v *round2BothPointer) MarshalText() ([]byte, error) { return v.round2Both.MarshalText() }

func round2Decoded(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	return mustJSON(decoded)
}

// Interface fields use encoding/json's chosen serializer without leaking alternate representations.
func TestRound2TextMarshalerInterfaceField(t *testing.T) {
	calls := 0
	value := struct {
		F encoding.TextMarshaler `json:"f"`
		J json.Marshaler         `json:"j"`
		N encoding.TextMarshaler `json:"n"`
	}{F: round2Both{calls: &calls, secret: "PRIVATE_TM"}, J: round2Both{calls: &calls, secret: "json"}}
	want := round2Decoded(t, value)
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprint(spans), func(t *testing.T) {
			r := setupAI(t, WithEmitSpans(spans), WithCaptureContent(true), WithLLMCallEvents(true))
			if got := mustJSON(captureValue(value).value); got != want {
				t.Fatalf("captured %s; encoding/json %s", got, want)
			}
			_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Classify: Ptr(false)})
			i.RecordCall(LLMCall{Provider: "openai", Model: "m", OutputToolCalls: []GenAIToolCall{{Name: "lookup", Arguments: map[string]any{"v": value}}}})
			i.End(EndOptions{})
			got := mustJSON(r.named("ai_llm_response_received")[0].Properties["output_tool_calls"].([]any)[0].(map[string]any)["arguments"].(map[string]any)["v"])
			if got != want {
				t.Fatalf("output_tool_calls=%s; encoding/json %s", got, want)
			}
		})
	}
}

// Interface pointers serialize once per report without sharing output between JSON and text selections.
func TestRound2InterfacePointerIdentity(t *testing.T) {
	setupAI(t)
	calls := 0
	v := &round2BothPointer{round2Both{calls: &calls, secret: "s"}}
	same := struct{ A, B json.Marshaler }{v, v}
	if got, want := mustJSON(captureValue(same).value), round2Decoded(t, same); got != want || calls != 3 {
		// json.Marshal ran it twice; the capture once.
		t.Fatalf("captured %s want %s; calls=%d", got, want, calls)
	}
	calls = 0
	mixed := struct {
		J json.Marshaler
		T encoding.TextMarshaler
		P *round2BothPointer
	}{v, v, v}
	want := round2Decoded(t, mixed)
	runs := 1 // MarshalJSON once for J and P
	if staticText() {
		runs = 2 // and MarshalText once for T
	}
	calls = 0
	if got := mustJSON(captureValue(mixed).value); got != want || calls != runs {
		t.Fatalf("captured %s want %s; calls=%d want %d", got, want, calls, runs)
	}
}

// The JSON marshaler uses json.Marshal, which HTML-escapes its output.
type round2EscapedJSON struct{ URL string }

func (v round2EscapedJSON) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"url": v.URL})
}

type round2TextHTML struct{}

func (round2TextHTML) MarshalText() ([]byte, error) { return []byte("a&b <c>"), nil }

// Sizes count serializer output as encoding/json wrote it: escapes a MarshalJSON built on
// json.Marshal emits are kept, unescaped text stays unescaped, as base measured them.
func TestRound2SerializerOutputSizes(t *testing.T) {
	for _, value := range []any{
		round2EscapedJSON{URL: "https://x.test/?a=1&b=2"},
		round2TextHTML{},
		map[string]any{"plain": "<b>", "app": round2EscapedJSON{URL: "&"}, "text": round2TextHTML{}},
		[]any{round2EscapedJSON{URL: "<"}, "a&b"},
	} {
		for _, spans := range []bool{false, true} {
			setupAI(t, WithEmitSpans(spans))
			b, err := compactJSON(value)
			if err != nil {
				t.Fatal(err)
			}
			report := captureValue(map[string]any{"v": value})
			if report.sizes["v"] != len(b) || captureValue(value).size != len(b) {
				t.Errorf("%T spans=%v: size=%d/%d want %d", value, spans, report.sizes["v"], captureValue(value).size, len(b))
			}
		}
	}
}

// Field names follow the running encoding/json, whose tag rules differ across Go releases.
type round2Tags struct {
	Quote  string `json:"a\"b"`     //nolint:staticcheck // Intentionally exercises encoding/json tag parsing.
	Single string `json:"c'd"`      //nolint:staticcheck // Intentionally exercises encoding/json tag parsing.
	Slash  string `json:"e\\"`      //nolint:staticcheck // Intentionally exercises encoding/json tag parsing.
	Tab    string `json:"bad\\tag"` //nolint:staticcheck // Intentionally exercises encoding/json tag parsing.
	Valid  string `json:"valid,omitempty"`
}

func TestRound2TagNamesFollowEncodingJSON(t *testing.T) {
	value := round2Tags{"q", "s", "b", "t", "v"}
	for _, spans := range []bool{false, true} {
		setupAI(t, WithEmitSpans(spans))
		if got, want := mustJSON(captureValue(value).value), round2Decoded(t, value); got != want {
			t.Fatalf("spans=%v: captured %s; encoding/json %s", spans, got, want)
		}
	}
}
