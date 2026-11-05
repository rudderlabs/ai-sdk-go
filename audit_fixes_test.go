package rudderstackai

import (
	"context"
	"errors"
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
		strings.ReplaceAll(key, "_", "-"), strings.ReplaceAll(key, "_", " "),
		strings.ReplaceAll(key, "_", "--"), strings.ReplaceAll(key, "_", "  "),
		strings.ReplaceAll(key, "_", "."), "__" + key + "__",
		".-" + strings.ReplaceAll(key, "_", "_ .-- ") + "-."}
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
	for _, key := range []string{"promptLength", "promptSource", "responseTimeMs", "cardKinds", "PromptLength", "card-kinds"} {
		if dropContentProperty(key, false) {
			t.Errorf("unrelated key %q dropped", key)
		}
	}
	if propertyName("LLMCallCount") != "llm_call_count" || propertyName("TotalCostUSD") != "total_cost_usd" {
		t.Fatal("acronym boundaries not normalized")
	}
}

func TestAuditWarehousePropertyNames(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{"promptText", "prompt_text"}, {"PromptText", "prompt_text"}, {"HTTPStatus", "http_status"},
		{"prompt--text", "prompt_text"}, {"prompt  text", "prompt_text"}, {"Prompt-Text", "prompt_text"},
		{"__prompt_text__", "prompt_text"}, {"primary--model", "primary_model"},
		{"response2Text", "response2_text"}, {"HTTP2Status", "http2_status"},
		{"...", ""}, {"", ""}, {"_ .prompt\t\n/text!_", "prompt_text"},
	} {
		if got := propertyName(tc.key); got != tc.want {
			t.Errorf("propertyName(%q) = %q, want %q", tc.key, got, tc.want)
		}
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

func TestAuditStatusPrecedence(t *testing.T) {
	type step struct {
		call   *LLMCall
		status ResponseStatus
		err    string
	}
	call := func(err, finish string) step { return step{call: &LLMCall{ErrorType: err, FinishReason: finish}} }
	hint := func(status ResponseStatus, err string) step { return step{status: status, err: err} }
	app := hint(StatusError, "app_error")
	timeout := hint(StatusTimeout, "timeout")
	success := hint(StatusSuccess, "")
	for _, tc := range []struct {
		name   string
		steps  []step
		end    EndOptions
		status ResponseStatus
		err    string
	}{
		{"latest specific hint", []step{call("", ""), timeout, hint(StatusError, "rate_limit"), app}, EndOptions{}, StatusError, "rate_limit"},
		{"latest success hint", []step{call("timeout", ""), timeout, success}, EndOptions{}, StatusSuccess, ""},
		{"success then generic hint", []step{call("timeout", ""), success, app}, EndOptions{}, StatusError, "app_error"},
		{"success then generic end", []step{call("timeout", ""), success}, EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusError, "app_error"},
		{"specific hint then generic end", []step{call("timeout", ""), hint(StatusError, "rate_limit")}, EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusError, "rate_limit"},
		{"timeout hint after success", []step{call("", ""), timeout, app}, EndOptions{}, StatusTimeout, "timeout"},
		{"timeout hint then generic end", []step{call("", ""), timeout}, EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusTimeout, "timeout"},
		{"new incomplete hint", []step{call("timeout", ""), hint(StatusIncomplete, "max_turns"), app}, EndOptions{}, StatusIncomplete, "max_turns"},
		{"new call success", []step{call("timeout", ""), timeout, call("", "")}, EndOptions{}, StatusSuccess, ""},
		{"new call specific failure", []step{call("timeout", ""), timeout, call("api_error", ""), app}, EndOptions{}, StatusError, "api_error"},
		{"new call finish reason", []step{call("timeout", ""), timeout, call("", "length"), app}, EndOptions{}, StatusIncomplete, "length"},
		{"provider error beats finish reason", []step{call("rate_limit", "max_tokens"), app}, EndOptions{}, StatusError, "rate_limit"},
		{"generic alone", []step{app}, EndOptions{}, StatusError, "app_error"},
		{"generic then provider failure", []step{app, call("timeout", "")}, EndOptions{}, StatusTimeout, "timeout"},
		{"generic then successful call", []step{app, call("", "")}, EndOptions{}, StatusSuccess, ""},
		{"explicit success", []step{call("timeout", "")}, EndOptions{Status: StatusSuccess}, StatusSuccess, ""},
		{"explicit cancelled", []step{call("timeout", "")}, EndOptions{Status: StatusCancelled}, StatusCancelled, "cancelled"},
		{"explicit incomplete", []step{call("timeout", "")}, EndOptions{Status: StatusIncomplete, ErrorType: "max_turns"}, StatusIncomplete, "max_turns"},
		{"explicit blocked", []step{call("timeout", "")}, EndOptions{Status: StatusBlocked}, StatusBlocked, ""},
		{"specific app error", []step{call("timeout", "")}, EndOptions{Status: StatusError, ErrorType: "context_limit"}, StatusError, "context_limit"},
		{"explicit cancelled hint suppresses fallback", []step{call("timeout", ""), hint(StatusCancelled, "")}, EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusError, "app_error"},
		{"explicit end before provider success", []step{call("", "")}, EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusError, "app_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := setupAI(t)
			_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			for _, s := range tc.steps {
				if s.call != nil {
					in.RecordCall(*s.call)
				} else {
					in.hintOutcome(s.status, s.err)
				}
			}
			in.End(tc.end)
			p := r.response(t)
			if p["response_status"] != string(tc.status) || str(p["error_type"]) != tc.err {
				t.Fatal(p)
			}
		})
	}
	for _, finish := range []string{"max_tokens", "length", "content_filter"} {
		for _, markerFirst := range []bool{false, true} {
			for _, explicitEnd := range []bool{false, true} {
				for _, retry := range []bool{false, true} {
					r := setupAI(t)
					_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
					if markerFirst {
						in.hintOutcome(StatusError, "app_error")
					}
					in.RecordCall(LLMCall{FinishReason: finish})
					if !markerFirst {
						in.hintOutcome(StatusError, "app_error")
					}
					if retry {
						in.RecordCall(LLMCall{})
					}
					end := EndOptions{}
					if explicitEnd {
						end = EndOptions{Status: StatusError, ErrorType: "app_error"}
					}
					in.End(end)
					status, err := statusForErrorType(finishErrors[finish]), finishErrors[finish]
					if retry {
						status, err = StatusSuccess, ""
						if explicitEnd {
							status, err = StatusError, "app_error"
						}
					}
					p := r.response(t)
					if p["response_status"] != string(status) || str(p["error_type"]) != err {
						t.Fatalf("finish=%s markerFirst=%t explicitEnd=%t retry=%t: %v", finish, markerFirst, explicitEnd, retry, p)
					}
				}
			}
		}
	}
}

