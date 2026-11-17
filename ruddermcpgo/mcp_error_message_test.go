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
				s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					if source == "rpc" {
						return nil, errors.New("handler failure")
					}
					return &mcp.CallToolResult{IsError: true}, nil
				})
				r := wrap(t, s, Options{CaptureContent: true})
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
				if len(r.named("mcp_tool_called")) != 0 {
					t.Fatal("event before successful write")
				}
				response["id"] = 2
				block["text"], rpc["message"] = "second failure", "second failure"
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
				}
				if len(seen) != 2 || seen["1"] != "first failure" || seen["2"] != "second failure" {
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
