package serializerfixture

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestResolveGo(t *testing.T) {
	var c Case
	if err := json.Unmarshal([]byte(`{
		"id": "override", "applies_to": ["go"],
		"setup": {"scenario": "report_sinks", "sinks": ["call_event", "pause_snapshot"],
			"sinks_by_sdk": {"go": ["call_event"]}},
		"expect": {"decoded": {"call_event": "default", "pause_snapshot": "default"},
			"serializer_calls": {"value": 1}, "never_emitted": ["secret"]},
		"expect_by_sdk": {"go": {"decoded": {"call_event": null},
			"serializer_calls": {"during_restore": 0}}, "node": {"never_emitted": []}}
	}`), &c); err != nil {
		t.Fatal(err)
	}
	resolved, err := c.ResolveGo()
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.AppliesToGo() || !reflect.DeepEqual(resolved.Setup["sinks"], []any{"call_event"}) {
		t.Fatalf("Go inputs not selected: %+v", resolved)
	}
	want := map[string]any{
		"decoded":          map[string]any{"call_event": nil},
		"serializer_calls": map[string]any{"during_restore": float64(0)},
		"never_emitted":    []any{"secret"},
	}
	if !reflect.DeepEqual(resolved.Expect, want) {
		t.Fatalf("expect=%v want=%v", resolved.Expect, want)
	}
	if c.Expect["decoded"].(map[string]any)["call_event"] != "default" || len(c.Setup["sinks"].([]any)) != 2 {
		t.Fatal("resolution changed shared defaults")
	}
	c.Setup["sinks_by_sdk"].(map[string]any)["go"] = []any{"unknown"}
	if _, err := c.ResolveGo(); err == nil {
		t.Fatal("unknown sink expectation accepted")
	}
	c.AppliesTo = []string{"node"}
	if c.AppliesToGo() {
		t.Fatal("non-applicable case accepted")
	}
}
