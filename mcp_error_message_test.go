package rudderstackai

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			source := tc.Result
			if source == nil {
				source = map[string]any{"error": tc.Error}
			}
			want := ""
			if tc.Expected != nil {
				want = *tc.Expected
			}
			if got := (&options{}).errorMessage(source); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestMCPErrorMessageCapture(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    Options
		success bool
		want    string
	}{
		{name: "capture", opts: Options{CaptureContent: true}, want: "Warehouse timed out for [email]"},
		{name: "default off"},
		{name: "success", opts: Options{CaptureContent: true}, success: true},
		{name: "hook", opts: Options{CaptureContent: true, Redact: func(s string) (string, error) { return strings.ReplaceAll(s, "Warehouse", "[system]"), nil }}, want: "[system] timed out for [email]"},
		{name: "hook error", opts: Options{CaptureContent: true, Redact: func(string) (string, error) { return "", errors.New("boom") }}},
		{name: "hook panic", opts: Options{CaptureContent: true, Redact: func(string) (string, error) { panic("boom") }}},
		{name: "hook sensitive", opts: Options{CaptureContent: true, Redact: func(string) (string, error) { return "jane@example.com", nil }}},
		{name: "hook empty", opts: Options{CaptureContent: true, Redact: func(string) (string, error) { return "", nil }}},
		{name: "mask off sensitive", opts: Options{CaptureContent: true, DisableRedaction: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupAI(t, WithCaptureContent(false))
			s := mcp.NewServer(&mcp.Implementation{Name: "errors-mcp", Version: "1"}, nil)
			s.AddTool(&mcp.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{IsError: !tc.success, Content: []mcp.Content{&mcp.TextContent{Text: "Warehouse timed out for jane@example.com"}}}, nil
			})
			r := wrap(t, s, tc.opts)
			cs := connect(t, s, "2025-06-18", nil)
			callTool(t, cs, "lookup", map[string]any{}, nil)
			p := r.props(t, "mcp_tool_called")
			if tc.want == "" {
				if _, ok := p["error_message"]; ok {
					t.Errorf("unexpected error_message: %v", p)
				}
			} else if p["error_message"] != tc.want {
				t.Errorf("error_message=%v, want %q", p["error_message"], tc.want)
			}
		})
	}
}

func TestMCPErrorMessageProtocolErrors(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[capture], func(t *testing.T) {
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
			r := wrap(t, s, Options{CaptureContent: capture})
			cs := connect(t, s, "2025-06-18", nil)
			ctx := context.Background()
			_, te := cs.CallTool(ctx, &mcp.CallToolParams{Name: "lookup"})
			_, re := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "db://x"})
			_, pe := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "summary"})
			for _, tc := range []struct {
				event, want string
				err         error
			}{
				{"mcp_tool_called", "workspace_id is required", te},
				{"mcp_resource_read", "Resource db://[email] not found", re},
				{"mcp_prompt_used", "Prompt store unavailable", pe},
			} {
				if tc.err == nil {
					t.Fatal("expected JSON-RPC error")
				}
				var wire *jsonrpc.Error
				if !errors.As(tc.err, &wire) {
					t.Fatalf("not a JSON-RPC error: %v", tc.err)
				}
				if got := maskBuiltIn(wire.Message); got != tc.want {
					t.Errorf("wire message=%q, want %q", got, tc.want)
				}
				p := r.props(t, tc.event)
				if p["tool_status"] != "error" {
					t.Errorf("status=%v", p)
				}
				if capture {
					if p["error_message"] != tc.want {
						t.Errorf("error_message=%v, want %q", p["error_message"], tc.want)
					}
				} else if _, ok := p["error_message"]; ok {
					t.Errorf("error_message with capture off: %v", p)
				}
			}
		})
	}
}

func TestMCPErrorMessageMaskingFailures(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		opts       Options
		config     []ConfigOption
		want       string
	}{
		{name: "global hook", text: "Warehouse jane@example.com", config: []ConfigOption{WithRedactHook(func(s string) (string, error) { return strings.ReplaceAll(s, "Warehouse", "[system]"), nil })}, want: "[system] [email]"},
		{name: "global masking off", text: "jane@example.com", config: []ConfigOption{WithRedaction(false)}},
		{name: "mask off safe", text: "plain failure", opts: Options{DisableRedaction: true}, want: "plain failure"},
		{name: "too long", text: strings.Repeat("x", maxMaskedCodePoints+1)},
		{name: "timeout", text: "failure", opts: Options{Redact: func(string) (string, error) { return redactionTimedOut, nil }}},
		{name: "sensitive after cap", text: "failure", opts: Options{Redact: func(string) (string, error) { return strings.Repeat("x", 201) + " jane@example.com", nil }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupAI(t, tc.config...)
			o := &options{Options: tc.opts}
			if got := o.errorMessage(map[string]any{"error": map[string]any{"message": tc.text}}); got != tc.want {
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
