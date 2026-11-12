package rudderstackai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func textMessage(role, text string) GenAIMessage {
	return GenAIMessage{Role: role, Parts: []GenAIPart{{"type": "text", "content": text}}}
}

func callEvents(t *testing.T, r *recorder) []map[string]any {
	t.Helper()
	Flush(context.Background())
	var out []map[string]any
	for _, m := range r.named("ai_llm_response_received") {
		out = append(out, m.Properties)
	}
	return out
}

func TestCallContentMaskedAndGated(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{
		Provider: "openai", Model: "gpt-4.1-mini", InputTokens: 1, ResponseID: "a",
		InputMessages:   []GenAIMessage{textMessage("user", "mail ann@example.com")},
		OutputText:      "to bob@example.com",
		OutputToolCalls: []GenAIToolCall{{Name: "look", Arguments: `{"e":"cy@example.com"}`}, {Name: "noop"}, {}},
	})
	i.End(EndOptions{})
	got := callEvents(t, r)
	if len(got) != 1 {
		t.Fatalf("events %v", got)
	}
	p := got[0]
	if p["call_index"] != 1 || p["output_text"] != "to [email]" {
		t.Fatalf("%v", p)
	}
	msgs := p["input_messages"].([]any)
	if msgs[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["content"] != "mail [email]" {
		t.Fatalf("%v", msgs)
	}
	calls := p["output_tool_calls"].([]any)
	if len(calls) != 2 || calls[0].(map[string]any)["arguments"].(map[string]any)["e"] != "[email]" || calls[1].(map[string]any)["arguments"] != nil {
		t.Fatalf("%v", calls)
	}
}

func TestCallContentOffWithoutCapture(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Provider: "openai", Model: "m", OutputText: "secret", InputMessages: []GenAIMessage{textMessage("user", "secret")}})
	i.End(EndOptions{})
	p := callEvents(t, r)[0]
	for _, k := range []string{"input_messages", "output_text", "output_tool_calls", "input_truncated"} {
		if _, ok := p[k]; ok {
			t.Fatalf("%s sent: %v", k, p)
		}
	}
	if p["call_index"] != 1 {
		t.Fatal(p)
	}
}

func TestCallIndexSurvivesPauseAndTwin(t *testing.T) {
	for _, restore := range []bool{false, true} {
		r := setupAI(t, WithLLMCallEvents(true))
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
		i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "1"})
		i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "2", CallCount: 3})
		s, ok := i.Pause(PauseOptions{})
		if !ok || s.CallSequence == nil || *s.CallSequence != 4 {
			t.Fatalf("snapshot %+v", s)
		}
		i = round3Resume(t, i, s, restore)
		i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "3"})
		i.End(EndOptions{})
		var got []any
		for _, p := range callEvents(t, r) {
			got = append(got, p["call_index"])
		}
		if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 5 {
			t.Fatalf("restore=%v call_index %v", restore, got)
		}
	}
}

func TestCallSequenceOnlyWithCallEvents(t *testing.T) {
	setupAI(t)
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Provider: "openai", Model: "m"})
	if s, _ := i.Pause(PauseOptions{}); s.CallSequence != nil {
		t.Fatal("call_sequence written without call events")
	}
}

func TestFoldedCallsHaveNoContent(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true), WithLimits(Limits{MaxCallsPerInteraction: 1}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	for n := 0; n < 3; n++ {
		i.RecordCall(LLMCall{Provider: "openai", Model: "m", OutputText: "hi"})
	}
	i.End(EndOptions{})
	got := callEvents(t, r)
	if len(got) != 3 || got[0]["output_text"] != "hi" || got[1]["call_index"] != 2 || got[2]["call_index"] != 3 {
		t.Fatalf("%v", got)
	}
	for _, p := range got[1:] {
		if _, ok := p["output_text"]; ok {
			t.Fatal("folded event carries content")
		}
	}
}

