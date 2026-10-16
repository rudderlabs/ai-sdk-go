package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

type redactionFixture struct {
	Tokens map[string]string `json:"tokens"`
	Cases  []struct {
		Name     string  `json:"name"`
		Input    string  `json:"input"`
		Expected *string `json:"expected"`
		Repeat   int     `json:"repeat"`
	} `json:"cases"`
	Rules []struct {
		Name      string `json:"name"`
		Pattern   string `json:"pattern"`
		Token     string `json:"token"`
		Check     string `json:"check"`
		KeepGroup int    `json:"keep_group"`
	} `json:"rules"`
}

func loadRedactionFixture(t *testing.T) redactionFixture {
	t.Helper()
	b, err := os.ReadFile("spec/fixtures/redaction-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var f redactionFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRedactionRulesMatchSharedFixture(t *testing.T) {
	f := loadRedactionFixture(t)
	if len(f.Rules) != len(redactRules) {
		t.Fatalf("%d rules, fixture has %d", len(redactRules), len(f.Rules))
	}
	for i, want := range f.Rules {
		got := redactRules[i]
		if got.name != want.Name || got.pattern != want.Pattern || got.token != want.Token || got.keep != (want.KeepGroup == 1) || (got.check != nil) != (want.Check != "") {
			t.Errorf("rule %d (%s) differs from the fixture", i, want.Name)
		}
	}
}

func TestRedactionConformance(t *testing.T) {
	for _, c := range loadRedactionFixture(t).Cases {
		t.Run(c.Name, func(t *testing.T) {
			n := max(c.Repeat, 1)
			got, ok := maskText(strings.Repeat(c.Input, n))
			if c.Expected == nil {
				if ok {
					t.Error("want the text withheld")
				}
				return
			}
			if want := strings.Repeat(*c.Expected, n); !ok || got != want {
				t.Errorf("mask(%q) = %q, want %q", c.Input, got, want)
			}
		})
	}
	if !hasSensitive("mail ann@example.com") || hasSensitive("merge two workspaces") {
		t.Error("hasSensitive")
	}
}

func TestTraceparent(t *testing.T) {
	cases := map[any]string{
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01":    "4bf92f3577b34da6a3ce929d0e0e4736",
		" 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-x": "4bf92f3577b34da6a3ce929d0e0e4736",
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01":    "",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01":    "",
		"00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01":    "",
		42: "",
	}
	for in, want := range cases {
		if got := traceIDFromTraceparent(in); got != want {
			t.Errorf("traceIDFromTraceparent(%v) = %q", in, got)
		}
	}
	if i, tr := joinIDs(nil); i != "" || tr != "" {
		t.Error("joinIDs(nil)")
	}
}

func TestInvalidArguments(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"required":             []any{"a", "missing"},
		"additionalProperties": false,
		"properties": map[string]any{
			"a": map[string]any{"type": "integer"},
			"b": map[string]any{"type": []any{"string", "null"}},
			"c": map[string]any{"enum": []any{"x", "y"}},
			"d": map[string]any{"const": 3.0},
			"e": map[string]any{"type": "array"},
			"f": map[string]any{"type": "object"},
			"g": map[string]any{"type": "boolean"},
			"h": map[string]any{"type": "number"},
			"i": map[string]any{"type": "weird"},
		},
	}
	args := map[string]any{"a": 1.5, "b": nil, "c": "z", "d": 3.0, "e": []any{}, "f": "no", "g": true, "h": "1", "i": 1.0, "zz": 1.0}
	got := invalidArguments(schema, args)
	if want := []string{"missing", "a", "c", "f", "h", "zz"}; !slices.Equal(got, want) {
		t.Errorf("invalidArguments = %v, want %v", got, want)
	}
	if invalidArguments(nil, args) != nil {
		t.Error("no schema")
	}
}

