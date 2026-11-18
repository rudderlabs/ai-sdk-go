package ruddermcpgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type reusedResponseWriter struct {
	started chan struct{}
	release chan struct{}
	header  http.Header
}

func (w *reusedResponseWriter) Header() http.Header { return w.header }
func (*reusedResponseWriter) WriteHeader(int)       {}
func (*reusedResponseWriter) Flush()                {}
func (w *reusedResponseWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "first failure") {
		close(w.started)
		<-w.release
	}
	return len(p), nil
}

func TestMCPGoReusedResponseSnapshot(t *testing.T) {
	for _, transport := range []string{"stdio", "http-json", "http-sse"} {
		for _, source := range []string{"rpc", "result"} {
			t.Run(transport+"/"+source, func(t *testing.T) {
				s := server.NewMCPServer("reuse", "1", server.WithToolCapabilities(true))
				result := &mcp.CallToolResult{IsError: true, StructuredContent: map[string]any{"errorType": "cancelled"}}
				s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					if source == "rpc" {
						return nil, errors.New("handler failure")
					}
					return result, nil
				})
				r := wrap(t, s, Options{CaptureContent: true, ClassifyToolError: func(_ string, got *mcp.CallToolResult) string {
					return got.StructuredContent.(map[string]any)["errorType"].(string)
				}})
				response := map[string]any{"jsonrpc": "2.0", "id": 1}
				block := map[string]any{"type": "text", "text": "first failure"}
				rpc := map[string]any{"code": -32603, "message": "first failure"}
				if source == "rpc" {
					response["error"] = rpc
				} else {
					response["result"] = map[string]any{"isError": true, "content": []any{block}}
				}
				w := &reusedResponseWriter{started: make(chan struct{}), release: make(chan struct{}), header: make(http.Header)}
				release := sync.OnceFunc(func() { close(w.release) })
				defer release()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				handled := make(chan struct{}, 2)
				handle := func(ctx context.Context, out io.Writer, id int) {
					request := map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": "lookup", "arguments": map[string]any{}}}
					data, err := json.Marshal(request)
					if err != nil {
						t.Error(err)
						return
					}
					s.HandleMessage(ctx, data)
					data, err = json.Marshal(response)
					if err != nil {
						t.Error(err)
						return
					}
					handled <- struct{}{}
					if transport == "http-sse" {
						data = append(append([]byte("data: "), data...), '\n', '\n')
					} else {
						data = append(data, '\n')
					}
					if _, err := out.Write(data); err != nil {
						t.Error(err)
					}
					if transport == "http-sse" {
						out.(http.Flusher).Flush()
					}
				}
				var send func(int)
				if transport == "stdio" {
					observed, _, out := ObserveStdio(ctx, strings.NewReader(""), w)
					send = func(id int) { handle(observed, out, id) }
				} else {
					h := ObserveHTTP(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
						id := 1
						if req.URL.Path == "/2" {
							id = 2
						}
						if transport == "http-sse" {
							out.Header().Set("Content-Type", "text/event-stream")
						}
						handle(req.Context(), out, id)
					}))
					send = func(id int) {
						out := w
						path := "/1"
						if id == 2 {
							path = "/2"
							out = &reusedResponseWriter{header: make(http.Header)}
						}
						h.ServeHTTP(out, httptest.NewRequest(http.MethodPost, path, nil).WithContext(ctx))
					}
				}
				first := make(chan struct{})
				go func() { defer close(first); send(1) }()
				select {
				case <-w.started:
				case <-ctx.Done():
					t.Fatal("first write never started")
				}
				<-handled
				if source == "rpc" && len(r.named("mcp_tool_called")) != 0 {
					t.Fatal("event before successful write")
				}
				response["id"] = 2
				block["text"], rpc["message"] = "second failure", "second failure"
				result.StructuredContent.(map[string]any)["errorType"] = "api_error"
				second := make(chan struct{})
				go func() { defer close(second); send(2) }()
				select {
				case <-handled:
				case <-ctx.Done():
					t.Fatal("second response never serialized")
				}
				release()
				<-first
				<-second
				seen := map[any]any{}
				for _, e := range r.named("mcp_tool_called") {
					seen[e.Properties["request_id"]] = e.Properties["error_message"]
					if source == "result" {
						expected := "api_error"
						if e.Properties["request_id"] == "1" {
							expected = "cancelled"
						}
						if e.Properties["error_type"] != expected {
							t.Fatalf("classification: %v", e.Properties)
						}
					}
				}
				var firstMessage any = "first failure"
				if source == "result" {
					firstMessage = nil
				}
				if len(seen) != 2 || seen["1"] != firstMessage || seen["2"] != "second failure" {
					t.Fatalf("request messages: %v", seen)
				}
			})
		}
	}
}

