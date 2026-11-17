package ruddermcpgo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
	"github.com/rudderlabs/ai-sdk-go/internal/mcpbridge"
)

func maskedErrorMessage(t *testing.T, s string) string {
	t.Helper()
	out, ok := rudderstackai.MaskText(s)
	if !ok {
		t.Fatal("mask failed")
	}
	return out
}

func testMCPGoErrorMessageCapture(t *testing.T, connect func(*testing.T, *server.MCPServer) *sdk.ClientSession, observed bool) {
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
			p := waitProps(t, r, "mcp_tool_called")
			wantStatus := "error"
			if tc.success {
				wantStatus = "success"
			}
			if p["tool_status"] != wantStatus {
				t.Errorf("status=%v", p)
			}
			want := ""
			if tc.capture && observed && !tc.success {
				want = "Warehouse timed out for [email]"
			}
			if got, _ := p["error_message"].(string); got != want {
				t.Errorf("error_message=%q, want %q: %v", got, want, p)
			}
		})
	}
}

func testMCPGoErrorMessageProtocolErrors(t *testing.T, connect func(*testing.T, *server.MCPServer) *sdk.ClientSession, observed bool) {
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
				p := waitProps(t, r, tc.event)
				if p["tool_status"] != "error" {
					t.Errorf("status=%v", p)
				}
				want := ""
				if capture && observed {
					want = tc.want
				}
				if got, _ := p["error_message"].(string); got != want {
					t.Errorf("error_message=%q, want %q: %v", got, want, p)
				}
			}
		})
	}
}

func TestMCPGoErrorMessage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		connect  func(*testing.T, *server.MCPServer) *sdk.ClientSession
		observed bool
	}{
		{"stdio", func(t *testing.T, s *server.MCPServer) *sdk.ClientSession {
			return connectStdio(t, s, "2025-06-18", nil)
		}, false},
		{"http", func(t *testing.T, s *server.MCPServer) *sdk.ClientSession {
			return connectHTTP(t, s, "2025-06-18", nil)
		}, false},
		{"stdio observed", func(t *testing.T, s *server.MCPServer) *sdk.ClientSession {
			return connectStdioObserved(t, s)
		}, true},
		{"http observed", func(t *testing.T, s *server.MCPServer) *sdk.ClientSession {
			return connectHTTPObserved(t, s)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("capture", func(t *testing.T) { testMCPGoErrorMessageCapture(t, tc.connect, tc.observed) })
			t.Run("protocol", func(t *testing.T) { testMCPGoErrorMessageProtocolErrors(t, tc.connect, tc.observed) })
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

type capoffWriter struct {
	started chan struct{}
	release chan struct{}
	header  http.Header
}

func (w *capoffWriter) Header() http.Header { return w.header }
func (*capoffWriter) WriteHeader(int)       {}
func (w *capoffWriter) Write(p []byte) (int, error) {
	close(w.started)
	<-w.release
	return len(p), nil
}

type capoffClient struct{ *recorder }

func (c *capoffClient) Flush(context.Context) error {
	if len(c.named("mcp_tool_called")) != 1 {
		return errors.New("event missing at analytics flush")
	}
	return nil
}

func TestMCPGoErrorMessageNoWriteWait(t *testing.T) {
	for _, transport := range []string{"stdio", "http"} {
		for _, mode := range []string{"off-result", "off-rpc", "split-result", "split-rpc", "on-success"} {
			t.Run(transport+"/"+mode, func(t *testing.T) {
				if err := rudderstackai.Configure(rudderstackai.WithCaptureContent(false)); err != nil {
					t.Fatal(err)
				}
				s := server.NewMCPServer("no-write-wait-mcpgo", "1", server.WithToolCapabilities(true))
				s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					if strings.HasSuffix(mode, "rpc") {
						return nil, errors.New("failure")
					}
					return &mcp.CallToolResult{IsError: mode != "on-success", Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "response"}}}, nil
				})
				r := wrap(t, s, Options{})
				client := &capoffClient{r}
				opts := Options{Analytics: client, CaptureContent: mode == "on-success"}
				if strings.HasPrefix(mode, "split") {
					opts.CaptureContent = true
					opts.ContentCapture = &rudderstackai.ContentCapture{ErrorMessage: rudderstackai.Ptr(false)}
				}
				if err := WrapServer(s, opts); err != nil {
					t.Fatal(err)
				}
				if err := rudderstackai.Configure(rudderstackai.WithAnalytics(client)); err != nil {
					t.Fatal(err)
				}
				writer := &capoffWriter{started: make(chan struct{}), release: make(chan struct{}), header: make(http.Header)}
				release := sync.OnceFunc(func() { close(writer.release) })
				defer release()
				done := make(chan struct{})
				returned := make(chan struct{})
				waiting := make(chan bool, 1)
				handle := func(ctx context.Context, out io.Writer) {
					response := s.HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{}}}`))
					// Handler has returned; inspect observation before any write starts.
					waiting <- mcpbridge.ResponsesFrom(ctx).Waiting()
					close(returned)
					data, _ := json.Marshal(response)
					_, _ = out.Write(append(data, '\n'))
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if transport == "stdio" {
					observed, _, out := ObserveStdio(ctx, strings.NewReader(""), writer)
					go func() { defer close(done); handle(observed, out) }()
				} else {
					h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handle(r.Context(), w) }))
					req := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(ctx)
					go func() { defer close(done); h.ServeHTTP(writer, req) }()
				}
				select {
				case <-returned:
				case <-time.After(2 * time.Second):
					t.Fatal("handler never returned")
				}
				if <-waiting {
					t.Error("response observer/timer registered")
				}
				select {
				case <-writer.started:
				case <-time.After(2 * time.Second):
					t.Fatal("write never started")
				}
				deadline, stop := context.WithTimeout(context.Background(), time.Second)
				defer stop()
				if !rudderstackai.Flush(deadline) {
					t.Error("application Flush failed")
				}
				if !rudderstackai.CloseClient(deadline, client) {
					t.Error("application CloseClient failed")
				}
				if len(r.named("mcp_tool_called")) != 1 {
					t.Error("event missing before flush returned")
				}
				if len(r.named("mcp_tool_called")) == 1 {
					if _, ok := r.props(t, "mcp_tool_called")["error_message"]; ok {
						t.Error("unexpected error_message")
					}
				}
				select {
				case <-done:
					t.Error("write completed before release")
				default:
				}
				// Join before the next test changes the global configuration.
				release()
				<-done
			})
		}
	}
}