func TestCallContentHookFailureDropsField(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true), WithRedactHook(func(s string) (string, error) {
		if strings.Contains(s, "boom") {
			return "", context.Canceled
		}
		return s, nil
	}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Provider: "openai", Model: "m", OutputText: "boom", InputMessages: []GenAIMessage{textMessage("user", "ok")}})
	i.End(EndOptions{})
	p := callEvents(t, r)[0]
	if _, ok := p["output_text"]; ok {
		t.Fatal("withheld output_text sent")
	}
	if p["input_messages"] == nil {
		t.Fatal("input_messages lost")
	}
}

func TestCallContentLongStringShortened(t *testing.T) {
	r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.RecordCall(LLMCall{Provider: "openai", Model: "m", InputMessages: []GenAIMessage{textMessage("user", strings.Repeat("é", 40000))}})
	i.End(EndOptions{})
	p := callEvents(t, r)[0]
	if p["input_truncated"] != true {
		t.Fatal(p["input_truncated"])
	}
	if byteSize(p) > maxPropertiesBytes {
		t.Fatalf("event %d bytes", byteSize(p))
	}
}

func TestTrimInputMessagesDropsOldestAfterSystem(t *testing.T) {
	msg := func(role, text string) any {
		return map[string]any{"role": role, "parts": []any{map[string]any{"type": "text", "content": text}}}
	}
	in := []any{msg("system", "S"), msg("user", strings.Repeat("a", 100)), msg("assistant", strings.Repeat("b", 100)), msg("user", "last")}
	out, ok := trimInputMessages(in, 150)
	if !ok || len(out) != 3 || out[0].(map[string]any)["role"] != "system" || out[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["content"] != "last" {
		t.Fatalf("%v", out)
	}
}

func TestTrimInputMessagesShortensSystemLast(t *testing.T) {
	msg := func(role, text string) any {
		return map[string]any{"role": role, "parts": []any{map[string]any{"type": "text", "content": text}}}
	}
	text := func(m any) string { return m.(map[string]any)["parts"].([]any)[0].(map[string]any)["content"].(string) }
	out, ok := trimInputMessages([]any{msg("system", strings.Repeat("s", 50)), msg("user", strings.Repeat("u", 50))}, 10)
	if !ok || text(out[1]) != strings.Repeat("u", 40) || text(out[0]) != strings.Repeat("s", 50) {
		t.Fatalf("%v", out)
	}
	out, ok = trimInputMessages([]any{msg("system", strings.Repeat("s", 50)), msg("user", "u")}, 10)
	if !ok || text(out[1]) != "" || text(out[0]) != strings.Repeat("s", 41) {
		t.Fatalf("%v", out)
	}
}

func TestDataURIPartBecomesBlob(t *testing.T) {
	c := captureCallContent([]GenAIMessage{{Role: "user", Parts: []GenAIPart{
		{"type": "uri", "modality": "image", "uri": "DATA:image/png;base64,AAAA"},
		{"type": "blob", "modality": "audio", "mime_type": "audio/wav", "content": "AAAA"},
	}}}, "", nil)
	parts := c.inputMessages[0].(map[string]any)["parts"].([]any)
	if p := parts[0].(map[string]any); p["type"] != "blob" || p["mime_type"] != "image/png" || p["uri"] != nil {
		t.Fatalf("%v", p)
	}
	if p := parts[1].(map[string]any); p["content"] != nil {
		t.Fatalf("%v", p)
	}
}

func TestCallIndexPersistsInPendingCall(t *testing.T) {
	for _, restore := range []bool{false, true} {
		r := setupAI(t, WithLLMCallEvents(true))
		client := &pauseReentrantClient{recorder: r}
		_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
		i.RecordCall(LLMCall{Provider: "openai", Model: "closing", ResponseID: "0"})
		// Accepted while the pause finishes: carried in the snapshot, not emitted yet.
		client.reenter = func() {
			i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "1", CallCount: 2})
			i.RecordCall(LLMCall{Provider: "openai", Model: "m", ResponseID: "2"})
		}
		s, ok := i.Pause(PauseOptions{})
		if !ok {
			t.Fatal("pause")
		}
		var indexes []int
		for _, p := range s.PendingCalls {
			indexes = append(indexes, int(pendingNumber(p, "call_index")))
		}
		if len(indexes) != 2 || indexes[0] != 2 || indexes[1] != 4 {
			t.Fatalf("snapshot call_index %v", indexes)
		}
		i = round3Resume(t, i, s, restore)
		if len(i.calls) != 2 || i.calls[0].callIndex != 2 || i.calls[1].callIndex != 4 {
			t.Fatalf("restore=%v: %+v", restore, i.calls)
		}
	}
}

