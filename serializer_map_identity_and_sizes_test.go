package rudderstackai

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

type reviewTextSize struct{}

func (reviewTextSize) MarshalText() ([]byte, error) { return []byte("visible"), nil }

type reviewTextKey int

func (reviewTextKey) MarshalText() ([]byte, error) { return []byte("public"), nil }

type reviewStructKey struct{ Secret string }

func (reviewStructKey) MarshalText() ([]byte, error) { return []byte("public"), nil }

type reviewGrowingMap map[string]any

func (v reviewGrowingMap) MarshalJSON() ([]byte, error) {
	v[fmt.Sprint(len(v))] = true
	return json.Marshal(len(v))
}

func TestExecutingMapKeys(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprint(spans), func(t *testing.T) {
			setupAI(t, WithEmitSpans(spans))
			for _, v := range []any{map[reviewTextKey]any{12345: "visible"}, map[reviewStructKey]any{{"PRIVATE"}: "visible"}} {
				if got := mustJSON(captureValue(v).value); got != `{"public":"visible"}` {
					t.Errorf("captured %s", got)
				}
			}
		})
	}
}
func TestExecutingSerializedStringSizes(t *testing.T) {
	setupAI(t)
	for _, v := range []any{&fixtureSerializer{Output: "visible"}, reviewTextSize{}, time.Unix(0, 0).UTC()} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		got := captureValue(v)
		if got.size != len(b) {
			t.Errorf("root size=%d want=%d", got.size, len(b))
		}
		got = captureValue(map[string]any{"v": v})
		if got.sizes["v"] != len(b) {
			t.Errorf("field size=%d want=%d", got.sizes["v"], len(b))
		}
	}
	if captureValue("visible").size != 7 {
		t.Fatal("plain string size changed")
	}
}
func TestExecutingMapIdentity(t *testing.T) {
	setupAI(t)
	v := reviewGrowingMap{}
	got := captureValue([]any{v, v}).value
	if len(v) != 1 || mustJSON(got) != `[1,1]` {
		t.Fatalf("map=%v got=%v", v, got)
	}
}
func TestExecutingOpaqueArguments(t *testing.T) {
	setupAI(t)
	for _, tc := range []struct {
		value any
		want  string
	}{
		{`"{\"private\":true}"`, `"{\"private\":true}"`},
		{"null", "null"},
		{&fixtureSerializer{Output: `{"private":true}`}, `"{\"private\":true}"`},
	} {
		got := captureCallContent(nil, "", []GenAIToolCall{{Name: "lookup", Arguments: tc.value}})
		m := got.outputToolCalls[0].(map[string]any)
		v, ok := m["arguments"]
		if !ok || mustJSON(v) != tc.want {
			t.Errorf("arguments=%s present=%v want=%s", mustJSON(v), ok, tc.want)
		}
	}
}