func TestAuditTaskLatestOutcome(t *testing.T) {
	for _, useHint := range []bool{false, true} {
		r := setupAI(t)
		appError := errors.New("application aborted")
		err := RunTask(context.Background(), "job", TaskOptions{Flush: Ptr(false)}, func(_ context.Context, in *Interaction) error {
			in.RecordCall(LLMCall{ErrorType: "timeout"})
			if useHint {
				in.hintOutcome(StatusIncomplete, "length")
			} else {
				in.RecordCall(LLMCall{FinishReason: "max_tokens"})
			}
			return appError
		})
		p := r.response(t)
		if err != appError || p["response_status"] != "incomplete" || p["error_type"] != "length" {
			t.Fatalf("useHint=%t err=%v response=%v", useHint, err, p)
		}
	}
}

func TestAuditEndMarkerBeforeProviderHook(t *testing.T) {
	for _, call := range []LLMCall{{}, {ErrorType: "timeout"}, {FinishReason: "length"}} {
		r := setupAI(t)
		_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
		in.onBeforeEnd(func() { in.RecordCall(call) })
		in.End(EndOptions{Status: StatusError, ErrorType: "app_error"})
		err := first(call.ErrorType, finishErrors[call.FinishReason], "app_error")
		p := r.response(t)
		if p["response_status"] != string(statusForErrorType(err)) || p["error_type"] != err {
			t.Fatal(p)
		}
	}
}
