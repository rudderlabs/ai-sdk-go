package rudderstackai

import (
	"context"
	"errors"
	"testing"
)

func TestAcceptOutput(t *testing.T) {
	capped := LLMCall{Provider: "anthropic", Model: "m", FinishReason: "max_tokens"}
	for _, tc := range []struct {
		name      string
		before    []LLMCall
		accept    bool
		after     []LLMCall
		end       EndOptions
		status    ResponseStatus
		errorType string
	}{
		{"unaccepted cap then app error", []LLMCall{capped}, false, nil, EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusIncomplete, "length"},
		{"unaccepted cap derived", []LLMCall{capped}, false, nil, EndOptions{}, StatusIncomplete, "length"},
		{"accepted cap then app error", []LLMCall{capped}, true, nil, EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusError, "app_error"},
		{"accepted cap derived", []LLMCall{capped}, true, nil, EndOptions{}, StatusSuccess, ""},
		{"later cap counts again", []LLMCall{capped}, true, []LLMCall{capped}, EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusIncomplete, "length"},
		{"later success stays success", []LLMCall{capped}, true, []LLMCall{{Provider: "anthropic", Model: "m", FinishReason: "end_turn"}}, EndOptions{}, StatusSuccess, ""},
		{"content filter is not a cap", []LLMCall{{Provider: "openai", Model: "m", FinishReason: "content_filter"}}, true, nil, EndOptions{}, StatusError, "content_filter"},
		{"provider error is kept", []LLMCall{{Provider: "openai", Model: "m", ErrorType: "rate_limit", FinishReason: "length"}}, true, nil, EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusError, "rate_limit"},
		{"accept before any call", nil, true, []LLMCall{capped}, EndOptions{}, StatusIncomplete, "length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := setupAI(t)
			_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			for _, c := range tc.before {
				in.RecordCall(c)
			}
			if tc.accept {
				in.AcceptOutput()
			}
			for _, c := range tc.after {
				in.RecordCall(c)
			}
			in.End(tc.end)
			p := r.response(t)
			if p["response_status"] != string(tc.status) || str(p["error_type"]) != tc.errorType {
				t.Fatalf("got %v/%v, want %s/%s", p["response_status"], p["error_type"], tc.status, tc.errorType)
			}
		})
	}
}

func TestAcceptOutputKeepsCallFinishReason(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true))
	_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	in.RecordCall(LLMCall{Provider: "anthropic", Model: "m", FinishReason: "max_tokens"})
	in.AcceptOutput()
	in.End(EndOptions{})
	if got := r.props(t, "ai_llm_response_received")["finish_reason"]; got != "max_tokens" {
		t.Fatalf("finish_reason = %v, want max_tokens", got)
	}
}

func TestRunTaskAcceptedCapThenFailure(t *testing.T) {
	r := setupAI(t)
	err := RunTask(context.Background(), "job", TaskOptions{Flush: Ptr(false)}, func(_ context.Context, in *Interaction) error {
		in.RecordCall(LLMCall{Provider: "anthropic", Model: "m", FinishReason: "max_tokens"})
		in.AcceptOutput()
		return errors.New("downstream lookup failed")
	})
	if err == nil {
		t.Fatal("want the task error back")
	}
	p := r.response(t)
	if p["response_status"] != string(StatusError) || p["error_type"] != "app_error" {
		t.Fatalf("got %v/%v, want error/app_error", p["response_status"], p["error_type"])
	}
}

func TestAcceptOutputClearsCapHint(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hint      string
		end       EndOptions
		status    ResponseStatus
		errorType string
	}{
		{"cap hint then success", "length", EndOptions{}, StatusSuccess, ""},
		{"cap hint then app error", "length", EndOptions{Status: StatusError, ErrorType: "app_error"}, StatusError, "app_error"},
		{"unrelated hint kept", "rate_limit", EndOptions{}, StatusError, "rate_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := setupAI(t)
			_, in := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
			in.RecordCall(LLMCall{Provider: "anthropic", Model: "m", FinishReason: "max_tokens"})
			in.hintOutcome(statusForErrorType(tc.hint), tc.hint)
			in.AcceptOutput()
			in.End(tc.end)
			p := r.response(t)
			if p["response_status"] != string(tc.status) || str(p["error_type"]) != tc.errorType {
				t.Fatalf("got %v/%v, want %s/%s", p["response_status"], p["error_type"], tc.status, tc.errorType)
			}
		})
	}
}
