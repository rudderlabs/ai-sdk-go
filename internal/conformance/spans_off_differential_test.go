package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/testkit"
)

// Recorded by running this test at origin/main a5132d5eea5b3df5a73120600f5c30b664dbf5fa.
// Only random IDs and wall-clock measurements use the shared fixture normalization;
// every product field, content value, call index, event count and ordering is compared exactly.
func TestSpansOffOriginMainDifferential(t *testing.T) {
	path := filepath.Join("testdata", "spans-off-origin-main.json")
	expected := map[string]json.RawMessage{}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, sc := range differentialScenarios(t) {
		if unsupported(sc.Steps) != "" || gaps[sc.Name] != "" || (!hasProviderCall(sc.Steps) && sc.Name != "per_call_content_binary_never_sent") {
			continue
		}
		seen++
		t.Run(sc.Name, func(t *testing.T) {
			rec := &recorder{}
			sc.Config["emit_spans"] = false
			configure(t, sc.Config, rec)
			llm.Reset()
			for route, replies := range sc.Replies {
				for _, raw := range replies {
					var reply testkit.Reply
					if err := remarshal(raw, &reply); err != nil {
						t.Fatal(err)
					}
					llm.Script(route, reply)
				}
			}
			r := &run{t: t, llm: llm}
			if err := r.steps(context.Background(), sc.Steps); err != nil {
				t.Fatal(err)
			}
			rudderstackai.Flush(context.Background())
			got, err := json.Marshal(normalise(rec.all(), false, true))
			if err != nil {
				t.Fatal(err)
			}
			want, ok := expected[sc.Name]
			if !ok {
				t.Fatal("missing origin/main recording")
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, want); err != nil {
				t.Fatal(err)
			}
			if string(got) != compact.String() {
				t.Fatalf("product events differ from origin/main\nwant: %s\ngot:  %s", want, got)
			}
		})
	}

	for _, provider := range []string{"openai", "anthropic", "gcp.gemini", "aws.bedrock"} {
		for _, edge := range differentialEdges {
			name := "round7_edge_" + provider + "_" + edge
			seen++
			t.Run(name, func(t *testing.T) {
				got, err := json.Marshal(differentialEdgeEvents(t, provider, edge))
				if err != nil {
					t.Fatal(err)
				}
				want, ok := expected[name]
				if !ok {
					t.Fatal("missing origin/main recording")
				}
				var compact bytes.Buffer
				if err := json.Compact(&compact, want); err != nil {
					t.Fatal(err)
				}
				if string(got) != compact.String() {
					t.Fatalf("product events differ from origin/main\nwant: %s\ngot: %s", want, got)
				}
			})
		}
	}
	if seen < 56 {
		t.Fatalf("matrix too narrow: %d scenarios", seen)
	}
	if seen != len(expected) {
		t.Fatalf("cases=%d recordings=%d", seen, len(expected))
	}
	t.Logf("%d origin/main scenarios compared", seen)
}
func hasProviderCall(steps []obj) bool {
	for _, s := range steps {
		if s["op"] == "call" {
			return true
		}
		for _, key := range []string{"steps", "during"} {
			if hasProviderCall(objs0(s[key])) {
				return true
			}
		}
	}
	return false
}

func differentialScenarios(t *testing.T) []scenario {
	scenarios := loadFixture(t)
	seeds := map[string]string{"openai_chat": "openai.chat", "openai_responses": "openai.responses", "anthropic_create": "anthropic.messages", "google_generate_plain": "google.generate", "google_vertex_generate_plain": "google.generate", "bedrock_converse_tool_use": "bedrock.converse"}

	for _, seed := range loadFixture(t) {
		if seed.Name == "openai_chat_stream" {
			for _, terminal := range []bool{false, true} {
				for _, finish := range []bool{false, true} {
					var sc scenario
					if err := remarshal(seed, &sc); err != nil {
						t.Fatal(err)
					}
					sc.Name = fmt.Sprintf("round7_openai_chat_terminal=%v_finish=%v", terminal, finish)
					for _, reply := range sc.Replies["openai.chat"] {
						events := []any{}
						for _, event := range objs0(reply["sse"]) {
							if event["data"] == "[DONE]" && !terminal {
								continue
							}
							if !finish {
								for _, choice := range objs0(obj0(event["data"])["choices"]) {
									delete(choice, "finish_reason")
								}
							}
							events = append(events, event)
						}
						reply["sse"] = events
					}
					scenarios = append(scenarios, sc)
				}
			}
		}
		route, ok := seeds[seed.Name]
		if !ok {
			continue
		}
		for _, inside := range []bool{false, true} {
			var sc scenario
			if err := remarshal(seed, &sc); err != nil {
				t.Fatal(err)
			}
			if sc.Config == nil {
				sc.Config = obj{}
			}
			if sc.Replies == nil {
				sc.Replies = map[string][]obj{}
			}
			sc.Name = "round7_retry_503_200_" + seed.Name
			if inside {
				sc.Name += "_interaction"
			} else {
				sc.Name += "_ephemeral"
			}
			step := firstDifferentialCall(sc.Steps)
			if step == nil {
				t.Fatal("missing seed call")
			}
			step["retry_attempts"] = float64(1)
			delete(step, "expect_error")
			if strings.HasPrefix(route, "google") {
				params := obj0(step["params"])
				config := obj0(params["config"])
				if config == nil {
					config = obj{}
				}
				config["httpOptions"] = obj{"retryOptions": obj{"attempts": 2, "initialDelay": 0.001}}
				params["config"] = config
			}
			errorBody := `{"error":{"type":"api_error","message":"busy","code":503}}`
			sc.Replies[route] = append([]obj{{"status": 503, "json": json.RawMessage(errorBody)}}, sc.Replies[route]...)
			sc.Config["emit_llm_calls"], sc.Config["capture_content"] = true, true
			if inside {
				sc.Steps = []obj{{"op": "interaction", "options": obj{"user_id": "retry-user", "classify": false}, "steps": []any{step}}}
			} else {
				sc.Steps = []obj{step}
			}
			scenarios = append(scenarios, sc)
		}
	}
	return scenarios
}
func firstDifferentialCall(steps []obj) obj {
	for _, s := range steps {
		if s["op"] == "call" {
			return s
		}
		if found := firstDifferentialCall(objs0(s["steps"])); found != nil {
			return found
		}
	}
	return nil
}
