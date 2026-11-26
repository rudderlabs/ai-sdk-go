package ruddermcpgo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	ai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

func TestExecutingDeliveredMCPResult(t *testing.T) {
	for _, transport := range []string{"stdio", "http", "sse"} {
		t.Run(transport, func(t *testing.T) {
			s := server.NewMCPServer("wire", "1", server.WithToolCapabilities(true))
			s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultText("INTERNAL_PRIVATE"), nil
			})
			r := wrap(t, s, Options{CaptureContent: true, EventRules: ai.EventRules{Tools: map[string]ai.ToolEventRule{"lookup": {Event: "done", Properties: func(_ map[string]any, v any) map[string]any { return map[string]any{"custom": v} }}}}})
			handle := func(ctx context.Context, out io.Writer) {
				reply := s.HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{}}}`))
				if reply == nil {
					t.Fatal("handler failed")
				}
				time.Sleep(40 * time.Millisecond)
				wire := []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"CLIENT_VISIBLE"}]}}`)
				if transport == "sse" {
					wire = append(append([]byte("data: "), wire...), '\n')
				}
				_, err := out.Write(append(wire, '\n'))
				if err != nil {
					t.Fatal(err)
				}
			}
			if transport == "stdio" {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ctx, _, out := ObserveStdio(ctx, strings.NewReader(""), io.Discard)
				handle(ctx, out)
				mcpbridge.ResponsesFrom(ctx).Close()
			} else {
				ObserveHTTP(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
					if transport == "sse" {
						out.Header().Set("Content-Type", "text/event-stream")
					}
					handle(req.Context(), out)
				})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
			}
			if r.props(t, "done")["custom"] != "CLIENT_VISIBLE" {
				t.Fatalf("content=%v", r.props(t, "done"))
			}
			if r.props(t, "mcp_tool_called")["duration_ms"].(int64) >= 30 {
				t.Fatal("write delay counted as handler duration")
			}
		})
	}
}
func TestExecutingLargeHTTPResponse(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[sse], func(t *testing.T) {
			s := server.NewMCPServer("large", "1", server.WithToolCapabilities(true))
			s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultText("ok"), nil
			})
			r := wrap(t, s, Options{CaptureContent: true})
			ObserveHTTP(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
				_ = s.HandleMessage(req.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{}}}`))
				wire, _ := json.Marshal(map[string]any{"id": 1, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", mcpbridge.MaxObserved+32)}}}})
				if sse {
					out.Header().Set("Content-Type", "text/event-stream")
					wire = append(append([]byte("data: "), wire...), '\n', '\n')
				}
				_, _ = out.Write(wire)
			})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
			if r.props(t, "mcp_tool_called")["tool_status"] != "success" {
				t.Fatalf("outcome=%v", r.props(t, "mcp_tool_called"))
			}
		})
	}
}
