package rudderstackai

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"testing"
	"time"
)

type auditPropertyCase struct {
	Input    string `json:"input"`
	Scope    string `json:"scope"`
	Expected struct {
		Normalized string `json:"normalized"`
		Content    bool   `json:"content"`
		Reserved   bool   `json:"reserved"`
	} `json:"expected"`
}

type auditReport struct {
	Model        string `json:"model"`
	Provider     string `json:"provider"`
	ResponseID   string `json:"response_id"`
	Source       string `json:"source"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	StartedAt    *int64 `json:"started_at"`
	EndedAt      *int64 `json:"ended_at"`
}

type auditCallCase struct {
	Name    string `json:"name"`
	Adapter string `json:"adapter"`
	Input   struct {
		Provider string        `json:"provider"`
		Reports  []auditReport `json:"reports"`
		Prices   map[string]struct {
			Input  float64 `json:"input_per_mtok"`
			Output float64 `json:"output_per_mtok"`
		} `json:"prices"`
		Credits map[string]float64 `json:"credits"`
	} `json:"input"`
	Expected map[string]any `json:"expected"`
}

type auditStatusStep struct {
	Op           string         `json:"op"`
	Status       ResponseStatus `json:"status"`
	ErrorType    string         `json:"error_type"`
	FinishReason string         `json:"finish_reason"`
}

// TestAuditNormalisationConformance runs every case in the canonical shared fixture.
// Hints and legacy notes both map to Go's outcome hint; end options apply at finalization.
func TestAuditNormalisationConformance(t *testing.T) {
	raw, err := os.ReadFile("spec/fixtures/audit-normalisation-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		FixtureVersion  int                 `json:"fixture_version"`
		PropertyNames   []auditPropertyCase `json:"property_names"`
		CanonicalModels []struct {
			Input    string `json:"input"`
			Expected string `json:"expected"`
		} `json:"canonical_models"`
		Calls    []auditCallCase `json:"calls"`
		Statuses []struct {
			Name     string            `json:"name"`
			Input    []auditStatusStep `json:"input"`
			Expected map[string]any    `json:"expected"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.FixtureVersion != 1 {
		t.Fatalf("unsupported fixture version %d", fixture.FixtureVersion)
	}
	total := len(fixture.PropertyNames) + len(fixture.CanonicalModels) + len(fixture.Calls) + len(fixture.Statuses)
	if total != 181 {
		t.Fatalf("expected 181 cases, got %d", total)
	}
	passed, skipped := 0, 0
	for n, tc := range fixture.PropertyNames {
		if t.Run(fmt.Sprintf("property/%02d/%s", n, tc.Input), func(t *testing.T) {
			if got := propertyName(tc.Input); got != tc.Expected.Normalized {
				t.Errorf("normalized: got %q, want %q", got, tc.Expected.Normalized)
			}
			r := setupAI(t)
			logs := captureLogs(t)
			for range 2 {
				TrackAction(context.Background(), TrackActionOptions{UserID: "u", ConversationID: "c", InteractionID: "i", ActionType: ActionFeedbackGiven, ActionDetails: map[string]any{tc.Input: "private"}})
			}
			for _, event := range r.named("ai_user_action") {
				details, _ := event.Properties["action_details"].(map[string]any)
				_, present := details[tc.Input]
				if present == tc.Expected.Content {
					t.Errorf("content guard: present=%t, content=%t", present, tc.Expected.Content)
				}
			}
			if len(r.named("ai_user_action")) != 2 {
				t.Fatal("missing action events")
			}
			wantWarnings := 0
			if tc.Expected.Content {
				wantWarnings = 1
			}
			if got := logs.count("custom property " + tc.Input + " was dropped"); got != wantWarnings {
				t.Errorf("warnings: got %d, want %d", got, wantWarnings)
			}
			r = setupAI(t, WithCaptureContent(true), WithEventRules(EventRules{Tools: map[string]ToolEventRule{"search": {Event: "searched", Properties: func(map[string]any, any) map[string]any { return map[string]any{tc.Input: "private"} }}}}))
			_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u", AgentVersion: "v1"})
			if tc.Scope == "tool" {
				in.RecordToolCalls(ToolCall{ID: "t", Name: "search"})
				in.RecordToolResult("t", ToolResult{Error: Ptr(false), Result: "ok"})
			}
			in.End(EndOptions{ExtraProperties: map[string]any{tc.Input: "private"}})
			p := r.response(t)
			if tc.Scope == "tool" {
				p = r.props(t, "searched")
			}
			if got := p[tc.Input] == "private"; got == tc.Expected.Reserved {
				t.Errorf("reservation: custom value retained=%t, reserved=%t", got, tc.Expected.Reserved)
			}
		}) {
			passed++
		}
	}
	for n, tc := range fixture.CanonicalModels {
		if t.Run(fmt.Sprintf("model/%02d/%s", n, tc.Input), func(t *testing.T) {
			if got := canonicalModel(tc.Input); got != tc.Expected {
				t.Errorf("helper: got %q, want %q", got, tc.Expected)
			}
			r := setupAI(t, WithLLMCallEvents(true), WithPrices(map[string]ModelPrice{tc.Input: {TokenPrice: TokenPrice{InputPerMtok: 2, OutputPerMtok: 4}}}), WithCredits(func(c CreditCall) (*float64, error) {
				if c.Model == tc.Input {
					return Ptr(7.0), nil
				}
				return nil, nil
			}))
			_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			in.RecordCall(LLMCall{Model: tc.Input, InputTokens: 1000, OutputTokens: 500})
			in.End(EndOptions{})
			p := r.response(t)
			auditAssertSubset(t, p, map[string]any{"primary_model": tc.Expected, "total_cost_usd": 0.004, "total_credits": 7.0})
			usage := p["usage_breakdown"].([]map[string]any)
			if len(usage) != 1 || usage[0]["model"] != tc.Expected {
				t.Errorf("breakdown: %v", usage)
			}
			if got := r.props(t, "ai_llm_response_received")["model"]; got != tc.Expected {
				t.Errorf("call model: got %v, want %s", got, tc.Expected)
			}
		}) {
			passed++
		}
	}
	for _, tc := range fixture.Calls {
		// Go has no Claude Agent SDK adapter; FIXTURE-FORMAT.md marks those cases Node.js/Python only.
		if tc.Adapter == "claude-agent-sdk" {
			skipped++
			continue
		}
		if t.Run("calls/"+tc.Name, func(t *testing.T) {
			prices := map[string]ModelPrice{}
			for model, p := range tc.Input.Prices {
				prices[model] = ModelPrice{TokenPrice: TokenPrice{InputPerMtok: p.Input, OutputPerMtok: p.Output}}
			}
			r := setupAI(t, WithLLMCallEvents(true), WithPrices(prices), WithCredits(func(c CreditCall) (*float64, error) {
				if v, ok := tc.Input.Credits[c.Model]; ok {
					return &v, nil
				}
				return nil, nil
			}))
			_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			for _, report := range tc.Input.Reports {
				provider := report.Provider
				if provider == "" {
					provider = tc.Input.Provider
				}
				integration := "anthropic"
				if report.Source == "span" {
					integration = "tracing"
				}
				call := LLMCall{Provider: provider, Model: report.Model, ResponseID: report.ResponseID, Integration: integration, InputTokens: report.InputTokens, OutputTokens: report.OutputTokens}
				if report.StartedAt != nil {
					call.StartedAt = time.UnixMilli(*report.StartedAt)
				}
				if report.EndedAt != nil {
					call.EndedAt = time.UnixMilli(*report.EndedAt)
				}
				in.RecordCall(call)
			}
			in.End(EndOptions{})
			p := r.response(t)
			expected := map[string]any{}
			for k, v := range tc.Expected {
				if k != "call_events" && k != "groups" {
					expected[k] = v
				}
			}
			auditAssertSubset(t, p, expected)
			if got := len(r.named("ai_llm_response_received")); float64(got) != tc.Expected["call_events"] {
				t.Errorf("call events: got %d, want %v", got, tc.Expected["call_events"])
			}
			groups := []map[string]any{}
			for _, group := range p["usage_breakdown"].([]map[string]any) {
				groups = append(groups, map[string]any{"model": group["model"], "call_count": group["call_count"]})
			}
			auditAssertEqual(t, groups, tc.Expected["groups"])
		}) {
			passed++
		}
	}
	for _, tc := range fixture.Statuses {
		if t.Run("status/"+tc.Name, func(t *testing.T) {
			r := setupAI(t)
			_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			end := EndOptions{}
			for _, step := range tc.Input {
				switch step.Op {
				case "call":
					in.RecordCall(LLMCall{Model: "m", ErrorType: step.ErrorType, FinishReason: step.FinishReason})
				case "hint", "note":
					in.hintOutcome(step.Status, step.ErrorType)
				case "end":
					end = EndOptions{Status: step.Status, ErrorType: step.ErrorType}
				default:
					t.Fatalf("unknown status op %q", step.Op)
				}
			}
			in.End(end)
			p := r.response(t)
			actual := map[string]any{"response_status": p["response_status"]}
			if v, ok := p["error_type"]; ok {
				actual["error_type"] = v
			}
			auditAssertEqual(t, actual, tc.Expected)
		}) {
			passed++
		}
	}
	if skipped != 2 {
		t.Errorf("expected 2 Claude Agent SDK cases skipped, got %d", skipped)
	}
	t.Logf("audit normalisation: %d/%d passed, %d skipped", passed, total, skipped)
}

func auditAssertSubset(t *testing.T, actual, expected map[string]any) {
	t.Helper()
	for k, want := range expected {
		t.Run(k, func(t *testing.T) { auditAssertEqual(t, actual[k], want) })
	}
}

func auditAssertEqual(t *testing.T, actual, expected any) {
	t.Helper()
	roundTrip := func(v any) any {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var out any
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	got, want := roundTrip(actual), roundTrip(expected)
	if gn, ok := got.(float64); ok {
		if wn, ok := want.(float64); ok && math.Abs(gn-wn) <= 1e-9*math.Max(1, math.Abs(wn)) {
			return
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