func TestErrorTypes(t *testing.T) {
	cases := map[error]string{
		context.DeadlineExceeded:              "timeout",
		fmt.Errorf("x: %w", context.Canceled): "cancelled",
		&jsonrpc.Error{Code: -32700}:          "invalid_request",
		&jsonrpc.Error{Code: -32601}:          "invalid_request",
		&jsonrpc.Error{Code: -32000}:          "connection_error",
		&jsonrpc.Error{Code: -32001}:          "timeout",
		&jsonrpc.Error{Code: -32603}:          "api_error",
		errors.New("anything"):                "api_error",
	}
	for err, want := range cases {
		if got := errorType(err); got != want {
			t.Errorf("errorType(%v) = %s, want %s", err, got, want)
		}
	}
}

func TestHelpers(t *testing.T) {
	if got := capText("héllo", 2); got != "hé" {
		t.Errorf("capText = %q", got)
	}
	if got := capText("hi", 5); got != "hi" {
		t.Errorf("capText = %q", got)
	}
	if got := closedList([]string{"a", "", "a", "other", "b"}); !slices.Equal(got, []string{"a", "other", "b"}) {
		t.Errorf("closedList = %v", got)
	}
	if argumentsHash("t", map[string]any{"a": 1, "b": 2}) != argumentsHash("t", map[string]any{"b": 2, "a": 1}) ||
		argumentsHash("t", nil) != argumentsHash("t", map[string]any{}) || argumentsHash("t", nil) == argumentsHash("u", nil) {
		t.Error("argumentsHash")
	}
	m := templateMatcher("destinations://{id}/runs/{run}")
	if !m.MatchString("destinations://d1/runs/r.2") || m.MatchString("destinations:///runs/r") {
		t.Error("templateMatcher")
	}
	if byteSize(func() {}) != 0 || toJSONValue(func() {}) != nil || capabilityNames("x") != nil || implementation(nil) != nil {
		t.Error("unencodable values")
	}
	if formatInt(42) != "42" {
		t.Error("formatInt")
	}
	stats := resultStats(map[string]any{"content": []any{map[string]any{"type": "image"}, map[string]any{"type": "image"}, map[string]any{"type": "custom"}}, "structuredContent": map[string]any{"next_cursor": "n"}}, 9)
	if !slices.Equal(stats["result_content_types"].([]string), []string{"image", "structured"}) || stats["result_paginated"] != true || stats["result_tokens_estimate"] != 2 {
		t.Errorf("resultStats = %v", stats)
	}
}

func TestBoundedMap(t *testing.T) {
	m := newBoundedMap[int](2)
	m.set("a", 1)
	m.set("b", 2)
	m.set("a", 3)
	m.set("c", 4)
	if _, ok := m.get("b"); ok {
		t.Error("oldest entry kept")
	}
	if v, ok := m.get("a"); !ok || v != 3 {
		t.Error("re-set entry forgotten")
	}
	m.remove("a")
	m.remove("missing")
	if _, ok := m.get("a"); ok {
		t.Error("removed entry kept")
	}
	m.clear()
	if _, ok := m.get("c"); ok {
		t.Error("cleared entry kept")
	}
}

func TestToolResultValue(t *testing.T) {
	text := func(s string) map[string]any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}
	}
	if v := toolResultValue(text(`{"id":1}`)); mustJSON(v) != `{"id":1}` {
		t.Errorf("json text = %v", v)
	}
	if v := toolResultValue(text("plain")); v != "plain" {
		t.Errorf("plain text = %v", v)
	}
	if v := toolResultValue(nil); v != nil {
		t.Errorf("nil = %v", v)
	}
	two := map[string]any{"content": []any{1, 2}}
	if v := toolResultValue(two); mustJSON(v) != "[1,2]" {
		t.Errorf("blocks = %v", v)
	}
}

func TestSchemaValidationCatchesBadEvents(t *testing.T) {
	if err := validateEvent("mcp_tool_called", map[string]any{"tool_name": "x"}); err == nil {
		t.Error("missing required fields passed")
	}
	if err := validateEvent("mcp_tool_called", map[string]any{"request_id": "1", "tool_name": "x", "tool_status": "maybe"}); err == nil {
		t.Error("bad enum passed")
	}
	if err := validateEvent("ai_unknown", map[string]any{}); err == nil {
		t.Error("reserved unknown event passed")
	}
	if err := validateEvent("audience_created", map[string]any{"tool_name": "x", "tool_status": "success"}); err != nil {
		t.Error(err)
	}
}
