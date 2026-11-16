package spanmodel_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/capture"
	"github.com/rudderlabs/ai-sdk-go/internal/spanmodel"
)

type kindSpan struct {
	Attributes map[string]any `json:"attributes"`
	Events     []struct {
		Name       string         `json:"name"`
		Attributes map[string]any `json:"attributes"`
	} `json:"events"`
	Status struct {
		Code    int    `json:"code"`
		Message string `json:"message,omitempty"`
	} `json:"status"`
}

// value decodes an OTLP AnyValue as fixture JSON.
func value(v map[string]any) any {
	switch {
	case v["stringValue"] != nil:
		return v["stringValue"]
	case v["intValue"] != nil:
		n, _ := strconv.ParseFloat(v["intValue"].(string), 64)
		return n
	case v["doubleValue"] != nil:
		return v["doubleValue"]
	case v["boolValue"] != nil:
		return v["boolValue"]
	case v["arrayValue"] != nil:
		out := []any{}
		for _, e := range v["arrayValue"].(map[string]any)["values"].([]any) {
			out = append(out, value(e.(map[string]any)))
		}
		return out
	}
	return nil
}

func attributes(list any) map[string]any {
	out := map[string]any{}
	for _, kv := range list.([]any) {
		kv := kv.(map[string]any)
		out[kv["key"].(string)] = value(kv["value"].(map[string]any))
	}
	return out
}

// The shared span-kind cases: each resolved kind set keeps exactly the expected content, masked.
func TestCapturePolicySpanKindsFixture(t *testing.T) {
	if err := rudderstackai.Configure(rudderstackai.WithRedaction(true)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../spec/fixtures/capture-policy-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Spans struct {
			ToolCallKeys []string `json:"tool_call_keys"`
			Input        kindSpan `json:"input"`
			Cases        []struct {
				Name  string `json:"name"`
				Spans struct {
					Messages  bool `json:"messages"`
					ToolCalls bool `json:"tool_calls"`
					HTTP      bool `json:"http"`
				} `json:"spans"`
				Input    *kindSpan `json:"input"`
				Expected kindSpan  `json:"expected"`
			} `json:"cases"`
		} `json:"spans"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Spans.Cases {
		t.Run(c.Name, func(t *testing.T) {
			in := fixture.Spans.Input
			if c.Input != nil {
				in = *c.Input
			}
			start := time.Unix(1700000000, 0)
			s := spanmodel.Span{
				Name: "chat", Kind: 3, Start: start, End: start.Add(time.Millisecond),
				Context:    spanmodel.Context{TraceID: strings.Repeat("1", 32), SpanID: strings.Repeat("2", 16)},
				Attributes: in.Attributes, StatusCode: in.Status.Code, StatusMessage: in.Status.Message,
			}
			for _, e := range in.Events {
				s.Events = append(s.Events, spanmodel.Event{Name: e.Name, Time: start, Attributes: e.Attributes})
			}
			kinds := capture.Spans{Messages: c.Spans.Messages, ToolCalls: c.Spans.ToolCalls, HTTP: c.Spans.HTTP}
			track, err := spanmodel.ToTrack(s, spanmodel.Options{Capture: &kinds})
			if err != nil || track == nil {
				t.Fatalf("ToTrack: %v %v", track, err)
			}
			b, _ := json.Marshal(track.Properties["otlp"])
			var otlp map[string]any
			_ = json.Unmarshal(b, &otlp)
			span := otlp["resourceSpans"].([]any)[0].(map[string]any)["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)[0].(map[string]any)
			got := map[string]any{"attributes": attributes(span["attributes"]), "status": span["status"]}
			events := []any{}
			for _, e := range span["events"].([]any) {
				e := e.(map[string]any)
				events = append(events, map[string]any{"name": e["name"], "attributes": attributes(e["attributes"])})
			}
			got["events"] = events
			want := map[string]any{}
			b, _ = json.Marshal(c.Expected)
			_ = json.Unmarshal(b, &want)
			b, _ = json.Marshal(got)
			var normalized map[string]any
			_ = json.Unmarshal(b, &normalized)
			if !reflect.DeepEqual(normalized, want) {
				gb, _ := json.MarshalIndent(normalized, "", " ")
				wb, _ := json.MarshalIndent(want, "", " ")
				t.Fatalf("got\n%s\nwant\n%s", gb, wb)
			}
		})
	}
}
