package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	analytics "github.com/rudderlabs/analytics-go/v4"
)

type callbackContent struct{ invoke func() }

func (v callbackContent) MarshalJSON() ([]byte, error) { v.invoke(); return []byte(`"prompt"`), nil }

type callbackClassifierClient struct{ invoke func() }

func (c callbackClassifierClient) Classify(context.Context, string, string) (ClassifierReply, error) {
	c.invoke()
	return ClassifierReply{Text: `{"intent":"other","outcome":"resolved"}`}, nil
}

func TestSDK6111CallbackKindsReenter(t *testing.T) {
	for _, kind := range []string{"credits", "redact", "capture_marshaler", "analytics", "on_error", "event_when", "event_properties", "evaluator", "classifier", "classifier_client", "trace_id", "disconnected", "before_end", "config_option"} {
		for _, threaded := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/same", true: "/goroutine"}[threaded], func(t *testing.T) {
				r := setupAI(t, WithLLMCallEvents(true), WithCaptureContent(true))
				_, target := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
				var fired atomic.Bool
				progress := make(chan bool, 1)
				invoke := func() {
					if fired.CompareAndSwap(false, true) {
						progress <- review117Invoke(func() {
							target.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o", InputTokens: 7, Credits: Ptr(1.0)})
						}, threaded)
					}
				}
				opts := []ConfigOption{}
				switch kind {
				case "credits":
					opts = append(opts, WithCredits(func(CreditCall) (*float64, error) { invoke(); return Ptr(1.0), nil }))
				case "redact":
					opts = append(opts, WithRedactHook(func(s string) (string, error) { invoke(); return s, nil }))
				case "on_error":
					opts = append(opts, WithCredits(func(CreditCall) (*float64, error) { return nil, errors.New("hook") }), WithOnError(func(error, ErrorInfo) { invoke() }))
				case "event_when":
					opts = append(opts, WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "looked_up", When: func(map[string]any, any) bool { invoke(); return true }}}}))
				case "event_properties":
					opts = append(opts, WithEventRules(EventRules{Tools: map[string]ToolEventRule{"lookup": {Event: "looked_up", Properties: func(map[string]any, any) map[string]any { invoke(); return nil }}}}))
				case "evaluator":
					opts = append(opts, WithEvaluator(func(context.Context, EvaluationTurn) ([]Score, error) { invoke(); return nil, nil }))
				case "classifier":
					opts = append(opts, WithClassifier(&ClassifierOptions{Classify: func(context.Context, ClassifyTurn) (ClassifyResult, error) {
						invoke()
						return ClassifyResult{Intent: "other", Outcome: "resolved"}, nil
					}}))
				case "classifier_client":
					opts = append(opts, WithClassifier(&ClassifierOptions{Client: callbackClassifierClient{invoke}, Intents: []string{"other"}}))
				case "trace_id":
					opts = append(opts, WithTraceIDFrom(func(context.Context) string { invoke(); return "trace" }))
				case "config_option":
					opts = append(opts, func(*config, *pending) error { invoke(); return nil })
				}
				if err := Configure(opts...); err != nil {
					t.Fatal(err)
				}
				if kind == "analytics" {
					client := &review117Client{recorder: r}
					client.setCallback(func(analytics.Track) { invoke() })
					target.adapterAnalytics = client
				}
				if kind == "disconnected" {
					target.disconnected = func() bool { invoke(); return false }
				}
				if kind == "before_end" {
					target.onBeforeEnd(invoke)
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					if kind == "trace_id" {
						_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u"})
						i.End(EndOptions{})
						return
					}
					if kind == "event_when" || kind == "event_properties" {
						target.RecordToolCalls(ToolCall{ID: "t", Name: "lookup"})
						target.RecordToolResult("t", ToolResult{Result: "ok"})
					} else {
						c := LLMCall{Provider: "openai", Model: "gpt-4o", InputTokens: 10, OutputText: "answer"}
						if kind == "capture_marshaler" {
							c.InputMessages = []GenAIMessage{{Role: "user", Parts: []GenAIPart{{"type": "text", "content": callbackContent{invoke}}}}}
						}
						target.RecordCall(c)
					}
					target.End(EndOptions{})
				}()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("same-goroutine callback deadlocked")
				}
				select {
				case ok := <-progress:
					if !ok {
						t.Fatal("callback blocked another goroutine")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("callback not invoked")
				}
			})
		}
	}
}

