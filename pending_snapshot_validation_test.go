package rudderstackai

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestPendingToolAgentKeyValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  any
		kept bool
	}{
		{"empty", "", true},
		{"named", "planner", true},
		{"null", nil, false},
		{"number", 1, false},
		{"boolean", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := setupAI(t)
			first := sdk6017Start()
			first.RecordCall(sdk6017Call("", 10))
			first.RecordToolCalls(ToolCall{ID: "approval", Name: "approve_me"})
			s, ok := first.Pause(PauseOptions{})
			if !ok || len(s.PendingTools) != 1 {
				t.Fatal("pending approval snapshot missing")
			}
			s.PendingTools[0]["agent_key"] = tc.key
			encoded, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &s); err != nil {
				t.Fatal(err)
			}
			_, resumed := StartInteraction(context.Background(), InteractionOptions{Resume: &s})
			if (resumed.ID() == s.InteractionID) != tc.kept {
				t.Fatal("agent_key validation differs")
			}
			resumed.RecordToolResult("approval", ToolResult{Error: Ptr(false)})
			resumed.End(EndOptions{})
			want := 0
			if tc.kept {
				want = 1
			}
			if got := lastResponse(t, r)["tool_call_count"]; got != want {
				t.Fatalf("tool_call_count: got %v, want %d", got, want)
			}
		})
	}
}

func TestSharedPendingSnapshotValidation(t *testing.T) {
	raw, err := os.ReadFile("spec/fixtures/pause-pending-usage-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		CrossSDK struct {
			Validation []struct {
				Name     string
				Snapshot json.RawMessage
				Expected struct {
					KeptID   bool `json:"kept_id"`
					Response map[string]any
				}
			}
			Roundtrip struct {
				Snapshots []struct {
					WrittenBy string `json:"written_by"`
					Snapshot  json.RawMessage
				}
				Expected struct {
					Response             map[string]any
					AbsentResponseFields []string `json:"absent_response_fields"`
				}
			}
		} `json:"cross_sdk"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.CrossSDK.Validation {
		t.Run(tc.Name, func(t *testing.T) {
			r := setupAI(t)
			var s PausedInteraction
			// A malformed array or record can fail JSON decoding; resume must still reject it.
			_ = json.Unmarshal(tc.Snapshot, &s)
			_, i := StartInteraction(context.Background(), InteractionOptions{Resume: &s})
			if (i.ID() == s.InteractionID) != tc.Expected.KeptID {
				t.Fatal("interaction ID validation differs")
			}
			i.End(EndOptions{})
			got := lastResponse(t, r)
			for k, v := range tc.Expected.Response {
				if mustJSON(got[k]) != mustJSON(v) {
					t.Errorf("%s: got %v, want %v", k, got[k], v)
				}
			}
		})
	}
	for _, tc := range fixture.CrossSDK.Roundtrip.Snapshots {
		t.Run(tc.WrittenBy, func(t *testing.T) {
			r := setupAI(t, WithLLMCallEvents(true), WithToolBreakdown(true))
			var s PausedInteraction
			if err := json.Unmarshal(tc.Snapshot, &s); err != nil {
				t.Fatal(err)
			}
			_, i := StartInteraction(context.Background(), InteractionOptions{Resume: &s})
			if i.ID() != s.InteractionID {
				t.Fatal("snapshot ID lost")
			}
			i.End(EndOptions{})
			i.End(EndOptions{})
			if len(r.named("ai_response_received")) != 1 {
				t.Fatal("held usage counted more than once")
			}
			got := lastResponse(t, r)
			for k, v := range fixture.CrossSDK.Roundtrip.Expected.Response {
				if mustJSON(got[k]) != mustJSON(v) {
					t.Errorf("%s: got %v, want %v", k, got[k], v)
				}
			}
			for _, k := range fixture.CrossSDK.Roundtrip.Expected.AbsentResponseFields {
				if _, exists := got[k]; exists {
					t.Errorf("%s must be absent", k)
				}
			}
		})
	}
}
