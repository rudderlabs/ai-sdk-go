package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

func TestMCPErrorMessageFixtures(t *testing.T) {
	data, err := os.ReadFile("spec/fixtures/mcp-error-message-v0.1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name          string
			Result, Error map[string]any
			Expected      *string
			Write         string
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			source := map[string]any{"result": tc.Result}
			if tc.Result == nil {
				source = map[string]any{"error": tc.Error}
			}
			want := ""
			if tc.Expected != nil {
				want = *tc.Expected
			}
			got := errorMessage(source, MaskText)
			if tc.Write != "" {
				responses := mcpbridge.NewResponses()
				source["jsonrpc"], source["id"] = "2.0", "fixture"
				responses.Await("fixture", func(message map[string]any) { got = errorMessage(message, MaskText) })
				b, _ := json.Marshal(source)
				var out io.Writer = io.Discard
				if tc.Write == "fails" {
					out = fixtureFailingWriter{}
				}
				_, _ = mcpbridge.NewWriter(out, responses).Write(append(b, '\n'))
			}
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestMCPErrorMessageSourcesAndWhitespace(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{name: "BOM only", text: "\ufeff"},
		{name: "Unicode whitespace only", text: "\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"},
		{name: "BOM around text", text: "\ufeff failure \ufeff", want: "failure"},
		{name: "embedded BOM", text: "a\ufeffb", want: "a\ufeffb"},
		{name: "NEL is not JS whitespace", text: "\u0085", want: "\u0085"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, source := range []map[string]any{
				{"result": map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": tc.text}}}},
				{"error": map[string]any{"message": tc.text, "data": "ignored"}},
			} {
				if got := errorMessage(source, MaskText); got != tc.want {
					t.Errorf("got %q, want %q", got, tc.want)
				}
			}
		})
	}
	for _, source := range []map[string]any{
		{"result": map[string]any{"isError": true, "error": map[string]any{"message": "ignored"}}},
		{"result": map[string]any{"isError": false, "content": []any{map[string]any{"type": "text", "text": "success"}}}},
		{"error": map[string]any{"data": "ignored"}, "result": map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "ignored"}}}},
	} {
		if got := errorMessage(source, MaskText); got != "" {
			t.Errorf("unexpected message %q from %v", got, source)
		}
	}
}

func TestMCPErrorMessageCancelledOutcomes(t *testing.T) {
	result := map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "cancelled secret"}}}
	for _, tc := range []struct {
		name    string
		outcome mcpbridge.Outcome
	}{
		{"classified result", mcpbridge.Outcome{Result: result, ClassifiedError: "cancelled"}},
		{"protocol error", mcpbridge.Outcome{ErrorType: "cancelled"}},
		{"cancelled call", mcpbridge.Outcome{Result: result, Cancelled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := workspaceServer("cancelled-errors", nil)
			r := wrap(t, s, Options{CaptureContent: true})
			w, _ := instances.get(s)
			rc := w.begin(mcpbridge.Request{Ctx: context.Background()})
			rc.ToolCall("list_destinations", nil, false, nil, "")
			rc.ToolDone(tc.outcome)
			p := r.props(t, "mcp_tool_called")
			if p["error_type"] != "cancelled" || p["tool_status"] != "error" {
				t.Errorf("cancel fields: %v", p)
			}
			if _, ok := p["error_message"]; ok {
				t.Errorf("error_message on cancellation: %v", p)
			}
		})
	}
}

func TestMCPErrorMessageCapture(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		capture, success, rewrite, observe bool
	}{
		{name: "capture", capture: true},
		{name: "default off"},
		{name: "success", capture: true, success: true},
		{name: "later middleware", capture: true, rewrite: true},
		{name: "observed", capture: true, observe: true},
		{name: "observed default off", observe: true},
		{name: "observed success", capture: true, success: true, observe: true},
		{name: "observed later middleware", capture: true, rewrite: true, observe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupAI(t, WithCaptureContent(false))
			s := mcp.NewServer(&mcp.Implementation{Name: "errors-mcp", Version: "1"}, nil)
			s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{IsError: !tc.success, Content: []mcp.Content{&mcp.TextContent{Text: "original failure"}}}, nil
			})
			r := wrap(t, s, Options{CaptureContent: tc.capture})
			if tc.rewrite {
				s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
					return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
						res, err := next(ctx, method, req)
						if method == "tools/call" {
							return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "client-visible failure"}}}, nil
						}
						return res, err
					}
				})
			}
			cs := connectMaybeObserved(t, s, tc.observe)
			result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup"})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError == tc.success {
				t.Fatalf("unexpected result: %v", result)
			}
			if tc.rewrite && result.Content[0].(*mcp.TextContent).Text != "client-visible failure" {
				t.Fatalf("middleware did not rewrite result: %v", result)
			}
			p := r.wait(t, "mcp_tool_called", 1)[0].Properties
			wantStatus := "error"
			if tc.success {
				wantStatus = "success"
			}
			if p["tool_status"] != wantStatus {
				t.Errorf("status=%v", p)
			}
			want := ""
			if tc.capture && tc.observe && !tc.success {
				// The text the client received, not what the handler returned.
				want = result.Content[0].(*mcp.TextContent).Text
			}
			if got, _ := p["error_message"].(string); got != want {
				t.Errorf("error_message=%q, want %q: %v", got, want, p)
			}
		})
	}
}