func TestCallContentNotMaskedForEndedInteraction(t *testing.T) {
	calls := 0
	setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true), WithRedactHook(func(s string) (string, error) {
		calls++
		return s, nil
	}))
	_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
	i.End(EndOptions{})
	i.RecordCall(LLMCall{Provider: "openai", Model: "m", OutputText: "late"})
	if calls != 0 {
		t.Fatalf("redact hook ran %d times for an ended interaction", calls)
	}
}

func TestStripBinary(t *testing.T) {
	got := stripBinary(map[string]any{
		"type": "image", "mime_type": "image/png", "data": "AAAA", "bytes": []byte{1}, "name": "x",
		"raw":  map[string]any{"0": 1, "1": 255},
		"buf":  map[string]any{"type": "Buffer", "data": []any{1, 2}},
		"url":  "DATA:image/png;base64,AAAA",
		"keep": map[string]any{"data": "kept", "n": 1},
		"list": []any{[]byte{1}, "ok", map[string]any{"0": 1}},
	}).(map[string]any)
	if _, ok := got["data"]; ok || got["name"] != "x" || got["url"] != "data:image/png" {
		t.Fatalf("%v", got)
	}
	for _, k := range []string{"bytes", "raw", "buf"} {
		if _, ok := got[k]; ok {
			t.Fatalf("%s kept: %v", k, got)
		}
	}
	if got["keep"].(map[string]any)["data"] != "kept" {
		t.Fatalf("%v", got["keep"])
	}
	if l := got["list"].([]any); len(l) != 1 || l[0] != "ok" {
		t.Fatalf("%v", l)
	}
}

func TestToolArgumentsAreStripped(t *testing.T) {
	c := captureCallContent(nil, "", []GenAIToolCall{{Name: "t", Arguments: `{"image":{"media_type":"image/png","data":"AAAA"}}`}})
	args := c.outputToolCalls[0].(map[string]any)["arguments"].(map[string]any)["image"].(map[string]any)
	if _, ok := args["data"]; ok {
		t.Fatalf("%v", args)
	}
}

type typedPayload struct {
	Name string `json:"name"`
	Blob []byte `json:"blob"`
	Raw  json.RawMessage
	Skip string `json:"-"`
}

func TestTypedContainersNeverEncodeBytes(t *testing.T) {
	typed := map[string][]byte{"payload": []byte("PRIVATE")}
	nested := []map[string][]byte{{"p": []byte("PRIVATE")}}
	st := typedPayload{Name: "n", Blob: []byte("PRIVATE"), Raw: json.RawMessage(`{"k":1}`), Skip: "x"}
	c := captureCallContent(
		[]GenAIMessage{{Role: "tool", Parts: []GenAIPart{
			{"type": "tool_call_response", "id": "1", "response": typed},
			{"type": "text", "content": "hi", "extra": nested},
			{"type": "tool_call_response", "response": &st},
			{"type": "text", "content": [3]byte{1, 2, 3}},
		}}},
		"", []GenAIToolCall{{Name: "t", Arguments: typed}, {Name: "u", Arguments: map[string]any{"deep": []any{typed, st}}}},
	)
	b, _ := json.Marshal(map[string]any{"in": c.inputMessages, "out": c.outputToolCalls})
	got := string(b)
	for _, leaked := range []string{"PRIVATE", "UFJJVkFURQ", "AQID"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("%s leaked: %s", leaked, got)
		}
	}
	if !strings.Contains(got, `"name":"n"`) || !strings.Contains(got, `"Raw":{"k":1}`) || strings.Contains(got, `"Skip"`) {
		t.Fatalf("struct lost: %s", got)
	}
}