// A function is not JSON-serializable by itself; MarshalJSON makes this data writable.
type snapshotErrorData struct {
	Unserializable func()
	fail           bool
}

func (d snapshotErrorData) MarshalJSON() ([]byte, error) {
	if d.fail {
		return nil, errors.New("data marshal failed")
	}
	return []byte(`{"detail":"custom data"}`), nil
}

type snapshotResponseWriter struct {
	*httptest.ResponseRecorder
	fail    bool
	writes  int
	onWrite func()
}

func (w *snapshotResponseWriter) Write(p []byte) (int, error) {
	w.writes++
	w.onWrite()
	if w.fail {
		return 0, errors.New("write failed")
	}
	return w.ResponseRecorder.Write(p)
}

func TestMCPGoCustomMarshalerSnapshot(t *testing.T) {
	for _, transport := range []string{"stdio", "http-json", "http-sse"} {
		for _, source := range []string{"rpc", "rpc-marshal-failed", "result"} {
			for _, fail := range []bool{false, true} {
				if source == "rpc-marshal-failed" && fail {
					continue // Encoding fails before the writer is reached.
				}
				t.Run(transport+"/"+source+map[bool]string{false: "/written", true: "/write-failed"}[fail], func(t *testing.T) {
					s := server.NewMCPServer("custom-data", "1", server.WithToolCapabilities(true))
					s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
						if source != "result" {
							return nil, errors.New("first failure")
						}
						return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
							mcp.ImageContent{Type: "image", Data: "aW1hZ2U=", MIMEType: "image/png"},
							mcp.TextContent{Type: "text", Text: "first failure"},
						}}, nil
					})
					r := wrap(t, s, Options{CaptureContent: true})
					w := &snapshotResponseWriter{ResponseRecorder: httptest.NewRecorder(), fail: fail, onWrite: func() {
						if len(r.named("mcp_tool_called")) != 0 {
							t.Error("event before successful write")
						}
					}}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					var writeErr error
					handle := func(ctx context.Context, out io.Writer) {
						response := s.HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{}}}`))
						if source != "result" {
							rpc, ok := response.(mcp.JSONRPCError)
							if !ok {
								t.Fatalf("expected JSON-RPC error, got %T", response)
							}
							// Model a final transport response whose error includes application data.
							rpc.Error.Data = snapshotErrorData{Unserializable: func() {}, fail: source == "rpc-marshal-failed"}
							response = rpc
						}
						if len(r.named("mcp_tool_called")) != 0 {
							t.Fatal("event before serialization")
						}
						if transport == "http-sse" {
							data, err := json.Marshal(response)
							writeErr = err
							if err == nil {
								_, writeErr = out.Write(append(append([]byte("data: "), data...), '\n', '\n'))
							}
						} else {
							writeErr = json.NewEncoder(out).Encode(response)
						}
					}
					if transport == "stdio" {
						observed, _, out := ObserveStdio(ctx, strings.NewReader(""), w)
						handle(observed, out)
						mcpbridge.ResponsesFrom(observed).Close()
					} else {
						h := ObserveHTTP(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
							if transport == "http-sse" {
								out.Header().Set("Content-Type", "text/event-stream")
							}
							handle(req.Context(), out)
						}))
						h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(ctx))
					}
					failed := fail || source == "rpc-marshal-failed"
					if (writeErr != nil) != failed {
						t.Fatalf("write error: %v (want failure=%v)", writeErr, failed)
					}
					if source == "rpc-marshal-failed" && w.writes != 0 {
						t.Fatal("transport wrote despite serialization failure")
					}
					if !failed && source == "rpc" && !strings.Contains(w.Body.String(), `"data":{"detail":"custom data"}`) {
						t.Fatalf("custom data missing from wire response: %s", w.Body.String())
					}
					events := r.named("mcp_tool_called")
					if len(events) != 1 {
						t.Fatalf("got %d tool events, want exactly one", len(events))
					}
					p := events[0].Properties
					if p["tool_status"] != "error" {
						t.Fatalf("status: %v", p)
					}
					message, present := p["error_message"]
					if failed && present || !failed && (!present || message != "first failure") {
						t.Fatalf("error_message after write (failed=%v): %v", failed, p)
					}
				})
			}
		}
	}
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
				resultHeld := !strings.HasSuffix(mode, "rpc")
				if got := <-waiting; got != resultHeld {
					t.Errorf("waiting=%v want=%v", got, resultHeld)
				}
				select {
				case <-writer.started:
				case <-time.After(2 * time.Second):
					t.Fatal("write never started")
				}
				if resultHeld {
					release()
					<-done
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
					if !resultHeld {
						t.Error("write completed before release")
					}
				default:
				}
				// Join before the next test changes the global configuration.
				release()
				<-done
			})
		}
	}
}

func TestMCPGoHeldFieldSnapshot(t *testing.T) {
	fields := []string{"error.code", "error.message", "error.data", "isError", "content.text", "content.type", "content.size", "structuredContent", "structured.nextCursor", "structured.next_cursor", "meta.status", "structured.status", "structured.statusCode", "classifier.input", "classifier.output", "invalid_arguments", "declared.when", "declared.properties", "declared.output", "declared.content", "request.identity"}
	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			s := server.NewMCPServer("fields", "1", server.WithToolCapabilities(true))
			sc := map[string]any{"label": "rate_limit"}
			result := mcp.NewToolResultError("first failure")
			result.StructuredContent = sc
			if field == "structured.nextCursor" {
				sc["nextCursor"] = "first"
			}
			if field == "structured.next_cursor" {
				sc["next_cursor"] = "first"
			}
			if field == "meta.status" {
				result.Meta = &mcp.Meta{AdditionalFields: map[string]any{"com.rudderstack/http_status": 429}}
			}
			if field == "structured.status" {
				sc["status"] = 429
			}
			if field == "structured.statusCode" {
				sc["statusCode"] = 429
			}
			if field == "declared.content" {
				result.StructuredContent = nil
				result.Content[0] = mcp.TextContent{Type: "text", Text: `{"label":"rate_limit","message":"first failure"}`}
			}
			tool := mcp.NewTool("lookup")
			tool.InputSchema.Properties = map[string]any{"value": map[string]any{"type": "object", "const": map[string]any{"valid": true}}}
			s.AddTool(tool, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if strings.HasPrefix(field, "error.") {
					return nil, errors.New("first failure")
				}
				return result, nil
			})
			output := map[string]any{"nested": map[string]any{"value": "first"}}
			var liveArgs map[string]any
			classification := "rate_limit"
			classified := []string{}
			r := wrap(t, s, Options{CaptureContent: true, ClassifyToolError: func(_ string, got *mcp.CallToolResult) string {
				if got != result {
					t.Error("classifier lost live result")
				}
				if got.StructuredContent == nil {
					return ""
				}
				classified = append(classified, got.StructuredContent.(map[string]any)["label"].(string))
				if field == "classifier.input" {
					return got.StructuredContent.(map[string]any)["label"].(string)
				}
				if field == "classifier.output" {
					return classification
				}
				return ""
			}, EventRules: rudderstackai.EventRules{Tools: map[string]rudderstackai.ToolEventRule{"lookup": {
				Event: "lookup_failed", When: func(args map[string]any, _ any) bool {
					liveArgs = args
					return field != "declared.when" || args["value"].(map[string]any)["valid"] == false
				},
				Properties: func(args map[string]any, _ any) map[string]any { liveArgs = args; return output },
			}}}})
			w := &reusedResponseWriter{started: make(chan struct{}), release: make(chan struct{}), header: make(http.Header)}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			observed, _, out := ObserveStdio(ctx, strings.NewReader(""), w)
			request := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{"value":{"valid":false}}}}`)
			var response map[string]any
			var size int
			done := make(chan error, 1)
			go func() {
				reply := s.HandleMessage(observed, request)
				wire, err := json.Marshal(reply)
				if err != nil {
					done <- err
					return
				}
				if err = json.Unmarshal(wire, &response); err != nil {
					done <- err
					return
				}
				if payload, ok := response["result"].(map[string]any); ok {
					encoded, _ := json.Marshal(payload)
					size = len(encoded)
				}
				_, err = out.Write(append(wire, '\n'))
				done <- err
			}()
			select {
			case <-w.started:
			case <-ctx.Done():
				t.Fatal("write never started")
			}
			if len(r.named("mcp_tool_called")) != 0 {
				t.Fatal("event before write settled")
			}
			switch field {
			case "error.code":
				response["error"].(map[string]any)["code"] = -32602
			case "error.message":
				response["error"].(map[string]any)["message"] = "second failure"
			case "error.data":
				response["error"].(map[string]any)["data"] = map[string]any{"changed": true}
			case "isError":
				result.IsError = false
			case "content.text":
				result.Content[0] = mcp.TextContent{Type: "text", Text: "second failure"}
			case "content.type":
				result.Content[0] = mcp.ImageContent{Type: "image", Data: "second", MIMEType: "image/png"}
			case "content.size":
				result.Content = append(result.Content, mcp.TextContent{Type: "text", Text: strings.Repeat("x", 200)})
			case "structuredContent":
				result.StructuredContent = map[string]any{"label": "auth_error"}
			case "structured.nextCursor":
				delete(sc, "nextCursor")
			case "structured.next_cursor":
				delete(sc, "next_cursor")
			case "meta.status":
				result.Meta.AdditionalFields["com.rudderstack/http_status"] = 401
			case "structured.status":
				sc["status"] = 401
			case "structured.statusCode":
				sc["statusCode"] = 401
			case "classifier.input":
				sc["label"] = "auth_error"
			case "classifier.output":
				classification = "auth_error"
			case "invalid_arguments", "declared.when", "declared.properties":
				if liveArgs != nil {
					t.Fatal("rule ran before successful write")
				}
			case "declared.content":
				result.Content[0] = mcp.TextContent{Type: "text", Text: `{"label":"auth_error","message":"second failure"}`}
			case "declared.output":
				output["nested"].(map[string]any)["value"] = "second"
			case "request.identity":
				copy(request, []byte(strings.Replace(string(request), `"id":1`, `"id":2`, 1)))
			}

			close(w.release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			p := r.props(t, "mcp_tool_called")
			wantKind := "invalid_request"
			if strings.Contains(field, "status") || strings.HasPrefix(field, "classifier.") {
				wantKind = "rate_limit"
			}
			if strings.HasPrefix(field, "error.") {
				wantKind = "api_error"
			}
			wantMessage := "first failure"
			if field == "declared.content" {
				wantMessage = `{"label":"rate_limit","message":"first failure"}`
			}
			wantStatus := "error"

			if field == "meta.status" || field == "structured.status" || field == "structured.statusCode" {
				wantKind = "rate_limit"
			}
			kind, _ := p["error_type"].(string)
			message, _ := p["error_message"].(string)
			if p["request_id"] != "1" || p["tool_status"] != wantStatus || kind != wantKind || message != wantMessage {
				t.Fatalf("completion: %v", p)
			}
			if !strings.HasPrefix(field, "error.") && (p["result_size_bytes"] != size || p["result_tokens_estimate"] != size/4 || p["is_error_result"] != true || p["result_paginated"] != strings.HasPrefix(field, "structured.next")) {
				t.Fatalf("stats: %v", p)
			}
			expectedOutput := `{"value":"first"}`
			if field == "declared.output" {
				expectedOutput = `{"value":"second"}`
			}
			if mustJSON(r.props(t, "lookup_failed")["nested"]) != expectedOutput {
				t.Fatalf("declared: %v", r.props(t, "lookup_failed"))
			}
		})
	}
}

