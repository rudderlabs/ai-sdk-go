package rudderstackai

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

type orderedCaptureSerializer struct {
	name  string
	order *[]string
}

func (v orderedCaptureSerializer) MarshalJSON() ([]byte, error) {
	*v.order = append(*v.order, v.name)
	return json.Marshal(len(*v.order))
}

func TestCaptureMapSerializerOrder(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprintf("spans=%t", spans), func(t *testing.T) {
			setupAI(t, WithEmitSpans(spans))
			var order []string
			value := map[int]any{
				2: orderedCaptureSerializer{"two", &order},
				1: orderedCaptureSerializer{"one", &order},
				10: map[string]any{
					"z": orderedCaptureSerializer{"nested-z", &order},
					"a": orderedCaptureSerializer{"nested-a", &order},
				},
			}
			want := []string{"one", "nested-a", "nested-z", "two"}
			for n := 0; n < 100; n++ {
				order = nil
				got := captureValue(value).value
				if !reflect.DeepEqual(order, want) {
					t.Fatalf("capture %d: serializer order %v, want %v", n, order, want)
				}
				if wire := mustJSON(got); wire != `{"1":1,"10":{"a":2,"z":3},"2":4}` {
					t.Fatalf("capture %d: output %s", n, wire)
				}
			}
		})
	}
}

func TestCaptureMapRedactionOrder(t *testing.T) {
	for _, spans := range []bool{false, true} {
		t.Run(fmt.Sprintf("spans=%t", spans), func(t *testing.T) {
			var inputs []string
			setupAI(t, WithEmitSpans(spans), WithRedactHook(func(s string) (string, error) {
				inputs = append(inputs, s)
				return fmt.Sprintf("hook-%d", len(inputs)), nil
			}))
			value := map[string]any{
				"z": "last",
				"a": "person@example.com",
				"m": map[string]any{"z": "nested-z", "a": "nested-a"},
			}
			want := []string{"lookup", "[email]", "nested-a", "nested-z", "last"}
			for n := 0; n < 100; n++ {
				inputs = nil
				got := captureCallContent(nil, "", []GenAIToolCall{{Name: "lookup", Arguments: value}})
				if !reflect.DeepEqual(inputs, want) {
					t.Fatalf("capture %d: hook inputs %v, want %v", n, inputs, want)
				}
				if got == nil || mustJSON(got.outputToolCalls) != `[{"arguments":{"a":"hook-2","m":{"a":"hook-3","z":"hook-4"},"z":"hook-5"},"name":"hook-1"}]` {
					t.Fatalf("capture %d: output %+v", n, got)
				}
			}
		})
	}
}
