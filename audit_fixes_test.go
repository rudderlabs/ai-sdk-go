package rudderstackai

import (
	"context"
	"strings"
	"testing"
)

func propertyVariants(key string) []string {
	parts := strings.Split(key, "_")
	var pascal string
	for _, part := range parts {
		pascal += strings.ToUpper(part[:1]) + part[1:]
	}
	return []string{key, strings.ToUpper(key), strings.ToUpper(key[:1]) + key[1:],
		strings.ToLower(pascal[:1]) + pascal[1:], pascal,
		strings.ReplaceAll(key, "_", "-"), strings.ReplaceAll(key, "_", " ")}
}

func TestAuditContentPropertyVariants(t *testing.T) {
	setupAI(t)
	logs := captureLogs(t)
	for _, name := range []string{"prompt_text", "response_text", "feedback_text", "intent_summary", "explanation", "call_reason", "capability_detail"} {
		for _, key := range propertyVariants(name) {
			if propertyName(key) != name {
				t.Errorf("propertyName(%q) = %q, want %q", key, propertyName(key), name)
			}
			for range 2 {
				if !dropContentProperty(key, false) {
					t.Errorf("%q retained without capture", key)
				}
			}
			if dropContentProperty(key, true) {
				t.Errorf("%q dropped with capture", key)
			}
			if n := logs.count("custom property " + key + " was dropped"); n != 1 {
				t.Errorf("%q warnings = %d, want 1", key, n)
			}
		}
	}
	for _, key := range []string{"promptLength", "cardKinds", "PromptLength", "card-kinds"} {
		if dropContentProperty(key, false) {
			t.Errorf("unrelated key %q dropped", key)
		}
	}
	if propertyName("LLMCallCount") != "llm_call_count" || propertyName("TotalCostUSD") != "total_cost_usd" {
		t.Fatal("acronym boundaries not normalized")
	}
}

func TestAuditCustomPropertyBoundaries(t *testing.T) {
	for _, capture := range []bool{false, true} {
		for _, pause := range []bool{false, true} {
			r := setupAI(t, WithCaptureContent(!capture), WithEventRules(EventRules{Tools: map[string]ToolEventRule{
				"lookup": {Event: "looked_up", Properties: func(map[string]any, any) map[string]any {
					return map[string]any{"feedbackText": "custom", "promptLength": 12, "ToolStatus": "shadow", "LLMCallCount": 99, "ResultSizeBytes": 99}
				}},
			}}))
			ctx, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u", CaptureContent: Ptr(capture), AgentVersion: "v1"})
			in.RecordToolCalls(ToolCall{ID: "t", Name: "lookup", Result: "ok"})
			TrackAction(ctx, TrackActionOptions{ActionType: ActionCopiedResponse, UserID: "u", InteractionID: in.ID(), ConversationID: in.ConversationID(),
				ActionDetails: map[string]any{"responseText": "custom", "cardKinds": 2}})
			extra := map[string]any{"feedbackText": "custom", "promptLength": 12, "AgentVersion": "shadow"}
			for _, name := range []string{"response_status", "primary_model", "interaction_id", "prompt_text", "response_text"} {
				for _, key := range propertyVariants(name) {
					extra[key] = "shadow"
				}
			}
			if pause {
				in.Pause(PauseOptions{ExtraProperties: extra})
			} else {
				in.End(EndOptions{ExtraProperties: extra})
			}
			p := r.response(t)
			if (p["feedbackText"] != nil) != capture || p["promptLength"] != 12 || p["AgentVersion"] != nil || p["agent_version"] != "v1" {
				t.Fatal(p)
			}
			for key := range extra {
				if reservedProperties[propertyName(key)] && key != propertyName(key) && p[key] != nil {
					t.Errorf("reserved variant %q retained", key)
				}
			}
			tool := r.named("looked_up")[0].Properties
			if (tool["feedbackText"] != nil) != capture || tool["ToolStatus"] != nil || tool["LLMCallCount"] != nil || tool["ResultSizeBytes"] != nil || tool["promptLength"] != 12 {
				t.Fatal(tool)
			}
			details := r.named("ai_user_action")[0].Properties["action_details"].(map[string]any)
			if (details["responseText"] != nil) != capture || details["cardKinds"] != 2 {
				t.Fatal(details)
			}
		}
	}
}

func TestAuditAppErrorPreservesProviderOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		calls     []LLMCall
		end       EndOptions
		status    ResponseStatus
		errorType string
	}{
		{"timeout", []LLMCall{{ErrorType: "timeout"}}, EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusTimeout, "timeout"},
		{"rate limit", []LLMCall{{ErrorType: "rate_limit"}}, EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusError, "rate_limit"},
		{"API error", []LLMCall{{ErrorType: "api_error"}}, EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusError, "api_error"},
		{"cancelled", []LLMCall{{ErrorType: "cancelled"}}, EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusCancelled, "cancelled"},
		{"output cap", []LLMCall{{FinishReason: "max_tokens"}}, EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusIncomplete, "length"},
		{"no provider failure", nil, EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusError, "app_error"},
		{"successful retry", []LLMCall{{ErrorType: "timeout"}, {}}, EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusError, "app_error"},
		{"specific override", []LLMCall{{ErrorType: "timeout"}}, EndOptions{Status: StatusError, ErrorType: "context_limit"}, StatusError, "context_limit"},
		{"non-error override", []LLMCall{{ErrorType: "timeout"}}, EndOptions{Status: StatusSuccess}, StatusSuccess, ""},
		{"implicit status", []LLMCall{{ErrorType: "timeout"}}, EndOptions{ErrorType: "app_error"}, StatusTimeout, "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := setupAI(t)
			_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			for _, call := range tc.calls {
				in.RecordCall(call)
			}
			in.End(tc.end)
			p := r.response(t)
			if p["response_status"] != string(tc.status) || str(p["error_type"]) != tc.errorType {
				t.Fatal(p)
			}
		})
	}
}