func TestMCPGoHTTPHeaderSnapshot(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprint(sse), func(t *testing.T) {
			s := server.NewMCPServer("header-snapshot", "1", server.WithToolCapabilities(true))
			s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultError("first failure"), nil
			})
			r := wrap(t, s, Options{CaptureContent: true})
			w := &reusedResponseWriter{started: make(chan struct{}), release: make(chan struct{}), header: make(http.Header)}
			h := ObserveHTTP(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
				contentType := "application/json"
				if sse {
					contentType = "text/event-stream"
				}
				out.Header().Set("Content-Type", contentType)
				reply := s.HandleMessage(req.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{}}}`))
				data, err := json.Marshal(reply)
				if err != nil {
					t.Error(err)
					return
				}
				if sse {
					data = append(append([]byte("data: "), data...), '\n', '\n')
				}
				if _, err := out.Write(data); err != nil {
					t.Error(err)
				}
				if sse {
					out.(http.Flusher).Flush()
				}
			}))
			done := make(chan struct{})
			go func() { defer close(done); h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", nil)) }()
			select {
			case <-w.started:
			case <-time.After(5 * time.Second):
				t.Fatal("write never started")
			}
			changed := "text/event-stream"
			if sse {
				changed = "application/json"
			}
			w.header.Set("Content-Type", changed)
			close(w.release)
			<-done
			if p := r.props(t, "mcp_tool_called"); p["error_message"] != "first failure" {
				t.Fatalf("header reuse changed completion: %v", p)
			}
		})
	}
}

func TestNonToolIsErrorOmitsErrorMessage(t *testing.T) {
	for _, method := range []string{"resources/read", "prompts/get"} {
		t.Run(method, func(t *testing.T) {
			s := server.NewMCPServer("non-tool-is-error-mcpgo", "1")
			r := wrap(t, s, Options{CaptureContent: true})
			tracked := lookup(s)
			responses := mcpbridge.NewResponses()
			defer responses.Close()
			ctx := context.Background()
			c := tracked.srv.Begin(mcpbridge.Request{Ctx: ctx, RequestID: 1, Responses: responses})
			c.Finished(method, "lookup", 0, "api_error")
			result := map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "delivered failure"}}}
			tracked.succeeded(ctx, &call{c: c, method: method, name: "lookup"}, result)
			event := "mcp_resource_read"
			if method == "prompts/get" {
				event = "mcp_prompt_used"
			}
			if len(r.named(event)) != 0 {
				t.Fatal("event before response write")
			}
			body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := mcpbridge.NewWriter(io.Discard, responses).Write(append(body, '\n')); err != nil {
				t.Fatal(err)
			}
			events := r.named(event)
			if len(events) != 2 {
				t.Fatalf("events: %v", events)
			}
			for _, e := range events {
				if _, ok := e.Properties["error_message"]; ok {
					t.Fatalf("non-tool error_message: %v", e.Properties)
				}
			}
			p := events[1].Properties
			if p["tool_status"] != "success" || p["result_size_bytes"] != len(mustJSON(result)) {
				t.Fatalf("success properties: %v", p)
			}
		})
	}
}

type serializerSequence struct{ calls *int }

func (s serializerSequence) MarshalJSON() ([]byte, error) {
	*s.calls++
	return fmt.Appendf(nil, `{"sequence":%d}`, *s.calls), nil
}

func TestMCPGoObserverPreservesSerializerOrder(t *testing.T) {
	for _, transport := range []string{"stdio", "http-json", "http-sse"} {
		for _, capture := range []bool{false, true} {
			for _, outcome := range []string{"rpc", "isError", "success"} {
				t.Run(fmt.Sprintf("%s/capture=%v/%s", transport, capture, outcome), func(t *testing.T) {
					calls := 0
					s := server.NewMCPServer("serializer-order", "1", server.WithToolCapabilities(true))
					s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
						if outcome == "rpc" {
							return nil, errors.New("first failure")
						}
						return &mcp.CallToolResult{IsError: outcome == "isError", Content: []mcp.Content{mcp.TextContent{Type: "text", Text: "first failure"}}, StructuredContent: serializerSequence{&calls}}, nil
					})
					wrap(t, s, Options{CaptureContent: capture})
					var received map[string]any
					handle := func(ctx context.Context, out io.Writer) {
						response := s.HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{}}}`))
						want := 0 // Telemetry waits until the response write succeeds.
						if outcome == "rpc" {
							want = 0
							rpc := response.(mcp.JSONRPCError)
							rpc.Error.Data = serializerSequence{&calls}
							response = rpc
						}
						if calls != want {
							t.Fatalf("before transport calls=%d, want %d", calls, want)
						}
						body, err := json.Marshal(response)
						if err != nil {
							t.Fatal(err)
						}
						if calls != want+1 {
							t.Fatalf("after transport calls=%d, want %d", calls, want+1)
						}
						if err := json.Unmarshal(body, &received); err != nil {
							t.Fatal(err)
						}
						if transport == "http-sse" {
							body = append(append([]byte("data: "), body...), '\n')
						}
						if _, err := out.Write(append(body, '\n')); err != nil {
							t.Fatal(err)
						}
						writtenCalls := 1

						if calls != writtenCalls {
							t.Fatalf("post-write calls=%d want=%d", calls, writtenCalls)
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
							if transport == "http-sse" {
								out.Header().Set("Content-Type", "text/event-stream")
							}
							handle(req.Context(), out)
						})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", nil))
					}
					var data map[string]any
					want := float64(1)
					if outcome == "rpc" {
						data = received["error"].(map[string]any)["data"].(map[string]any)
						want = 1
					} else {
						data = received["result"].(map[string]any)["structuredContent"].(map[string]any)
					}
					if data["sequence"] != want {
						t.Fatalf("client sequence=%v, want %v", data["sequence"], want)
					}
				})
			}
		}
	}
}
