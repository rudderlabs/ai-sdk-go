package ruddermcpgo

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	rudderstackai "github.com/rudderlabs/ai-sdk-go"
)

type unwrapOnly struct{ http.ResponseWriter }

func (u unwrapOnly) Unwrap() http.ResponseWriter { return u.ResponseWriter }

type flushErrOnly struct{ http.ResponseWriter }

func (flushErrOnly) FlushError() error { return http.ErrNotSupported }

func rawPost(ctx context.Context, t *testing.T, url, session, body string) (status int, contentType, newSession string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", ""
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode, res.Header.Get("Content-Type"), res.Header.Get("Mcp-Session-Id")
}

// Behind writers that flush only through Unwrap or can't flush, ObserveHTTP leaves what mcp-go
// sends unchanged (it picks SSE by asserting http.Flusher) and still reads the response.
func TestObserveHTTPWriterModes(t *testing.T) {
	for name, writer := range map[string]func(http.ResponseWriter) http.ResponseWriter{
		"direct":      func(w http.ResponseWriter) http.ResponseWriter { return w },
		"unwrap only": func(w http.ResponseWriter) http.ResponseWriter { return unwrapOnly{w} },
		"unsupported": func(w http.ResponseWriter) http.ResponseWriter { return flushErrOnly{w} },
	} {
		t.Run(name, func(t *testing.T) {
			wire := map[bool]string{}
			for _, observed := range []bool{false, true} {
				s := server.NewMCPServer("writer-modes", "1", server.WithToolCapabilities(true), server.WithLogging())
				s.AddTool(mcp.NewTool("lookup"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					_ = server.ServerFromContext(ctx).SendNotificationToClient(ctx, "notifications/message", map[string]any{"level": "info", "data": "working"})
					return mcp.NewToolResultError("mode failure"), nil
				})
				r := wrap(t, s, Options{CaptureContent: true})
				var h http.Handler = server.NewStreamableHTTPServer(s)
				if observed {
					h = ObserveHTTP(h)
				}
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { h.ServeHTTP(writer(w), req) }))
				_, _, session := rawPost(context.Background(), t, ts.URL, "", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
				_, _, _ = rawPost(context.Background(), t, ts.URL, session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
				status, contentType, _ := rawPost(context.Background(), t, ts.URL, session, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{}}}`)
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
				get, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
				get.Header.Set("Accept", "text/event-stream")
				get.Header.Set("Mcp-Session-Id", session)
				getStatus := 0
				if res, err := http.DefaultClient.Do(get); err == nil {
					getStatus = res.StatusCode
					_ = res.Body.Close()
				}
				cancel()
				ts.Close()
				wire[observed] = strings.Join([]string{http.StatusText(status), contentType, http.StatusText(getStatus)}, " | ")
				if p := waitProps(t, r, "mcp_tool_called"); observed && p["error_message"] != "mode failure" {
					t.Errorf("error_message=%v", p["error_message"])
				}
			}
			if wire[true] != wire[false] {
				t.Errorf("ObserveHTTP changed the response: %q, without it %q", wire[true], wire[false])
			}
		})
	}
}

// ObserveHTTP around a go-sdk stateless handler: the request context carries the observer.
func TestObserveHTTPAroundGoSDKStateless(t *testing.T) {
	s := sdk.NewServer(&sdk.Implementation{Name: "gosdk-behind-observehttp", Version: "1"}, nil)
	s.AddTool(&sdk.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "gosdk failure"}}}, nil
	})
	r := newRecorder(t)
	if err := rudderstackai.WrapMCPServer(s, rudderstackai.Options{Analytics: r, CaptureContent: true}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(ObserveHTTP(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s }, &sdk.StreamableHTTPOptions{Stateless: true})))
	defer ts.Close()
	_, _, _ = rawPost(context.Background(), t, ts.URL, "", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{}}}`)
	if p := waitProps(t, r, "mcp_tool_called"); p["error_message"] != "gosdk failure" {
		t.Errorf("error_message=%v", p["error_message"])
	}
}

// diagnosing adds a named SSE event, which MCP clients ignore, before each response it sees.
type diagnosing struct{ http.ResponseWriter }

func (w diagnosing) Flush() { w.ResponseWriter.(http.Flusher).Flush() }
func (w diagnosing) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"result"`)) {
		_, _ = w.ResponseWriter.Write([]byte("event: diagnostic\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"error\":{\"code\":-32603,\"message\":\"internal note\"}}\n\n"))
	}
	return w.ResponseWriter.Write(p)
}

// ObserveHTTP shares the response reader: named SSE events are skipped, and a response still
// buffered when the client leaves is never read.
func TestObserveHTTPReadsOnlyWhatTheClientGets(t *testing.T) {
	t.Run("named sse event", func(t *testing.T) {
		s := server.NewMCPServer("named-event", "1", server.WithToolCapabilities(true), server.WithLogging())
		s.AddTool(mcp.NewTool("lookup"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// A notification makes mcp-go answer over SSE.
			_ = server.ServerFromContext(ctx).SendNotificationToClient(ctx, "notifications/message", map[string]any{"level": "info", "data": "working"})
			return mcp.NewToolResultError("lookup failed"), nil
		})
		r := wrap(t, s, Options{CaptureContent: true})
		inner := server.NewStreamableHTTPServer(s, server.WithStateLess(true))
		ts := httptest.NewServer(ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			inner.ServeHTTP(diagnosing{w}, req)
		})))
		defer ts.Close()
		_, contentType, _ := rawPost(context.Background(), t, ts.URL, "", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{}}}`)
		if !strings.HasPrefix(contentType, "text/event-stream") {
			t.Fatalf("mcp-go answered %q, want SSE", contentType)
		}
		if p := waitProps(t, r, "mcp_tool_called"); p["error_message"] != "lookup failed" {
			t.Errorf("error_message=%v, want the response the client read", p["error_message"])
		}
	})
	t.Run("client gone", func(t *testing.T) {
		s := workspaceServer(t.Name(), nil)
		r := wrap(t, s, Options{CaptureContent: true})
		buffered := make(chan struct{})
		ts := httptest.NewServer(ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			server.NewStreamableHTTPServer(s, server.WithStateLess(true)).ServeHTTP(w, req)
			close(buffered)
			<-req.Context().Done()
		})))
		defer ts.Close()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _, _ = rawPost(ctx, t, ts.URL, "", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fail","arguments":{}}}`)
		}()
		<-buffered
		cancel()
		<-done
		if p := waitProps(t, r, "mcp_tool_called"); p["error_message"] != nil {
			t.Errorf("error_message from a response the client never got: %v", p["error_message"])
		}
	})
}
