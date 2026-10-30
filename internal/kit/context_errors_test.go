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
			if got := HTTPErrorType(c.Status, map[string]any{"error": map[string]any{"code": c.Code, "message": c.Message}}); got != c.Expected {
				t.Fatalf("got %s, want %s", got, c.Expected)
			}
		})
	}
}

func TestContextSignalOn500IsAPIError(t *testing.T) {
	if got := HTTPErrorType(500, map[string]any{"error": map[string]any{"message": "upstream failed: prompt is too long"}}); got != "api_error" {
		t.Fatal(got)
	}
}
