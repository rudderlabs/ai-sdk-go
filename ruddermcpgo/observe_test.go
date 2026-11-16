package ruddermcpgo

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectStdioObserved(t *testing.T, s *server.MCPServer) *sdk.ClientSession {
	t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	listenCtx, in, out := ObserveStdio(ctx, serverIn, serverOut)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.NewStdioServer(s).Listen(listenCtx, in, out)
	}()
	cs, err := sdk.NewClient(testClient, nil).Connect(context.Background(),
		&sdk.IOTransport{Reader: clientIn, Writer: clientOut}, &sdk.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		cancel()
		_ = serverIn.Close()
		_ = clientIn.Close()
		<-done
	})
	return cs
}

func connectHTTPObserved(t *testing.T, s *server.MCPServer) *sdk.ClientSession {
	t.Helper()
	ts := httptest.NewServer(ObserveHTTP(server.NewStreamableHTTPServer(s)))
	t.Cleanup(ts.Close)
	cs, err := sdk.NewClient(testClient, nil).Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: ts.URL, DisableStandaloneSSE: true}, &sdk.ClientSessionOptions{ProtocolVersion: "2025-06-18"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// waitProps waits for one event: a failed request's event is sent after its response.
func waitProps(t *testing.T, r *recorder, event string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(r.named(event)) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	return r.props(t, event)
}

var observedConnectors = []struct {
	name    string
	connect func(*testing.T, *server.MCPServer) *sdk.ClientSession
}{
	{"stdio", connectStdioObserved},
	{"http", connectHTTPObserved},
}

// An OnAfterCallTool hook added after WrapServer changes the result in place; the event
// records the text the client received.
func TestObserveAfterCallToolHookRewrite(t *testing.T) {
	for _, tc := range observedConnectors {
		t.Run(tc.name, func(t *testing.T) {
			hooks := &server.Hooks{}
			s := server.NewMCPServer("rewrite-mcpgo", "1", server.WithToolCapabilities(true), server.WithHooks(hooks))
			s.AddTool(mcp.NewTool("lookup"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultError("original failure"), nil
			})
			r := wrap(t, s, Options{CaptureContent: true})
			hooks.AddAfterCallTool(func(_ context.Context, _ any, _ *mcp.CallToolRequest, result any) {
				if res, ok := result.(*mcp.CallToolResult); ok {
					res.Content = []mcp.Content{mcp.TextContent{Type: "text", Text: "client-visible failure"}}
				}
			})
			cs := tc.connect(t, s)
			result, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "lookup"})
			if err != nil {
				t.Fatal(err)
			}
			if text := result.Content[0].(*sdk.TextContent).Text; text != "client-visible failure" {
				t.Fatalf("hook did not rewrite: %q", text)
			}
			if got := waitProps(t, r, "mcp_tool_called")["error_message"]; got != "client-visible failure" {
				t.Errorf("error_message=%v", got)
			}
		})
	}
}

// A notification during the call upgrades the HTTP response to SSE; the response frame is read.
func TestObserveHTTPServerSentEvents(t *testing.T) {
	s := server.NewMCPServer("sse-mcpgo", "1", server.WithToolCapabilities(true), server.WithLogging())
	s.AddTool(mcp.NewTool("lookup"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_ = server.ServerFromContext(ctx).SendNotificationToClient(ctx, "notifications/message", map[string]any{"level": "info", "data": "working"})
		return mcp.NewToolResultError("streamed failure"), nil
	})
	r := wrap(t, s, Options{CaptureContent: true})
	cs := connectHTTPObserved(t, s)
	if _, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "lookup"}); err != nil {
		t.Fatal(err)
	}
	if got := waitProps(t, r, "mcp_tool_called")["error_message"]; got != "streamed failure" {
		t.Errorf("error_message=%v", got)
	}
}

func TestObserveConcurrentIDs(t *testing.T) {
	for _, tc := range observedConnectors {
		t.Run(tc.name, func(t *testing.T) {
			s := server.NewMCPServer("concurrent-mcpgo", "1", server.WithToolCapabilities(true))
			s.AddTool(mcp.NewTool("lookup", mcp.WithString("text")), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultError(req.GetString("text", "")), nil
			})
			r := wrap(t, s, Options{CaptureContent: true})
			cs := tc.connect(t, s)
			var wg sync.WaitGroup
			for i := range 4 {
				wg.Go(func() {
					if _, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "lookup", Arguments: map[string]any{"text": "failure " + strconv.Itoa(i)}}); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			deadline := time.Now().Add(5 * time.Second)
			for len(r.named("mcp_tool_called")) < 4 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			seen := map[any]bool{}
			for _, e := range r.named("mcp_tool_called") {
				seen[e.Properties["error_message"]] = true
			}
			if len(seen) != 4 || seen[nil] {
				t.Errorf("messages not matched by id: %v", seen)
			}
		})
	}
}

type plainWriter struct{ http.ResponseWriter }

