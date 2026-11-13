package spanmodel_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/hook"
	"github.com/rudderlabs/ai-sdk-go/internal/spanmodel"
)

func wrapperSpan() spanmodel.Span {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return spanmodel.Span{Name: "chat m", Kind: 3, Context: spanmodel.Context{TraceID: strings.Repeat("1", 32), SpanID: strings.Repeat("2", 16), TraceState: "vendor=value"},
		Parent: spanmodel.Context{SpanID: strings.Repeat("3", 16)}, Start: start, End: start.Add(1500 * time.Microsecond), StatusCode: 1,
		Attributes: map[string]any{"gen_ai.operation.name": "chat", "gen_ai.provider.name": "openai", "gen_ai.request.model": "m", "rudderstack.span_source": "wrapper", "rudderstack.interaction_id": "interaction", "gen_ai.conversation.id": "conversation", "user.id": "user"},
		Scope:      spanmodel.Scope{Name: rudderstackai.LibraryName, Version: rudderstackai.Version}}
}

func TestWrapperSpanMappingUsageAndIdentity(t *testing.T) {
	hook.Reset()
	t.Cleanup(hook.Reset)
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown usage", true: "known zero usage"}[known], func(t *testing.T) {
			s := wrapperSpan()
			if known {
				s.Attributes["gen_ai.usage.input_tokens"] = 0
				s.Attributes["gen_ai.usage.output_tokens"] = int64(0)
			}
			track, err := spanmodel.ToTrack(s, spanmodel.Options{LibraryName: rudderstackai.LibraryName, LibraryVersion: rudderstackai.Version})
			if err != nil || track == nil {
				t.Fatalf("mapping failed: %v", err)
			}
			if track.UserId != "user" || track.MessageId != s.Context.TraceID+"-"+s.Context.SpanID || track.Event != "ai_otel_span" {
				t.Fatalf("wrapper identity lost: %+v", track)
			}
			p := track.Properties
			if p["parent_span_id"] != s.Parent.SpanID || p["duration_ms"] != 1.5 || p["span_kind"] != "client" || p["status_code"] != "ok" || p["interaction_id"] != "interaction" {
				t.Fatalf("wrapper lifecycle metadata lost: %v", p)
			}
			g := p["gen_ai"].(map[string]any)
			for _, key := range []string{"input_tokens", "output_tokens"} {
				value, present := g[key]
				if present != known || (present && value != 0) {
					t.Fatalf("%s=%v present=%v, known=%v", key, value, present, known)
				}
			}
		})
	}
}

func TestWrapperSpanContentPolicyAndException(t *testing.T) {
	hook.Reset()
	t.Cleanup(hook.Reset)
	if err := rudderstackai.Configure(rudderstackai.WithCaptureContent(true)); err != nil {
		t.Fatal(err)
	}
	for _, capture := range []bool{false, true} {
		t.Run(map[bool]string{false: "private", true: "captured"}[capture], func(t *testing.T) {
			s := wrapperSpan()
			s.StatusCode, s.StatusMessage = 2, "failure for person@example.com"
			s.Attributes["gen_ai.output.messages"] = []any{map[string]any{"role": "assistant", "parts": []any{map[string]any{"type": "text", "content": "person@example.com"}}}}
			s.Attributes["http.request.header.authorization"] = "secret"
			s.Events = []spanmodel.Event{{Name: "exception", Time: s.End, Attributes: map[string]any{"exception.type": "string", "exception.message": s.StatusMessage}}, {Name: "gen_ai.assistant.message", Time: s.End, Attributes: map[string]any{"content": "person@example.com"}}}
			before, _ := json.Marshal(s)
			track, err := spanmodel.ToTrack(s, spanmodel.Options{CaptureContent: &capture})
			if err != nil || track == nil {
				t.Fatalf("mapping failed: %v", err)
			}
			after, _ := json.Marshal(s)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("mapping mutated the input span")
			}
			encoded, err := json.Marshal(track.Properties["otlp"])
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "person@example.com") || strings.Contains(string(encoded), "secret") {
				t.Fatalf("span leaked content or transport credentials: %s", encoded)
			}
			if track.Properties["status_code"] != "error" || !strings.Contains(string(encoded), `"exception.type"`) {
				t.Fatal("privacy filtering lost the failure or exception category")
			}
			for _, key := range []string{"gen_ai.output.messages", "gen_ai.assistant.message", "exception.message"} {
				if strings.Contains(string(encoded), key) != capture {
					t.Fatalf("capture=%v: unexpected presence of %s: %s", capture, key, encoded)
				}
			}
		})
	}
}