func TestSDK6111MCPMapEvictionReenter(t *testing.T) {
	for _, threaded := range []bool{false, true} {
		t.Run(map[bool]string{false: "same", true: "goroutine"}[threaded], func(t *testing.T) {
			setupAI(t)
			old := servers.m
			servers.m = newBoundedMap[*serverState](1)
			t.Cleanup(func() { servers.m = old })
			progress := make(chan bool, 1)
			var fired atomic.Bool
			if err := Configure(WithOnError(func(error, ErrorInfo) {
				if fired.CompareAndSwap(false, true) {
					progress <- review117Invoke(func() { stateFor("second") }, threaded)
				}
			})); err != nil {
				t.Fatal(err)
			}
			stateFor("first")
			done := make(chan struct{})
			go func() { defer close(done); stateFor("second") }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("map callback deadlocked")
			}
			select {
			case ok := <-progress:
				if !ok {
					t.Fatal("map callback blocked")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("missing map notice")
			}
		})
	}
}

func TestSDK6111MCPCallbacksReenter(t *testing.T) {
	for _, kind := range []string{"identify", "client_info", "client_info_fallback", "tool_error", "mcp_redact"} {
		for _, threaded := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/same", true: "/goroutine"}[threaded], func(t *testing.T) {
				setupAI(t)
				s := workspaceServer(t.Name(), nil)
				var core *wrapped
				progress := make(chan bool, 1)
				var fired atomic.Bool
				invoke := func() {
					if fired.CompareAndSwap(false, true) {
						progress <- review117Invoke(func() { core.state() }, threaded)
					}
				}
				opts := Options{CaptureContent: true, RequestContext: RequestContextOptions{Enabled: true}}
				switch kind {
				case "identify":
					opts.Identify = func(context.Context, *auth.TokenInfo) (Identity, error) { invoke(); return Identity{UserID: "u"}, nil }
				case "client_info":
					opts.ClientInfo = func(context.Context, *auth.TokenInfo) (string, string) { invoke(); return "client", "1" }
				case "client_info_fallback":
					opts.ClientInfoFallback = func(context.Context, *auth.TokenInfo) (string, string) { invoke(); return "client", "1" }
				case "tool_error":
					opts.ClassifyToolError = func(string, *mcp.CallToolResult) string { invoke(); return "api_error" }
				case "mcp_redact":
					opts.Redact = func(text string) (string, error) { invoke(); return text, nil }
				}
				wrap(t, s, opts)
				core, _ = instances.get(s)
				done := make(chan struct{})
				go func() {
					defer close(done)
					_, _ = core.receiving(func(context.Context, string, mcp.Request) (mcp.Result, error) {
						return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "private"}}}, nil
					})(context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "list_destinations", Arguments: json.RawMessage(`{"request_context":{"call_reason":"private"}}`)}})
				}()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("MCP callback deadlocked")
				}
				select {
				case ok := <-progress:
					if !ok {
						t.Fatal("MCP callback blocked")
					}
				case <-time.After(time.Second):
					t.Fatal("missing MCP callback")
				}
			})
		}
	}
}

type callbackLifecycleClient struct {
	*recorder
	flushCallback, closeCallback func()
}

func (c *callbackLifecycleClient) Flush(context.Context) error {
	if c.flushCallback != nil {
		c.flushCallback()
	}
	return nil
}
func (c *callbackLifecycleClient) Close() error {
	if c.closeCallback != nil {
		c.closeCallback()
	}
	return nil
}
func TestSDK6111AnalyticsLifecycleCallbacksReenter(t *testing.T) {
	for _, kind := range []string{"flush", "close"} {
		for _, threaded := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/same", true: "/goroutine"}[threaded], func(t *testing.T) {
				r := setupAI(t)
				client := &callbackLifecycleClient{recorder: r}
				_, i := StartInteraction(context.Background(), InteractionOptions{UserID: "u", Analytics: client})
				progress := make(chan bool, 1)
				var fired atomic.Bool
				callback := func() {
					if fired.CompareAndSwap(false, true) {
						progress <- review117Invoke(func() { i.RecordCompaction() }, threaded)
					}
				}
				if kind == "flush" {
					client.flushCallback = callback
				} else {
					client.closeCallback = callback
				}
				i.RecordCall(LLMCall{Provider: "openai", Model: "gpt-4o", InputTokens: 10})
				i.End(EndOptions{})
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				ok := false
				if kind == "flush" {
					ok = Flush(ctx)
				} else {
					ok = CloseClient(ctx, client)
				}
				if !ok {
					t.Fatal("client callback did not finish")
				}
				select {
				case done := <-progress:
					if !done {
						t.Fatal("client callback blocked")
					}
				case <-time.After(time.Second):
					t.Fatal("callback not invoked")
				}
			})
		}
	}
}