func TestMCPErrorMessageProtocolErrors(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		capture, rewrite, observe bool
	}{
		{name: "off"}, {name: "on", capture: true}, {name: "later middleware", capture: true, rewrite: true},
		{name: "observed off", observe: true}, {name: "observed", capture: true, observe: true},
		{name: "observed later middleware", capture: true, rewrite: true, observe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupAI(t)
			s := mcp.NewServer(&mcp.Implementation{Name: "rpc-errors-mcp", Version: "1"}, nil)
			s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return nil, &jsonrpc.Error{Code: -32602, Message: "workspace_id is required", Data: json.RawMessage(`{"detail":"secret stack"}`)}
			})
			s.AddResource(&mcp.Resource{Name: "db", URI: "db://x"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return nil, &jsonrpc.Error{Code: -32602, Message: "Resource db://jane@example.com not found"}
			})
			s.AddPrompt(&mcp.Prompt{Name: "summary"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				return nil, &jsonrpc.Error{Code: -32603, Message: "Prompt store unavailable"}
			})
			r := wrap(t, s, Options{CaptureContent: tc.capture})
			if tc.rewrite {
				s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
					return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
						res, err := next(ctx, method, req)
						if err != nil {
							return nil, &jsonrpc.Error{Code: -32603, Message: "client-visible failure"}
						}
						return res, err
					}
				})
			}
			cs := connectMaybeObserved(t, s, tc.observe)
			ctx := context.Background()
			_, te := cs.CallTool(ctx, &mcp.CallToolParams{Name: "lookup"})
			_, re := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "db://x"})
			_, pe := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "summary"})
			for _, failure := range []struct {
				event, want string
				err         error
			}{
				{"mcp_tool_called", "workspace_id is required", te},
				{"mcp_resource_read", "Resource db://[email] not found", re},
				{"mcp_prompt_used", "Prompt store unavailable", pe},
			} {
				if failure.err == nil {
					t.Fatal("expected JSON-RPC error")
				}
				var wire *jsonrpc.Error
				if !errors.As(failure.err, &wire) {
					t.Fatalf("not a JSON-RPC error: %v", failure.err)
				}
				want := failure.want
				if tc.rewrite {
					want = "client-visible failure"
				}
				if got := maskBuiltIn(wire.Message); got != want {
					t.Errorf("wire message=%q, want %q", got, want)
				}
				p := r.wait(t, failure.event, 1)[0].Properties
				if p["tool_status"] != "error" {
					t.Errorf("status=%v", p)
				}
				if !tc.capture || !tc.observe {
					want = ""
				}
				if got, _ := p["error_message"].(string); got != want {
					t.Errorf("error_message=%q, want %q: %v", got, want, p)
				}
				if strings.Contains(mustJSON(p), "secret stack") {
					t.Errorf("error.data sent: %v", p)
				}
			}
		})
	}
}

func TestMCPErrorMessageMaskingFailures(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		mask       func(string) (string, bool)
		want       string
	}{
		{name: "built-in", text: "jane@example.com", mask: MaskText, want: "[email]"},
		{name: "too long", text: strings.Repeat("x", maxMaskedCodePoints+1), mask: MaskText},
		{name: "failed masking", text: "failure", mask: func(string) (string, bool) { return "", false }},
		{name: "empty", text: "failure", mask: func(string) (string, bool) { return "", true }},
		{name: "timeout", text: "failure", mask: func(string) (string, bool) { return redactionTimedOut, true }},
		{name: "sensitive after cap", text: "failure", mask: func(string) (string, bool) { return strings.Repeat("x", 201) + " jane@example.com", true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorMessage(map[string]any{"error": map[string]any{"message": tc.text}}, tc.mask); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMCPErrorMessageSuccessfulReadsAndPrompts(t *testing.T) {
	setupAI(t)
	s := mcp.NewServer(&mcp.Implementation{Name: "success-errors-mcp", Version: "1"}, nil)
	s.AddResource(&mcp.Resource{Name: "db", URI: "db://x"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "db://x", Text: "success body"}}}, nil
	})
	s.AddPrompt(&mcp.Prompt{Name: "summary"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "success prompt"}}}}, nil
	})
	r := wrap(t, s, Options{CaptureContent: true})
	cs := connect(t, s, "2025-06-18", nil)
	if _, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "db://x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "summary"}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"mcp_resource_read", "mcp_prompt_used"} {
		p := r.props(t, event)
		if p["tool_status"] != "success" {
			t.Errorf("success fields: %v", p)
		}
		if _, ok := p["error_message"]; ok {
			t.Errorf("error_message on success: %v", p)
		}
	}
}

type fixtureFailingWriter struct{}

func (fixtureFailingWriter) Write(p []byte) (int, error) {
	return len(p), errors.New("fixture write failed")
}
