package conformance

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/kit"
)

var differentialEdges = strings.Fields("raw_json raw_invalid_json raw_null raw_empty raw_no_content unmapped_response response_getter_throws input_getter_throws content_marshaler_throws provider_panics transport_error context_cancelled stream_terminal stream_no_terminal stream_finish_reason stream_close stream_cancel")

type differentialStream struct{ *kit.Observer }

func (s *differentialStream) OnEvent(_ string, data []byte) {
	if string(data) == "[DONE]" || string(data) == "finish" {
		s.Done = true
		s.FinishReason = "stop"
	}
	in, out := 3, 2
	s.InputTokens, s.OutputTokens = &in, &out
}

type differentialContent struct{ Value string }

func (differentialContent) MarshalJSON() ([]byte, error) { panic("content getter") }

func differentialEdgeEvents(t *testing.T, provider, edge string) (beforeEnd, final []obj) {
	rec := &recorder{}
	configure(t, obj{"emit_llm_calls": true, "capture_content": true}, rec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, owner := rudderstackai.StartInteraction(ctx, rudderstackai.InteractionOptions{UserID: "edge-user", Classify: rudderstackai.Ptr(false)})
	defer owner.End(rudderstackai.EndOptions{})
	spec := kit.Spec{Provider: provider, Integration: "round7-differential", InputMessages: func(map[string]any) []rudderstackai.GenAIMessage {
		if edge == "input_getter_throws" {
			panic("input getter")
		}
		var content any = "hello"
		if edge == "content_marshaler_throws" {
			content = differentialContent{Value: "hello"}
		}
		return []rudderstackai.GenAIMessage{{Role: "user", Parts: []rudderstackai.GenAIPart{{"type": "text", "content": content}}}}
	}, OnResponse: func(c *kit.Call, _ map[string]any, _ *http.Response) bool {
		if edge == "response_getter_throws" {
			panic("response getter")
		}
		if edge == "unmapped_response" {
			return false
		}
		return c.RecordCall(rudderstackai.LLMCall{Provider: provider, Model: "m", InputTokens: 3, OutputTokens: 2, OutputText: "ok"})
	}, IsStream: func(*http.Request, map[string]any) bool { return strings.HasPrefix(edge, "stream_") }, NewStream: func(c *kit.Call, _ *http.Response) kit.StreamHandler {
		return &differentialStream{Observer: kit.NewObserver(c)}
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://provider.test", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	var resp *http.Response
	func() {
		defer func() {
			if value := recover(); value != nil && (edge != "provider_panics" || value != "provider panic") {
				t.Fatalf("unexpected panic: %v", value)
			}
		}()
		resp, err = spec.Intercept(req, func(*http.Request) (*http.Response, error) {
			switch edge {
			case "provider_panics":
				panic("provider panic")
			case "transport_error":
				return nil, errors.New("connection refused")
			case "context_cancelled":
				cancel()
				return nil, context.Canceled
			}
			status, data := 200, `{}`
			switch edge {
			case "raw_invalid_json":
				data = "invalid"
			case "raw_null":
				data = "null"
			case "raw_empty":
				data = ""
			case "raw_no_content":
				status, data = 204, ""
			}
			header := http.Header{}
			if strings.HasPrefix(edge, "stream_") {
				header.Set("Content-Type", "text/event-stream")
				data = "data: partial\n\n"
				if edge == "stream_terminal" {
					data += "data: [DONE]\n\n"
				}
				if edge == "stream_finish_reason" {
					data += "data: finish\n\n"
				}
			}
			return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(data))}, nil
		})
	}()
	if resp != nil {
		switch edge {
		case "stream_close":
			_ = resp.Body.Close()
		case "stream_cancel":
			cancel()
			_ = resp.Body.Close()
		default:
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
	}
	beforeEnd = normalise(rec.all(), false, true)
	owner.End(rudderstackai.EndOptions{})
	rudderstackai.Flush(context.Background())
	return beforeEnd, normalise(rec.all(), false, true)
}