func TestWrapperSpanBudgetTruncatesUnicodeAndDropsWhenTooSmall(t *testing.T) {
	hook.Reset()
	t.Cleanup(hook.Reset)
	s := wrapperSpan()
	s.Attributes["gen_ai.output.messages"] = strings.Repeat("界", 4000)
	capture := true
	track, err := spanmodel.ToTrack(s, spanmodel.Options{CaptureContent: &capture, ContentMasked: true, MaxEventBytes: 3000})
	if err != nil || track == nil {
		t.Fatalf("truncation failed: %v", err)
	}
	encoded, err := json.Marshal(track)
	if err != nil || len(encoded) > 3000 || track.Properties["truncated"] != true {
		t.Fatalf("event budget not respected: bytes=%d, truncated=%v, err=%v", len(encoded), track.Properties["truncated"], err)
	}
	if !utf8.Valid(encoded) || strings.Contains(string(encoded), `\ufffd`) || !strings.Contains(string(encoded), "...[truncated]") {
		t.Fatalf("truncation damaged Unicode or lost its marker: %s", encoded)
	}
	if s.Attributes["gen_ai.output.messages"] != strings.Repeat("界", 4000) {
		t.Fatal("truncation mutated provider content")
	}
	dropped, err := spanmodel.ToTrack(s, spanmodel.Options{CaptureContent: &capture, ContentMasked: true, MaxEventBytes: 1})
	if err != nil || dropped != nil {
		t.Fatalf("impossible budget must drop the span: %v, %v", dropped, err)
	}
}

func TestSummaryUsesNormalizedBinaryStrippedAttributes(t *testing.T) {
	hook.Reset()
	t.Cleanup(hook.Reset)
	for _, capture := range []bool{false, true} {
		for _, masked := range []bool{false, true} {
			s := wrapperSpan()
			s.Attributes["gen_ai.response.finish_reasons"] = []any{"stop", []byte("PRIVATE"), map[string]any{"bytes": []byte("PRIVATE"), "reason": "tool_calls"}}
			s.Attributes["gen_ai.usage.input_tokens"] = uint16(7)
			track, err := spanmodel.ToTrack(s, spanmodel.Options{CaptureContent: &capture, ContentMasked: masked})
			if err != nil || track == nil {
				t.Fatalf("mapping: %v", err)
			}
			encoded, err := json.Marshal(track)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "UFJJVkFURQ==") || strings.Contains(string(encoded), "bytesValue") {
				t.Fatalf("binary leaked: %s", encoded)
			}
			g := track.Properties["gen_ai"].(map[string]any)
			want := []any{"stop", map[string]any{"reason": "tool_calls"}}
			if !reflect.DeepEqual(g["finish_reasons"], want) || g["input_tokens"] != 7 {
				t.Fatalf("summary used raw attributes: %v", g)
			}
		}
	}
}

func TestOTelMapperKeepsTraceIdentityFallback(t *testing.T) {
	hook.Reset()
	t.Cleanup(hook.Reset)
	s := wrapperSpan()
	track, err := spanmodel.ToTrack(s, spanmodel.Options{})
	if err != nil || track == nil {
		t.Fatalf("mapping: %v", err)
	}
	if track.UserId != "user" || track.AnonymousId != s.Context.TraceID {
		t.Fatal("OTel identity fallback changed")
	}
}

func TestSpanByteBudgetIncludesTrackContextAndIdentity(t *testing.T) {
	hook.Reset()
	t.Cleanup(hook.Reset)
	s := wrapperSpan()
	s.Attributes["gen_ai.output.messages"] = strings.Repeat("x", 32000)
	capture := true
	options := spanmodel.Options{CaptureContent: &capture, ContentMasked: true, SDKIdentity: true,
		UserID: strings.Repeat("u", 2000), AnonymousID: strings.Repeat("a", 1000),
		MessageContext: map[string]any{"traits": map[string]any{"description": strings.Repeat("t", 2000)}}}
	track, err := spanmodel.ToTrack(s, options)
	if err != nil || track == nil {
		t.Fatalf("map: %v", err)
	}
	encoded, err := json.Marshal(track)
	if err != nil || len(encoded) > spanmodel.DefaultMaxEventBytes || track.Properties["truncated"] != true {
		t.Fatalf("whole event bytes=%d truncated=%v err=%v", len(encoded), track.Properties["truncated"], err)
	}
	if track.UserId != options.UserID || track.Context.Extra["traits"] == nil {
		t.Fatal("identity/context lost")
	}
	options.MessageContext["oversized"] = strings.Repeat("z", 32000)
	track, err = spanmodel.ToTrack(s, options)
	if err != nil || track != nil {
		t.Fatal("untruncatable track context exceeded budget")
	}
}
