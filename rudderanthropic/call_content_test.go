package rudderanthropic

import (
	"encoding/json"
	"reflect"
	"testing"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func mapOf(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestInputMessagesSplitsToolResults(t *testing.T) {
	req := mapOf(t, `{
	  "system": [{"type":"text","text":"be brief"}],
	  "messages": [
	    {"role":"user","content":"hi"},
	    {"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"t1","name":"get","input":{"a":1}}]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},
	      {"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
	      {"type":"document","source":{"type":"url","url":"https://x/y.pdf","media_type":"application/pdf"}},
	      {"type":"mystery"}
	    ]}
	  ]}`)
	got := inputMessages(req)
	var gotJSON any
	b, _ := json.Marshal(got)
	_ = json.Unmarshal(b, &gotJSON)
	var want any
	_ = json.Unmarshal([]byte(`[
	  {"role":"system","parts":[{"type":"text","content":"be brief"}]},
	  {"role":"user","parts":[{"type":"text","content":"hi"}]},
	  {"role":"assistant","parts":[{"type":"text","content":"checking"},{"type":"tool_call","id":"t1","name":"get","arguments":{"a":1}}]},
	  {"role":"tool","parts":[{"type":"tool_call_response","id":"t1","response":"ab"}]},
	  {"role":"user","parts":[
	    {"type":"blob","modality":"image","mime_type":"image/png"},
	    {"type":"uri","modality":"document","uri":"https://x/y.pdf","mime_type":"application/pdf"},
	    {"type":"mystery"}]}
	]`), &want)
	if !reflect.DeepEqual(gotJSON, want) {
		t.Fatalf("got %s", b)
	}
}

func TestToolResultResponseKeepsNonTextParts(t *testing.T) {
	content := mapOf(t, `{"c":[{"type":"text","text":"x"},{"type":"image","source":{"type":"base64","media_type":"image/png"}}]}`)["c"]
	parts, ok := toolResultResponse(content).([]rudderstackai.GenAIPart)
	if !ok || len(parts) != 2 || parts[1]["type"] != "blob" {
		t.Fatalf("got %v", parts)
	}
	if s := toolResultResponse("plain"); s != "plain" {
		t.Fatalf("got %v", s)
	}
}