func TestObserveHTTPKeepsWriterInterfaces(t *testing.T) {
	var flusher, plain bool
	h := ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := w.(http.Flusher)
		if r.URL.Path == "/plain" {
			plain = ok
		} else {
			flusher = ok
		}
		// http.ResponseController reaches the server's writer (write deadlines) through Unwrap.
		if _, ok := w.(interface{ Unwrap() http.ResponseWriter }); !ok {
			t.Error("no Unwrap")
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	h.ServeHTTP(plainWriter{httptest.NewRecorder()}, httptest.NewRequest(http.MethodPost, "/plain", nil))
	if !flusher || plain {
		t.Errorf("flusher=%v plain=%v", flusher, plain)
	}
}

type failingFlusher struct{ *httptest.ResponseRecorder }

func (failingFlusher) FlushError() error { return errFlush }

var errFlush = errors.New("flush failed")

func TestObserveHTTPReportsFlushErrors(t *testing.T) {
	var got error
	ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		got = http.NewResponseController(w).Flush()
	})).ServeHTTP(failingFlusher{httptest.NewRecorder()}, httptest.NewRequest(http.MethodPost, "/", nil))
	if !errors.Is(got, errFlush) {
		t.Errorf("flush error = %v", got)
	}
}

var errCommit = errors.New("commit failed")

// commitFailingWriter accepts a whole frame carrying marker, then reports that writing or
// flushing it failed; the client never receives it.
type commitFailingWriter struct {
	http.ResponseWriter
	marker  string
	onFlush bool
	mu      sync.Mutex
	pending bool
}

func (c *commitFailingWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.Contains(string(p), c.marker) {
		if !c.onFlush {
			return len(p), errCommit
		}
		c.pending = true
		return len(p), nil
	}
	return c.ResponseWriter.Write(p)
}

func (c *commitFailingWriter) Flush() { _ = c.FlushError() }

func (c *commitFailingWriter) FlushError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending {
		c.pending = false
		return errCommit
	}
	return http.NewResponseController(c.ResponseWriter).Flush()
}

// stdioCommitFailing swallows a line carrying marker and reports an error.
type stdioCommitFailing struct {
	io.Writer
	marker string
}

func (s stdioCommitFailing) Write(p []byte) (int, error) {
	if strings.Contains(string(p), s.marker) {
		return len(p), errCommit
	}
	return s.Writer.Write(p)
}

func TestObserveFailedCommitSendsWithoutMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		sse  bool
		// serve connects a client to s through a writer that fails to commit the marker frame.
		serve func(t *testing.T, s *server.MCPServer) *sdk.ClientSession
	}{
		{name: "stdio", serve: func(t *testing.T, s *server.MCPServer) *sdk.ClientSession {
			serverIn, clientOut := io.Pipe()
			clientIn, serverOut := io.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			listenCtx, in, out := ObserveStdio(ctx, serverIn, stdioCommitFailing{serverOut, "uncommitted failure"})
			done := make(chan struct{})
			go func() { defer close(done); _ = server.NewStdioServer(s).Listen(listenCtx, in, out) }()
			cs, err := sdk.NewClient(testClient, nil).Connect(context.Background(), &sdk.IOTransport{Reader: clientIn, Writer: clientOut}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cs.Close(); cancel(); _ = serverIn.Close(); _ = clientIn.Close(); <-done })
			return cs
		}},
		{name: "http json", serve: func(t *testing.T, s *server.MCPServer) *sdk.ClientSession {
			return connectHTTPThrough(t, s, false)
		}},
		{name: "http sse write", sse: true, serve: func(t *testing.T, s *server.MCPServer) *sdk.ClientSession {
			return connectHTTPThrough(t, s, false)
		}},
		{name: "http sse flush", sse: true, serve: func(t *testing.T, s *server.MCPServer) *sdk.ClientSession {
			return connectHTTPThrough(t, s, true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := server.NewMCPServer("commit-mcpgo", "1", server.WithToolCapabilities(true), server.WithLogging())
			s.AddTool(mcp.NewTool("lookup"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if tc.sse {
					_ = server.ServerFromContext(ctx).SendNotificationToClient(ctx, "notifications/message", map[string]any{"level": "info", "data": "working"})
				}
				return mcp.NewToolResultError("uncommitted failure"), nil
			})
			r := wrap(t, s, Options{CaptureContent: true})
			cs := tc.serve(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lookup"}); err == nil {
				t.Fatal("the client received the uncommitted response")
			}
			p := waitProps(t, r, "mcp_tool_called")
			if p["tool_status"] != "error" {
				t.Errorf("status: %v", p)
			}
			if got, ok := p["error_message"]; ok {
				t.Errorf("error_message from a response the client never got: %v", got)
			}
		})
	}
}

func connectHTTPThrough(t *testing.T, s *server.MCPServer, onFlush bool) *sdk.ClientSession {
	t.Helper()
	observed := ObserveHTTP(server.NewStreamableHTTPServer(s))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed.ServeHTTP(&commitFailingWriter{ResponseWriter: w, marker: "uncommitted failure", onFlush: onFlush}, r)
	}))
	t.Cleanup(ts.Close)
	cs, err := sdk.NewClient(testClient, nil).Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: ts.URL, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}
