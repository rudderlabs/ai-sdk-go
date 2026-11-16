package ruddermcpgo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

func maskedErrorMessage(t *testing.T, s string) string {
	t.Helper()
	out, ok := rudderstackai.MaskText(s)
	if !ok {
		t.Fatal("mask failed")
	}
	return out
}

func testMCPGoErrorMessageCapture(t *testing.T, connect func(*testing.T, *server.MCPServer) *sdk.ClientSession) {
	for _, tc := range []struct {
		name             string
		capture, success bool
	}{
		{name: "capture", capture: true}, {name: "default off"}, {name: "success", capture: true, success: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := rudderstackai.Configure(rudderstackai.WithCaptureContent(false)); err != nil {
				t.Fatal(err)
			}
			s := server.NewMCPServer("errors-mcpgo", "1", server.WithToolCapabilities(true))
			s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{IsError: !tc.success, Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "Warehouse timed out for jane@example.com"}}}, nil
			})
			r := wrap(t, s, Options{CaptureContent: tc.capture})
			cs := connect(t, s)
			result, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "lookup"})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError == tc.success {
				t.Fatalf("unexpected result: %v", result)
			}
			p := r.props(t, "mcp_tool_called")
			wantStatus := "error"
			if tc.success {
				wantStatus = "success"
			}
			if p["tool_status"] != wantStatus {
				t.Errorf("status=%v", p)
			}
			if _, ok := p["error_message"]; ok {
				t.Errorf("unexpected error_message: %v", p)
			}
		})
	}
}

func testMCPGoErrorMessageProtocolErrors(t *testing.T, connect func(*testing.T, *server.MCPServer) *sdk.ClientSession) {
	for _, capture := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[capture], func(t *testing.T) {
			if err := rudderstackai.Configure(rudderstackai.WithCaptureContent(false)); err != nil {
				t.Fatal(err)
			}
			s := server.NewMCPServer("rpc-errors-mcpgo", "1", server.WithToolCapabilities(true), server.WithResourceCapabilities(true, true), server.WithPromptCapabilities(true))
			s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return nil, errors.New("workspace_id is required")
			})
			s.AddResource(mcp.NewResource("db://x", "db"), func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				return nil, errors.New("Resource db://jane@example.com not found")
			})
			s.AddPrompt(mcp.NewPrompt("summary"), func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				return nil, errors.New("Prompt store unavailable")
			})
			r := wrap(t, s, Options{CaptureContent: capture})
			cs := connect(t, s)
			ctx := context.Background()
			_, te := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lookup"})
			_, re := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "db://x"})
			_, pe := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: "summary"})
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
				if got := maskedErrorMessage(t, wire.Message); got != tc.want {
					t.Errorf("wire message=%q, want %q", got, tc.want)
				}
				p := r.props(t, tc.event)
				if p["tool_status"] != "error" {
					t.Errorf("status=%v", p)
				}
				if _, ok := p["error_message"]; ok {
					t.Errorf("unexpected error_message: %v", p)
				}
			}
		})
	}
}

func TestMCPGoErrorMessage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		connect func(*testing.T, *server.MCPServer) *sdk.ClientSession
	}{
		{"stdio", func(t *testing.T, s *server.MCPServer) *sdk.ClientSession {
			return connectStdio(t, s, "2025-06-18", nil)
		}},
		{"http", func(t *testing.T, s *server.MCPServer) *sdk.ClientSession {
			return connectHTTP(t, s, "2025-06-18", nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("capture", func(t *testing.T) { testMCPGoErrorMessageCapture(t, tc.connect) })
			t.Run("protocol", func(t *testing.T) { testMCPGoErrorMessageProtocolErrors(t, tc.connect) })
			t.Run("success", func(t *testing.T) { testMCPGoErrorMessageSuccessfulReadsAndPrompts(t, tc.connect) })
			t.Run("cancel", func(t *testing.T) {
				s := server.NewMCPServer("cancel-errors-mcpgo", "1", server.WithToolCapabilities(true))
				started := make(chan struct{})
				s.AddTool(mcp.NewTool("slow"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					close(started)
					<-ctx.Done()
					return mcp.NewToolResultError("cancelled for jane@example.com"), nil
				})
				r := wrap(t, s, Options{CaptureContent: true})
				cs := tc.connect(t, s)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go func() { <-started; time.Sleep(20 * time.Millisecond); cancel() }()
				if _, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "slow"}); err == nil {
					t.Fatal("expected cancellation")
				}
				deadline := time.Now().Add(5 * time.Second)
				for len(r.named("mcp_tool_called")) == 0 {
					if time.Now().After(deadline) {
						t.Fatal("missing cancelled event")
					}
					time.Sleep(time.Millisecond)
				}
				p := r.props(t, "mcp_tool_called")
				if p["error_type"] != "cancelled" {
					t.Errorf("cancel fields: %v", p)
				}
				if _, ok := p["error_message"]; ok {
					t.Errorf("error_message on cancellation: %v", p)
				}
			})
		})
	}
}
func testMCPGoErrorMessageSuccessfulReadsAndPrompts(t *testing.T, connect func(*testing.T, *server.MCPServer) *sdk.ClientSession) {
	if err := rudderstackai.Configure(rudderstackai.WithCaptureContent(false)); err != nil {
		t.Fatal(err)
	}
	s := server.NewMCPServer("success-errors-mcpgo", "1", server.WithResourceCapabilities(true, true), server.WithPromptCapabilities(true))
	s.AddResource(mcp.NewResource("db://x", "db"), func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		return []mcp.ResourceContents{mcp.TextResourceContents{URI: "db://x", Text: "success body"}}, nil
	})
	s.AddPrompt(mcp.NewPrompt("summary"), func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []mcp.PromptMessage{{Role: "user", Content: mcp.TextContent{Type: "text", Text: "success prompt"}}}}, nil
	})
	r := wrap(t, s, Options{CaptureContent: true})
	cs := connect(t, s)
	if _, err := cs.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: "db://x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: "summary"}); err != nil {
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
