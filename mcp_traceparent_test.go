package rudderstackai

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestMetaJoinKeysSharedFixture(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name     string            `json:"name"`
			Meta     any               `json:"meta"`
			Expected map[string]string `json:"expected"`
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
			meta, _ := tc.Meta.(map[string]any)
			interactionID, traceID := joinIDs(meta)
			got := map[string]string{}
			if interactionID != "" {
				got["interaction_id"] = interactionID
			}
			if traceID != "" {
				got["trace_id"] = traceID
			}
			if !reflect.DeepEqual(got, tc.Expected) {
				t.Fatalf("joinIDs = %q, want %q", got, tc.Expected)
			}
		})
	}
}
