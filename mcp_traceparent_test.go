package rudderstackai

import (
	"encoding/json"
	"os"
	"testing"
)

func TestTraceparentSharedFixture(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name        string  `json:"name"`
			Traceparent any     `json:"traceparent"`
			Expected    *string `json:"expected"`
		} `json:"cases"`
	}
	data, err := os.ReadFile("spec/fixtures/mcp-traceparent-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			want := ""
			if tc.Expected != nil {
				want = *tc.Expected
			}
			if got := traceIDFromTraceparent(tc.Traceparent); got != want {
				t.Fatalf("traceIDFromTraceparent(%q) = %q, want %q", tc.Traceparent, got, want)
			}
		})
	}
}
