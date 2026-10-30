package kit

import (
	"encoding/json"
	"os"
	"testing"
)

func TestSharedContextSignals(t *testing.T) {
	raw, err := os.ReadFile("../../spec/fixtures/context-errors-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name, Code, Message, Expected string
			Status                        int
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			expected := c.Expected
			if c.Status == 500 && ContextLimit(c.Code, c.Message) {
				expected = "context_limit"
			}
			if got := HTTPErrorType(c.Status, map[string]any{"error": map[string]any{"code": c.Code, "message": c.Message}}); got != expected {
				t.Fatalf("got %s, want %s", got, expected)
			}
		})
	}
}

func TestWrappedContextLimit500(t *testing.T) {
	if got := HTTPErrorType(500, map[string]any{"error": map[string]any{"message": "upstream failed: prompt is too long"}}); got != "context_limit" {
		t.Fatal(got)
	}
}
